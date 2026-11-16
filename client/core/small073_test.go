package core

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestRemoveMemberWithOnlyTeamAccess(t *testing.T) {
	f := newFx(t)
	team, e := f.CreateTeam(ctx, "Alpha")
	f.ok(e)
	p, u := f.pid["fakture"], f.uid["cene"]
	f.ok(f.AddTeamMember(ctx, team.ID, u))
	f.ok(f.AddTeamProject(ctx, team.ID, p))
	f.ok(f.SetTeamAssignment(ctx, team.ID, p, u, Reader))
	var inv ErrInvalid
	e = f.RemoveMember(ctx, p, u)
	if !errors.As(e, &inv) || !strings.Contains(e.Error(), "no direct role; team access: minigw gw team access remove Alpha fakture cene") {
		t.Fatal(e)
	}
	is(t, f.RemoveMember(ctx, p, f.uid["dana"]), ErrNotFound)
}

func TestRestoreRefusesInconsistentMemberships(t *testing.T) {
	dir := t.TempDir()
	s := openT(t, dir)
	u, e := s.CreateUser(ctx, "Ana")
	is(t, e, nil)
	p, e := s.CreateProject(ctx, "pr", "Pr")
	is(t, e, nil)
	exec(t, s, `INSERT INTO memberships VALUES(?,?,?)`, p.ID, u.ID, Admin) // no origin backs it
	bak := filepath.Join(t.TempDir(), "b.db")
	if err := s.Backup(ctx, bak); err != nil {
		t.Fatal(err)
	}
	s.Close()
	before := hash(t, filepath.Join(dir, dbFile))
	if err := Restore(dir, bak); err == nil || !strings.Contains(err.Error(), "membership origins inconsistent") {
		t.Fatalf("got %v", err)
	}
	if hash(t, filepath.Join(dir, dbFile)) != before {
		t.Fatal("the database changed")
	}
}

func TestTeamNamesRejectControlAndFormatting(t *testing.T) {
	f := newFx(t)
	ok, e := f.CreateTeam(ctx, "Fine")
	f.ok(e)
	for _, n := range []string{"a\nb", "a\x00b", "a​b", "a‮b", "a b", "a b"} {
		_, e := f.CreateTeam(ctx, n)
		is(t, e, ErrInvalid{"name"})
		is(t, f.RenameTeam(ctx, ok.ID, n), ErrInvalid{"name"})
	}
}
