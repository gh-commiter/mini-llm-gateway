package core

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// UpstreamTransport is how the host talks to an upstream: one request, as the gateway's own device. status 0 = no answer. What comes back
// is DATA: this file reads a few fields of it and never lets it choose a path, a host, a note or a setting here.
type UpstreamTransport interface {
	Do(ctx context.Context, upstream, method, path string, header map[string]string, body []byte) (status int, data []byte, retryAfter time.Duration, err error)
}

const (
	maxUpstreamAnswer = 1 << 20 // what is parsed of one answer; more is a malformed answer
	maxRateWaits      = 5       // 429 answers waited out per request
	maxRateWait       = 2 * time.Minute
)

// UpstreamPush is the result of one link of one run.
type UpstreamPush struct {
	Label    string   `json:"label"` // the project's short name or "team <name>"
	Upstream string   `json:"upstream"`
	Remote   string   `json:"remote"`
	Lines    []string `json:"lines"`
	Created  int      `json:"created"`
	Skipped  int      `json:"skipped"`
	Replaced int      `json:"replaced"`
	Refused  int      `json:"refused"`
	Stopped  string   `json:"stopped,omitempty"` // why the run ended before the last note
	DryRun   bool     `json:"dryRun"`
}

// PushUpstream sends the published notes of the linked projects (project or team > 0: that one; both 0: every link) to their upstreams as
// SUBMISSIONS for the upstream's reviewers. dry: only asks the upstream what exists; nothing is written there or here. sleep waits out a
// rate limit (the tests give a fake).
func (s *Store) PushUpstream(ctx context.Context, tr UpstreamTransport, project, team int64, dry bool, sleep func(time.Duration)) ([]UpstreamPush, error) {
	links, err := s.UpstreamLinks(ctx, project, team)
	if err != nil {
		return nil, err
	}
	if len(links) == 0 {
		return nil, ErrRefusal{"no upstream link for this: minigw gw upstream link <project> <upstream>"}
	}
	var out []UpstreamPush
	for _, l := range links {
		r, err := s.pushLink(ctx, tr, l.UpstreamLink, l.Label, dry, sleep)
		if err != nil {
			return out, err
		}
		out = append(out, r)
	}
	return out, nil
}

type pusher struct {
	s     *Store
	tr    UpstreamTransport
	up    string
	sleep func(time.Duration)
	ctx   context.Context
}

type upAnswer struct {
	status  int
	data    []byte
	errType string
	errMsg  string
}

// call: one request; a 429 is waited out (as `kb import` does) up to a few times.
func (p *pusher) call(method, path string, hdr map[string]string, body []byte) (a upAnswer) {
	for i := 0; ; i++ {
		st, data, retry, err := p.tr.Do(p.ctx, p.up, method, path, hdr, body)
		if err != nil || len(data) > maxUpstreamAnswer {
			return upAnswer{status: 0, errMsg: "no usable answer from the upstream"}
		}
		a = upAnswer{status: st, data: data}
		var e struct {
			Error struct{ Type, Message string }
		}
		if json.Unmarshal(data, &e) == nil {
			a.errType, a.errMsg = e.Error.Type, upText(e.Error.Message, 200)
		}
		if st != 429 || i >= maxRateWaits {
			return a
		}
		if retry < time.Second {
			retry = time.Second
		}
		if retry > maxRateWait {
			retry = maxRateWait
		}
		p.sleep(retry)
	}
}

// upText: text that came from the upstream or from a person there, as one printable line of at most n bytes.
func upText(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == 0x2028 || r == 0x2029 {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	for len(s) > n {
		_, w := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-w]
	}
	return s
}

func refusal(a upAnswer) string {
	switch {
	case a.status == 0:
		return "no answer from the upstream"
	case a.status == 401 || a.status == 403 && a.errMsg == "":
		return "the upstream refuses this gateway (HTTP " + strconv.Itoa(a.status) + "): the owner there must allow its device, bind it to a user and give that user a role"
	}
	m := a.errMsg
	if m == "" {
		m = "HTTP " + strconv.Itoa(a.status)
	}
	return "the upstream says: " + m
}

