package main

// minigw gw user|bind|project|member|grant|core …: the core's admin API from the terminal (serve_core_admin.go).

import (
	"encoding/json"
	"fmt"
	"io"
	"minigw/coreapi"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"minigw/core"
)

const coreHelp = `
  minigw gw export --team <name> [--project slug ...] [--with-private] <file.zip>
  minigw gw import --source <name> <file.zip> [--dry-run] [--map old=new]
  minigw gw kb review [<project>|--team <team>] | accept <id> | reject <id> [--reason text]
  minigw gw user list | add <name> | rename <user> <name> | activate <user> | deactivate <user>
  minigw gw bind list | <device id> <user> | remove <device id>        which user a device belongs to
  minigw gw project list | add <slug> <name> | archive <project> | unarchive <project>
  minigw gw project remote list <project> | add <project> <url> | remove <project> <url>   git remotes registered for a project
  minigw gw project depends list <project> | add <project> <other> | remove <project> <other>   <project> depends on <other>: its members' search also covers <other> as far as they may read it
  minigw gw project depends add|remove <project> --team <team>   the same for a team's notes: only for members of that team (list shows teams too)
  minigw gw team add <name> | list | rename <team> <name> | archive <team> | unarchive <team>
  minigw gw team path <team> <label>                    free-text path label of a team ("-" clears it)
  minigw gw team member add|remove <team> <user>
  minigw gw team project add|remove <team> <project>
  minigw gw team access set <team> <project> <user> <role> | remove <team> <project> <user> | list <team>
  minigw gw member <project> list [--explain] | set <user> <role> | remove <user>   role: reader, developer, admin
  minigw gw grant <source> list | add <reader> <class> | remove <reader> <class>   class: structure, data
  minigw gw core backup [name]                          a consistent copy of the core database into <config>/core/backups (gateway running)
  minigw gw core restore <file|name> [--yes]            only with the gateway stopped; a name is looked up in <config>/core/backups
  minigw gw core audit                                  the audit trail of the core
  minigw gw project-sessions --project <p> [--days n] [--model m] [--device d] [--phase p] [--activity a] [--task t] [--json]
  minigw gw usage-events [--days n] [--user u] [--project p] [--model m] [--phase p] [--activity a] [--task t]   model invocations per day, model and user (core; all of them)
  minigw gw quota [--claude-probe=on|off]               subscription quota and provider credit, with source, time and staleness
  minigw gw quota --history [--days N] [--provider P] [--json]   recorded probes (default 7 days; no probe)
(<user> and <project>: the name or slug, or the number.)`

type coreUser struct {
	ID     int64
	Name   string
	Active bool
	Owner  bool
}

type coreProject struct {
	ID       int64
	Slug     string
	Name     string
	Archived bool
}

// userID resolves a user given by name or number.
func userID(a *adminClient, ref string) int64 {
	var r struct{ Users []coreUser }
	a.do("GET", "/users", nil, &r)
	n, _ := strconv.ParseInt(ref, 10, 64)
	for _, u := range r.Users {
		if u.ID == n || strings.EqualFold(u.Name, ref) {
			return u.ID
		}
	}
	fail("no user " + strconv.Quote(ref) + " (minigw gw user list)")
	return 0
}

// projectID resolves a project given by slug or number.
func projectID(a *adminClient, ref string) int64 {
	var r struct{ Projects []coreProject }
	a.do("GET", "/projects", nil, &r)
	n, _ := strconv.ParseInt(ref, 10, 64)
	for _, p := range r.Projects {
		if p.ID == n || p.Slug == ref {
			return p.ID
		}
	}
	fail("no project " + strconv.Quote(ref) + " (minigw gw project list)")
	return 0
}

// coreOpts: the flags of usage-events and usage.
type coreOpts struct {
	days, user, project, model, claudeProbe, provider, device, folder, sort, reason, team, by, phase, activity, task string
	json, csv, history, explain                                                                                      bool
	// upstream (upstream_cmd.go): the token comes from in; the rest are the link and push options
	in                        io.Reader
	remoteProject, remoteTeam string
	class                     string
	withPrivate, dryRun       bool
	pause                     func(time.Duration) // between the calls of a long upstream push (nil: time.Sleep)
}

