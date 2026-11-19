package main

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"minigw/core"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// faqPages bounds the pages of kb faq (50 entries each).
const faqPages = 20

const kbHelp = `minigw kb export <project>|--team <team> <dir>
minigw kb import <project>|--team <team> <dir|file.md> [--class c] [--submit]
minigw kb mcp [--project slug] [--print-config]
minigw kb search [<project>|--team <team>] <query> [--json]   a project search also covers its team's notes (as far as you may read them); a hit from there ends with "from team:<name>", a read-only copy from an upstream gateway with "from hub:<name> project:<slug>"
minigw kb submit <project>|--team <team> [--class structure|data] [--kind finding|faq] [--title t] [--key k] [--replaces id --revision n] <file|->   faq: the title is the question, the text a short answer (up to 2 KiB); private is for projects only
minigw kb faq [<project>|--team <team>] [--json]   the project's (or the team's) questions and answers
minigw kb submissions [<project>|--team <team>] [--json]   your submissions (all projects and your teams' own notes if omitted)
minigw kb hooks on|off|status [<project>]   the plugin's hooks that ask a Claude Code session to hand findings in (off by default, per project, on this computer)
minigw kb review [<project>|--team <team>]   project admins: the pending submissions you may review (never your own), including bodies
minigw kb accept <id> [--version <hash>]   project admins: publish a submission of another member; --version: the whole version line kb review showed (without it: only in a terminal, after you confirm the text)
minigw kb reject <id> [--reason text]`
const kbAdminHelp = `minigw gw kb history <id> [--revision n] [--json]
minigw gw kb restore <id> --revision n [--class c] [--current n]
minigw gw kb rebuild-search
minigw gw kb review [<project>|--team <team>]   pending submissions, including bodies
minigw gw kb accept <id> [--version <hash>]   --version: the whole version line gw kb review showed (without it: only in a terminal, after you confirm the text)
minigw gw kb reject <id> [--reason text]`

// kindSuffix marks a faq at the end of the first line; a finding prints as before.
func kindSuffix(kind string) string {
	if kind == "faq" {
		return "  (faq)"
	}
	return ""
}

const (
	teamProjectUsage = "usage: give either a project or --team <team>"
	teamAgentRefusal = "inside an agent run the project is the run's own: --team is not accepted"
)

// teamSegment resolves --team to the id of the team's own notes, which the notes, search and submission routes
// take in place of a project id. A team the caller is not on is the usual 404.
func (e *projectEnv) teamSegment(c connection, team string) (int64, error) {
	id, _, err := e.teamInfo(c, team)
	return id, err
}

// teamInfo is teamSegment with the team's name as the gateway spells it.
func (e *projectEnv) teamInfo(c connection, team string) (int64, string, error) {
	st, b, h := e.do(c, "GET", "/minigw/v1/teams/"+url.PathEscape(team), nil, nil)
	if st != 200 {
		return 0, "", gatewayError(st, b, h)
	}
	var t struct {
		Name    string
		Segment int64
	}
	if err := json.Unmarshal(b, &t); err != nil {
		return 0, "", err
	}
	if t.Segment == 0 {
		return 0, "", errors.New("this gateway has no team notes: update it")
	}
	return t.Segment, t.Name, nil
}

// subjectOf names where a note, hit or submission belongs: "project <id>", or "team <name>" for a team's own notes
// (never the hidden id: an unknown name prints "team"). It is the only place that prints a project column.
func subjectOf(project int64, name string) string {
	if _, ok := core.SegmentTeam(project); ok {
		if name == "" {
			return "team"
		}
		return "team " + name
	}
	return fmt.Sprintf("project %d", project)
}

// teamSubject is subjectOf for a submission, whose team name comes from a map of team ids to names.
func teamSubject(project int64, names map[int64]string) string {
	team, _ := core.SegmentTeam(project)
	return subjectOf(project, names[team])
}

func submissionLine(v core.Submission, names map[int64]string) string {
	return clean(fmt.Sprintf("submission %d  %s  %s  %s  %s%s", v.ID, teamSubject(v.Project, names), v.State, v.Class, v.Title, kindSuffix(v.EntryKind)))
}

// rejectedHintDays: how far back a rejected submission with the same title is still mentioned after a submit.
const rejectedHintDays = 30

