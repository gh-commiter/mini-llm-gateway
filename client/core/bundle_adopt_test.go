package core

import (
	"database/sql"
	"strings"
	"testing"
	"time"
)

// adoptExisting lists every object of the bundle that already exists in s, for tests that import onto a populated gateway.
func adoptExisting(t *testing.T, s *Store, file string) (list []string) {
	t.Helper()
	b, err := readBundle(file)
	if err != nil {
		t.Fatal(err)
	}
	has := func(q, arg string) bool {
		var n int
		if err := s.DB().QueryRow(q, arg).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n > 0
	}
	if has(`SELECT count(*) FROM teams WHERE name=? COLLATE NOCASE`, b.manifest.Team) {
		list = append(list, "team:"+b.manifest.Team)
	}
	for _, p := range b.manifest.Projects {
		if has(`SELECT count(*) FROM projects WHERE slug=?`, p.Slug) {
			list = append(list, "project:"+p.Slug)
		}
	}
	for _, u := range b.manifest.Users {
		if has(`SELECT count(*) FROM users WHERE name=? COLLATE NOCASE`, u) {
			list = append(list, "user:"+u)
		}
	}
	return list
}

// hostileBundle names a team, a project and a user that a gateway may already have, and an admin role for the user.
func hostileBundle(t *testing.T, team string, projects ...string) string {
	t.Helper()
	b := bundle{files: map[string][]byte{"INDEX.md": []byte("x")}}
	b.manifest = BundleManifest{Format: 1, Exported: time.Now().UTC().Format(time.RFC3339Nano), OriginGateway: "elsewhere", Team: team,
		Users: []string{"alice"}, Members: []string{"alice"}}
	for _, p := range projects {
		b.manifest.Projects = append(b.manifest.Projects, BundleProject{Slug: p, Name: p})
		b.manifest.Memberships = append(b.manifest.Memberships, BundleMembership{Project: p, User: "alice", Role: Admin})
	}
	return rewriteZip(t, b, nil)
}

func localRole(t *testing.T, f *fx, project, user string) (role string) {
	t.Helper()
	if err := f.DB().QueryRow(`SELECT role FROM direct_memberships WHERE project_id=? AND user_id=?`, f.pid[project], f.uid[user]).Scan(&role); err != nil && err != sql.ErrNoRows {
		t.Fatal(err)
	}
	return role
}

func untouched(t *testing.T, f *fx) {
	t.Helper()
	for _, table := range []string{"teams", "team_projects", "team_members", "import_sources", "import_objects"} {
		if n := countTable(t, f.Store, table); n != 0 {
			t.Fatalf("%s has %d rows after a refused import", table, n)
		}
	}
	if role := localRole(t, f, "shop", "alice"); role != "" {
		t.Fatalf("alice got the role %q on shop", role)
	}
	for _, e := range auditEvents(t, f) {
		if e.Action == "kb.import" {
			t.Fatal("audit row of a refused import")
		}
	}
}

func auditEvents(t *testing.T, f *fx) []AuditEvent {
	t.Helper()
	ev, err := f.AuditEvents(ctx, 0, 1000)
	f.ok(err)
	return ev
}

func shopFx(t *testing.T) *fx {
	f := newFx(t)
	f.project("shop")
	f.user("alice")
	return f
}

func TestBundleImportRefusesLocalNamesWithList(t *testing.T) {
	f := shopFx(t)
	file := hostileBundle(t, "Acme", "shop")
	for _, dry := range []bool{false, true} {
		rep, err := f.ImportBundle(ctx, BundleImport{Path: file, Source: "partner", DryRun: dry})
		if err == nil {
			t.Fatal("hostile bundle imported", dry)
		}
		text := strings.Join(rep.Lines, "\n")
		for _, want := range []string{"did not create; nothing was written", "project:shop ", "user:alice ", "--adopt project:shop,user:alice", "never adopted implicitly"} {
			if !strings.Contains(text, want) {
				t.Fatalf("dry=%v: %q missing in\n%s", dry, want, text)
			}
		}
		if strings.Contains(text, "team:") {
			t.Fatal("the team is new, not a conflict:\n" + text)
		}
		untouched(t, f)
	}
}

