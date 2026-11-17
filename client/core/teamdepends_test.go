package core

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// teamDepFx: project eureko (members ana and bor, readers) depends on team acme. ana is on acme's roster, bor is not.
// fani writes a team note about "deploy".
func teamDepFx(t *testing.T) (*segTeams, Note) {
	f := newSeg(t)
	f.ok(f.SetMember(ctx, f.pid["eureko"], f.uid["ana"], Reader))
	f.ok(f.SetMember(ctx, f.pid["eureko"], f.uid["bor"], Reader))
	n := f.put("fani", Structure, "rollout")
	f.ok(f.AddTeamDepends(ctx, f.pid["eureko"], f.acme))
	return f, n
}

func TestTeamDependsSearchAndRead(t *testing.T) {
	f, n := teamDepFx(t)
	eu := f.pid["eureko"]
	// On the roster: the team note, labelled with the team.
	h := f.hits("ana", eu, "deploy")
	if len(h) != 1 || h[0].From != "team:acme" || h[0].ID != n.ID {
		t.Fatal(h)
	}
	g, e := f.GetNote(ctx, f.p("ana"), eu, n.ID)
	f.ok(e)
	if g.ID != n.ID {
		t.Fatal(g)
	}
	// Not on the roster (bor reads eureko): no hit, no label, and the note by id is not found through the project.
	if h = f.hits("bor", eu, "deploy"); len(h) != 0 {
		t.Fatal("a caller outside the roster got a team hit", h)
	}
	_, e = f.GetNote(ctx, f.p("bor"), eu, n.ID)
	is(t, e, ErrNotFound)
	// The member's own count of the project's notes does not differ between a team dependency and none.
	f.ok(f.RemoveTeamDepends(ctx, eu, f.acme))
	if h = f.hits("ana", eu, "deploy"); len(h) != 0 {
		t.Fatal(h)
	}
	_, e = f.GetNote(ctx, f.p("ana"), eu, n.ID)
	is(t, e, ErrNotFound)
}

func TestTeamDependsRunGetsNothing(t *testing.T) {
	f, n := teamDepFx(t)
	eu := f.pid["eureko"]
	p := f.p("ana")
	cap := RunCap{UserID: p.UserID, DeviceID: p.DeviceID, ProjectID: eu, Epoch: f.epoch.Load()}
	rs, e := f.RunSearchNotes(ctx, cap, true, "deploy")
	f.ok(e)
	if len(rs.Hits) != 0 {
		t.Fatal("a run read a team through a dependency", rs.Hits)
	}
	_, e = f.RunGetNote(ctx, cap, true, n.ID)
	is(t, e, ErrNotFound)
}

func TestTeamDependsArchivedTeamGivesNothing(t *testing.T) {
	f, _ := teamDepFx(t)
	eu := f.pid["eureko"]
	f.ok(f.SetTeamArchived(ctx, f.acme, true))
	if h := f.hits("ana", eu, "deploy"); len(h) != 0 {
		t.Fatal(h)
	}
	f.ok(f.SetTeamArchived(ctx, f.acme, false))
	if h := f.hits("ana", eu, "deploy"); len(h) != 1 {
		t.Fatal(h)
	}
}

func TestTeamDependsAdminLimits(t *testing.T) {
	f, _ := teamDepFx(t)
	eu := f.pid["eureko"]
	// Adding twice is a no-op; list shows it to the owner (also an archived team) and the roster member, not to bor.
	f.ok(f.AddTeamDepends(ctx, eu, f.acme))
	ts, e := f.AdminListTeamDepends(ctx, eu)
	if f.ok(e); len(ts) != 1 || ts[0].ID != f.acme {
		t.Fatal(ts)
	}
	if ts, e = f.ListTeamDepends(ctx, f.p("ana"), eu); e != nil || len(ts) != 1 {
		t.Fatal(ts, e)
	}
	if ts, e = f.ListTeamDepends(ctx, f.p("bor"), eu); e != nil || len(ts) != 0 {
		t.Fatal("a name the caller may not know was shown", ts, e)
	}
	// The project's own team is refused as redundant; an archived team is refused; an unknown team is not found.
	var ref ErrRefusal
	if e = f.AddTeamDepends(ctx, f.pid["fakture"], f.acme); !errors.As(e, &ref) || !strings.Contains(ref.Msg, "already holds") {
		t.Fatal(e)
	}
	f.ok(f.SetTeamArchived(ctx, f.zeta, true))
	if e = f.AddTeamDepends(ctx, eu, f.zeta); !errors.As(e, &ref) || !strings.Contains(ref.Msg, "archived") {
		t.Fatal(e)
	}
	is(t, f.AddTeamDepends(ctx, eu, 999), ErrNotFound)
	is(t, f.AddTeamDepends(ctx, 999, f.acme), ErrNotFound)
	// The cap of 16 counts both kinds: 15 projects and then the team make 16; one more of either kind is refused.
	for i := 0; i < 15; i++ {
		p, e := f.CreateProject(ctx, fmt.Sprintf("dep-%d", i), "dep")
		f.ok(e)
		f.ok(f.AddDepends(ctx, eu, p.ID))
	}
	extra, e := f.CreateProject(ctx, "dep-extra", "dep")
	f.ok(e)
	if e = f.AddDepends(ctx, eu, extra.ID); e == nil {
		t.Fatal("a 17th dependency was accepted (team counted as one)")
	}
	f.ok(f.AddTeamDepends(ctx, eu, f.acme)) // idempotent at the cap
	f.ok(f.RemoveTeamDepends(ctx, eu, f.acme))
	f.ok(f.AddDepends(ctx, eu, extra.ID)) // 16 projects
	f.ok(f.SetTeamArchived(ctx, f.zeta, false))
	if e = f.AddTeamDepends(ctx, eu, f.zeta); e == nil {
		t.Fatal("a team dependency above the cap was accepted")
	}
	// Remove: a second removal is not found.
	f.ok(f.RemoveDepends(ctx, eu, extra.ID))
	f.ok(f.AddTeamDepends(ctx, eu, f.zeta))
	f.ok(f.RemoveTeamDepends(ctx, eu, f.zeta))
	is(t, f.RemoveTeamDepends(ctx, eu, f.zeta), ErrNotFound)
}

