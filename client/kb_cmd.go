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

const kbHelp = `minigw kb export <project> <dir>
minigw kb import <project> <dir|file.md> [--class c] [--submit]
minigw kb mcp [--project slug] [--print-config]
minigw kb search [<project>] <query> [--json]
minigw kb submit <project> [--class structure|data|private] [--title t] [--key k] [--replaces id --revision n] <file|->
minigw kb submissions [<project>] [--json]   your submissions (all projects if omitted)`
const kbAdminHelp = `minigw gw kb rebuild-search
minigw gw kb review [<project>]   pending submissions, including bodies
minigw gw kb accept <id>
minigw gw kb reject <id> [--reason text]`

func submissionLine(v core.Submission) string {
	return clean(fmt.Sprintf("submission %d  project %d  %s  %s  %s", v.ID, v.Project, v.State, v.Class, v.Title))
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
	if e.agent {
		return errors.New(agentRefusal)
	}
	fs := flag.NewFlagSet("kb", flag.ContinueOnError)
	class := fs.String("class", "data", "")
	title := fs.String("title", "", "")
	key := fs.String("key", "", "")
	replaces := fs.Int64("replaces", 0, "")
	revision := fs.Int64("revision", 0, "")
	asJSON := fs.Bool("json", false, "")
	pos, err := parseFlags(fs, args)
	usage := errors.New("usage:\n" + kbHelp)
	if err != nil || len(pos) == 0 {
		return usage
	}
	if pos[0] != "submit" && pos[0] != "submissions" {
		return usage
	}
	if pos[0] == "submit" && len(pos) != 3 || pos[0] == "submissions" && len(pos) > 2 {
		return usage
	}
	c, _ := e.connect()
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
	if len(pos) > 1 {
		project, err = resolve(pos[1])
		if err != nil {
			return err
		}
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
		payload, _ := json.Marshal(map[string]any{"class": *class, "title": t, "body": body, "replaces": *replaces, "revision": *revision})
		st, b, h := e.push(c, base, k, payload)
		if st == 0 {
			return fmt.Errorf("no answer from the gateway; repeat with --key %s (safe: no duplicate), or check with minigw kb submissions", k)
		}
		if st != 201 {
			return gatewayError(st, b, h)
		}
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
		e.say(submissionLine(v))
	}
	if len(all) == 0 {
		e.say("no submissions")
	}
	return nil
}

func searchLine(v core.SearchHit) string {
	return clean(fmt.Sprintf("%d  %s  project %d  %s  %s\n%s", v.ID, v.Title, v.Project, v.Class, noteTime(v.Updated), v.Snippet))
}
func (e *projectEnv) kbSearch(args []string) error {
	fs := flag.NewFlagSet("kb search", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "")
	pos, err := parseFlags(fs, args)
	if err != nil || len(pos) < 1 || len(pos) > 2 {
		return errors.New("usage: minigw kb search [<project>] <query> [--json]")
	}
	var c connection
	base := "/minigw/v1/kb/search"
	if e.runConn != nil {
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
	}
	return nil
}

func kbAdmin(a *adminClient, pos []string, reason string) {
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
	if pos[1] == "review" && len(pos) <= 3 {
		project := int64(0)
		if len(pos) == 3 {
			project = projectID(a, pos[2])
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
				say(submissionLine(full))
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
