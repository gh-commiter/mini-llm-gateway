package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

const teamHelp = `minigw team list [--archived] [--json]  your teams (directory only; no project access)
minigw team show <team> [--json]        team members and projects you can already read`

type cliTeam struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Archived bool   `json:"archived"`
	Path     string `json:"path"`
	Members  []struct {
		User int64  `json:"user"`
		Name string `json:"name"`
	} `json:"members"`
	Projects []struct {
		ID       int64  `json:"id"`
		Slug     string `json:"slug"`
		Name     string `json:"name"`
		Archived bool   `json:"archived"`
	} `json:"projects"`
	Notes *int `json:"notes"` // the team's own notes (a roster member's view only)
}

func teamLine(t cliTeam) string {
	line := fmt.Sprintf("%d  %s", t.ID, t.Name)
	if t.Archived {
		line += "  (archived)"
	}
	if t.Path != "" {
		line += "  path " + t.Path
	}
	return clean(line)
}

func teamID(a *adminClient, ref string) int64 {
	var r struct{ Teams []cliTeam }
	a.do("GET", "/teams", nil, &r)
	n, _ := strconv.ParseInt(ref, 10, 64)
	for _, t := range r.Teams {
		if n > 0 && t.ID == n || n <= 0 && strings.EqualFold(t.Name, ref) {
			return t.ID
		}
	}
	fail("no team " + strconv.Quote(ref) + " (minigw gw team list)")
	return 0
}

func teamAdmin(a *adminClient, pos []string) {
	usage := func() { fail(strings.TrimSpace(coreHelp)) }
	if len(pos) < 2 {
		usage()
		return
	}
	switch {
	case pos[1] == "access":
		if len(pos) < 4 {
			usage()
			return
		}
		op := pos[2]
		tid := teamID(a, pos[3])
		path := fmt.Sprintf("/teams/%d/assignments", tid)
		if op == "list" && len(pos) == 4 {
			var r struct {
				Assignments []struct {
					Project, User int64
					Role          string
				}
			}
			a.do("GET", path, nil, &r)
			for _, v := range r.Assignments {
				say(fmt.Sprintf("project %d  user %d  %s", v.Project, v.User, v.Role))
			}
		} else if (op == "set" && len(pos) == 7) || (op == "remove" && len(pos) == 6) {
			b := map[string]any{"project": projectID(a, pos[4]), "user": userID(a, pos[5])}
			method := "DELETE"
			if op == "set" {
				method = "PUT"
				b["role"] = pos[6]
			}
			a.do(method, path, b, nil)
		} else {
			usage()
		}
	case pos[1] == "list" && len(pos) == 2:
		var r struct{ Teams []cliTeam }
		a.do("GET", "/teams", nil, &r)
		for _, t := range r.Teams {
			say(teamLine(t))
		}
	case pos[1] == "add" && len(pos) == 3:
		a.do("POST", "/teams", map[string]any{"name": pos[2]}, nil)
		say("team " + clean(pos[2]) + " added")
	case pos[1] == "rename" && len(pos) == 4:
		a.do("PATCH", fmt.Sprintf("/teams/%d", teamID(a, pos[2])), map[string]any{"name": pos[3]}, nil)
	case pos[1] == "path" && len(pos) == 4: // "-" clears the label
		label := pos[3]
		if label == "-" {
			label = ""
		}
		a.do("PATCH", fmt.Sprintf("/teams/%d", teamID(a, pos[2])), map[string]any{"path": label}, nil)
	case pos[1] == "archive" && len(pos) == 3:
		a.do("PATCH", fmt.Sprintf("/teams/%d", teamID(a, pos[2])), map[string]any{"archived": true}, nil)
	case (pos[1] == "member" || pos[1] == "project") && len(pos) == 5 && (pos[2] == "add" || pos[2] == "remove"):
		tid := teamID(a, pos[3])
		var id int64
		collection := "members"
		if pos[1] == "member" {
			id = userID(a, pos[4])
		} else {
			collection = "projects"
			id = projectID(a, pos[4])
		}
		method := "PUT"
		if pos[2] == "remove" {
			method = "DELETE"
		}
		a.do(method, fmt.Sprintf("/teams/%d/%s/%d", tid, collection, id), nil, nil)
	default:
		usage()
	}
}

func (e *projectEnv) team(args []string) error {
	if e.agent {
		return errors.New(agentRefusal)
	}
	fs := flag.NewFlagSet("team", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "JSON")
	archived := fs.Bool("archived", false, "also archived teams")
	pos, err := parseFlags(fs, args)
	if err != nil || len(pos) == 0 || !(pos[0] == "list" && len(pos) == 1 || pos[0] == "show" && len(pos) == 2) {
		return errors.New("usage:\n" + teamHelp)
	}
	c, _ := e.connect()
	path := "/minigw/v1/teams"
	if pos[0] == "list" && *archived {
		path += "?archived=1"
	}
	if pos[0] == "show" {
		ref := pos[1]
		if n, err := strconv.ParseInt(ref, 10, 64); err != nil || n <= 0 {
			st, data, h := e.do(c, "GET", path+"?archived=1", nil, nil)
			if st != 200 {
				return gatewayError(st, data, h)
			}
			var teams []cliTeam
			if err := json.Unmarshal(data, &teams); err != nil {
				return err
			}
			ref = ""
			for _, t := range teams {
				if strings.EqualFold(t.Name, pos[1]) {
					ref = strconv.FormatInt(t.ID, 10)
					break
				}
			}
			if ref == "" {
				return gatewayError(404, nil, nil)
			}
		}
		path += "/" + url.PathEscape(ref)
	}
	st, data, h := e.do(c, "GET", path, nil, nil)
	if st != 200 {
		return gatewayError(st, data, h)
	}
	if *asJSON {
		e.say(strings.TrimSpace(string(data)))
		return nil
	}
	if pos[0] == "list" {
		var teams []cliTeam
		if err := json.Unmarshal(data, &teams); err != nil {
			return err
		}
		for _, t := range teams {
			e.say(teamLine(t))
		}
		if len(teams) == 0 {
			e.say("no teams: ask the gateway owner to add you to one")
		}
	} else {
		var t cliTeam
		if err := json.Unmarshal(data, &t); err != nil {
			return err
		}
		e.say(teamLine(t))
		for _, m := range t.Members {
			e.say(clean(fmt.Sprintf("member %d  %s", m.User, m.Name)))
		}
		for _, p := range t.Projects {
			line := fmt.Sprintf("project %d  %s  %s", p.ID, p.Slug, p.Name)
			if p.Archived {
				line += "  (archived)"
			}
			e.say(clean(line))
		}
		if t.Notes != nil {
			e.say(clean(fmt.Sprintf("team notes: %d  (read them: minigw note list --team %s)", *t.Notes, t.Name)))
		}
	}
	return nil
}

// Origin text is shared by owner explanations and member project lists.
type cliOrigin struct{ Kind, Name, Role string }

func originText(origins []cliOrigin, roles bool) string {
	parts := []string{}
	for _, o := range origins {
		label := o.Kind
		if o.Kind == "team" {
			label = "team " + o.Name
		}
		if roles {
			label += " " + o.Role
		}
		parts = append(parts, label)
	}
	return strings.Join(parts, ", ")
}

type memberReply struct {
	Role    string
	Origins []cliOrigin
}

func memberFeedback(r memberReply, direct string) {
	if r.Role != "" && r.Role != direct {
		say(clean("effective role remains " + r.Role + " (from " + originText(r.Origins, true) + ")"))
	}
}
