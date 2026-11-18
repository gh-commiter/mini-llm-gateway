package core

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestNormalizeRemoteFixture(t *testing.T) {
	b, err := os.ReadFile("../testdata/remote-identity.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Raw      string `json:"raw"`
		Identity string `json:"identity"`
		Error    bool   `json:"error"`
	}
	if err = json.Unmarshal(b, &cases); err != nil || len(cases) < 20 {
		t.Fatal(len(cases), err)
	}
	for _, c := range cases {
		got, err := NormalizeRemote(c.Raw)
		if c.Error {
			var inv ErrInvalid
			if !errors.As(err, &inv) {
				t.Errorf("%q: want refusal, got %q %v", c.Raw, got, err)
			}
			continue
		}
		if err != nil || got != c.Identity {
			t.Errorf("%q: got %q %v, want %q", c.Raw, got, err, c.Identity)
		}
	}
}

// A hand-built schema 13 file: old rows read as finding with an empty path; the snapshot is taken first.
func TestMigration14FromSchema13(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, dbFile))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations[:13] {
		if err = m(tx); err != nil {
			t.Fatal(err)
		}
	}
	if err = setMeta(tx, "schema", "13"); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO users(id,name) VALUES(1,'ana')`,
		`INSERT INTO projects(id,slug,name) VALUES(1,'fakture','F')`,
		`INSERT INTO notes(project_id,class,title,body,bytes,author_id,created,updated) VALUES(1,'data','Old','body',7,1,1,1)`,
		`INSERT INTO kb_submissions(project_id,user_id,device_id,class,title,body,bytes,replaces,revision,provenance,key,hash,received) VALUES(1,1,'d','data','Sub','b',4,0,0,'{}','k','h',1)`,
		`INSERT INTO teams(name) VALUES('Team')`,
	} {
		if _, err = tx.Exec(q); err != nil {
			t.Fatal(q, err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s := openT(t, dir)
	check := func(s *Store) {
		t.Helper()
		var nk, sk, path string
		if err := s.DB().QueryRow(`SELECT n.entry_kind,(SELECT entry_kind FROM kb_submissions),(SELECT path FROM teams) FROM notes n`).Scan(&nk, &sk, &path); err != nil || nk != "finding" || sk != "finding" || path != "" {
			t.Fatal(nk, sk, path, err)
		}
		if _, err := s.DB().Exec(`INSERT INTO notes(project_id,class,title,body,bytes,author_id,created,updated,entry_kind) VALUES(1,'data','x','y',2,1,1,1,'other')`); err == nil {
			t.Fatal("entry_kind check missing")
		}
	}
	check(s)
	if snaps, _ := filepath.Glob(filepath.Join(dir, "core-13-*.db")); len(snaps) != 1 {
		t.Fatal("no core-13 snapshot", snaps)
	}
	if err = checkSchemaObjects(ctx, s.DB(), len(migrations)); err != nil {
		t.Fatal(err)
	}
	bak := filepath.Join(t.TempDir(), "d.db")
	if err = s.Backup(ctx, bak); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if err = Restore(dir, bak); err != nil {
		t.Fatal(err)
	}
	s = openT(t, dir)
	defer s.Close()
	check(s)
}

// A schema 13 backup restores into the 14 program (migrated); a 14 backup keeps identities, path and kinds.
func TestBackup14KeepsIdentitiesPathAndKinds(t *testing.T) {
	f := newFx(t)
	project := f.pid["fakture"]
	_, err := f.ChangeProjectRemotes(ctx, f.p("fani"), project, []string{"git@host.example:a/b.git"}, nil)
	f.ok(err)
	team, err := f.CreateTeam(ctx, "Team")
	f.ok(err)
	f.ok(f.SetTeamPath(ctx, team.ID, "Eureko/Core"))
	v, _, err := f.Submit(ctx, f.p("ana"), project, "k", SubmissionInput{NoteInput: NoteInput{Class: Data, Title: "Q?", Body: "A.", EntryKind: KindFAQ}})
	f.ok(err)
	_, err = f.ReviewSubmission(ctx, v.ID, true, "", f.seen(v.ID))
	f.ok(err)
	bak := filepath.Join(t.TempDir(), "b.db")
	f.ok(f.Backup(ctx, bak))
	dir := t.TempDir()
	f.ok(Restore(dir, bak))
	s := openT(t, dir)
	defer s.Close()
	var a, b, c int
	f.ok(s.DB().QueryRow(`SELECT (SELECT count(*) FROM project_remotes WHERE identity='host.example/a/b'),(SELECT count(*) FROM teams WHERE path='eureko/core'),(SELECT count(*) FROM notes WHERE entry_kind='faq')`).Scan(&a, &b, &c))
	if a != 1 || b != 1 || c != 1 {
		t.Fatal(a, b, c)
	}
}

func TestEntryKindFlow(t *testing.T) {
	f := newFx(t)
	project := f.pid["fakture"]
	ana := f.p("ana")
	faq := SubmissionInput{NoteInput: NoteInput{Class: Structure, Title: "How do I reset?", Body: "Press the button.", EntryKind: KindFAQ}}
	v, _, err := f.Submit(ctx, ana, project, "faq", faq)
	f.ok(err)
	if v.EntryKind != KindFAQ {
		t.Fatal(v)
	}
	// A pending faq is no note: not in the faq view, not in search.
	pg, err := f.ListFAQ(ctx, ana, project, "")
	f.ok(err)
	if len(pg.Entries) != 0 {
		t.Fatal("pending faq listed", pg)
	}
	v, err = f.ReviewSubmission(ctx, v.ID, true, "", f.seen(v.ID))
	f.ok(err)
	n, err := f.GetNote(ctx, ana, project, v.NoteID)
	f.ok(err)
	if n.EntryKind != KindFAQ {
		t.Fatal(n)
	}
	hits, err := f.SearchNotes(ctx, ana, project, "reset")
	f.ok(err)
	if len(hits.Hits) != 1 || hits.Hits[0].EntryKind != KindFAQ {
		t.Fatal(hits)
	}
	if md := string(Markdown(n, "")); !strings.Contains(md, "entry_kind: \"faq\"\n") || strings.Index(md, "kind:") > strings.Index(md, "entry_kind:") {
		t.Fatal(md)
	}
	// A plain submit is a finding; CreateNote ignores a kind; an update never changes it (spoof ignored).
	plain, _, err := f.Submit(ctx, ana, project, "plain", SubmissionInput{NoteInput: NoteInput{Class: Data, Title: "Found", Body: "x"}})
	f.ok(err)
	if plain.EntryKind != KindFinding {
		t.Fatal(plain)
	}
	made, _, err := f.CreateNote(ctx, ana, project, "created", NoteInput{Class: Data, Title: "c", Body: "x", EntryKind: KindFAQ})
	f.ok(err)
	if made.EntryKind != KindFinding {
		t.Fatal("CreateNote took a kind", made)
	}
	n, err = f.UpdateNote(ctx, ana, project, n.ID, n.Revision, NoteInput{Class: Structure, Title: "How to reset?", Body: "Press it.", EntryKind: KindFinding})
	f.ok(err)
	if n.EntryKind != KindFAQ {
		t.Fatal("update changed the kind", n)
	}
	// Unspecified on a replacement keeps the kind of the replaced note.
	rep, _, err := f.Submit(ctx, ana, project, "rep", SubmissionInput{NoteInput: NoteInput{Class: Structure, Title: "Again?", Body: "Yes."}, Replaces: n.ID, Revision: n.Revision})
	f.ok(err)
	if rep.EntryKind != KindFAQ {
		t.Fatal(rep)
	}
	rep, err = f.ReviewSubmission(ctx, rep.ID, true, "", f.seen(rep.ID))
	f.ok(err)
	if n, err = f.GetNote(ctx, ana, project, rep.NoteID); err != nil || n.EntryKind != KindFAQ || n.Title != "Again?" {
		t.Fatal(n, err)
	}
	// Limits and values.
	long := strings.Repeat("a", maxFAQBody+1)
	_, _, err = f.Submit(ctx, ana, project, "long", SubmissionInput{NoteInput: NoteInput{Class: Data, Title: "Q", Body: long, EntryKind: KindFAQ}})
	is(t, err, ErrInvalid{"faq answer: at most 2048 bytes"})
	_, _, err = f.Submit(ctx, ana, project, "longq", SubmissionInput{NoteInput: NoteInput{Class: Data, Title: strings.Repeat("q", 257), Body: "a", EntryKind: KindFAQ}})
	is(t, err, ErrInvalid{"faq question: at most 256 bytes"})
	_, _, err = f.Submit(ctx, ana, project, "bad", SubmissionInput{NoteInput: NoteInput{Class: Data, Title: "Q", Body: "a", EntryKind: "rumor"}})
	is(t, err, ErrInvalid{"kind"})
	_, err = f.UpdateNote(ctx, ana, project, n.ID, n.Revision, NoteInput{Class: Structure, Title: "Q", Body: long})
	is(t, err, ErrInvalid{"faq answer: at most 2048 bytes"})
	if _, err = f.UpdateNote(ctx, ana, project, made.ID, made.Revision, NoteInput{Class: Data, Title: "c", Body: long}); err != nil {
		t.Fatal("a finding keeps the large limit", err)
	}
}

// The hash of a request without the field is what an old gateway computed, so a repeat across the upgrade works.
func TestEntryKindOmittedFromIdempotencyBytes(t *testing.T) {
	b, _ := json.Marshal(SubmissionInput{NoteInput: NoteInput{Class: Data, Title: "T", Body: "B"}, Replaces: 3, Revision: 2})
	if string(b) != `{"Class":"data","Title":"T","Body":"B","replaces":3,"revision":2}` {
		t.Fatal(string(b))
	}
	b, _ = json.Marshal(NoteInput{Class: Data, Title: "T", Body: "B"})
	if string(b) != `{"Class":"data","Title":"T","Body":"B"}` {
		t.Fatal(string(b))
	}
	f := newFx(t)
	in := SubmissionInput{NoteInput: NoteInput{Class: Data, Title: "T", Body: "B"}}
	v, _, err := f.Submit(ctx, f.p("ana"), f.pid["fakture"], "up", in)
	f.ok(err)
	old, _ := json.Marshal(in)
	h := sha256.Sum256(old)
	var stored string
	f.ok(f.DB().QueryRow(`SELECT hash FROM kb_submissions WHERE id=?`, v.ID).Scan(&stored))
	if stored != hex.EncodeToString(h[:]) {
		t.Fatal("hash changed")
	}
	again, replay, err := f.Submit(ctx, f.p("ana"), f.pid["fakture"], "up", in)
	f.ok(err)
	if !replay || again.ID != v.ID {
		t.Fatal(again)
	}
}

func TestProjectRemotesAuthorization(t *testing.T) {
	f := newFx(t)
	project := f.pid["fakture"]
	add := []string{"git@Host.example:Eureko/Fakture.git"}
	for _, u := range []string{"ana", "eva"} { // developer, reader
		_, err := f.ChangeProjectRemotes(ctx, f.p(u), project, add, nil)
		is(t, err, ErrForbidden)
	}
	_, err := f.ChangeProjectRemotes(ctx, f.p("cene"), project, add, nil) // grant only
	is(t, err, ErrForbidden)
	_, err = f.ChangeProjectRemotes(ctx, f.p("dana"), project, add, nil) // not a member, cannot see
	is(t, err, ErrNotFound)
	if n := countTable(t, f.Store, "project_remotes"); n != 0 {
		t.Fatal("refused writes stored", n)
	}
	got, err := f.ChangeProjectRemotes(ctx, f.p("fani"), project, add, nil)
	f.ok(err)
	if !reflect.DeepEqual(got, []string{"host.example/eureko/fakture"}) {
		t.Fatal(got)
	}
	// Reading needs a direct membership: a reader may, a grant-only user may not, a stranger sees nothing.
	if got, err = f.ProjectRemotes(ctx, f.p("eva"), project); err != nil || len(got) != 1 {
		t.Fatal(got, err)
	}
	_, err = f.ProjectRemotes(ctx, f.p("cene"), project)
	is(t, err, ErrForbidden)
	got, err = f.ProjectRemotes(ctx, f.p("dana"), project)
	is(t, err, ErrNotFound)
	if got != nil {
		t.Fatal("identities leaked", got)
	}
	// The same identity may be registered for another project; at most 8 per project.
	other := f.pid["eureko"]
	if _, err = f.AdminChangeProjectRemotes(ctx, other, add, nil); err != nil {
		t.Fatal(err)
	}
	var many []string
	for i := 0; i < 8; i++ {
		many = append(many, "git@host.example:o/r"+string(rune('a'+i)))
	}
	_, err = f.ChangeProjectRemotes(ctx, f.p("fani"), project, many, nil)
	is(t, err, ErrRefusal{"a project can have at most 8 git remotes"})
	if got, _ = f.ProjectRemotes(ctx, f.p("fani"), project); len(got) != 1 {
		t.Fatal("limit failure was not atomic", got)
	}
	got, err = f.ChangeProjectRemotes(ctx, f.p("fani"), project, many[:7], add)
	f.ok(err)
	if len(got) != 7 {
		t.Fatal(got)
	}
	_, err = f.ChangeProjectRemotes(ctx, f.p("fani"), project, []string{"/local/path"}, nil)
	is(t, err, ErrInvalid{"git remote"})
	// Archived: refused.
	f.ok(f.SetProjectArchived(ctx, project, true))
	_, err = f.ChangeProjectRemotes(ctx, f.p("fani"), project, add, nil)
	is(t, err, ErrArchived)
	var audits int
	f.ok(f.DB().QueryRow(`SELECT count(*) FROM audit_events WHERE action='project.remotes' AND actor=?`, actor(f.p("fani"))).Scan(&audits))
	if audits != 2 { // the first add and the 7-add/remove
		t.Fatal("audit events", audits)
	}
}

func TestProjectsByRemote(t *testing.T) {
	f := newFx(t)
	fak, eur := f.pid["fakture"], f.pid["eureko"]
	raw := "https://tok:secret@Host.example:8443/Eureko/Fakture.git"
	_, err := f.ChangeProjectRemotes(ctx, f.p("fani"), fak, []string{raw}, nil)
	f.ok(err)
	_, err = f.AdminChangeProjectRemotes(ctx, eur, []string{"git@host.example:eureko/fakture"}, nil) // monorepo
	f.ok(err)
	names := func(user string) (string, []string) {
		t.Helper()
		id, ps, err := f.ProjectsByRemote(ctx, f.p(user), "ssh://git@host.example/EUREKO/fakture.git")
		f.ok(err)
		var out []string
		for _, p := range ps {
			out = append(out, p.Slug)
		}
		return id, out
	}
	id, got := names("ana") // member of fakture only
	if id != "host.example/eureko/fakture" || !reflect.DeepEqual(got, []string{"fakture"}) {
		t.Fatal(id, got)
	}
	if _, got = names("cene"); !reflect.DeepEqual(got, []string{"eureko"}) { // eureko member, fakture is grant-only for cene
		t.Fatal(got)
	}
	if _, got = names("dana"); len(got) != 0 { // import member: reads eureko through a grant only
		t.Fatal("grant-only project matched", got)
	}
	f.ok(f.SetMember(ctx, eur, f.uid["ana"], Developer))
	if _, got = names("ana"); !reflect.DeepEqual(got, []string{"eureko", "fakture"}) {
		t.Fatal("both identities must be ambiguous", got)
	}
	f.ok(f.SetProjectArchived(ctx, eur, true))
	if _, got = names("ana"); !reflect.DeepEqual(got, []string{"fakture"}) {
		t.Fatal("archived project matched", got)
	}
	_, _, err = f.ProjectsByRemote(ctx, f.p("ana"), "/srv/git/repo")
	is(t, err, ErrInvalid{"git remote"})
}

func TestTeamPathLabelAndProjectQuery(t *testing.T) {
	f := newFx(t)
	mine, other := f.pid["fakture"], f.pid["eureko"]
	t1, err := f.CreateTeam(ctx, "Mine")
	f.ok(err)
	t2, err := f.CreateTeam(ctx, "Other")
	f.ok(err)
	f.ok(f.AddTeamProject(ctx, t1.ID, mine))
	f.ok(f.AddTeamProject(ctx, t2.ID, other))
	f.ok(f.AddTeamMember(ctx, t1.ID, f.uid["ana"]))
	f.ok(f.SetMember(ctx, other, f.uid["ana"], Developer)) // ana sees both projects, belongs to one team
	f.ok(f.SetTeamPath(ctx, t1.ID, " Eureko/Backend "))
	f.ok(f.SetTeamPath(ctx, t2.ID, "Eureko/Backend/Sub"))
	for _, bad := range []string{"/x", "x/", "a\x01b", strings.Repeat("a", 129)} {
		is(t, f.SetTeamPath(ctx, t1.ID, bad), ErrInvalid{"path"})
	}
	is(t, f.SetTeamPath(ctx, 999, "x"), ErrNotFound)
	list, err := f.ListTeams(ctx, f.p("ana"), false)
	f.ok(err)
	if len(list) != 1 || list[0].Path != "eureko/backend" {
		t.Fatal(list)
	}
	slugs := func(user, prefix string) []string {
		t.Helper()
		ps, err := f.ListProjectsByPath(ctx, f.p(user), prefix, false)
		f.ok(err)
		out := []string{}
		for _, p := range ps {
			out = append(out, p.Slug)
		}
		return out
	}
	if got := slugs("ana", "Eureko"); !reflect.DeepEqual(got, []string{"fakture"}) { // team Other is not ana's team
		t.Fatal(got)
	}
	f.ok(f.AddTeamMember(ctx, t2.ID, f.uid["ana"]))
	if got := slugs("ana", "eureko"); !reflect.DeepEqual(got, []string{"eureko", "fakture"}) {
		t.Fatal(got)
	}
	if got := slugs("ana", "eureko/backend/sub"); !reflect.DeepEqual(got, []string{"eureko"}) {
		t.Fatal(got)
	}
	if got := slugs("ana", "eureko/back"); len(got) != 0 { // segment boundary
		t.Fatal(got)
	}
	// The label never widens visibility: a team member without access to the project sees nothing.
	f.ok(f.AddTeamMember(ctx, t1.ID, f.uid["dana"]))
	if got := slugs("dana", "eureko"); len(got) != 0 {
		t.Fatal("label widened visibility", got)
	}
	f.ok(f.SetTeamArchived(ctx, t2.ID, true))
	if got := slugs("ana", "eureko/backend/sub"); len(got) != 0 {
		t.Fatal("archived team", got)
	}
	f.ok(f.SetTeamPath(ctx, t1.ID, ""))
	if got := slugs("ana", "eureko"); len(got) != 0 {
		t.Fatal("cleared label", got)
	}
}

func TestFAQViewVisibility(t *testing.T) {
	f := newFx(t)
	project := f.pid["fakture"]
	ana, bor, fani := f.p("ana"), f.p("bor"), f.p("fani")
	mk := func(p Principal, key string, class Class, title string) Note {
		t.Helper()
		v, _, err := f.Submit(ctx, p, project, key, SubmissionInput{NoteInput: NoteInput{Class: class, Title: title, Body: "answer " + title, EntryKind: KindFAQ}})
		f.ok(err)
		v, err = f.ReviewSubmission(ctx, v.ID, true, "", f.seen(v.ID))
		f.ok(err)
		f.clock = f.clock.Add(1e9)
		n, err := f.GetNote(ctx, p, project, v.NoteID)
		f.ok(err)
		return n
	}
	shared := mk(ana, "a", Structure, "shared?")
	private := mk(ana, "b", Private, "private?")
	f.mk("ana", "fakture", Structure) // a finding never shows
	titles := func(p Principal) []string {
		t.Helper()
		pg, err := f.ListFAQ(ctx, p, project, "")
		f.ok(err)
		out := []string{}
		for _, e := range pg.Entries {
			out = append(out, e.Title)
		}
		return out
	}
	if got := titles(ana); !reflect.DeepEqual(got, []string{"private?", "shared?"}) {
		t.Fatal(got)
	}
	if got := titles(fani); len(got) != 2 {
		t.Fatal("admin sees private", got)
	}
	if got := titles(bor); !reflect.DeepEqual(got, []string{"shared?"}) {
		t.Fatal("private leaked to another developer", got)
	}
	if hits, _ := f.SearchNotes(ctx, bor, project, "private"); len(hits.Hits) != 0 {
		t.Fatal("private faq in search", hits)
	}
	_, err := f.ListFAQ(ctx, f.p("dana"), project, "")
	is(t, err, ErrNotFound)
	_ = shared
	_ = private
	// Paging: 50 per page.
	for i := 0; i < 52; i++ {
		keySeq++
		v, _, err := f.Submit(ctx, ana, project, "p"+i64(int64(keySeq)), SubmissionInput{NoteInput: NoteInput{Class: Structure, Title: "q" + i64(int64(i)), Body: "a", EntryKind: KindFAQ}})
		f.ok(err)
		_, err = f.ReviewSubmission(ctx, v.ID, true, "", f.seen(v.ID))
		f.ok(err)
	}
	pg, err := f.ListFAQ(ctx, bor, project, "")
	f.ok(err)
	if len(pg.Entries) != 50 || pg.Next == "" {
		t.Fatal(len(pg.Entries), pg.Next)
	}
	rest, err := f.ListFAQ(ctx, bor, project, pg.Next)
	f.ok(err)
	if len(rest.Entries) != 3 || rest.Next != "" {
		t.Fatal(len(rest.Entries), rest.Next)
	}
	_, err = f.ListFAQ(ctx, ana, project, pg.Next)
	is(t, err, ErrInvalid{"cursor"})
}

func TestBundleCarriesKindsRemotesAndPath(t *testing.T) {
	f := newFx(t)
	project := f.pid["fakture"]
	team, err := f.CreateTeam(ctx, "Team")
	f.ok(err)
	f.ok(f.AddTeamProject(ctx, team.ID, project))
	f.ok(f.AddTeamMember(ctx, team.ID, f.uid["ana"]))
	f.ok(f.SetTeamPath(ctx, team.ID, "Eureko/Core"))
	_, err = f.ChangeProjectRemotes(ctx, f.p("fani"), project, []string{"git@host.example:Eureko/Fakture.git"}, nil)
	f.ok(err)
	v, _, err := f.Submit(ctx, f.p("ana"), project, "faq", SubmissionInput{NoteInput: NoteInput{Class: Structure, Title: "Q?", Body: "A.", EntryKind: KindFAQ}})
	f.ok(err)
	_, err = f.ReviewSubmission(ctx, v.ID, true, "", f.seen(v.ID))
	f.ok(err)
	f.mk("ana", "fakture", Structure)
	file := filepath.Join(t.TempDir(), "b.zip")
	f.ok(f.ExportBundle(ctx, BundleExport{Team: "Team", Path: file}))
	b, err := readBundle(file)
	f.ok(err)
	if b.manifest.TeamPath != "eureko/core" || !reflect.DeepEqual(b.manifest.Projects[0].Remotes, []string{"host.example/eureko/fakture"}) {
		t.Fatal(b.manifest)
	}
	target := openT(t, t.TempDir())
	defer target.Close()
	dry, err := target.ImportBundle(ctx, BundleImport{Path: file, Source: "src", DryRun: true})
	f.ok(err)
	if !strings.Contains(strings.Join(dry.Lines, "\n"), "create remote fakture host.example/eureko/fakture") || countTable(t, target, "project_remotes") != 0 {
		t.Fatal(dry)
	}
	_, err = target.ImportBundle(ctx, BundleImport{Path: file, Source: "src"})
	f.ok(err)
	var path, kinds string
	f.ok(target.DB().QueryRow(`SELECT (SELECT path FROM teams),(SELECT group_concat(entry_kind) FROM (SELECT entry_kind FROM notes ORDER BY title))`).Scan(&path, &kinds))
	if path != "eureko/core" || !strings.Contains(kinds, "faq") || countTable(t, target, "project_remotes") != 1 {
		t.Fatal(path, kinds)
	}
	// Re-import: remotes by union (skip), a team path that is already set is kept and reported.
	f.ok(target.SetTeamPath(ctx, 1, "Other/Place"))
	rep, err := target.ImportBundle(ctx, BundleImport{Path: file, Source: "src"})
	f.ok(err)
	if all := strings.Join(rep.Lines, "\n"); !strings.Contains(all, "skip team path eureko/core") || !strings.Contains(all, "skip remote fakture") || strings.Contains(all, "create remote") {
		t.Fatal(all)
	}
	f.ok(target.DB().QueryRow(`SELECT path FROM teams`).Scan(&path))
	if path != "other/place" || countTable(t, target, "project_remotes") != 1 {
		t.Fatal(path)
	}
	// An existing team with an empty label takes the bundle's.
	f.ok(target.SetTeamPath(ctx, 1, ""))
	_, err = target.ImportBundle(ctx, BundleImport{Path: file, Source: "src"})
	f.ok(err)
	f.ok(target.DB().QueryRow(`SELECT path FROM teams`).Scan(&path))
	if path != "eureko/core" {
		t.Fatal(path)
	}
}

// A bundle made by 0.7.11 has none of the new fields; bad values in the new ones are refused.
func TestBundleWithoutAndWithBadNewFields(t *testing.T) {
	_, file := bundleFixture(t)
	b, err := readBundle(file)
	if err != nil {
		t.Fatal(err)
	}
	strip := func(files map[string][]byte) {
		for name, data := range files {
			if strings.HasSuffix(name, ".md") && name != "INDEX.md" {
				files[name] = []byte(strings.Replace(string(data), "entry_kind: \"finding\"\n", "", 1))
			}
		}
	}
	old := rewriteZip(t, b, strip)
	raw, _ := os.ReadFile(old)
	if strings.Contains(string(raw), "team_path") {
		t.Fatal("manifest of a plain export has the new fields")
	}
	s := openT(t, t.TempDir())
	defer s.Close()
	if _, err = s.ImportBundle(ctx, BundleImport{Path: old, Source: "old"}); err != nil {
		t.Fatal(err)
	}
	var kind string
	if err = s.DB().QueryRow(`SELECT entry_kind FROM notes`).Scan(&kind); err != nil || kind != "finding" {
		t.Fatal(kind, err)
	}
	refuse := func(name string, mutate func(*bundle), files func(map[string][]byte)) {
		t.Helper()
		bb, _ := readBundle(file)
		if mutate != nil {
			mutate(&bb)
		}
		bad := rewriteZip(t, bb, files)
		_, err := openTWith(t).ImportBundle(ctx, BundleImport{Path: bad, Source: "x"})
		var inv ErrInvalid
		if !errors.As(err, &inv) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	refuse("entry_kind", nil, func(files map[string][]byte) {
		for name, data := range files {
			if strings.HasSuffix(name, ".md") && name != "INDEX.md" {
				files[name] = []byte(strings.Replace(string(data), "entry_kind: \"finding\"", "entry_kind: \"rumor\"", 1))
			}
		}
	})
	refuse("remotes over 8", func(bb *bundle) {
		for i := 0; i < 9; i++ {
			bb.manifest.Projects[0].Remotes = append(bb.manifest.Projects[0].Remotes, "host.example/o/r"+string(rune('a'+i)))
		}
	}, nil)
	refuse("bad remote", func(bb *bundle) { bb.manifest.Projects[0].Remotes = []string{"/local/path"} }, nil)
	refuse("bad team path", func(bb *bundle) { bb.manifest.TeamPath = "/x" }, nil)
}

func openTWith(t *testing.T) *Store {
	s := openT(t, t.TempDir())
	t.Cleanup(func() { s.Close() })
	return s
}

// A replacement or a re-import without a kind inherits the kind of the published note, and with it the faq limits.
func TestInheritedFAQKeepsItsLimitsAndKind(t *testing.T) {
	f := newFx(t)
	project := f.pid["fakture"]
	ana := f.p("ana")
	v, _, err := f.Submit(ctx, ana, project, "k", SubmissionInput{NoteInput: NoteInput{Class: Structure, Title: "Q?", Body: "A.", EntryKind: KindFAQ}})
	f.ok(err)
	v, err = f.ReviewSubmission(ctx, v.ID, true, "", f.seen(v.ID))
	f.ok(err)
	n, err := f.GetNote(ctx, ana, project, v.NoteID)
	f.ok(err)
	long := strings.Repeat("a", maxFAQBody+1)
	_, _, err = f.Submit(ctx, ana, project, "r", SubmissionInput{NoteInput: NoteInput{Class: Structure, Title: "Q?", Body: long}, Replaces: n.ID, Revision: n.Revision})
	is(t, err, ErrInvalid{"faq answer: at most 2048 bytes"})
	// an explicit finding does not shed the limit or the kind either
	r, _, err := f.Submit(ctx, ana, project, "r2", SubmissionInput{NoteInput: NoteInput{Class: Structure, Title: "Q?", Body: "B.", EntryKind: KindFinding}, Replaces: n.ID, Revision: n.Revision})
	f.ok(err)
	if r.EntryKind != KindFAQ {
		t.Fatal(r.EntryKind)
	}
	// backstop: an oversized body stored for a faq is refused on accept
	f.ok(f.write(ctx, func(tx *sql.Tx) error {
		_, e := tx.Exec(`UPDATE kb_submissions SET body=? WHERE id=?`, long, r.ID)
		return e
	}))
	_, err = f.ReviewSubmission(ctx, r.ID, true, "", f.seen(r.ID))
	is(t, err, ErrInvalid{"faq answer: at most 2048 bytes"})

	// Markdown re-import without entry_kind: same limits; a different entry_kind never changes the kind.
	md := func(kind, body string) MarkdownInput {
		front := "---\ntitle: \"Imported Q?\"\n"
		if kind != "" {
			front += "entry_kind: \"" + kind + "\"\n"
		}
		return MarkdownInput{Markdown: front + "---\n" + body, Path: "q.md", Class: Structure}
	}
	_, err = f.ImportMarkdown(ctx, ana, project, md("faq", "short"))
	f.ok(err)
	_, err = f.ImportMarkdown(ctx, ana, project, md("", long))
	is(t, err, ErrInvalid{"faq answer: at most 2048 bytes"})
	_, err = f.ImportMarkdown(ctx, ana, project, md("finding", "changed"))
	f.ok(err)
	var kind string
	f.ok(f.DB().QueryRow(`SELECT entry_kind FROM notes WHERE title='Imported Q?'`).Scan(&kind))
	if kind != KindFAQ {
		t.Fatal("import changed the kind of a published note:", kind)
	}
}

func TestRemoveRemoteByIdentityOrUrl(t *testing.T) {
	f := newFx(t)
	project := f.pid["fakture"]
	fani := f.p("fani")
	_, err := f.ChangeProjectRemotes(ctx, fani, project, []string{"git@host.example:a/b.git", "git@host.example:c/d.git"}, nil)
	f.ok(err)
	got, err := f.ChangeProjectRemotes(ctx, fani, project, nil, []string{"host.example/a/b"})
	f.ok(err)
	if !reflect.DeepEqual(got, []string{"host.example/c/d"}) {
		t.Fatal(got)
	}
	got, err = f.ChangeProjectRemotes(ctx, fani, project, nil, []string{"https://tok@host.example/c/d.git"})
	f.ok(err)
	if len(got) != 0 {
		t.Fatal(got)
	}
	_, err = f.ChangeProjectRemotes(ctx, fani, project, nil, []string{"not-registered"})
	is(t, err, ErrInvalid{"git remote"}) // not an identity of this project and not a URL
}