// rejectedHint tells the author, after a submit, that a submission with the same title was rejected in the last
// rejectedHintDays days, and why. The author's own rows carry no hash or body (only the review reads do), so the text itself
// cannot be compared without a new route: the title is what can be. Best effort: any failure says nothing.
func (e *projectEnv) rejectedHint(c connection, project int64, title string) {
	now := time.Now
	if e.now != nil {
		now = e.now
	}
	var found *core.Submission
	after := int64(0)
	for page := 0; page < 10; page++ {
		st, b, _ := e.do(c, "GET", fmt.Sprintf("%s/%d/kb/submissions?after=%d", apiProjects, project, after), nil, nil)
		var pg struct {
			Submissions []core.Submission
			Next        int64
		}
		if st != 200 || json.Unmarshal(b, &pg) != nil {
			return
		}
		for i, v := range pg.Submissions {
			if v.State == "rejected" && v.Title == title && now().Sub(v.Provenance.Received) < rejectedHintDays*24*time.Hour && (found == nil || v.ID > found.ID) {
				found = &pg.Submissions[i]
			}
		}
		if pg.Next == 0 {
			break
		}
		after = pg.Next
	}
	if found != nil {
		if found.Reason == "" {
			e.say("an earlier submission with this title was rejected (no reason given)")
		} else {
			e.say(clean("an earlier submission with this title was rejected: " + found.Reason))
		}
	}
}

func submissionResult(v core.Submission) string {
	return fmt.Sprintf("submission %d %s", v.ID, v.State)
}

