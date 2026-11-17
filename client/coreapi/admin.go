package coreapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"

	"minigw/core"
)

// UserJSON is the shape of a user in the admin routes.
type UserJSON struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Active bool   `json:"active"`
	Owner  bool   `json:"owner"`
}

func errorBody(status int, msg string) map[string]any {
	return map[string]any{"type": "error", "error": map[string]string{"type": errType(status), "message": msg}}
}

func errType(status int) string {
	t := map[int]string{401: "authentication_error", 403: "permission_error", 404: "not_found_error", 413: "request_too_large",
		429: "rate_limit_error", 529: "overloaded_error"}[status]
	if t == "" {
		t = "invalid_request_error"
		if status >= 500 {
			t = "api_error"
		}
	}
	return t
}

// IsAdminPath: does this admin route (path below the admin prefix, escaped) belong to the core? The adapter handles
// all others (devices, providers, settings, quota …) itself.
func IsAdminPath(method, escapedPath string) bool {
	seg := adminSeg(escapedPath)
	if len(seg) == 0 {
		return false
	}
	switch seg[0] {
	case "users", "bindings", "projects", "teams", "kb", "grants", "audit", "core", "export", "import":
		return true
	case "panels":
		return len(seg) == 1 || (len(seg) == 2 && seg[1] == "strings")
	case "quota":
		return len(seg) == 2 && seg[1] == "history"
	case "usage": // GET /usage itself remains the gateway counter report
		return len(seg) == 2 && (seg[1] == "events" || seg[1] == "sessions")
	}
	return false
}

func adminSeg(escapedPath string) (seg []string) {
	for _, p := range strings.Split(strings.Trim(escapedPath, "/"), "/") {
		v, err := url.PathUnescape(p)
		if err != nil {
			return nil
		}
		seg = append(seg, v)
	}
	if len(seg) == 1 && seg[0] == "" {
		return nil
	}
	return seg
}

// admin serves the core's admin routes; the adapter has authenticated the owner. Status 0 = not a core route.
func (a *API) admin(ctx context.Context, r Request) Response {
	if !IsAdminPath(r.Method, r.Path) {
		return Response{}
	}
	seg := adminSeg(r.Path)
	if seg[0] == "panels" {
		return a.adminPanels(r.Method, seg, r.RawQuery)
	}
	if seg[0] == "kb" && (r.BodyTooLarge || len(r.Body) > MaxBody) {
		return a.errResponse(core.ErrTooLarge)
	}
	if seg[0] == "kb" && r.BodyUnreadable {
		return a.errResponse(core.ErrInvalid{What: "body"})
	}
	q, _ := url.ParseQuery(r.RawQuery)
	st := a.store()
	fail := func(status int, msg string) (int, any, bool) { return status, errorBody(status, msg), true }
	var code int
	var obj any
	if st == nil {
		why := a.o.Unavailable()
		if why == "" {
			why = "the gateway is not running"
		}
		code, obj, _ = fail(503, "The project database is not available: "+why)
	} else if seg[0] == "export" || seg[0] == "import" {
		return a.adminBundle(ctx, st, r)
	} else if seg[0] == "kb" {
		return a.adminKB(ctx, st, r.Method, seg, q, r.Body)
	} else {
		code, obj, _ = a.adminDo(ctx, st, r.Method, seg, q, r.Body)
	}
	return resp(code, obj)
}

