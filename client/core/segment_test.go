package core

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// segFx: team acme = projects fakture; roster ana (developer in fakture), fani (admin of fakture), eva (reader of fakture);
// bor is a developer of fakture but NOT on the roster; team zeta = project import, roster dana (admin of import).
type segTeams struct {
	*fx
	acme, zeta int64
	seg, zseg  int64
}

func newSeg(t *testing.T) *segTeams {
	f := &segTeams{fx: newFx(t)}
	a, e := f.CreateTeam(ctx, "acme")
	f.ok(e)
	z, e := f.CreateTeam(ctx, "zeta")
	f.ok(e)
	f.acme, f.zeta, f.seg, f.zseg = a.ID, z.ID, segmentID(a.ID), segmentID(z.ID)
	for _, u := range []string{"ana", "fani", "eva"} {
		f.ok(f.AddTeamMember(ctx, f.acme, f.uid[u]))
	}
	f.ok(f.AddTeamProject(ctx, f.acme, f.pid["fakture"]))
	f.ok(f.AddTeamMember(ctx, f.zeta, f.uid["dana"]))
	f.ok(f.SetMember(ctx, f.pid["import"], f.uid["dana"], Admin))
	f.ok(f.AddTeamProject(ctx, f.zeta, f.pid["import"]))
	return f
}

var segKey int

// put writes a segment note as fani (the team's writer).
func (f *segTeams) put(user string, c Class, title string) Note {
	f.t.Helper()
	segKey++
	f.clock = f.clock.Add(time.Second)
	n, _, err := f.CreateNote(ctx, f.p(user), f.seg, fmt.Sprint("sk", segKey), NoteInput{Class: c, Title: title, Body: "deploy order " + title})
	f.ok(err)
	return n
}

func (f *segTeams) hits(user string, project int64, q string) []SearchHit {
	f.t.Helper()
	pg, err := f.SearchNotes(ctx, f.p(user), project, q)
	f.ok(err)
	return pg.Hits
}

