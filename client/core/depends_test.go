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
	is(t, f.AddDepends(ctx, a, a), ErrRefusal{"a project cannot depend on itself"})
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
			is(t, f.AddDepends(ctx, a, p.ID), ErrRefusal{"a project can have at most 16 dependencies (projects and teams together)"})
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

func TestDependsBundle(t *testing.T) {
	source, _ := bundleFixture(t) // team "Team" holds fakture and eureko; import is outside it
	fk, eu, imp := source.pid["fakture"], source.pid["eureko"], source.pid["import"]
	source.ok(source.AddDepends(ctx, fk, eu))
	source.ok(source.AddDepends(ctx, fk, imp)) // outside the bundle: not exported
	file := filepath.Join(t.TempDir(), "d.zip")
	source.ok(source.ExportBundle(ctx, BundleExport{Team: "Team", Path: file}))
	b, e := readBundle(file)
	source.ok(e)
	got := map[string][]string{}
	for _, p := range b.manifest.Projects {
		got[p.Slug] = p.DependsOn
	}
	if len(got["fakture"]) != 1 || got["fakture"][0] != "eureko" || len(got["eureko"]) != 0 {
		t.Fatal(got)
	}
	target := openT(t, t.TempDir())
	defer target.Close()
	dry, e := target.ImportBundle(ctx, BundleImport{Path: file, Source: "s", DryRun: true})
	source.ok(e)
	if !strings.Contains(strings.Join(dry.Lines, "\n"), "create depends fakture eureko") || countTable(t, target, "project_depends") != 0 {
		t.Fatal(dry)
	}
	_, e = target.ImportBundle(ctx, BundleImport{Path: file, Source: "s"})
	source.ok(e)
	if countTable(t, target, "project_depends") != 1 {
		t.Fatal("relation not imported")
	}
	rep, e := target.ImportBundle(ctx, BundleImport{Path: file, Source: "s"})
	source.ok(e)
	if !strings.Contains(strings.Join(rep.Lines, "\n"), "skip depends fakture eureko") {
		t.Fatal(rep)
	}
	// A name outside the bundle that the target does not have is skipped with a line, never created.
	b2, e := readBundle(file)
	source.ok(e)
	for i := range b2.manifest.Projects {
		if b2.manifest.Projects[i].Slug == "eureko" {
			b2.manifest.Projects[i].DependsOn = []string{"nowhere"}
		}
	}
	t2 := openT(t, t.TempDir())
	defer t2.Close()
	rep, e = t2.ImportBundle(ctx, BundleImport{Path: rewriteZip(t, b2, nil), Source: "s", DryRun: true})
	source.ok(e)
	if !strings.Contains(strings.Join(rep.Lines, "\n"), "skip depends eureko nowhere: unknown project") {
		t.Fatal(rep)
	}
	// More than 16 is refused; self and duplicates too.
	for _, bad := range [][]string{make([]string, 17), {"fakture"}, {"eureko", "eureko"}} {
		b3, _ := readBundle(file)
		for i := range b3.manifest.Projects {
			if b3.manifest.Projects[i].Slug == "fakture" {
				d := bad
				if len(bad) == 17 {
					for j := range d {
						d[j] = fmt.Sprint("proj-", j)
					}
				}
				b3.manifest.Projects[i].DependsOn = d
			}
		}
		if _, e = t2.ImportBundle(ctx, BundleImport{Path: rewriteZip(t, b3, nil), Source: "s", DryRun: true}); e == nil {
			t.Fatal("accepted", bad)
		}
	}
}

func TestSegmentCheckRefusesDependsRows(t *testing.T) {
	f := newSeg(t)
	f.ok(f.write(ctx, func(tx *sql.Tx) error {
		if _, e := tx.Exec(`DROP TRIGGER segment_no_depends`); e != nil {
			return e
		}
		_, e := tx.Exec(`INSERT INTO project_depends(project_id,depends_on) VALUES(?,?)`, f.pid["fakture"], f.seg)
		return e
	}))
	if err := f.read(ctx, checkSegments); err == nil {
		t.Fatal("a dependency on a segment passed the check")
	}
}

