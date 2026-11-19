package core

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"
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
	// More: the run used its time budget (or must wait out the upstream's write limit) and stopped without a fault: push again
	// (after Wait seconds). The notes done so far are remembered, so the next run goes on where this one ended.
	More bool `json:"more,omitempty"`
	Wait int  `json:"wait,omitempty"`
	// Conflicts: publish mode, notes that changed (or are gone) on the upstream since our last write; they are not overwritten.
	Conflicts int  `json:"conflicts,omitempty"`
	more      int  // lines left out of Lines (see maxReportLines)
	archived  bool // the link's project or team is archived: nothing was tried
	// Next: with More, the id of the last local note this run has dealt with; the next call names it as `after` (with Project or Team and
	// Upstream) and goes on with the notes above it, so the run always gets further, whatever happened to a note that was refused.
	Next    int64 `json:"next,omitempty"`
	Project int64 `json:"project"` // the local side of the link: a project id, or a team's segment id
	Team    bool  `json:"team"`
}

// PushUpstream sends the published notes of the linked projects (project or team > 0: that one; both 0: every link) to their upstreams as
// SUBMISSIONS for the upstream's reviewers. dry: only asks the upstream what exists; nothing is written there or here. sleep waits out a
// rate limit and says whether it did (false: it cannot, for example the time budget of ctx is nearly used: the run then ends with More).
// ctx may carry a deadline, the time budget of one call (the Mac helper answers one request within 30 seconds): when it is used the
// run ends with More and the next call goes on.
//
// upstream (optional) names the link of a project that has several; after (> 0, only with exactly one link named) is the Next of an earlier
// run that ended with More: the run goes on with the notes above that id.
func (s *Store) PushUpstream(ctx context.Context, tr UpstreamTransport, project, team int64, upstream string, after int64, dry bool, sleep func(time.Duration) bool) ([]UpstreamPush, error) {
	if after < 0 || after > 0 && (upstream == "" || project == 0 && team == 0) {
		return nil, ErrRefusal{"a continuation names its link (project or team, and upstream) and a note id"}
	}
	links, err := s.UpstreamLinks(ctx, project, team)
	if err != nil {
		return nil, err
	}
	if upstream != "" {
		kept := links[:0]
		for _, l := range links {
			if l.Upstream == upstream {
				kept = append(kept, l)
			}
		}
		links = kept
	}
	if len(links) == 0 {
		return nil, ErrRefusal{"no upstream link for this: minigw gw upstream link <project> <upstream>"}
	}
	var out []UpstreamPush
	for _, l := range links {
		r, err := s.pushLink(ctx, tr, l.UpstreamLink, l.Label, after, dry, sleep, false)
		if err != nil {
			return out, err
		}
		out = append(out, r)
	}
	return out, nil
}

type pusher struct {
	s      *Store
	tr     UpstreamTransport
	up     string
	sleep  func(time.Duration) bool
	ctx    context.Context
	secret []byte        // this gateway's key for what it sends (Store.upstreamKey)
	more   bool          // set when a limit could not be waited out
	wait   time.Duration // for how long
	status int           // the HTTP status of the last call (0: no usable answer)
	big    bool          // the last call's answer was over maxUpstreamAnswer
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
		p.status, p.big = 0, err == nil && len(data) > maxUpstreamAnswer
		if err != nil || len(data) > maxUpstreamAnswer {
			return upAnswer{status: 0, errMsg: "no usable answer from the upstream"}
		}
		p.status = st
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
		if !p.sleep(retry) {
			p.more, p.wait = true, retry
			return a
		}
	}
}