func TestBundleAdoptDoesExactlyWhatWasListed(t *testing.T) {
	f := shopFx(t)
	file := hostileBundle(t, "Acme", "shop")
	// Adopting the project does not adopt the user.
	rep, err := f.ImportBundle(ctx, BundleImport{Path: file, Source: "partner", Adopt: []string{"project:shop"}})
	if err == nil || !strings.Contains(err.Error(), "user:alice ") || strings.Contains(err.Error(), "project:shop ") {
		t.Fatal(err, rep)
	}
	untouched(t, f)
	// A dry run with the full list writes nothing and says what happens.
	rep, err = f.ImportBundle(ctx, BundleImport{Path: file, Source: "partner", DryRun: true, Adopt: []string{"project:shop", "user:alice"}})
	f.ok(err)
	text := strings.Join(rep.Lines, "\n")
	for _, want := range []string{"merge adopt project:shop", "merge adopt user:alice", "create team Acme", "merge team project shop", "merge team member alice", "merge role shop alice admin"} {
		if !strings.Contains(text, want) {
			t.Fatalf("%q missing in\n%s", want, text)
		}
	}
	untouched(t, f)
	_, err = f.ImportBundle(ctx, BundleImport{Path: file, Source: "partner", Adopt: []string{"project:shop", "user:alice"}})
	f.ok(err)
	if role := localRole(t, f, "shop", "alice"); role != "admin" {
		t.Fatal("role", role)
	}
	var team, members int
	f.ok(f.DB().QueryRow(`SELECT (SELECT count(*) FROM team_projects WHERE project_id=?), (SELECT count(*) FROM team_members WHERE user_id=?)`, f.pid["shop"], f.uid["alice"]).Scan(&team, &members))
	if team != 1 || members != 1 || countTable(t, f.Store, "teams") != 1 {
		t.Fatal(team, members)
	}
	var found bool
	for _, e := range auditEvents(t, f) {
		if e.Action == "kb.import" {
			found = true
			if !strings.Contains(string(e.Detail), `"adopted":["project:shop","user:alice"]`) && !strings.Contains(string(e.Detail), `"adopted":["user:alice","project:shop"]`) {
				t.Fatal(string(e.Detail))
			}
		}
	}
	if !found {
		t.Fatal("no audit row")
	}
}

func TestBundleAdoptNeverRaisesLocalRole(t *testing.T) {
	f := shopFx(t)
	f.ok(f.SetMember(ctx, f.pid["shop"], f.uid["alice"], Developer))
	file := hostileBundle(t, "Acme", "shop")
	rep, err := f.ImportBundle(ctx, BundleImport{Path: file, Source: "partner", Adopt: []string{"project:shop", "user:alice"}})
	f.ok(err)
	if role := localRole(t, f, "shop", "alice"); role != "developer" {
		t.Fatal("role raised to", role)
	}
	if !strings.Contains(strings.Join(rep.Lines, "\n"), "skip role shop alice: adopted object, the local role developer is kept") {
		t.Fatal(rep.Lines)
	}
	// The same later, without the list: the adoption is remembered and the role is still kept.
	_, err = f.ImportBundle(ctx, BundleImport{Path: file, Source: "partner"})
	f.ok(err)
	if role := localRole(t, f, "shop", "alice"); role != "developer" {
		t.Fatal("role raised to", role)
	}
}

