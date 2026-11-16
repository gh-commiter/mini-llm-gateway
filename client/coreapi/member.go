package coreapi

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/url"
	"strconv"
	"strings"
	"time"

	"minigw/core"
)

const (
	projectsPrefix = "/minigw/v1/projects"
	usagePath      = "/minigw/v1/usage"
	notesPath      = "/minigw/v1/notes"
)

// IsMemberPath: the paths of the member routes (the adapter routes them to Handle before the model routes' checks).
func IsMemberPath(p string) bool {
	return p == projectsPrefix || strings.HasPrefix(p, projectsPrefix+"/") || p == usagePath || p == teamsPrefix || strings.HasPrefix(p, teamsPrefix+"/")
}

// --- JSON shapes (identical on both gateways) ---

type ProjectJSON struct {
	ID       int64                   `json:"id"`
	Slug     string                  `json:"slug"`
	Name     string                  `json:"name"`
	Archived bool                    `json:"archived"`
	Role     core.Role               `json:"role,omitempty"`
	Granted  []core.Class            `json:"granted"`
	Origins  []core.MembershipOrigin `json:"origins,omitempty"`
}

func toProject(p core.Project) ProjectJSON {
	if p.Granted == nil {
		p.Granted = []core.Class{}
	}
	return ProjectJSON{p.ID, p.Slug, p.Name, p.Archived, p.Role, p.Granted, p.Origins}
}

type NoteJSON struct {
	ID       int64      `json:"id"`
	Project  int64      `json:"project"`
	Class    core.Class `json:"class"`
	Title    string     `json:"title"`
	Body     *string    `json:"body,omitempty"`
	Author   string     `json:"author"`
	Created  time.Time  `json:"created"`
	Updated  time.Time  `json:"updated"`
	Revision int64      `json:"revision"`
}

func toNote(n core.Note, withBody bool) NoteJSON {
	j := NoteJSON{n.ID, n.ProjectID, n.Class, n.Title, nil, n.Author, n.Created, n.Updated, n.Revision}
	if withBody {
		j.Body = &n.Body
	}
	return j
}

type UsageRowJSON struct {
	Cost                 *string `json:"costUsd"`
	EstimatedEvents      int64   `json:"estimatedCostEvents"`
	SubscriptionEvents   int64   `json:"subscriptionEvents"`
	UnknownBillingEvents int64   `json:"unknownBillingEvents"`
	Provider             string  `json:"provider"`
	ReportedCost         *string `json:"reportedCostUsd"`
	EstimatedCost        *string `json:"estimatedCostUsd"`
	UnknownCost          int64   `json:"eventsWithoutCost"`
	Day                  string  `json:"day"`
	Model                string  `json:"model"`
	User                 int64   `json:"user,omitempty"`
	UserName             string  `json:"userName,omitempty"`
	Project              int64   `json:"project,omitempty"`
	Events               int64   `json:"events"`
	TokensIn             int64   `json:"tokensIn"`
	TokensOut            int64   `json:"tokensOut"`
	UnknownTokens        int64   `json:"eventsWithoutTokens"`
	Nested               int64   `json:"nestedEvents"`
	Failed               int64   `json:"failedEvents"`
	ErrorEvents          int64   `json:"errorEvents"`
	CancelledEvents      int64   `json:"cancelledEvents"`
	InterruptedEvents    int64   `json:"interruptedEvents"`
	DurationMs           int64   `json:"durationMs"`
	FailedDurationMs     int64   `json:"failedDurationMs"`
	UnknownDuration      int64   `json:"unknownDurationEvents"`
}

type UsageGapJSON struct {
	Count int64     `json:"count"`
	From  time.Time `json:"from"`
	To    time.Time `json:"to"`
}

// UsageJSON turns a report into the shape of the usage routes.
func UsageJSON(rep core.UsageReport) map[string]any {
	rows, gaps := []UsageRowJSON{}, []UsageGapJSON{}
	for _, r := range rep.Rows {
		var total *int64
		if r.ReportedCost != nil {
			v := *r.ReportedCost
			total = &v
		}
		if r.EstimatedCost != nil {
			if total == nil {
				v := *r.EstimatedCost
				total = &v
			} else if *total <= math.MaxInt64-*r.EstimatedCost {
				*total += *r.EstimatedCost
			} else {
				total = nil
			}
		}
		rows = append(rows, UsageRowJSON{costString(total), r.EstimatedEvents, r.SubscriptionEvents, r.UnknownBillingEvents, r.Provider, costString(r.ReportedCost), costString(r.EstimatedCost), r.UnknownCost, r.Day, r.Model, r.UserID, r.UserName, r.ProjectID, r.Events, r.TokensIn, r.TokensOut, r.UnknownTokens, r.Nested, r.Failed, r.ErrorEvents, r.CancelledEvents, r.InterruptedEvents, r.DurationMs, r.FailedDurationMs, r.UnknownDuration})
	}
	for _, g := range rep.Gaps {
		gaps = append(gaps, UsageGapJSON{g.Count, g.From, g.To})
	}
	return map[string]any{"days": rows, "gaps": gaps, "timezone": "UTC"}
}

