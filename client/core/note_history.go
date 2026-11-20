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
	historyBurst           = 10 * time.Minute // a version saved this soon after the one before it, by the same author, is a burst interior
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
// live in upstream_mirror, never in notes. An unchanged write needs no snapshot; every other overwrite saves the
// previous version. A storm of small edits is handled at eviction (see pickVictim), not by skipping versions.
// by is the editing user, 0 for a reviewed replacement or an import.
// Cost per overwrite: O(historyRevisions) rows of this note and one totals lookup; the project's rows are read
// only when its budget is exceeded, through note_history_project, and only as many as must go.
func saveNoteHistory(tx *sql.Tx, id int64, in NoteInput, now time.Time, by int64) error {
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
	// A logical byte budget, not the size on disk: UTF-8 text bytes plus the seven stored integers (id, revision, updated, charge, saved, saved_by, project).
	charge := int64(len(title)+len(body)+len(class)+len(kind)+len(provenance)) + 7*8
	if charge > maxHistoryNoteBytes || charge > maxHistoryProjectBytes {
		return nil // cannot be kept within the budget; the overwrite goes on
	}
	// Plain insert, never the replace form: a replace deletes the old row without firing the totals trigger
	// (recursive triggers are off), and the project total would drift.
	if _, err := tx.Exec(`INSERT INTO note_history(note_id,revision,title,body,class,entry_kind,updated,bytes,provenance,saved,saved_by,project_id) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, revision, title, body, class, kind, updated, charge, provenance, ms(now), by, project); err != nil {
		return err
	}
	// Rolling window: the oldest versions go; the one just saved (the note's newest revision) stays.
	if err := trimNote(tx, id); err != nil {
		return err
	}
	return trimProject(tx, project, id, revision, by)
}

type historyRow struct{ rev, saved, by, size int64 }

// pickVictim returns the index of the row to evict from rows (ordered by revision, the last one is the row just
// saved and is never chosen), or -1. Among the burst interiors it picks the version that lived the shortest, the
// smallest gap to the row before it (ties: the oldest). An interior is a row whose predecessor has the same
// non-zero author and was saved 0 < gap < historyBurst earlier (a zero or negative gap, a clock that went back, is
// not interior). Without an interior, the oldest row goes.
func pickVictim(rows []historyRow) int {
	best, bestGap := -1, int64(0)
	for i := 1; i < len(rows)-1; i++ {
		gap := rows[i].saved - rows[i-1].saved
		if rows[i].by == 0 || rows[i].by != rows[i-1].by || gap <= 0 || time.Duration(gap)*time.Millisecond >= historyBurst {
			continue
		}
		if best < 0 || gap < bestGap {
			best, bestGap = i, gap
		}
	}
	if best >= 0 {
		return best
	}
	if len(rows) > 1 {
		return 0
	}
	return -1
}

// noteRows: the saved versions of one note by revision, and their byte total.
func noteRows(tx *sql.Tx, id int64) ([]historyRow, int64, error) {
	var rows []historyRow
	var total int64
	rs, err := tx.Query(`SELECT revision,saved,saved_by,bytes FROM note_history WHERE note_id=? ORDER BY revision`, id)
	if err != nil {
		return nil, 0, err
	}
	for rs.Next() {
		var r historyRow
		if err := rs.Scan(&r.rev, &r.saved, &r.by, &r.size); err != nil {
			rs.Close()
			return nil, 0, err
		}
		rows = append(rows, r)
		total += r.size
	}
	if err := rs.Err(); err != nil { // before any deletion: a failed iteration must not leave a partial eviction
		rs.Close()
		return nil, 0, err
	}
	return rows, total, rs.Close()
}

func trimNote(tx *sql.Tx, id int64) error {
	rows, total, err := noteRows(tx, id)
	if err != nil {
		return err
	}
	var drop []int64
	for int64(len(rows)) > maxHistoryRevisions || total > maxHistoryNoteBytes {
		i := pickVictim(rows)
		if i < 0 {
			break
		}
		drop = append(drop, rows[i].rev)
		total -= rows[i].size
		rows = append(rows[:i:i], rows[i+1:]...)
	}
	for _, rev := range drop {
		if _, err := tx.Exec(`DELETE FROM note_history WHERE note_id=? AND revision=?`, id, rev); err != nil {
			return err
		}
	}
	return nil
}

// trimProject keeps the project within its budget, never touching the row just saved (keepID, keepRev: the newest version of the
// note being written). The order is the writer's own first, so one developer's edit loop does not evict everyone else's history:
// 1. older versions of the note being written (the burst interiors first, then the oldest, as trimNote picks);
// 2. versions saved by the same writer (by != 0) in other notes, oldest first, keeping each note's newest saved version;
// 3. the project's oldest rows, as before (when the writer's own rows cannot free enough).
func trimProject(tx *sql.Tx, project, keepID, keepRev, by int64) error {
	var total int64
	if err := tx.QueryRow(`SELECT coalesce((SELECT bytes FROM note_history_totals WHERE project_id=?),0)`, project).Scan(&total); err != nil {
		return err
	}
	if total <= maxHistoryProjectBytes {
		return nil
	}
	type key struct{ id, rev int64 }
	var drop []key
	gone := map[key]bool{}
	take := func(k key, size int64) {
		drop = append(drop, k)
		gone[k] = true
		total -= size
	}
	rows, _, err := noteRows(tx, keepID)
	if err != nil {
		return err
	}
	for total > maxHistoryProjectBytes {
		i := pickVictim(rows)
		if i < 0 {
			break
		}
		take(key{keepID, rows[i].rev}, rows[i].size)
		rows = append(rows[:i:i], rows[i+1:]...)
	}
	// phases 2 and 3 read candidates in order and stop as soon as the total fits; rows already chosen are skipped
	phase := func(q string, args ...any) error {
		if total <= maxHistoryProjectBytes {
			return nil
		}
		rs, err := tx.Query(q, args...)
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
			if gone[k] {
				continue
			}
			take(k, size)
		}
		if err := rs.Err(); err != nil { // before any deletion
			rs.Close()
			return err
		}
		return rs.Close()
	}
	if by != 0 {
		if err := phase(`SELECT h.note_id,h.revision,h.bytes FROM note_history h WHERE h.project_id=? AND h.saved_by=? AND h.note_id<>?
 AND h.revision<(SELECT MAX(m.revision) FROM note_history m WHERE m.note_id=h.note_id) ORDER BY h.saved,h.note_id,h.revision`, project, by, keepID); err != nil {
			return err
		}
	}
	if err := phase(`SELECT note_id,revision,bytes FROM note_history WHERE project_id=? AND NOT (note_id=? AND revision=?) ORDER BY saved,note_id,revision`, project, keepID, keepRev); err != nil {
		return err
	}
	for _, k := range drop {
		if _, err := tx.Exec(`DELETE FROM note_history WHERE note_id=? AND revision=?`, k.id, k.rev); err != nil {
			return err
		}
	}
	return nil
}
