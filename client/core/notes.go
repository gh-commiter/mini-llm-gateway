package core

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// Note is a note; Body is empty in lists.
type Note struct {
	Provenance       Provenance
	UnverifiedSource string `json:"-"` // opaque history the bundle supplied; never trusted
	ID               int64
	ProjectID        int64
	Class            Class
	Title            string
	Body             string `json:",omitempty"`
	AuthorID         int64
	Author           string
	// Editor names the user who wrote the current revision when that is not the author ("" otherwise); display only, never stored.
	Editor    string `json:"-"`
	Created   time.Time
	Updated   time.Time
	Revision  int64
	EntryKind string // finding or faq; "" (an old receipt) reads as finding
	// Team names the team of a note that lives in a team's own notes; ProjectID is then a hidden id no output should show.
	Team string `json:",omitempty"`
	// Origin names the gateway and project the note lives in ("<gateway id>:<project id>"): the export writes it into the front
	// matter so that a re-import into the same project knows its own file. Set by the API when a note is read, never stored.
	Origin string `json:",omitempty"`
	// Upstream, AsOf and From: a note read from the mirror of another gateway (its id is mirrorBase and up; it is never stored here).
	Upstream string    `json:"-"`
	AsOf     time.Time `json:"-"`
	From     string    `json:"-"`
	Source   string    `json:"-"` // the source as the upstream named it
	Text     string    `json:"-"` // the sanitised text itself (Body is then the framed, quoted rendering)
}

// NoteInput is the payload of create and update.
type NoteInput struct {
	Class Class
	Title string
	Body  string
	// EntryKind "" = unspecified; omitted from JSON so the idempotency hash of a request without it is unchanged.
	EntryKind string `json:",omitempty"`
}

type NotePage struct {
	Notes []Note
	Next  string // opaque cursor; "" = last page
}

// readable: the classes of one project a user may read, all notes or only their own.
type readable struct {
	project  int64
	all, own []Class
}

func (a access) readable(project int64, noPrivate bool) readable {
	r := readable{project: project}
	for _, c := range allClasses {
		if noPrivate && c == Private {
			continue
		}
		if a.can(pRead, c, false) {
			r.all = append(r.all, c)
		} else if a.can(pRead, c, true) {
			r.own = append(r.own, c)
		}
	}
	return r
}

func (r readable) allows(c Class, own bool) bool {
	for _, x := range r.all {
		if x == c {
			return true
		}
	}
	if own {
		for _, x := range r.own {
			if x == c {
				return true
			}
		}
	}
	return false
}

func marks(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }

// where builds the visibility filter for a list of readable sets; the filter runs before paging.
func where(user int64, rs []readable) (string, []any) {
	var parts []string
	var args []any
	for _, r := range rs {
		if len(r.all) > 0 {
			parts = append(parts, "(n.project_id=? AND n.class IN ("+marks(len(r.all))+"))")
			args = append(args, r.project)
			for _, c := range r.all {
				args = append(args, c)
			}
		}
		if len(r.own) > 0 {
			parts = append(parts, "(n.project_id=? AND n.imported=0 AND n.author_id=? AND n.class IN ("+marks(len(r.own))+"))")
			args = append(args, r.project, user)
			for _, c := range r.own {
				args = append(args, c)
			}
		}
	}
	if len(parts) == 0 {
		return "0", nil
	}
	return "(" + strings.Join(parts, " OR ") + ")", args
}

// Bundle authors are display-only; zero grants no ownership permissions.
const noteCols = `n.id,n.project_id,n.class,n.title,n.body,CASE WHEN n.imported=0 THEN n.author_id ELSE 0 END,u.name,n.created,n.updated,n.revision,n.entry_kind,COALESCE((SELECT eu.name FROM note_revisions r JOIN users eu ON eu.id=json_extract(r.provenance,'$.user') WHERE r.note_id=n.id AND r.revision=n.revision AND n.imported=0 AND eu.id<>n.author_id),'')`

