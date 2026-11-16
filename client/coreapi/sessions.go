package coreapi

import (
	"context"
	"minigw/core"
	"net/url"
	"strconv"
	"strings"
)

// IsSessionsPath: the routes that answer a session page: GET /minigw/v1/projects/{id}/sessions (member) and
// GET /usage/sessions (admin, below the admin prefix). The adapter adds device names to those pages only.
func IsSessionsPath(method, p string) bool {
	if method != "GET" {
		return false
	}
	if p == "/usage/sessions" {
		return true
	}
	if !strings.HasPrefix(p, projectsPrefix+"/") {
		return false
	}
	seg := strings.Split(strings.Trim(strings.TrimPrefix(p, projectsPrefix), "/"), "/")
	return len(seg) == 2 && seg[1] == "sessions"
}

func sessionFilter(q url.Values) (core.SessionFilter, error) {
	f := core.SessionFilter{Days: 7, Model: q.Get("model"), Device: q.Get("device"), After: q.Get("after")}
	for _, x := range []struct {
		k string
		v *int
	}{{"days", &f.Days}, {"limit", &f.Limit}} {
		if q.Has(x.k) {
			n, e := strconv.Atoi(q.Get(x.k))
			if e != nil || n < 1 {
				return f, core.ErrInvalid{What: x.k}
			}
			*x.v = n
		}
	}
	return f, nil
}
func (a *API) sessionRoute(ctx context.Context, st *core.Store, p core.Principal, project string, q url.Values, admin bool) Response {
	f, err := sessionFilter(q)
	if err != nil {
		return a.errResponse(err)
	}
	var id int64
	if admin {
		if project == "" {
			return a.errResponse(core.ErrInvalid{What: "project required"})
		}
		ps, e := st.AdminListProjects(ctx)
		if e != nil {
			return a.errResponse(e)
		}
		for _, pr := range ps {
			if pr.Slug == project || strconv.FormatInt(pr.ID, 10) == project {
				id = pr.ID
				break
			}
		}
		if id == 0 {
			return a.errResponse(core.ErrNotFound)
		}
	} else {
		pr, e := st.GetProject(ctx, p, project)
		if e != nil {
			return a.errResponse(e)
		}
		id = pr.ID
	}
	var page core.SessionPage
	if admin {
		page, err = st.AdminSessions(ctx, id, f)
	} else {
		page, err = st.Sessions(ctx, p, id, f)
	}
	if err != nil {
		return a.errResponse(err)
	}
	return resp(200, page)
}
