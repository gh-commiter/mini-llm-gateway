package core

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Fixture roles: ana developer of fakture; cene developer of eureko (reads fakture structure through the grant
// fakture->eureko); dana developer of import. A note per project carries the word "needle".
func needleFx(t *testing.T) (*fx, map[string]Note) {
	f := newFx(t)
	n := map[string]Note{}
	for _, c := range []struct{ user, project string }{{"ana", "fakture"}, {"cene", "eureko"}, {"dana", "import"}} {
		f.clock = f.clock.Add(time.Second)
		keySeq++
		note, _, e := f.CreateNote(ctx, f.p(c.user), f.pid[c.project], fmt.Sprint("nk", keySeq), NoteInput{Class: Structure, Title: "needle " + c.project, Body: "x"})
		f.ok(e)
		n[c.project] = note
	}
	return f, n
}

func (f *fx) needle(user, project string) []SearchHit {
	f.t.Helper()
	pg, e := f.SearchNotes(ctx, f.p(user), f.pid[project], "needle")
	f.ok(e)
	return pg.Hits
}

func TestDependsAdmin(t *testing.T) {
	f := newFx(t)
	a, b := f.pid["fakture"], f.pid["eureko"]
	is(t, f.AddDepends(ctx, a, a), ErrInvalid{"depends: a project cannot depend on itself"})
	f.ok(f.AddDepends(ctx, a, b))
	f.ok(f.AddDepends(ctx, a, b)) // twice: a no-op
	f.ok(f.AddDepends(ctx, b, a)) // a cycle is allowed
	dep, used, e := f.AdminListDepends(ctx, a)
	f.ok(e)
	if len(dep) != 1 || dep[0].ID != b || len(used) != 1 || used[0].ID != b {
		t.Fatal(dep, used)
	}
	is(t, f.AddDepends(ctx, a, f.seg0()), ErrNotFound)
	is(t, f.AddDepends(ctx, 99999, a), ErrNotFound)
	is(t, f.RemoveDepends(ctx, a, 99999), ErrNotFound)
	// the cap
	for i := 0; i < maxDepends; i++ {
		p, e := f.CreateProject(ctx, fmt.Sprint("dep-p", i), "x")
		f.ok(e)
		if i < maxDepends-1 {
			f.ok(f.AddDepends(ctx, a, p.ID))
		} else {
			is(t, f.AddDepends(ctx, a, p.ID), ErrInvalid{"depends: at most 16"})
		}
	}
	// archived: add refused, remove allowed
	f.ok(f.SetProjectArchived(ctx, b, true))
	is(t, f.AddDepends(ctx, f.pid["import"], b), ErrArchived)
	f.ok(f.RemoveDepends(ctx, a, b))
	f.ok(f.SetProjectArchived(ctx, b, false))
}

func (f *fx) seg0() int64 { return segmentID(1) }

func TestDependsSegmentNeverASide(t *testing.T) {
	f := newSeg(t)
	a := f.pid["fakture"]
	is(t, f.AddDepends(ctx, a, f.seg), ErrNotFound)
	is(t, f.AddDepends(ctx, f.seg, a), ErrNotFound)
	_, _, e := f.AdminListDepends(ctx, f.seg)
	is(t, e, ErrNotFound)
	_, e = f.ListDepends(ctx, f.p("fani"), f.seg)
	is(t, e, ErrNotFound)
	for _, q := range []string{
		fmt.Sprintf(`INSERT INTO project_depends(project_id,depends_on) VALUES(%d,%d)`, a, f.seg),
		fmt.Sprintf(`INSERT INTO project_depends(project_id,depends_on) VALUES(%d,%d)`, f.seg, a),
	} {
		if err := f.write(ctx, func(tx *sql.Tx) error { _, e := tx.Exec(q); return e }); err == nil {
			t.Fatal("accepted:", q)
		}
	}
	f.ok(f.AddDepends(ctx, a, f.pid["eureko"]))
	if err := f.write(ctx, func(tx *sql.Tx) error {
		_, e := tx.Exec(fmt.Sprintf(`UPDATE project_depends SET depends_on=%d`, f.seg))
		return e
	}); err == nil {
		t.Fatal("update to a segment accepted")
	}
}

// A dependency is a search hint, never a permission: each source only as far as the caller could read it directly.
func TestDependsSearchAndRead(t *testing.T) {
	f, n := needleFx(t)
	fk, imp := f.pid["fakture"], f.pid["import"]
	f.ok(f.AddDepends(ctx, fk, imp))
	// ana has no access to import: nothing, no label, same hits, and note read refuses like today.
	before := f.needle("ana", "fakture")
	if len(before) != 1 || before[0].From != "" {
		t.Fatal(before)
	}
	_, e := f.GetNote(ctx, f.p("ana"), fk, n["import"].ID)
	is(t, e, ErrNotFound)
	// Archived or not, still nothing without access.
	f.ok(f.SetProjectArchived(ctx, imp, true))
	if h := f.needle("ana", "fakture"); len(h) != 1 {
		t.Fatal(h)
	}
	f.ok(f.SetProjectArchived(ctx, imp, false))
	// With read access of her own she gets the hit, labelled, the own project first (the import note is newer).
	f.ok(f.SetMember(ctx, imp, f.uid["ana"], Reader))
	h := f.needle("ana", "fakture")
	if len(h) != 2 || h[0].Project != fk || h[1].Project != imp || h[1].From != "project:import" || h[0].From != "" {
		t.Fatal(h)
	}
	g, e := f.GetNote(ctx, f.p("ana"), fk, n["import"].ID)
	f.ok(e)
	if g.ID != n["import"].ID {
		t.Fatal(g)
	}
	// An archived dependency is still searched.
	f.ok(f.SetProjectArchived(ctx, imp, true))
	if h = f.needle("ana", "fakture"); len(h) != 2 {
		t.Fatal(h)
	}
	f.ok(f.SetProjectArchived(ctx, imp, false))
	// Removed: gone on the next call, search and read.
	f.ok(f.RemoveDepends(ctx, fk, imp))
	if h = f.needle("ana", "fakture"); len(h) != 1 {
		t.Fatal(h)
	}
	_, e = f.GetNote(ctx, f.p("ana"), fk, n["import"].ID)
	is(t, e, ErrNotFound)
}

func TestDependsGrantOnlyPrivateAndOneLevel(t *testing.T) {
	f, n := needleFx(t)
	fk, eu, imp := f.pid["fakture"], f.pid["eureko"], f.pid["import"]
	// cene (eureko) depends-reads fakture only through the grant: structure yes, data no, private never.
	for _, c := range []Class{Data, Private} {
		keySeq++
		_, _, e := f.CreateNote(ctx, f.p("ana"), fk, fmt.Sprint("g", keySeq), NoteInput{Class: c, Title: "needle " + string(c), Body: "x"})
		f.ok(e)
	}
	f.ok(f.AddDepends(ctx, eu, fk))
	h := f.needle("cene", "eureko")
	if len(h) != 2 { // her own eureko note and fakture's structure note
		t.Fatal(h)
	}
	for _, x := range h {
		if x.Class != Structure {
			t.Fatal("class beyond the grant", x)
		}
	}
	// Private notes of the caller in the dependency never appear.
	f.ok(f.SetMember(ctx, fk, f.uid["cene"], Developer))
	keySeq++
	_, _, e := f.CreateNote(ctx, f.p("cene"), fk, fmt.Sprint("p", keySeq), NoteInput{Class: Private, Title: "needle mine", Body: "x"})
	f.ok(e)
	for _, x := range f.needle("cene", "eureko") {
		if x.Class == Private {
			t.Fatal("private through a dependency", x)
		}
	}
	// One level: eureko -> fakture -> import: import is not pulled into eureko's search.
	f.ok(f.SetMember(ctx, imp, f.uid["cene"], Reader))
	f.ok(f.AddDepends(ctx, fk, imp))
	for _, x := range f.needle("cene", "eureko") {
		if x.Project == imp {
			t.Fatal("two levels followed", x)
		}
	}
	_ = n
}

// The run channel: a dependency adds nothing beyond an owner grant from it to the run's project.
func TestDependsRunNeedsAGrant(t *testing.T) {
	f, n := needleFx(t)
	fk, imp := f.pid["fakture"], f.pid["import"]
	f.ok(f.SetMember(ctx, imp, f.uid["ana"], Developer)) // the run's user reads import herself
	f.ok(f.AddDepends(ctx, fk, imp))
	p := f.p("ana")
	cap := RunCap{UserID: p.UserID, DeviceID: p.DeviceID, ProjectID: fk, Epoch: f.epoch.Load()}
	rs, e := f.RunSearchNotes(ctx, cap, true, "needle")
	f.ok(e)
	for _, h := range rs.Hits {
		if h.Project == imp {
			t.Fatal("a run read a dependency without a grant", h)
		}
	}
	_, e = f.RunGetNote(ctx, cap, true, n["import"].ID)
	is(t, e, ErrNotFound)
	// With the owner's grant import -> fakture the run reads it, and the hit is labelled and ordered.
	f.ok(f.AddGrant(ctx, imp, fk, Structure))
	rs, e = f.RunSearchNotes(ctx, cap, true, "needle")
	f.ok(e)
	if len(rs.Hits) != 2 || rs.Hits[0].Project != fk || rs.Hits[1].Project != imp || rs.Hits[1].From != "project:import" {
		t.Fatal(rs.Hits)
	}
	// Without the dependency the grant still works, unlabelled.
	f.ok(f.RemoveDepends(ctx, fk, imp))
	if rs, e = f.RunSearchNotes(ctx, cap, true, "needle"); e != nil || len(rs.Hits) != 2 || rs.Hits[1].From != "" {
		t.Fatal(rs.Hits, e)
	}
}

func TestDependsMemberListHidesUnreadable(t *testing.T) {
	f := newFx(t)
	fk, eu, imp := f.pid["fakture"], f.pid["eureko"], f.pid["import"]
	f.ok(f.AddDepends(ctx, fk, eu))
	f.ok(f.AddDepends(ctx, fk, imp))
	got, e := f.ListDepends(ctx, f.p("ana"), fk)
	f.ok(e)
	if len(got) != 0 {
		t.Fatal("unreadable dependencies listed", got)
	}
	f.ok(f.SetMember(ctx, imp, f.uid["ana"], Reader))
	got, e = f.ListDepends(ctx, f.p("ana"), fk)
	f.ok(e)
	if len(got) != 2 { // import, and eureko through the grant eureko->import
		t.Fatal(got)
	}
	_, e = f.ListDepends(ctx, f.p("cene"), fk) // cene reads fakture through a grant: visible
	f.ok(e)
	_, e = f.ListDepends(ctx, f.p("dana"), fk)
	is(t, e, ErrNotFound)
}

func TestDependsMigration16AndRestore(t *testing.T) {
	s := openT(t, t.TempDir())
	if err := checkSchemaObjects(ctx, s.DB(), len(migrations)); err != nil {
		t.Fatal(err)
	}
	bak := filepath.Join(t.TempDir(), "d.db")
	if err := s.Backup(ctx, bak); err != nil {
		t.Fatal(err)
	}
	s.Close()
	db, _ := sql.Open("sqlite", "file:"+bak)
	if _, err := db.Exec(`DROP TRIGGER segment_no_depends`); err != nil {
		t.Fatal(err)
	}
	db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	db.Close()
	if err := Restore(t.TempDir(), bak); err == nil || !strings.Contains(err.Error(), "missing trigger") {
		t.Fatal(err)
	}
}