func TestSegmentMigration15(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, dbFile))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations[:14] {
		if err = m(tx); err != nil {
			t.Fatal(err)
		}
	}
	if err = setMeta(tx, "schema", "14"); err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{`INSERT INTO teams(name) VALUES('acme'),('zeta')`,
		`INSERT INTO users(id,name) VALUES(1,'ana')`, `INSERT INTO projects(id,slug,name) VALUES(1,'shop','Shop')`,
		`INSERT INTO notes(project_id,class,title,body,bytes,author_id,created,updated) VALUES(1,'data','t','b',2,1,1,1)`} {
		if _, err = tx.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s := openT(t, dir)
	if snaps, _ := filepath.Glob(filepath.Join(dir, "core-14-*.db")); len(snaps) != 1 {
		t.Fatal("no core-14 snapshot", snaps)
	}
	var n int
	if err = s.DB().QueryRow(`SELECT count(*) FROM projects WHERE id IN (?,?) AND slug IN ('@team-1','@team-2') AND archived=0`, segmentID(1), segmentID(2)).Scan(&n); err != nil || n != 2 {
		t.Fatal("segment rows", n, err)
	}
	if err = s.read(ctx, checkSegments); err != nil {
		t.Fatal(err)
	}
	if err = checkSchemaObjects(ctx, s.DB(), len(migrations)); err != nil {
		t.Fatal(err)
	}
	if err = s.CheckMemberships(ctx); err != nil {
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
	if err = s.read(ctx, checkSegments); err != nil {
		t.Fatal(err)
	}
	// A team made without its row must fail loudly.
	exec(t, s, `INSERT INTO teams(name) VALUES('lost')`)
	if err = s.read(ctx, checkSegments); err == nil {
		t.Fatal("missing segment not reported")
	}
}

func TestSegmentReadersAndWriters(t *testing.T) {
	f := newSeg(t)
	s, d := f.put("fani", Structure, "layout"), f.put("fani", Data, "runbook")
	for _, u := range []string{"ana", "eva", "fani"} { // the roster reads both classes
		pg, e := f.ListNotes(ctx, f.p(u), f.seg, "", "", 50)
		f.ok(e)
		if len(pg.Notes) != 2 {
			t.Fatal(u, pg.Notes)
		}
		for _, n := range []Note{s, d} {
			if _, e = f.GetNote(ctx, f.p(u), f.seg, n.ID); e != nil {
				t.Fatal(u, e)
			}
		}
	}
	// A roster member who is not admin of a team project reads but writes nothing.
	for _, u := range []string{"ana", "eva"} {
		_, _, e := f.CreateNote(ctx, f.p(u), f.seg, "x"+u, NoteInput{Class: Data, Title: "t", Body: "b"})
		is(t, e, ErrForbidden)
		_, e = f.UpdateNote(ctx, f.p(u), f.seg, s.ID, s.Revision, NoteInput{Class: Structure, Title: "t2"})
		is(t, e, ErrForbidden)
		is(t, f.DeleteNote(ctx, f.p(u), f.seg, s.ID, s.Revision), ErrForbidden)
		_, _, e = f.Submit(ctx, f.p(u), f.seg, "s"+u, SubmissionInput{NoteInput: NoteInput{Class: Data, Title: "t", Body: "b"}})
		is(t, e, ErrForbidden)
		_, e = f.ImportMarkdown(ctx, f.p(u), f.seg, MarkdownInput{Markdown: "b", Path: "a.md", Class: Data})
		is(t, e, ErrForbidden)
	}
	// The team's project admin writes: update, import, delete.
	u, e := f.UpdateNote(ctx, f.p("fani"), f.seg, s.ID, s.Revision, NoteInput{Class: Data, Title: "layout2", Body: "b"})
	f.ok(e)
	_, e = f.ImportMarkdown(ctx, f.p("fani"), f.seg, MarkdownInput{Markdown: "imported", Path: "a.md", Class: Data})
	f.ok(e)
	f.ok(f.DeleteNote(ctx, f.p("fani"), f.seg, u.ID, u.Revision))
	// A submission to the segment is reviewed by the owner like any other.
	v, _, e := f.Submit(ctx, f.p("fani"), f.seg, "sub1", SubmissionInput{NoteInput: NoteInput{Class: Data, Title: "sub", Body: "b"}})
	f.ok(e)
	if v.Project != f.seg {
		t.Fatal(v)
	}
	if v, e = f.ReviewSubmission(ctx, v.ID, true, ""); e != nil || v.NoteID == 0 {
		t.Fatal(v, e)
	}
	// Without the roster entry the same admin of fakture would read nothing; the roster is the only door.
	f.ok(f.RemoveTeamMember(ctx, f.acme, f.uid["fani"]))
	_, e = f.ListNotes(ctx, f.p("fani"), f.seg, "", "", 50)
	is(t, e, ErrNotFound)
	_, _, e = f.CreateNote(ctx, f.p("fani"), f.seg, "again", NoteInput{Class: Data, Title: "t", Body: "b"})
	is(t, e, ErrNotFound)
	f.ok(f.AddTeamMember(ctx, f.acme, f.uid["fani"]))
	// The admin upgrade is scoped to THIS team: eva is on acme's roster, only a reader of fakture, and admin of
	// import, a project of team zeta. She must not write acme's segment.
	f.ok(f.SetMember(ctx, f.pid["import"], f.uid["eva"], Admin))
	_, _, e = f.CreateNote(ctx, f.p("eva"), f.seg, "eva1", NoteInput{Class: Data, Title: "t", Body: "b"})
	is(t, e, ErrForbidden)
	_, e = f.UpdateNote(ctx, f.p("eva"), f.seg, d.ID, d.Revision, NoteInput{Class: Data, Title: "t", Body: "b"})
	is(t, e, ErrForbidden)
	// An admin role that comes only from a team assignment counts (team-derived roles count as elsewhere).
	f.ok(f.SetTeamAssignment(ctx, f.acme, f.pid["fakture"], f.uid["ana"], Admin))
	if _, _, e = f.CreateNote(ctx, f.p("ana"), f.seg, "ana-assigned", NoteInput{Class: Data, Title: "t", Body: "b"}); e != nil {
		t.Fatal("team-assigned admin cannot write", e)
	}
	// Removing the admin role of the last team project takes effect on the next call.
	f.ok(f.SetMember(ctx, f.pid["fakture"], f.uid["fani"], Developer))
	_, _, e = f.CreateNote(ctx, f.p("fani"), f.seg, "again2", NoteInput{Class: Data, Title: "t", Body: "b"})
	is(t, e, ErrForbidden)
}

// Each of these must fail if the roster check of loadSegmentAccess is removed.
func TestSegmentNonMemberReadsNothing(t *testing.T) {
	f := newSeg(t)
	n := f.put("fani", Data, "secret deploy")
	f.put("fani", Structure, "secret layout")
	v, _, e := f.Submit(ctx, f.p("fani"), f.seg, "sub", SubmissionInput{NoteInput: NoteInput{Class: Data, Title: "pending", Body: "b"}})
	f.ok(e)
	// bor is a developer of fakture (the team's project) but not on the roster; dana/cene are in no part of acme.
	for _, u := range []string{"bor", "cene", "dana"} {
		p := f.p(u)
		_, e := f.ListNotes(ctx, p, f.seg, "", "", 50)
		is(t, e, ErrNotFound)
		_, e = f.GetNote(ctx, p, f.seg, n.ID)
		is(t, e, ErrNotFound)
		_, e = f.ListFAQ(ctx, p, f.seg, "")
		is(t, e, ErrNotFound)
		_, e = f.SearchNotes(ctx, p, f.seg, "secret")
		is(t, e, ErrNotFound)
		_, e = f.GetSubmission(ctx, p, f.seg, v.ID, false)
		is(t, e, ErrNotFound)
		_, e = f.ListSubmissions(ctx, p, f.seg, 0, false)
		is(t, e, ErrNotFound)
		_, _, e = f.CreateNote(ctx, p, f.seg, "k"+u, NoteInput{Class: Data, Title: "t", Body: "b"})
		is(t, e, ErrNotFound)
		_, e = f.UpdateNote(ctx, p, f.seg, n.ID, n.Revision, NoteInput{Class: Data, Title: "t"})
		is(t, e, ErrNotFound)
		is(t, f.DeleteNote(ctx, p, f.seg, n.ID, n.Revision), ErrNotFound)
		// The note id of the segment cannot be opened through another project the caller reads, either.
		for _, proj := range []string{"fakture", "eureko", "import"} {
			if g, e := f.GetNote(ctx, p, f.pid[proj], n.ID); e == nil {
				t.Fatal(u, proj, "read a segment note", g.ID)
			}
			if u == "dana" && proj != "import" || u == "cene" && proj != "eureko" || u == "bor" && proj != "fakture" {
				continue
			}
			for _, h := range f.hits(u, f.pid[proj], "secret") {
				t.Fatal(u, proj, "search found", h)
			}
		}
	}
	// The run channel: bor works in fakture but is not on the roster.
	p := f.p("bor")
	cap := RunCap{UserID: p.UserID, DeviceID: p.DeviceID, ProjectID: f.pid["fakture"], Epoch: f.epoch.Load()}
	rs, e := f.RunSearchNotes(ctx, cap, true, "secret")
	f.ok(e)
	if len(rs.Hits) != 0 {
		t.Fatal(rs.Hits)
	}
	_, e = f.RunGetNote(ctx, cap, true, n.ID)
	is(t, e, ErrNotFound)
	// An admin of a project of ANOTHER team does not get in.
	_, e = f.ListNotes(ctx, f.p("dana"), f.seg, "", "", 50)
	is(t, e, ErrNotFound)
	_, e = f.ListNotes(ctx, f.p("ana"), f.zseg, "", "", 50)
	is(t, e, ErrNotFound)
	_, e = f.GetNote(ctx, f.p("ana"), f.zseg, n.ID)
	is(t, e, ErrNotFound)
}

func TestSegmentSearchReadAndRun(t *testing.T) {
	f := newSeg(t)
	own := f.mk("ana", "fakture", Data) // title t-data
	team := f.put("fani", Data, "t-data team")
	f.put("fani", Structure, "elsewhere")
	pg, e := f.SearchNotes(ctx, f.p("ana"), f.pid["fakture"], "data")
	f.ok(e)
	if len(pg.Hits) != 2 || pg.Hits[0].ID != own.ID || pg.Hits[0].From != "" || pg.Hits[1].ID != team.ID || pg.Hits[1].From != "team:acme" {
		t.Fatalf("home first, then the team: %+v", pg.Hits)
	}
	// Home hits come first even when the team note is newer than the home one (it is: created after).
	if !pg.Hits[1].Updated.After(pg.Hits[0].Updated) {
		t.Fatal("test needs the team note to be newer", pg.Hits)
	}
	// Searching the segment itself yields only the segment.
	if h := f.hits("ana", f.seg, "data"); len(h) != 1 || h[0].ID != team.ID || h[0].From != "team:acme" {
		t.Fatal(h)
	}
	// A project outside the team does not see the segment.
	if h := f.hits("cene", f.pid["eureko"], "t-data"); len(h) != 0 {
		t.Fatal(h)
	}
	// note read: a hit of the segment opens through the project, a note of a non-source project does not.
	if g, e := f.GetNote(ctx, f.p("ana"), f.pid["fakture"], team.ID); e != nil || g.ID != team.ID {
		t.Fatal(g, e)
	}
	other := f.mk("cene", "eureko", Data)
	_, e = f.GetNote(ctx, f.p("ana"), f.pid["fakture"], other.ID)
	is(t, e, ErrNotFound)
	// list and faq of a project stay the one project.
	l, e := f.ListNotes(ctx, f.p("ana"), f.pid["fakture"], "", "", 50)
	f.ok(e)
	if len(l.Notes) != 1 || l.Notes[0].ID != own.ID {
		t.Fatal(l.Notes)
	}
	// The run channel: the run's user on the roster gets the segment in search and read, not in list.
	p := f.p("ana")
	cap := RunCap{UserID: p.UserID, DeviceID: p.DeviceID, ProjectID: f.pid["fakture"], Epoch: f.epoch.Load()}
	rs, e := f.RunSearchNotes(ctx, cap, true, "data")
	f.ok(e)
	if len(rs.Hits) != 2 || rs.Hits[1].From != "team:acme" {
		t.Fatal(rs.Hits)
	}
	if g, e := f.RunGetNote(ctx, cap, true, team.ID); e != nil || g.ID != team.ID {
		t.Fatal(g, e)
	}
	rl, e := f.RunListNotes(ctx, cap, true, "", "", 50)
	f.ok(e)
	for _, n := range rl.Notes {
		if n.ProjectID == f.seg {
			t.Fatal("run list shows the segment")
		}
	}
	// A team member removed from the roster: gone on the next call, member and run.
	f.ok(f.RemoveTeamMember(ctx, f.acme, f.uid["ana"]))
	if h := f.hits("ana", f.pid["fakture"], "data"); len(h) != 1 || h[0].From != "" {
		t.Fatal(h)
	}
	cap.Epoch = f.epoch.Load()
	if rs, e = f.RunSearchNotes(ctx, cap, true, "data"); e != nil || len(rs.Hits) != 1 {
		t.Fatal(rs, e)
	}
	_, e = f.RunGetNote(ctx, cap, true, team.ID)
	is(t, e, ErrNotFound)
}

func TestSegmentNoPrivate(t *testing.T) {
	f := newSeg(t)
	_, _, e := f.CreateNote(ctx, f.p("fani"), f.seg, "p1", NoteInput{Class: Private, Title: "t", Body: "b"})
	is(t, e, ErrForbidden)
	n := f.put("fani", Data, "n")
	_, e = f.UpdateNote(ctx, f.p("fani"), f.seg, n.ID, n.Revision, NoteInput{Class: Private, Title: "n", Body: "b"})
	is(t, e, ErrForbidden)
	_, _, e = f.Submit(ctx, f.p("fani"), f.seg, "p2", SubmissionInput{NoteInput: NoteInput{Class: Private, Title: "t", Body: "b"}})
	is(t, e, ErrForbidden)
	_, e = f.ImportMarkdown(ctx, f.p("fani"), f.seg, MarkdownInput{Markdown: "b", Path: "p.md", Class: Private})
	is(t, e, ErrForbidden)
	// canMove is refused on its own too (an admin may otherwise move anything).
	if (access{Role: Admin, NoPrivate: true}).canMove(Data, Private, true) {
		t.Fatal("canMove into private")
	}
}

func TestSegmentArchivedTeam(t *testing.T) {
	f := newSeg(t)
	n := f.put("fani", Data, "archive me")
	f.ok(f.SetTeamArchived(ctx, f.acme, true))
	_, e := f.ListNotes(ctx, f.p("fani"), f.seg, "", "", 50)
	is(t, e, ErrNotFound)
	_, e = f.GetNote(ctx, f.p("fani"), f.seg, n.ID)
	is(t, e, ErrNotFound)
	_, _, e = f.CreateNote(ctx, f.p("fani"), f.seg, "ka", NoteInput{Class: Data, Title: "t", Body: "b"})
	is(t, e, ErrNotFound)
	if h := f.hits("fani", f.pid["fakture"], "archive"); len(h) != 0 {
		t.Fatal(h)
	}
	if err := f.read(ctx, checkSegments); err != nil {
		t.Fatal(err)
	}
	f.ok(f.SetTeamArchived(ctx, f.acme, false))
	if h := f.hits("ana", f.pid["fakture"], "archive"); len(h) != 1 {
		t.Fatal(h)
	}
}

func TestSegmentDeactivatedAndRevoked(t *testing.T) {
	f := newSeg(t)
	n := f.put("fani", Data, "n")
	pa, pe := f.p("ana"), f.p("eva")
	f.ok(f.SetUserActive(ctx, f.uid["ana"], false))
	_, e := f.GetNote(ctx, pa, f.seg, n.ID)
	is(t, e, ErrNoProjectAccess)
	f.ok(f.RemoveBinding(ctx, "dev-eva"))
	_, e = f.ListNotes(ctx, pe, f.seg, "", "", 50)
	is(t, e, ErrNoProjectAccess)
}

func TestSegmentLimitsPerSegment(t *testing.T) {
	f := newSeg(t)
	old := maxNotes
	maxNotes = 2
	defer func() { maxNotes = old }()
	f.put("fani", Data, "one")
	f.put("fani", Data, "two")
	_, _, e := f.CreateNote(ctx, f.p("fani"), f.seg, "three", NoteInput{Class: Data, Title: "t", Body: "b"})
	is(t, e, ErrTooLarge)
	f.mk("ana", "fakture", Data) // a real project has its own count
}

// A segment id is not a project: every place that takes or lists a project refuses it.
func TestSegmentIsNotAProject(t *testing.T) {
	f := newSeg(t)
	f.put("fani", Data, "x")
	segStr, slug := fmt.Sprint(f.seg), segmentSlug(f.acme)
	for _, key := range []string{segStr, slug} {
		_, e := f.GetProject(ctx, f.p("fani"), key)
		is(t, e, ErrNotFound)
		_, e = f.ResolveForLink(ctx, f.p("fani"), key)
		is(t, e, ErrNotFound)
	}
	is(t, f.SetMember(ctx, f.seg, f.uid["ana"], Reader), ErrNotFound)
	is(t, f.AddGrant(ctx, f.seg, f.pid["fakture"], Data), ErrNotFound)
	is(t, f.AddGrant(ctx, f.pid["fakture"], f.seg, Data), ErrNotFound)
	_, e := f.AdminProjectRemotes(ctx, f.seg)
	is(t, e, ErrNotFound)
	_, e = f.AdminChangeProjectRemotes(ctx, f.seg, []string{"github.com/acme/shop"}, nil)
	is(t, e, ErrNotFound)
	_, e = f.ProjectRemotes(ctx, f.p("fani"), f.seg)
	is(t, e, ErrNotFound)
	_, e = f.ChangeProjectRemotes(ctx, f.p("fani"), f.seg, []string{"github.com/acme/shop"}, nil)
	is(t, e, ErrNotFound)
	is(t, f.AddTeamProject(ctx, f.acme, f.seg), ErrNotFound)
	is(t, f.SetProjectArchived(ctx, f.seg, true), ErrNotFound)
	_, e = f.Sessions(ctx, f.p("fani"), f.seg, SessionFilter{})
	is(t, e, ErrNotFound)
	_, e = f.Usage(ctx, f.p("fani"), f.seg, 7)
	is(t, e, ErrNotFound)
	p := f.p("fani")
	_, e = f.RunSearchNotes(ctx, RunCap{UserID: p.UserID, DeviceID: p.DeviceID, ProjectID: f.seg, Epoch: f.epoch.Load()}, true, "x")
	if e == nil {
		t.Fatal("a run on a segment")
	}
	// The database refuses what the code guards miss (triggers).
	for _, q := range []string{
		`INSERT INTO direct_memberships(project_id,user_id,role) VALUES(` + segStr + `,1,'reader')`,
		`INSERT INTO memberships(project_id,user_id,role) VALUES(` + segStr + `,1,'reader')`,
		`INSERT INTO grants(source_id,reader_id,class) VALUES(` + segStr + `,1,'data')`,
		`INSERT INTO grants(source_id,reader_id,class) VALUES(1,` + segStr + `,'data')`,
		`INSERT INTO project_remotes(project_id,identity) VALUES(` + segStr + `,'github.com/acme/shop')`,
		`INSERT INTO team_projects(team_id,project_id) VALUES(` + fmt.Sprint(f.zeta) + `,` + segStr + `)`,
		`INSERT INTO team_assignments(team_id,project_id,user_id,role) VALUES(1,` + segStr + `,1,'reader')`,
		`UPDATE projects SET archived=1 WHERE id=` + segStr,
		`INSERT INTO projects(id,slug,name) VALUES(` + fmt.Sprint(segmentBase+9999) + `,'shop-x','')`,
		`INSERT INTO projects(id,slug,name) VALUES(777,'@team-777','')`,
	} {
		err := f.write(ctx, func(tx *sql.Tx) error { _, e := tx.Exec(q); return e })
		if err == nil {
			t.Fatal("accepted:", q)
		}
	}
}

// One table-driven pass over EVERY listing that reads projects: no segment row, no id above segmentBase.
func TestSegmentNeverInAListing(t *testing.T) {
	f := newSeg(t)
	f.put("fani", Data, "x")
	f.ok(f.AddGrant(ctx, f.pid["fakture"], f.pid["eureko"], Data))
	f.ok(f.SetTeamPath(ctx, f.acme, "co/acme"))
	check := func(name string, ids ...int64) {
		t.Helper()
		for _, id := range ids {
			if isSegment(id) {
				t.Fatalf("%s lists segment id %d", name, id)
			}
		}
	}
	prIDs := func(ps []Project) (out []int64) {
		for _, p := range ps {
			out = append(out, p.ID)
			if strings.HasPrefix(p.Slug, "@team") {
				t.Fatal("segment slug listed", p)
			}
		}
		return
	}
	for _, u := range []string{"ana", "fani", "eva", "bor", "cene", "dana"} {
		p := f.p(u)
		ps, e := f.ListProjects(ctx, p, true)
		f.ok(e)
		check("ListProjects "+u, prIDs(ps)...)
		_, byRemote, e := f.ProjectsByRemote(ctx, p, "https://github.com/acme/shop.git")
		f.ok(e)
		check("ProjectsByRemote", prIDs(byRemote)...)
		ps, e = f.ListProjectsByPath(ctx, p, "co", true)
		f.ok(e)
		check("ListProjectsByPath", prIDs(ps)...)
		teams, e := f.ListTeams(ctx, p, true)
		f.ok(e)
		for _, tm := range teams {
			d, e := f.GetTeam(ctx, p, fmt.Sprint(tm.ID))
			f.ok(e)
			check("GetTeam", prIDs(d.Projects)...)
		}
	}
	now := f.now()
	f.event("u1", "", f.uid["fani"], f.pid["fakture"], "m", OutcomeOK, 1, 1, now)
	for _, u := range []string{"ana", "fani", "eva", "bor"} {
		p := f.p(u)
		for _, project := range []int64{0, f.pid["fakture"]} {
			if rep, e := f.Usage(ctx, p, project, 7); e == nil {
				for _, r := range rep.Rows {
					check("Usage", r.ProjectID)
				}
			}
		}
		if pg, e := f.Sessions(ctx, p, f.pid["fakture"], SessionFilter{}); e == nil {
			for _, r := range pg.Sessions {
				check("Sessions", r.ProjectID)
			}
		}
		_, e := f.Usage(ctx, p, f.seg, 7)
		is(t, e, ErrNotFound)
		_, e = f.Sessions(ctx, p, f.seg, SessionFilter{})
		is(t, e, ErrNotFound)
	}
	if rep, e := f.AdminUsage(ctx, 7, UsageFilter{}); e != nil {
		t.Fatal(e)
	} else {
		for _, r := range rep.Rows {
			check("AdminUsage", r.ProjectID)
		}
	}
	for _, pid := range []int64{0, f.pid["fakture"]} {
		if pg, e := f.AdminSessions(ctx, pid, SessionFilter{}); e == nil {
			for _, r := range pg.Sessions {
				check("AdminSessions", r.ProjectID)
			}
		}
	}
	ps, e := f.AdminListProjects(ctx)
	f.ok(e)
	check("AdminListProjects", prIDs(ps)...)
	for _, tm := range []int64{f.acme, f.zeta} {
		d, e := f.AdminGetTeam(ctx, tm)
		f.ok(e)
		check("AdminGetTeam", prIDs(d.Projects)...)
		tp, e := f.ListTeamProjects(ctx, tm)
		f.ok(e)
		check("ListTeamProjects", prIDs(tp)...)
	}
	grants, e := f.ListGrants(ctx)
	f.ok(e)
	for _, g := range grants {
		check("ListGrants", g.SourceID, g.ReaderID)
	}
	for _, pid := range []int64{f.pid["fakture"], f.pid["eureko"], f.pid["import"]} {
		ms, e := f.ListMembers(ctx, pid)
		f.ok(e)
		_ = ms
	}
	// Bundle export of the team: no project row of a segment in the manifest.
	out := filepath.Join(t.TempDir(), "b")
	f.ok(f.ExportBundle(ctx, BundleExport{Team: "acme", Path: out}))
	b, e := readBundle(out)
	f.ok(e)
	for _, pr := range b.manifest.Projects {
		if strings.HasPrefix(pr.Slug, "@team") {
			t.Fatal("bundle lists a segment project", pr)
		}
	}
	// Direct table checks: segments are in no member, grant, remote or team-project row.
	for _, tbl := range []string{"memberships", "direct_memberships", "team_assignments", "team_projects", "project_remotes"} {
		var n int
		f.ok(f.DB().QueryRow(`SELECT count(*) FROM `+tbl+` WHERE project_id>?`, segmentBase).Scan(&n))
		if n != 0 {
			t.Fatal(tbl, n)
		}
	}
	var n int
	f.ok(f.DB().QueryRow(`SELECT count(*) FROM grants WHERE source_id>?1 OR reader_id>?1`, segmentBase).Scan(&n))
	if n != 0 {
		t.Fatal("grants", n)
	}
}

func TestSegmentRowOfCreateTeam(t *testing.T) {
	f := newSeg(t)
	if err := f.read(ctx, checkSegments); err != nil {
		t.Fatal(err)
	}
}

func TestSegmentBundleRoundTrip(t *testing.T) {
	source, _ := bundleFixture(t)
	var team int64
	source.ok(source.DB().QueryRow(`SELECT id FROM teams WHERE name='Team'`).Scan(&team))
	source.ok(source.SetTeamAssignment(ctx, team, source.pid["fakture"], source.uid["ana"], Admin))
	seg := segmentID(team)
	n, _, e := source.CreateNote(ctx, source.p("ana"), seg, "bs1", NoteInput{Class: Data, Title: "team rule", Body: "migrate first"})
	source.ok(e)
	file := filepath.Join(t.TempDir(), "with-team.zip")
	source.ok(source.ExportBundle(ctx, BundleExport{Team: "Team", Path: file, WithPrivate: true}))
	b, e := readBundle(file)
	source.ok(e)
	found := 0
	for _, bn := range b.manifest.Notes {
		if bn.Project == bundleSegment {
			found++
			if bn.File != "@team/"+fmt.Sprint(n.ID)+".md" {
				t.Fatal(bn)
			}
		}
	}
	if found != 1 || !strings.HasPrefix(string(b.files["INDEX.md"]), "# Team\n\n- [team rule](@team/") {
		t.Fatal(found, string(b.files["INDEX.md"]))
	}
	only := filepath.Join(t.TempDir(), "only-team.zip")
	source.ok(source.ExportBundle(ctx, BundleExport{Team: "Team", Projects: []string{bundleSegment}, Path: only}))
	ob, e := readBundle(only)
	source.ok(e)
	if len(ob.manifest.Projects) != 0 || len(ob.manifest.Notes) != 1 {
		t.Fatal(ob.manifest)
	}
	target := openT(t, t.TempDir())
	defer target.Close()
	dry, e := target.ImportBundle(ctx, BundleImport{Path: file, Source: "testsrc", DryRun: true})
	source.ok(e)
	if !strings.Contains(strings.Join(dry.Lines, "\n"), "create src:testsrc:@team:") || countTable(t, target, "notes") != 0 {
		t.Fatal(dry)
	}
	_, e = target.ImportBundle(ctx, BundleImport{Path: file, Source: "testsrc"})
	source.ok(e)
	var tt, pid int64
	source.ok(target.DB().QueryRow(`SELECT id FROM teams WHERE name='Team'`).Scan(&tt))
	source.ok(target.DB().QueryRow(`SELECT project_id FROM notes WHERE title='team rule'`).Scan(&pid))
	if pid != segmentID(tt) {
		t.Fatal("note landed in", pid, "not the segment of", tt)
	}
	if e = target.read(ctx, checkSegments); e != nil {
		t.Fatal(e)
	}
	var key string
	source.ok(target.DB().QueryRow(`SELECT key FROM note_imports WHERE project_id=?`, pid).Scan(&key))
	if !strings.HasPrefix(key, "src:testsrc:@team:") {
		t.Fatal(key)
	}
	before := countTable(t, target, "notes")
	_, e = target.ImportBundle(ctx, BundleImport{Path: file, Source: "testsrc"})
	source.ok(e)
	if countTable(t, target, "notes") != before {
		t.Fatal("not a no-op")
	}
	// A segment note of class private is refused.
	bad := rewriteZip(t, b, func(files map[string][]byte) {
		for name, data := range files {
			if strings.HasPrefix(name, "@team/") {
				files[name] = []byte(strings.Replace(string(data), `class: "data"`, `class: "private"`, 1))
			}
		}
	})
	t2 := openT(t, t.TempDir())
	defer t2.Close()
	rep, e := t2.ImportBundle(ctx, BundleImport{Path: bad, Source: "testsrc"})
	if e == nil || !strings.Contains(strings.Join(rep.Lines, "\n"), "cannot be private") || countTable(t, t2, "notes") != 0 || countTable(t, t2, "teams") != 0 {
		t.Fatal(rep, e)
	}
	// A note file of an unknown "project" is still refused.
	b2, e := readBundle(file)
	source.ok(e)
	for i := range b2.manifest.Notes {
		if b2.manifest.Notes[i].Project == bundleSegment {
			b2.manifest.Notes[i].Project = "@team2"
		}
	}
	if _, e = t2.ImportBundle(ctx, BundleImport{Path: rewriteZip(t, b2, nil), Source: "testsrc"}); e == nil || countTable(t, t2, "notes") != 0 {
		t.Fatal("unknown project accepted", e)
	}
}

func TestSegmentUpdateGuards(t *testing.T) {
	f := newSeg(t)
	segStr := fmt.Sprint(f.seg)
	f.ok(f.SetMember(ctx, f.pid["fakture"], f.uid["ana"], Developer))
	f.ok(f.AddGrant(ctx, f.pid["fakture"], f.pid["eureko"], Data))
	f.ok(f.AddRemote(t, f.pid["fakture"]))
	f.ok(f.SetTeamAssignment(ctx, f.acme, f.pid["fakture"], f.uid["ana"], Developer))
	for _, q := range []string{
		`UPDATE memberships SET project_id=` + segStr + ` WHERE project_id=` + fmt.Sprint(f.pid["fakture"]),
		`UPDATE direct_memberships SET project_id=` + segStr + ` WHERE project_id=` + fmt.Sprint(f.pid["fakture"]),
		`UPDATE team_projects SET project_id=` + segStr,
		`UPDATE team_assignments SET project_id=` + segStr,
		`UPDATE project_remotes SET project_id=` + segStr,
		`UPDATE grants SET source_id=` + segStr,
		`UPDATE grants SET reader_id=` + segStr,
		`UPDATE projects SET id=` + fmt.Sprint(segmentBase+777) + ` WHERE slug='eureko'`,
		`UPDATE projects SET slug='@team-5' WHERE slug='eureko'`,
		`UPDATE projects SET slug='shop-x' WHERE id=` + segStr,
		`UPDATE projects SET id=5000 WHERE id=` + segStr,
	} {
		if err := f.write(ctx, func(tx *sql.Tx) error { _, e := tx.Exec(q); return e }); err == nil {
			t.Fatal("accepted:", q)
		}
	}
}

func (f *segTeams) AddRemote(t *testing.T, project int64) error {
	_, err := f.AdminChangeProjectRemotes(ctx, project, []string{"https://github.com/acme/shop.git"}, nil)
	return err
}

// A schema-15 backup that lacks the guard triggers is refused by the restore self-check.
func TestSegmentRestoreRequiresTriggers(t *testing.T) {
	dir := t.TempDir()
	s := openT(t, dir)
	bak := filepath.Join(t.TempDir(), "b.db")
	if err := s.Backup(ctx, bak); err != nil {
		t.Fatal(err)
	}
	s.Close()
	db, err := sql.Open("sqlite", "file:"+bak)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`DROP TRIGGER segment_no_grant_source`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if err = Restore(t.TempDir(), bak); err == nil || !strings.Contains(err.Error(), "missing trigger") {
		t.Fatal("restore accepted a backup without the triggers:", err)
	}
}

// checkSegments also refuses a database in which a segment id has a member, grant, remote or team assignment
// (made here with the guard trigger absent in the test's own file).
func TestSegmentCheckRefusesReferences(t *testing.T) {
	f := newSeg(t)
	f.ok(f.write(ctx, func(tx *sql.Tx) error {
		if _, e := tx.Exec(`DROP TRIGGER segment_no_grant_source`); e != nil {
			return e
		}
		_, e := tx.Exec(`INSERT INTO grants(source_id,reader_id,class) VALUES(?,?,'data')`, f.seg, f.pid["fakture"])
		return e
	}))
	if err := f.read(ctx, checkSegments); err == nil {
		t.Fatal("a grant on a segment passed the check")
	}
	bak := filepath.Join(t.TempDir(), "g.db")
	f.ok(f.Backup(ctx, bak))
	if err := Restore(t.TempDir(), bak); err == nil {
		t.Fatal("restore accepted a segment in a grant")
	}
}

// A dry run for a team that does not exist yet reports create lines and never uses a segment id.
func TestSegmentDryRunNewTeam(t *testing.T) {
	if segmentID(-1) != 0 || segmentID(0) != 0 {
		t.Fatal("segmentID of a non-real team")
	}
	source, _ := bundleFixture(t)
	var team int64
	source.ok(source.DB().QueryRow(`SELECT id FROM teams WHERE name='Team'`).Scan(&team))
	source.ok(source.SetTeamAssignment(ctx, team, source.pid["fakture"], source.uid["ana"], Admin))
	_, _, e := source.CreateNote(ctx, source.p("ana"), segmentID(team), "dr1", NoteInput{Class: Data, Title: "team rule", Body: "b"})
	source.ok(e)
	file := filepath.Join(t.TempDir(), "t.zip")
	source.ok(source.ExportBundle(ctx, BundleExport{Team: "Team", Path: file}))
	target := openT(t, t.TempDir())
	defer target.Close()
	rep, e := target.ImportBundle(ctx, BundleImport{Path: file, Source: "testsrc", DryRun: true})
	source.ok(e)
	if !strings.Contains(strings.Join(rep.Lines, "\n"), "create src:testsrc:@team:") || countTable(t, target, "notes") != 0 {
		t.Fatal(rep)
	}
}
