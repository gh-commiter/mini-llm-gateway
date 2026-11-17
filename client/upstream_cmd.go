package main

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const upstreamHelp = `
  minigw gw upstream add <name> <address>              another gateway to send knowledge to (host[:port]); its Copy Token on stdin
  minigw gw upstream list | remove <name>
  minigw gw upstream link <project> <name> [--remote-project <slug>] [--class structure,data] [--with-private]
  minigw gw upstream link --team <team> <name> [--remote-team <name>]   a team's own notes
  minigw gw upstream unlink <project> <name> | --team <team> <name>
  minigw gw upstream status [<project>|--team <team>]  per link: what would go, when it last went, the last refusal
  minigw gw upstream push [<project>|--team <team>] [--dry-run]   send the published notes as submissions (nothing without a link)`

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
		a.do("PUT", "/upstreams/"+url.PathEscape(pos[2]), map[string]any{"address": pos[3], "token": tok}, &r)
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
		a.do("PUT", fmt.Sprintf("/upstreams/%s/links/%d", url.PathEscape(name), id), map[string]any{"team": isTeam, "remote": remote, "classes": u.class, "withPrivate": u.withPrivate}, nil)
		say(clean(fmt.Sprintf("linked %s to %s as %s: nothing is sent until minigw gw upstream push", local, name, remote)))
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
			if l.LastError != "" {
				say(clean("  last refusal: " + l.LastError))
			}
		}
		if len(r.Links) == 0 {
			say("no upstream links (minigw gw upstream link <project> <name>)")
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
		var r struct {
			Pushes []struct {
				Label, Upstream, Remote string
				Lines                   []string
				Created, Skipped        int
				Replaced, Refused       int
				Stopped                 string
				DryRun                  bool
			}
		}
		a.do("POST", "/upstream-push", body, &r)
		for _, p := range r.Pushes {
			say(clean(fmt.Sprintf("%s -> %s (%s)", p.Label, p.Upstream, p.Remote)))
			for _, l := range p.Lines {
				say(clean("  " + l))
			}
			verb := ""
			if p.DryRun {
				verb = "would be "
			}
			say(fmt.Sprintf("  %screated %d, replaced %d, skipped %d, refused %d", verb, p.Created, p.Replaced, p.Skipped, p.Refused))
			if p.Stopped != "" {
				say(clean("  stopped: " + p.Stopped))
			}
		}
	default:
		usage()
	}
}

func upstreamLinkLine(l upLink) string {
	last := "never pushed"
	if l.LastPush != nil {
		last = "last push " + l.LastPush.Local().Format("2006-01-02 15:04")
	}
	cls := strings.Join(l.Classes, ",")
	return fmt.Sprintf("%s -> %s (%s): %d of %d notes would go, %s, classes %s", l.Label, l.Upstream, l.Remote, l.Pending, l.Total, last, cls)
}