// member serves the member routes. Failures of the device or the principal all answer the same 403
// no_project_access; the gateway's own device refusals and the exceptions of the model routes do not apply.
func (a *API) member(ctx context.Context, c Context, r Request) Response {
	if c.AgentPeer { // a process of one of the gateway's agents may not use the member routes
		a.o.Log("project route refused: agent peer")
		return noAccess()
	}
	st := a.store()
	if st == nil {
		return unavailable()
	}
	if c.DeviceID == "" { // no device proven in this tunnel
		return noAccess()
	}
	p, err := st.Principal(ctx, c.DeviceID, c.Approved) // approved NOW, as the adapter says
	if err != nil {
		if !errors.Is(err, core.ErrNoProjectAccess) {
			return a.errResponse(err)
		}
		a.o.Log("project route refused: %v", err) // the reason stays in the log
		return noAccess()
	}
	if a.o.AfterAdmission != nil {
		a.o.AfterAdmission(ctx, c, r)
	}
	if r.Path == usagePath {
		return a.usageRoute(ctx, st, p, r)
	}
	if r.Path == teamsPrefix || strings.HasPrefix(r.Path, teamsPrefix+"/") {
		return a.teamRoute(ctx, st, p, r)
	}
	return a.projectRoute(ctx, st, p, r)
}

type noteBody struct {
	Class    core.Class `json:"class"`
	Title    string     `json:"title"`
	Body     string     `json:"body"`
	Revision *int64     `json:"revision"`
}

// payload parses the body of a write: nil = parsed.
func payload(r Request, v any) error {
	if r.BodyTooLarge {
		return core.ErrTooLarge
	}
	if r.BodyUnreadable {
		return core.ErrInvalid{What: "body"}
	}
	if err := decodeStrict(r.Body, v); err != nil {
		return core.ErrInvalid{What: "JSON body"}
	}
	return nil
}

func posID(v string) (int64, bool) {
	n, err := strconv.ParseInt(v, 10, 64)
	return n, err == nil && n > 0
}

func (a *API) projectRoute(ctx context.Context, st *core.Store, p core.Principal, r Request) Response {
	seg := strings.Split(strings.Trim(strings.TrimPrefix(r.Path, projectsPrefix), "/"), "/")
	if seg[0] == "" {
		seg = nil
	}
	q, _ := url.ParseQuery(r.RawQuery)
	notFound := a.errResponse(core.ErrNotFound)
	switch {
	case len(seg) == 0 && r.Method == "GET":
		ps, err := st.ListProjects(ctx, p, q.Get("archived") == "1")
		if err != nil {
			return a.errResponse(err)
		}
		list := []ProjectJSON{}
		for _, x := range ps {
			list = append(list, toProject(x))
		}
		return resp(200, list)
	case len(seg) == 1 && r.Method == "GET":
		pr, err := st.GetProject(ctx, p, seg[0])
		if err != nil {
			return a.errResponse(err)
		}
		return resp(200, toProject(pr))
	case len(seg) == 2 && seg[1] == "link-check" && r.Method == "POST":
		pr, err := st.ResolveForLink(ctx, p, seg[0])
		if err != nil {
			return a.errResponse(err)
		}
		return resp(200, toProject(pr))
	case len(seg) >= 2 && seg[1] == "notes" && len(seg) <= 3:
		pid, ok := posID(seg[0])
		if !ok {
			return a.errResponse(core.ErrInvalid{What: "project id"})
		}
		return a.noteRoute(ctx, st, p, pid, seg[2:], q, r)
	}
	return notFound
}