func TestBundleAdoptGrantNeedsListedSourceProject(t *testing.T) {
	f := shopFx(t)
	file := hostileBundle(t, "Acme", "shop", "wiki")
	b, err := readBundle(file)
	f.ok(err)
	b.manifest.Grants = []BundleGrant{{Source: "shop", Reader: "wiki", Class: Structure}}
	file = rewriteZip(t, b, nil)
	grants := func() int { return countTable(t, f.Store, "grants") }
	before := grants()
	_, err = f.ImportBundle(ctx, BundleImport{Path: file, Source: "partner", Adopt: []string{"project:shop", "user:alice"}})
	f.ok(err)
	if grants() != before+1 {
		t.Fatal("grant of the listed project not added")
	}
	var wiki int64
	f.ok(f.DB().QueryRow(`SELECT id FROM projects WHERE slug='wiki'`).Scan(&wiki))
	f.ok(f.RemoveGrant(ctx, f.pid["shop"], wiki, Structure))
	// The re-import needs no flags for the objects, but a grant FROM an adopted project is added only when it is listed again.
	rep, err := f.ImportBundle(ctx, BundleImport{Path: file, Source: "partner"})
	f.ok(err)
	if grants() != before || !strings.Contains(strings.Join(rep.Lines, "\n"), "shop was adopted earlier") {
		t.Fatal(rep.Lines)
	}
	_, err = f.ImportBundle(ctx, BundleImport{Path: file, Source: "partner", Adopt: []string{"project:shop"}})
	f.ok(err)
	if grants() != before+1 {
		t.Fatal("listed again, the grant is added")
	}
}

func TestBundleTeamNameCollisionIsAConflict(t *testing.T) {
	f := newFx(t)
	f.user("alice")
	local, err := f.CreateTeam(ctx, "Acme")
	f.ok(err)
	f.ok(f.AddTeamMember(ctx, local.ID, f.uid["bor"]))
	file := hostileBundle(t, "acme") // other spelling, same team
	_, err = f.ImportBundle(ctx, BundleImport{Path: file, Source: "partner", Adopt: []string{"user:alice"}})
	if err == nil || !strings.Contains(err.Error(), "team:acme ") || strings.Contains(err.Error(), "user:alice ") {
		t.Fatal(err)
	}
	if countTable(t, f.Store, "team_members") != 1 {
		t.Fatal("a local team took a member")
	}
	_, err = f.ImportBundle(ctx, BundleImport{Path: file, Source: "partner", Adopt: []string{"team:ACME", "user:alice"}})
	f.ok(err)
	if countTable(t, f.Store, "team_members") != 2 || countTable(t, f.Store, "teams") != 1 {
		t.Fatal("adopted team not merged")
	}
}

func TestBundleReimportFromSameSourceNeedsNoFlags(t *testing.T) {
	f := newFx(t)
	file := hostileBundle(t, "Acme", "shop")
	_, err := f.ImportBundle(ctx, BundleImport{Path: file, Source: "partner"})
	f.ok(err) // nothing of it exists here: all created by this source
	rep, err := f.ImportBundle(ctx, BundleImport{Path: file, Source: "partner"})
	f.ok(err)
	if text := strings.Join(rep.Lines, "\n"); strings.Contains(text, "create") || !strings.Contains(text, "skip project shop") {
		t.Fatal(text)
	}
	// Another source finds those objects foreign.
	_, err = f.ImportBundle(ctx, BundleImport{Path: file, Source: "other"})
	if err == nil || !strings.Contains(err.Error(), "project:shop ") {
		t.Fatal(err)
	}
	// A local rename ends the source's claim: the bundle's name then makes a new object.
	var id int64
	f.ok(f.DB().QueryRow(`SELECT id FROM users WHERE name='alice'`).Scan(&id))
	f.ok(f.RenameUser(ctx, id, "alicia"))
	_, err = f.ImportBundle(ctx, BundleImport{Path: file, Source: "partner"})
	f.ok(err)
	var alice, alicia int
	f.ok(f.DB().QueryRow(`SELECT (SELECT count(*) FROM users WHERE name='alice'), (SELECT count(*) FROM users WHERE name='alicia')`).Scan(&alice, &alicia))
	if alice != 1 || alicia != 1 {
		t.Fatal(alice, alicia)
	}
}

func TestBundleAdoptListIsChecked(t *testing.T) {
	f := shopFx(t)
	file := hostileBundle(t, "Acme", "shop")
	for _, bad := range []string{"shop", "group:shop", "user:", "project:nope", "user:bor"} {
		if _, err := f.ImportBundle(ctx, BundleImport{Path: file, Source: "partner", DryRun: true, Adopt: []string{"project:shop", "user:alice", bad}}); err == nil {
			t.Fatalf("accepted --adopt %q", bad)
		}
	}
	untouched(t, f)
}
