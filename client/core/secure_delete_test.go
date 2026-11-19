package core

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Catches: text of a deleted note (and of its saved versions) staying readable in the database file. secure_delete overwrites
// freed content; the WAL is checkpointed first because it keeps older page images until then (the test reads the main file).
func TestDeletedNoteTextIsNotLeftInTheDatabaseFile(t *testing.T) {
	f := newFx(t)
	ana, pid := f.p("ana"), f.pid["fakture"]
	marker := "PASTED-SECRET-0123456789 quokkaxylophone " + strings.Repeat("z", 2000)
	n, _, e := f.CreateNote(ctx, ana, pid, "sd", NoteInput{Class: Structure, Title: "sd title", Body: marker})
	f.ok(e)
	pg, e := f.SearchNotes(ctx, ana, pid, "quokkaxylophone") // the word is an FTS term while the note holds it
	f.ok(e)
	if len(pg.Hits) != 1 {
		t.Fatal("the marker word is not findable as a search term before the delete", pg.Hits)
	}
	n, e = f.UpdateNote(ctx, ana, pid, n.ID, n.Revision, NoteInput{Class: Structure, Title: "sd title", Body: "clean"}) // the marker is now only in the history
	f.ok(e)
	f.ok(f.DeleteNote(ctx, ana, pid, n.ID, n.Revision))
	f.ok(f.Close())                                           // closing checkpoints the WAL into the main file
	for _, name := range []string{"core.db", "core.db-wal"} { // the main file and, if it is still there, the log
		raw, err := os.ReadFile(filepath.Join(f.dir, name))
		if err != nil {
			continue
		}
		for _, needle := range []string{"PASTED-SECRET-0123456789", "quokkaxylophone"} { // the stored text, and the word as an FTS posting
			if strings.Contains(string(raw), needle) {
				t.Fatalf("%q is still in %s", needle, name)
			}
		}
	}
}

// Catches: the FTS5 secure-delete option missing on a new or an existing database (set at open, idempotently, no migration).
func TestFTSSecureDeleteIsSetAtOpen(t *testing.T) {
	f := newFx(t)
	option := func(s *Store, table string) int64 {
		var v int64
		if err := s.DB().QueryRow(`SELECT coalesce((SELECT v FROM ` + table + `_config WHERE k='secure-delete'),0)`).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	for _, table := range []string{"note_search", "upstream_mirror_search"} {
		if option(f.Store, table) != 1 {
			t.Fatal("not set on a new database", table)
		}
	}
	// an existing database that never had it: the option row is removed, the store reopened
	exec(t, f.Store, `DELETE FROM note_search_config WHERE k='secure-delete'`)
	exec(t, f.Store, `DELETE FROM upstream_mirror_search_config WHERE k='secure-delete'`)
	dir := f.dir
	f.ok(f.Close())
	s := openT(t, dir)
	defer s.Close()
	for _, table := range []string{"note_search", "upstream_mirror_search"} {
		if option(s, table) != 1 {
			t.Fatal("not set on reopening an existing database", table)
		}
	}
}
