package core

import (
	"errors"
	"strings"
	"testing"
)

// A project held by an archived team: the refusal names the team and says it is archived; the owner can move the project by
// removing it from the archived team (allowed) and adding it to the other one. No refusal text starts with "invalid ".
func TestProjectOfAnArchivedTeamCanBeMoved(t *testing.T) {
	f := newSeg(t)
	acme2, e := f.CreateTeam(ctx, "acme2")
	f.ok(e)
	f.ok(f.SetTeamArchived(ctx, f.acme, true))
	e = f.AddTeamProject(ctx, acme2.ID, f.pid["fakture"])
	var ref ErrRefusal
	if !errors.As(e, &ref) || !strings.Contains(ref.Msg, "already in team acme (archived)") || !strings.Contains(ref.Msg, "minigw gw team project remove acme fakture") || strings.HasPrefix(e.Error(), "invalid") {
		t.Fatal(e)
	}
	if !errors.Is(e, ErrInvalid{ref.Msg}) { // still a bad request for the callers that test for ErrInvalid
		t.Fatal(e)
	}
	f.ok(f.RemoveTeamProject(ctx, f.acme, f.pid["fakture"])) // the archived team lets go
	f.ok(f.AddTeamProject(ctx, acme2.ID, f.pid["fakture"]))
	// A team that is not archived is named too, without the marker.
	f3, e := f.CreateTeam(ctx, "acme3")
	f.ok(e)
	if e = f.AddTeamProject(ctx, f3.ID, f.pid["fakture"]); !errors.As(e, &ref) || !strings.Contains(ref.Msg, "already in team acme2:") || strings.Contains(ref.Msg, "archived") {
		t.Fatal(e)
	}
	// Adding to an archived team says which one and how to fix it.
	f.ok(f.SetTeamArchived(ctx, f3.ID, true))
	if e = f.AddTeamProject(ctx, f3.ID, f.pid["import"]); !errors.As(e, &ref) || !strings.Contains(ref.Msg, "team acme3 is archived: unarchive it first (minigw gw team unarchive acme3)") {
		t.Fatal(e)
	}
}

// Refusals that are sentences are ErrRefusal: they print as written, not glued to "invalid ".
func TestRefusalsAreSentences(t *testing.T) {
	f := newSeg(t)
	for name, e := range map[string]error{
		"team name":    func() error { _, e := f.CreateTeam(ctx, "acme"); return e }(),
		"user name":    func() error { _, e := f.CreateUser(ctx, "ana"); return e }(),
		"project slug": func() error { _, e := f.CreateProject(ctx, "fakture", "F"); return e }(),
		"grant same":   f.AddGrant(ctx, f.pid["fakture"], f.pid["fakture"], Structure),
		"depends self": f.AddDepends(ctx, f.pid["fakture"], f.pid["fakture"]),
		"assignment":   f.SetTeamAssignment(ctx, f.zeta, f.pid["import"], f.uid["ana"], Reader),
	} {
		if e == nil {
			continue
		}
		var ref ErrRefusal
		if !errors.As(e, &ref) || strings.HasPrefix(e.Error(), "invalid") {
			t.Errorf("%s: %v", name, e)
		}
	}
}