func (e *projectEnv) kb(args []string) error {
	if len(args) > 0 && (args[0] == "export" || args[0] == "import") {
		return e.kbFiles(args)
	}
	if len(args) > 0 && args[0] == "mcp" {
		return e.kbMCP(args[1:])
	}
	if len(args) > 0 && args[0] == "search" {
		return e.kbSearch(args[1:])
	}
	if len(args) > 0 && args[0] == "hook" { // before the agent refusal: a hook inside an agent run is silent
		return e.kbHook(args[1:])
	}
	if len(args) > 0 && args[0] == "hooks" {
		return e.kbHooks(args[1:])
	}
	if e.agent {
		return errors.New(agentRefusal)
	}
	if len(args) > 0 && args[0] == "faq" {
		return e.kbFAQ(args[1:])
	}
	fs := flag.NewFlagSet("kb", flag.ContinueOnError)
	class := fs.String("class", "data", "")
	kind := fs.String("kind", "", "")
	title := fs.String("title", "", "")
	key := fs.String("key", "", "")
	replaces := fs.Int64("replaces", 0, "")
	revision := fs.Int64("revision", 0, "")
	asJSON := fs.Bool("json", false, "")
	team := fs.String("team", "", "")
	reason := fs.String("reason", "", "")
	version := fs.String("version", "", "")
	pos, err := parseFlags(fs, args)
	usage := errors.New("usage:\n" + kbHelp)
	if err != nil || len(pos) == 0 {
		return usage
	}
	if pos[0] == "accept" || pos[0] == "reject" {
		return e.kbDecide(pos, *team, *reason, *version)
	}
	if pos[0] != "submit" && pos[0] != "submissions" && pos[0] != "review" {
		return usage
	}
	if *team != "" {
		if pos[0] == "submit" && len(pos) != 2 || pos[0] != "submit" && len(pos) != 1 {
			return errors.New(teamProjectUsage)
		}
		if pos[0] == "submit" {
			pos = []string{"submit", "", pos[1]}
		}
	} else if pos[0] == "submit" && len(pos) != 3 || pos[0] != "submit" && len(pos) > 2 {
		return usage
	}
	if *kind != "" && *kind != "finding" && *kind != "faq" {
		return usage
	}
	c, gw := e.connect()
	project := int64(0)
	resolve := func(ref string) (int64, error) {
		st, b, h := e.do(c, "GET", apiProjects+"/"+url.PathEscape(ref), nil, nil)
		if st != 200 {
			return 0, gatewayError(st, b, h)
		}
		var p struct{ ID int64 }
		if err := json.Unmarshal(b, &p); err != nil {
			return 0, err
		}
		return p.ID, nil
	}
	names := map[int64]string{} // team id -> name, for the lines of submissions to a team's own notes
	if *team != "" {
		var name string
		if project, name, err = e.teamInfo(c, *team); err == nil {
			t, _ := core.SegmentTeam(project)
			names[t] = name
		}
	} else if len(pos) > 1 {
		project, err = resolve(pos[1])
	}
	if err != nil {
		return err
	}
	if pos[0] == "review" {
		return e.kbReview(c, project, names)
	}
	base := fmt.Sprintf("%s/%d/kb/submissions", apiProjects, project)
	if pos[0] == "submit" {
		body, err := readInput(e.in, pos[2])
		if err != nil {
			return err
		}
		t := *title
		if t == "" {
			t = defaultTitle(body, pos[2])
		}
		if len(t) > maxNoteTitle {
			return fmt.Errorf("too large: a title is at most %d bytes", maxNoteTitle)
		}
		k := *key
		if k == "" {
			k = newNoteKey()
		}
		fields := map[string]any{"class": *class, "title": t, "body": body, "replaces": *replaces, "revision": *revision}
		if *kind != "" {
			fields["entryKind"] = *kind
		}
		payload, _ := json.Marshal(fields)
		st, b, h := e.push(c, base, k, payload)
		if st == 0 {
			return fmt.Errorf("no answer from the gateway; repeat with --key %s (safe: no duplicate), or check with minigw kb submissions", k)
		}
		if st != 201 {
			return gatewayError(st, b, h)
		}
		e.recordSubmitted(gw, project)
		if *asJSON {
			e.say(jsonText(strings.TrimSpace(string(b))))
			return nil
		}
		var v core.Submission
		if err := json.Unmarshal(b, &v); err != nil {
			return err
		}
		e.say(submissionResult(v))
		e.rejectedHint(c, project, t)
		return nil
	}
	// Enumerate readable projects only to resolve names; the API returns only the authenticated author's rows.
	projects := []int64{project}
	if project == 0 {
		st, b, h := e.do(c, "GET", apiProjects+"?archived=1", nil, nil)
		if st != 200 {
			return gatewayError(st, b, h)
		}
		var ps []struct{ ID int64 }
		if err := json.Unmarshal(b, &ps); err != nil {
			return err
		}
		projects = nil
		for _, p := range ps {
			projects = append(projects, p.ID)
		}
		// The teams you are on: your submissions to their own notes too (a gateway without teams just has none).
		if st, b, _ := e.do(c, "GET", "/minigw/v1/teams", nil, nil); st == 200 {
			var ts []struct {
				Name    string
				Segment int64
			}
			if json.Unmarshal(b, &ts) == nil {
				for _, t := range ts {
					if team, ok := core.SegmentTeam(t.Segment); ok {
						projects = append(projects, t.Segment)
						names[team] = t.Name
					}
				}
			}
		}
	}
	raw := []any{}
	all := []core.Submission{}
	for _, pid := range projects {
		after := int64(0)
		for {
			path := fmt.Sprintf("%s/%d/kb/submissions?after=%d", apiProjects, pid, after)
			st, b, h := e.do(c, "GET", path, nil, nil)
			if st != 200 {
				return gatewayError(st, b, h)
			}
			var pg struct {
				Submissions []core.Submission
				Next        int64
			}
			if err := json.Unmarshal(b, &pg); err != nil {
				return err
			}
			all = append(all, pg.Submissions...)
			var obj struct{ Submissions []any }
			dec := json.NewDecoder(strings.NewReader(string(b)))
			dec.UseNumber()
			if err := dec.Decode(&obj); err != nil {
				return err
			}
			raw = append(raw, obj.Submissions...)
			if pg.Next == 0 {
				break
			}
			after = pg.Next
		}
	}
	if *asJSON {
		b, _ := json.Marshal(map[string]any{"submissions": raw})
		e.say(jsonText(string(b)))
		return nil
	}
	for _, v := range all {
		e.say(submissionLine(v, names))
		if v.State == "rejected" && v.Reason != "" { // why it was rejected: the author reads it before handing the text in again (#163)
			e.say(clean("    reason: " + v.Reason))
		}
	}
	if len(all) == 0 {
		e.say("no submissions")
	}
	return nil
}

