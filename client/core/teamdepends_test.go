package core

import (
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
