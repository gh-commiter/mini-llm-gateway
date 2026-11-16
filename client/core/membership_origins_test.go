package core

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
)

func TestMembershipOrigins(t *testing.T) {
	for _, removal := range []string{"assignment", "member", "project", "archive"} {
		t.Run(removal, func(t *testing.T) {
			f := newFx(t)
			team, e := f.CreateTeam(ctx, "Engineering")
			f.ok(e)
			p, u := f.pid["fakture"], f.uid["eva"]
			check := func(e error) { t.Helper(); f.ok(e); f.ok(f.CheckMemberships(ctx)) }
			check(f.AddTeamMember(ctx, team.ID, u))
			check(f.AddTeamProject(ctx, team.ID, p))
			if e = f.SetTeamAssignment(ctx, team.ID, p, f.uid["dana"], Admin); e == nil {
				t.Fatal("nonmember assigned")
			}
			if e = f.SetTeamAssignment(ctx, team.ID, f.pid["import"], u, Admin); e == nil {
				t.Fatal("foreign project assigned")
			}
			if e = f.SetTeamAssignment(ctx, team.ID, p, u, "invalid"); e == nil {
				t.Fatal("invalid role")
			}
			check(f.SetTeamAssignment(ctx, team.ID, p, u, Admin))
			check(f.SetTeamAssignment(ctx, team.ID, p, u, Developer))
			pr, e := f.GetProject(ctx, f.p("eva"), "fakture")
			f.ok(e)
			if pr.Role != Developer {
				t.Fatal("team downgrade", pr)
			}
			check(f.SetTeamAssignment(ctx, team.ID, p, u, Admin))
			check(f.SetMember(ctx, p, u, Developer))
			pr, e = f.GetProject(ctx, f.p("eva"), "fakture")
			f.ok(e)
			if pr.Role != Admin {
				t.Fatal(pr)
			}
			ms, e := f.ListMembers(ctx, p)
			f.ok(e)
			for _, m := range ms {
				if m.UserID == u && len(m.Origins) != 2 {
					t.Fatal(m)
				}
			}
			epoch, e := f.Epoch(ctx)
			f.ok(e)
			cap := RunCap{RunID: "old", UserID: u, DeviceID: "dev-eva", ProjectID: p, Epoch: epoch}
			_, e = f.RunListNotes(ctx, cap, true, "", "", 50)
			f.ok(e)
			// Removing the direct origin preserves team access and the epoch.
			check(f.RemoveMember(ctx, p, u))
			if got, _ := f.Epoch(ctx); got != epoch {
				t.Fatal("surviving team access invalidated")
			}
			check(f.SetMember(ctx, p, u, Reader))
			switch removal {
			case "assignment":
				check(f.RemoveTeamAssignment(ctx, team.ID, p, u))
			case "member":
				check(f.RemoveTeamMember(ctx, team.ID, u))
			case "project":
				check(f.RemoveTeamProject(ctx, team.ID, p))
			case "archive":
				check(f.SetTeamArchived(ctx, team.ID, true))
			}
			pr, e = f.GetProject(ctx, f.p("eva"), "fakture")
			f.ok(e)
			if pr.Role != Reader {
				t.Fatal("direct origin lost", pr)
			}
			if got, _ := f.Epoch(ctx); got != epoch+1 {
				t.Fatal("narrowing did not bump once", got, epoch)
			}
			_, e = f.RunListNotes(ctx, cap, true, "", "", 50)
			is(t, e, ErrNoProjectAccess)
			check(f.RemoveMember(ctx, p, u))
			_, e = f.GetProject(ctx, f.p("eva"), "fakture")
			is(t, e, ErrNotFound)
			a, e := f.ListTeamAssignments(ctx, team.ID)
			f.ok(e)
			if len(a) != 0 {
				t.Fatal(a)
			}
			if removal == "archive" {
				check(f.SetTeamArchived(ctx, team.ID, false))
				a, e = f.ListTeamAssignments(ctx, team.ID)
				f.ok(e)
				if len(a) != 0 {
					t.Fatal("unarchive recreated assignments", a)
				}
			}
		})
	}
}

