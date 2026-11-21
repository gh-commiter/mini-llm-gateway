package coreapi

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"minigw/core"
)

const teamsPrefix = "/minigw/v1/teams"

type TeamJSON struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Archived bool   `json:"archived"`
	Path     string `json:"path"`
	// Segment is the id of the team's own notes: the notes, search and submission routes take it as a project id.
	Segment int64 `json:"segment"`
	// Generation: as ProjectJSON's, in the team list only (where a linked gateway finds its team's notes).
	Generation string `json:"generation,omitempty"`
}

type TeamMemberJSON struct {
	User int64  `json:"user"`
	Name string `json:"name"`
}

type TeamDetailJSON struct {
	TeamJSON
	Members  []TeamMemberJSON `json:"members"`
	Projects []ProjectJSON    `json:"projects"`
	// Notes: how many notes the team's own notes hold (only for a roster member of an active team).
	Notes *int `json:"notes,omitempty"`
}

func toTeam(t core.Team) TeamJSON {
	return TeamJSON{t.ID, t.Name, t.Archived, t.Path, core.TeamSegment(t.ID), ""}
}
func teamMembers(ms []core.TeamMember) []TeamMemberJSON {
	out := []TeamMemberJSON{}
	for _, m := range ms {
		out = append(out, TeamMemberJSON{m.UserID, m.Name})
	}
	return out
}
func teamProjects(ps []core.Project) []ProjectJSON {
	out := []ProjectJSON{}
	for _, p := range ps {
		out = append(out, toProject(p))
	}
	return out
}
func teamDetail(t core.TeamDetail) TeamDetailJSON {
	return TeamDetailJSON{toTeam(t.Team), teamMembers(t.Members), teamProjects(t.Projects), t.Notes}
}

func (a *API) teamRoute(ctx context.Context, st *core.Store, p core.Principal, r Request) Response {
	if r.Method != "GET" {
		return a.errResponse(core.ErrNotFound)
	}
	rest := strings.Trim(strings.TrimPrefix(r.Path, teamsPrefix), "/")
	if rest == "" {
		q, _ := url.ParseQuery(r.RawQuery)
		ts, err := st.ListTeams(ctx, p, q.Get("archived") == "1")
		if err != nil {
			return a.errResponse(err)
		}
		gen, err := st.HubGeneration(ctx)
		if err != nil {
			return a.errResponse(err)
		}
		out := []TeamJSON{}
		for _, t := range ts {
			j := toTeam(t)
			j.Generation = gen
			out = append(out, j)
		}
		return resp(200, out)
	}
	if strings.Contains(rest, "/") {
		return a.errResponse(core.ErrNotFound)
	}
	t, err := st.GetTeam(ctx, p, rest)
	if err != nil {
		return a.errResponse(err)
	}
	return resp(200, teamDetail(t))
}

