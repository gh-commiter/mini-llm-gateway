package core

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Catches: a rebuild of the search index that is quadratic in the number of notes (FTS5 secure-delete makes a bulk DELETE of a
// whole table slow unless the option is off around it).
func TestRebuildSearchOfManyNotesIsFast(t *testing.T) {
	f := newFx(t)
	pid, uid := f.pid["fakture"], f.uid["ana"]
	f.ok(f.write(ctx, func(tx *sql.Tx) error {
		for i := 0; i < 2000; i++ {
			var b strings.Builder
			for w := 0; w < 400; w++ {
				fmt.Fprintf(&b, "w%dx%d ", i, w%97)
			}
			body := b.String()
			if _, e := tx.Exec(`INSERT INTO notes(project_id,class,title,body,bytes,author_id,created,updated,entry_kind) VALUES(?,?,?,?,?,?,1,1,'finding')`, pid, Structure, fmt.Sprint("note ", i), body, len(body)+8, uid); e != nil {
				return e
			}
		}
		return nil
	}))
	// An index as incremental writes leave it (many small segments: that is what makes a whole-table delete with secure-delete on slow);
	// the same state is made before each measurement, the bulk rebuild runs first and the old way second.
	unmerged := func() {
		f.ok(f.write(ctx, func(tx *sql.Tx) error {
			for _, q := range []string{`INSERT INTO note_search(note_search, rank) VALUES('secure-delete', 0)`, `DELETE FROM note_search`, searchInsert, `INSERT INTO note_search(note_search, rank) VALUES('secure-delete', 1)`} {
				if _, err := tx.Exec(q); err != nil {
					return err
				}
			}
			return nil
		}))
	}
	unmerged()
	start := time.Now()
	f.ok(f.RebuildSearch(ctx)) // deletes 2000 indexed notes and indexes them again (the bulk path)
	bulk := time.Since(start)
	unmerged()
	start = time.Now() // the same delete and re-index of the same rows the OLD way: secure-delete left on for the whole-table delete
	f.ok(f.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM note_search; ` + searchInsert)
		return err
	}))
	old := time.Since(start)
	t.Logf("rebuild of 2000 notes of 400 words: bulk %v, old way %v", bulk, old)
	if bulk > old/3 { // relative, on the same machine
		t.Fatalf("the bulk rebuild (%v) is not clearly faster than the old way (%v)", bulk, old)
	}
	for _, table := range []string{"note_search", "upstream_mirror_search"} { // the option is back on after the bulk delete
		var v int64
		f.ok(f.DB().QueryRow(`SELECT coalesce((SELECT v FROM ` + table + `_config WHERE k='secure-delete'),0)`).Scan(&v))
		if v != 1 {
			t.Fatal("secure-delete is not back on for", table)
		}
	}
}

// Catches: an unlink of a link with thousands of mirrored notes that is quadratic (FTS5 secure-delete on a bulk delete), AND the
// unlinked gateway's text staying in the database file afterwards (the bulk path turns secure-delete off around the delete and merges).
func TestMirrorPurgeOfManyRowsIsFastAndLeavesNoText(t *testing.T) {
	f := newFx(t)
	fk := f.pid["fakture"]
	f.ok(f.AddUpstream(ctx, "hub", "hub.example:8765"))
	f.ok(f.AddUpstream(ctx, "hub2", "hub2.example:8765"))
	f.ok(f.write(ctx, func(tx *sql.Tx) error {
		for _, up := range []string{"hub", "hub2"} {
			for i := 0; i < 2000; i++ {
				var b strings.Builder
				for w := 0; w < 400; w++ {
					fmt.Fprintf(&b, "m%dx%d ", i, w%97)
				}
				if i == 1500 && up == "hub" {
					b.WriteString("zebrafootballxyz")
				}
				body := b.String()
				r, e := tx.Exec(`INSERT INTO upstream_mirror(project_id,upstream,source_id,source_label,remote_note,remote_rev,class,kind,title,body,bytes,updated,fetched) VALUES(?,?,7,'',?,3,'structure','finding',?,?,?,1,1)`, fk, up, i+1, fmt.Sprint("m ", i), body, len(body))
				if e != nil {
					return e
				}
				id, _ := r.LastInsertId()
				if _, e = tx.Exec(`INSERT INTO upstream_mirror_search(rowid,title,body) VALUES(?,?,?)`, id, fmt.Sprint("m ", i), body); e != nil {
					return e
				}
			}
		}
		return nil
	}))
	inFile := func() bool {
		for _, name := range []string{"core.db", "core.db-wal"} {
			raw, err := os.ReadFile(filepath.Join(f.dir, name))
			if errors.Is(err, fs.ErrNotExist) && name == "core.db-wal" {
				continue
			}
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), "zebrafootballxyz") {
				return true
			}
		}
		return false
	}
	if !inFile() {
		t.Fatal("the marker word is not in the database files before the unlink: the test cannot see anything")
	}
	// The two paths are timed on the statements alone: each run is rolled back, so no commit (the fsync and the checkpoint of a
	// large log, the one cost both paths share and the one a busy disk stretches: on the 0.9.5 Windows run both took 32.8 s) is in
	// the number, and the same state is measured again. Each is run three times and the minimum counts.
	errUndo := errors.New("undo")
	measure := func(fn func(tx *sql.Tx) error) time.Duration {
		best := time.Duration(1<<63 - 1)
		for i := 0; i < 3; i++ {
			var d time.Duration
			err := f.write(ctx, func(tx *sql.Tx) error {
				start := time.Now()
				if e := fn(tx); e != nil {
					return e
				}
				d = time.Since(start)
				return errUndo
			})
			if !errors.Is(err, errUndo) {
				t.Fatal(err)
			}
			best = min(best, d)
		}
		return best
	}
	old := measure(func(tx *sql.Tx) error { // one link's 2000 rows the OLD way (secure-delete left on) ...
		if _, e := tx.Exec(`DELETE FROM upstream_mirror_search WHERE rowid IN (SELECT id FROM upstream_mirror WHERE upstream='hub2')`); e != nil {
			return e
		}
		_, e := tx.Exec(`DELETE FROM upstream_mirror WHERE upstream='hub2'`)
		return e
	})
	bulk := measure(func(tx *sql.Tx) error { _, e := purgeMirror(tx, fk, "hub"); return e }) // ... the other link's through the bulk path
	t.Logf("purge of 2000 mirror rows (statements only, best of 3): bulk %v, old way %v", bulk, old)
	if old < time.Second { // the per-row work is what the bulk path saves: where the old way is not slow, there is nothing to compare
		t.Logf("comparison skipped: the old way took %v (< 1s), too fast here to show a difference", old)
	} else if bulk > old/3 { // relative, same machine, same state
		t.Fatalf("the bulk purge (%v) is not clearly faster than the old way (%v)", bulk, old)
	}
	f.ok(f.write(ctx, func(tx *sql.Tx) error { _, e := purgeMirror(tx, fk, "hub"); return e })) // the real purge, for the leftover check
	f.ok(f.Close())                                                                             // a clean close checkpoints the log into the main file
	if inFile() {
		t.Fatal("the unlinked gateway's text is still in the database files")
	}
}
