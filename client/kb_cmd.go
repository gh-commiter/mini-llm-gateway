package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"minigw/core"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// faqPages bounds the pages of kb faq (50 entries each).
const faqPages = 20

const kbHelp = `minigw kb export <project>|--team <team> <dir>
minigw kb import <project>|--team <team> <dir|file.md> [--class c] [--submit]
minigw kb mcp [--project slug] [--print-config]
minigw kb search [<project>|--team <team>] <query> [--json]   a project search also covers its team's notes (as far as you may read them); a hit from there ends with "from team:<name>"
minigw kb submit <project>|--team <team> [--class structure|data] [--kind finding|faq] [--title t] [--key k] [--replaces id --revision n] <file|->   faq: the title is the question, the text a short answer (up to 2 KiB); private is for projects only
minigw kb faq [<project>|--team <team>] [--json]   the project's (or the team's) questions and answers
minigw kb submissions [<project>|--team <team>] [--json]   your submissions (all projects and your teams' own notes if omitted)
minigw kb hooks on|off|status [<project>]   the plugin's hooks that ask a Claude Code session to hand findings in (off by default, per project, on this computer)`
const kbAdminHelp = `minigw gw kb rebuild-search
minigw gw kb review [<project>|--team <team>]   pending submissions, including bodies
minigw gw kb accept <id>
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
	pos, err := parseFlags(fs, args)
	usage := errors.New("usage:\n" + kbHelp)
	if err != nil || len(pos) == 0 {
		return usage
	}
	if pos[0] != "submit" && pos[0] != "submissions" {
		return usage
	}
	if *team != "" {
		if pos[0] == "submit" && len(pos) != 2 || pos[0] == "submissions" && len(pos) != 1 {
			return errors.New(teamProjectUsage)
		}
		if pos[0] == "submit" {
			pos = []string{"submit", "", pos[1]}
		}
	} else if pos[0] == "submit" && len(pos) != 3 || pos[0] == "submissions" && len(pos) > 2 {
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
	}
	if len(all) == 0 {
		e.say("no submissions")
	}
	return nil
}

func searchLine(v core.SearchHit) string {
	from := ""
	if v.From != "" {
		from = "  from " + v.From
	}
	return clean(fmt.Sprintf("%d  %s  %s  %s  %s%s%s\n%s", v.ID, v.Title, subjectOf(v.Project, v.Team), v.Class, noteTime(v.Updated), kindSuffix(v.EntryKind), from, v.Snippet))
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
		e.say(clean(fmt.Sprintf("%d  %s  %s  %s", v.ID, v.Title, v.Class, noteTime(v.Updated))))
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
	if len(pg.Hits) == 0 {
		e.say("no hits")
	} else if *team != "" {
		e.say("read a team's note with: minigw note read <id> --team " + clean(*team)) // the folder's own project cannot read it
	} else if len(pos) == 2 {
		e.say("read a note of project " + clean(pos[0]) + " from a folder linked to it: minigw note read <id>")
	}
	return nil
}

func kbAdmin(a *adminClient, pos []string, reason, team string) {
	usage := func() { fail("usage:\n" + kbAdminHelp) }
	if len(pos) < 2 {
		usage()
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
				var full core.Submission
				a.do("GET", "/kb/submissions/"+strconv.FormatInt(v.ID, 10), nil, &full)
				var known map[int64]string
				if _, seg := core.SegmentTeam(full.Project); seg {
					known = teamNames()
				}
				say(submissionLine(full, known))
				say(clean(fmt.Sprintf("writer %d  device %s  replaces %d  revision %d", full.User, full.Provenance.Device, full.Replaces, full.Revision)))
				if full.Body != nil {
					say(clean(*full.Body))
				}
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
		var v core.Submission
		a.do("POST", fmt.Sprintf("/kb/submissions/%d/%s", id, pos[1]), map[string]any{"reason": reason}, &v)
		say(submissionResult(v))
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