// listCols is noteCols without the body (lists never load it).
const listCols = `n.id,n.project_id,n.class,n.title,'',CASE WHEN n.imported=0 THEN n.author_id ELSE 0 END,u.name,n.created,n.updated,n.revision,n.entry_kind,COALESCE((SELECT eu.name FROM note_revisions r JOIN users eu ON eu.id=json_extract(r.provenance,'$.user') WHERE r.note_id=n.id AND r.revision=n.revision AND n.imported=0 AND eu.id<>n.author_id),'')`

func scanNote(sc interface{ Scan(...any) error }, withBody bool) (n Note, err error) {
	var c, u int64
	if err = sc.Scan(&n.ID, &n.ProjectID, &n.Class, &n.Title, &n.Body, &n.AuthorID, &n.Author, &c, &u, &n.Revision, &n.EntryKind, &n.Editor); err != nil {
		return
	}
	n.Created, n.Updated = fromMs(c), fromMs(u)
	if !withBody {
		n.Body = ""
	}
	return
}

type cursor struct {
	U int64 `json:"u"`
	P int64 `json:"p"`
	C Class `json:"c,omitempty"`
	T int64 `json:"t"`
	I int64 `json:"i"`
}

func encodeCursor(c cursor) string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

// decodeCursor refuses a cursor that was not made for this user, project and class filter.
func decodeCursor(s string, user, project int64, class Class) (c cursor, err error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err == nil {
		err = json.Unmarshal(b, &c)
	}
	if err != nil || c.U != user || c.P != project || c.C != class {
		return c, ErrInvalid{"cursor"}
	}
	return c, nil
}

// listNotes pages through the notes in rs: updated descending, then id; limit+1 rows tell whether there is a next page.
func listNotes(tx *sql.Tx, user, project int64, rs []readable, class Class, kind, after string, limit int) (NotePage, error) {
	q, args := where(user, rs)
	if kind != "" {
		q += " AND n.entry_kind=?"
		args = append(args, kind)
	}
	if class != "" {
		q += " AND n.class=?"
		args = append(args, class)
	}
	if after != "" {
		c, err := decodeCursor(after, user, project, class)
		if err != nil {
			return NotePage{}, err
		}
		q += " AND (n.updated<? OR (n.updated=? AND n.id>?))"
		args = append(args, c.T, c.T, c.I)
	}
	rows, err := tx.Query(`SELECT `+listCols+` FROM notes n JOIN users u ON u.id=n.author_id WHERE `+q+` ORDER BY n.updated DESC, n.id LIMIT ?`, append(args, limit+1)...)
	if err != nil {
		return NotePage{}, err
	}
	defer rows.Close()
	var pg NotePage
	for rows.Next() {
		n, err := scanNote(rows, false)
		if err != nil {
			return pg, err
		}
		pg.Notes = append(pg.Notes, n)
	}
	if err := rows.Err(); err != nil {
		return pg, err
	}
	rows.Close()
	names := map[int64]string{}
	for i := range pg.Notes {
		if err := fillTeam(tx, &pg.Notes[i], names); err != nil {
			return pg, err
		}
	}
	if len(pg.Notes) > limit {
		pg.Notes = pg.Notes[:limit]
		last := pg.Notes[limit-1]
		pg.Next = encodeCursor(cursor{U: user, P: project, C: class, T: ms(last.Updated), I: last.ID})
	}
	return pg, nil
}

func checkListArgs(class Class, limit int) (int, error) {
	if class != "" && !validClass(class) {
		return 0, ErrInvalid{"class"}
	}
	if limit == 0 {
		return 50, nil
	}
	if limit < 0 || limit > 100 {
		return 0, ErrInvalid{"limit"}
	}
	return limit, nil
}

func getNote(tx *sql.Tx, id int64) (Note, error) {
	if isMirrorID(id) { // a mirrored note is never a local one: every route by note id answers it as unknown
		return Note{}, ErrNotFound
	}
	n, err := scanNote(tx.QueryRow(`SELECT `+noteCols+` FROM notes n JOIN users u ON u.id=n.author_id WHERE n.id=?`, id), true)
	if err == nil {
		err = noteProvenance(tx, &n)
	}
	if err == nil {
		err = tx.QueryRow(`SELECT source_meta FROM notes WHERE id=?`, id).Scan(&n.UnverifiedSource)
	}
	if err == nil {
		err = fillTeam(tx, &n, nil)
	}
	return n, notFound(err)
}