// kbReview prints the pending submissions a project admin may review (project 0: all of them), as gw kb review does.
func (e *projectEnv) kbReview(c connection, project int64, names map[int64]string) error {
	teamsAsked := false
	count := 0
	for after := int64(0); ; {
		st, b, h := e.do(c, "GET", fmt.Sprintf("%s/%d/kb/review?after=%d", apiProjects, project, after), nil, nil)
		if st != 200 {
			return gatewayError(st, b, h)
		}
		var pg struct {
			Submissions []core.Submission
			Next        int64
		}
		if err := json.Unmarshal(b, &pg); err != nil {
			return err
		}
		for _, v := range pg.Submissions {
			st, b, h := e.do(c, "GET", fmt.Sprintf("%s/0/kb/review/%d", apiProjects, v.ID), nil, nil)
			if st != 200 {
				return gatewayError(st, b, h)
			}
			var full struct {
				core.Submission
				AuthorName string
			}
			if err := json.Unmarshal(b, &full); err != nil {
				return err
			}
			if team, seg := core.SegmentTeam(full.Project); seg && names[team] == "" && !teamsAsked {
				teamsAsked = true // the notes of a team print as "team <name>": the teams you are on, asked once
				if st, b, _ := e.do(c, "GET", "/minigw/v1/teams", nil, nil); st == 200 {
					var ts []struct {
						Name    string
						Segment int64
					}
					if json.Unmarshal(b, &ts) == nil {
						for _, t := range ts {
							if id, ok := core.SegmentTeam(t.Segment); ok {
								names[id] = t.Name
							}
						}
					}
				}
			}
			printReview(e.say, full.Submission, full.AuthorName, names)
			count++
		}
		if pg.Next == 0 {
			break
		}
		after = pg.Next
	}
	if count == 0 {
		e.say("no submissions")
	}
	return nil
}

// printReview prints one submission as gw kb review and kb review do: its line, the writer, and the text.
func printReview(say func(string), v core.Submission, authorName string, names map[int64]string) {
	say(submissionLine(v, names))
	writer := strconv.FormatInt(v.User, 10)
	if authorName != "" {
		writer = authorName
	}
	say(clean(fmt.Sprintf("writer %s  device %s  replaces %d  revision %d", writer, v.Provenance.Device, v.Replaces, v.Revision)))
	if v.ReviewHash != "" { // on its own line, to copy whole into kb accept --version
		say(clean("version " + v.ReviewHash))
	}
	if v.ReviewCheck == "long" {
		say("long text: the app and the remote page do not offer accept for it")
	}
	if v.Replaces != 0 { // a replacement: the sizes of both texts, the note's current text, then the new text
		if line := sizesLine(v); line != "" {
			say(line)
		}
		printCurrent(say, v)
		say(oneLine(fmt.Sprintf("New text: %s  %s  %s", v.Class, kindOf(v.EntryKind), v.Title)))
	}
	if v.Hidden > 0 {
		say(fmt.Sprintf(hiddenLine, v.Hidden))
	}
	if v.Body != nil {
		say(core.ReviewText(*v.Body))
	}
}

func kindOf(k string) string {
	if k == "" {
		return "finding"
	}
	return k
}

// sizesLine: how much the replacement shortens or lengthens the note, from the sizes the gateway counted (the texts as shown:
// characters are runes, lines the text's lines). Nothing when a side is not there (an older gateway, a missing note).
func sizesLine(v core.Submission) string {
	if v.CurrentLines == nil || v.CurrentChars == nil || v.NewLines == nil || v.NewChars == nil {
		return ""
	}
	line := fmt.Sprintf("current: %d lines, %d characters -> new: %d lines, %d characters", *v.CurrentLines, *v.CurrentChars, *v.NewLines, *v.NewChars)
	switch d := *v.NewLines - *v.CurrentLines; {
	case d < 0:
		line += fmt.Sprintf(" (lines shorter: %d, %d %%)", -d, (-d*100+*v.CurrentLines/2) / *v.CurrentLines)
	case d > 0:
		line += fmt.Sprintf(" (lines longer: %d)", d)
	}
	return line
}

// printCurrent prints the note a replacement overwrites, as the gateway's review read shows it. A gateway older than
// 0.8.8 sends none of it: that is said, and the accept is refused locally (acceptNeedsCurrent).
func printCurrent(say func(string), v core.Submission) {
	switch v.TargetState {
	case "":
		say("Current text: not shown (the gateway is older than 0.8.8)")
	case "missing":
		say(fmt.Sprintf("Current text: note %d no longer exists: this submission cannot be accepted", v.Replaces))
	default: // ok, stale
		rev := int64(0)
		if v.CurrentRevision != nil {
			rev = *v.CurrentRevision
		}
		head := "Current text"
		if v.TargetState == "ok" {
			head += " (will be replaced)"
		}
		say(oneLine(fmt.Sprintf("%s: note %d  revision %d  %s  %s  %s", head, v.Replaces, rev, v.CurrentClass, kindOf(v.CurrentKind), v.CurrentTitle)))
		if v.CurrentHidden > 0 {
			say(fmt.Sprintf("the current text has %d hidden characters (not shown); accepting this replacement removes them from the note", v.CurrentHidden))
		}
		if v.TargetState == "stale" {
			say(fmt.Sprintf("the note changed since this submission was written (it expected revision %d, the note is at %d): it cannot be accepted", v.Revision, rev))
		}
		if v.CurrentBody != nil {
			say(core.ReviewText(*v.CurrentBody))
		}
	}
}

