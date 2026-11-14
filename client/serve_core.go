package main

// The 0.6.0 core on the gateway (docs/plans/contracts-0.6.0.md): opening the store, the member routes under
// /minigw/v1/projects and the error mapping. The policy itself is in package core.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"minigw/core"
)

const (
	projectsPrefix = "/minigw/v1/projects"
	coreDirName    = "core"
	maxProjectBody = 512 << 10 // 64 KiB of body can grow to 384 KiB of \u escapes; refused before parsing
)

// openCore opens <config dir>/core. A gateway that cannot open it still starts: project routes answer 503, the
// models work. Not called for the offline server (minigw gw without a running gateway).
func (s *server) openCore() {
	st, err := core.Open(filepath.Join(s.cfgDir, coreDirName), core.Options{ProtectDir: protectDir})
	if err == nil {
		err = ensureOwnerUser(st)
		if err != nil {
			st.Close()
		}
	}
	if err != nil {
		s.coreErr = err
		s.log.Printf("WARNING: the core database cannot be used, project routes answer 503 core_unavailable (models work): %v", err)
		s.startUsage() // usage events go to the spool until a start with a usable core
		return
	}
	s.core = st
	s.startUsage()
}

func (s *server) closeCore() {
	s.stopUsage() // drain the usage queue while the core is still open
	if s.core != nil {
		s.core.Close()
	}
}