// The cap in a dry run: the same lines as the real import.
func TestDependsBundleCapDryRunMatchesImport(t *testing.T) {
	source := newFx(t)
	team, e := source.CreateTeam(ctx, "Team")
	source.ok(e)
	var slugs []string
	for i := 0; i < 20; i++ {
		slug := fmt.Sprint("cap-p", i)
		source.project(slug)
		source.ok(source.AddTeamProject(ctx, team.ID, source.pid[slug]))
		slugs = append(slugs, slug)
	}
	for i := 1; i <= 10; i++ {
		source.ok(source.AddDepends(ctx, source.pid[slugs[0]], source.pid[slugs[i]]))
	}
	file := filepath.Join(t.TempDir(), "cap.zip")
	source.ok(source.ExportBundle(ctx, BundleExport{Team: "Team", Path: file}))
	b, e := readBundle(file)
	source.ok(e)
	for i := range b.manifest.Projects { // the bundle names 10 further dependencies for cap-p0
		if b.manifest.Projects[i].Slug == "cap-p0" {
			b.manifest.Projects[i].DependsOn = slugs[10:19] // 9
			b.manifest.Projects[i].DependsOn = append(b.manifest.Projects[i].DependsOn, slugs[19])
		}
	}
	// the target already has cap-p0 with 10 other dependencies: build it from the first export
	target := openT(t, t.TempDir())
	defer target.Close()
	_, e = target.ImportBundle(ctx, BundleImport{Path: file, Source: "s"})
	source.ok(e)
	more := rewriteZip(t, b, nil)
	lines := func(dry bool) []string {
		rep, e := target.ImportBundle(ctx, BundleImport{Path: more, Source: "s", DryRun: dry})
		source.ok(e)
		var out []string
		for _, l := range rep.Lines {
			if strings.Contains(l, "depends") {
				out = append(out, l)
			}
		}
		return out
	}
	dry, real := lines(true), lines(false)
	if strings.Join(dry, "\n") != strings.Join(real, "\n") {
		t.Fatalf("dry run differs:\n%v\n%v", dry, real)
	}
	capped := 0
	for _, l := range real {
		if strings.Contains(l, "at most 16") {
			capped++
		}
	}
	if capped != 3 { // 10 present (cap-p1..10), 6 created (11..16) = 16, the other 3 are skipped
		t.Fatal(capped, real)
	}
}

func TestBundlePreflightNestedLimits(t *testing.T) {
	for _, key := range []string{"depends_on", "remotes"} {
		n := maxDepends + 1
		if key == "remotes" {
			n = maxRemotes + 1
		}
		items := make([]string, n)
		for i := range items {
			items[i] = `"x"`
		}
		data := `{"format":1,"projects":[{"slug":"a-b","name":"n","` + key + `":[` + strings.Join(items, ",") + `]}]}`
		if err := preflightManifest([]byte(data)); err == nil {
			t.Fatal(key, "oversized nested array passed the preflight")
		}
	}
	ok := `{"format":1,"projects":[{"slug":"a-b","name":"n","depends_on":["x"],"remotes":["y"]}]}`
	if err := preflightManifest([]byte(ok)); err != nil {
		t.Fatal(err)
	}
}

// A dependency does not carry the dependency's team notes along: only a roster member of that team reads them.
func TestDependsDoesNotBringTheDependencysSegment(t *testing.T) {
	f := newSeg(t)
	// import (project of team zeta) has the segment f.zseg; ana depends-reads import as a reader but is not on zeta's roster.
	keySeq++
	f.clock = f.clock.Add(time.Second)
	zn, _, e := f.CreateNote(ctx, f.p("dana"), f.zseg, fmt.Sprint("z", keySeq), NoteInput{Class: Data, Title: "needle zeta team", Body: "x"})
	f.ok(e)
	f.ok(f.SetMember(ctx, f.pid["import"], f.uid["ana"], Reader))
	f.ok(f.AddDepends(ctx, f.pid["fakture"], f.pid["import"]))
	for _, h := range f.hits("ana", f.pid["fakture"], "needle") {
		if h.Project == f.zseg {
			t.Fatal("a team's notes came along through a dependency", h)
		}
	}
	_, e = f.GetNote(ctx, f.p("ana"), f.pid["fakture"], zn.ID)
	is(t, e, ErrNotFound)
}

// The caller's own private note in a dependency is never read through the dependency (the rule is: never private).
func TestDependsPrivateNoteByIDRefused(t *testing.T) {
	f := newFx(t)
	fk, imp := f.pid["fakture"], f.pid["import"]
	f.ok(f.SetMember(ctx, imp, f.uid["ana"], Developer))
	f.ok(f.AddDepends(ctx, fk, imp))
	keySeq++
	mine, _, e := f.CreateNote(ctx, f.p("ana"), imp, fmt.Sprint("pv", keySeq), NoteInput{Class: Private, Title: "needle private", Body: "x"})
	f.ok(e)
	_, e = f.GetNote(ctx, f.p("ana"), fk, mine.ID)
	is(t, e, ErrNotFound)
	if _, e = f.GetNote(ctx, f.p("ana"), imp, mine.ID); e != nil { // through its own project she may
		t.Fatal(e)
	}
}