// acceptNeedsCurrent: an accept of a replacement needs the current text the gateway shows; an older gateway shows none.
func acceptNeedsCurrent(v core.Submission) error {
	if v.Replaces != 0 && v.TargetState == "" {
		return fmt.Errorf("not accepted: this gateway cannot show the current text of note %d; update the gateway, or reject it", v.Replaces)
	}
	return nil
}

// acceptFields adds what an accept of v states: the review hash and, for a replacement, the revision of the note whose
// current text was shown (a non-replacement sends neither more nor less than before).
func acceptFields(fields map[string]any, v core.Submission) {
	fields["reviewHash"] = v.ReviewHash
	if v.Replaces != 0 && v.CurrentRevision != nil {
		fields["currentRevision"] = *v.CurrentRevision
	}
}

// savedLine tells where the replaced text went after an accepted replacement. team: the team name of a team note ("" for
// a project's note); owner: the commands of the gateway owner.
func savedLine(v core.Submission, team string, owner bool) string {
	rev := int64(0)
	if v.CurrentRevision != nil {
		rev = *v.CurrentRevision
	}
	scope := ""
	if team != "" {
		scope = " --team " + team
	}
	show, restore := "minigw note history", "minigw note restore"
	if owner {
		show, restore, scope = "minigw gw kb history", "minigw gw kb restore", ""
	}
	return clean(fmt.Sprintf("the previous text of note %d is saved as revision %d: %s %d%s shows it, %s %d --revision %d%s puts it back", v.Replaces, rev, show, v.Replaces, scope, restore, v.Replaces, rev, scope))
}

const (
	changedSinceRead = "the submission changed since you read it: review it again"
	hiddenLine       = "contains hidden characters: %d removed from the text shown; it cannot be accepted (reject it and ask for the text without them)"
	acceptNeedsTag   = "not accepted: without a terminal, name the version you read: --version <hash> (kb review shows it)"
	acceptDeclined   = "not accepted"
	versionWhole     = "--version takes the whole version line of kb review (64 hex characters), not a part of it"
)

// confirmAccept decides an accept of the text just printed, whose review hash is hash: with --version only when it is
// exactly that hash (all 64 hex: a prefix could be made to collide by the author, who writes both texts), without it
// only in a terminal after a "y". Nothing is sent unless it returns nil: the
// person (or the script, by naming the version) accepts what was read, never what is there now unseen.
func confirmAccept(hash, version string, terminal bool, in io.Reader, out io.Writer) error {
	if version != "" {
		if len(version) != 64 {
			return errors.New(versionWhole)
		}
		if version != hash {
			return errors.New(changedSinceRead)
		}
		return nil
	}
	if !terminal {
		return errors.New(acceptNeedsTag)
	}
	fmt.Fprint(out, "Accept this text? [y/N] ")
	line, _ := bufio.NewReader(in).ReadString('\n')
	if a := strings.ToLower(strings.TrimSpace(line)); a == "y" || a == "yes" {
		return nil
	}
	return errors.New(acceptDeclined)
}

