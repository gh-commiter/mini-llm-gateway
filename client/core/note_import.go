package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path"
	"strconv"
	"strings"
	"unicode"
)

func migrate13(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE note_imports(project_id INTEGER NOT NULL REFERENCES projects(id), key TEXT NOT NULL, note_id INTEGER NOT NULL DEFAULT 0, submission_id INTEGER NOT NULL DEFAULT 0, hash TEXT NOT NULL, revision INTEGER NOT NULL DEFAULT 0, class TEXT NOT NULL DEFAULT '', origin_gateway TEXT NOT NULL DEFAULT '', exported TEXT NOT NULL DEFAULT '', PRIMARY KEY(project_id,key));
CREATE TABLE import_sources(source TEXT PRIMARY KEY, origin_gateway TEXT NOT NULL);
ALTER TABLE notes ADD COLUMN source_meta TEXT NOT NULL DEFAULT '';
ALTER TABLE notes ADD COLUMN imported INTEGER NOT NULL DEFAULT 0;`)
	return err
}

// migrate22: the objects (projects, teams, users) that an import source created or that the owner adopted for it. An import merges
// only into these; a name that matches any other local object is a conflict the owner settles with --adopt. The name is kept so that
// a local rename (or a deleted object whose id is reused) ends the source's claim on the object.
func migrate22(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE import_objects(source TEXT NOT NULL, kind TEXT NOT NULL CHECK(kind IN('project','team','user')), id INTEGER NOT NULL, name TEXT NOT NULL, adopted INTEGER NOT NULL DEFAULT 0 CHECK(adopted IN(0,1)), PRIMARY KEY(source,kind,id)) WITHOUT ROWID`)
	return err
}

// importReceipt is the last import of one source entry: the note (or submission) it wrote,
// the content hash, the note revision and class it left behind.
type importReceipt struct {
	note, submission, revision int64
	hash, class                string
}

func receipt(tx *sql.Tx, project int64, key string) (r importReceipt, err error) {
	err = tx.QueryRow(`SELECT note_id,submission_id,hash,revision,class FROM note_imports WHERE project_id=? AND key=?`, project, key).Scan(&r.note, &r.submission, &r.hash, &r.revision, &r.class)
	if err == sql.ErrNoRows {
		err = nil
	}
	return
}
func saveImport(tx *sql.Tx, project int64, key string, r importReceipt, origin, exported string) error {
	_, err := tx.Exec(`INSERT INTO note_imports(project_id,key,note_id,submission_id,hash,revision,class,origin_gateway,exported) VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(project_id,key) DO UPDATE SET note_id=excluded.note_id,submission_id=excluded.submission_id,hash=excluded.hash,revision=excluded.revision,class=excluded.class,origin_gateway=excluded.origin_gateway,exported=excluded.exported`, project, key, r.note, r.submission, r.hash, r.revision, r.class, origin, exported)
	return err
}

// setSourceMeta keeps what the bundle said about a note's history as opaque, unverified text.
func setSourceMeta(tx *sql.Tx, id int64, meta string) error {
	if meta == "" {
		return nil
	}
	_, err := tx.Exec(`UPDATE notes SET source_meta=? WHERE id=?`, meta, id)
	return err
}
func pendingLimit(tx *sql.Tx, user, project, size, exclude int64) error {
	var un, pn, ub, pb int64
	if err := tx.QueryRow(`SELECT count(*),coalesce(sum(bytes),0) FROM kb_submissions WHERE state='pending' AND user_id=? AND id<>?`, user, exclude).Scan(&un, &ub); err != nil {
		return err
	}
	if err := tx.QueryRow(`SELECT count(*),coalesce(sum(bytes),0) FROM kb_submissions WHERE state='pending' AND project_id=? AND id<>?`, project, exclude).Scan(&pn, &pb); err != nil {
		return err
	}
	if un >= int64(maxPendingUser) || pn >= int64(maxPendingProject) || ub+size > maxPendingUserBytes || pb+size > maxPendingProjectBytes {
		return ErrPendingLimit
	}
	return nil
}

// ownFile: is the file one this gateway exported from this very project (or team notes)? Its front matter then names this
// gateway and project as its origin and a note id. Nothing in it is trusted beyond that: the id is only a candidate, and the
// note must exist in this project and be writable by the caller (see selfReceipt); anything else is imported as before.
func (s *Store) ownFile(input MarkdownInput, project int64) (id int64, rev int64, hash, class string, ok bool) {
	f, _, err := parseMarkdown([]byte(input.Markdown))
	if err != nil || f["origin"] != s.NoteOrigin(project) {
		return 0, 0, "", "", false
	}
	id, err = markdownID(f)
	rev, err2 := strconv.ParseInt(f["revision"], 10, 64)
	if err != nil || err2 != nil || rev < 1 || f["hash"] == "" {
		return 0, 0, "", "", false
	}
	return id, rev, f["hash"], f["class"], true
}