func (a *API) adminTeams(ctx context.Context, st *core.Store, method string, seg []string, q url.Values, body []byte) (int, any, bool) {
	fail := func(status int, msg string) (int, any, bool) { return status, errorBody(status, msg), true }
	failErr := func(err error) (int, any, bool) {
		var inv core.ErrInvalid
		switch {
		case errors.Is(err, core.ErrNotFound):
			return fail(404, "not found")
		case errors.As(err, &inv):
			return fail(400, inv.Error())
		}
		a.o.Log("admin core error: %v", err)
		return fail(500, "internal error: "+err.Error())
	}
	if len(seg) == 1 {
		switch method {
		case "GET":
			ts, err := st.AdminListTeams(ctx)
			if err != nil {
				return failErr(err)
			}
			counts, err := st.AdminCounts(ctx)
			if err != nil {
				return failErr(err)
			}
			out := []adminTeamJSON{}
			for _, t := range ts {
				out = append(out, adminTeamJSON{toTeam(t), counts.MembersOfTeam[t.ID], counts.ProjectsOfTeam[t.ID]})
			}
			return 200, map[string]any{"teams": out}, true
		case "POST":
			var b struct{ Name string }
			if decodeStrict(body, &b) != nil {
				return fail(400, "bad JSON body")
			}
			t, err := st.CreateTeam(ctx, b.Name)
			if err != nil {
				return failErr(err)
			}
			return 200, map[string]any{"team": toTeam(t)}, true
		}
	}
	if len(seg) < 2 || len(seg) > 4 {
		return fail(404, "not found")
	}
	id, ok := posID(seg[1])
	if !ok {
		return fail(400, "bad team id")
	}
	if len(seg) == 2 {
		switch method {
		case "GET":
			t, err := st.AdminGetTeam(ctx, id)
			if err != nil {
				return failErr(err)
			}
			return 200, teamDetail(t), true
		case "PATCH":
			var b struct {
				Name     *string
				Archived *bool
				Path     *string
			}
			bad := decodeStrict(body, &b) != nil
			n := 0
			for _, set := range []bool{b.Name != nil, b.Archived != nil, b.Path != nil} {
				if set {
					n++
				}
			}
			if bad || n != 1 {
				return fail(400, "bad JSON body (name, archived or path)")
			}
			var err error
			if b.Name != nil {
				err = st.RenameTeam(ctx, id, *b.Name)
			} else if b.Path != nil {
				err = st.SetTeamPath(ctx, id, *b.Path)
			} else {
				err = st.SetTeamArchived(ctx, id, *b.Archived)
			}
			if err != nil {
				return failErr(err)
			}
			return 200, map[string]any{"ok": true}, true
		}
	}
	if len(seg) == 3 && seg[2] == "assignments" {
		if method == "GET" {
			assignments, err := st.ListTeamAssignments(ctx, id)
			if err != nil {
				return failErr(err)
			}
			names, err := a.names(ctx, st)
			if err != nil {
				return failErr(err)
			}
			rows := []adminAssignmentJSON{}
			for _, x := range assignments {
				rows = append(rows, adminAssignmentJSON{x, fmt.Sprintf("%d-%d", x.Project, x.User), names.users[x.User], names.projects[x.Project]})
			}
			return 200, map[string]any{"assignments": rows}, true
		}
		if method == "PUT" || method == "DELETE" {
			var b struct {
				Project int64
				User    int64
				Role    core.Role
			}
			if method == "DELETE" && len(body) > 0 && (q.Get("project") != "" || q.Get("user") != "") {
				return fail(400, "give the assignment either as a JSON body or as ?project=&user=, not both")
			}
			if method == "DELETE" && len(body) == 0 { // DELETE takes ?project=&user= (a panel sends no body with a DELETE); a body still works
				b.Project, _ = strconv.ParseInt(q.Get("project"), 10, 64)
				b.User, _ = strconv.ParseInt(q.Get("user"), 10, 64)
			} else if decodeStrict(body, &b) != nil {
				return fail(400, "bad assignment body")
			}
			if b.Project <= 0 || b.User <= 0 {
				return fail(400, "bad assignment body")
			}
			var err error
			if method == "PUT" {
				err = st.SetTeamAssignment(ctx, id, b.Project, b.User, b.Role)
			} else {
				err = st.RemoveTeamAssignment(ctx, id, b.Project, b.User)
			}
			if err != nil {
				return failErr(err)
			}
			return 200, map[string]any{"ok": true}, true
		}
	}
	if len(seg) == 3 && method == "GET" {
		switch seg[2] {
		case "members":
			ms, err := st.ListTeamMembers(ctx, id)
			if err != nil {
				return failErr(err)
			}
			return 200, map[string]any{"members": teamMembers(ms)}, true
		case "projects":
			ps, err := st.ListTeamProjects(ctx, id)
			if err != nil {
				return failErr(err)
			}
			return 200, map[string]any{"projects": teamProjects(ps)}, true
		}
	}
	if len(seg) == 4 && (method == "PUT" || method == "DELETE") {
		target, ok := posID(seg[3])
		if !ok {
			return fail(400, "bad user or project id")
		}
		var err error
		switch seg[2] {
		case "members":
			if method == "PUT" {
				err = st.AddTeamMember(ctx, id, target)
			} else {
				err = st.RemoveTeamMember(ctx, id, target)
			}
		case "projects":
			if method == "PUT" {
				err = st.AddTeamProject(ctx, id, target)
			} else {
				err = st.RemoveTeamProject(ctx, id, target)
			}
		default:
			return fail(404, "not found")
		}
		if err != nil {
			return failErr(err)
		}
		return 200, map[string]any{"ok": true}, true
	}
	return fail(404, "not found")
}

// adminTeamJSON is a team in the owner's list with its counts (so a panel needs no extra calls).
type adminTeamJSON struct {
	TeamJSON
	MemberCount  int `json:"memberCount"`
	ProjectCount int `json:"projectCount"`
}

// adminAssignmentJSON is a team role assignment with an id and the names of the person and the project.
type adminAssignmentJSON struct {
	core.TeamAssignment
	ID          string `json:"id"`
	UserName    string `json:"userName"`
	ProjectName string `json:"projectName"`
}

// adminProjectJSON is a project in the owner's list with its team (a project is in at most one) and its number of members.
type adminProjectJSON struct {
	ProjectJSON
	TeamName    string `json:"teamName"`
	MemberCount int    `json:"memberCount"`
}

func (a *API) adminProjectRows(ctx context.Context, st *core.Store, ps []core.Project) ([]adminProjectJSON, error) {
	counts, err := st.AdminCounts(ctx)
	if err != nil {
		return nil, err
	}
	out := []adminProjectJSON{}
	for _, p := range ps {
		out = append(out, adminProjectJSON{toProject(p), counts.TeamOfProject[p.ID], counts.MembersOfProject[p.ID]})
	}
	return out, nil
}