// kbDecide is a project admin's kb accept|reject <id>: the gateway finds the submission's project and decides who may.
// An accept first reads the submission, prints what it accepts and sends the version it read (the gateway refuses
// when the text changed in between).
func (e *projectEnv) kbDecide(pos []string, team, reason, version string) error {
	if len(pos) != 2 || team != "" {
		return errors.New("usage:\n" + kbHelp)
	}
	id, err := strconv.ParseInt(pos[1], 10, 64)
	if err != nil || id < 1 {
		return errors.New("usage:\n" + kbHelp)
	}
	c, _ := e.connect()
	fields := map[string]any{"reason": reason}
	var shown core.Submission // what an accept showed: its replaced note
	if pos[0] == "accept" {
		st, b, h := e.do(c, "GET", fmt.Sprintf("%s/0/kb/review/%d", apiProjects, id), nil, nil)
		if st != 200 {
			return gatewayError(st, b, h)
		}
		var full struct {
			core.Submission
			AuthorName string
		}
		if err := json.Unmarshal(b, &full); err != nil {
			return err
		}
		printReview(e.say, full.Submission, full.AuthorName, nil)
		if err := acceptNeedsCurrent(full.Submission); err != nil {
			return err
		}
		terminal := e.tty == nil && stdinIsTTY() || e.tty != nil && e.tty()
		if err := confirmAccept(full.ReviewHash, version, terminal, e.in, e.out); err != nil {
			return err
		}
		acceptFields(fields, full.Submission)
		shown = full.Submission
	}
	body, _ := json.Marshal(fields)
	st, b, h := e.do(c, "POST", fmt.Sprintf("%s/0/kb/review/%d/%s", apiProjects, id, pos[0]), nil, body)
	if st != 200 {
		err := gatewayError(st, b, h)
		if pos[0] == "accept" && st == 400 { // a refused accept is not a rejection: nothing was decided
			err = gwStatusError{errors.New(strings.Replace(err.Error(), "rejected: ", "not accepted: ", 1)), st}
		}
		return err
	}
	var v core.Submission
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	e.say(submissionResult(v))
	if pos[0] == "accept" && shown.Replaces != 0 {
		team := ""
		if t, seg := core.SegmentTeam(shown.Project); seg {
			team = e.teamName(c, t)
		}
		e.say(savedLine(shown, team, false))
	}
	return nil
}

// teamName: the name of team id among the teams of this user ("" when not found).
func (e *projectEnv) teamName(c connection, id int64) string {
	st, b, _ := e.do(c, "GET", "/minigw/v1/teams", nil, nil)
	var ts []struct {
		Name    string
		Segment int64
	}
	if st == 200 && json.Unmarshal(b, &ts) == nil {
		for _, t := range ts {
			if tid, ok := core.SegmentTeam(t.Segment); ok && tid == id {
				return t.Name
			}
		}
	}
	return ""
}

func searchLine(v core.SearchHit) string {
	from := ""
	if v.From != "" {
		from = "  from " + v.From
	}
	return clean(fmt.Sprintf("%d  %s  %s  %s  %s%s%s\n%s", v.ID, oneLine(v.Title), subjectOf(v.Project, v.Team), v.Class, noteTime(v.Updated), kindSuffix(v.EntryKind), from, v.Snippet))
}

// kbFAQ: minigw kb faq [<project>] [--json]. The pages are followed (at most faqPages).
func (e *projectEnv) kbFAQ(args []string) error {
	fs := flag.NewFlagSet("kb faq", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "")
	team := fs.String("team", "", "")
	pos, err := parseFlags(fs, args)
	if err != nil || len(pos) > 1 {
		return errors.New("usage: minigw kb faq [<project>|--team <team>] [--json]")
	}
	if *team != "" && len(pos) > 0 {
		return errors.New(teamProjectUsage)
	}
	var c connection
	var project int64
	if *team != "" {
		c, _ = e.connect()
		if project, err = e.teamSegment(c, *team); err != nil {
			return err
		}
	} else if len(pos) == 1 {
		c, _ = e.connect()
		if project, err = e.projectIDOf(c, pos[0]); err != nil {
			return err
		}
	} else {
		cc, link, err := e.linkedProject("")
		if err != nil {
			return err
		}
		c, project = cc, link.Project
	}
	type entry struct {
		ID      int64
		Title   string
		Class   string
		Answer  string
		Updated time.Time
		From    string
	}
	var entries []entry
	raw := []any{}
	after := ""
	for page := 0; page < faqPages; page++ {
		path := fmt.Sprintf("%s/%d/kb/faq", apiProjects, project)
		if after != "" {
			path += "?after=" + url.QueryEscape(after)
		}
		st, b, h := e.do(c, "GET", path, nil, nil)
		if st != 200 {
			return gatewayError(st, b, h)
		}
		var pg struct {
			Entries []entry
			Next    string
		}
		if err = json.Unmarshal(b, &pg); err != nil {
			return err
		}
		entries = append(entries, pg.Entries...)
		var obj struct{ Entries []any } // exact numbers for --json
		dec := json.NewDecoder(strings.NewReader(string(b)))
		dec.UseNumber()
		if err = dec.Decode(&obj); err != nil {
			return err
		}
		raw = append(raw, obj.Entries...)
		if after = pg.Next; after == "" {
			break
		}
	}
	if *asJSON {
		b, _ := json.Marshal(map[string]any{"entries": raw})
		e.say(jsonText(string(b)))
		return nil
	}
	for _, v := range entries {
		from := ""
		if v.From != "" { // another gateway's entry: its answer starts with the marker line
			from = "  from " + v.From
		}
		e.say(clean(fmt.Sprintf("%d  %s  %s  %s%s", v.ID, oneLine(v.Title), v.Class, noteTime(v.Updated), from)))
		e.say(clean(v.Answer))
	}
	if len(entries) == 0 {
		e.say("no faq entries")
	}
	return nil
}

