package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// NoteVersion is an outgoing revision. Bytes counts text, not the budget charge.
type NoteVersion struct {
	Revision  int64     `json:"revision"`
	Saved     time.Time `json:"saved"`
	SavedBy   string    `json:"savedBy"`
	Class     Class     `json:"class"`
	EntryKind string    `json:"entryKind"`
	Bytes     int64     `json:"bytes"`
	Title     string    `json:"title"`
	Updated   time.Time `json:"updated"`
	// WrittenBy: who wrote this version; ReplacedBy: who or what overwrote it (Saved is when). Both are words, not ids.
	WrittenBy  string          `json:"writtenBy"`
	ReplacedBy string          `json:"replacedBy"`
	Body       *string         `json:"body,omitempty"`
	Provenance json.RawMessage `json:"provenance,omitempty"`
}

type HistoryNote struct {
	ID       int64  `json:"id"`
	Revision int64  `json:"revision"`
	Class    Class  `json:"class"`
	Title    string `json:"title"`
	Team     string `json:"team,omitempty"`
}
type NoteHistory struct {
	Note    HistoryNote   `json:"note"`
	History []NoteVersion `json:"history"`
}

// Use the same source entry for current visibility and saved classes.
func historyAccess(tx *sql.Tx, p *Principal, project, id int64) (Note, readable, error) {
	if p == nil {
		n, err := noteMetadata(tx, id)
		return n, readable{project: n.ProjectID, all: allClasses}, err
	}
	a, err := memberAccess(tx, *p, project)
	if err != nil {
		return Note{}, readable{}, err
	}
	rs, err := noteSources(tx, p.UserID, project, a)
	if err != nil {
		return Note{}, readable{}, err
	}
	n, err := visibleNote(tx, p.UserID, id, rs)
	if err != nil {
		return Note{}, readable{}, err
	}
	for _, r := range rs {
		if r.project == n.ProjectID && r.allows(n.Class, n.AuthorID == p.UserID) {
			return n, r, nil
		}
	}
	return Note{}, readable{}, ErrNotFound
}