// invisibleRune is the context-free part of THE rule for what a person (or a reviewer reading a screen) cannot see in text that came from
// somewhere else: controls, format characters (bidi, zero-width, the tag block), line and paragraph separators, variation selectors (also the
// supplement), the Hangul and combining-grapheme fillers. Do not copy the list; callers that hold a whole text use invisibleAt, which also
// lets an emoji's own U+FE0F and U+200D through.
func invisibleRune(r rune) bool {
	switch {
	case unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == 0x2028 || r == 0x2029:
		return true
	case r >= 0xFE00 && r <= 0xFE0F, r >= 0xE0100 && r <= 0xE01EF, r >= 0x180B && r <= 0x180F, r >= 0xE0000 && r <= 0xE007F:
		return true
	case r == 0x2065, r >= 0xFFF0 && r <= 0xFFF8, r >= 0xE0080 && r <= 0xE00FF, r >= 0xE01F0 && r <= 0xE0FFF: // default-ignorable, unassigned: they render as nothing too
		return true
	}
	switch r {
	case 0x034F, 0x115F, 0x1160, 0x3164, 0xFFA0:
		return true
	}
	return false
}

// emojiBase: a character that can be (or begin) an emoji. Go's standard library has no emoji property table and there is no dependency for
// one, so this is a small range check and DELIBERATELY a superset (it admits plain symbols and arrows too). That is safe: the purpose of the
// rule is that a reviewer is never shown text that differs from what is stored, and a U+FE0F after a symbol or a U+200D between two
// pictographs hides no text. Letters and digits are not in it (digits only in the keycap form, see invisibleAt).
func emojiBase(r rune) bool {
	switch {
	case r >= 0x1F000 && r <= 0x1FAFF, r >= 0x2600 && r <= 0x27BF, r >= 0x2190 && r <= 0x21FF, r >= 0x2300 && r <= 0x23FF, r >= 0x2B00 && r <= 0x2BFF:
		return true
	}
	switch r {
	case 0xA9, 0xAE, 0x203C, 0x2049, 0x2122, 0x2139, 0x3030, 0x303D, 0x3297, 0x3299:
		return true
	}
	return false
}

// pictograph: what a joiner may join, narrower than emojiBase: the pictograph blocks only (no arrows, technical symbols, (c) (r) and the like).
func pictograph(r rune) bool { return r >= 0x1F000 && r <= 0x1FAFF || r >= 0x2600 && r <= 0x27BF }

// maxZWJ: joiners in one sequence (the longest real family sequences have fewer).
const maxZWJ = 7

// zwjLeft: the index of the pictograph left of the joiner at z (skipping one U+FE0F and one skin-tone modifier), or -1.
func zwjLeft(rs []rune, z int) int {
	j := z - 1
	for k := 0; k < 2 && j >= 0 && (rs[j] == 0xFE0F || rs[j] >= 0x1F3FB && rs[j] <= 0x1F3FF); k++ {
		j--
	}
	if j >= 0 && pictograph(rs[j]) {
		return j
	}
	return -1
}

// invisibleAt: whether rs[i] is invisible in the text rs. As invisibleRune, except that U+FE0F is visible right after an emoji base (or
// in a keycap: # * 0-9, FE0F, U+20E3) and U+200D is visible between two pictographs (optionally FE0F or a skin-tone modifier before it), at most maxZWJ in a row.
// A ZWJ between letters, a lone FE0F after a letter, zero-width spaces, bidi controls and the tag characters (so an emoji TAG sequence such as
// a subdivision flag) stay invisible.
func invisibleAt(rs []rune, i int) bool {
	switch rs[i] {
	case 0xFE0F:
		if i == 0 {
			return true
		}
		p := rs[i-1]
		return !(emojiBase(p) || (p == '#' || p == '*' || p >= '0' && p <= '9') && i+1 < len(rs) && rs[i+1] == 0x20E3)
	case 0x200D:
		n := 1 // this joiner's place in its sequence
		for z := i; ; n++ {
			p := zwjLeft(rs, z)
			if p < 0 || z+1 >= len(rs) || !pictograph(rs[z+1]) {
				return true
			}
			if p == 0 || rs[p-1] != 0x200D {
				break
			}
			z = p - 1
		}
		return n > maxZWJ
	}
	return invisibleRune(rs[i])
}

// dropInvisible: s without its invisible characters (invisibleAt), each replaced by repl, or removed if repl is -1; keepWS keeps \n and \t.
func dropInvisible(s string, repl rune, keepWS bool) string {
	rs := []rune(s)
	out := make([]rune, 0, len(rs))
	for i, r := range rs {
		switch {
		case keepWS && (r == '\n' || r == '\t'), !invisibleAt(rs, i):
			out = append(out, r)
		case repl >= 0:
			out = append(out, repl)
		}
	}
	return string(out)
}

