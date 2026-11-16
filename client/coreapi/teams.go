package coreapi

import (
	"context"
	"errors"
	"net/url"
	"strings"

	"minigw/core"
)

const teamsPrefix = "/minigw/v1/teams"

type TeamJSON struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Archived bool   `json:"archived"`
}

type TeamMemberJSON struct {
	User int64  `json:"user"`
	Name string `json:"name"`
}

type TeamDetailJSON struct {
	TeamJSON
	Members  []TeamMemberJSON `json:"members"`
	Projects []ProjectJSON    `json:"projects"`
}

func toTeam(t core.Team) TeamJSON { return TeamJSON{t.ID, t.Name, t.Archived} }
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
	return TeamDetailJSON{toTeam(t.Team), teamMembers(t.Members), teamProjects(t.Projects)}
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
		out := []TeamJSON{}
		for _, t := range ts {
			out = append(out, toTeam(t))
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

func (a *API) adminTeams(ctx context.Context, st *core.Store, method string, seg []string, body []byte) (int, any, bool) {
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
			out := []TeamJSON{}
			for _, t := range ts {
				out = append(out, toTeam(t))
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
			}
			if decodeStrict(body, &b) != nil || (b.Name == nil) == (b.Archived == nil) {
				return fail(400, "bad JSON body (name or archived)")
			}
			var err error
			if b.Name != nil {
				err = st.RenameTeam(ctx, id, *b.Name)
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
			return 200, map[string]any{"assignments": assignments}, true
		}
		if method == "PUT" || method == "DELETE" {
			var b struct {
				Project int64
				User    int64
				Role    core.Role
			}
			if decodeStrict(body, &b) != nil || b.Project <= 0 || b.User <= 0 {
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
