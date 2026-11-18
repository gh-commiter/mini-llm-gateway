package coreapi

import (
	"context"
	"encoding/json"
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
	Pending  int                     `json:"pendingReview,omitempty"` // project list, admins only: submissions waiting for their review
}

func toProject(p core.Project) ProjectJSON {
	if p.Granted == nil {
		p.Granted = []core.Class{}
	}
	return ProjectJSON{p.ID, p.Slug, p.Name, p.Archived, p.Role, p.Granted, p.Origins, p.Pending}
}

type NoteJSON struct {
	Provenance       *core.Provenance `json:"provenance,omitempty"`
	UnverifiedSource json.RawMessage  `json:"unverifiedSource,omitempty"`
	ID               int64            `json:"id"`
	Project          int64            `json:"project"`
	Class            core.Class       `json:"class"`
	Title            string           `json:"title"`
	Body             *string          `json:"body,omitempty"`
	Author           string           `json:"author"`
	Editor           string           `json:"editor,omitempty"`
	Created          time.Time        `json:"created"`
	Updated          time.Time        `json:"updated"`
	Revision         int64            `json:"revision"`
	EntryKind        string           `json:"entryKind"`
	Team             string           `json:"team,omitempty"` // the team of a note in a team's own notes (Project is then a hidden id)
	Origin           string           `json:"origin,omitempty"`
	// From, Upstream and AsOf: a note of another gateway's mirror (read-only here; the body starts with the marker line).
	From     string     `json:"from,omitempty"`
	Upstream string     `json:"upstream,omitempty"`
	Source   string     `json:"source,omitempty"`
	Text     *string    `json:"text,omitempty"` // the sanitised text itself; body is the framed, quoted rendering
	AsOf     *time.Time `json:"asOf,omitempty"`
}

func toNote(n core.Note, withBody bool) NoteJSON {
	j := NoteJSON{ID: n.ID, Project: n.ProjectID, Class: n.Class, Title: n.Title, Author: n.Author, Editor: n.Editor, Created: n.Created, Updated: n.Updated, Revision: n.Revision, EntryKind: n.EntryKind, Team: n.Team, Origin: n.Origin}
	if j.EntryKind == "" { // an old idempotency receipt
		j.EntryKind = core.KindFinding
	}
	if n.Upstream != "" {
		at := n.AsOf
		j.From, j.Upstream, j.Source, j.AsOf = n.From, n.Upstream, n.Source, &at
		if withBody {
			j.Text = &n.Text
		}
	}
	if withBody {
		j.Body = &n.Body
		if n.Upstream == "" { // a mirrored note has no provenance here: its origin is From
			j.Provenance = &n.Provenance
		}
		if n.UnverifiedSource != "" && json.Valid([]byte(n.UnverifiedSource)) {
			j.UnverifiedSource = json.RawMessage(n.UnverifiedSource)
		}
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
	CacheReadTokens      int64   `json:"cacheReadTokens"`
	CacheWriteTokens     int64   `json:"cacheWriteTokens"`
	// Labels (only in a report asked with labels=1; data, never permissions) and where each came from.
	Phase          string `json:"phase,omitempty"`
	Activity       string `json:"activity,omitempty"`
	Task           string `json:"task,omitempty"`
	PhaseSource    string `json:"phaseSource,omitempty"`
	ActivitySource string `json:"activitySource,omitempty"`
	TaskSource     string `json:"taskSource,omitempty"`
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
		rows = append(rows, UsageRowJSON{costString(total), r.EstimatedEvents, r.SubscriptionEvents, r.UnknownBillingEvents, r.Provider, costString(r.ReportedCost), costString(r.EstimatedCost), r.UnknownCost, r.Day, r.Model, r.UserID, r.UserName, r.ProjectID, r.Events, r.TokensIn, r.TokensOut, r.UnknownTokens, r.Nested, r.Failed, r.ErrorEvents, r.CancelledEvents, r.InterruptedEvents, r.DurationMs, r.FailedDurationMs, r.UnknownDuration, r.CacheReadTokens, r.CacheWriteTokens, r.Phase, r.Activity, r.Task, r.PhaseSource, r.ActivitySource, r.TaskSource})
	}
	for _, g := range rep.Gaps {
		gaps = append(gaps, UsageGapJSON{g.Count, g.From, g.To})
	}
	out := map[string]any{"days": rows, "gaps": gaps, "timezone": "UTC"}
	if rep.Truncated { // a report split by label returned only the newest rows
		out["truncated"] = true
	}
	return out
}

// member serves the member routes. Failures of the device or the principal are the 403 no_project_access; when the
// device itself is the reason, principalRefusal names the condition. The gateway's own device refusals and the
// exceptions of the model routes do not apply.
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
		return principalRefusal(c.DeviceID, err)
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

