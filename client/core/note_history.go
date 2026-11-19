package core

import (
	"database/sql"
	"fmt"
)

// Separate from current-content limits; variables let tests exercise small budgets.
var (
	maxHistoryRevisions    = int64(100)
	maxHistoryNoteBytes    = int64(8 << 20)
	maxHistoryProjectBytes = int64(500 << 20)
)

func migrate26(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE note_history(
 note_id INTEGER NOT NULL REFERENCES notes(id) ON DELETE CASCADE,
 revision INTEGER NOT NULL,
 title TEXT NOT NULL,
 body TEXT NOT NULL,
 class TEXT NOT NULL,
 entry_kind TEXT NOT NULL,
 updated INTEGER NOT NULL,
 bytes INTEGER NOT NULL,
 provenance TEXT NOT NULL,
 PRIMARY KEY(note_id,revision)
) WITHOUT ROWID`)
	return err
}

// saveNoteHistory runs before an overwrite, in the same write transaction. Mirrors
// live in upstream_mirror, never in notes. An unchanged write needs no snapshot.
func saveNoteHistory(tx *sql.Tx, id int64, in NoteInput) error {
	var project, revision, updated int64
	var title, body, class, kind, provenance string
	if err := tx.QueryRow(`SELECT n.project_id,n.revision,n.title,n.body,n.class,n.entry_kind,n.updated,r.provenance
 FROM notes n JOIN note_revisions r ON r.note_id=n.id AND r.revision=n.revision WHERE n.id=?`, id).
		Scan(&project, &revision, &title, &body, &class, &kind, &updated, &provenance); err != nil {
		return err
	}
	if title == in.Title && body == in.Body && class == string(in.Class) && kind == in.EntryKind {
		return nil
	}
	// UTF-8 text bytes plus four stored integers (id, revision, updated, charge).
	charge := int64(len(title)+len(body)+len(class)+len(kind)+len(provenance)) + 4*8
	var count, noteBytes, projectBytes int64
	if err := tx.QueryRow(`SELECT count(*),coalesce(sum(bytes),0) FROM note_history WHERE note_id=?`, id).Scan(&count, &noteBytes); err != nil {
		return err
	}
	if count+1 > maxHistoryRevisions {
		return ErrRefusal{fmt.Sprintf("history limit for this note reached: %d historical revisions", maxHistoryRevisions)}
	}
	if noteBytes+charge > maxHistoryNoteBytes {
		return ErrRefusal{fmt.Sprintf("history limit for this note reached: %d bytes", maxHistoryNoteBytes)}
	}
	// Team segments have their own project_id, as in checkProjectLimits.
	if err := tx.QueryRow(`SELECT coalesce(sum(h.bytes),0) FROM note_history h JOIN notes n ON n.id=h.note_id WHERE n.project_id=?`, project).Scan(&projectBytes); err != nil {
		return err
	}
	if projectBytes+charge > maxHistoryProjectBytes {
		return ErrRefusal{fmt.Sprintf("history limit for this project reached: %d bytes", maxHistoryProjectBytes)}
	}
	_, err := tx.Exec(`INSERT INTO note_history(note_id,revision,title,body,class,entry_kind,updated,bytes,provenance) VALUES(?,?,?,?,?,?,?,?,?)`, id, revision, title, body, class, kind, updated, charge, provenance)
	return err
}