// projectIDOf resolves a project given by slug (or id) to its id.
func (e *projectEnv) projectIDOf(c connection, ref string) (int64, error) {
	st, b, h := e.do(c, "GET", apiProjects+"/"+url.PathEscape(ref), nil, nil)
	if st != 200 {
		return 0, gatewayError(st, b, h)
	}
	var p struct{ ID int64 }
	if err := json.Unmarshal(b, &p); err != nil {
		return 0, err
	}
	return p.ID, nil
}
func (e *projectEnv) kbSearch(args []string) error {
	fs := flag.NewFlagSet("kb search", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "")
	team := fs.String("team", "", "")
	pos, err := parseFlags(fs, args)
	if err != nil || len(pos) < 1 || len(pos) > 2 {
		return errors.New("usage: minigw kb search [<project>|--team <team>] <query> [--json]")
	}
	var c connection
	base := "/minigw/v1/kb/search"
	if *team != "" {
		if e.agent || e.runConn != nil {
			return errors.New(teamAgentRefusal)
		}
		if len(pos) != 1 {
			return errors.New(teamProjectUsage)
		}
		c, _ = e.connect()
		id, err := e.teamSegment(c, *team)
		if err != nil {
			return err
		}
		base = fmt.Sprintf("%s/%d/kb/search", apiProjects, id)
	} else if e.runConn != nil {
		if len(pos) != 1 {
			return errors.New("inside an agent run the project is the run's own: a project argument is not accepted")
		}
		c = *e.runConn
	} else {
		if e.agent {
			return errors.New(agentRefusal)
		}
		if len(pos) == 1 {
			cc, link, err := e.linkedProject("")
			if err != nil {
				return err
			}
			c = cc
			base = fmt.Sprintf("%s/%d/kb/search", apiProjects, link.Project)
		} else {
			c, _ = e.connect()
			st, b, h := e.do(c, "GET", apiProjects+"/"+url.PathEscape(pos[0]), nil, nil)
			if st != 200 {
				return gatewayError(st, b, h)
			}
			var p struct{ ID int64 }
			if err = json.Unmarshal(b, &p); err != nil {
				return err
			}
			base = fmt.Sprintf("%s/%d/kb/search", apiProjects, p.ID)
		}
	}
	st, b, h := e.do(c, "GET", base+"?q="+url.QueryEscape(pos[len(pos)-1]), nil, nil)
	if st != 200 {
		return gatewayError(st, b, h)
	}
	if *asJSON {
		e.say(jsonText(strings.TrimSpace(string(b))))
		return nil
	}
	var pg core.SearchPage
	if err = json.Unmarshal(b, &pg); err != nil {
		return err
	}
	for _, v := range pg.Hits {
		e.say(searchLine(v))
	}
	if pg.Note != "" {
		e.say(clean(pg.Note))
	}
	if len(pg.Hits) == 0 {
		e.say("no hits")
	} else if *team != "" {
		e.say("read a team's note with: minigw note read <id> --team " + clean(*team)) // the folder's own project cannot read it
	} else if len(pos) == 2 {
		e.say("read a note of project " + clean(pos[0]) + " from a folder linked to it: minigw note read <id>")
	}
	return nil
}

// adminTerminal: is the owner's standard input a terminal (tests replace it).
var adminTerminal = stdinIsTTY

