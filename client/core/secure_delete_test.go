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
	marker := "PASTED-SECRET-0123456789-" + strings.Repeat("z", 2000)
	n, _, e := f.CreateNote(ctx, ana, pid, "sd", NoteInput{Class: Structure, Title: "sd title", Body: marker})
	f.ok(e)
	n, e = f.UpdateNote(ctx, ana, pid, n.ID, n.Revision, NoteInput{Class: Structure, Title: "sd title", Body: "clean"}) // the marker is now only in the history
	f.ok(e)
	f.ok(f.DeleteNote(ctx, ana, pid, n.ID, n.Revision))
	f.ok(f.Close()) // closing checkpoints the WAL into the main file
	raw, err := os.ReadFile(filepath.Join(f.dir, "core.db"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "PASTED-SECRET-0123456789") {
		t.Fatal("the deleted text is still in core.db")
	}
}