func TestTeamDependsSegmentNeverHasOne(t *testing.T) {
	f, _ := teamDepFx(t)
	is(t, f.AddTeamDepends(ctx, f.seg, f.zeta), ErrNotFound)
	err := f.write(ctx, func(tx *sql.Tx) error {
		_, e := tx.Exec(`INSERT INTO project_team_depends(project_id,team_id) VALUES(?,?)`, f.seg, f.zeta)
		return e
	})
	if err == nil || !strings.Contains(err.Error(), "segment with a dependency") {
		t.Fatal(err)
	}
	// The self-check catches a row that got in without the trigger.
	f.ok(f.write(ctx, func(tx *sql.Tx) error {
		if _, e := tx.Exec(`DROP TRIGGER segment_no_team_depends`); e != nil {
			return e
		}
		_, e := tx.Exec(`INSERT INTO project_team_depends(project_id,team_id) VALUES(?,?)`, f.seg, f.zeta)
		return e
	}))
	if err := f.read(ctx, checkSegments); err == nil {
		t.Fatal("a segment with a team dependency passed the check")
	}
}

func TestTeamDependsRestoreNeedsTheTriggers(t *testing.T) {
	s := openT(t, t.TempDir())
	bak := filepath.Join(t.TempDir(), "d.db")
	if err := s.Backup(ctx, bak); err != nil {
		t.Fatal(err)
	}
	s.Close()
	db, _ := sql.Open("sqlite", "file:"+bak)
	if _, err := db.Exec(`DROP TRIGGER segment_no_team_depends_upd`); err != nil {
		t.Fatal(err)
	}
	db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	db.Close()
	if err := Restore(t.TempDir(), bak); err == nil || !strings.Contains(err.Error(), "missing trigger") {
		t.Fatal(err)
	}
}

// One cap for both kinds, also in a bundle import (dry run and real): a project with 16 team dependencies gets no project dependency.
func TestTeamDependsCountInBundleImportCap(t *testing.T) {
	source := newFx(t)
	team, e := source.CreateTeam(ctx, "Team")
	source.ok(e)
	for _, s := range []string{"cap-a", "cap-b", "cap-c"} {
		source.project(s)
		source.ok(source.AddTeamProject(ctx, team.ID, source.pid[s]))
	}
	file := filepath.Join(t.TempDir(), "cap.zip")
	source.ok(source.ExportBundle(ctx, BundleExport{Team: "Team", Path: file}))
	b, e := readBundle(file)
	source.ok(e)
	for i := range b.manifest.Projects {
		if b.manifest.Projects[i].Slug == "cap-a" {
			b.manifest.Projects[i].DependsOn = []string{"cap-b", "cap-c"}
		}
	}
	target := openT(t, t.TempDir())
	defer target.Close()
	_, e = target.ImportBundle(ctx, BundleImport{Path: file, Source: "s"})
	source.ok(e)
	ps, e := target.AdminListProjects(ctx)
	source.ok(e)
	var a int64
	for _, p := range ps {
		if p.Slug == "cap-a" {
			a = p.ID
		}
	}
	for i := 0; i < 16; i++ {
		tm, e := target.CreateTeam(ctx, fmt.Sprint("dep-team-", i))
		source.ok(e)
		source.ok(target.AddTeamDepends(ctx, a, tm.ID))
	}
	more := rewriteZip(t, b, nil)
	for _, dry := range []bool{true, false} {
		rep, e := target.ImportBundle(ctx, BundleImport{Path: more, Source: "s", DryRun: dry})
		source.ok(e)
		capped := 0
		for _, l := range rep.Lines {
			if strings.Contains(l, "create depends") {
				t.Fatalf("dry=%v: a project dependency was added above the cap: %v", dry, rep.Lines)
			}
			if strings.Contains(l, "depends cap-a") && strings.Contains(l, "at most 16") {
				capped++
			}
		}
		if capped != 2 {
			t.Fatalf("dry=%v: %v", dry, rep.Lines)
		}
	}
	if n, _, e := target.AdminListDepends(ctx, a); e != nil || len(n) != 0 {
		t.Fatal(n, e)
	}
}