func TestTeamRolesNotesGrantsAndInactiveUser(t *testing.T) {
	f := newFx(t)
	team, e := f.CreateTeam(ctx, "Readers")
	f.ok(e)
	check := func(e error) { t.Helper(); f.ok(e); f.ok(f.CheckMemberships(ctx)) }
	u, p := f.uid["eva"], f.pid["eureko"]
	check(f.AddTeamMember(ctx, team.ID, u))
	check(f.AddTeamProject(ctx, team.ID, p))
	check(f.SetTeamAssignment(ctx, team.ID, p, u, Reader))
	structure := f.mk("ana", "fakture", Structure)
	data := f.mk("ana", "fakture", Data)
	private := f.mk("ana", "fakture", Private)
	// Team membership in the final project does not make grants transitive.
	check(f.RemoveMember(ctx, f.pid["fakture"], u))
	check(f.RemoveTeamAssignment(ctx, team.ID, p, u))
	check(f.RemoveTeamProject(ctx, team.ID, p))
	check(f.AddTeamProject(ctx, team.ID, f.pid["import"]))
	check(f.SetTeamAssignment(ctx, team.ID, f.pid["import"], u, Reader))
	_, e = f.GetNote(ctx, f.p("eva"), structure.ProjectID, structure.ID)
	is(t, e, ErrNotFound)
	check(f.RemoveTeamAssignment(ctx, team.ID, f.pid["import"], u))
	check(f.AddTeamProject(ctx, team.ID, p))
	check(f.SetTeamAssignment(ctx, team.ID, p, u, Reader))
	check(f.AddGrant(ctx, f.pid["fakture"], p, Data))
	principal := f.p("eva")
	for _, n := range []Note{structure, data} {
		_, e = f.GetNote(ctx, principal, n.ProjectID, n.ID)
		f.ok(e)
	}
	_, e = f.GetNote(ctx, principal, private.ProjectID, private.ID)
	is(t, e, ErrNotFound)
	// Receiving-project readers can read granted data, but cannot write there.
	_, _, e = f.CreateNote(ctx, principal, p, "reader-write", NoteInput{Class: Data, Title: "x", Body: "x"})
	is(t, e, ErrForbidden)
	check(f.SetTeamAssignment(ctx, team.ID, p, u, Developer))
	f.mk("eva", "eureko", Data)
	check(f.SetUserActive(ctx, u, false))
	check(f.SetTeamAssignment(ctx, team.ID, p, u, Admin))
	_, e = f.GetProject(ctx, principal, "eureko")
	is(t, e, ErrNoProjectAccess)
	check(f.SetUserActive(ctx, u, true))
	check(f.RemoveTeamAssignment(ctx, team.ID, p, u))
	_, e = f.GetProject(ctx, principal, "eureko")
	is(t, e, ErrNotFound)
}

func TestOriginsMigration9RestartRestore(t *testing.T) {
	base := migrations
	t.Cleanup(func() { migrations = base })
	migrations = base[:9]
	dir := t.TempDir()
	s := openT(t, dir)
	u, e := s.CreateUser(ctx, "Legacy")
	is(t, e, nil)
	p, e := s.CreateProject(ctx, "legacy", "Legacy")
	is(t, e, nil)
	is(t, s.write(ctx, func(tx *sql.Tx) error {
		_, e := tx.Exec(`INSERT INTO memberships VALUES(?,?,?)`, p.ID, u.ID, Developer)
		return e
	}), nil)
	s.Close()
	migrations = base
	s = openT(t, dir)
	is(t, s.CheckMemberships(ctx), nil)
	snaps := snapshots(dir)
	if len(snaps) != 1 || !strings.HasPrefix(snaps[0], "core-9-") {
		t.Fatal(snaps)
	}
	ms, e := s.ListMembers(ctx, p.ID)
	is(t, e, nil)
	if len(ms) != 1 || len(ms[0].Origins) != 1 || ms[0].Origins[0].Kind != "direct" {
		t.Fatal(ms)
	}
	team, e := s.CreateTeam(ctx, "Legacy team")
	is(t, e, nil)
	is(t, s.AddTeamMember(ctx, team.ID, u.ID), nil)
	is(t, s.AddTeamProject(ctx, team.ID, p.ID), nil)
	is(t, s.SetTeamAssignment(ctx, team.ID, p.ID, u.ID, Admin), nil)
	is(t, s.RemoveMember(ctx, p.ID, u.ID), nil)
	// Reapplying migration must not turn a team-only origin into a direct one.
	is(t, s.write(ctx, migrate10), nil)
	bak := filepath.Join(t.TempDir(), "origins.db")
	is(t, s.Backup(ctx, bak), nil)
	s.Close()
	s = openT(t, dir)
	is(t, s.CheckMemberships(ctx), nil)
	is(t, s.RemoveTeamAssignment(ctx, team.ID, p.ID, u.ID), nil)
	s.Close()
	is(t, Restore(dir, bak), nil)
	s = openT(t, dir)
	defer s.Close()
	is(t, s.CheckMemberships(ctx), nil)
	ms, e = s.ListMembers(ctx, p.ID)
	is(t, e, nil)
	if len(ms) != 1 || ms[0].Role != Admin || len(ms[0].Origins) != 1 || ms[0].Origins[0].Kind != "team" {
		t.Fatal(ms)
	}
	is(t, s.RemoveTeamAssignment(ctx, team.ID, p.ID, u.ID), nil)
	is(t, s.CheckMemberships(ctx), nil)
	ms, e = s.ListMembers(ctx, p.ID)
	is(t, e, nil)
	if len(ms) != 0 {
		t.Fatal(ms)
	}
}