// fillTeam sets Note.Team for a note of a team's own notes (names caches the lookups of one listing; nil = none).
func fillTeam(tx *sql.Tx, n *Note, names map[int64]string) error {
	if !isSegment(n.ProjectID) {
		return nil
	}
	if name, ok := names[n.ProjectID]; ok {
		n.Team = name
		return nil
	}
	if err := tx.QueryRow(`SELECT name FROM teams WHERE id=?`, n.ProjectID-segmentBase).Scan(&n.Team); err != nil {
		return err
	}
	if names != nil {
		names[n.ProjectID] = n.Team
	}
	return nil
}

// noteMetadata never selects the body.
func noteMetadata(tx *sql.Tx, id int64) (Note, error) {
	if isMirrorID(id) {
		return Note{}, ErrNotFound
	}
	n, err := scanNote(tx.QueryRow(`SELECT `+listCols+` FROM notes n JOIN users u ON u.id=n.author_id WHERE n.id=?`, id), false)
	return n, notFound(err)
}

// visibleNote checks scope before any body query.
func visibleNote(tx *sql.Tx, user, id int64, rs []readable) (Note, error) {
	n, err := noteMetadata(tx, id)
	if err != nil {
		return Note{}, err
	}
	for _, r := range rs {
		if r.project == n.ProjectID && r.allows(n.Class, n.AuthorID == user) {
			return n, nil
		}
	}
	return Note{}, ErrNotFound
}

// memberAccess is steps 3 and 4 of section 1 for a project: principal still valid, project visible (else 404).
func memberAccess(tx *sql.Tx, p Principal, project int64) (access, error) {
	if err := checkPrincipal(tx, p); err != nil {
		return access{}, err
	}
	a, err := loadAccess(tx, p.UserID, project)
	if err == nil && !a.visible() {
		err = ErrNotFound
	}
	return a, err
}

// ListNotes lists the notes of a project the caller may read (class filter optional, "" = all).
func (s *Store) ListNotes(ctx context.Context, p Principal, project int64, class Class, after string, limit int) (pg NotePage, err error) {
	lim := s.rate(p.UserID, false)
	if limit, err = checkListArgs(class, limit); err != nil {
		return
	}
	err = s.read(ctx, func(tx *sql.Tx) error {
		a, err := memberAccess(tx, p, project)
		if err != nil {
			return err
		}
		if lim != nil {
			return lim
		}
		pg, err = listNotes(tx, p.UserID, project, []readable{a.readable(project, false)}, class, "", after, limit)
		return err
	})
	return
}

func (s *Store) GetNote(ctx context.Context, p Principal, project, id int64) (n Note, err error) {
	lim := s.rate(p.UserID, false)
	err = s.read(ctx, func(tx *sql.Tx) error {
		a, err := memberAccess(tx, p, project)
		if err != nil {
			return err
		}
		rs, err := noteSources(tx, p.UserID, project, a)
		if err != nil {
			return err
		}
		if isMirrorID(id) {
			if n, err = readMirrorNote(ctx, tx, s.now(), p.UserID, project, rs, id); err == nil && lim != nil {
				return lim
			}
			return err
		}
		if _, err = visibleNote(tx, p.UserID, id, rs); err != nil {
			return err
		}
		if lim != nil {
			return lim
		}
		n, err = getNote(tx, id)
		return err
	})
	if err != nil {
		return Note{}, err
	}
	return
}

func checkInput(in NoteInput) error {
	switch {
	case !validClass(in.Class):
		return ErrInvalid{"class"}
	case strings.TrimSpace(in.Title) == "":
		return ErrInvalid{"title"}
	case in.EntryKind != "" && !validKind(in.EntryKind):
		return ErrInvalid{"kind"}
	case in.EntryKind == KindFAQ && len(in.Title) > maxFAQTitle:
		return ErrInvalid{"faq question: at most 256 bytes"}
	case in.EntryKind == KindFAQ && len(in.Body) > maxFAQBody:
		return ErrInvalid{"faq answer: at most 2048 bytes"}
	}
	return nil
}

// checkSize is the per-note part of "limits" (413).
func checkSize(in NoteInput) error {
	if len(in.Title) > maxTitle || len(in.Body) > maxBody {
		return ErrTooLarge
	}
	return nil
}

