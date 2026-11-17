package core

import (
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestTeamDirectory(t *testing.T) {
	f := newFx(t)
	alpha, e := f.CreateTeam(ctx, "Alpha")
	f.ok(e)
	beta, e := f.CreateTeam(ctx, "Beta")
	f.ok(e)
	hidden, e := f.CreateTeam(ctx, "Hidden")
	f.ok(e)
	for _, id := range []int64{alpha.ID, beta.ID} {
		f.ok(f.AddTeamMember(ctx, id, f.uid["ana"]))
	}
	f.ok(f.AddTeamMember(ctx, alpha.ID, f.uid["cene"]))
	f.ok(f.AddTeamMember(ctx, hidden.ID, f.uid["dana"]))
	f.ok(f.AddTeamProject(ctx, alpha.ID, f.pid["fakture"]))
	f.ok(f.AddTeamProject(ctx, alpha.ID, f.pid["eureko"]))
	f.ok(f.AddTeamProject(ctx, alpha.ID, f.pid["import"]))
	teams, e := f.ListTeams(ctx, f.p("ana"), false)
	f.ok(e)
	if len(teams) != 2 {
		t.Fatalf("teams: %+v", teams)
	}
	detail, e := f.GetTeam(ctx, f.p("ana"), "ALPHA")
	f.ok(e)
	if len(detail.Members) != 2 || len(detail.Projects) != 1 || detail.Projects[0].Slug != "fakture" {
		t.Fatalf("detail: %+v", detail)
	}
	// A grant permits project visibility, but team membership grants no project role.
	detail, e = f.GetTeam(ctx, f.p("cene"), "Alpha")
	f.ok(e)
	if len(detail.Projects) != 2 {
		t.Fatalf("grant projects: %+v", detail.Projects)
	}
	_, e = f.GetTeam(ctx, f.p("ana"), strconv.FormatInt(hidden.ID, 10))
	is(t, e, ErrNotFound)
	_, e = f.GetTeam(ctx, f.p("ana"), "missing")
	is(t, e, ErrNotFound)
	_, e = f.GetProject(ctx, f.p("ana"), "import")
	is(t, e, ErrNotFound)
	_, e = f.ResolveForLink(ctx, f.p("cene"), "fakture")
	is(t, e, ErrForbidden)
	epoch, _ := f.Epoch(ctx)
	f.ok(f.RemoveTeamMember(ctx, alpha.ID, f.uid["ana"]))
	_, e = f.GetTeam(ctx, f.p("ana"), "Alpha")
	is(t, e, ErrNotFound)
	if _, e = f.GetProject(ctx, f.p("ana"), "fakture"); e != nil {
		t.Fatal("team removal changed project access", e)
	}
	f.ok(f.RemoveTeamProject(ctx, alpha.ID, f.pid["fakture"]))
	f.ok(f.AddTeamProject(ctx, beta.ID, f.pid["fakture"]))
	if e = f.AddTeamProject(ctx, alpha.ID, f.pid["fakture"]); e == nil {
		t.Fatal("project in two teams")
	}
	f.ok(f.SetTeamArchived(ctx, beta.ID, true))
	teams, e = f.ListTeams(ctx, f.p("ana"), false)
	f.ok(e)
	if len(teams) != 0 {
		t.Fatal(teams)
	}
	teams, e = f.ListTeams(ctx, f.p("ana"), true)
	f.ok(e)
	if len(teams) != 1 || !teams[0].Archived {
		t.Fatal(teams)
	}
	detail, e = f.GetTeam(ctx, f.p("ana"), "Beta")
	f.ok(e)
	if len(detail.Projects) != 1 {
		t.Fatal(detail)
	}
	if e = f.AddTeamMember(ctx, beta.ID, f.uid["bor"]); e == nil {
		t.Fatal("adding to archived team")
	}
	if e = f.AddTeamProject(ctx, beta.ID, f.pid["import"]); e == nil {
		t.Fatal("adding project to archived team")
	}
	f.ok(f.RemoveTeamProject(ctx, beta.ID, f.pid["fakture"]))
	if got, _ := f.Epoch(ctx); got != epoch {
		t.Fatalf("directory changed epoch: %d -> %d", epoch, got)
	}
	principal := f.p("ana")
	f.ok(f.RemoveBinding(ctx, principal.DeviceID))
	_, e = f.ListTeams(ctx, principal, true)
	is(t, e, ErrNoProjectAccess)
	_, e = f.GetTeam(ctx, principal, "Beta")
	is(t, e, ErrNoProjectAccess)
	_, e = f.Principal(ctx, "unbound", true)
	is(t, e, ErrNoProjectAccess)
	f.ok(f.SetBinding(ctx, principal.DeviceID, principal.UserID))
	f.ok(f.SetUserActive(ctx, principal.UserID, false))
	_, e = f.GetTeam(ctx, principal, "Beta")
	is(t, e, ErrNoProjectAccess)
}

func TestTeamValidationAndAudit(t *testing.T) {
	f := newFx(t)
	for _, name := range []string{"", "  ", strings.Repeat("x", 129)} {
		if _, e := f.CreateTeam(ctx, name); e == nil {
			t.Fatal("accepted", name)
		}
	}
	a, e := f.CreateTeam(ctx, "Alpha")
	f.ok(e)
	if _, e = f.CreateTeam(ctx, "alpha"); e == nil {
		t.Fatal("case insensitive duplicate")
	}
	b, e := f.CreateTeam(ctx, "Beta")
	f.ok(e)
	numeric, e := f.CreateTeam(ctx, "12345")
	f.ok(e)
	f.ok(f.AddTeamMember(ctx, numeric.ID, f.uid["ana"]))
	byID, e := f.GetTeam(ctx, f.p("ana"), strconv.FormatInt(numeric.ID, 10))
	f.ok(e)
	if byID.Name != "12345" {
		t.Fatal(byID)
	}
	if e = f.RenameTeam(ctx, b.ID, "ALPHA"); e == nil {
		t.Fatal("duplicate rename")
	}
	f.ok(f.RenameTeam(ctx, a.ID, "Renamed"))
	f.ok(f.AddTeamMember(ctx, a.ID, f.uid["ana"]))
	f.ok(f.AddTeamProject(ctx, a.ID, f.pid["fakture"]))
	f.ok(f.RemoveTeamMember(ctx, a.ID, f.uid["ana"]))
	f.ok(f.RemoveTeamProject(ctx, a.ID, f.pid["fakture"]))
	f.ok(f.SetTeamArchived(ctx, a.ID, true))
	for _, action := range []string{"team.create", "team.rename", "team.member.add", "team.project.add", "team.member.remove", "team.project.remove", "team.archive"} {
		var n int
		f.ok(f.DB().QueryRow(`SELECT count(*) FROM audit_events WHERE action=?`, action).Scan(&n))
		if n == 0 {
			t.Fatal("missing audit", action)
		}
	}
	is(t, f.RemoveTeamMember(ctx, a.ID, f.uid["ana"]), ErrNotFound)
	is(t, f.AddTeamMember(ctx, 9999, f.uid["ana"]), ErrNotFound)
	is(t, f.AddTeamProject(ctx, b.ID, 9999), ErrNotFound)
	_, e = f.AdminGetTeam(ctx, 9999)
	is(t, e, ErrNotFound)
}

func TestTeamsMigration8AndRestore(t *testing.T) {
	base := migrations
	t.Cleanup(func() { migrations = base })
	migrations = base[:8]
	dir := t.TempDir()
	s := openT(t, dir)
	u, e := s.CreateUser(ctx, "Legacy")
	if e != nil {
		t.Fatal(e)
	}
	p, e := s.CreateProject(ctx, "legacy", "Legacy")
	if e != nil {
		t.Fatal(e)
	}
	if e = s.write(ctx, func(tx *sql.Tx) error {
		_, e := tx.Exec(`INSERT INTO memberships(project_id,user_id,role) VALUES(?,?,?)`, p.ID, u.ID, Developer)
		return e
	}); e != nil {
		t.Fatal(e)
	}
	if e = s.SetBinding(ctx, "legacy-device", u.ID); e != nil {
		t.Fatal(e)
	}
	p2, e := s.CreateProject(ctx, "reader-project", "Reader")
	if e != nil {
		t.Fatal(e)
	}
	if e = s.AddGrant(ctx, p.ID, p2.ID, Structure); e != nil {
		t.Fatal(e)
	}
	principal, e := s.Principal(ctx, "legacy-device", true)
	if e != nil {
		t.Fatal(e)
	}
	// Seed schema 8 directly: current note writes require schema 11 provenance.
	note := Note{ProjectID: p.ID, Class: Data, Title: "Legacy", Body: "Preserve me", AuthorID: principal.UserID, Author: "Legacy", Created: fromMs(1), Updated: fromMs(1), Revision: 1, EntryKind: "finding", Provenance: Provenance{ActorKind: "member", User: principal.UserID, Device: "UNKNOWN", Project: p.ID, SourceKind: "UNKNOWN", ContentHash: contentHash("Legacy", "Preserve me"), Authored: fromMs(1), Received: fromMs(1)}}
	if e = s.write(ctx, func(tx *sql.Tx) error {
		r, e := tx.Exec(`INSERT INTO notes(project_id,class,title,body,bytes,author_id,created,updated) VALUES(?,?,?,?,?,?,?,?)`, p.ID, Data, "Legacy", "Preserve me", 17, principal.UserID, 1, 1)
		if e != nil {
			return e
		}
		note.ID, _ = r.LastInsertId()
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	note.Provenance.SourceReference = "UNKNOWN"
	grants, _ := s.ListGrants(ctx)
	users, _ := s.ListUsers(ctx)
	projects, _ := s.AdminListProjects(ctx)
	members := []Member{{UserID: u.ID, Name: u.Name, Role: Developer, Origins: []MembershipOrigin{{Kind: "direct", Role: Developer}}}}
	s.Close()
	migrations = base
	s = openT(t, dir)
	if v, _ := schemaVersion(ctx, s.DB()); v != len(migrations) {
		t.Fatal("version", v)
	}
	if e = s.write(ctx, migrate9); e != nil {
		t.Fatal("idempotent migration", e)
	}
	snaps := snapshots(dir)
	if len(snaps) != 1 || !strings.HasPrefix(snaps[0], "core-8-") {
		t.Fatal("snapshot", snaps)
	}
	if _, e = validate(ctx, filepath.Join(dir, snaps[0]), ""); e != nil {
		t.Fatal("schema8 snapshot", e)
	}
	gotUsers, _ := s.ListUsers(ctx)
	gotProjects, _ := s.AdminListProjects(ctx)
	gotMembers, _ := s.ListMembers(ctx, p.ID)
	if !reflect.DeepEqual(users, gotUsers) || !reflect.DeepEqual(projects, gotProjects) || !reflect.DeepEqual(members, gotMembers) {
		t.Fatal("legacy state changed")
	}
	gotGrants, _ := s.ListGrants(ctx)
	if !reflect.DeepEqual(grants, gotGrants) {
		t.Fatal("grants changed")
	}
	gotNote, e := s.GetNote(ctx, principal, p.ID, note.ID)
	if e != nil || !reflect.DeepEqual(note, gotNote) {
		t.Fatal("note changed", gotNote, e)
	}
	team, e := s.CreateTeam(ctx, "Team")
	if e != nil {
		t.Fatal(e)
	}
	if e = s.AddTeamMember(ctx, team.ID, u.ID); e != nil {
		t.Fatal(e)
	}
	if e = s.AddTeamProject(ctx, team.ID, p.ID); e != nil {
		t.Fatal(e)
	}
	backup := filepath.Join(t.TempDir(), "teams.db")
	if e = s.Backup(ctx, backup); e != nil {
		t.Fatal(e)
	}
	s.Close()
	s = openT(t, dir)
	if len(snapshots(dir)) != 1 {
		t.Fatal("reopen repeated migration")
	}
	if e = s.RemoveTeamMember(ctx, team.ID, u.ID); e != nil {
		t.Fatal(e)
	}
	s.Close()
	if e = Restore(dir, backup); e != nil {
		t.Fatal("restore teams allowlist", e)
	}
	s = openT(t, dir)
	defer s.Close()
	detail, e := s.AdminGetTeam(ctx, team.ID)
	if e != nil {
		t.Fatal(e)
	}
	if len(detail.Members) != 1 || len(detail.Projects) != 1 {
		t.Fatal("restore lost directory", detail)
	}
	principal, e = s.Principal(ctx, "legacy-device", true)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.GetTeam(ctx, principal, "Team"); errors.Is(e, ErrNotFound) || e != nil {
		t.Fatal(e)
	}
}

func TestTeamsSnapshotFailureStopsMigration(t *testing.T) {
	base := migrations
	t.Cleanup(func() { migrations = base })
	migrations = base[:8]
	dir := t.TempDir()
	s := openT(t, dir)
	s.Close()
	migrations = base
	db, e := openDB(filepath.Join(dir, dbFile))
	if e != nil {
		t.Fatal(e)
	}
	defer db.Close()
	// The snapshot destination is unavailable while the schema 8 database is writable.
	s = &Store{db: db, dir: filepath.Join(dir, "missing")}
	if e = s.migrate(ctx); e == nil || !strings.Contains(e.Error(), "snapshot before migration") {
		t.Fatalf("snapshot failure: %v", e)
	}
	if v, e := schemaVersion(ctx, db); e != nil || v != 8 {
		t.Fatal("schema changed", v, e)
	}
	var n int
	if e = db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='teams'`).Scan(&n); e != nil || n != 0 {
		t.Fatal("team table created", n, e)
	}
}