func TestDirectOriginSurvivesTeamRemoval(t *testing.T) {
	f := newFx(t)
	team, err := f.CreateTeam(ctx, "Support")
	f.ok(err)
	p, u := f.pid["eureko"], f.uid["eva"]
	check := func(err error) { t.Helper(); f.ok(err); f.ok(f.CheckMemberships(ctx)) }
	check(f.SetMember(ctx, p, u, Admin))
	check(f.AddTeamMember(ctx, team.ID, u))
	check(f.AddTeamProject(ctx, team.ID, p))
	check(f.SetTeamAssignment(ctx, team.ID, p, u, Developer))
	epoch, err := f.Epoch(ctx)
	f.ok(err)
	check(f.RemoveTeamAssignment(ctx, team.ID, p, u))
	pr, err := f.GetProject(ctx, f.p("eva"), "eureko")
	f.ok(err)
	if pr.Role != Admin || len(pr.Origins) != 1 || pr.Origins[0].Kind != "direct" {
		t.Fatal(pr)
	}
	if got, err := f.Epoch(ctx); err != nil || got != epoch {
		t.Fatal("surviving direct access changed epoch", got, err)
	}
	check(f.SetTeamAssignment(ctx, team.ID, p, u, Reader))
	check(f.RemoveTeamProject(ctx, team.ID, p))
	check(f.AddTeamProject(ctx, team.ID, p))
	assignments, err := f.ListTeamAssignments(ctx, team.ID)
	f.ok(err)
	if len(assignments) != 0 {
		t.Fatal("reattachment restored revoked assignment", assignments)
	}
}

func TestOriginRevocationAuditIsAtomic(t *testing.T) {
	f := newFx(t)
	team, e := f.CreateTeam(ctx, "Atomic")
	f.ok(e)
	p, u := f.pid["eureko"], f.uid["eva"]
	check := func(e error) { t.Helper(); f.ok(e); f.ok(f.CheckMemberships(ctx)) }
	check(f.AddTeamMember(ctx, team.ID, u))
	check(f.AddTeamProject(ctx, team.ID, p))
	check(f.SetTeamAssignment(ctx, team.ID, p, u, Admin))
	epoch, e := f.Epoch(ctx)
	f.ok(e)
	f.ok(f.write(ctx, func(tx *sql.Tx) error {
		_, e := tx.Exec(`CREATE TRIGGER refuse_assignment_audit BEFORE INSERT ON audit_events WHEN NEW.action='team.assignment.remove' BEGIN SELECT RAISE(ABORT,'audit unavailable'); END`)
		return e
	}))
	if e = f.RemoveTeamMember(ctx, team.ID, u); e == nil {
		t.Fatal("revocation without audit succeeded")
	}
	f.ok(f.CheckMemberships(ctx))
	if got, _ := f.Epoch(ctx); got != epoch {
		t.Fatal("failed transaction changed epoch")
	}
	pr, e := f.GetProject(ctx, f.p("eva"), "eureko")
	f.ok(e)
	if pr.Role != Admin {
		t.Fatal(pr)
	}
	f.ok(f.write(ctx, func(tx *sql.Tx) error { _, e := tx.Exec(`DROP TRIGGER refuse_assignment_audit`); return e }))
	check(f.RemoveTeamMember(ctx, team.ID, u))
	var count int
	f.ok(f.DB().QueryRow(`SELECT count(*) FROM audit_events WHERE action='team.assignment.remove'`).Scan(&count))
	if count != 1 {
		t.Fatal("implicit revocation audit", count)
	}
	// The consistency check also detects accidental writes to the effective authority.
	f.ok(f.write(ctx, func(tx *sql.Tx) error {
		_, e := tx.Exec(`INSERT INTO memberships(project_id,user_id,role) VALUES(?,?,'admin')`, p, u)
		return e
	}))
	if e = f.CheckMemberships(ctx); e == nil {
		t.Fatal("unexplained membership accepted")
	}
}