// ImportMarkdown keeps a durable receipt, independently of push idempotency.
func (s *Store) ImportMarkdown(ctx context.Context, p Principal, project int64, input MarkdownInput) (out ImportResult, err error) {
	lim := s.rate(p.UserID, true)
	in, key, parseErr := markdownInput(input)
	selfID, selfRev, selfHash, selfClass, self := s.ownFile(input, project)
	out.Key = key
	err = s.write(ctx, func(tx *sql.Tx) error {
		a, e := memberAccess(tx, p, project)
		if e != nil {
			return e
		}
		if parseErr != nil {
			return parseErr
		}
		if !a.can(pCreate, in.Class, true) {
			return ErrForbidden
		}
		if e = archivedErr(tx, project); e != nil {
			return e
		}
		if lim != nil {
			return lim
		}
		// A file this gateway exported from this project is the note it came from: the receipt is that of the note itself (a
		// later import remembers its own result under self:<id>); the revision of the file is what the note had at the export,
		// so a note changed here since is kept ("local edit"). A note that is missing, of another project or that the caller may
		// not write is not claimed: the file is imported as any other (a new note).
		if self {
			if _, _, we := writeAccess(tx, p, project, selfID); we == nil {
				key = "self:" + i64(selfID)
				out.Key = key
			} else if !errors.Is(we, ErrNotFound) && !errors.Is(we, ErrForbidden) {
				return we
			} else {
				self = false
			}
		}
		r, e := receipt(tx, project, key)
		if e != nil {
			return e
		}
		if self && r.note == 0 && r.submission == 0 {
			r = importReceipt{note: selfID, revision: selfRev, hash: selfHash, class: selfClass}
		}
		hash := contentHash(in.Title, in.Body)
		out.NoteID, out.SubmissionID = r.note, r.submission
		var old Note
		var pending SubmissionInput
		if r.note != 0 {
			_, old, e = writeAccess(tx, p, project, r.note)
			if e != nil {
				return e
			}
			if !a.canMove(old.Class, in.Class, old.AuthorID == p.UserID) {
				return ErrForbidden
			}
			old, e = getNote(tx, r.note)
			if e != nil {
				return e
			}
		}
		if r.submission != 0 {
			v, e := getSubmission(tx, p, project, r.submission, false, true)
			if e != nil {
				return e
			}
			pending = SubmissionInput{Replaces: v.Replaces, Revision: v.Revision}
			if v.State != "pending" {
				if v.State != "rejected" || (r.hash == hash && r.class == string(in.Class)) {
					out.Action, out.Reason = "skip", "submission already reviewed"
					return nil
				}
				// Keep reviewed submissions as history. Corrected content gets a new proposal.
				r.submission, r.note = 0, v.Replaces
				out.SubmissionID, out.NoteID = 0, r.note
				if r.note != 0 {
					_, _, e = writeAccess(tx, p, project, r.note)
					if e != nil {
						return e
					}
					old, e = getNote(tx, r.note)
					if e != nil {
						return e
					}
					if !a.canMove(old.Class, in.Class, old.AuthorID == p.UserID) {
						return ErrForbidden
					}
					var encoded string
					if e = tx.QueryRow(`SELECT provenance FROM note_revisions WHERE note_id=? AND revision=?`, r.note, v.Revision).Scan(&encoded); e != nil {
						return e
					}
					var previous Provenance
					if e = json.Unmarshal([]byte(encoded), &previous); e != nil {
						return e
					}
					// The receipt keeps the revision the proposal was based on, so a local edit shows.
					r.hash, r.class = previous.ContentHash, string(old.Class)
				} else {
					r.hash, r.class, r.revision = "", "", 0
				}
			}
		}
		if r.note != 0 && old.Revision != r.revision {
			out.Action, out.Reason = "skip", "local edit"
			return nil
		}
		if r.hash == hash && r.class == string(in.Class) {
			out.Action = "skip"
			return nil
		}
		if old.ID != 0 { // the kind of a published note never changes through an import; its limits apply
			in.EntryKind = old.EntryKind
			if e = checkInput(in); e != nil {
				return e
			}
		}
		if r.submission != 0 && pending.Replaces != 0 { // a replacement keeps the kind of the note it replaces, also when a re-import updates it
			if e = tx.QueryRow(`SELECT entry_kind FROM notes WHERE id=?`, pending.Replaces).Scan(&in.EntryKind); e != nil {
				return e
			}
			if e = checkInput(in); e != nil {
				return e
			}
		}
		pr := writerProvenance(p, project, in, s.now())
		pr.SourceKind, pr.SourceReference = SourceImport, path.Base(strings.ReplaceAll(input.Path, "\\", "/"))
		size := int64(len(in.Title) + len(in.Body))
		if input.Submit {
			replacement := SubmissionInput{NoteInput: in, Replaces: r.note, Revision: old.Revision}
			if r.submission != 0 {
				replacement.Replaces, replacement.Revision = pending.Replaces, pending.Revision
			}
			if e = submissionAccess(tx, p, project, replacement, true); e != nil {
				return e
			}
			for _, c := range in.Title {
				if unicode.IsControl(c) {
					return ErrInvalid{"title: control characters"}
				}
			}
			if e = pendingLimit(tx, p.UserID, project, size, r.submission); e != nil {
				return e
			}
			b, _ := json.Marshal(pr)
			if r.submission == 0 {
				res, e := tx.Exec(`INSERT INTO kb_submissions(project_id,user_id,device_id,class,title,body,bytes,replaces,revision,provenance,key,hash,received,entry_kind) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,`+newKind+`)`, project, p.UserID, p.DeviceID, in.Class, in.Title, in.Body, size, replacement.Replaces, replacement.Revision, string(b), key, hash, ms(s.now()), replacement.Replaces, in.EntryKind)
				if e != nil {
					return e
				}
				r.submission, _ = res.LastInsertId()
				r.note = 0
				r.revision = replacement.Revision
				out.Action = "create"
			} else {
				_, e := tx.Exec(`UPDATE kb_submissions SET class=?,title=?,body=?,bytes=?,provenance=?,hash=?,received=?,entry_kind=COALESCE((SELECT entry_kind FROM notes WHERE id=kb_submissions.replaces),NULLIF(?,''),entry_kind) WHERE id=?`, in.Class, in.Title, in.Body, size, string(b), hash, ms(s.now()), in.EntryKind, r.submission)
				if e != nil {
					return e
				}
				r.revision = replacement.Revision
				out.Action = "merge"
			}
		} else {
			if r.submission != 0 {
				return ErrInvalid{"import key belongs to a submission"}
			}
			r.note, e = s.importNote(tx, project, p.UserID, r.note, in, pr)
			if e != nil {
				return e
			}
			if e = tx.QueryRow(`SELECT revision FROM notes WHERE id=?`, r.note).Scan(&r.revision); e != nil {
				return e
			}
			out.Action = "create"
			if old.ID != 0 {
				out.Action = "merge"
			}
			if old.ID != 0 && old.Class != in.Class {
				if e = setMeta(tx, "epoch", i64(max(s.st.Epoch, dbInt(tx, "epoch"))+1)); e != nil {
					return e
				}
			}
		}
		r.hash, r.class = hash, string(in.Class)
		out.NoteID, out.SubmissionID = r.note, r.submission
		if e = saveImport(tx, project, key, r, pr.OriginGateway, pr.Exported); e != nil {
			return e
		}
		return Audit(tx, actor(p), "kb.import", key, map[string]any{"project": project, "submit": input.Submit})
	})
	return
}
func (s *Store) importNote(tx *sql.Tx, project, author, id int64, in NoteInput, pr Provenance) (int64, error) {
	size := int64(len(in.Title) + len(in.Body))
	var oldSize, revision int64
	if id != 0 {
		if err := tx.QueryRow(`SELECT bytes,revision FROM notes WHERE id=?`, id).Scan(&oldSize, &revision); err != nil {
			return 0, err
		}
	}
	if err := checkProjectLimits(tx, project, id == 0, size-oldSize); err != nil {
		return 0, err
	}
	if id == 0 {
		r, err := tx.Exec(`INSERT INTO notes(project_id,class,title,body,bytes,author_id,created,updated,entry_kind) VALUES(?,?,?,?,?,?,?,?,COALESCE(NULLIF(?,''),'finding'))`, project, in.Class, in.Title, in.Body, size, author, ms(s.now()), ms(s.now()), in.EntryKind)
		if err != nil {
			return 0, err
		}
		id, _ = r.LastInsertId()
	} else {
		if _, err := tx.Exec(`UPDATE notes SET class=?,title=?,body=?,bytes=?,updated=?,revision=revision+1 WHERE id=?`, in.Class, in.Title, in.Body, size, ms(s.now()), id); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(`UPDATE idempotency SET result='' WHERE note_id=?`, id); err != nil {
			return 0, err
		}
	}
	if pr.ActorKind == ActorImport {
		if _, err := tx.Exec(`UPDATE notes SET imported=1 WHERE id=?`, id); err != nil {
			return 0, err
		}
	}
	if err := syncSearch(tx, id); err != nil {
		return 0, err
	}
	if err := s.markUpstreamDirty(tx, project); err != nil {
		return 0, err
	}
	if err := saveProvenance(tx, id, revision+1, pr); err != nil {
		return 0, err
	}
	return id, nil
}
