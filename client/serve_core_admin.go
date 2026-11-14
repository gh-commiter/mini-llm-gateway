package main

// Admin routes of the core (docs/admin-api.md): users, bindings, projects, members, grants, audit, backup. They run
// behind adminHTTP's full authentication (loopback, Host, MGW-Admin proof, agents refused); the core does not check
// who the caller is.

import (
	"context"
	"errors"
	"net/url"
	"path/filepath"
	"strconv"
	"time"

	"minigw/core"
)

type userJSON struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Active bool   `json:"active"`
	Owner  bool   `json:"owner"`
}

// coreAdmin handles the core's admin routes; ok = false when seg is not one of them.
func (s *server) coreAdmin(method string, seg []string, q url.Values, body []byte) (status int, obj any, ok bool) {
	switch seg[0] {
	case "users", "bindings", "projects", "grants", "audit", "core", "quota":
	case "usage": // only usage/events; GET /usage is the 0.5.x folder counters (serve_admin.go)
		if len(seg) != 2 || seg[1] != "events" {
			return 0, nil, false
		}
	default:
		return 0, nil, false
	}
	fail := func(status int, msg string) (int, any, bool) { return status, errorBody(status, msg), true }
	if seg[0] == "quota" { // no core needed
		if len(seg) != 1 {
			return fail(404, "Not found: "+method+" "+seg[0])
		}
		switch method {
		case "PATCH": // {"claudeProbe": bool}: the opt-in of the Claude probe
			var b struct{ ClaudeProbe *bool }
			if decodeStrict(body, &b) != nil || b.ClaudeProbe == nil {
				return fail(400, "bad JSON body (claudeProbe)")
			}
			if err := s.update(func(c *serveConfig) error { c.ClaudeQuotaProbe = *b.ClaudeProbe; return nil }); err != nil {
				return fail(500, err.Error())
			}
		case "GET":
		default:
			return fail(404, "Not found: "+method+" "+seg[0])
		}
		return 200, map[string]any{"quota": s.quotaReport(context.Background()), "claudeProbe": s.conf().ClaudeQuotaProbe}, true
	}
	if s.core == nil {
		why := "the gateway is not running"
		if s.coreErr != nil {
			why = s.coreErr.Error()
		}
		return fail(503, "The project database is not available: "+why)
	}
	failErr := func(err error) (int, any, bool) {
		var inv core.ErrInvalid
		switch {
		case errors.Is(err, core.ErrNotFound):
			return fail(404, "not found")
		case errors.As(err, &inv):
			return fail(400, inv.Error())
		}
		s.log.Printf("admin core error: %v", err)
		return fail(500, "internal error: "+err.Error())
	}
	ctx := context.Background()
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
	if seg[0] == "core" && len(seg) == 2 {
		route = method + " core/" + seg[1]
	}
	switch route {
	case "GET users":
		us, err := s.core.ListUsers(ctx)
		if err != nil {
			return failErr(err)
		}
		list := []userJSON{}
		for _, u := range us {
			list = append(list, userJSON{u.ID, u.Name, u.Active, u.Owner})
		}
		return 200, map[string]any{"users": list}, true
	case "POST users":
		var b struct{ Name string }
		if !dec(&b) {
			return fail(400, "bad JSON body")
		}
		u, err := s.core.CreateUser(ctx, b.Name)
		if err != nil {
			return failErr(err)
		}
		return 200, map[string]any{"user": userJSON{u.ID, u.Name, u.Active, u.Owner}}, true
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
			err = s.core.RenameUser(ctx, id, *b.Name)
		}
		if err == nil && b.Active != nil {
			err = s.core.SetUserActive(ctx, id, *b.Active)
		}
		if err == nil && b.Owner != nil {
			err = s.core.SetUserOwner(ctx, id, *b.Owner)
		}
		if err != nil {
			return failErr(err)
		}
		return 200, map[string]any{"ok": true}, true
	case "GET bindings":
		bs, err := s.core.ListBindings(ctx)
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
		if err := s.core.SetBinding(ctx, seg[1], b.User); err != nil {
			return failErr(err)
		}
		return 200, map[string]any{"ok": true}, true
	case "DELETE bindings/{}":
		if err := s.core.RemoveBinding(ctx, seg[1]); err != nil {
			return failErr(err)
		}
		return 200, map[string]any{"ok": true}, true
	case "GET projects":
		ps, err := s.core.AdminListProjects(ctx)
		if err != nil {
			return failErr(err)
		}
		list := []projectJSON{}
		for _, p := range ps {
			list = append(list, toProject(p))
		}
		return 200, map[string]any{"projects": list}, true
	case "POST projects":
		var b struct{ Slug, Name string }
		if !dec(&b) {
			return fail(400, "bad JSON body")
		}
		p, err := s.core.CreateProject(ctx, b.Slug, b.Name)
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
		if err := s.core.SetProjectArchived(ctx, id, *b.Archived); err != nil {
			return failErr(err)
		}
		return 200, map[string]any{"ok": true}, true
	case "GET projects/{}/members":
		id, good := num(seg[1])
		if !good {
			return fail(400, "bad project id")
		}
		ms, err := s.core.ListMembers(ctx, id)
		if err != nil {
			return failErr(err)
		}
		list := []map[string]any{}
		for _, m := range ms {
			list = append(list, map[string]any{"user": m.UserID, "name": m.Name, "role": m.Role})
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
			err = s.core.SetMember(ctx, pid, uid, b.Role)
		} else {
			err = s.core.RemoveMember(ctx, pid, uid)
		}
		if err != nil {
			return failErr(err)
		}
		return 200, map[string]any{"ok": true}, true
	case "GET grants":
		gs, err := s.core.ListGrants(ctx)
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
			err = s.core.AddGrant(ctx, b.Source, b.Reader, b.Class)
		} else {
			err = s.core.RemoveGrant(ctx, b.Source, b.Reader, b.Class)
		}
		if err != nil {
			return failErr(err)
		}
		return 200, map[string]any{"ok": true}, true
	case "GET audit":
		after, _ := strconv.ParseInt(q.Get("after"), 10, 64)
		limit, _ := strconv.Atoi(q.Get("limit"))
		evs, err := s.core.AuditEvents(ctx, after, limit)
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
	case "GET usage/{}": // /usage/events
		days := 7
		if v := q.Get("days"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				return fail(400, "days: a number")
			}
			days = n
		}
		f := core.UsageFilter{Model: q.Get("model")}
		f.User, _ = strconv.ParseInt(q.Get("user"), 10, 64)
		f.Project, _ = strconv.ParseInt(q.Get("project"), 10, 64)
		rep, err := s.core.AdminUsage(ctx, days, f)
		if err != nil {
			return failErr(err)
		}
		return 200, usageJSON(rep), true
	case "POST core/backup":
		var b struct{ File string }
		if !dec(&b) || b.File == "" || !filepath.IsAbs(b.File) {
			return fail(400, "file: an absolute path on the gateway computer")
		}
		if err := s.core.Backup(ctx, b.File); err != nil {
			return fail(400, "backup: "+err.Error())
		}
		return 200, map[string]any{"file": b.File}, true
	}
	return fail(404, "Not found: "+method+" "+seg[0])
}
