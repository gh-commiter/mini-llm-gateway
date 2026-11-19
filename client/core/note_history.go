package core

import (
	"database/sql"
	"time"
)

// Limits of the history. An overwrite is never refused because of them: the oldest versions go first.
// Variables let tests exercise small budgets and short bursts.
var (
	maxHistoryRevisions    = int64(100)
	maxHistoryNoteBytes    = int64(8 << 20)
	maxHistoryProjectBytes = int64(500 << 20)
	historyBurst           = 10 * time.Minute // edits by one author this soon after the newest saved version add no version
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
 saved INTEGER NOT NULL,
 saved_by INTEGER NOT NULL,
 PRIMARY KEY(note_id,revision)
) WITHOUT ROWID`)
	return err
}

// saveNoteHistory runs before an overwrite, in the same write transaction, and never refuses it for size. Mirrors
// live in upstream_mirror, never in notes. An unchanged write needs no snapshot. burst is true only for a direct
// edit: it adds no version when the same author (by) saved the newest version less than historyBurst ago, so the
// text from before a storm of small edits survives. A reviewed replacement or an import passes burst=false, by=0.
func saveNoteHistory(tx *sql.Tx, id int64, in NoteInput, now time.Time, by int64, burst bool) error {
	var project, revision, updated int64
	var title, body, class, kind, provenance string
	// A note without a provenance row (older databases) is kept with an empty provenance.
	if err := tx.QueryRow(`SELECT n.project_id,n.revision,n.title,n.body,n.class,n.entry_kind,n.updated,COALESCE(r.provenance,'')
 FROM notes n LEFT JOIN note_revisions r ON r.note_id=n.id AND r.revision=n.revision WHERE n.id=?`, id).
		Scan(&project, &revision, &title, &body, &class, &kind, &updated, &provenance); err != nil {
		return err
	}
	if title == in.Title && body == in.Body && class == string(in.Class) && kind == in.EntryKind {
		return nil
	}
	if burst {
		var saved, savedBy int64
		err := tx.QueryRow(`SELECT saved,saved_by FROM note_history WHERE note_id=? ORDER BY revision DESC LIMIT 1`, id).Scan(&saved, &savedBy)
		if err == nil && savedBy == by && now.Sub(fromMs(saved)) < historyBurst {
			return nil
		}
		if err != nil && err != sql.ErrNoRows {
			return err
		}
	}
	// UTF-8 text bytes plus four stored integers (id, revision, updated, charge).
	charge := int64(len(title)+len(body)+len(class)+len(kind)+len(provenance)) + 4*8
	if charge > maxHistoryNoteBytes || charge > maxHistoryProjectBytes {
		return nil // cannot be kept within the budget; the overwrite goes on
	}
	if _, err := tx.Exec(`INSERT INTO note_history(note_id,revision,title,body,class,entry_kind,updated,bytes,provenance,saved,saved_by) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		id, revision, title, body, class, kind, updated, charge, provenance, ms(now), by); err != nil {
		return err
	}
	// Rolling window: the oldest versions go; the one just saved (the note's newest revision) stays.
	if _, err := tx.Exec(`DELETE FROM note_history WHERE note_id=? AND revision IN (SELECT revision FROM note_history WHERE note_id=? ORDER BY revision DESC LIMIT -1 OFFSET ?)`, id, id, maxHistoryRevisions); err != nil {
		return err
	}
	if err := trimHistory(tx, `note_id=?`, id, `revision`, maxHistoryNoteBytes, id, revision); err != nil {
		return err
	}
	// Team segments have their own project_id, as in checkProjectLimits.
	return trimHistory(tx, `note_id IN (SELECT id FROM notes WHERE project_id=?)`, project, `saved,note_id,revision`, maxHistoryProjectBytes, id, revision)
}

// trimHistory deletes the oldest rows (order) of the scope until its bytes fit the cap, never the row keepID/keepRev.
func trimHistory(tx *sql.Tx, scope string, arg any, order string, limit, keepID, keepRev int64) error {
	for {
		var total int64
		if err := tx.QueryRow(`SELECT coalesce(sum(bytes),0) FROM note_history WHERE `+scope, arg).Scan(&total); err != nil {
			return err
		}
		if total <= limit {
			return nil
		}
		var id, rev, size int64
		err := tx.QueryRow(`SELECT note_id,revision,bytes FROM note_history WHERE (`+scope+`) AND NOT (note_id=? AND revision=?) ORDER BY `+order+` LIMIT 1`, arg, keepID, keepRev).Scan(&id, &rev, &size)
		if err == sql.ErrNoRows {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM note_history WHERE note_id=? AND revision=?`, id, rev); err != nil {
			return err
		}
	}
}
