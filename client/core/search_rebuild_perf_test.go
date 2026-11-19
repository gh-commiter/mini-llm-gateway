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
		for i := 0; i < 3000; i++ {
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
	f.ok(f.RebuildSearch(ctx)) // the first one fills the index
	start := time.Now()
	f.ok(f.RebuildSearch(ctx)) // the second deletes 3000 indexed notes and indexes them again
	d := time.Since(start)
	t.Logf("rebuild of 3000 notes of 400 words: %v", d)
	if d > 2500*time.Millisecond { // with the option on it took about 5 s here (quadratic), now about 0.4 s
		t.Fatalf("rebuild took %v", d)
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
	f.ok(f.write(ctx, func(tx *sql.Tx) error {
		for i := 0; i < 3000; i++ {
			var b strings.Builder
			for w := 0; w < 400; w++ {
				fmt.Fprintf(&b, "m%dx%d ", i, w%97)
			}
			if i == 1500 {
				b.WriteString("zebrafootballxyz")
			}
			body := b.String()
			r, e := tx.Exec(`INSERT INTO upstream_mirror(project_id,upstream,source_id,source_label,remote_note,remote_rev,class,kind,title,body,bytes,updated,fetched) VALUES(?,?,7,'',?,3,'structure','finding',?,?,?,1,1)`, fk, "hub", i+1, fmt.Sprint("m ", i), body, len(body))
			if e != nil {
				return e
			}
			id, _ := r.LastInsertId()
			if _, e = tx.Exec(`INSERT INTO upstream_mirror_search(rowid,title,body) VALUES(?,?,?)`, id, fmt.Sprint("m ", i), body); e != nil {
				return e
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
	start := time.Now()
	f.ok(f.write(ctx, func(tx *sql.Tx) error { _, e := purgeMirror(tx, fk, "hub"); return e }))
	d := time.Since(start)
	t.Logf("purge of 3000 mirror rows: %v", d)
	if d > 2500*time.Millisecond { // with the option on it took about 5 s here, now about 0.3 s
		t.Fatalf("purge took %v", d)
	}
	f.ok(f.Close()) // a clean close checkpoints the log into the main file
	if inFile() {
		t.Fatal("the unlinked gateway's text is still in the database files")
	}
}