func (a *API) noteRoute(ctx context.Context, st *core.Store, p core.Principal, pid int64, rest []string, q url.Values, r Request) Response {
	if len(rest) == 0 {
		switch r.Method {
		case "GET":
			limit := 0
			if v := q.Get("limit"); v != "" {
				n, err := strconv.Atoi(v)
				if err != nil {
					return a.errResponse(core.ErrInvalid{What: "limit"})
				}
				limit = n
			}
			pg, err := st.ListNotes(ctx, p, pid, core.Class(q.Get("class")), q.Get("after"), limit)
			if err != nil {
				return a.errResponse(err)
			}
			notes := []NoteJSON{}
			for _, n := range pg.Notes {
				notes = append(notes, toNote(n, false))
			}
			res := map[string]any{"notes": notes}
			if pg.Next != "" {
				res["next"] = pg.Next
			}
			return resp(200, res)
		case "POST":
			var b noteBody
			if err := payload(r, &b); err != nil {
				return a.errResponse(err)
			}
			n, replay, err := st.CreateNote(ctx, p, pid, r.Header.Get("Idempotency-Key"), core.NoteInput{Class: b.Class, Title: b.Title, Body: b.Body})
			if err != nil {
				return a.errResponse(err)
			}
			if replay {
				return resp(201, toNote(n, true), "Idempotent-Replay", "true")
			}
			return resp(201, toNote(n, true))
		}
		return a.errResponse(core.ErrNotFound)
	}
	nid, ok := posID(rest[0])
	if !ok {
		return a.errResponse(core.ErrInvalid{What: "note id"})
	}
	// A bad payload or revision about a note the caller may not see or change is still 404 or 403 (visible first).
	payloadErr := func(err error) Response {
		if cerr := st.CanChangeNote(ctx, p, pid, nid); cerr != nil {
			return a.errResponse(cerr)
		}
		return a.errResponse(err)
	}
	switch r.Method {
	case "GET":
		n, err := st.GetNote(ctx, p, pid, nid)
		if err != nil {
			return a.errResponse(err)
		}
		return resp(200, toNote(n, true))
	case "PUT":
		var b noteBody
		if err := payload(r, &b); err != nil {
			return payloadErr(err)
		}
		if b.Revision == nil {
			return payloadErr(core.ErrInvalid{What: "revision"})
		}
		n, err := st.UpdateNote(ctx, p, pid, nid, *b.Revision, core.NoteInput{Class: b.Class, Title: b.Title, Body: b.Body})
		if err != nil {
			return a.errResponse(err)
		}
		return resp(200, toNote(n, true))
	case "DELETE":
		rev, err := strconv.ParseInt(q.Get("revision"), 10, 64)
		if err != nil {
			return payloadErr(core.ErrInvalid{What: "revision"})
		}
		if err := st.DeleteNote(ctx, p, pid, nid, rev); err != nil {
			return a.errResponse(err)
		}
		return Response{Status: 204}
	}
	return a.errResponse(core.ErrNotFound)
}

// usageRoute: GET /minigw/v1/usage?project=&days= (project: id or slug; days 1-400, default 7).
func (a *API) usageRoute(ctx context.Context, st *core.Store, p core.Principal, r Request) Response {
	if r.Method != "GET" {
		return a.errResponse(core.ErrNotFound)
	}
	q, _ := url.ParseQuery(r.RawQuery)
	days := 7
	if v := q.Get("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return a.errResponse(core.ErrInvalid{What: "days"})
		}
		days = n
	}
	var project int64
	if v := q.Get("project"); v != "" {
		pr, err := st.GetProject(ctx, p, v) // not visible: 404, like the project itself
		if err != nil {
			return a.errResponse(err)
		}
		project = pr.ID
	}
	rep, err := st.Usage(ctx, p, project, days)
	if err != nil {
		return a.errResponse(err)
	}
	return resp(200, UsageJSON(rep))
}

// run serves a run's notes capability: GET /minigw/v1/notes[?class=&after=&limit=] and GET /minigw/v1/notes/{id}.
// The capability's project and the grants of that project only; a project named in the path, query, header or body is
// not read.
func (a *API) run(ctx context.Context, c Context, r Request) Response {
	st := a.store()
	if st == nil {
		return unavailable()
	}
	if c.Run == nil || r.Method != "GET" || !(r.Path == notesPath || strings.HasPrefix(r.Path, notesPath+"/")) {
		return a.errResponse(core.ErrNotFound)
	}
	cap := *c.Run
	rest := strings.TrimPrefix(strings.TrimPrefix(r.Path, notesPath), "/")
	q, _ := url.ParseQuery(r.RawQuery)
	if rest == "" {
		limit := 0
		if v := q.Get("limit"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				return a.errResponse(core.ErrInvalid{What: "limit"})
			}
			limit = n
		}
		pg, err := st.RunListNotes(ctx, cap, c.Approved, core.Class(q.Get("class")), q.Get("after"), limit)
		if err != nil {
			return a.errResponse(err)
		}
		notes := []NoteJSON{}
		for _, n := range pg.Notes {
			notes = append(notes, toNote(n, false))
		}
		res := map[string]any{"notes": notes}
		if pg.Next != "" {
			res["next"] = pg.Next
		}
		return resp(200, res)
	}
	id, ok := posID(rest)
	if !ok {
		return a.errResponse(core.ErrNotFound)
	}
	n, err := st.RunGetNote(ctx, cap, c.Approved, id)
	if err != nil {
		return a.errResponse(err)
	}
	return resp(200, toNote(n, true))
}

func costString(n *int64) *string {
	if n == nil {
		return nil
	}
	s := fmt.Sprintf("%d.%09d", *n/1000000000, *n%1000000000)
	return &s
}