func coreCommand(a *adminClient, pos []string, o coreOpts) {
	need := func(min, max int) {
		if len(pos) < min || len(pos) > max {
			fail(strings.TrimSpace(coreHelp))
		}
	}
	id := strconv.FormatInt
	switch pos[0] {
	case "kb":
		kbAdmin(a, pos, o.reason, o.team)
	case "team":
		teamAdmin(a, pos)
	case "upstream":
		upstreamCommand(a, pos, o)
	case "user":
		need(2, 4)
		switch {
		case pos[1] == "list" && len(pos) == 2:
			var r struct{ Users []coreUser }
			a.do("GET", "/users", nil, &r)
			for _, u := range r.Users {
				state := ""
				if !u.Active {
					state = "  (deactivated)"
				}
				if u.Owner {
					state += "  (owner)"
				}
				say(clean(fmt.Sprintf("%d  %s%s", u.ID, u.Name, state)))
			}
		case pos[1] == "add" && len(pos) == 3:
			a.do("POST", "/users", map[string]any{"name": pos[2]}, nil)
			say("user " + clean(pos[2]) + " added")
		case pos[1] == "rename" && len(pos) == 4:
			a.do("PATCH", "/users/"+id(userID(a, pos[2]), 10), map[string]any{"name": pos[3]}, nil)
		case (pos[1] == "activate" || pos[1] == "deactivate") && len(pos) == 3:
			a.do("PATCH", "/users/"+id(userID(a, pos[2]), 10), map[string]any{"active": pos[1] == "activate"}, nil)
			if pos[1] == "deactivate" {
				say("deactivated: the user's runs and capabilities end (the epoch grew)")
			}
		default:
			fail(strings.TrimSpace(coreHelp))
		}
	case "bind":
		switch {
		case len(pos) == 2 && pos[1] == "list":
			var r struct {
				Bindings []struct {
					Device string
					User   int64
				}
			}
			a.do("GET", "/bindings", nil, &r)
			for _, b := range r.Bindings {
				say(fmt.Sprintf("%s  user %d", b.Device, b.User))
			}
		case len(pos) == 3 && pos[1] == "remove":
			a.do("DELETE", "/bindings/"+url.PathEscape(pos[2]), nil, nil)
		case len(pos) == 3 && validDeviceID(pos[1]):
			a.do("PUT", "/bindings/"+url.PathEscape(pos[1]), map[string]any{"user": userID(a, pos[2])}, nil)
			var d struct{ Devices []apiDevice }
			a.do("GET", "/devices", nil, &d)
			// A device may be bound before the owner allows it; without the list entry projects still refuse.
			if !slices.ContainsFunc(d.Devices, func(x apiDevice) bool { return x.ID == pos[1] }) {
				say("warning: this device is not on the device list yet: minigw gw devices allow " + pos[1])
			}
		default:
			fail(strings.TrimSpace(coreHelp))
		}
	case "project":
		need(2, 5)
		switch {
		case pos[1] == "list" && len(pos) == 2:
			var r struct{ Projects []coreapi.ProjectJSON }
			a.do("GET", "/projects", nil, &r)
			for _, p := range r.Projects {
				arch := ""
				if p.Archived {
					arch = "  (archived)"
				}
				say(clean(fmt.Sprintf("%d  %s  %s%s", p.ID, p.Slug, p.Name, arch)))
			}
		case pos[1] == "add" && len(pos) == 4:
			a.do("POST", "/projects", map[string]any{"slug": pos[2], "name": pos[3]}, nil)
			say("project " + clean(pos[2]) + " added")
		case (pos[1] == "archive" || pos[1] == "unarchive") && len(pos) == 3:
			a.do("PATCH", "/projects/"+id(projectID(a, pos[2]), 10), map[string]any{"archived": pos[1] == "archive"}, nil)
		case pos[1] == "remote" && (pos[2] == "list" && len(pos) == 4 || (pos[2] == "add" || pos[2] == "remove") && len(pos) == 5):
			path := "/projects/" + id(projectID(a, pos[3]), 10) + "/remotes"
			if pos[2] == "list" {
				var r struct{ Remotes []string }
				a.do("GET", path, nil, &r)
				for _, v := range r.Remotes {
					say(clean(v))
				}
				if len(r.Remotes) == 0 {
					say("no remotes registered")
				}
				break
			}
			var r struct{ Added, Removed []string }
			a.do("POST", path, map[string][]string{pos[2]: {stripUserinfo(pos[4])}}, &r)
			ids, verb, prep := r.Added, "registered", "for"
			if pos[2] == "remove" {
				ids, verb, prep = r.Removed, "removed", "from"
			}
			if len(ids) != 1 {
				fail("the gateway did not name the remote")
			}
			say(clean(fmt.Sprintf("%s %s %s project %s", verb, ids[0], prep, pos[3])))
		case pos[1] == "depends" && (pos[2] == "list" && len(pos) == 4 || (pos[2] == "add" || pos[2] == "remove") && len(pos) == 5 && o.team == "" || (pos[2] == "add" || pos[2] == "remove") && len(pos) == 4 && o.team != ""):
			base := "/projects/" + id(projectID(a, pos[3]), 10) + "/depends"
			if pos[2] == "list" {
				var r struct {
					Depends, UsedBy []coreProject
					Teams           []cliTeam
				}
				a.do("GET", base, nil, &r)
				for _, p := range r.Depends {
					say(clean("depends on " + p.Slug))
				}
				for _, t := range r.Teams {
					note := ""
					if t.Archived {
						note = " (archived: gives nothing)"
					}
					say(clean("depends on team " + t.Name + note))
				}
				for _, p := range r.UsedBy {
					say(clean("used by " + p.Slug))
				}
				if len(r.Depends)+len(r.UsedBy)+len(r.Teams) == 0 {
					say("no dependencies")
				}
				break
			}
			if o.team != "" {
				path := base + "/teams/" + id(teamID(a, o.team), 10)
				if pos[2] == "add" {
					a.do("PUT", path, nil, nil)
					say(clean(pos[3] + " depends on team " + o.team))
				} else {
					a.do("DELETE", path, nil, nil)
					say(clean(pos[3] + " no longer depends on team " + o.team))
				}
				break
			}
			path := base + "/" + id(projectID(a, pos[4]), 10)
			if pos[2] == "add" {
				a.do("PUT", path, nil, nil)
				say(clean(pos[3] + " depends on " + pos[4]))
			} else {
				a.do("DELETE", path, nil, nil)
				say(clean(pos[3] + " no longer depends on " + pos[4]))
			}
		default:
			fail(strings.TrimSpace(coreHelp))
		}
	case "member":
		need(3, 5)
		p := id(projectID(a, pos[1]), 10)
		switch {
		case pos[2] == "list" && len(pos) == 3:
			var r struct {
				Members []struct {
					User       int64
					Name, Role string
					Origins    []cliOrigin
				}
			}
			a.do("GET", "/projects/"+p+"/members", nil, &r)
			for _, m := range r.Members {
				say(clean(fmt.Sprintf("%d  %s  %s", m.User, m.Name, m.Role)))
				if o.explain {
					say(clean("  origins: " + originText(m.Origins, true)))
				}
			}
		case pos[2] == "set" && len(pos) == 5:
			var r memberReply
			a.do("PUT", "/projects/"+p+"/members/"+id(userID(a, pos[3]), 10), map[string]any{"role": pos[4]}, &r)
			memberFeedback(r, pos[4])
		case pos[2] == "remove" && len(pos) == 4:
			var r memberReply
			a.do("DELETE", "/projects/"+p+"/members/"+id(userID(a, pos[3]), 10), nil, &r)
			memberFeedback(r, "")
		default:
			fail(strings.TrimSpace(coreHelp))
		}
	case "grant":
		need(3, 5)
		srcID := projectID(a, pos[1])
		src := id(srcID, 10)
		switch {
		case pos[2] == "list" && len(pos) == 3:
			var r struct {
				Grants []struct {
					Source, Reader int64
					Class          string
				}
			}
			a.do("GET", "/grants", nil, &r)
			for _, g := range r.Grants {
				if id(g.Source, 10) == src {
					say(fmt.Sprintf("project %d reads %s of %s", g.Reader, g.Class, pos[1]))
				}
			}
		case pos[2] == "add" && len(pos) == 5:
			a.do("POST", "/grants", map[string]any{"source": srcID, "reader": projectID(a, pos[3]), "class": pos[4]}, nil)
		case pos[2] == "remove" && len(pos) == 5:
			a.do("DELETE", "/grants?source="+src+"&reader="+id(projectID(a, pos[3]), 10)+"&class="+url.QueryEscape(pos[4]), nil, nil)
		default:
			fail(strings.TrimSpace(coreHelp))
		}
	case "usage": // the report of the new events (the folder and device counters: usage --legacy)
		if len(pos) != 1 || o.json && o.csv {
			fail(usageReportHelp)
		}
		if o.device != "" || o.folder != "" {
			fail("--device and --folder belong to the old counters: use minigw gw usage --legacy")
		}
		if o.user != "" || o.project != "" {
			fail("--user and --project belong to minigw gw usage-events")
		}
		if o.sort != "" && o.sort != "failed" || o.sort != "" && (o.json || o.csv) {
			fail(usageReportHelp)
		}
		var by []string
		if o.by != "" {
			var ok bool
			if by, ok = parseUsageBy(o.by); !ok || o.csv || o.sort != "" {
				fail(usageReportHelp)
			}
		}
		q := url.Values{"days": {o.days}}
		if o.provider != "" {
			q.Set("provider", o.provider)
		}
		if o.model != "" {
			q.Set("model", o.model)
		}
		addLabelFilters(q, o)
		if byLabels(by) {
			q.Set("labels", "1")
		}
		var raw json.RawMessage
		a.do("GET", "/usage/events?"+q.Encode(), nil, &raw)
		if o.json && by == nil {
			os.Stdout.Write(append(raw, '\n'))
			break
		}
		rows, gaps, err := parseUsageReply(raw)
		if err != nil {
			fail("the usage answer is not understood: " + err.Error())
		}
		if by != nil {
			projects := map[int64]string{}
			if slices.Contains(by, "project") {
				var r struct{ Projects []coreProject }
				a.do("GET", "/projects", nil, &r)
				for _, p := range r.Projects {
					projects[p.ID] = p.Slug
				}
			}
			if o.json { // the same groups and numbers as the table
				fmt.Println(usageByJSON(rows, gaps, by, projects, usageTruncated(raw)))
				break
			}
			fmt.Print(usageByText(rows, gaps, by, projects, usageTruncated(raw)))
		} else if o.csv {
			fmt.Print(usageReportCSV(rows))
		} else {
			fmt.Print(usageReportText(rows, gaps, o.sort == "failed"))
		}
	case "project-sessions":
		need(1, 1)
		if o.project == "" {
			fail("--project required")
		}
		q := url.Values{"days": {o.days}, "project": {id(projectID(a, o.project), 10)}, "model": {o.model}, "device": {o.device}}
		addLabelFilters(q, o)
		page := core.SessionPage{Sessions: []core.SessionRow{}}
		seen := map[string]bool{}
		for {
			var part core.SessionPage
			a.do("GET", "/usage/sessions?"+q.Encode(), nil, &part)
			page.Sessions = append(page.Sessions, part.Sessions...)
			if part.Next == "" {
				break
			}
			if seen[part.Next] {
				fail("repeated session page")
			}
			seen[part.Next] = true
			q.Set("after", part.Next)
		}
		if o.json {
			b, _ := json.Marshal(page)
			fmt.Println(string(b))
		} else {
			fmt.Print(sessionText(page))
		}
	case "usage-events":
		need(1, 1)
		q := url.Values{"days": {o.days}}
		if o.user != "" {
			q.Set("user", id(userID(a, o.user), 10))
		}
		if o.project != "" {
			q.Set("project", id(projectID(a, o.project), 10))
		}
		if o.model != "" {
			q.Set("model", o.model)
		}
		addLabelFilters(q, o)
		var r usageReply
		a.do("GET", "/usage/events?"+q.Encode(), nil, &r)
		printUsage(os.Stdout, r, true)
	case "quota":
		need(1, 1)
		if o.history {
			if o.claudeProbe != "" {
				fail("--history cannot be combined with --claude-probe")
			}
			q := url.Values{"days": {o.days}}
			if o.provider != "" {
				q.Set("provider", o.provider)
			}
			var raw json.RawMessage
			status := a.do("GET", "/quota/history?"+q.Encode(), nil, &raw)
			if status == 206 {
				fmt.Fprintln(os.Stderr, "quota history capped: newest 1000 probes; gaps capped at 1000")
			}
			if o.json {
				os.Stdout.Write(append(raw, '\n'))
			} else {
				fmt.Fprint(os.Stdout, quotaHistoryText(raw))
			}
			break
		}
		if o.claudeProbe != "" {
			if o.claudeProbe != "on" && o.claudeProbe != "off" {
				fail("--claude-probe: on or off")
			}
			a.do("PATCH", "/quota", map[string]any{"claudeProbe": o.claudeProbe == "on"}, nil)
		}
		var r struct {
			Quota       []quotaValue
			ClaudeProbe bool
		}
		a.do("GET", "/quota", nil, &r)
		say("claude probe: " + map[bool]string{true: "on (runs claude -p /usage, at most every 10 minutes)", false: "off (minigw gw quota --claude-probe=on turns it on)"}[r.ClaudeProbe])
		for _, v := range r.Quota {
			val := "-"
			if v.Value != nil {
				val = strconv.FormatFloat(*v.Value, 'f', -1, 64) + " " + v.Unit
			}
			line := fmt.Sprintf("%-28s %-14s %s", v.Name, val, v.Source)
			if v.ObservedAt != nil {
				line += "  observed " + v.ObservedAt.Local().Format("2006-01-02 15:04")
			}
			if v.ResetsAt != nil {
				line += "  resets " + v.ResetsAt.Local().Format("2006-01-02 15:04")
			} else if v.ResetsText != "" {
				line += "  resets " + v.ResetsText
			}
			if v.Stale {
				line += "  (stale)"
			}
			if v.Note != "" {
				line += "  " + v.Note
			}
			if v.Reason != "" {
				line += "  unavailable: " + v.Reason
			}
			say(clean(line))
		}
	case "core":
		need(2, 3)
		switch {
		case pos[1] == "backup":
			if a.off != nil {
				fail("the gateway is not running: a backup needs it (start it, or copy the core folder while it is stopped)")
			}
			name := "core-" + time.Now().UTC().Format("20060102T150405Z") + ".db"
			if len(pos) == 3 {
				if name = pos[2]; strings.ContainsAny(name, `/\:`) {
					fail("a backup is kept in the core folder's backups folder (a copy anywhere else would be readable by agents): give a name, not a path")
				}
			}
			var r struct{ File string }
			a.do("POST", "/core/backup", map[string]any{"name": name}, &r)
			say("backup written to " + r.File)
		case pos[1] == "audit" && len(pos) == 2:
			var after int64
			for {
				var r struct {
					Events []struct {
						ID                           int64
						Time, Actor, Action, Subject string
						Detail                       any
					}
					Next int64
				}
				a.do("GET", "/audit?after="+id(after, 10)+"&limit=500", nil, &r)
				for _, e := range r.Events {
					say(clean(fmt.Sprintf("%d  %s  %s  %s  %s  %v", e.ID, e.Time, e.Actor, e.Action, e.Subject, e.Detail)))
				}
				if len(r.Events) == 0 {
					break
				}
				after = r.Next
			}
		default:
			fail(strings.TrimSpace(coreHelp))
		}
	}
}