func versions(tx *sql.Tx, n Note, r readable, own bool, revision int64) ([]NoteVersion, error) {
	// succ: the provenance of the revision that overwrote this one (it tells a review or an import from an edit).
	q := `SELECT h.revision,h.saved,COALESCE(u.name,''),h.class,h.entry_kind,h.title,h.body,h.updated,h.provenance,COALESCE((SELECT r2.provenance FROM note_revisions r2 WHERE r2.note_id=h.note_id AND r2.revision=h.revision+1),'') FROM note_history h LEFT JOIN users u ON u.id=h.saved_by WHERE h.note_id=?`
	args := []any{n.ID}
	if revision > 0 {
		q += ` AND h.revision=?`
		args = append(args, revision)
	}
	rows, err := tx.Query(q+` ORDER BY h.revision DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []NoteVersion{}
	for rows.Next() {
		var v NoteVersion
		var saved, updated int64
		var body, provenance, successor string
		if err := rows.Scan(&v.Revision, &saved, &v.SavedBy, &v.Class, &v.EntryKind, &v.Title, &body, &updated, &provenance, &successor); err != nil {
			return nil, err
		}
		if !r.allows(v.Class, own) {
			continue
		}
		v.Saved, v.Updated = fromMs(saved), fromMs(updated)
		v.WrittenBy, v.ReplacedBy = describeWriter(tx, provenance), describeReplacer(tx, v.SavedBy, successor)
		v.Bytes = int64(len(v.Title) + len(body))
		if revision > 0 {
			v.Body = &body
			v.Provenance = json.RawMessage("null")
			if provenance != "" {
				v.Provenance = json.RawMessage(provenance)
			}
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ListNoteHistory accepts nil only on the owner API. Lists do not audit private reads.
func (s *Store) ListNoteHistory(ctx context.Context, p *Principal, project, id int64) (out NoteHistory, err error) {
	var lim error
	if p != nil {
		lim = s.rate(p.UserID, false)
	}
	err = s.read(ctx, func(tx *sql.Tx) error {
		if e := s.mirrorHistory(ctx, tx, p, project, id); e != nil {
			return e
		}
		n, r, e := historyAccess(tx, p, project, id)
		if e != nil {
			return e
		}
		if lim != nil {
			return lim
		}
		if e = fillTeam(tx, &n, nil); e != nil {
			return e
		}
		out.Note = HistoryNote{n.ID, n.Revision, n.Class, n.Title, n.Team}
		out.History, e = versions(tx, n, r, p != nil && n.AuthorID == p.UserID, 0)
		return e
	})
	if err != nil {
		return NoteHistory{}, err
	}
	return
}

func (s *Store) GetNoteVersion(ctx context.Context, p *Principal, project, id, revision int64) (out NoteVersion, err error) {
	var n Note
	var lim error
	if p != nil {
		lim = s.rate(p.UserID, false)
	}
	err = s.read(ctx, func(tx *sql.Tx) error {
		var r readable
		var e error
		if e = s.mirrorHistory(ctx, tx, p, project, id); e != nil {
			return e
		}
		n, r, e = historyAccess(tx, p, project, id)
		if e != nil {
			return e
		}
		if lim != nil {
			return lim
		}
		if revision <= 0 {
			return ErrNotFound
		}
		vs, e := versions(tx, n, r, p != nil && n.AuthorID == p.UserID, revision)
		if e != nil {
			return e
		}
		if len(vs) == 0 {
			return ErrNotFound
		}
		out = vs[0]
		return fillTeam(tx, &n, nil)
	})
	if err == nil && p == nil && (n.Class == Private || out.Class == Private) {
		detail := noteScope(n)
		detail["revision"] = revision
		err = s.write(ctx, func(tx *sql.Tx) error {
			return Audit(tx, adminActor, "kb.owner.read", strconv.FormatInt(id, 10), detail)
		})
	}
	if err != nil {
		return NoteVersion{}, err
	}
	return
}

func noteScope(n Note) map[string]any {
	if isSegment(n.ProjectID) {
		return map[string]any{"team": n.Team}
	}
	return map[string]any{"project": n.ProjectID}
}

// RestoreNote copies saved text into a new revision, without changing the author.
func (s *Store) RestoreNote(ctx context.Context, p *Principal, project, id, from, expected int64, class Class) (n Note, err error) {
	var lim error
	if p != nil {
		lim = s.rate(p.UserID, true)
	}
	err = s.write(ctx, func(tx *sql.Tx) error {
		var old Note
		var a access
		var e error
		if e = s.mirrorHistory(ctx, tx, p, project, id); e != nil {
			return e
		}
		if p == nil {
			old, e = noteMetadata(tx, id)
			if e == nil {
				e = archivedErr(tx, old.ProjectID)
			}
		} else {
			a, old, e = writeAccess(tx, *p, project, id)
			if e == nil && a.Role != Admin {
				e = ErrForbidden
			}
		}
		if e != nil {
			return e
		}
		if expected != old.Revision {
			return ErrRevision{Current: old.Revision}
		}
		if from <= 0 {
			return ErrNotFound
		}
		vs, e := versions(tx, old, ownerReadable(old.ProjectID)[0], false, from)
		if e != nil {
			return e
		}
		if len(vs) == 0 {
			return ErrNotFound
		}
		v := vs[0]
		if class != old.Class && class != v.Class {
			return ErrInvalid{"class"}
		}
		in := NoteInput{Class: class, Title: v.Title, Body: *v.Body, EntryKind: v.EntryKind}
		if e = checkInput(in); e != nil {
			return e
		}
		if p != nil && (!a.can(pRead, v.Class, old.AuthorID == p.UserID) || !a.canMove(old.Class, class, old.AuthorID == p.UserID)) {
			return ErrForbidden
		}
		if lim != nil {
			return lim
		}
		if e = checkSize(in); e != nil {
			return e
		}
		current, e := getNote(tx, id)
		if e != nil {
			return e
		}
		if current.Title == in.Title && current.Body == in.Body && current.Class == in.Class && current.EntryKind == in.EntryKind {
			return ErrRefusal{fmt.Sprintf("revision %d has the same text as the current note: nothing to restore", from)}
		}
		size := int64(len(in.Title) + len(in.Body))
		var oldSize int64
		if e = tx.QueryRow(`SELECT bytes FROM notes WHERE id=?`, id).Scan(&oldSize); e != nil {
			return e
		}
		if e = checkProjectLimits(tx, old.ProjectID, false, size-oldSize); e != nil {
			return e
		}
		now := fromMs(ms(s.now()))
		by := int64(0)
		who := "owner"
		provenance := Provenance{ActorKind: "owner"}
		if p != nil {
			by = p.UserID
			who = actor(*p)
			provenance = writerProvenance(*p, old.ProjectID, in, now)
		}
		provenance.SourceKind = "restored"
		provenance.RestoredFrom = from
		if e = saveNoteHistory(tx, id, in, now, by); e != nil {
			return e
		}
		if _, e = tx.Exec(`UPDATE notes SET class=?,title=?,body=?,bytes=?,updated=?,entry_kind=?,revision=revision+1 WHERE id=?`, class, in.Title, in.Body, size, ms(now), in.EntryKind, id); e != nil {
			return e
		}
		if e = s.markUpstreamDirty(tx, old.ProjectID); e != nil {
			return e
		}
		if class != old.Class {
			if _, e = tx.Exec(`UPDATE idempotency SET result='' WHERE note_id=?`, id); e != nil {
				return e
			}
			if _, e = s.BumpEpochTx(tx, "note class narrowed"); e != nil {
				return e
			}
		}
		if e = syncSearch(tx, id); e != nil {
			return e
		}
		if e = saveProvenance(tx, id, old.Revision+1, provenance); e != nil {
			return e
		}
		if e = fillTeam(tx, &old, nil); e != nil {
			return e
		}
		detail := noteScope(old)
		detail["from"] = from
		detail["revision"] = old.Revision + 1
		detail["class"] = class
		detail["fromClass"] = v.Class
		if e = Audit(tx, who, "note.restore", strconv.FormatInt(id, 10), detail); e != nil {
			return e
		}
		n, e = getNote(tx, id)
		return e
	})
	if err != nil {
		return Note{}, err
	}
	return
}

func parseProvenance(s string) (p Provenance, ok bool) {
	return p, s != "" && json.Unmarshal([]byte(s), &p) == nil
}

// describeWriter names the writer of a version from its stored provenance, as the provenance marks it.
func describeWriter(tx *sql.Tx, provenance string) string {
	p, ok := parseProvenance(provenance)
	switch {
	case !ok:
		return "unknown"
	case p.ActorKind == ActorImport:
		return "an import"
	case p.ActorKind == ActorGatewayService:
		return "the gateway"
	case p.ActorKind == "owner":
		return "the gateway owner"
	}
	var name string
	if p.User == 0 || tx.QueryRow(`SELECT name FROM users WHERE id=?`, p.User).Scan(&name) != nil {
		return "unknown"
	}
	return name
}

// describeReplacer says who or what overwrote a version: the editing user (saved_by) or, for a review or an import,
// what the overwriting revision's provenance shows.
func describeReplacer(tx *sql.Tx, savedBy, successor string) string {
	p, ok := parseProvenance(successor)
	restored := ""
	if ok && p.SourceKind == "restored" {
		restored = " (restore)"
		if p.RestoredFrom > 0 {
			restored = fmt.Sprintf(" (restore of revision %d)", p.RestoredFrom)
		}
	}
	switch {
	case savedBy != "":
		return savedBy + restored
	case !ok:
		return "a review or import"
	case p.ActorKind == "owner":
		return "the gateway owner" + restored
	case p.ActorKind == ActorImport || p.SourceKind == SourceImport:
		return "an import"
	case p.Reviewer != "":
		return "an accepted submission (reviewer " + reviewerName(tx, p.Reviewer) + ")"
	}
	return "a review or import"
}

// mirrorHistory: a mirrored note (a copy from an upstream gateway) has no history here. A caller who may read the copy
// is told where its versions are, which they can already see; anyone else gets the same not found as for any unknown note.
func (s *Store) mirrorHistory(ctx context.Context, tx *sql.Tx, p *Principal, project, id int64) error {
	if p == nil || !isMirrorID(id) {
		return nil
	}
	a, err := memberAccess(tx, *p, project)
	if err != nil {
		return err
	}
	rs, err := noteSources(tx, p.UserID, project, a)
	if err != nil {
		return err
	}
	n, err := readMirrorNote(ctx, tx, s.now(), p.UserID, project, rs, id)
	if err != nil {
		return err
	}
	return ErrRefusal{"this note is a copy from the upstream gateway " + n.From + ": its saved versions are on that gateway; run minigw note history there"}
}

// reviewerName: a member reviewer is stored as "user:<id>" (the owner as "owner"); the row shows the user's name,
// or "a former member" for a user that no longer exists, never the id.
func reviewerName(tx *sql.Tx, reviewer string) string {
	id, ok := strings.CutPrefix(reviewer, "user:")
	if !ok {
		return reviewer
	}
	var name string
	if n, err := strconv.ParseInt(id, 10, 64); err != nil || tx.QueryRow(`SELECT name FROM users WHERE id=?`, n).Scan(&name) != nil {
		return "a former member"
	}
	return name
}