// checkProjectLimits: count and bytes of the project after the change (delta bytes, +1 note when adding).
func checkProjectLimits(tx *sql.Tx, project int64, addNote bool, deltaBytes int64) error {
	var n, b int64
	if err := tx.QueryRow(`SELECT count(*),coalesce(sum(bytes),0) FROM notes WHERE project_id=?`, project).Scan(&n, &b); err != nil {
		return err
	}
	if addNote {
		n++
	}
	if n > int64(maxNotes) || b+deltaBytes > maxBytes {
		return ErrTooLarge
	}
	return nil
}

// writeAccess authorizes a change of an existing note in the contract's order: visible (404), allowed (403),
// archived (403).
func writeAccess(tx *sql.Tx, p Principal, project, id int64) (a access, n Note, err error) {
	if a, err = memberAccess(tx, p, project); err != nil {
		return
	}
	if n, err = noteMetadata(tx, id); err != nil {
		return
	}
	own := n.AuthorID == p.UserID
	if n.ProjectID != project || !a.can(pRead, n.Class, own) {
		return a, n, ErrNotFound
	}
	if !a.can(pWriteOwn, n.Class, own) {
		return a, n, ErrForbidden
	}
	return a, n, archivedErr(tx, project)
}

func archivedErr(tx *sql.Tx, project int64) error {
	var arch bool
	if err := tx.QueryRow(`SELECT archived FROM projects WHERE id=?`, project).Scan(&arch); err != nil {
		return err
	}
	if arch {
		return ErrArchived
	}
	return nil
}

// CreateNote creates a note; key is the Idempotency-Key. A repeat of the same key and payload within 24 h returns
// the first result (replayed = true) but still needs current access. Deleted or reclassified notes are not replayed;
// another payload is ErrIdempotency.
func (s *Store) CreateNote(ctx context.Context, p Principal, project int64, key string, in NoteInput) (n Note, replayed bool, err error) {
	lim := s.rate(p.UserID, true)
	if key == "" || len(key) > 64 {
		return n, false, ErrInvalid{"idempotency key"}
	}
	if err = checkInput(in); err != nil {
		return
	}
	enc, _ := json.Marshal(in) // unambiguous: no two inputs give the same bytes
	h := sha256.Sum256(enc)
	hash := hex.EncodeToString(h[:])
	err = s.write(ctx, func(tx *sql.Tx) error {
		a, err := memberAccess(tx, p, project)
		if err != nil {
			return err
		}
		if !a.can(pCreate, in.Class, true) {
			return ErrForbidden
		}
		if err := archivedErr(tx, project); err != nil { // also for a replay
			return err
		}
		now := ms(s.now())
		tx.Exec(`DELETE FROM idempotency WHERE time<?`, now-idempotencyTTL.Milliseconds())
		var oldHash string
		var noteID int64
		switch err := tx.QueryRow(`SELECT hash,note_id FROM idempotency WHERE user_id=? AND project_id=? AND key=?`, p.UserID, project, key).Scan(&oldHash, &noteID); err {
		case nil:
			if oldHash != hash {
				return ErrIdempotency
			}
			if lim != nil {
				return lim
			}
			current, err := visibleNote(tx, p.UserID, noteID, []readable{a.readable(project, false)})
			if err != nil {
				return err
			}
			if current.Class != in.Class {
				return ErrNotFound
			}
			var result string
			if err := tx.QueryRow(`SELECT result FROM idempotency WHERE user_id=? AND project_id=? AND key=?`, p.UserID, project, key).Scan(&result); err != nil {
				return err
			}
			if result == "" {
				return ErrNotFound
			}
			replayed = true
			return json.Unmarshal([]byte(result), &n)
		case sql.ErrNoRows:
		default:
			return err
		}
		if lim != nil {
			return lim
		}
		if err := checkSize(in); err != nil {
			return err
		}
		size := int64(len(in.Title) + len(in.Body))
		if err := checkProjectLimits(tx, project, true, size); err != nil {
			return err
		}
		r, err := tx.Exec(`INSERT INTO notes(project_id,class,title,body,bytes,author_id,created,updated) VALUES(?,?,?,?,?,?,?,?)`,
			project, in.Class, in.Title, in.Body, size, p.UserID, now, now)
		if err != nil {
			return err
		}
		id, _ := r.LastInsertId()
		if err = syncSearch(tx, id); err != nil {
			return err
		}
		if err = s.markUpstreamDirty(tx, project); err != nil {
			return err
		}
		if err = saveProvenance(tx, id, 1, writerProvenance(p, project, in, fromMs(now))); err != nil {
			return err
		}
		if n, err = getNote(tx, id); err != nil {
			return err
		}
		res, _ := json.Marshal(n)
		if _, err := tx.Exec(`INSERT INTO idempotency(user_id,project_id,key,hash,note_id,result,time) VALUES(?,?,?,?,?,?,?)`, p.UserID, project, key, hash, id, string(res), now); err != nil {
			return err
		}
		return Audit(tx, actor(p), "note.create", strconv.FormatInt(id, 10), map[string]any{"project": project, "class": in.Class})
	})
	if err != nil {
		return Note{}, false, err
	}
	return
}