// coreRestore runs core.Restore on the config folder's core, with no gateway running (the store's lock tells); the
// Mac's minigw-core restore runs the same code.
func coreRestore(cfgPath, file string, yes bool, in io.Reader) {
	cfgDir, _ := filepath.Abs(filepath.Dir(cfgPath))
	if code := core.RestoreCLI(filepath.Join(cfgDir, coreDirName), file, yes, in, os.Stdout, os.Stderr, "stop minigw serve first"); code != 0 {
		exitFn(code)
	}
}

// usageReply is the answer of the usage routes (member and admin).
type usageReply struct {
	Days []coreapi.UsageRowJSON
	Gaps []coreapi.UsageGapJSON
}

// printUsage: one line per day, model and user, the totals (each event once; nested events are inside them), and
// the gaps. withUser: the admin view names the user and the project.
func printUsage(w io.Writer, r usageReply, withUser bool) {
	var ev, in, out int64
	for _, d := range r.Days {
		who := ""
		if withUser {
			who = fmt.Sprintf("  user %s", orDash(d.UserName))
			if d.Project != 0 {
				who += fmt.Sprintf("  project %d", d.Project)
			}
		} else if d.UserName != "" {
			who = "  " + d.UserName
		}
		extra := ""
		if d.Nested > 0 {
			extra += fmt.Sprintf(", %d nested", d.Nested)
		}
		if d.UnknownTokens > 0 {
			extra += fmt.Sprintf(", %d without token counts", d.UnknownTokens)
		}
		if d.Failed > 0 {
			extra += fmt.Sprintf(", %d failed", d.Failed)
		}
		fmt.Fprintln(w, clean(fmt.Sprintf("%s  %-24s%s  %d events, %d in, %d out%s", d.Day, d.Model, who, d.Events, d.TokensIn, d.TokensOut, extra)))
		ev, in, out = ev+d.Events, in+d.TokensIn, out+d.TokensOut
	}
	if len(r.Days) == 0 {
		fmt.Fprintln(w, "no events")
	} else {
		fmt.Fprintf(w, "total: %d events, %d in, %d out (every event once, nested ones included)\n", ev, in, out)
	}
	for _, g := range r.Gaps {
		fmt.Fprintf(w, "%d events lost between %s and %s\n", g.Count, g.From.Local().Format("2006-01-02 15:04"), g.To.Local().Format("2006-01-02 15:04"))
	}
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// restoreOutcome: core.RestoreOutcome for minigw serve.
func restoreOutcome(err error) (msg string, failed bool) {
	return core.RestoreOutcome(err, "stop minigw serve first")
}

// addLabelFilters adds the label filters of the owner's usage commands (they select events; they never widen access).
func addLabelFilters(q url.Values, o coreOpts) {
	for k, v := range map[string]string{"phase": o.phase, "activity": o.activity, "task": o.task} {
		if v != "" {
			q.Set(k, v)
		}
	}
}