// upText: text that came from the upstream or from a person there, as one printable line of at most n bytes.
func upText(s string, n int) string {
	s = dropInvisible(s, ' ', false)
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
		p.status = 404 // not a failure of the upstream: the team is not (or no longer) ours to read
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

// mac: HMAC-SHA256 under this gateway's secret (upstreamKey): what leaves is not a hash of guessable numbers, so an upstream cannot work
// out project or note ids from an idempotency key or a reference.
func (p *pusher) mac(parts ...any) []byte {
	m := hmac.New(sha256.New, p.secret)
	fmt.Fprint(m, parts...)
	return m.Sum(nil)
}

// upstreamRefMark starts the trailer; upstreamRef is the opaque reference in it (12 hex digits). The read side uses both to know a hub's copy
// of a note this gateway sent (upstream_mirror_read.go).
const upstreamRefMark = "<!-- minigw upstream ref "

func upstreamRef(secret []byte, up string, project, note int64) string {
	return hex.EncodeToString((&pusher{secret: secret}).mac("ref|", up, "|", project, "|", note)[:6])
}

func (p *pusher) submissionKey(project int64, n UpstreamNote, supersedes int64) string {
	return "up-" + hex.EncodeToString(p.mac("idem|", p.up, "|", project, "|", n.ID, "|", n.Hash, "|", supersedes)[:16])
}

// trailer: where the text came from, as display only, and as little as possible: an opaque reference (stable for this note on this
// link, so that reviewers see two submissions are the same note; it says nothing about this gateway) and the local author's name. No
// gateway id, note id or revision leaves with it. It carries no identity: that is the bound device.
func (p *pusher) trailer(project int64, n UpstreamNote, supersedes int64) string {
	author := strings.ReplaceAll(upText(n.Author, 64), "-", "\u2010") // never "-->"
	t := "\n\n" + upstreamRefMark + upstreamRef(p.secret, p.up, project, n.ID) + ", author " + author
	if supersedes > 0 {
		t += fmt.Sprintf("; supersedes submission %d", supersedes)
	}
	return t + " -->\n"
}

// maxReportLines: the lines of one report; the totals count everything, the rest of the lines is "... and K more".
const maxReportLines = 200

// errPushRunning: another push or pull of this link is running; a manual push answers it as a refusal, the timer just comes back later.
type errPushRunning struct{}

func (errPushRunning) Error() string {
	return "a push or pull of this link is running: try again in a moment"
}
func (e errPushRunning) Unwrap() error { return ErrRefusal{e.Error()} }

func (s *Store) linkLock(project int64, upstream string) *sync.Mutex {
	s.autoMu.Lock()
	defer s.autoMu.Unlock()
	k := fmt.Sprintf("%d|%s", project, upstream)
	if s.linkMu == nil {
		s.linkMu = map[string]*sync.Mutex{}
	}
	if s.linkMu[k] == nil {
		s.linkMu[k] = &sync.Mutex{}
	}
	return s.linkMu[k]
}

func (s *Store) upstreamLock(upstream string) *sync.RWMutex {
	s.autoMu.Lock()
	defer s.autoMu.Unlock()
	if s.upMu == nil {
		s.upMu = map[string]*sync.RWMutex{}
	}
	if s.upMu[upstream] == nil {
		s.upMu[upstream] = &sync.RWMutex{}
	}
	return s.upMu[upstream]
}

func (s *Store) pushLink(ctx context.Context, tr UpstreamTransport, l UpstreamLink, label string, after int64, dry bool, sleep func(time.Duration) bool, auto bool) (UpstreamPush, error) {
	if !dry { // one real push of a link at a time: two would both write with the same revision and one would record a false conflict
		upMu := s.upstreamLock(l.Upstream)
		if !upMu.TryRLock() {
			return UpstreamPush{}, errPushRunning{}
		}
		defer upMu.RUnlock()
		mu := s.linkLock(l.Project, l.Upstream)
		if !mu.TryLock() {
			return UpstreamPush{}, errPushRunning{}
		}
		defer mu.Unlock()
		if err := s.beginTraversal(ctx, l, after); err != nil {
			return UpstreamPush{}, err
		}
	}
	res, err := s.pushLinkRun(ctx, tr, l, label, after, dry, sleep)
	if err == nil && !dry && !res.archived { // the outbox: cleared by a success, a back-off after a failure
		err = s.afterRun(context.WithoutCancel(ctx), l, res)
	}
	if auto && (err == nil || res.Created+res.Replaced > 0) {
		if e := s.auditAutoRun(context.WithoutCancel(ctx), l, label, res); e != nil {
			return res, e
		}
	}
	if res.more > 0 {
		res.Lines = append(res.Lines, fmt.Sprintf("... and %d more", res.more))
	}
	return res, err
}

func (s *Store) pushLinkRun(ctx context.Context, tr UpstreamTransport, l UpstreamLink, label string, after int64, dry bool, sleep func(time.Duration) bool) (UpstreamPush, error) {
	limit := maxPushNotes // notes to SEND per run; a dry run sends nothing and reports them all
	if dry {
		limit = 0
	}
	l, _, _, archived, scan, err := s.upstreamPlan(ctx, l.Project, l.Upstream, after, limit)
	if err != nil {
		return UpstreamPush{}, err
	}
	if archived { // linked before it was archived: nothing is sent for an archived project or team
		_, isT := SegmentTeam(l.Project)
		return UpstreamPush{Label: label, Upstream: l.Upstream, Remote: l.Remote, DryRun: dry, Project: l.Project, Team: isT,
			Stopped: label + " is archived: nothing is sent until it is unarchived", archived: true}, nil
	}
	notes := scan.Notes
	wctx := context.WithoutCancel(ctx) // a receipt is written even when the time budget of the call is used up meanwhile
	_, isTeam := SegmentTeam(l.Project)
	res := UpstreamPush{Label: label, Upstream: l.Upstream, Remote: l.Remote, DryRun: dry, Project: l.Project, Team: isTeam}
	lastDone := after
	more := func(why string, wait int) (UpstreamPush, error) {
		if dry { // a dry run is one report, never continued: it says where it had to stop
			res.Stopped = why
			return res, nil
		}
		res.More, res.Wait, res.Stopped, res.Next = true, wait, why, lastDone
		return res, nil
	}
	secret, err := s.upstreamKey(wctx)
	if err != nil {
		return UpstreamPush{}, err
	}
	p := &pusher{s: s, tr: tr, up: l.Upstream, sleep: sleep, ctx: ctx, secret: secret}
	stop := func(why string) (UpstreamPush, error) {
		res.Stopped = why
		if !dry {
			if err := s.upstreamRecordPush(wctx, l.Project, l.Upstream, why, false); err != nil {
				return res, err
			}
		}
		return res, nil
	}
	remote, why := p.resolve(l)
	if p.more { // the upstream's write limit cannot be waited out within this call
		return more("the upstream's write limit: push again in "+strconv.Itoa(int(p.wait/time.Second))+" seconds", int(p.wait/time.Second))
	}
	if why != "" {
		return stop(why)
	}
	line := func(action string, n UpstreamNote, detail string) {
		l := action + " " + strconv.FormatInt(n.ID, 10) + " " + upText(n.Title, 80)
		if detail != "" {
			l += ": " + detail
		}
		if len(res.Lines) < maxReportLines {
			res.Lines = append(res.Lines, l)
		} else {
			res.more++
		}
	}
	base := fmt.Sprintf("/minigw/v1/projects/%d/kb/submissions", remote)
	for i, n := range notes {
		if i > 0 {
			lastDone = notes[i-1].ID
		}
		if ctx.Err() != nil { // the time budget of this call is used: the next call goes on (nothing is lost, the receipts are written)
			return more("time budget of this call used: push again", 0)
		}
		if n.Foreign {
			res.Skipped++
			line("skip", n, "arrived from another gateway, never pushed onward")
			continue
		}
		if n.Unchanged {
			res.Skipped++
			line("skip", n, "unchanged since the last push")
			continue
		}
		if n.Conflicted {
			res.Conflicts++
			line("conflict", n, "changed on the upstream since our last write, not overwritten: minigw gw upstream resolve <project> "+strconv.FormatInt(n.ID, 10)+" --theirs|--ours")
			continue
		}
		if l.Mode == "publish" && n.EntryKind != "faq" && !(n.Sent != nil && (n.Sent.Conflict == 2 || n.Sent.RemoteSub > 0)) { // a note sent as a submission ("ours") stays on the submission path
			// publish mode: the note is written on the upstream directly (a note, not a submission)
			notesBase := fmt.Sprintf("/minigw/v1/projects/%d/notes", remote)
			waitMsg := func() (UpstreamPush, error) {
				return more("the upstream's write limit: push again in "+strconv.Itoa(int(p.wait/time.Second))+" seconds", int(p.wait/time.Second))
			}
			// refused: counts a refusal; done=true ends the run (nothing else would be accepted either)
			refused := func(a upAnswer) (r UpstreamPush, done bool, e error) {
				if p.more {
					r, e = waitMsg()
					return r, true, e
				}
				res.Refused++
				line("refused", n, refusal(a))
				if a.status == 0 || a.status == 401 || a.status == 403 || a.status == 404 || a.errType == "pending_limit" || a.status == 429 {
					r, e = stop(refusal(a))
					return r, true, e
				}
				return res, false, nil
			}
			conflict := func(rev int64) error {
				res.Conflicts++
				why := "changed on the upstream since our last write (revision " + strconv.FormatInt(rev, 10) + "), not overwritten"
				if rev == 0 {
					why = "deleted on the upstream, not recreated"
				}
				if dry {
					line("would conflict", n, why)
					return nil
				}
				if e := s.upstreamRecordConflict(wctx, l.Project, l.Upstream, n.ID, rev); e != nil {
					return e
				}
				line("conflict", n, why+": minigw gw upstream resolve <project> "+strconv.FormatInt(n.ID, 10)+" --theirs|--ours")
				return nil
			}
			text := n.Body + p.trailer(l.Project, n, 0)
			if n.Sent != nil && n.Sent.RemoteNote > 0 { // an update of the note we wrote: only when it is still as we left it
				a := p.call("GET", fmt.Sprintf("%s/%d", notesBase, n.Sent.RemoteNote), nil, nil)
				if p.more {
					return waitMsg()
				}
				var cur struct{ Revision int64 }
				switch {
				case a.status == 404:
					if e := conflict(0); e != nil {
						return res, e
					}
				case a.status == 200 && json.Unmarshal(a.data, &cur) == nil && cur.Revision > 0:
					if cur.Revision != n.Sent.RemoteRev {
						if e := conflict(cur.Revision); e != nil {
							return res, e
						}
						continue
					}
					if dry {
						res.Replaced++
						line("would replace", n, "updates the note "+strconv.FormatInt(n.Sent.RemoteNote, 10)+" on the upstream")
						continue
					}
					payload, _ := json.Marshal(map[string]any{"revision": cur.Revision, "class": n.Class, "title": n.Title, "body": text})
					u := p.call("PUT", fmt.Sprintf("%s/%d", notesBase, n.Sent.RemoteNote), nil, payload)
					if p.more {
						return waitMsg()
					}
					if u.status == 409 { // it moved between our look and our write
						rev := cur.Revision + 1
						var er struct{ Error struct{ Revision int64 } }
						if json.Unmarshal(u.data, &er) == nil && er.Error.Revision > 0 {
							rev = er.Error.Revision
						}
						if e := conflict(rev); e != nil {
							return res, e
						}
						continue
					}
					var out struct{ Revision int64 }
					if u.status != 200 || json.Unmarshal(u.data, &out) != nil || out.Revision <= 0 {
						if r, done, e := refused(u); done {
							return r, e
						}
						continue
					}
					if e := s.upstreamRecordPublished(wctx, l.Project, l.Upstream, n, n.Sent.RemoteNote, out.Revision); e != nil {
						return res, e
					}
					res.Replaced++
					line("replace", n, "updated the note "+strconv.FormatInt(n.Sent.RemoteNote, 10)+" on the upstream")
				default:
					if r, done, e := refused(a); done {
						return r, e
					}
				}
				continue
			}
			if dry {
				res.Created++
				line("would create", n, "published as a note")
				continue
			}
			payload, _ := json.Marshal(map[string]any{"class": n.Class, "title": n.Title, "body": text})
			a := p.call("POST", notesBase, map[string]string{"Idempotency-Key": p.submissionKey(l.Project, n, 0)}, payload)
			var out struct{ ID, Revision int64 }
			if a.status != 201 || json.Unmarshal(a.data, &out) != nil || out.ID <= 0 || out.Revision <= 0 {
				if r, done, e := refused(a); done {
					return r, e
				}
				continue
			}
			if e := s.upstreamRecordPublished(wctx, l.Project, l.Upstream, n, out.ID, out.Revision); e != nil {
				return res, e
			}
			res.Created++
			line("create", n, "published as note "+strconv.FormatInt(out.ID, 10)+" on the upstream")
			continue
		}
		var replaces, revision, supersedes int64
		action, detail := "create", ""
		if n.Sent != nil && n.Sent.Conflict == 2 { // the owner chose "ours": our version goes as a submission, never as an overwrite
			action = "replace"
			supersedes = n.Sent.RemoteSub
			if n.Sent.RemoteNote > 0 && n.Sent.ConflictRev > 0 {
				replaces, revision = n.Sent.RemoteNote, n.Sent.ConflictRev
			}
			detail = "resolved as ours: sent as a submission for the upstream's reviewers"
		} else if n.Sent != nil { // changed since it was sent: a replacement where the upstream's routes allow it
			action = "replace"
			supersedes = n.Sent.RemoteSub
			a := p.call("GET", fmt.Sprintf("%s/%d", base, n.Sent.RemoteSub), nil, nil)
			if p.more {
				return more("the upstream's write limit: push again in "+strconv.Itoa(int(p.wait/time.Second))+" seconds", int(p.wait/time.Second))
			}
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
		fields := map[string]any{"class": n.Class, "title": n.Title, "body": n.Body + p.trailer(l.Project, n, supersedes), "replaces": replaces, "revision": revision}
		if n.EntryKind == "faq" {
			fields["entryKind"] = "faq"
		}
		payload, _ := json.Marshal(fields)
		a := p.call("POST", base, map[string]string{"Idempotency-Key": p.submissionKey(l.Project, n, supersedes)}, payload)
		if a.status != 201 {
			if p.more { // a write limit that cannot be waited out in this call: not a refusal of the note
				return more("the upstream's write limit: push again in "+strconv.Itoa(int(p.wait/time.Second))+" seconds", int(p.wait/time.Second))
			}
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
		if err := s.upstreamRecordSent(wctx, l.Project, l.Upstream, n, sub.ID, replaces); err != nil {
			return res, err
		}
		if action == "replace" {
			res.Replaced++
		} else {
			res.Created++
		}
		line(action, n, detail)
	}
	if scan.Overflow && len(notes) > 0 { // more notes to send follow: the next call starts above the last one dealt with here
		lastDone = notes[len(notes)-1].ID
		return more(fmt.Sprintf("the first %d notes to send are done: push again for the rest", maxPushNotes), 0)
	}
	if !dry && res.Refused == 0 && res.Stopped == "" {
		if err := s.upstreamRecordPush(wctx, l.Project, l.Upstream, "", true); err != nil {
			return res, err
		}
	} else if !dry && res.Refused > 0 {
		for i := len(res.Lines) - 1; i >= 0; i-- {
			if strings.HasPrefix(res.Lines[i], "refused ") {
				if err := s.upstreamRecordPush(wctx, l.Project, l.Upstream, upText(res.Lines[i], 300), false); err != nil {
					return res, err
				}
				break
			}
		}
	}
	return res, nil
}

// UpText: text that came from an upstream, as one printable line of at most n bytes.
func UpText(s string, n int) string { return upText(s, n) }