// principalRefusal: the member routes' 403 no_project_access. When the device itself is the reason it names the
// missing condition and the owner's command, with the caller's own device id (it already knows it). Any other
// reason (no device, an agent peer, an inactive user) keeps the fixed body.
func principalRefusal(device string, err error) Response {
	switch {
	case errors.Is(err, core.ErrDeviceNotListed):
		return jsonErr(403, "no_project_access", fmt.Sprintf("no project access: this device is not on the gateway's device list (the owner runs: minigw gw devices allow %s)", device))
	case errors.Is(err, core.ErrDeviceNotBound):
		return jsonErr(403, "no_project_access", fmt.Sprintf("no project access: this device is not bound to a user (the owner runs: minigw gw bind %s USER)", device))
	}
	return noAccess()
}

type noteBody struct {
	Class    core.Class `json:"class"`
	Title    string     `json:"title"`
	Body     string     `json:"body"`
	Revision *int64     `json:"revision"`
	// EntryKind is accepted and ignored: the kind is set by a reviewed submission or an import, never by a push or an edit.
	EntryKind string `json:"entryKind"`
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
		var ps []core.Project
		var err error
		if prefix := q.Get("path"); prefix != "" {
			ps, err = st.ListProjectsByPath(ctx, p, prefix, q.Get("archived") == "1")
		} else {
			ps, err = st.ListProjects(ctx, p, q.Get("archived") == "1")
		}
		if err != nil {
			return a.errResponse(err)
		}
		list := []ProjectJSON{}
		for _, x := range ps {
			list = append(list, toProject(x))
		}
		return resp(200, list)
	case len(seg) == 1 && seg[0] == "by-remote" && r.Method == "POST":
		var b struct {
			Remote string `json:"remote"`
		}
		if err := payload(r, &b); err != nil {
			return a.errResponse(err)
		}
		identity, ps, err := st.ProjectsByRemote(ctx, p, b.Remote)
		if err != nil {
			return a.errResponse(err)
		}
		return resp(200, map[string]any{"identity": identity, "projects": teamProjects(ps)})
	case len(seg) == 2 && seg[1] == "depends" && r.Method == "GET":
		pid, ok := posID(seg[0])
		if !ok {
			return a.errResponse(core.ErrInvalid{What: "project id"})
		}
		ds, err := st.ListDepends(ctx, p, pid)
		if err != nil {
			return a.errResponse(err)
		}
		ts, err := st.ListTeamDepends(ctx, p, pid)
		if err != nil {
			return a.errResponse(err)
		}
		return resp(200, map[string]any{"depends": dependsJSON(ds), "teams": teamDependsJSON(ts)})
	case len(seg) == 2 && seg[1] == "remotes" && (r.Method == "GET" || r.Method == "POST"):
		pid, ok := posID(seg[0])
		if !ok {
			return a.errResponse(core.ErrInvalid{What: "project id"})
		}
		var list []string
		var b remotesBody
		var err error
		if r.Method == "GET" {
			list, err = st.ProjectRemotes(ctx, p, pid)
		} else {
			if err = payload(r, &b); err == nil {
				list, err = st.ChangeProjectRemotes(ctx, p, pid, b.Add, b.Remove)
			}
		}
		if err != nil {
			return a.errResponse(err)
		}
		return resp(200, remotesReply(list, b))
	case len(seg) == 3 && seg[1] == "kb" && seg[2] == "faq" && r.Method == "GET":
		pid, ok := posID(seg[0])
		if !ok {
			return a.errResponse(core.ErrInvalid{What: "project id"})
		}
		pg, err := st.ListFAQ(ctx, p, pid, q.Get("after"))
		if err != nil {
			return a.errResponse(err)
		}
		return resp(200, faqJSON(pg))
	case len(seg) == 1 && r.Method == "GET":
		pr, err := st.GetProject(ctx, p, seg[0])
		if err != nil {
			return a.errResponse(err)
		}
		return resp(200, toProject(pr))
	case len(seg) == 2 && seg[1] == "sessions" && r.Method == "GET":
		return a.sessionRoute(ctx, st, p, seg[0], q, false)
	case len(seg) == 2 && seg[1] == "link-check" && r.Method == "POST":
		pr, err := st.ResolveForLink(ctx, p, seg[0])
		if err != nil {
			return a.errResponse(err)
		}
		return resp(200, toProject(pr))
	case len(seg) == 3 && seg[1] == "kb" && seg[2] == "search" && r.Method == "GET":
		pid, ok := posID(seg[0])
		if !ok {
			return a.errResponse(core.ErrInvalid{What: "project id"})
		}
		v, e := st.SearchNotes(ctx, p, pid, q.Get("q"))
		if e != nil {
			return a.errResponse(e)
		}
		return resp(200, v)
	case len(seg) == 3 && seg[1] == "kb" && seg[2] == "manifest" && r.Method == "GET":
		pid, ok := posID(seg[0])
		if !ok {
			return a.errResponse(core.ErrInvalid{What: "project id"})
		}
		m, e := st.Manifest(ctx, p, pid)
		if e != nil {
			return a.errResponse(e)
		}
		return resp(200, m)
	case len(seg) == 3 && seg[1] == "kb" && seg[2] == "import" && r.Method == "POST":
		pid, ok := posID(seg[0])
		if !ok {
			return a.errResponse(core.ErrInvalid{What: "project id"})
		}
		var in core.MarkdownInput
		if err := payload(r, &in); err != nil {
			return a.errResponse(err)
		}
		result, err := st.ImportMarkdown(ctx, p, pid, in)
		if err != nil {
			return a.errResponse(err)
		}
		return resp(200, result)
	case len(seg) >= 3 && seg[1] == "kb" && seg[2] == "review" && len(seg) <= 5:
		pid, ok := posID(seg[0]) // 0: every project the caller reviews (list), the submission's own project (one submission)
		if seg[0] == "0" {
			pid, ok = 0, true
		}
		if !ok {
			return a.errResponse(core.ErrInvalid{What: "project id"})
		}
		return a.reviewRoute(ctx, st, p, pid, seg[3:], q, r)
	case len(seg) >= 3 && seg[1] == "kb" && seg[2] == "submissions" && len(seg) <= 4:
		pid, ok := posID(seg[0])
		if !ok {
			return a.errResponse(core.ErrInvalid{What: "project id"})
		}
		return a.submissionRoute(ctx, st, p, pid, seg[3:], q, r)
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
		if n.Upstream == "" { // a mirrored note does not live here: no origin of this gateway
			n.Origin = st.NoteOrigin(n.ProjectID) // the export writes it into the file
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
	rep, err := st.Usage(ctx, p, project, days, labelQuery(q))
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
	if c.Run == nil || r.Method != "GET" || !(r.Path == "/minigw/v1/kb/search" || r.Path == notesPath || strings.HasPrefix(r.Path, notesPath+"/")) {
		return a.errResponse(core.ErrNotFound)
	}
	cap := *c.Run
	if r.Path == "/minigw/v1/kb/search" {
		q, _ := url.ParseQuery(r.RawQuery)
		v, e := st.RunSearchNotes(ctx, cap, c.Approved, q.Get("q"))
		if e != nil {
			return a.errResponse(e)
		}
		return resp(200, v)
	}
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

// remotesBody is the body of POST .../remotes: raw URLs to add and to remove.
type remotesBody struct {
	Add    []string `json:"add"`
	Remove []string `json:"remove"`
}

type faqEntryJSON struct {
	ID       int64      `json:"id"`
	Title    string     `json:"title"`
	Class    core.Class `json:"class"`
	Answer   string     `json:"answer"`
	Updated  time.Time  `json:"updated"`
	From     string     `json:"from,omitempty"`
	Upstream string     `json:"upstream,omitempty"`
	Source   string     `json:"source,omitempty"`
	Text     string     `json:"text,omitempty"`
	AsOf     *time.Time `json:"asOf,omitempty"`
}

func faqJSON(pg core.FAQPage) map[string]any {
	out := []faqEntryJSON{}
	for _, e := range pg.Entries {
		out = append(out, faqEntryJSON{e.ID, e.Title, e.Class, e.Answer, e.Updated, e.From, e.Upstream, e.Source, e.Text, e.AsOf})
	}
	return map[string]any{"entries": out, "next": pg.Next}
}

// remotesReply: the list, and for a change the identities it named (the client prints them).
func remotesReply(list []string, b remotesBody) map[string]any {
	out := map[string]any{"remotes": list}
	for key, raw := range map[string][]string{"added": b.Add, "removed": b.Remove} {
		var ids []string
		for _, r := range raw {
			if id, err := core.NormalizeRemote(r); err == nil { // the core accepted them
				ids = append(ids, id)
			} else if key == "removed" { // an identity exactly as listed
				ids = append(ids, r)
			}
		}
		if len(ids) > 0 {
			out[key] = ids
		}
	}
	return out
}

// teamDependsJSON lists the teams of a dependency view as {id, name, archived}.
func teamDependsJSON(ts []core.Team) []map[string]any {
	out := make([]map[string]any, 0, len(ts))
	for _, t := range ts {
		out = append(out, map[string]any{"id": t.ID, "name": t.Name, "archived": t.Archived})
	}
	return out
}

// dependsJSON lists projects of a dependency view as {id, slug, name}.
func dependsJSON(ps []core.Project) []map[string]any {
	out := []map[string]any{}
	for _, p := range ps {
		out = append(out, map[string]any{"id": p.ID, "slug": p.Slug, "name": p.Name})
	}
	return out
}

// labelQuery reads the label parameters of the usage routes: phase, activity, task, label_source (filters: they only narrow) and labels=1 (rows split by label).
func labelQuery(q url.Values) core.LabelQuery {
	return core.LabelQuery{Phase: q.Get("phase"), Activity: q.Get("activity"), Task: q.Get("task"), Labels: q.Get("labels") == "1", LabelSource: q.Get("label_source")}
}
