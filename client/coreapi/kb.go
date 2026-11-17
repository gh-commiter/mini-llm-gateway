package coreapi

import (
	"context"
	"minigw/core"
	"net/url"
	"strconv"
)

func (a *API) submissionRoute(ctx context.Context, st *core.Store, p core.Principal, project int64, rest []string, q url.Values, r Request) Response {
	if len(rest) == 1 && r.Method == "GET" {
		id, ok := posID(rest[0])
		if !ok {
			return a.errResponse(core.ErrNotFound)
		}
		v, e := st.GetSubmission(ctx, p, project, id, false)
		if e != nil {
			return a.errResponse(e)
		}
		return resp(200, v)
	}
	if len(rest) != 0 {
		return a.errResponse(core.ErrNotFound)
	}
	switch r.Method {
	case "GET":
		after, e := submissionAfter(q)
		if e != nil {
			return a.errResponse(e)
		}
		vs, e := st.ListSubmissions(ctx, p, project, after, false)
		if e != nil {
			return a.errResponse(e)
		}
		return submissionPage(vs)
	case "POST":
		var b struct {
			Class    core.Class `json:"class"`
			Title    string     `json:"title"`
			Body     string     `json:"body"`
			Replaces int64      `json:"replaces"`
			Revision int64      `json:"revision"`
			Kind     string     `json:"entryKind"`
		}
		if e := payload(r, &b); e != nil {
			return a.errResponse(e)
		}
		v, replay, e := st.Submit(ctx, p, project, r.Header.Get("Idempotency-Key"), core.SubmissionInput{NoteInput: core.NoteInput{Class: b.Class, Title: b.Title, Body: b.Body, EntryKind: b.Kind}, Replaces: b.Replaces, Revision: b.Revision})
		if e != nil {
			return a.errResponse(e)
		}
		if replay {
			return resp(201, v, "Idempotent-Replay", "true")
		}
		return resp(201, v)
	}
	return a.errResponse(core.ErrNotFound)
}
func submissionAfter(q url.Values) (int64, error) {
	if q.Get("after") == "" {
		return 0, nil
	}
	n, e := strconv.ParseInt(q.Get("after"), 10, 64)
	if e != nil || n < 0 {
		return 0, core.ErrInvalid{What: "after"}
	}
	return n, nil
}
func submissionPage(v []core.Submission) Response {
	var next int64
	if len(v) == 100 {
		next = v[len(v)-1].ID
	}
	return resp(200, map[string]any{"submissions": v, "next": next})
}
func (a *API) adminKB(ctx context.Context, st *core.Store, method string, seg []string, q url.Values, body []byte) Response {
	if len(seg) == 3 && seg[1] == "search" && seg[2] == "rebuild" && method == "POST" {
		if e := st.RebuildSearch(ctx); e != nil {
			return a.errResponse(e)
		}
		return resp(200, map[string]bool{"rebuilt": true})
	}
	if len(seg) < 2 || seg[1] != "submissions" {
		return a.errResponse(core.ErrNotFound)
	}
	if len(seg) == 2 && method == "GET" {
		var project int64
		if q.Get("project") != "" && q.Get("project") != "0" {
			var ok bool
			project, ok = posID(q.Get("project"))
			if !ok {
				return a.errResponse(core.ErrInvalid{What: "project"})
			}
		}
		after, e := submissionAfter(q)
		if e != nil {
			return a.errResponse(e)
		}
		vs, e := st.ListSubmissions(ctx, core.Principal{}, project, after, true)
		if e != nil {
			return a.errResponse(e)
		}
		return submissionPage(vs)
	}
	if len(seg) < 3 {
		return a.errResponse(core.ErrNotFound)
	}
	id, ok := posID(seg[2])
	if !ok {
		return a.errResponse(core.ErrNotFound)
	}
	if len(seg) == 3 && method == "GET" {
		v, e := st.GetSubmission(ctx, core.Principal{}, 0, id, true)
		if e != nil {
			return a.errResponse(e)
		}
		return resp(200, v)
	}
	if len(seg) == 4 && method == "POST" && (seg[3] == "accept" || seg[3] == "reject") {
		var b struct {
			Reason string `json:"reason"`
		}
		if e := payload(Request{Body: body}, &b); e != nil {
			return a.errResponse(e)
		}
		v, e := st.ReviewSubmission(ctx, id, seg[3] == "accept", b.Reason)
		if e != nil {
			return a.errResponse(e)
		}
		return resp(200, v)
	}
	return a.errResponse(core.ErrNotFound)
}
