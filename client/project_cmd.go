package main

// minigw project list|link|unlink|show and minigw note list|read|push|edit|rm: the member's side of the gateway's
// projects (docs/plans/contracts-0.6.0.md sections 3 and 6). The commands work on a projectEnv so that tests can
// run them against an in-process gateway with a temporary state file.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	usageHelp   = `minigw usage [--project slug] [--days n] [--json]   your model usage (a developer sees own events in a project, an admin all of it)`
	projectHelp = `minigw project list [--archived] [--path <prefix>] [--json]   the projects you can see (role, and what a grant gives you); --path: only those of your teams whose path label equals the prefix or starts with prefix/
minigw project link [<slug>]                link this folder (and its subfolders) to a project you are a member of; without a slug the project is found by this folder's git remote
minigw project remote list [<slug>]         the git remotes registered for a project (default: the linked one)
minigw project remote add <url>|--here [<slug>]   register a git remote (project admin); --here: this folder's origin
minigw project remote remove <url> [<slug>]
minigw project depends [<slug>]             the projects and teams this project depends on (those you may see) (default: the linked one); a search of the project also covers them
minigw project unlink                       remove the link that applies here
minigw project show                         the link that applies here, and why
minigw project sessions [<project>] [--days n] [--model m] [--device d] [--phase p] [--activity a] [--task t] [--json]`
	noteHelp = `minigw note list [--class c] [--limit n] [--after cursor] [--json]   notes of the linked project
minigw note read <id> [--json]
minigw note push [--class structure|data|private] [--title t] [--key k] <file|->   new note (default class data)
minigw note edit <id> [--class c] [--title t] [--revision n] [<file|->]
minigw note history <id> [--revision n] [--json] [--project slug|--team team]
minigw note restore <id> --revision n [--class c] [--current n] [--project slug|--team team]
minigw note rm <id> [--revision n]        (inside an agent run only note list and note read work, on the run's own project)
   --project <slug> only confirms the project this folder is linked to; classes: structure, data, private
   (private = visible to you and the project admins)
   --team <team> on any of these: the team's own notes instead of the linked project (every team member reads, a project admin of the team writes; structure and data only)`
	privateNote  = "private = visible to you and the project admins"
	agentRefusal = "inside an agent run only minigw kb search, note list and note read work (on the run's own project); link, push, edit, rm and project commands are not available"
	maxNoteBody  = 64 << 10
	maxNoteTitle = 256
	pushRetries  = 3
	apiProjects  = "/minigw/v1/projects"
)

type projectEnv struct {
	connect func() (connection, string) // the routed connection and the gateway's identity (only when needed)
	state   string                      // projects.json
	cwd     string
	in      io.Reader
	out     io.Writer
	err     io.Writer
	agent   bool
	runConn *connection // inside an agent run: the run port and its token; only note list|read work, on the run's own project
	do      func(c connection, method, path string, headers map[string]string, body []byte) (int, []byte, http.Header)
	sleep   func(time.Duration)
	origin  func(dir string) string // the raw remote.origin.url of the folder's repository, "" = none; nil = ask git
	// kb hooks (kb_hook.go), per instance so that parallel tests do not interfere; nil = the real thing
	gateway  func() string                          // the default profile's gateway id, "" = not connected
	gitState func(dir string) (head, status string) // HEAD and the status hash of the linked folder
	now      func() time.Time
	tty      func() bool // kb accept: is stdin a terminal; nil = ask the real one
}

// projectMain, noteMain and usageMain are the entry points of main().
func projectMain(args []string) { runProjectCmd("project", args) }
func noteMain(args []string)    { runProjectCmd("note", args) }
func usageMain(args []string)   { runProjectCmd("usage", args) }
func runningMain(args []string) { runProjectCmd("running", args) }

func runProjectCmd(kind string, args []string) {
	cwd, _ := os.Getwd()
	e := &projectEnv{state: projectStatePath(), cwd: cwd, in: os.Stdin, out: os.Stdout, err: os.Stderr, agent: runEnvironment(), do: olderRouteHint(realDo), sleep: time.Sleep}
	e.connect = func() (connection, string) {
		p := requireProfile()
		return route(p), gatewayID(p.conn)
	}
	if ac := agentConn(); ac != nil { // the run port gives the run its project's notes, read only
		e.runConn = ac
		e.connect = func() (connection, string) { return *ac, "" }
	}
	var err error
	switch kind {
	case "kb":
		err = e.kb(args)
	case "team":
		err = e.team(args)
	case "note":
		err = e.note(args)
	case "usage":
		err = e.usage(args)
	case "running":
		err = e.running(args)
	default:
		err = e.project(args)
	}
	if err != nil {
		fail(withGatewayHint(err))
	}
}