// resolve: the id of the remote project or of the team's own notes, as the upstream names it to this device.
func (p *pusher) resolve(l UpstreamLink) (int64, string) {
	if l.Team {
		a := p.call("GET", "/minigw/v1/teams", nil, nil)
		if a.status != 200 {
			return 0, refusal(a)
		}
		var ts []struct {
			Name    string
			Segment int64
		}
		if json.Unmarshal(a.data, &ts) != nil {
			return 0, "the upstream's list of teams is not readable"
		}
		for _, t := range ts {
			if strings.EqualFold(t.Name, l.Remote) {
				if _, ok := SegmentTeam(t.Segment); !ok {
					return 0, "the upstream named no notes of team " + l.Remote
				}
				return t.Segment, ""
			}
		}
		return 0, "this gateway is not on team " + upText(l.Remote, 64) + " on the upstream (or it does not exist there)"
	}
	a := p.call("GET", "/minigw/v1/projects/"+url.PathEscape(l.Remote), nil, nil)
	if a.status != 200 {
		return 0, refusal(a)
	}
	var pr struct {
		ID   int64
		Slug string
	}
	if json.Unmarshal(a.data, &pr) != nil || pr.ID <= 0 || !strings.EqualFold(pr.Slug, l.Remote) {
		return 0, "the upstream answered for another project than " + upText(l.Remote, 64)
	}
	return pr.ID, ""
}

func (p *pusher) submissionKey(project int64, n UpstreamNote, supersedes int64) string {
	h := sha256.Sum256([]byte(fmt.Sprint(p.up, "|", project, "|", n.ID, "|", n.Hash, "|", supersedes)))
	return "up-" + hex.EncodeToString(h[:16])
}

// trailer: the origin of the text as display only: the local author's name, the gateway, the local note and revision. It is part of the
// submission's text, so the reviewers see where it came from; it carries no identity (that is the bound device).
func (p *pusher) trailer(n UpstreamNote, supersedes int64) string {
	author := strings.ReplaceAll(upText(n.Author, 64), "-", "‐") // never "-->"
	t := fmt.Sprintf("\n\n<!-- sent by gateway %s: local note %d, revision %d, author %s", p.s.GatewayID(), n.ID, n.Revision, author)
	if supersedes > 0 {
		t += fmt.Sprintf("; supersedes submission %d", supersedes)
	}
	return t + " -->\n"
}