func (a *API) adminDo(ctx context.Context, st *core.Store, method string, seg []string, q url.Values, body []byte) (status int, obj any, ok bool) {
	fail := func(status int, msg string) (int, any, bool) { return status, errorBody(status, msg), true }
	failErr := func(err error) (int, any, bool) {
		var inv core.ErrInvalid
		switch {
		case errors.Is(err, core.ErrNotFound):
			return fail(404, "not found")
		case errors.As(err, &inv):
			return fail(400, inv.Error())
		case errors.Is(err, core.ErrArchived):
			return fail(403, "the project is archived")
		}
		a.o.Log("admin core error: %v", err)
		return fail(500, "internal error: "+err.Error())
	}
	num := func(v string) (int64, bool) { n, err := strconv.ParseInt(v, 10, 64); return n, err == nil && n > 0 }
	dec := func(v any) bool { return decodeStrict(body, v) == nil }
	route := method + " " + seg[0]
	if len(seg) > 1 {
		route += "/{}"
	}
	if len(seg) > 2 {
		route += "/" + seg[2]
	}
	if len(seg) > 3 {
		route += "/{}"
	}
	if (seg[0] == "core" || seg[0] == "usage") && len(seg) == 2 {
		route = method + " " + seg[0] + "/" + seg[1]
	}
	if seg[0] == "teams" {
		return a.adminTeams(ctx, st, method, seg, body)
	}
	switch route {
	case "GET users":
		us, err := st.ListUsers(ctx)
		if err != nil {
			return failErr(err)
		}
		list := []UserJSON{}
		for _, u := range us {
			list = append(list, UserJSON{u.ID, u.Name, u.Active, u.Owner})
		}
		return 200, map[string]any{"users": list}, true
	case "POST users":
		var b struct{ Name string }
		if !dec(&b) {
			return fail(400, "bad JSON body")
		}
		u, err := st.CreateUser(ctx, b.Name)
		if err != nil {
			return failErr(err)
		}
		return 200, map[string]any{"user": UserJSON{u.ID, u.Name, u.Active, u.Owner}}, true
	case "PATCH users/{}":
		id, good := num(seg[1])
		var b struct {
			Name   *string
			Active *bool
			Owner  *bool
		}
		if !good || !dec(&b) {
			return fail(400, "bad user id or JSON body")
		}
		var err error
		if b.Name != nil {
			err = st.RenameUser(ctx, id, *b.Name)
		}
		if err == nil && b.Active != nil {
			err = st.SetUserActive(ctx, id, *b.Active)
		}
		if err == nil && b.Owner != nil {
			err = st.SetUserOwner(ctx, id, *b.Owner)
		}
		if err != nil {
			return failErr(err)
		}
		return 200, map[string]any{"ok": true}, true
	case "GET bindings":
		bs, err := st.ListBindings(ctx)
		if err != nil {
			return failErr(err)
		}
		list := []map[string]any{}
		for _, b := range bs {
			list = append(list, map[string]any{"device": b.DeviceID, "user": b.UserID})
		}
		return 200, map[string]any{"bindings": list}, true
	case "PUT bindings/{}":
		var b struct{ User int64 }
		if !dec(&b) {
			return fail(400, "bad JSON body")
		}
		if err := st.SetBinding(ctx, seg[1], b.User); err != nil {
			return failErr(err)
		}
		return 200, map[string]any{"ok": true}, true
	case "DELETE bindings/{}":
		if err := st.RemoveBinding(ctx, seg[1]); err != nil {
			return failErr(err)
		}
		return 200, map[string]any{"ok": true}, true
	case "GET projects":
		ps, err := st.AdminListProjects(ctx)
		if err != nil {
			return failErr(err)
		}
		list := []ProjectJSON{}
		for _, p := range ps {
			list = append(list, toProject(p))
		}
		return 200, map[string]any{"projects": list}, true
	case "POST projects":
		var b struct{ Slug, Name string }
		if !dec(&b) {
			return fail(400, "bad JSON body")
		}
		p, err := st.CreateProject(ctx, b.Slug, b.Name)
		if err != nil {
			return failErr(err)
		}
		return 200, map[string]any{"project": toProject(p)}, true
	case "PATCH projects/{}":
		id, good := num(seg[1])
		var b struct{ Archived *bool }
		if !good || !dec(&b) || b.Archived == nil {
			return fail(400, "bad project id or JSON body (archived)")
		}
		if err := st.SetProjectArchived(ctx, id, *b.Archived); err != nil {
			return failErr(err)
		}
		return 200, map[string]any{"ok": true}, true
	case "GET projects/{}/depends":
		id, good := num(seg[1])
		if !good {
			return fail(400, "bad project id")
		}
		dep, used, err := st.AdminListDepends(ctx, id)
		if err != nil {
			return failErr(err)
		}
		return 200, map[string]any{"depends": dependsJSON(dep), "usedBy": dependsJSON(used)}, true
	case "PUT projects/{}/depends/{}", "DELETE projects/{}/depends/{}":
		id, good := num(seg[1])
		other, good2 := num(seg[3])
		if !good || !good2 {
			return fail(400, "bad project id")
		}
		var err error
		if method == "PUT" {
			err = st.AddDepends(ctx, id, other)
		} else {
			err = st.RemoveDepends(ctx, id, other)
		}
		if err != nil {
			return failErr(err)
		}
		return 200, map[string]any{"ok": true}, true
	case "GET projects/{}/remotes", "POST projects/{}/remotes":
		id, good := num(seg[1])
		if !good {
			return fail(400, "bad project id")
		}
		var list []string
		var b remotesBody
		var err error
		if method == "GET" {
			list, err = st.AdminProjectRemotes(ctx, id)
		} else {
			if !dec(&b) {
				return fail(400, "bad JSON body (add, remove)")
			}
			list, err = st.AdminChangeProjectRemotes(ctx, id, b.Add, b.Remove)
		}
		if err != nil {
			return failErr(err)
		}
		return 200, remotesReply(list, b), true
	case "GET projects/{}/members":
		id, good := num(seg[1])
		if !good {
			return fail(400, "bad project id")
		}
		ms, err := st.ListMembers(ctx, id)
		if err != nil {
			return failErr(err)
		}
		list := []map[string]any{}
		for _, m := range ms {
			list = append(list, map[string]any{"user": m.UserID, "name": m.Name, "role": m.Role, "origins": m.Origins})
		}
		return 200, map[string]any{"members": list}, true
	case "PUT projects/{}/members/{}", "DELETE projects/{}/members/{}":
		pid, good1 := num(seg[1])
		uid, good2 := num(seg[3])
		if !good1 || !good2 {
			return fail(400, "bad project or user id")
		}
		var err error
		if method == "PUT" {
			var b struct{ Role core.Role }
			if !dec(&b) {
				return fail(400, "bad JSON body")
			}
			err = st.SetMember(ctx, pid, uid, b.Role)
		} else {
			err = st.RemoveMember(ctx, pid, uid)
		}
		if err != nil {
			return failErr(err)
		}
		ms, e := st.ListMembers(ctx, pid)
		if e != nil {
			return failErr(e)
		}
		for _, m := range ms {
			if m.UserID == uid {
				return 200, map[string]any{"ok": true, "role": m.Role, "origins": m.Origins}, true
			}
		}
		return 200, map[string]any{"ok": true}, true
	case "GET grants":
		gs, err := st.ListGrants(ctx)
		if err != nil {
			return failErr(err)
		}
		list := []map[string]any{}
		for _, g := range gs {
			list = append(list, map[string]any{"source": g.SourceID, "reader": g.ReaderID, "class": g.Class})
		}
		return 200, map[string]any{"grants": list}, true
	case "POST grants", "DELETE grants":
		var b struct {
			Source, Reader int64
			Class          core.Class
		}
		if method == "POST" {
			if !dec(&b) {
				return fail(400, "bad JSON body")
			}
		} else { // DELETE has no body: ?source=&reader=&class=
			b.Source, _ = strconv.ParseInt(q.Get("source"), 10, 64)
			b.Reader, _ = strconv.ParseInt(q.Get("reader"), 10, 64)
			b.Class = core.Class(q.Get("class"))
		}
		var err error
		if method == "POST" {
			err = st.AddGrant(ctx, b.Source, b.Reader, b.Class)
		} else {
			err = st.RemoveGrant(ctx, b.Source, b.Reader, b.Class)
		}
		if err != nil {
			return failErr(err)
		}
		return 200, map[string]any{"ok": true}, true
	case "GET audit":
		after, _ := strconv.ParseInt(q.Get("after"), 10, 64)
		limit, _ := strconv.Atoi(q.Get("limit"))
		evs, err := st.AuditEvents(ctx, after, limit)
		if err != nil {
			return failErr(err)
		}
		list := []map[string]any{}
		for _, e := range evs {
			list = append(list, map[string]any{"id": e.ID, "time": e.Time.Format(time.RFC3339), "actor": e.Actor, "action": e.Action, "subject": e.Subject, "detail": e.Detail})
		}
		res := map[string]any{"events": list}
		if len(evs) > 0 {
			res["next"] = evs[len(evs)-1].ID // pass as ?after= for the next page
		}
		return 200, res, true
	case "GET quota/{}":
		days := 7
		if v := q.Get("days"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				return fail(400, "days: 1 to 400")
			}
			days = n
		}
		for k := range q {
			if k != "days" && k != "provider" {
				return fail(400, "unknown quota history filter")
			}
		}
		if days < 1 || days > 400 {
			return fail(400, "days: 1 to 400")
		}
		rep, capped, err := st.QuotaHistory(ctx, days, q.Get("provider"))
		if err != nil {
			return failErr(err)
		}
		if capped {
			return 206, rep, true
		}
		return 200, rep, true
	case "GET usage/sessions":
		r := a.sessionRoute(ctx, st, core.Principal{}, q.Get("project"), q, true)
		return r.Status, json.RawMessage(r.Body), true
	case "GET usage/events":
		days := 7
		if v := q.Get("days"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				return fail(400, "days: a number")
			}
			days = n
		}
		f := core.UsageFilter{Model: q.Get("model"), Provider: q.Get("provider")}
		f.User, _ = strconv.ParseInt(q.Get("user"), 10, 64)
		f.Project, _ = strconv.ParseInt(q.Get("project"), 10, 64)
		f.Phase, f.Activity, f.Task, f.Labels = q.Get("phase"), q.Get("activity"), q.Get("task"), q.Get("labels") == "1"
		rep, err := st.AdminUsage(ctx, days, f)
		if err != nil {
			return failErr(err)
		}
		return 200, UsageJSON(rep), true
	case "POST core/backup":
		var b struct{ Name string }
		if !dec(&b) || b.Name == "" {
			return fail(400, "name: the backup's name (letters, digits, . _ -); it is written into the core folder's backups folder, a path is not accepted")
		}
		dest, err := st.BackupNamed(ctx, b.Name)
		if err != nil {
			return fail(400, "backup: "+err.Error())
		}
		return 200, map[string]any{"name": b.Name, "file": dest}, true
	}
	return fail(404, "Not found: "+method+" "+seg[0])
}