// realDo: like call() but with the response headers (Retry-After).
func realDo(c connection, method, path string, headers map[string]string, body []byte) (int, []byte, http.Header) {
	req, err := http.NewRequest(method, c.URL+path, strings.NewReader(string(body)))
	if err != nil {
		return 0, nil, nil
	}
	req.Header.Set("authorization", "Bearer "+c.Token)
	req.Header.Set("X-Minigw-Client", clientInfo())
	if body != nil {
		req.Header.Set("content-type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second, CheckRedirect: noRedirect}).Do(req)
	if err != nil {
		return 0, nil, nil
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return resp.StatusCode, data, resp.Header
}

func (e *projectEnv) say(s string) { fmt.Fprintln(e.out, s) }

// noteTime: a note's time as note list and note read show it: local time, to the minute.
func noteTime(t time.Time) string { return t.Local().Format("2006-01-02 15:04") }

// gwStatusError is a gateway's answer (as gatewayError words it) with the HTTP status it came with.
type gwStatusError struct {
	error
	status int
}

func (e gwStatusError) Unwrap() error { return e.error }

// gatewayError: section 6's answers as short sentences. 403 forbidden is printed plainly.
func gatewayError(st int, data []byte, h http.Header) error {
	return gwStatusError{gatewayAnswer(st, data, h), st}
}

// gatewayHintText: a refusal or a "not found" of a gateway says which gateway it came from when the client has more than
// one profile (the default profile may be another gateway than the one the user means). Shared fixture testdata/gateway-hint.json.
func gatewayHintText(msg string, status int, profile, url string, profiles int) string {
	if (status != 403 && status != 404) || profiles < 2 || profile == "" {
		return msg
	}
	return msg + "\nhint: this answer came from profile " + profile + " (" + url + "); to ask another gateway add --minigw-profile <name> (minigw profiles lists them)"
}

// withGatewayHint adds that line to an error that is a gateway's refusal.
func withGatewayHint(err error) string {
	var g gwStatusError
	if !errors.As(err, &g) || agentConn() != nil {
		return err.Error()
	}
	names := profileNames()
	ps := loadProfiles()
	if len(ps) == 0 {
		return err.Error()
	}
	return gatewayHintText(err.Error(), g.status, ps[0].name, ps[0].conn.URL, len(names))
}

func gatewayAnswer(st int, data []byte, h http.Header) error {
	var b struct {
		Error struct {
			Type, Message string
			Revision      int64
		}
	}
	json.Unmarshal(data, &b)
	switch st {
	case 0:
		return errors.New("no answer from the gateway")
	case 400:
		return errors.New("rejected: " + oneLine(b.Error.Message))
	case 403:
		switch b.Error.Type {
		case "forbidden":
			return errors.New("not allowed: your role in this project does not permit it (a grant alone only lets you read; linking needs a direct role)")
		case "archived":
			return errors.New("the project is archived: nobody can write to it")
		}
		if m := oneLine(b.Error.Message); strings.HasPrefix(m, "no project access:") { // the gateway names the condition
			return errors.New(m)
		}
		return errors.New("no project access: this device is not approved or not bound to a user on the gateway (ask the gateway owner)")
	case 404:
		if b.Error.Type == "older_gateway" { // olderRouteHint: the gateway does not have this command's routes yet
			return errors.New(oneLine(b.Error.Message))
		}
		return errors.New("not found or not visible")
	case 409:
		if b.Error.Type == "revision_mismatch" {
			return fmt.Errorf("the note has changed: its current revision is %d (read it again, then retry)", b.Error.Revision)
		}
		return errors.New("this idempotency key was already used for another note")
	case 413:
		if b.Error.Type == "pending_limit" {
			return errors.New(oneLine(b.Error.Message))
		}
		return fmt.Errorf("too large (title up to %d bytes, body up to %d KiB, project limits apply)", maxNoteTitle, maxNoteBody>>10)
	case 429:
		wait := "a minute"
		if s, err := strconv.Atoi(h.Get("Retry-After")); err == nil {
			wait = fmt.Sprintf("%d seconds", s)
		}
		return errors.New("too many requests: try again in " + wait)
	case 503:
		return errors.New("the gateway's project store is not available")
	}
	return fmt.Errorf("gateway answered HTTP %d", st)
}

// olderRouteHint wraps the gateway call: a 404 on the routes of the kb (first in 0.7.10) or team (0.7.1) routes from a
// gateway older than that becomes a sentence that names the gateway's version and the update, instead of "not found or not visible".
// The version is asked (/health/ready) only after such a 404, once per command.
func olderRouteHint(do func(connection, string, string, map[string]string, []byte) (int, []byte, http.Header)) func(connection, string, string, map[string]string, []byte) (int, []byte, http.Header) {
	version := ""
	return func(c connection, method, path string, headers map[string]string, body []byte) (int, []byte, http.Header) {
		st, data, h := do(c, method, path, headers, body)
		family, min := "", ""
		switch {
		case st != 404:
			return st, data, h
		case strings.HasPrefix(path, "/minigw/v1/teams"):
			family, min = "team", "0.7.1"
		case strings.HasPrefix(path, apiProjects+"/") && strings.Contains(path, "/kb/"):
			family, min = "kb", "0.7.10"
		default:
			return st, data, h
		}
		if version == "" {
			hs, hd, _ := do(c, "GET", "/health/ready", nil, nil)
			var r struct{ Version string }
			if hs != 200 || json.Unmarshal(hd, &r) != nil || strings.Trim(r.Version, "0123456789.") != "" {
				return st, data, h // the version is not known: the plain answer
			}
			if version = r.Version; version == "" {
				version = "0.5" // a gateway that reports none is older than 0.6.0
			}
		}
		if !versionLess(version, min) {
			return st, data, h
		}
		msg, _ := json.Marshal(map[string]any{"error": map[string]string{"type": "older_gateway",
			"message": fmt.Sprintf("this gateway is %s; %s needs %s or newer: update the gateway (/minigw:gw update there)", version, family, min)}})
		return st, msg, h
	}
}

// olderGatewayHint: the gateway's version (/health/ready) when it is below 0.6.0, else "". A gateway that reports none is older still.
func (e *projectEnv) olderGatewayHint(c connection) string {
	st, data, _ := e.do(c, "GET", "/health/ready", nil, nil)
	var r struct{ Version string }
	if st != 200 || json.Unmarshal(data, &r) != nil || (r.Version != "" && strings.Trim(r.Version, "0123456789.") != "") {
		return ""
	}
	if r.Version == "" {
		return "hint: this gateway is older than 0.6.0 and has no projects (update the gateway)"
	}
	if !versionLess(r.Version, "0.6.0") {
		return ""
	}
	return "hint: this gateway runs " + r.Version + ", older than 0.6.0: it has no projects yet (update the gateway)"
}

// parseFlags: flags and positionals in any order.
func parseFlags(fs *flag.FlagSet, args []string) ([]string, error) {
	fs.SetOutput(io.Discard)
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		if args = fs.Args(); len(args) == 0 {
			return pos, nil
		}
		pos, args = append(pos, args[0]), args[1:]
	}
}

func (e *projectEnv) state3() (projectState, error) { return loadProjectState(e.state) }

func (e *projectEnv) project(args []string) error {
	if e.agent {
		return errors.New(agentRefusal)
	}
	if len(args) == 0 {
		return errors.New("usage:\n" + projectHelp)
	}
	if args[0] == "sessions" {
		return e.projectSessions(args[1:])
	}
	fs := flag.NewFlagSet("project", flag.ContinueOnError)
	archived := fs.Bool("archived", false, "also archived projects")
	asJSON := fs.Bool("json", false, "JSON")
	prefix := fs.String("path", "", "team path prefix")
	here := fs.Bool("here", false, "this folder's git remote")
	pos, err := parseFlags(fs, args[1:])
	if err != nil {
		return errors.New("usage:\n" + projectHelp)
	}
	switch args[0] {
	case "list":
		c, gw := e.connect()
		path := apiProjects
		q := url.Values{}
		if *archived {
			q.Set("archived", "1")
		}
		if *prefix != "" {
			q.Set("path", *prefix)
		}
		if len(q) > 0 {
			path += "?" + q.Encode()
		}
		st, data, h := e.do(c, "GET", path, nil, nil)
		if st != 200 {
			err := gatewayError(st, data, h)
			if st == 404 { // a gateway older than 0.6.0 has no projects: say so, instead of "not found"
				if hint := e.olderGatewayHint(c); hint != "" {
					err = errors.New(err.Error() + "\n" + hint)
				}
			}
			return err
		}
		if *asJSON {
			e.say(strings.TrimSpace(string(data)))
			return nil
		}
		var list []struct {
			ID       int64
			Slug     string
			Name     string
			Archived bool
			Role     string
			Origins  []cliOrigin
			Granted  []string
			Pending  int `json:"pendingReview"`
		}
		json.Unmarshal(data, &list)
		ps, _ := e.state3()
		here, _ := projectFor(ps, gw, e.cwd)
		for _, p := range list {
			access := p.Role
			if len(p.Origins) > 0 {
				access += " (from " + originText(p.Origins, false) + ")"
			}
			if len(p.Granted) > 0 {
				if access != "" {
					access += ", "
				}
				access += "reads " + strings.Join(p.Granted, "+") + " by grant"
			}
			line := fmt.Sprintf("%-24s %-28s %s", p.Slug, access, clean(p.Name))
			if p.Archived {
				line += "  (archived)"
			}
			if here.State != noLink && here.Link.Project == p.ID {
				line += "  (linked here)"
			}
			if p.Pending == 1 {
				line += "  (1 submission waits for review: minigw kb review " + p.Slug + ")"
			} else if p.Pending > 1 {
				line += fmt.Sprintf("  (%d submissions wait for review: minigw kb review %s)", p.Pending, p.Slug)
			}
			e.say(clean(line))
		}
		if len(list) == 0 {
			e.say("no projects: ask the gateway owner to add you to one")
		}
	case "link":
		if len(pos) > 1 {
			return errors.New("usage: minigw project link [<slug>]")
		}
		if len(pos) == 0 {
			return e.link("")
		}
		return e.link(pos[0])
	case "remote":
		return e.remote(pos, *here)
	case "depends":
		return e.depends(pos)
	case "unlink":
		if len(pos) != 0 {
			return errors.New("usage: minigw project unlink")
		}
		return e.unlink()
	case "show":
		return e.show()
	default:
		return errors.New("unknown command minigw project " + args[0] + "\n" + projectHelp)
	}
	return nil
}

func (e *projectEnv) link(slug string) error {
	if slug != "" && !hintRe.MatchString(slug) {
		return errors.New("not a project slug (lower case letters, digits and -): " + slug)
	}
	c, gw := e.connect()
	st, err := e.state3()
	if err != nil {
		return err
	}
	real, err := realFolder(e.cwd)
	if err != nil {
		return err
	}
	for _, l := range st.Links {
		if l.Gateway == gw && sameProjectPath(real, l.Path) { // a deeper folder may be linked on its own; the same one not twice
			return fmt.Errorf("this folder is already linked to %s: run minigw project unlink first", l.Slug)
		}
	}
	if slug == "" {
		if slug, err = e.findByRemote(c, real); err != nil {
			return err
		}
	}
	code, data, h := e.do(c, "POST", apiProjects+"/"+url.PathEscape(slug)+"/link-check", nil, nil)
	if code != 200 {
		return gatewayError(code, data, h)
	}
	var p struct {
		ID   int64
		Slug string
	}
	json.Unmarshal(data, &p)
	if p.ID == 0 {
		return errors.New("the gateway sent no project")
	}
	st.Links = append(st.Links, projectLink{Gateway: gw, Project: p.ID, Slug: p.Slug, Path: real})
	if err := saveProjectState(e.state, st); err != nil {
		return errors.New("cannot save the link: " + err.Error())
	}
	e.say(fmt.Sprintf("linked %s to project %s (id %d)", real, p.Slug, p.ID))
	if h := readHint(real); h != "" && h != p.Slug {
		fmt.Fprintf(e.err, "minigw: warning: %s says project %s; runs here are refused until you remove or fix it\n", hintFile, h)
	}
	return nil
}

func (e *projectEnv) unlink() error {
	_, gw := e.connect()
	st, err := e.state3()
	if err != nil {
		return err
	}
	res, err := projectFor(st, gw, e.cwd)
	if err != nil {
		return err
	}
	if res.State == noLink {
		return errors.New("this folder is not linked to a project")
	}
	var keep []projectLink
	for _, l := range st.Links {
		if !(l.Gateway == gw && l.Path == res.Link.Path) {
			keep = append(keep, l)
		}
	}
	st.Links = keep
	if err := saveProjectState(e.state, st); err != nil {
		return errors.New("cannot save: " + err.Error())
	}
	e.say(fmt.Sprintf("unlinked %s from project %s", res.Link.Path, res.Link.Slug))
	return nil
}

func (e *projectEnv) show() error {
	_, gw := e.connect()
	st, err := e.state3()
	if err != nil {
		return err
	}
	res, err := projectFor(st, gw, e.cwd)
	if err != nil {
		return err
	}
	e.say("folder:   " + res.RealPath)
	switch res.State {
	case noLink:
		e.say("project:  none (not linked)")
		if res.Hint != "" {
			e.say(fmt.Sprintf("hint:     %s in %s suggests project %s; run minigw project link %s", hintFile, res.HintDir, res.Hint, res.Hint))
		}
	default:
		e.say(fmt.Sprintf("project:  %s (id %d)", res.Link.Slug, res.Link.Project))
		e.say("linked:   " + res.Link.Path + " (the nearest linked folder above or at this one)")
		if res.Hint == "" {
			e.say("hint:     no " + hintFile + " in the linked folder")
		} else if res.State == linkHintDiff {
			e.say(fmt.Sprintf("hint:     %s says %s but the link is %s: runs here are refused; run minigw project unlink, link again or remove the hint", hintFile, res.Hint, res.Link.Slug))
		} else {
			e.say("hint:     " + hintFile + " agrees")
		}
	}
	e.say("gateway: " + gw)
	return nil
}

func notLinkedMessage(res projectResolution) string {
	if res.Hint != "" {
		return fmt.Sprintf("this folder suggests project %s (%s); run minigw project link %s", res.Hint, hintFile, res.Hint)
	}
	return "this folder is not linked to a project: run minigw project link <slug>"
}

// linkedProject is the project of the current folder for the note commands; --project only confirms it.
func (e *projectEnv) linkedProject(flagSlug string) (connection, projectLink, error) {
	c, gw := e.connect()
	st, err := e.state3()
	if err != nil {
		return c, projectLink{}, err
	}
	res, err := projectFor(st, gw, e.cwd)
	if err != nil {
		return c, projectLink{}, err
	}
	switch res.State {
	case noLink:
		return c, projectLink{}, errors.New(notLinkedMessage(res))
	case linkHintDiff:
		return c, projectLink{}, errors.New(hintDiffMessage(res))
	}
	if flagSlug != "" && flagSlug != res.Link.Slug {
		return c, projectLink{}, fmt.Errorf("this folder is linked to %s, not %s: --project only names the linked project (link the other checkout instead)", res.Link.Slug, flagSlug)
	}
	return c, res.Link, nil
}

type noteView struct {
	ID       int64
	Class    string
	Title    string
	Body     *string
	Author   string
	Editor   string
	Updated  time.Time
	Revision int64
	Upstream string // a note of another gateway's mirror: its title is quoted like its text
}

// editedBy formats the editor of a note that someone other than its author last changed ("" otherwise).
func editedBy(n noteView, format string) string {
	if n.Editor == "" {
		return ""
	}
	return fmt.Sprintf(format, n.Editor)
}

func readInput(in io.Reader, file string) (string, error) {
	var r io.Reader = in
	if file != "-" {
		f, err := os.Open(file)
		if err != nil {
			return "", err
		}
		defer f.Close()
		r = f
	}
	b, err := io.ReadAll(io.LimitReader(r, maxNoteBody+1))
	if err != nil {
		return "", err
	}
	if len(b) > maxNoteBody {
		return "", fmt.Errorf("too large: a note body is at most %d KiB", maxNoteBody>>10)
	}
	if !utf8.Valid(b) {
		return "", errors.New("the note must be text (UTF-8)")
	}
	return string(b), nil
}

// defaultTitle: the first non-empty line, at most maxNoteTitle bytes, else the file name.
func defaultTitle(body, file string) string {
	for _, l := range strings.Split(body, "\n") {
		if t := strings.TrimSpace(l); t != "" {
			for len(t) > maxNoteTitle {
				_, n := utf8.DecodeLastRuneInString(t)
				t = t[:len(t)-n]
			}
			return t
		}
	}
	if file != "-" {
		return filepath.Base(file)
	}
	return "note"
}

// noteScopeHint: the answer for a note that is not in the linked project's scope is the same as for one that does not exist; the hint is
// client-side text only, so the gateway still does not tell the two apart.
func noteScopeHint(err error, scoped bool) error {
	if err == nil || scoped || err.Error() != "not found or not visible" {
		return err
	}
	out := errors.New(err.Error() + "\n" + noteScopeText)
	var g gwStatusError
	if errors.As(err, &g) {
		return gwStatusError{out, g.status} // the profile hint still applies
	}
	return out
}

const noteScopeText = "a note of a team needs --team <team>; a note of another project needs a folder linked to that project (a search of a team or another project shows the ids)"

func (e *projectEnv) note(args []string) error {
	if len(args) == 0 {
		return errors.New("usage:\n" + noteHelp)
	}
	if e.agent && !(e.runConn != nil && (args[0] == "list" || args[0] == "read")) {
		return errors.New(agentRefusal)
	}
	fs := flag.NewFlagSet("note", flag.ContinueOnError)
	class := fs.String("class", "", "class")
	title := fs.String("title", "", "title")
	limit := fs.Int("limit", 0, "page size")
	after := fs.String("after", "", "cursor")
	asJSON := fs.Bool("json", false, "JSON")
	proj := fs.String("project", "", "the linked project's slug")
	team := fs.String("team", "", "a team's own notes, in place of the linked project")
	rev := fs.Int64("revision", 0, "revision")
	current := fs.Int64("current", 0, "confirmed current revision")
	key := fs.String("key", "", "idempotency key")
	pos, err := parseFlags(fs, args[1:])
	if err != nil {
		return errors.New("usage:\n" + noteHelp)
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	usage := func() error { return errors.New("usage:\n" + noteHelp) }
	sub := args[0]
	if sub != "list" && sub != "read" && sub != "push" && sub != "edit" && sub != "rm" && sub != "history" && sub != "restore" {
		return errors.New("unknown command minigw note " + sub + "\n" + noteHelp)
	}
	var id int64
	if sub == "read" || sub == "edit" || sub == "rm" || sub == "history" || sub == "restore" {
		if len(pos) < 1 {
			return usage()
		}
		if id, err = strconv.ParseInt(pos[0], 10, 64); err != nil || id < 1 {
			return errors.New("not a note id: " + pos[0])
		}
		pos = pos[1:]
	}
	var c connection
	var link projectLink
	base := "/minigw/v1/notes" // inside a run: the run's own project, named nowhere
	if *team != "" && (e.agent || e.runConn != nil) {
		return errors.New(teamAgentRefusal)
	}
	if *team != "" && *proj != "" {
		return errors.New(teamProjectUsage)
	}
	if e.runConn != nil {
		if *proj != "" {
			return errors.New("inside an agent run the project is the run's own: --project is not accepted")
		}
		c = *e.runConn
	} else if *team != "" {
		c, _ = e.connect()
		id, err := e.teamSegment(c, *team)
		if err != nil {
			return err
		}
		base = fmt.Sprintf("%s/%d/notes", apiProjects, id)
	} else {
		if c, link, err = e.linkedProject(*proj); err != nil {
			return err
		}
		base = fmt.Sprintf("%s/%d/notes", apiProjects, link.Project)
	}
	one := func(method string, hdr map[string]string, body []byte) ([]byte, error) {
		st, data, h := e.do(c, method, fmt.Sprintf("%s/%d", base, id), hdr, body)
		if st < 200 || st > 299 {
			return nil, noteScopeHint(e.gwError(st, data, h), e.runConn != nil || *team != "")
		}
		return data, nil
	}
	print := func(data []byte, f func(noteView)) {
		if *asJSON {
			e.say(jsonText(strings.TrimSpace(string(data))))
			return
		}
		var n noteView
		json.Unmarshal(data, &n)
		f(n)
	}
	privacy := func(cl string) {
		if cl == "private" {
			if *asJSON {
				fmt.Fprintln(e.err, privateNote)
			} else {
				e.say(privateNote)
			}
		}
	}
	switch sub {
	case "history", "restore":
		if len(pos) != 0 || sub == "restore" && *rev < 1 {
			return usage()
		}
		return noteHistoryCommand(sub, fmt.Sprintf("%s/%d", base, id), id, *rev, *current, set["current"], *class, *asJSON, e.tty == nil && stdinIsTTY() || e.tty != nil && e.tty(), e.in, e.out, func(method, path string, body []byte) ([]byte, error) {
			st, data, h := e.do(c, method, path, nil, body)
			if st != 200 {
				return nil, e.gwError(st, data, h)
			}
			return data, nil
		})
	case "list":
		if len(pos) != 0 {
			return usage()
		}
		q := url.Values{}
		if *class != "" {
			q.Set("class", *class)
		}
		if set["limit"] {
			q.Set("limit", strconv.Itoa(*limit))
		}
		if *after != "" {
			q.Set("after", *after)
		}
		path := base
		if len(q) > 0 {
			path += "?" + q.Encode()
		}
		st, data, h := e.do(c, "GET", path, nil, nil)
		if st != 200 {
			return e.gwError(st, data, h)
		}
		if *asJSON {
			e.say(jsonText(strings.TrimSpace(string(data))))
			return nil
		}
		var pg struct {
			Notes []noteView
			Next  string
		}
		json.Unmarshal(data, &pg)
		for _, n := range pg.Notes {
			e.say(clean(fmt.Sprintf("%-6d %-9s r%-3d %s  %-12s %s", n.ID, n.Class, n.Revision, noteTime(n.Updated), n.Author, oneLine(n.Title)+editedBy(n, "  (edited: %s)"))))
		}
		if len(pg.Notes) == 0 {
			e.say("no notes")
		}
		if pg.Next != "" {
			e.say("more: minigw note list --after " + pg.Next)
		}
	case "read":
		data, err := one("GET", nil, nil)
		if err != nil {
			return err
		}
		print(data, func(n noteView) {
			body := ""
			if n.Body != nil {
				body = *n.Body
			}
			quote := ""
			if n.Upstream != "" {
				quote = "> "
			}
			e.say(clean(fmt.Sprintf("note %d  %s  revision %d  by %s%s  %s\n%s%s\n", n.ID, n.Class, n.Revision, n.Author, editedBy(n, ", last changed by %s"), noteTime(n.Updated), quote, oneLine(n.Title))))
			fmt.Fprint(e.out, clean(strings.ReplaceAll(body, "\t", "    ")))
			if !strings.HasSuffix(body, "\n") {
				e.say("")
			}
		})
	case "push":
		if len(pos) != 1 {
			return usage()
		}
		body, err := readInput(e.in, pos[0])
		if err != nil {
			return err
		}
		cl := *class
		if cl == "" {
			cl = "data"
		}
		t := *title
		if t == "" {
			t = defaultTitle(body, pos[0])
		}
		if len(t) > maxNoteTitle {
			return fmt.Errorf("too large: a title is at most %d bytes", maxNoteTitle)
		}
		k := *key
		if k == "" {
			b := make([]byte, 16)
			rand.Read(b)
			k = "mgw-" + hex.EncodeToString(b)
		}
		payload, _ := json.Marshal(map[string]string{"class": cl, "title": t, "body": body})
		var st int
		var data []byte
		var h http.Header
		for try := 0; try <= pushRetries; try++ { // a lost answer: the same key makes the retry safe (the gateway replays)
			if st, data, h = e.do(c, "POST", base, map[string]string{"Idempotency-Key": k}, payload); st != 0 {
				break
			}
			e.sleep(time.Second)
		}
		if st == 0 {
			return fmt.Errorf("no answer from the gateway: the note may have been created; repeat with --key %s (safe: no duplicate), or check with minigw note list", k)
		}
		if st != 201 {
			return gatewayError(st, data, h)
		}
		print(data, func(n noteView) { e.say(fmt.Sprintf("note %d created (%s, revision %d)", n.ID, n.Class, n.Revision)) })
		privacy(cl)
	case "edit":
		if len(pos) > 1 {
			return usage()
		}
		var cur noteView
		if *rev == 0 || len(pos) == 0 || *class == "" || !set["title"] { // the current note supplies what the flags do not
			data, err := one("GET", nil, nil)
			if err != nil {
				return err
			}
			json.Unmarshal(data, &cur)
		}
		n := map[string]any{"revision": cur.Revision, "class": cur.Class, "title": cur.Title, "body": ""}
		if cur.Body != nil {
			n["body"] = *cur.Body
		}
		if set["revision"] {
			n["revision"] = *rev
		}
		if *class != "" {
			n["class"] = *class
		}
		if set["title"] {
			n["title"] = *title
		}
		if len(pos) == 1 {
			body, err := readInput(e.in, pos[0])
			if err != nil {
				return err
			}
			n["body"] = body
		}
		payload, _ := json.Marshal(n)
		data, err := one("PUT", nil, payload)
		if err != nil {
			return err
		}
		print(data, func(v noteView) { e.say(fmt.Sprintf("note %d updated (%s, revision %d)", v.ID, v.Class, v.Revision)) })
		if cl, _ := n["class"].(string); cl == "private" {
			privacy(cl)
		}
	case "rm":
		if len(pos) != 0 {
			return usage()
		}
		r := *rev
		if !set["revision"] {
			data, err := one("GET", nil, nil)
			if err != nil {
				return err
			}
			var cur noteView
			json.Unmarshal(data, &cur)
			r = cur.Revision
		}
		st, data, h := e.do(c, "DELETE", fmt.Sprintf("%s/%d?revision=%d", base, id, r), nil, nil)
		if st != 204 {
			return noteScopeHint(gatewayError(st, data, h), e.runConn != nil || *team != "")
		}
		if *asJSON {
			e.say(fmt.Sprintf(`{"deleted":%d}`, id))
		} else {
			e.say(fmt.Sprintf("note %d deleted", id))
		}
	}
	return nil
}

// usage: GET /minigw/v1/usage. The project is --project, else the linked project of this folder, else none (then
// your own events over all your projects).
func (e *projectEnv) usage(args []string) error {
	if e.agent {
		return errors.New(agentRefusal)
	}
	fs := flag.NewFlagSet("usage", flag.ContinueOnError)
	proj := fs.String("project", "", "project slug")
	days := fs.Int("days", 7, "days (1-400)")
	asJSON := fs.Bool("json", false, "JSON")
	pos, err := parseFlags(fs, args)
	if err != nil || len(pos) != 0 {
		return errors.New("usage:\n" + usageHelp)
	}
	c, gw := e.connect()
	slug := *proj
	if slug == "" {
		if st, err := e.state3(); err == nil {
			if res, err := projectFor(st, gw, e.cwd); err == nil && res.State == linkOK {
				slug = res.Link.Slug
			}
		}
	}
	q := url.Values{"days": {strconv.Itoa(*days)}}
	if slug != "" {
		q.Set("project", slug)
	}
	st, data, h := e.do(c, "GET", "/minigw/v1/usage?"+q.Encode(), nil, nil)
	if st != 200 {
		if st == 404 && slug != "" {
			return errors.New("no usage to show for project " + slug + ": not found, not visible to you, or you are not a developer or admin there")
		}
		return gatewayError(st, data, h)
	}
	if *asJSON {
		e.say(strings.TrimSpace(string(data)))
		return nil
	}
	var r usageReply
	json.Unmarshal(data, &r)
	if slug != "" {
		e.say("project " + slug)
	} else {
		e.say("your own usage, all projects")
	}
	printUsage(e.out, r, false)
	return nil
}

// gwError: gatewayError, with one clear line for a run that cannot reach its run port (M1: the Windows sandbox gets
// no loopback unless the agent may ask other models).
func (e *projectEnv) gwError(st int, data []byte, h http.Header) error {
	if st == 0 && e.runConn != nil {
		return errors.New("the notes are not reachable from this sandbox (it has no access to the gateway's run port)")
	}
	return gatewayError(st, data, h)
}

// running: GET /minigw/v1/running. The requests of this device that run on the gateway now: model, folder, elapsed time,
// number of steps and the last step. The gateway answers only with this device's own requests (never a prompt).
func (e *projectEnv) running(args []string) error {
	if e.agent {
		return errors.New(agentRefusal)
	}
	fs := flag.NewFlagSet("running", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "JSON")
	pos, err := parseFlags(fs, args)
	if err != nil || len(pos) != 0 {
		return errors.New("usage: minigw running [--json]")
	}
	c, _ := e.connect()
	st, data, h := e.do(c, "GET", "/minigw/v1/running", nil, nil)
	if st == 404 {
		return errors.New("this gateway is too old for minigw running: update it")
	}
	if st != 200 {
		return gatewayError(st, data, h)
	}
	if *asJSON {
		e.say(strings.TrimSpace(string(data)))
		return nil
	}
	var r struct {
		Running []struct {
			Model, Folder, LastStep string
			ElapsedSec, StepAgoSec  int64
			Steps                   int
		}
	}
	json.Unmarshal(data, &r)
	if len(r.Running) == 0 {
		e.say("nothing of yours is running on the gateway")
		return nil
	}
	for _, x := range r.Running {
		line := fmt.Sprintf("%s  %s  running %s  %s", oneLine(x.Model), oneLine(x.Folder), (time.Duration(x.ElapsedSec) * time.Second).String(), plural(x.Steps, "step", "steps"))
		if x.LastStep != "" {
			line += fmt.Sprintf("  last: %s (%s ago)", shortLine(x.LastStep, 160), (time.Duration(x.StepAgoSec) * time.Second).String())
		}
		e.say(line)
	}
	return nil
}

// userinfoRe: the credentials of a URL remote; they never leave this computer.
var userinfoRe = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9+.-]*://)[^/]*@`)

func stripUserinfo(raw string) string {
	return userinfoRe.ReplaceAllString(strings.TrimSpace(raw), "$1")
}

// gitOrigin is the folder's raw remote.origin.url from the repository's own config (no insteadOf rewrite, no hooks).
// Git missing, no origin or an untrusted checkout all give "".
func gitOrigin(dir string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := commandContext(ctx, "git", append(slices.Clone(gitNoExecPins), "config", "--local", "--get", "remote.origin.url")...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), gitNoExecEnv...)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// folderRemote is the folder's origin without credentials, or the error "no git remote origin ...".
func (e *projectEnv) folderRemote(dir string) (string, error) {
	origin := e.origin
	if origin == nil {
		origin = gitOrigin
	}
	raw := stripUserinfo(origin(dir))
	if raw == "" {
		return "", errors.New("no git remote origin in this folder")
	}
	return raw, nil
}

// findByRemote asks the gateway which of the caller's projects is registered for the folder's origin.
func (e *projectEnv) findByRemote(c connection, dir string) (string, error) {
	const hint = "; run minigw project link <slug>"
	raw, err := e.folderRemote(dir)
	if err != nil {
		return "", errors.New(err.Error() + hint)
	}
	body, _ := json.Marshal(map[string]string{"remote": raw})
	st, data, h := e.do(c, "POST", apiProjects+"/by-remote", nil, body)
	if st == 400 {
		return "", errors.New("the git remote origin of this folder is not a remote URL minigw can use" + hint)
	}
	if st != 200 {
		return "", gatewayError(st, data, h)
	}
	var r struct {
		Identity string
		Projects []struct{ Slug string }
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return "", err
	}
	switch len(r.Projects) {
	case 0:
		return "", fmt.Errorf("no project of yours is registered for this git remote (%s)%s", r.Identity, hint)
	case 1:
		e.say(fmt.Sprintf("found project %s by git remote %s", r.Projects[0].Slug, r.Identity))
		return r.Projects[0].Slug, nil
	}
	slugs := make([]string, len(r.Projects))
	for i, p := range r.Projects {
		slugs[i] = p.Slug
	}
	return "", fmt.Errorf("this git remote matches several of your projects (%s)%s", strings.Join(slugs, ", "), hint)
}

// remote: minigw project remote list [<slug>] | add <url>|--here [<slug>] | remove <url> [<slug>].
func (e *projectEnv) remote(pos []string, here bool) error {
	usage := errors.New("usage:\n" + projectHelp)
	if len(pos) == 0 || (pos[0] != "list" && pos[0] != "add" && pos[0] != "remove") || here && pos[0] != "add" {
		return usage
	}
	op, rest := pos[0], pos[1:]
	var raw string
	switch {
	case op == "add" && here:
		if len(rest) > 1 {
			return usage
		}
	case op == "list":
		if len(rest) > 1 {
			return usage
		}
	default:
		if len(rest) < 1 || len(rest) > 2 {
			return usage
		}
		raw, rest = stripUserinfo(rest[0]), rest[1:]
	}
	var c connection
	var project int64
	slug := ""
	if len(rest) == 1 {
		c, _ = e.connect()
		slug = rest[0]
		var err error
		if project, err = e.projectIDOf(c, slug); err != nil {
			return err
		}
	} else {
		cc, link, err := e.linkedProject("")
		if err != nil {
			return err
		}
		c, project, slug = cc, link.Project, link.Slug
	}
	path := fmt.Sprintf("%s/%d/remotes", apiProjects, project)
	if op == "list" {
		st, data, h := e.do(c, "GET", path, nil, nil)
		if st != 200 {
			return gatewayError(st, data, h)
		}
		var r struct{ Remotes []string }
		if err := json.Unmarshal(data, &r); err != nil {
			return err
		}
		for _, id := range r.Remotes {
			e.say(clean(id))
		}
		if len(r.Remotes) == 0 {
			e.say("no remotes registered")
		}
		return nil
	}
	if here {
		real, err := realFolder(e.cwd)
		if err != nil {
			return err
		}
		if raw, err = e.folderRemote(real); err != nil {
			return err
		}
	}
	field := map[string]string{"add": "add", "remove": "remove"}[op]
	body, _ := json.Marshal(map[string][]string{field: {raw}})
	st, data, h := e.do(c, "POST", path, nil, body)
	if st != 200 {
		return gatewayError(st, data, h)
	}
	var r struct{ Added, Removed []string }
	if err := json.Unmarshal(data, &r); err != nil {
		return err
	}
	ids, verb, prep := r.Added, "registered", "for"
	if op == "remove" {
		ids, verb, prep = r.Removed, "removed", "from"
	}
	if len(ids) != 1 {
		return errors.New("the gateway did not name the remote")
	}
	e.say(clean(fmt.Sprintf("%s %s %s project %s", verb, ids[0], prep, slug)))
	return nil
}

// depends: minigw project depends [<slug>]: the dependencies of a project that the caller can read.
func (e *projectEnv) depends(pos []string) error {
	if len(pos) > 1 {
		return errors.New("usage: minigw project depends [<slug>]")
	}
	var c connection
	var project int64
	if len(pos) == 1 {
		c, _ = e.connect()
		var err error
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
	st, data, h := e.do(c, "GET", fmt.Sprintf("%s/%d/depends", apiProjects, project), nil, nil)
	if st != 200 {
		return gatewayError(st, data, h)
	}
	var r struct {
		Depends []struct{ Slug string }
		Teams   []struct{ Name string } // only the teams the caller is on: the gateway hides the others
	}
	if err := json.Unmarshal(data, &r); err != nil {
		return err
	}
	for _, d := range r.Depends {
		e.say(clean("depends on " + d.Slug))
	}
	for _, t := range r.Teams {
		e.say(clean("depends on team " + t.Name))
	}
	if len(r.Depends)+len(r.Teams) == 0 {
		e.say("no dependencies")
	}
	return nil
}