func (s *Store) pushLink(ctx context.Context, tr UpstreamTransport, l UpstreamLink, label string, dry bool, sleep func(time.Duration)) (UpstreamPush, error) {
	l, _, _, notes, err := s.upstreamPlan(ctx, l.Project, l.Upstream)
	if err != nil {
		return UpstreamPush{}, err
	}
	res := UpstreamPush{Label: label, Upstream: l.Upstream, Remote: l.Remote, DryRun: dry}
	p := &pusher{s: s, tr: tr, up: l.Upstream, sleep: sleep, ctx: ctx}
	stop := func(why string) (UpstreamPush, error) {
		res.Stopped = why
		if !dry {
			if err := s.upstreamRecordPush(ctx, l.Project, l.Upstream, why, false); err != nil {
				return res, err
			}
		}
		return res, nil
	}
	remote, why := p.resolve(l)
	if why != "" {
		return stop(why)
	}
	if len(notes) > maxPushNotes {
		notes = notes[:maxPushNotes]
		res.Stopped = fmt.Sprintf("only the first %d notes this run: push again for the rest", maxPushNotes)
	}
	line := func(action string, n UpstreamNote, detail string) {
		l := action + " " + strconv.FormatInt(n.ID, 10) + " " + upText(n.Title, 80)
		if detail != "" {
			l += ": " + detail
		}
		res.Lines = append(res.Lines, l)
	}
	base := fmt.Sprintf("/minigw/v1/projects/%d/kb/submissions", remote)
	for _, n := range notes {
		if n.Sent != nil && n.Sent.Hash == n.Hash {
			res.Skipped++
			line("skip", n, "unchanged since the last push")
			continue
		}
		var replaces, revision, supersedes int64
		action, detail := "create", ""
		if n.Sent != nil { // changed since it was sent: a replacement where the upstream's routes allow it
			action = "replace"
			supersedes = n.Sent.RemoteSub
			a := p.call("GET", fmt.Sprintf("%s/%d", base, n.Sent.RemoteSub), nil, nil)
			var sub struct {
				State string
				Note  int64
			}
			switch {
			case a.status == 200 && json.Unmarshal(a.data, &sub) == nil && sub.State == "accepted" && sub.Note > 0:
				na := p.call("GET", fmt.Sprintf("/minigw/v1/projects/%d/notes/%d", remote, sub.Note), nil, nil)
				var cur struct{ Revision int64 }
				if na.status == 200 && json.Unmarshal(na.data, &cur) == nil && cur.Revision > 0 {
					replaces, revision, detail = sub.Note, cur.Revision, "replaces the accepted note "+strconv.FormatInt(sub.Note, 10)
				} else {
					detail = "the accepted note cannot be read there: sent as a new submission"
				}
			case a.status == 200 && sub.State == "pending":
				detail = "the earlier submission is still pending: a new one is sent"
			case a.status == 200:
				detail = "the earlier submission was not accepted: sent again"
			default:
				detail = "the earlier submission cannot be read there: sent as a new submission"
			}
		}
		if dry {
			if action == "replace" {
				res.Replaced++
			} else {
				res.Created++
			}
			line("would "+action, n, detail)
			continue
		}
		fields := map[string]any{"class": n.Class, "title": n.Title, "body": n.Body + p.trailer(n, supersedes), "replaces": replaces, "revision": revision}
		if n.EntryKind == "faq" {
			fields["entryKind"] = "faq"
		}
		payload, _ := json.Marshal(fields)
		a := p.call("POST", base, map[string]string{"Idempotency-Key": p.submissionKey(l.Project, n, supersedes)}, payload)
		if a.status != 201 {
			res.Refused++
			line("refused", n, refusal(a))
			if a.status == 0 || a.status == 401 || a.status == 403 || a.status == 404 || a.errType == "pending_limit" || a.status == 429 {
				return stop(refusal(a)) // nothing else would be accepted either
			}
			continue
		}
		var sub struct{ ID int64 }
		if json.Unmarshal(a.data, &sub) != nil || sub.ID <= 0 {
			res.Refused++
			line("refused", n, "the upstream's answer names no submission")
			continue
		}
		if err := s.upstreamRecordSent(ctx, l.Project, l.Upstream, n, sub.ID, replaces); err != nil {
			return res, err
		}
		if action == "replace" {
			res.Replaced++
		} else {
			res.Created++
		}
		line(action, n, detail)
	}
	if !dry && res.Refused == 0 && res.Stopped == "" {
		if err := s.upstreamRecordPush(ctx, l.Project, l.Upstream, "", true); err != nil {
			return res, err
		}
	} else if !dry && res.Refused > 0 {
		for i := len(res.Lines) - 1; i >= 0; i-- {
			if strings.HasPrefix(res.Lines[i], "refused ") {
				if err := s.upstreamRecordPush(ctx, l.Project, l.Upstream, upText(res.Lines[i], 300), false); err != nil {
					return res, err
				}
				break
			}
		}
	}
	return res, nil
}

// UpstreamProbe words the answer of the one look that `upstream add` takes: what the upstream's owner still has to do.
func UpstreamProbe(status int, data []byte, err error) string {
	if err != nil || status == 0 {
		return "the upstream did not answer: check the address, and that its gateway is running and reachable from here"
	}
	var e struct{ Error struct{ Message string } }
	json.Unmarshal(data, &e)
	switch {
	case status == 200:
		return "connected: the device is allowed and bound to a user there"
	case status == 401:
		return "the upstream refused the token: copy it again (Settings, General, Copy Token on the upstream)"
	case status == 403 && e.Error.Message != "":
		return "the upstream says: " + upText(e.Error.Message, 200)
	case status == 403:
		return "the device is not allowed there yet: its owner must allow it and bind it to a user"
	}
	return "the upstream answered HTTP " + strconv.Itoa(status)
}
