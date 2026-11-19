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
	// project_id is copied from the note (a note never changes project) so triggers and the eviction index need no join,
	// also while a note delete cascades. note_history_totals is the project's byte total, kept by the triggers.
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
 project_id INTEGER NOT NULL,
 PRIMARY KEY(note_id,revision)
) WITHOUT ROWID;
CREATE INDEX note_history_project ON note_history(project_id,saved,note_id,revision);
CREATE TABLE note_history_totals(project_id INTEGER PRIMARY KEY, bytes INTEGER NOT NULL);
CREATE TRIGGER note_history_total_ins AFTER INSERT ON note_history BEGIN
 INSERT INTO note_history_totals(project_id,bytes) VALUES(NEW.project_id,NEW.bytes)
 ON CONFLICT(project_id) DO UPDATE SET bytes=bytes+NEW.bytes;
END;
CREATE TRIGGER note_history_total_del AFTER DELETE ON note_history BEGIN
 UPDATE note_history_totals SET bytes=bytes-OLD.bytes WHERE project_id=OLD.project_id;
END;`)
	return err
}

// saveNoteHistory runs before an overwrite, in the same write transaction, and never refuses it for size. Mirrors
// live in upstream_mirror, never in notes. An unchanged write needs no snapshot. burst is true only for a direct
// edit: it adds no version when the same author (by) saved the newest version less than historyBurst ago, so the
// text from before a storm of small edits survives. A reviewed replacement or an import passes burst=false, by=0.
// Cost per overwrite: O(historyRevisions) rows of this note and one totals lookup; the project's rows are read
// only when its budget is exceeded, through note_history_project, and only as many as must go.
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
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		// A clock that went backwards (negative elapsed) never coalesces.
		if elapsed := now.Sub(fromMs(saved)); err == nil && savedBy == by && elapsed >= 0 && elapsed < historyBurst {
			return nil
		}
	}
	// A logical byte budget, not the size on disk: UTF-8 text bytes plus the six stored integers.
	charge := int64(len(title)+len(body)+len(class)+len(kind)+len(provenance)) + 6*8
	if charge > maxHistoryNoteBytes || charge > maxHistoryProjectBytes {
		return nil // cannot be kept within the budget; the overwrite goes on
	}
	if _, err := tx.Exec(`INSERT INTO note_history(note_id,revision,title,body,class,entry_kind,updated,bytes,provenance,saved,saved_by,project_id) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, revision, title, body, class, kind, updated, charge, provenance, ms(now), by, project); err != nil {
		return err
	}
	// Rolling window: the oldest versions go; the one just saved (the note's newest revision) stays.
	if err := trimNote(tx, id, revision); err != nil {
		return err
	}
	return trimProject(tx, project, id, revision)
}

func trimNote(tx *sql.Tx, id, keep int64) error {
	var count, total int64
	if err := tx.QueryRow(`SELECT count(*),coalesce(sum(bytes),0) FROM note_history WHERE note_id=?`, id).Scan(&count, &total); err != nil {
		return err
	}
	if count <= maxHistoryRevisions && total <= maxHistoryNoteBytes {
		return nil
	}
	type row struct{ rev, size int64 }
	var rows []row
	rs, err := tx.Query(`SELECT revision,bytes FROM note_history WHERE note_id=? AND revision<>? ORDER BY revision`, id, keep)
	if err != nil {
		return err
	}
	for rs.Next() {
		var r row
		if err := rs.Scan(&r.rev, &r.size); err != nil {
			rs.Close()
			return err
		}
		rows = append(rows, r)
	}
	if err := rs.Close(); err != nil {
		return err
	}
	for _, r := range rows {
		if count <= maxHistoryRevisions && total <= maxHistoryNoteBytes {
			break
		}
		if _, err := tx.Exec(`DELETE FROM note_history WHERE note_id=? AND revision=?`, id, r.rev); err != nil {
			return err
		}
		count--
		total -= r.size
	}
	return rs.Err()
}

// trimProject deletes the project's oldest rows (saved, note, revision) until its total fits, never the row just saved.
func trimProject(tx *sql.Tx, project, keepID, keepRev int64) error {
	var total int64
	if err := tx.QueryRow(`SELECT coalesce((SELECT bytes FROM note_history_totals WHERE project_id=?),0)`, project).Scan(&total); err != nil {
		return err
	}
	if total <= maxHistoryProjectBytes {
		return nil
	}
	type key struct{ id, rev int64 }
	var drop []key
	rs, err := tx.Query(`SELECT note_id,revision,bytes FROM note_history WHERE project_id=? AND NOT (note_id=? AND revision=?) ORDER BY saved,note_id,revision`, project, keepID, keepRev)
	if err != nil {
		return err
	}
	for rs.Next() && total > maxHistoryProjectBytes {
		var k key
		var size int64
		if err := rs.Scan(&k.id, &k.rev, &size); err != nil {
			rs.Close()
			return err
		}
		drop = append(drop, k)
		total -= size
	}
	if err := rs.Close(); err != nil {
		return err
	}
	for _, k := range drop {
		if _, err := tx.Exec(`DELETE FROM note_history WHERE note_id=? AND revision=?`, k.id, k.rev); err != nil {
			return err
		}
	}
	return nil
}