// Held checks of AddTeamDepends, the member list, checkSegments and the update trigger.
func TestTeamDependsArchivedProjectRefused(t *testing.T) {
	f, _ := teamDepFx(t)
	eu := f.pid["eureko"]
	f.ok(f.RemoveTeamDepends(ctx, eu, f.acme))
	f.ok(f.SetProjectArchived(ctx, eu, true))
	is(t, f.AddTeamDepends(ctx, eu, f.acme), ErrArchived)
}

func TestTeamDependsMemberListNeedsAProjectRole(t *testing.T) {
	f, _ := teamDepFx(t)
	// fani is on acme's roster but has no role in eureko: the list answers like for any project she cannot see.
	if _, e := f.ListTeamDepends(ctx, f.p("fani"), f.pid["eureko"]); e == nil {
		t.Fatal("a user without a role in the project got its team dependencies")
	}
}

func TestTeamDependsCheckSegmentsRefusesAnUnknownTeam(t *testing.T) {
	f, _ := teamDepFx(t)
	// a restored file may come from a connection without foreign keys: write the row the way such a file would hold it
	bak := filepath.Join(t.TempDir(), "d.db")
	f.ok(f.Backup(ctx, bak))
	db, err := sql.Open("sqlite", "file:"+bak)
	f.ok(err)
	defer db.Close()
	if err := func() error {
		tx, e := db.Begin()
		if e != nil {
			return e
		}
		defer tx.Rollback()
		return checkSegments(tx)
	}(); err != nil {
		t.Fatal("the control: a consistent backup failed the check", err)
	}
	if _, err = db.Exec(`INSERT INTO project_team_depends(project_id,team_id) VALUES(?,?)`, f.pid["fakture"], 9999); err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	f.ok(err)
	defer tx.Rollback()
	if checkSegments(tx) == nil {
		t.Fatal("a dependency on a team that does not exist passed the check")
	}
}

func TestTeamDependsUpdateTriggerRefuses(t *testing.T) {
	f, _ := teamDepFx(t)
	err := f.write(ctx, func(tx *sql.Tx) error {
		_, e := tx.Exec(`UPDATE project_team_depends SET project_id=? WHERE team_id=?`, f.seg, f.acme)
		return e
	})
	if err == nil || !strings.Contains(err.Error(), "segment with a dependency") {
		t.Fatal(err)
	}
}

func TestBundleRefusesATeamDependsField(t *testing.T) {
	source, _ := bundleFixture(t)
	file := filepath.Join(t.TempDir(), "d.zip")
	source.ok(source.ExportBundle(ctx, BundleExport{Team: "Team", Path: file}))
	b, e := readBundle(file)
	source.ok(e)
	evil := rewriteZip(t, b, func(files map[string][]byte) {
		files["manifest.json"] = bytes.Replace(files["manifest.json"], []byte(`"slug":`), []byte(`"team_depends":["x"],"slug":`), 1)
	})
	if _, err := readBundle(evil); err == nil || !strings.Contains(err.Error(), "team_depends") {
		t.Fatal(err)
	}
}

// A backup whose guard trigger keeps its name but not its body is refused.
func TestRestoreComparesTriggerBodies(t *testing.T) {
	for _, trig := range []string{"segment_no_team_depends", "segment_no_depends", "segment_no_depends_upd", "segment_no_team_depends_upd"} {
		s := openT(t, t.TempDir())
		bak := filepath.Join(t.TempDir(), "d.db")
		if err := s.Backup(ctx, bak); err != nil {
			t.Fatal(err)
		}
		s.Close()
		db, _ := sql.Open("sqlite", "file:"+bak)
		table := "project_depends"
		if strings.Contains(trig, "team_depends") {
			table = "project_team_depends"
		}
		ev := "INSERT"
		if strings.HasSuffix(trig, "_upd") {
			ev = "UPDATE OF project_id"
		}
		for _, q := range []string{`DROP TRIGGER ` + trig, `CREATE TRIGGER ` + trig + ` BEFORE ` + ev + ` ON ` + table + ` WHEN 0 BEGIN SELECT 1; END`} {
			if _, err := db.Exec(q); err != nil {
				t.Fatal(trig, err)
			}
		}
		db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
		db.Close()
		if err := Restore(t.TempDir(), bak); err == nil || !strings.Contains(err.Error(), "differs") {
			t.Fatal(trig, err)
		}
	}
}