// An archived project confers nothing: a grant whose READER project is archived no longer gives its members access to the source.
func TestGrantThroughAnArchivedReaderConfersNothing(t *testing.T) {
	f := newFx(t)
	fk, eu := f.pid["fakture"], f.pid["eureko"]
	n := f.mk("ana", "fakture", Structure)
	f.ok(f.AddDepends(ctx, eu, fk))
	cene := f.p("cene") // member of eureko only: eureko -> fakture is her only path
	reads := func() (list, search, manifest, depends bool) {
		pg, e := f.ListNotes(ctx, cene, fk, "", "", 10)
		list = e == nil && len(pg.Notes) == 1 && pg.Notes[0].ID == n.ID
		if sp, e := f.SearchNotes(ctx, cene, fk, "body"); e == nil {
			search = len(sp.Hits) > 0
		}
		if m, e := f.Manifest(ctx, cene, eu); e == nil {
			for _, x := range m.Notes {
				manifest = manifest || x.Source == fk
			}
		}
		if ds, e := f.ListDepends(ctx, cene, eu); e == nil {
			depends = len(ds) == 1
		}
		return
	}
	if l, s, m, d := reads(); !l || !s || !m || !d {
		t.Fatal("active reader: list/search/manifest/depends", l, s, m, d)
	}
	f.ok(f.SetProjectArchived(ctx, eu, true))
	if l, s, m, d := reads(); l || s || m || d {
		t.Fatal("archived reader still confers access: list/search/manifest/depends", l, s, m, d)
	}
	if _, err := f.GetNote(ctx, cene, fk, n.ID); err == nil {
		t.Fatal("note readable through an archived reader")
	}
	f.ok(f.SetProjectArchived(ctx, eu, false))
	if l, _, _, _ := reads(); !l {
		t.Fatal("unarchived reader lost the grant")
	}
}

// The other grant joins (a run's reads, the project list, a team's project list) follow the same rule, each with its own assertion.
func TestArchivedReaderGrantInRunScopeProjectListAndTeamProjects(t *testing.T) {
	f := newFx(t)
	fk, eu := f.pid["fakture"], f.pid["eureko"]
	n := f.mk("ana", "fakture", Structure)
	team, err := f.CreateTeam(ctx, "core")
	f.ok(err)
	f.ok(f.AddTeamMember(ctx, team.ID, f.uid["cene"]))
	f.ok(f.AddTeamProject(ctx, team.ID, fk))
	cene := f.p("cene")
	run := func() bool { // a run of eureko (cap issued before the archive is reused: only the reader's state changes)
		hits, e := f.RunSearchNotes(ctx, RunCap{UserID: cene.UserID, DeviceID: cene.DeviceID, ProjectID: eu, Epoch: f.epoch.Load()}, true, "body")
		if e != nil {
			return false
		}
		for _, h := range hits.Hits {
			if h.Project == fk {
				return true
			}
		}
		return false
	}
	listed := func(includeArchived bool) bool {
		ps, e := f.ListProjects(ctx, cene, includeArchived)
		f.ok(e)
		for _, p := range ps {
			if p.ID == fk {
				return true
			}
		}
		return false
	}
	teamListed := func() bool {
		td, e := f.GetTeam(ctx, cene, "core")
		f.ok(e)
		for _, p := range td.Projects {
			if p.ID == fk {
				return true
			}
		}
		return false
	}
	_ = n
	if !run() || !listed(false) || !teamListed() {
		t.Fatal("active reader: run/list/team", run(), listed(false), teamListed())
	}
	f.ok(f.SetProjectArchived(ctx, eu, true))
	if run() {
		t.Error("runScope still reads through the grant of an archived reader")
	}
	if listed(false) || listed(true) {
		t.Error("ListProjects still lists the source through an archived reader")
	}
	if teamListed() {
		t.Error("teamProjects still lists the source through an archived reader")
	}
	f.ok(f.SetProjectArchived(ctx, eu, false))
	if !run() || !listed(false) || !teamListed() {
		t.Error("unarchived reader lost the grant: run/list/team", run(), listed(false), teamListed())
	}
}
