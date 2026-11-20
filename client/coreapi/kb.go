package coreapi

import (
	"context"
	"encoding/json"
	"minigw/core"
	"net/url"
	"strconv"
	"time"
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

// reviewRoute: a project admin's review of the project's submissions (member API, member context only):
// GET /kb/review?after=, GET /kb/review/{id}, POST /kb/review/{id}/accept|reject {"reason"}. The store decides who may.
func (a *API) reviewRoute(ctx context.Context, st *core.Store, p core.Principal, project int64, rest []string, q url.Values, r Request) Response {
	switch {
	case len(rest) == 0 && r.Method == "GET":
		after, e := submissionAfter(q)
		if e != nil {
			return a.errResponse(e)
		}
		vs, e := st.ListReview(ctx, p, project, after)
		if e != nil {
			return a.errResponse(e)
		}
		return submissionPage(vs)
	case len(rest) == 1 && r.Method == "GET":
		id, ok := posID(rest[0])
		if !ok {
			return a.errResponse(core.ErrNotFound)
		}
		v, writer, e := st.GetReview(ctx, p, project, id)
		if e != nil {
			return a.errResponse(e)
		}
		return resp(200, struct {
			core.Submission
			AuthorName string `json:"authorName"`
		}{v, writer})
	case len(rest) == 2 && r.Method == "POST" && (rest[1] == "accept" || rest[1] == "reject"):
		id, ok := posID(rest[0])
		if !ok {
			return a.errResponse(core.ErrNotFound)
		}
		var b struct {
			Reason          string `json:"reason"`
			ReviewHash      string `json:"reviewHash"`
			CurrentRevision int64  `json:"currentRevision"`
		}
		if e := payload(r, &b); e != nil {
			return a.errResponse(e)
		}
		if b.CurrentRevision < 0 {
			return a.errResponse(core.ErrInvalid{What: "currentRevision"})
		}
		v, e := st.MemberReviewAt(ctx, p, project, id, rest[1] == "accept", b.Reason, b.ReviewHash, b.CurrentRevision)
		if e != nil {
			return a.errResponse(e)
		}
		return resp(200, v)
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
	if len(seg) >= 4 && seg[1] == "notes" {
		id, ok := posID(seg[2])
		if !ok {
			return a.errResponse(core.ErrNotFound)
		}
		return a.noteHistoryRoute(ctx, st, nil, 0, id, seg[3:], Request{Method: method, Body: body})
	}
	if len(seg) >= 2 && (seg[1] == "notes" || seg[1] == "faq") {
		return a.adminKBRead(ctx, st, method, seg, q)
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
		return a.adminSubmissionPage(ctx, st, vs)
	}
	if len(seg) < 3 {
		return a.errResponse(core.ErrNotFound)
	}
	id, ok := posID(seg[2])
	if !ok {
		return a.errResponse(core.ErrNotFound)
	}
	if len(seg) == 5 && seg[3] == "versions" && method == "GET" { // the panel's detail: only while it is the row's version
		v, e := st.GetSubmissionVersion(ctx, id, seg[4])
		if e != nil {
			return a.errResponse(e)
		}
		return a.adminSubmission(ctx, st, v)
	}
	if len(seg) == 3 && method == "GET" {
		v, e := st.GetSubmission(ctx, core.Principal{}, 0, id, true)
		if e != nil {
			return a.errResponse(e)
		}
		return a.adminSubmission(ctx, st, v)
	}
	if len(seg) == 4 && method == "POST" && (seg[3] == "accept" || seg[3] == "reject") {
		var b struct {
			Reason          string `json:"reason"`
			ReviewHash      string `json:"reviewHash"`
			CurrentRevision int64  `json:"currentRevision"`
		}
		if e := payload(Request{Body: body}, &b); e != nil {
			return a.errResponse(e)
		}
		if b.CurrentRevision < 0 {
			return a.errResponse(core.ErrInvalid{What: "currentRevision"})
		}
		v, e := st.ReviewSubmissionAt(ctx, id, seg[3] == "accept", b.Reason, b.ReviewHash, b.CurrentRevision)
		if e != nil {
			return a.errResponse(e)
		}
		return resp(200, v)
	}
	return a.errResponse(core.ErrNotFound)
}

// adminSubmissionJSON is a submission as the owner's admin routes show it: the stored fields, plus what a panel would
// otherwise have to join: the author's name, the project's name or the team's (never the hidden segment id as a project),
// when it was received, and whether it replaces a note.
type adminSubmissionJSON struct {
	core.Submission
	AuthorName    string    `json:"authorName"`
	ScopeKind     string    `json:"scopeKind"` // "project" or "team"
	ScopeName     string    `json:"scopeName"`
	Received      time.Time `json:"received"`
	IsReplacement bool      `json:"isReplacement"`
	// AcceptInPanel: the queue row may offer Accept: the review is ok and nothing is replaced. A replacement is accepted where its
	// current text is shown in full (minigw gw kb accept, or the remote page), never from a row.
	AcceptInPanel bool `json:"acceptInPanel"`
	// AcceptReplacementInPanel: the row may offer "Accept replacement" (with a confirmation): the review is ok and the replaced note
	// is unchanged, so both texts are ok and the detail shows them.
	AcceptReplacementInPanel bool `json:"acceptReplacementInPanel"`
	// LinesChange and CharsChange: "current -> new" for a replacement ("-" for a side that is not there), "" otherwise: what a panel
	// row shows so that a shortened rewrite is seen (the numbers are the review read's currentLines/newLines and chars).
	LinesChange string `json:"linesChange"`
	CharsChange string `json:"charsChange"`
}

func sizeChange(cur, now *int) string {
	s := func(p *int) string {
		if p == nil {
			return "-"
		}
		return strconv.Itoa(*p)
	}
	if cur == nil && now == nil {
		return ""
	}
	return s(cur) + " -> " + s(now)
}

type nameMaps struct{ users, projects, teams map[int64]string }

func (a *API) names(ctx context.Context, st *core.Store) (nameMaps, error) {
	m := nameMaps{map[int64]string{}, map[int64]string{}, map[int64]string{}}
	us, err := st.ListUsers(ctx)
	if err != nil {
		return m, err
	}
	for _, u := range us {
		m.users[u.ID] = u.Name
	}
	ps, err := st.AdminListProjects(ctx)
	if err != nil {
		return m, err
	}
	for _, p := range ps {
		m.projects[p.ID] = p.Name
	}
	ts, err := st.AdminListTeams(ctx)
	if err != nil {
		return m, err
	}
	for _, t := range ts {
		m.teams[t.ID] = t.Name
	}
	return m, nil
}

func (m nameMaps) submission(v core.Submission) adminSubmissionJSON {
	j := adminSubmissionJSON{Submission: v, AuthorName: m.users[v.User], Received: v.Provenance.Received, IsReplacement: v.Replaces > 0, AcceptInPanel: v.ReviewCheck == "ok" && v.TargetState == "none", AcceptReplacementInPanel: v.ReviewCheck == "ok" && v.TargetState == "ok", LinesChange: sizeChange(v.CurrentLines, v.NewLines), CharsChange: sizeChange(v.CurrentChars, v.NewChars), ScopeKind: "project", ScopeName: m.projects[v.Project]}
	if team, ok := core.SegmentTeam(v.Project); ok {
		j.ScopeKind, j.ScopeName = "team", m.teams[team]
		j.Project, j.Provenance.Project = 0, 0 // the hidden segment id never leaves: scopeKind and scopeName say where it belongs
	}
	return j
}

func (a *API) adminSubmissionPage(ctx context.Context, st *core.Store, vs []core.Submission) Response {
	m, e := a.names(ctx, st)
	if e != nil {
		return a.errResponse(e)
	}
	out := []adminSubmissionJSON{}
	for _, v := range vs {
		out = append(out, m.submission(v))
	}
	var next int64
	if len(vs) == 100 {
		next = vs[len(vs)-1].ID
	}
	return resp(200, map[string]any{"submissions": out, "next": next})
}

// PeekSubmission: the owner's review read of a submission as the admin route shows it, WITHOUT the kb.owner.read audit: for the
// gateway's own checks (the remote accept refusal), whose answer never leaves the process. 0 = not found or no database.
func (a *API) PeekSubmission(ctx context.Context, id int64) (int, map[string]any) {
	st := a.store()
	if st == nil {
		return 0, nil
	}
	v, e := st.PeekSubmission(ctx, id)
	if e != nil {
		return 404, nil
	}
	r := a.adminSubmission(ctx, st, v)
	var out map[string]any
	if r.Status != 200 || json.Unmarshal(r.Body, &out) != nil {
		return 0, nil
	}
	return 200, out
}

func (a *API) adminSubmission(ctx context.Context, st *core.Store, v core.Submission) Response {
	m, e := a.names(ctx, st)
	if e != nil {
		return a.errResponse(e)
	}
	return resp(200, m.submission(v))
}

// adminKBRead: GET /kb/notes?project=&class=&kind=&after=&limit=, GET /kb/notes/{id}, GET /kb/faq?project=&after=. The
// owner reads every class, `private` included (owner only: the member routes keep their own checks). `project` is a project
// id or a team's segment id (the `segment` of GET /teams); a team's notes carry the team's name and never the hidden id.
func (a *API) adminKBRead(ctx context.Context, st *core.Store, method string, seg []string, q url.Values) Response {
	if method != "GET" || len(seg) > 3 || (seg[1] == "faq" && len(seg) != 2) {
		return a.errResponse(core.ErrNotFound)
	}
	if seg[1] == "notes" && len(seg) == 3 {
		id, ok := posID(seg[2])
		if !ok {
			return a.errResponse(core.ErrNotFound)
		}
		n, e := st.OwnerGetNote(ctx, id)
		if e != nil {
			return a.errResponse(e)
		}
		return resp(200, adminNote(n, true))
	}
	project, ok := posID(q.Get("project"))
	if !ok {
		return a.errResponse(core.ErrInvalid{What: "project"})
	}
	if seg[1] == "faq" {
		pg, e := st.OwnerListFAQ(ctx, project, q.Get("after"))
		if e != nil {
			return a.errResponse(e)
		}
		return resp(200, faqJSON(pg))
	}
	limit := 0
	if v := q.Get("limit"); v != "" {
		n, e := strconv.Atoi(v)
		if e != nil {
			return a.errResponse(core.ErrInvalid{What: "limit"})
		}
		limit = n
	}
	pg, e := st.OwnerListNotes(ctx, project, core.Class(q.Get("class")), q.Get("kind"), q.Get("after"), limit)
	if e != nil {
		return a.errResponse(e)
	}
	notes := []NoteJSON{}
	for _, n := range pg.Notes {
		notes = append(notes, adminNote(n, false))
	}
	res := map[string]any{"notes": notes}
	if pg.Next != "" {
		res["next"] = pg.Next
	}
	return resp(200, res)
}

// adminNote is toNote without the hidden segment id (also in the provenance): a team's note has project 0 and the team's name.
func adminNote(n core.Note, withBody bool) NoteJSON {
	if _, seg := core.SegmentTeam(n.ProjectID); seg {
		n.Provenance.Project = 0 // (n is a copy)
		n.ProjectID = 0
	}
	return toNote(n, withBody)
}
