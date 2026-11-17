package core

import (
	"strings"
	"testing"
)

// exportOf is what the export writes for a note read through the API: the Markdown with the origin of its project.
func (f *fx) exportOf(user string, project, id int64) string {
	f.t.Helper()
	n, e := f.GetNote(ctx, f.p(user), project, id)
	f.ok(e)
	n.Origin = f.NoteOrigin(n.ProjectID)
	return string(Markdown(n, ""))
}

func (f *fx) importFile(user string, project int64, md, path string) ImportResult {
	f.t.Helper()
	r, e := f.ImportMarkdown(ctx, f.p(user), project, MarkdownInput{Markdown: md, Path: path})
	f.ok(e)
	return r
}

func TestExportThenImportIsTheSameNote(t *testing.T) {
	f := newFx(t)
	fk := f.pid["fakture"]
	n := f.mk("ana", "fakture", Data)
	md := f.exportOf("ana", fk, n.ID)
	if !strings.Contains(md, `origin: "`+f.NoteOrigin(fk)+`"`) {
		t.Fatal(md)
	}
	// unchanged: skipped, and no second note
	if r := f.importFile("ana", fk, md, "1.md"); r.Action != "skip" || r.NoteID != n.ID {
		t.Fatalf("%+v", r)
	}
	if got := f.ids("ana", "fakture", ""); len(got) != 1 {
		t.Fatal("duplicate", got)
	}
	// the file is edited: an update of that note, still authored by ana (not an import author)
	edited := strings.Replace(md, "body", "edited body", 1)
	if r := f.importFile("ana", fk, edited, "1.md"); r.Action != "merge" || r.NoteID != n.ID {
		t.Fatalf("%+v", r)
	}
	g, e := f.GetNote(ctx, f.p("ana"), fk, n.ID)
	f.ok(e)
	if !strings.Contains(g.Body, "edited body") || g.Revision != n.Revision+1 || g.AuthorID != f.uid["ana"] {
		t.Fatalf("%+v", g)
	}
	// the same edited file again: nothing new (the receipt of the first import)
	if r := f.importFile("ana", fk, edited, "1.md"); r.Action != "skip" {
		t.Fatalf("%+v", r)
	}
	// the note is changed here after an export: the file is kept out, reported as a local edit
	stale := f.exportOf("ana", fk, n.ID)
	_, e = f.UpdateNote(ctx, f.p("ana"), fk, n.ID, g.Revision, NoteInput{Class: Data, Title: g.Title, Body: "changed here"})
	f.ok(e)
	if r := f.importFile("ana", fk, strings.Replace(stale, "edited body", "from the old file", 1), "1.md"); r.Action != "skip" || r.Reason != "local edit" {
		t.Fatalf("%+v", r)
	}
}

// A file never claims a note by its id alone: origin from another gateway or project, or a note the caller may not write.
func TestForgedOriginNeverClaimsANote(t *testing.T) {
	f := newFx(t)
	fk, eu := f.pid["fakture"], f.pid["eureko"]
	mine := f.mk("ana", "fakture", Data)
	theirs := f.mk("cene", "eureko", Data)
	priv := f.mk("fani", "fakture", Private) // another member's private note
	file := func(origin string, n Note) string {
		x := n
		x.Origin = origin
		return string(Markdown(x, ""))
	}
	withID := func(n Note, id int64) Note { n.ID = id; return n }
	// another gateway's origin: imported as a new note, like any foreign file
	if r := f.importFile("ana", fk, file("another-gateway:"+i64(fk), mine), "a.md"); r.Action != "create" || r.NoteID == mine.ID { // a foreign file with the id of a local note
		t.Fatalf("%+v", r)
	}
	// another project's origin (same gateway)
	if r := f.importFile("ana", fk, file(f.NoteOrigin(eu), withID(mine, mine.ID)), "b.md"); r.Action != "skip" && r.Action != "create" || r.NoteID == mine.ID {
		t.Fatalf("%+v", r)
	}
	// the right origin but the id of a note of another project: not claimed, a new note here
	forged := theirs
	if r := f.importFile("ana", fk, file(f.NoteOrigin(fk), forged), "c.md"); r.Action != "create" || r.NoteID == theirs.ID {
		t.Fatalf("%+v", r)
	}
	if g, _ := f.GetNote(ctx, f.p("cene"), eu, theirs.ID); g.Body != "body" || g.Revision != 1 {
		t.Fatal("another project's note changed", g)
	}
	// the right origin and the id of another member's private note that the caller cannot read or write
	pf := priv
	pf.Body = "overwrite!"
	if r := f.importFile("ana", fk, file(f.NoteOrigin(fk), pf), "d.md"); r.NoteID == priv.ID {
		t.Fatalf("a private note of another member was claimed: %+v", r)
	}
	if g, _ := f.GetNote(ctx, f.p("fani"), fk, priv.ID); g.Body == "overwrite!" {
		t.Fatal("private note overwritten")
	}
	// an id that never existed or was deleted
	gone := mine
	gone.ID = 99999
	if r := f.importFile("ana", fk, file(f.NoteOrigin(fk), gone), "e.md"); r.Action != "create" || r.NoteID == 99999 {
		t.Fatalf("%+v", r)
	}
	// the caller may edit the note anyway: a forged file with the right origin is the same as editing it (no gain)
	own := mine
	own.Body = "edited by the owner of the note"
	own.Revision = mine.Revision
	if r := f.importFile("ana", fk, file(f.NoteOrigin(fk), own), "f.md"); r.NoteID != mine.ID {
		t.Fatalf("%+v", r)
	}
}

func TestOwnExportOfTeamNotesRoundTrips(t *testing.T) {
	f := newSeg(t)
	n := f.put("fani", Data, "team rule")
	md := f.exportOf("fani", f.seg, n.ID)
	if !strings.Contains(md, f.NoteOrigin(f.seg)) {
		t.Fatal(md)
	}
	if r := f.importFile("fani", f.seg, md, "1.md"); r.Action != "skip" || r.NoteID != n.ID {
		t.Fatalf("%+v", r)
	}
	// the same file into a project (another origin project): a new note there
	if r := f.importFile("fani", f.pid["fakture"], md, "1.md"); r.Action != "create" {
		t.Fatalf("%+v", r)
	}
	// a team member who is not admin cannot write the segment: the file is not claimed (and the import is forbidden anyway)
	if _, e := f.ImportMarkdown(ctx, f.p("ana"), f.seg, MarkdownInput{Markdown: md, Path: "1.md"}); e == nil {
		t.Fatal("a reader imported into the team notes")
	}
}

// An export from before this field (no origin) imports as it always did.
func TestOlderExportWithoutOriginImportsAsBefore(t *testing.T) {
	f := newFx(t)
	fk := f.pid["fakture"]
	n := f.mk("ana", "fakture", Data)
	g, _ := f.GetNote(ctx, f.p("ana"), fk, n.ID)
	if r := f.importFile("ana", fk, string(Markdown(g, "")), "1.md"); r.Action != "create" || r.NoteID == n.ID {
		t.Fatalf("%+v", r)
	}
}