// ensureOwnerUser creates the owner's user row once (named after the OS account); the mark gives no power, it only
// lets the owner be a project member.
func ensureOwnerUser(st *core.Store) error {
	users, err := st.ListUsers(context.Background())
	if err != nil || len(users) > 0 {
		return err
	}
	name := "owner"
	if u, err := user.Current(); err == nil && u.Username != "" {
		name = u.Username[strings.LastIndexAny(u.Username, `\`)+1:]
	}
	u, err := st.CreateUser(context.Background(), name)
	if err != nil {
		return err
	}
	return st.SetUserOwner(context.Background(), u.ID, true)
}

// --- JSON shapes (identical on both gateways) ---

type projectJSON struct {
	ID       int64        `json:"id"`
	Slug     string       `json:"slug"`
	Name     string       `json:"name"`
	Archived bool         `json:"archived"`
	Role     core.Role    `json:"role,omitempty"`
	Granted  []core.Class `json:"granted"`
}

func toProject(p core.Project) projectJSON {
	if p.Granted == nil {
		p.Granted = []core.Class{}
	}
	return projectJSON{p.ID, p.Slug, p.Name, p.Archived, p.Role, p.Granted}
}

type noteJSON struct {
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

func toNote(n core.Note, withBody bool) noteJSON {
	j := noteJSON{n.ID, n.ProjectID, n.Class, n.Title, nil, n.Author, n.Created, n.Updated, n.Revision}
	if withBody {
		j.Body = &n.Body
	}
	return j
}

// --- errors: one place maps core errors to status and the envelope {"error":{"type","message"}} ---

func coreFail(w http.ResponseWriter, rid string, status int, typ, msg string, extra map[string]any) int {
	e := map[string]any{"type": typ, "message": msg}
	for k, v := range extra {
		e[k] = v
	}
	return writeJSON(w, rid, status, map[string]any{"error": e})
}

// coreError: an unknown error is a 500 with a fixed message; the detail goes only to the log.
func (s *server) coreError(w http.ResponseWriter, r *req, err error) int {
	var rev core.ErrRevision
	var lim core.ErrLimit
	var inv core.ErrInvalid
	switch {
	case errors.Is(err, core.ErrNoProjectAccess):
		return coreFail(w, r.rid, 403, "no_project_access", "No project access", nil)
	case errors.Is(err, core.ErrNotFound):
		return coreFail(w, r.rid, 404, "not_found", "Not found", nil)
	case errors.Is(err, core.ErrForbidden):
		return coreFail(w, r.rid, 403, "forbidden", "Not allowed", nil)
	case errors.Is(err, core.ErrArchived):
		return coreFail(w, r.rid, 403, "archived", "The project is archived", nil)
	case errors.As(err, &rev):
		return coreFail(w, r.rid, 409, "revision_mismatch", "The note has changed", map[string]any{"revision": rev.Current})
	case errors.Is(err, core.ErrIdempotency):
		return coreFail(w, r.rid, 409, "idempotency_conflict", "The Idempotency-Key was used with another payload", nil)
	case errors.Is(err, core.ErrTooLarge):
		return coreFail(w, r.rid, 413, "too_large", "Too large", nil)
	case errors.As(err, &lim):
		w.Header().Set("Retry-After", strconv.Itoa(max(1, int((lim.RetryAfter+time.Second-1)/time.Second))))
		return coreFail(w, r.rid, 429, "limit", "Too many requests", nil)
	case errors.As(err, &inv):
		return coreFail(w, r.rid, 400, "invalid", inv.Error(), nil)
	}
	s.log.Printf("%s core error: %v", r.rid, err)
	return coreFail(w, r.rid, 500, "internal", "Internal error", nil)
}

// projectHTTP serves /minigw/v1/projects…. Failures of the token, the device or the principal all answer the same
// 403 no_project_access; the gateway's own device refusals and the loopback and allow-any-device exceptions of
// the model routes do not apply here.
func (s *server) projectHTTP(w http.ResponseWriter, r *req) int {
	deny := func() int { return s.coreError(w, r, core.ErrNoProjectAccess) }
	if runOf(r) != nil || !s.authorized(r) {
		return deny()
	}
	if s.core == nil {
		return coreFail(w, r.rid, 503, "core_unavailable", "The project database is not available", nil)
	}
	dev := r.conn.device // proven in this Noise tunnel (readDevice); nil for plain loopback and plain remote
	if dev == nil {
		return deny()
	}
	// approved NOW: in the device list itself, not deviceAllowed (its loopback and allow-any-device rules).
	p, err := s.core.Principal(r.Context(), dev.ID, s.conf().listed(dev.ID))
	if err != nil {
		if !errors.Is(err, core.ErrNoProjectAccess) {
			return s.coreError(w, r, err)
		}
		s.log.Printf("%s project route refused: %v", r.rid, err) // the reason stays in the log
		return deny()
	}
	if s.coreHook != nil {
		s.coreHook(r, "admitted")
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxProjectBody)
	return s.projectRoute(w, r, p)
}

func (s *server) projectRoute(w http.ResponseWriter, r *req, p core.Principal) int {
	if r.URL.Path == usagePath {
		return s.usageRoute(w, r, p)
	}
	seg := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, projectsPrefix), "/"), "/")
	if seg[0] == "" {
		seg = nil
	}
	ctx := r.Context()
	q := r.URL.Query()
	notFound := func() int { return s.coreError(w, r, core.ErrNotFound) }
	bad := func(what string) int { return s.coreError(w, r, core.ErrInvalid{What: what}) }
	id := func(v string) (int64, bool) { n, err := strconv.ParseInt(v, 10, 64); return n, err == nil && n > 0 }
	body := func(v any) int { // 0 = parsed
		data, err := io.ReadAll(r.Body)
		if err != nil {
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				return s.coreError(w, r, core.ErrTooLarge)
			}
			return bad("body")
		}
		if err := decodeStrict(data, v); err != nil {
			return bad("JSON body")
		}
		return 0
	}
	out := func(status int, v any) int { return writeJSON(w, r.rid, status, v) }

	switch {
	case len(seg) == 0 && r.Method == "GET":
		ps, err := s.core.ListProjects(ctx, p, q.Get("archived") == "1")
		if err != nil {
			return s.coreError(w, r, err)
		}
		list := []projectJSON{}
		for _, x := range ps {
			list = append(list, toProject(x))
		}
		return out(200, list)
	case len(seg) == 1 && r.Method == "GET":
		pr, err := s.core.GetProject(ctx, p, seg[0])
		if err != nil {
			return s.coreError(w, r, err)
		}
		return out(200, toProject(pr))
	case len(seg) == 2 && seg[1] == "link-check" && r.Method == "POST":
		pr, err := s.core.ResolveForLink(ctx, p, seg[0])
		if err != nil {
			return s.coreError(w, r, err)
		}
		return out(200, toProject(pr))
	case len(seg) >= 2 && seg[1] == "notes" && len(seg) <= 3:
		pid, ok := id(seg[0])
		if !ok {
			return bad("project id")
		}
		return s.noteRoute(w, r, p, pid, seg[2:], q, body, out, bad, notFound, id)
	}
	return notFound()
}

type noteBody struct {
	Class    core.Class `json:"class"`
	Title    string     `json:"title"`
	Body     string     `json:"body"`
	Revision *int64     `json:"revision"`
}

func (s *server) noteRoute(w http.ResponseWriter, r *req, p core.Principal, pid int64, rest []string, q url.Values,
	body func(any) int, out func(int, any) int, bad func(string) int, notFound func() int, id func(string) (int64, bool)) int {
	ctx := r.Context()
	get := q.Get
	if len(rest) == 0 {
		switch r.Method {
		case "GET":
			limit := 0
			if v := get("limit"); v != "" {
				n, err := strconv.Atoi(v)
				if err != nil {
					return bad("limit")
				}
				limit = n
			}
			pg, err := s.core.ListNotes(ctx, p, pid, core.Class(get("class")), get("after"), limit)
			if err != nil {
				return s.coreError(w, r, err)
			}
			notes := []noteJSON{}
			for _, n := range pg.Notes {
				notes = append(notes, toNote(n, false))
			}
			res := map[string]any{"notes": notes}
			if pg.Next != "" {
				res["next"] = pg.Next
			}
			return out(200, res)
		case "POST":
			var b noteBody
			if st := body(&b); st != 0 {
				return st
			}
			n, replay, err := s.core.CreateNote(ctx, p, pid, r.h("Idempotency-Key"), core.NoteInput{Class: b.Class, Title: b.Title, Body: b.Body})
			if err != nil {
				return s.coreError(w, r, err)
			}
			if replay {
				w.Header().Set("Idempotent-Replay", "true")
			}
			return out(201, toNote(n, true))
		}
		return notFound()
	}
	nid, ok := id(rest[0])
	if !ok {
		return bad("note id")
	}
	switch r.Method {
	case "GET":
		n, err := s.core.GetNote(ctx, p, pid, nid)
		if err != nil {
			return s.coreError(w, r, err)
		}
		return out(200, toNote(n, true))
	case "PUT":
		var b noteBody
		if st := body(&b); st != 0 {
			return st
		}
		if b.Revision == nil {
			return bad("revision")
		}
		n, err := s.core.UpdateNote(ctx, p, pid, nid, *b.Revision, core.NoteInput{Class: b.Class, Title: b.Title, Body: b.Body})
		if err != nil {
			return s.coreError(w, r, err)
		}
		return out(200, toNote(n, true))
	case "DELETE":
		rev, err := strconv.ParseInt(get("revision"), 10, 64)
		if err != nil {
			return bad("revision")
		}
		if err := s.core.DeleteNote(ctx, p, pid, nid, rev); err != nil {
			return s.coreError(w, r, err)
		}
		w.WriteHeader(204)
		return 204
	}
	return notFound()
}