func kbAdmin(a *adminClient, pos []string, o coreOpts) {
	reason, team := o.reason, o.team
	usage := func() { fail("usage:\n" + kbAdminHelp) }
	if len(pos) < 2 {
		usage()
		return
	}
	if len(pos) == 3 && (pos[1] == "history" || pos[1] == "restore") {
		if o.team != "" || o.project != "" {
			usage()
			return
		}
		id, err := strconv.ParseInt(pos[2], 10, 64)
		if err != nil || id < 1 {
			usage()
			return
		}
		a.noteCommand = "note " + pos[1]
		err = noteHistoryCommand(pos[1], "/kb/notes/"+pos[2], id, o.revision, o.hasRevision, o.current, o.hasCurrent, o.class, o.json, adminTerminal(), o.in, os.Stdout, func(method, path string, body []byte) ([]byte, error) {
			var out json.RawMessage
			var payload any
			if body != nil {
				payload = json.RawMessage(body)
			}
			a.do(method, path, payload, &out)
			return []byte(out), nil
		})
		if err != nil {
			fail(clean(err.Error()))
		}
		return
	}
	if len(pos) == 2 && pos[1] == "rebuild-search" {
		a.do("POST", "/kb/search/rebuild", nil, nil)
		say("search index rebuilt")
		return
	}
	if pos[1] == "review" && len(pos) <= 3 && (team == "" || len(pos) == 2) {
		project := int64(0)
		if len(pos) == 3 {
			project = projectID(a, pos[2])
		}
		if team != "" {
			project = core.TeamSegment(teamID(a, team))
		}
		var names map[int64]string // the notes of a team print as "team <name>"; asked for only when one shows up
		teamNames := func() map[int64]string {
			if names == nil {
				names = map[int64]string{}
				var ts struct{ Teams []cliTeam }
				a.do("GET", "/teams", nil, &ts)
				for _, t := range ts.Teams {
					names[t.ID] = t.Name
				}
			}
			return names
		}
		after := int64(0)
		count := 0
		for {
			var pg struct {
				Submissions []core.Submission
				Next        int64
			}
			a.do("GET", fmt.Sprintf("/kb/submissions?project=%d&after=%d", project, after), nil, &pg)
			for _, v := range pg.Submissions {
				var full struct {
					core.Submission
					AuthorName string // the admin answer names the writer; an older gateway has only the number
				}
				a.do("GET", "/kb/submissions/"+strconv.FormatInt(v.ID, 10), nil, &full)
				var known map[int64]string
				if _, seg := core.SegmentTeam(full.Project); seg {
					known = teamNames()
				}
				printReview(say, full.Submission, full.AuthorName, known)
				count++
			}
			if pg.Next == 0 {
				break
			}
			after = pg.Next
		}
		if count == 0 {
			say("no submissions")
		}
		return
	}
	if len(pos) == 3 && (pos[1] == "accept" || pos[1] == "reject") {
		id, err := strconv.ParseInt(pos[2], 10, 64)
		if err != nil || id < 1 {
			usage()
			return
		}
		fields := map[string]any{"reason": reason}
		var shown core.Submission
		if pos[1] == "accept" { // what is accepted, and the version read: the gateway refuses a text changed since
			var full struct {
				core.Submission
				AuthorName string
			}
			a.do("GET", "/kb/submissions/"+strconv.FormatInt(id, 10), nil, &full)
			printReview(say, full.Submission, full.AuthorName, nil)
			if err := acceptNeedsCurrent(full.Submission); err != nil {
				fail(err.Error())
				return
			}
			if err := confirmAccept(full.ReviewHash, o.version, adminTerminal(), o.in, os.Stdout); err != nil {
				fail(err.Error())
				return
			}
			acceptFields(fields, full.Submission)
			shown = full.Submission
		}
		var v core.Submission
		a.do("POST", fmt.Sprintf("/kb/submissions/%d/%s", id, pos[1]), fields, &v)
		say(submissionResult(v))
		if pos[1] == "accept" && shown.Replaces != 0 {
			say(savedLine(shown, "", true))
		}
		return
	}
	usage()
}

func newNoteKey() string { b := make([]byte, 16); rand.Read(b); return "mgw-" + hex.EncodeToString(b) }
func (e *projectEnv) push(c connection, base, key string, payload []byte) (st int, b []byte, h http.Header) {
	for i := 0; i <= pushRetries; i++ {
		st, b, h = e.do(c, "POST", base, map[string]string{"Idempotency-Key": key}, payload)
		if st != 0 {
			break
		}
		e.sleep(time.Second)
	}
	return
}