// UpdateNote replaces class, title and body of a note when revision is the current one. A class change is
// checked against the transition table and always bumps the epoch (also private -> shared: the author may be only a reader by now).
func (s *Store) UpdateNote(ctx context.Context, p Principal, project, id, revision int64, in NoteInput) (n Note, err error) {
	lim := s.rate(p.UserID, true)
	err = s.write(ctx, func(tx *sql.Tx) error {
		a, old, err := writeAccess(tx, p, project, id) // visible first: an invisible note is 404 whatever the payload
		if err != nil {
			return err
		}
		in.EntryKind = old.EntryKind // an update never changes the kind, a faq keeps its limits
		if err := checkInput(in); err != nil {
			return err
		}
		own := old.AuthorID == p.UserID
		if !a.canMove(old.Class, in.Class, own) {
			return ErrForbidden
		}
		if revision != old.Revision {
			return ErrRevision{Current: old.Revision}
		}
		if lim != nil {
			return lim
		}
		if err := checkSize(in); err != nil {
			return err
		}
		size := int64(len(in.Title) + len(in.Body))
		var oldSize int64
		if err := tx.QueryRow(`SELECT bytes FROM notes WHERE id=?`, id).Scan(&oldSize); err != nil {
			return err
		}
		if err := checkProjectLimits(tx, project, false, size-oldSize); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE notes SET class=?,title=?,body=?,bytes=?,updated=?,revision=revision+1 WHERE id=?`, in.Class, in.Title, in.Body, size, ms(s.now()), id); err != nil {
			return err
		}
		if err := s.markUpstreamDirty(tx, project); err != nil {
			return err
		}
		if err := Audit(tx, actor(p), "note.update", strconv.FormatInt(id, 10), map[string]any{"project": project, "class": in.Class, "from": old.Class}); err != nil {
			return err
		}
		if in.Class != old.Class {
			// Keep the key reserved, but discard content even if the class is later changed back.
			if _, err := tx.Exec(`UPDATE idempotency SET result='' WHERE note_id=?`, id); err != nil {
				return err
			}
			if _, err := s.BumpEpochTx(tx, "note class narrowed"); err != nil {
				return err
			}
		}
		if err = syncSearch(tx, id); err != nil {
			return err
		}
		if err = saveProvenance(tx, id, old.Revision+1, writerProvenance(p, project, in, fromMs(ms(s.now())))); err != nil {
			return err
		}
		n, err = getNote(tx, id)
		return err
	})
	if err != nil {
		return Note{}, err
	}
	return
}

// DeleteNote removes a note when revision is current; the audit event keeps id, class and author.
func (s *Store) DeleteNote(ctx context.Context, p Principal, project, id, revision int64) error {
	lim := s.rate(p.UserID, true)
	return s.write(ctx, func(tx *sql.Tx) error {
		_, n, err := writeAccess(tx, p, project, id)
		if err != nil {
			return err
		}
		if revision != n.Revision {
			return ErrRevision{Current: n.Revision}
		}
		if lim != nil {
			return lim
		}
		if _, err := tx.Exec(`DELETE FROM note_imports WHERE note_id=?`, id); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM notes WHERE id=?`, id); err != nil {
			return err
		}
		if err := syncSearch(tx, id); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE idempotency SET result='' WHERE note_id=?`, id); err != nil {
			return err
		}
		return Audit(tx, actor(p), "note.delete", strconv.FormatInt(id, 10), map[string]any{"project": project, "class": n.Class, "author": n.AuthorID})
	})
}

// ListProjects returns the projects the caller can see with role and granted classes; archived ones only when asked.
func (s *Store) ListProjects(ctx context.Context, p Principal, includeArchived bool) (out []Project, err error) {
	lim := s.rate(p.UserID, false)
	err = s.read(ctx, func(tx *sql.Tx) error {
		if err := checkPrincipal(tx, p); err != nil {
			return err
		}
		if lim != nil {
			return lim
		}
		rows, err := tx.Query(`SELECT id,slug,name,archived FROM projects WHERE (archived=0 OR ?1) AND (
  id IN (SELECT project_id FROM memberships WHERE user_id=?2) OR
  id IN (SELECT g.source_id FROM grants g JOIN memberships m ON m.project_id=g.reader_id JOIN projects rp ON rp.id=g.reader_id AND rp.archived=0 WHERE m.user_id=?2)) ORDER BY slug`, includeArchived, p.UserID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var pr Project
			if err := rows.Scan(&pr.ID, &pr.Slug, &pr.Name, &pr.Archived); err != nil {
				return err
			}
			out = append(out, pr)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		for i := range out {
			a, err := loadAccess(tx, p.UserID, out[i].ID)
			if err != nil {
				return err
			}
			out[i].Role = a.Role
			if a.Role == Admin && !out[i].Archived {
				if err = tx.QueryRow(`SELECT count(*) FROM kb_submissions WHERE state='pending' AND project_id=? AND user_id<>?`, out[i].ID, p.UserID).Scan(&out[i].Pending); err != nil {
					return err
				}
			}
			out[i].Origins, err = membershipOrigins(tx, out[i].ID, p.UserID)
			if err != nil {
				return err
			}
			for _, c := range []Class{Structure, Data} {
				if a.Grants[c] {
					out[i].Granted = append(out[i].Granted, c)
				}
			}
		}
		return nil
	})
	return
}

// GetProject looks a project up by id (digits) or slug; not visible = ErrNotFound.
func (s *Store) GetProject(ctx context.Context, p Principal, idOrSlug string) (pr Project, err error) {
	lim := s.rate(p.UserID, false)
	err = s.read(ctx, func(tx *sql.Tx) error {
		if err := checkPrincipal(tx, p); err != nil {
			return err
		}
		q, arg := `SELECT id,slug,name,archived FROM projects WHERE slug=?`, any(idOrSlug)
		if id, err := strconv.ParseInt(idOrSlug, 10, 64); err == nil {
			q, arg = `SELECT id,slug,name,archived FROM projects WHERE id=?`, id
		}
		if err := tx.QueryRow(q, arg).Scan(&pr.ID, &pr.Slug, &pr.Name, &pr.Archived); err != nil {
			return notFound(err)
		}
		a, err := loadAccess(tx, p.UserID, pr.ID)
		if err != nil {
			return err
		}
		if isSegment(pr.ID) || !a.visible() {
			return ErrNotFound
		}
		if lim != nil {
			return lim
		}
		pr.Role = a.Role
		pr.Origins, err = membershipOrigins(tx, pr.ID, p.UserID)
		if err != nil {
			return err
		}
		for _, c := range []Class{Structure, Data} {
			if a.Grants[c] {
				pr.Granted = append(pr.Granted, c)
			}
		}
		return nil
	})
	if err != nil {
		return Project{}, err
	}
	return
}

// ResolveForLink is GetProject for `project link`: a link is accepted only for a project the user is a direct
// member of (a grant alone lets the user read, not work in the project): ErrForbidden for a grant-only project.
func (s *Store) ResolveForLink(ctx context.Context, p Principal, idOrSlug string) (Project, error) {
	pr, err := s.GetProject(ctx, p, idOrSlug)
	if err == nil && pr.Role == "" {
		return Project{}, ErrForbidden
	}
	return pr, err
}

// RunCap is the notes capability of one agent run (section 4). The gateway keeps it; the core checks it.
type RunCap struct {
	RunID     string `json:"runId"`
	UserID    int64  `json:"userId"`
	DeviceID  string `json:"deviceId"`
	ProjectID int64  `json:"projectId"`
	Epoch     int64  `json:"epoch"`
}

// runScope authorizes a capability read: epoch unchanged, principal valid now, user a member of the run's
// project. The readable sets are the run project's own structure and data notes (never private) and the notes
// reachable through grants whose reader is the run's project; nothing from the user's other memberships. With
// extended (search and read by id, not the list) the run's project team segment is added when the run's user is
// on that team's roster.
func (s *Store) runScope(tx *sql.Tx, c RunCap, approved, extended bool) ([]readable, error) {
	if !approved || isSegment(c.ProjectID) {
		return nil, noAccess("device not approved")
	}
	if c.Epoch != s.epoch.Load() {
		return nil, noAccess("epoch changed")
	}
	if err := checkPrincipal(tx, Principal{UserID: c.UserID, DeviceID: c.DeviceID}); err != nil {
		return nil, err
	}
	a, err := loadAccess(tx, c.UserID, c.ProjectID)
	if err != nil {
		return nil, err
	}
	if a.Role == "" {
		return nil, noAccess("not a member of the run project")
	}
	rs := []readable{a.readable(c.ProjectID, true)}
	rows, err := tx.Query(`SELECT g.source_id,g.class FROM grants g JOIN projects rp ON rp.id=g.reader_id AND rp.archived=0 WHERE g.reader_id=? ORDER BY g.source_id`, c.ProjectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var src int64
		var cl Class
		if err := rows.Scan(&src, &cl); err != nil {
			return nil, err
		}
		if last := len(rs) - 1; last > 0 && rs[last].project == src {
			rs[last].all = append(rs[last].all, cl)
		} else {
			rs = append(rs, readable{project: src, all: []Class{cl}})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	if extended { // search and read also cover the team segment, by the user's own right in it
		more, err := extraSources(tx, c.UserID, c.ProjectID, true)
		if err != nil {
			return nil, err
		}
		rs = append(rs, more...)
	}
	return rs, nil
}

// RunListNotes lists what the run may read, run project and granted sources together.
func (s *Store) RunListNotes(ctx context.Context, c RunCap, approved bool, class Class, after string, limit int) (pg NotePage, err error) {
	lim := s.rate(c.UserID, false)
	if limit, err = checkListArgs(class, limit); err != nil {
		return
	}
	err = s.read(ctx, func(tx *sql.Tx) error {
		rs, err := s.runScope(tx, c, approved, false)
		if err != nil {
			return err
		}
		if lim != nil {
			return lim
		}
		pg, err = listNotes(tx, c.UserID, c.ProjectID, rs, class, "", after, limit)
		return err
	})
	return
}

// RunGetNote reads one note by id if it is in the run's scope (any other note is ErrNotFound).
func (s *Store) RunGetNote(ctx context.Context, c RunCap, approved bool, id int64) (n Note, err error) {
	lim := s.rate(c.UserID, false)
	err = s.read(ctx, func(tx *sql.Tx) error {
		rs, err := s.runScope(tx, c, approved, true)
		if err != nil {
			return err
		}
		if isMirrorID(id) {
			if n, err = readMirrorNote(ctx, tx, s.now(), c.UserID, c.ProjectID, rs, id); err == nil && lim != nil {
				return lim
			}
			return err
		}
		if _, err := visibleNote(tx, c.UserID, id, rs); err != nil {
			return err
		}
		if lim != nil {
			return lim
		}
		n, err = getNote(tx, id)
		return err
	})
	if err != nil {
		return Note{}, err
	}
	return
}

// CanChangeNote runs the checks that come before any payload or revision check of an update or delete: visible (404),
// allowed (403), archived (403). The HTTP layer calls it when a request body is malformed, too large or lacks a
// revision, so that such a request about a note the caller may not see is still a 404.
func (s *Store) CanChangeNote(ctx context.Context, p Principal, project, id int64) error {
	return s.read(ctx, func(tx *sql.Tx) error {
		_, _, err := writeAccess(tx, p, project, id)
		return err
	})
}
