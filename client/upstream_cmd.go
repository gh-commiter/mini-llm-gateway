package main

import (
	"fmt"
	"minigw/core"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const upstreamHelp = `
  minigw gw upstream add <name> <address> [--device-name <text>]   another gateway to send knowledge to (host[:port]); its Copy Token on stdin
  minigw gw upstream list | remove <name>
  minigw gw upstream link <project> <name> [--remote-project <slug>] [--class structure,data] [--with-private] [--auto|--manual] [--mode submit|publish]
  minigw gw upstream link --with-private   also the private notes of ALL members of the project (needs private in --class)
  minigw gw upstream link --team <team> <name> [--remote-team <name>] [--auto|--manual] [--mode ..]   a team's own notes
  minigw gw upstream link <project> <name> --read [--read-class structure[,data]] [--read-sources <label,...>] --accept-reach   read the upstream's notes into a local mirror (look first: pull --dry-run)
  minigw gw upstream link <project> <name> --no-read   stop reading and delete the mirror (--team <team> works the same)
  minigw gw upstream unlink <project> <name> | --team <team> <name>
  minigw gw upstream resolve <project> <note> --theirs|--ours [--upstream <name>]   a publish link: a note changed on the upstream (--theirs: keep theirs, forget our change; --ours: send ours as a submission)
  minigw gw upstream resolve --team <team> <note> --theirs|--ours [--upstream <name>]
  minigw gw upstream status [<project>|--team <team>]  per link: what would go, when it last went, the last refusal
  minigw gw upstream push [<project>|--team <team>] [--dry-run]   send the published notes as submissions (nothing without a link)
  minigw gw upstream pull [<project>|--team <team>] [--dry-run]   read the linked project's notes into the mirror (links with --read), or with --dry-run only ask what could be read (nothing is stored)`

type upLink struct {
	Project     int64
	Team        bool
	Upstream    string
	Remote      string
	Classes     []string
	WithPrivate bool
	LastPush    *time.Time
	LastError   string
	Label       string
	Pending     int
	Total       int
	Auto        bool
	Mode        string
	DirtySince  *time.Time
	LastOK      *time.Time
	LastFail    *time.Time
	FailCount   int
	NextAttempt *time.Time

	Read         bool
	ReadClasses  []string
	ReadSources  []string
	ReadAccepted *time.Time
	LastPull     *time.Time
	LastPullOK   *time.Time
	LastPullAge  int
	PullError    string
	Mirror       int
	MirrorBytes  int64

	NotPropagated []struct {
		Note int64
		Why  string
	}
	Conflicts []struct {
		Note      int64
		RemoteRev int64
		Gone      bool
	}
}

func upstreamCommand(a *adminClient, pos []string, o coreOpts) {
	u := o
	usage := func() { fail(strings.TrimSpace(upstreamHelp)) }
	if len(pos) < 2 {
		usage()
		return
	}
	team := o.team
	switch {
	case pos[1] == "add" && len(pos) == 4 && team == "":
		tok, err := readKey(u.in, "Copy Token of the upstream")
		if err != nil {
			fail(err.Error())
		}
		var r struct{ Name, Address, Device, Where, Probe string }
		req := map[string]any{"address": pos[3], "token": tok}
		if u.deviceName != "" {
			req["deviceName"] = u.deviceName
		}
		a.do("PUT", "/upstreams/"+url.PathEscape(pos[2]), req, &r)
		say(clean(fmt.Sprintf("upstream %s: %s", r.Name, r.Address)))
		say(clean("device " + r.Device + ": the upstream's owner must allow this device and bind it to a user there, and give that user a role in the project"))
		say(clean("credentials stored in " + r.Where))
		say(clean(r.Probe))
	case pos[1] == "list" && len(pos) == 2 && team == "":
		var r struct {
			Upstreams []struct{ Name, Address, Device string }
		}
		a.do("GET", "/upstreams", nil, &r)
		for _, x := range r.Upstreams {
			say(clean(fmt.Sprintf("%s  %s  device %s", x.Name, x.Address, x.Device)))
		}
		if len(r.Upstreams) == 0 {
			say("no upstreams (add one with: minigw gw upstream add <name> <address>)")
		}
	case pos[1] == "remove" && len(pos) == 3 && team == "":
		a.do("DELETE", "/upstreams/"+url.PathEscape(pos[2]), nil, nil)
		say("removed upstream " + clean(pos[2]) + ", its links and its credentials (what was sent stays there)")
	case (pos[1] == "link" || pos[1] == "unlink") && (len(pos) == 4 && team == "" || len(pos) == 3 && team != ""):
		name, local, remote, isTeam := pos[len(pos)-1], "", u.remoteProject, team != ""
		var id int64
		if isTeam {
			id, local = teamID(a, team), team
			remote = u.remoteTeam
			if u.remoteProject != "" {
				fail("--remote-project is for a project; a team link takes --remote-team")
			}
		} else {
			id, local = projectID(a, pos[2]), pos[2]
			if u.remoteTeam != "" {
				fail("--remote-team needs --team")
			}
		}
		if pos[1] == "unlink" {
			a.do("DELETE", fmt.Sprintf("/upstreams/%s/links/%d", url.PathEscape(name), id), map[string]any{"team": isTeam}, nil)
			say(clean("unlinked " + local + " from " + name))
			return
		}
		if remote == "" {
			remote = local // the same name there, unless told
		}
		if u.auto && u.manual {
			fail("--auto and --manual together: choose one")
		}
		if u.read && u.noRead {
			fail("--read and --no-read together: choose one")
		}
		req := map[string]any{"team": isTeam, "remote": remote, "classes": u.class, "withPrivate": u.withPrivate}
		if u.auto {
			req["auto"] = true
		} else if u.manual {
			req["auto"] = false
		}
		if u.mode != "" {
			req["mode"] = u.mode
		}
		reading := u.read || u.noRead || u.readClass != "" || u.readSources != ""
		pushFlags := u.class != "" || u.withPrivate || u.auto || u.manual || u.mode != "" || u.remoteProject != "" || u.remoteTeam != ""
		if reading {
			req["read"] = !u.noRead
			req["readClasses"], req["readSources"], req["acceptReach"] = u.readClass, u.readSources, u.acceptReach
			req["readOnly"] = !pushFlags // an existing link keeps its push settings
		}
		var linked struct{ ReadOff bool }
		a.do("PUT", fmt.Sprintf("/upstreams/%s/links/%d", url.PathEscape(name), id), req, &linked)
		if linked.ReadOff {
			defer say(clean("reading was turned off and the mirror deleted: the link now reaches another project or team than the one you accepted; repeat with --read --accept-reach to accept the new reach"))
		}
		if reading && !pushFlags {
			if u.noRead {
				say(clean(fmt.Sprintf("%s -> %s: reading is off, the mirror was deleted", local, name)))
			} else {
				say(clean(fmt.Sprintf("%s -> %s: reading is on; minigw gw upstream pull fills the mirror (members do not see it yet)", local, name)))
			}
			return
		}
		if u.auto {
			say(clean(fmt.Sprintf("linked %s to %s as %s: changes are sent automatically (a minute after the last one; minigw gw upstream status shows it)", local, name, remote)))
		} else {
			say(clean(fmt.Sprintf("linked %s to %s as %s: nothing is sent until minigw gw upstream push", local, name, remote)))
		}
		if u.withPrivate {
			what := "project"
			if isTeam {
				what = "team"
			}
			say(clean("also the private notes of ALL members of the " + what + " are sent"))
		}
		if reading {
			if u.noRead {
				say("reading is off, the mirror was deleted")
			} else {
				say("reading is on; minigw gw upstream pull fills the mirror (members do not see it yet)")
			}
		}
	case pos[1] == "status" && len(pos) <= 3:
		q := ""
		switch {
		case team != "" && len(pos) == 2:
			q = "?team=" + strconv.FormatInt(teamID(a, team), 10)
		case team == "" && len(pos) == 3:
			q = "?project=" + strconv.FormatInt(projectID(a, pos[2]), 10)
		case len(pos) != 2:
			usage()
		}
		var r struct{ Links []upLink }
		a.do("GET", "/upstreams"+q, nil, &r)
		for _, l := range r.Links {
			say(clean(upstreamLinkLine(l)))
			if s := upstreamSyncLine(l); s != "" {
				say(clean("  " + s))
			}
			say(clean("  " + upstreamReadLine(l)))
			if l.Read && l.PullError != "" {
				say(clean("  last pull error: " + l.PullError))
			}
			if l.LastError != "" {
				say(clean("  last refusal: " + l.LastError))
			}
			if s := upstreamNotPropagated(l); s != "" {
				say(clean("  " + s))
			}
			if s := upstreamConflicts(l); s != "" {
				say(clean("  " + s))
			}
		}
		if len(r.Links) == 0 {
			say("no upstream links (minigw gw upstream link <project> <name>)")
		}
	case pos[1] == "resolve" && (len(pos) == 4 && team == "" || len(pos) == 3 && team != ""):
		if u.theirs == u.ours {
			fail("resolve: choose --theirs or --ours")
		}
		note, err := strconv.ParseInt(pos[len(pos)-1], 10, 64)
		if err != nil || note <= 0 {
			fail("resolve: the note is its number")
		}
		body := map[string]any{"note": note, "ours": u.ours}
		if u.upstream != "" {
			body["upstream"] = u.upstream
		}
		if team != "" {
			body["team"] = teamID(a, team)
		} else {
			body["project"] = projectID(a, pos[2])
		}
		a.do("POST", "/upstream-resolve", body, nil)
		if u.ours {
			say(fmt.Sprintf("note %d: our version goes to the upstream as a submission at the next push", note))
		} else {
			say(fmt.Sprintf("note %d: the upstream's version stays, our change is forgotten for it", note))
		}
	case pos[1] == "push" && len(pos) <= 3:
		body := map[string]any{"dryRun": u.dryRun}
		switch {
		case team != "" && len(pos) == 2:
			body["team"] = teamID(a, team)
		case team == "" && len(pos) == 3:
			body["project"] = projectID(a, pos[2])
		case len(pos) != 2:
			usage()
		}
		// A helper answers a call within 30 seconds and a run sends a bounded number of notes: a push that is not through comes back with
		// "more" and where it stopped; ask again for that link, as long as it gets further (or only waits for the upstream's write limit,
		// a bounded number of times).
		targets := []map[string]any{body}
		lastNext, stalls := map[string]int64{}, map[string]int{}
		for len(targets) > 0 {
			var next []map[string]any
			wait := 0
			for _, tgt := range targets {
				var r struct{ Pushes []upPush }
				a.do("POST", "/upstream-push", tgt, &r)
				upstreamPrint(r.Pushes)
				for _, p := range r.Pushes {
					if !p.More {
						continue
					}
					key := fmt.Sprint(p.Project, "|", p.Upstream)
					switch {
					case p.Next > lastNext[key]:
						lastNext[key], stalls[key] = p.Next, 0
					case p.Wait > 0 && stalls[key] < 30:
						stalls[key]++
					default:
						say(clean(fmt.Sprintf("  %s -> %s: no progress: run the push again later", p.Label, p.Upstream)))
						continue
					}
					nt := map[string]any{"upstream": p.Upstream, "after": p.Next, "dryRun": u.dryRun}
					if t, ok := core.SegmentTeam(p.Project); p.Team && ok {
						nt["team"] = t
					} else {
						nt["project"] = p.Project
					}
					next = append(next, nt)
					wait = max(wait, p.Wait)
				}
			}
			targets = next
			if wait > 0 && len(targets) > 0 {
				pause := u.pause
				if pause == nil {
					pause = time.Sleep
				}
				pause(time.Duration(wait) * time.Second)
			}
		}
	case pos[1] == "pull" && len(pos) <= 3:
		body := map[string]any{"dryRun": u.dryRun}
		switch {
		case team != "" && len(pos) == 2:
			body["team"] = teamID(a, team)
		case team == "" && len(pos) == 3:
			body["project"] = projectID(a, pos[2])
		case len(pos) != 2:
			usage()
		}
		if u.dryRun {
			var r struct{ Pulls []upPull }
			a.do("POST", "/upstream-pull", body, &r)
			for _, p := range r.Pulls {
				say(clean(fmt.Sprintf("%s -> %s (%s)", p.Label, p.Upstream, p.Remote)))
				for _, l := range p.Lines {
					say(clean("  " + l))
				}
				if p.Stopped != "" {
					say(clean("  stopped: " + p.Stopped))
				} else {
					say(fmt.Sprintf("  dry run: %d notes could be read, nothing was stored", p.Total))
				}
			}
			return
		}
		// A run reads a bounded number of notes (and a helper answers a call within 30 seconds): a pull that is not through comes back with
		// "more" and its cursor; ask again for that link, as long as it gets further (or only waits for the upstream's read limit, a bounded
		// number of times).
		// The loop is bounded: at most maxPullRuns runs (the per-link cap of notes) and pullWall of time per command, whatever an upstream offers.
		targets := []map[string]any{body}
		lastNext, stalls, runs := map[string]int64{}, map[string]int{}, map[string]int{}
		wall := u.pullWall
		if wall == 0 {
			wall = pullWallDefault
		}
		now := u.now
		if now == nil {
			now = time.Now
		}
		deadline := now().Add(wall)
		for len(targets) > 0 {
			var next []map[string]any
			wait := 0
			for _, tgt := range targets {
				var r struct{ Pulls []upPull }
				a.do("POST", "/upstream-pull", tgt, &r)
				for _, p := range r.Pulls {
					upstreamPullPrint(p)
					if !p.More {
						continue
					}
					key := fmt.Sprint(p.Project, "|", p.Upstream)
					runs[key]++
					switch {
					case runs[key] >= maxPullRuns || now().After(deadline):
						say(clean(fmt.Sprintf("  %s -> %s: more remains: run the pull again", p.Label, p.Upstream)))
						continue
					case p.Next > lastNext[key]:
						lastNext[key], stalls[key] = p.Next, 0
					case p.Wait > 0 && stalls[key] < 30:
						stalls[key]++
					default:
						say(clean(fmt.Sprintf("  %s -> %s: no progress: run the pull again later", p.Label, p.Upstream)))
						continue
					}
					nt := map[string]any{"upstream": p.Upstream, "dryRun": false}
					if t, ok := core.SegmentTeam(p.Project); p.Team && ok {
						nt["team"] = t
					} else {
						nt["project"] = p.Project
					}
					next = append(next, nt)
					wait = max(wait, p.Wait)
				}
			}
			targets = next
			if wait > 0 && len(targets) > 0 {
				pause := u.pause
				if pause == nil {
					pause = time.Sleep
				}
				pause(time.Duration(wait) * time.Second)
			}
		}
	default:
		usage()
	}
}

// maxPullRuns: runs of one pull command for one link (25 runs of 200 notes are the 5000 notes a link's mirror may hold); pullWallDefault: its time.
const (
	maxPullRuns     = 25
	pullWallDefault = 10 * time.Minute
)

type upPull struct {
	Label, Upstream, Remote string
	Lines                   []string
	Total                   int
	Stopped                 string
	Added, Updated          int
	Removed, Skipped        int
	Mirror                  int
	Purged, More            bool
	Wait                    int
	Next, Project           int64
	Team                    bool
}

func upstreamPullPrint(p upPull) {
	say(clean(fmt.Sprintf("%s -> %s (%s)", p.Label, p.Upstream, p.Remote)))
	for _, l := range p.Lines {
		say(clean("  " + l))
	}
	say(fmt.Sprintf("  added %d, updated %d, removed %d, skipped %d, mirror %d notes", p.Added, p.Updated, p.Removed, p.Skipped, p.Mirror))
	if p.Purged {
		say("  the upstream withdrew the right to read: the mirror of this link was deleted")
	}
	if p.Stopped != "" {
		say(clean("  stopped: " + p.Stopped))
	}
}

type upPush struct {
	Label, Upstream, Remote string
	Lines                   []string
	Created, Skipped        int
	Replaced, Refused       int
	Conflicts               int
	Stopped                 string
	DryRun                  bool
	More                    bool
	Wait                    int
	Next                    int64
	Project                 int64
	Team                    bool
}

func upstreamPrint(pushes []upPush) {
	for _, p := range pushes {
		say(clean(fmt.Sprintf("%s -> %s (%s)", p.Label, p.Upstream, p.Remote)))
		for _, l := range p.Lines {
			say(clean("  " + l))
		}
		verb := ""
		if p.DryRun {
			verb = "would be "
		}
		tail := ""
		if p.Conflicts > 0 {
			tail = fmt.Sprintf(", conflicts %d", p.Conflicts)
		}
		say(fmt.Sprintf("  %screated %d, replaced %d, skipped %d, refused %d%s", verb, p.Created, p.Replaced, p.Skipped, p.Refused, tail))
		if p.Stopped != "" {
			say(clean("  stopped: " + p.Stopped))
		}
	}
}

func upstreamLinkLine(l upLink) string {
	last := "never pushed"
	if l.LastPush != nil {
		last = "last push " + l.LastPush.Local().Format("2006-01-02 15:04")
	}
	cls := strings.Join(l.Classes, ",")
	how := "manual"
	if l.Auto {
		how = "auto"
	}
	if l.Mode == "publish" {
		how += ", publish"
	}
	return fmt.Sprintf("%s -> %s (%s): %d of %d notes would go, %s, classes %s, %s", l.Label, l.Upstream, l.Remote, l.Pending, l.Total, last, cls, how)
}

// upstreamReadLine: whether the link reads, the reach the owner accepted, the mirror and how current it is.
func upstreamReadLine(l upLink) string {
	if !l.Read {
		return "read: off"
	}
	src := "none"
	if len(l.ReadSources) > 0 {
		src = strings.Join(l.ReadSources, ",")
	}
	acc := ""
	if l.ReadAccepted != nil {
		acc = ", accepted " + l.ReadAccepted.Local().Format("2006-01-02 15:04") + " for " + l.Remote
	}
	pulled := ", never pulled"
	if l.LastPullOK != nil {
		pulled = ", last pull " + l.LastPullOK.Local().Format("2006-01-02 15:04") + " (" + upstreamAge(l.LastPullAge) + " old)"
	}
	if l.LastPull != nil && (l.LastPullOK == nil || l.LastPull.After(*l.LastPullOK)) {
		pulled += ", last try " + l.LastPull.Local().Format("2006-01-02 15:04")
	}
	return fmt.Sprintf("read: on, classes %s, sources %s%s, mirror %d notes (%s)%s", strings.Join(l.ReadClasses, ","), src, acc, l.Mirror, upstreamSize(l.MirrorBytes), pulled)
}

func upstreamSize(b int64) string {
	switch {
	case b < 1024:
		return fmt.Sprintf("%d B", b)
	case b < 1<<20:
		return fmt.Sprintf("%d.%d KiB", b/1024, b%1024*10/1024)
	}
	return fmt.Sprintf("%d.%d MiB", b>>20, b%(1<<20)*10/(1<<20))
}

func upstreamAge(sec int) string {
	switch {
	case sec < 60:
		return "under a minute"
	case sec < 3600:
		return fmt.Sprintf("%d min", sec/60)
	case sec < 86400:
		return fmt.Sprintf("%d h", sec/3600)
	}
	return fmt.Sprintf("%d d", sec/86400)
}

// upstreamSyncLine: the state of the automatic push, when there is any to tell.
func upstreamSyncLine(l upLink) string {
	var parts []string
	at := func(label string, t *time.Time) {
		if t != nil {
			parts = append(parts, label+" "+t.Local().Format("2006-01-02 15:04"))
		}
	}
	at("dirty since", l.DirtySince)
	at("last success", l.LastOK)
	at("last failure", l.LastFail)
	if l.FailCount > 1 {
		parts[len(parts)-1] += fmt.Sprintf(" (%d in a row)", l.FailCount)
	}
	at("next attempt", l.NextAttempt)
	if len(parts) == 0 {
		return ""
	}
	return "sync: " + strings.Join(parts, "; ")
}

// upstreamNotPropagated: sent notes that were deleted or made private here, which stay on the upstream until removed there.
func upstreamNotPropagated(l upLink) string {
	by := map[string][]string{}
	for _, n := range l.NotPropagated {
		by[n.Why] = append(by[n.Why], strconv.FormatInt(n.Note, 10))
	}
	var parts []string
	for _, why := range []string{"deleted here", "now private here"} {
		if ids := by[why]; len(ids) > 0 {
			parts = append(parts, why+": note "+strings.Join(ids, ", "))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return "not propagated: " + strings.Join(parts, "; ") + " (the copy stays on the upstream until you remove it there)"
}

// upstreamConflicts: publish notes that changed on the upstream and were not overwritten.
func upstreamConflicts(l upLink) string {
	var parts []string
	for _, c := range l.Conflicts {
		what := "changed there"
		if c.Gone {
			what = "deleted there"
		}
		parts = append(parts, fmt.Sprintf("note %d (%s)", c.Note, what))
	}
	if len(parts) == 0 {
		return ""
	}
	return "CONFLICT: " + strings.Join(parts, ", ") + ": not overwritten until minigw gw upstream resolve <project> <note> --theirs|--ours"
}
