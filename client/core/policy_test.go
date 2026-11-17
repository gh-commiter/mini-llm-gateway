package core

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// fx is the fixture of section 9: projects fakture, eureko (structure grant from fakture), import (structure grant
// from eureko only); users ana, bor (developers of fakture), fani (admin), eva (reader), cene (developer of
// eureko), dana (developer of import); one device each, ana has two (dev-ana, dev-ana2).
type fx struct {
	*Store
	t     *testing.T
	clock time.Time
	uid   map[string]int64
	pid   map[string]int64
}

func newFx(t *testing.T) *fx {
	t.Helper()
	oldW, oldR := writesPerMin, readsPerMin
	writesPerMin, readsPerMin = 1<<30, 1<<30
	t.Cleanup(func() { writesPerMin, readsPerMin = oldW, oldR })
	f := &fx{Store: openT(t, t.TempDir()), t: t, clock: time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC), uid: map[string]int64{}, pid: map[string]int64{}}
	t.Cleanup(func() { f.Close() })
	f.Store.now = func() time.Time { return f.clock }
	for _, n := range []string{"ana", "bor", "fani", "eva", "cene", "dana"} {
		f.user(n)
	}
	f.ok(f.SetBinding(ctx, "dev-ana2", f.uid["ana"]))
	for _, n := range []string{"fakture", "eureko", "import"} {
		f.project(n)
	}
	for u, m := range map[string]struct {
		p string
		r Role
	}{"ana": {"fakture", Developer}, "bor": {"fakture", Developer}, "fani": {"fakture", Admin}, "eva": {"fakture", Reader}, "cene": {"eureko", Developer}, "dana": {"import", Developer}} {
		f.ok(f.SetMember(ctx, f.pid[m.p], f.uid[u], m.r))
	}
	f.ok(f.AddGrant(ctx, f.pid["fakture"], f.pid["eureko"], Structure))
	f.ok(f.AddGrant(ctx, f.pid["eureko"], f.pid["import"], Structure))
	return f
}

func (f *fx) ok(err error) {
	f.t.Helper()
	if err != nil {
		f.t.Fatal(err)
	}
}

func (f *fx) user(name string) {
	u, err := f.CreateUser(ctx, name)
	f.ok(err)
	f.uid[name] = u.ID
	f.ok(f.SetBinding(ctx, "dev-"+name, u.ID))
}

func (f *fx) project(slug string) {
	p, err := f.CreateProject(ctx, slug, strings.ToUpper(slug))
	f.ok(err)
	f.pid[slug] = p.ID
}

func (f *fx) p(user string) Principal {
	f.t.Helper()
	p, err := f.Principal(ctx, "dev-"+user, true)
	f.ok(err)
	return p
}

var keySeq int

// A view whose body expression fails proves denied paths never evaluate bodies.
func TestNoteChecksDoNotLoadBodies(t *testing.T) {
	f := newFx(t)
	n := f.mk("ana", "fakture", Private)
	data := f.mk("ana", "fakture", Data)
	shared := f.mk("ana", "fakture", Structure)
	f.ok(f.write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`ALTER TABLE notes RENAME TO stored_notes`); err != nil {
			return err
		}
		_, err := tx.Exec(`CREATE VIEW notes AS SELECT id,project_id,class,title,json_extract('invalid', '$') AS body,bytes,author_id,created,updated,revision,imported FROM stored_notes`)
		return err
	}))
	p := f.p("bor")
	project := f.pid["fakture"]
	cap := RunCap{UserID: p.UserID, DeviceID: p.DeviceID, ProjectID: project, Epoch: f.epoch.Load()}
	for _, id := range []int64{n.ID, 99999} {
		_, err := f.GetNote(ctx, p, project, id)
		is(t, err, ErrNotFound)
		_, err = f.RunGetNote(ctx, cap, true, id)
		is(t, err, ErrNotFound)
		is(t, f.CanChangeNote(ctx, p, project, id), ErrNotFound)
		_, err = f.UpdateNote(ctx, p, project, id, 1, NoteInput{Class: Structure, Title: "x"})
		is(t, err, ErrNotFound)
		is(t, f.DeleteNote(ctx, p, project, id, 1), ErrNotFound)
	}
	// Even a successful write precheck needs no body; an authorized read does.
	f.ok(f.CanChangeNote(ctx, f.p("ana"), project, n.ID))
	is(t, f.CanChangeNote(ctx, f.p("bor"), project, shared.ID), ErrForbidden)
	is(t, f.CanChangeNote(ctx, f.p("eva"), project, shared.ID), ErrForbidden)
	_, err := f.GetNote(ctx, f.p("eva"), project, data.ID)
	is(t, err, ErrNotFound)
	_, err = f.GetNote(ctx, p, f.pid["eureko"], shared.ID)
	is(t, err, ErrNotFound)
	f.ok(f.SetMember(ctx, f.pid["eureko"], f.uid["eva"], Reader))
	f.ok(f.AddGrant(ctx, project, f.pid["eureko"], Data))
	// Data grants include receiving readers; an allowed body reaches the trap.
	if _, err := f.GetNote(ctx, f.p("eva"), project, data.ID); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("data grant: %v", err)
	}
	f.ok(f.RemoveGrant(ctx, project, f.pid["eureko"], Data))
	_, err = f.RunGetNote(ctx, cap, true, shared.ID)
	is(t, err, ErrNoProjectAccess)
	if _, err := f.GetNote(ctx, f.p("ana"), project, n.ID); err == nil {
		t.Fatal("body trap did not fire")
	}
}

func TestReplayRechecksCurrentNote(t *testing.T) {
	for _, change := range []string{"delete", "private", "data"} {
		t.Run(change, func(t *testing.T) {
			f := newFx(t)
			p, project := f.p("ana"), f.pid["fakture"]
			in := NoteInput{Class: Structure, Title: "original", Body: "cached body"}
			n, _, err := f.CreateNote(ctx, p, project, "replay", in)
			f.ok(err)
			if change == "delete" {
				f.ok(f.DeleteNote(ctx, p, project, n.ID, n.Revision))
			} else {
				_, err = f.UpdateNote(ctx, f.p("fani"), project, n.ID, n.Revision, NoteInput{Class: Class(change), Title: "changed", Body: "new body"})
				f.ok(err)
			}
			got, replay, err := f.CreateNote(ctx, p, project, "replay", in)
			is(t, err, ErrNotFound)
			if replay || got.Body != "" {
				t.Fatalf("stale replay: %+v", got)
			}
		})
	}
}

func TestReplayKeepsAuthorizedSameClassResult(t *testing.T) {
	f := newFx(t)
	p, project := f.p("ana"), f.pid["fakture"]
	in := NoteInput{Class: Data, Title: "first", Body: "original body"}
	n, _, err := f.CreateNote(ctx, p, project, "same-class", in)
	f.ok(err)
	_, err = f.UpdateNote(ctx, p, project, n.ID, n.Revision, NoteInput{Class: Data, Title: "edited", Body: "new body"})
	f.ok(err)
	got, replay, err := f.CreateNote(ctx, p, project, "same-class", in)
	f.ok(err)
	if !replay || got != n {
		t.Fatalf("changed authorized replay: %+v", got)
	}
	f.ok(f.RemoveMember(ctx, project, p.UserID))
	_, _, err = f.CreateNote(ctx, p, project, "same-class", in)
	is(t, err, ErrNotFound)
}

// mk creates a note as user and returns it (fails the test on error).
func (f *fx) mk(user, project string, c Class) Note {
	f.t.Helper()
	keySeq++
	f.clock = f.clock.Add(time.Second)
	n, _, err := f.CreateNote(ctx, f.p(user), f.pid[project], fmt.Sprint("k", keySeq), NoteInput{Class: c, Title: "t-" + string(c), Body: "body"})
	f.ok(err)
	return n
}

// seed inserts a note straight into the database (author may be a user who has no role in the project).
func (f *fx) seed(project int64, c Class, author int64) int64 {
	f.t.Helper()
	var id int64
	f.ok(f.write(ctx, func(tx *sql.Tx) error {
		r, err := tx.Exec(`INSERT INTO notes(project_id,class,title,body,bytes,author_id,created,updated) VALUES(?,?,?,?,?,?,?,?)`, project, c, "s", "b", 2, author, 1, 1)
		if err != nil {
			return err
		}
		id, _ = r.LastInsertId()
		return saveProvenance(tx, id, 1, Provenance{ActorKind: "member", User: author, Project: project, Device: "UNKNOWN", SourceKind: "UNKNOWN", Authored: fromMs(1), Received: fromMs(1), ContentHash: contentHash("s", "b")})
	}))
	return id
}

func is(t *testing.T, err, want error) {
	t.Helper()
	if want == nil && err != nil || want != nil && !errors.Is(err, want) {
		t.Fatalf("got %v, want %v", err, want)
	}
}

func (f *fx) ids(user, project string, class Class) (out map[int64]bool) {
	f.t.Helper()
	out = map[int64]bool{}
	pg, err := f.ListNotes(ctx, f.p(user), f.pid[project], class, "", 100)
	f.ok(err)
	for _, n := range pg.Notes {
		out[n.ID] = true
	}
	return
}

func TestCase1CreateAndRead(t *testing.T) {
	f := newFx(t)
	s, d := f.mk("ana", "fakture", Structure), f.mk("ana", "fakture", Data)
	for _, n := range []Note{s, d} {
		got, err := f.GetNote(ctx, f.p("bor"), f.pid["fakture"], n.ID)
		f.ok(err)
		if got.Body != "body" || got.Author != "ana" || got.Revision != 1 {
			t.Fatalf("%+v", got)
		}
	}
}

func TestCase2Matrix(t *testing.T) {
	// The tables of contracts section 2, one row per cell group, columns reader, developer, admin, grant structure,
	// grant data. "own" = only the caller's own notes, "all" = every note. Keep in step with the document.
	type row struct{ reader, dev, admin, gS, gD string }
	see := row{"yes", "yes", "yes", "yes", "yes"}
	read := map[Class]row{
		Structure: {"yes", "yes", "yes", "yes", "no"},
		Data:      {"no", "yes", "yes", "no", "yes"},
		Private:   {"own", "own", "all", "no", "no"},
	}
	create := map[Class]row{ // structure, data and private rows are equal in the document
		Structure: {"no", "yes", "yes", "no", "no"}, Data: {"no", "yes", "yes", "no", "no"}, Private: {"no", "yes", "yes", "no", "no"},
	}
	changeOwn := row{"no", "yes", "yes", "no", "no"}
	changeOther := row{"no", "no", "yes", "no", "no"}
	col := func(r row, subject string) string {
		return map[string]string{"reader": r.reader, "dev": r.dev, "admin": r.admin, "gS": r.gS, "gD": r.gD}[subject]
	}

	f := newFx(t)
	proj := func(slug string) int64 { f.project(slug); return f.pid[slug] }
	mx, rdS, rdD := proj("mx"), proj("rd-s"), proj("rd-d")
	f.ok(f.AddGrant(ctx, mx, rdS, Structure))
	f.ok(f.AddGrant(ctx, mx, rdD, Data))
	other := f.uid["bor"] // author of "another author's" notes
	subjects := map[string]struct {
		user string
		role Role
		in   int64
	}{"reader": {"eva", Reader, mx}, "dev": {"ana", Developer, mx}, "admin": {"fani", Admin, mx}, "gS": {"cene", Developer, rdS}, "gD": {"dana", Developer, rdD}}
	for _, s := range subjects {
		f.ok(f.SetMember(ctx, s.in, f.uid[s.user], s.role))
	}
	yes := func(v string, own bool) bool { return v == "yes" || v == "all" || v == "own" && own }

	for name, sj := range subjects {
		p := f.p(sj.user)
		me := f.uid[sj.user]
		t.Run(name+"/see", func(t *testing.T) {
			_, err := f.GetProject(ctx, p, "mx")
			is(t, err, map[bool]error{true: nil, false: ErrNotFound}[col(see, name) == "yes"])
		})
		for _, c := range allClasses {
			c := c
			for _, own := range []bool{true, false} {
				own := own
				author := other
				if own {
					author = me
				}
				who := map[bool]string{true: "own", false: "other"}[own]
				canRead := yes(col(read[c], name), own)
				t.Run(fmt.Sprintf("%s/%s/%s/read", name, c, who), func(t *testing.T) {
					id := f.seed(mx, c, author)
					_, err := f.GetNote(ctx, p, mx, id)
					is(t, err, map[bool]error{true: nil, false: ErrNotFound}[canRead])
					if f.ids(sj.user, "mx", "")[id] != canRead {
						t.Fatalf("list visibility of the note is not %v", canRead)
					}
				})
				// a denied change is 403 when the caller may read the note, else 404 (section 1, order)
				deny := map[bool]error{true: ErrForbidden, false: ErrNotFound}[canRead]
				chg := changeOther
				if own {
					chg = changeOwn
				}
				want := map[bool]error{true: nil, false: deny}[col(chg, name) == "yes"]
				t.Run(fmt.Sprintf("%s/%s/%s/update", name, c, who), func(t *testing.T) {
					id := f.seed(mx, c, author)
					_, err := f.UpdateNote(ctx, p, mx, id, 1, NoteInput{Class: c, Title: "x", Body: "y"})
					is(t, err, want)
				})
				t.Run(fmt.Sprintf("%s/%s/%s/delete", name, c, who), func(t *testing.T) {
					id := f.seed(mx, c, author)
					is(t, f.DeleteNote(ctx, p, mx, id, 1), want)
				})
			}
			t.Run(fmt.Sprintf("%s/%s/create", name, c), func(t *testing.T) {
				keySeq++
				_, _, err := f.CreateNote(ctx, p, mx, fmt.Sprint("m", keySeq), NoteInput{Class: c, Title: "x"})
				is(t, err, map[bool]error{true: nil, false: ErrForbidden}[col(create[c], name) == "yes"])
			})
		}
	}

	// Class changes (second table): developer and admin, own and another author's note.
	type tr struct {
		from, to   Class
		dev, admin bool // own note
	}
	moves := []tr{
		{Structure, Data, true, true}, {Data, Structure, true, true},
		{Private, Structure, true, true}, {Private, Data, true, true},
		{Structure, Private, false, true}, {Data, Private, false, true},
	}
	for _, m := range moves {
		for _, who := range []struct {
			name, user string
			own, ok    bool
		}{{"dev", "ana", true, m.dev}, {"admin", "fani", true, m.admin}, {"dev", "ana", false, false}, {"admin", "fani", false, true}} {
			author := other
			if who.own {
				author = f.uid[who.user]
			}
			t.Run(fmt.Sprintf("move/%s/%s->%s/own=%v", who.name, m.from, m.to, who.own), func(t *testing.T) {
				id := f.seed(mx, m.from, author)
				_, err := f.UpdateNote(ctx, f.p(who.user), mx, id, 1, NoteInput{Class: m.to, Title: "x"})
				want := error(nil)
				if !who.ok {
					want = ErrForbidden
					if m.from == Private && !who.own { // another author's private note is not visible to a developer
						want = ErrNotFound
					}
				}
				is(t, err, want)
			})
		}
	}
}

func TestCase3GrantedReaderSeesStructureOnly(t *testing.T) {
	f := newFx(t)
	s, d := f.mk("ana", "fakture", Structure), f.mk("ana", "fakture", Data)
	got := f.ids("cene", "fakture", "")
	if !got[s.ID] || got[d.ID] || len(got) != 1 {
		t.Fatalf("cene sees %v", got)
	}
	_, err := f.GetNote(ctx, f.p("cene"), f.pid["fakture"], d.ID)
	is(t, err, ErrNotFound)
	pr, err := f.GetProject(ctx, f.p("cene"), "fakture")
	f.ok(err)
	if pr.Role != "" || len(pr.Granted) != 1 || pr.Granted[0] != Structure {
		t.Fatalf("%+v", pr)
	}
	pg, _ := f.ListNotes(ctx, f.p("cene"), f.pid["fakture"], Data, "", 10) // a class filter cannot reveal more
	if len(pg.Notes) != 0 {
		t.Fatal("data listed")
	}
}

func TestCase4UnrelatedAndNoChaining(t *testing.T) {
	f := newFx(t)
	n := f.mk("ana", "fakture", Structure)
	d := f.p("dana")
	var bodies []string
	for _, call := range []func() error{
		func() error { _, err := f.ListNotes(ctx, d, f.pid["fakture"], "", "", 0); return err },
		func() error { _, err := f.GetProject(ctx, d, "fakture"); return err },
		func() error { _, err := f.GetProject(ctx, d, fmt.Sprint(f.pid["fakture"])); return err },
		func() error { _, err := f.GetNote(ctx, d, f.pid["fakture"], n.ID); return err },
		func() error {
			_, _, err := f.CreateNote(ctx, d, f.pid["fakture"], "k", NoteInput{Class: Data, Title: "x"})
			return err
		},
	} {
		err := call()
		is(t, err, ErrNotFound)
		bodies = append(bodies, err.Error())
	}
	for _, b := range bodies {
		if b != bodies[0] {
			t.Fatal("different bodies", bodies)
		}
	}
	// dana does reach eureko's structure (her own grant) but not fakture's through eureko's grant
	f.mk("cene", "eureko", Structure)
	if len(f.ids("dana", "eureko", "")) != 1 {
		t.Fatal("grant from eureko not working")
	}
	run, err := f.RunListNotes(ctx, RunCap{UserID: f.uid["dana"], DeviceID: "dev-dana", ProjectID: f.pid["import"], Epoch: f.epoch.Load()}, true, "", "", 0)
	f.ok(err)
	for _, x := range run.Notes {
		if x.ProjectID == f.pid["fakture"] {
			t.Fatal("grant chained")
		}
	}
	ps, _ := f.ListProjects(ctx, d, true)
	for _, p := range ps {
		if p.Slug == "fakture" {
			t.Fatal("fakture listed for dana")
		}
	}
}

func TestCase5Private(t *testing.T) {
	f := newFx(t)
	n := f.mk("ana", "fakture", Private)
	for _, u := range []string{"bor", "cene"} {
		_, err := f.GetNote(ctx, f.p(u), f.pid["fakture"], n.ID)
		is(t, err, ErrNotFound)
		if f.ids(u, "fakture", "")[n.ID] {
			t.Fatal(u, "lists it")
		}
	}
	_, err := f.GetNote(ctx, f.p("fani"), f.pid["fakture"], n.ID)
	f.ok(err)
	if !f.ids("fani", "fakture", "")[n.ID] || !f.ids("ana", "fakture", "")[n.ID] {
		t.Fatal("fani and ana must list it")
	}
}

func TestCase6Union(t *testing.T) {
	f := newFx(t)
	f.project("labs")
	f.ok(f.AddGrant(ctx, f.pid["fakture"], f.pid["labs"], Data))
	f.ok(f.SetMember(ctx, f.pid["labs"], f.uid["eva"], Developer))
	d := f.mk("ana", "fakture", Data)
	if _, err := f.GetNote(ctx, f.p("eva"), f.pid["fakture"], d.ID); err != nil {
		t.Fatal(err)
	}
	pr, err := f.GetProject(ctx, f.p("eva"), "fakture")
	f.ok(err)
	if pr.Role != Reader || len(pr.Granted) != 1 || pr.Granted[0] != Data {
		t.Fatalf("%+v", pr)
	}
	ps, _ := f.ListProjects(ctx, f.p("eva"), false)
	if len(ps) != 2 {
		t.Fatalf("%+v", ps)
	}
}

func TestCase7Archived(t *testing.T) {
	f := newFx(t)
	n := f.mk("ana", "fakture", Structure)
	f.ok(f.SetProjectArchived(ctx, f.pid["fakture"], true))
	_, _, err := f.CreateNote(ctx, f.p("fani"), f.pid["fakture"], "ka", NoteInput{Class: Data, Title: "x"})
	is(t, err, ErrArchived)
	_, err = f.UpdateNote(ctx, f.p("fani"), f.pid["fakture"], n.ID, 1, NoteInput{Class: Data, Title: "x"})
	is(t, err, ErrArchived)
	is(t, f.DeleteNote(ctx, f.p("fani"), f.pid["fakture"], n.ID, 1), ErrArchived)
	_, _, err = f.CreateNote(ctx, f.p("eva"), f.pid["fakture"], "kb", NoteInput{Class: Data, Title: "x"})
	is(t, err, ErrForbidden) // a reader learns nothing more than before
	if !f.ids("cene", "fakture", "")[n.ID] || !f.ids("ana", "fakture", "")[n.ID] {
		t.Fatal("reads must go on")
	}
	ps, _ := f.ListProjects(ctx, f.p("ana"), false)
	if len(ps) != 0 {
		t.Fatal("archived project listed")
	}
	if ps, _ = f.ListProjects(ctx, f.p("ana"), true); len(ps) != 1 {
		t.Fatal("archived=1 must list it")
	}
}

func TestCase8Revision(t *testing.T) {
	f := newFx(t)
	fk := f.pid["fakture"]
	mine := f.mk("bor", "fakture", Data)
	in := NoteInput{Class: Data, Title: "new"}
	n, err := f.UpdateNote(ctx, f.p("bor"), fk, mine.ID, 1, in)
	f.ok(err)
	if n.Revision != 2 {
		t.Fatalf("revision %d", n.Revision)
	}
	_, err = f.UpdateNote(ctx, f.p("bor"), fk, mine.ID, 1, in)
	var rev ErrRevision
	if !errors.As(err, &rev) || rev.Current != 2 {
		t.Fatalf("got %v", err)
	}
	is(t, f.DeleteNote(ctx, f.p("bor"), fk, mine.ID, 1), rev)
	theirs := f.mk("ana", "fakture", Data)
	for _, r := range []int64{1, 2, 99} { // never a revision to someone who may not change the note
		_, err = f.UpdateNote(ctx, f.p("bor"), fk, theirs.ID, r, in)
		is(t, err, ErrForbidden)
		if errors.As(err, &rev) {
			t.Fatal("revision leaked")
		}
		_, err = f.UpdateNote(ctx, f.p("dana"), fk, theirs.ID, r, in)
		is(t, err, ErrNotFound)
	}
	f.ok(f.DeleteNote(ctx, f.p("bor"), fk, mine.ID, 2))
	_, err = f.GetNote(ctx, f.p("ana"), fk, mine.ID)
	is(t, err, ErrNotFound)
	// the audit keeps id, class and author of the deleted note
	ev, _ := f.AuditEvents(ctx, 0, 500)
	found := false
	for _, e := range ev {
		if e.Action == "note.delete" && e.Subject == fmt.Sprint(mine.ID) && strings.Contains(string(e.Detail), `"class":"data"`) {
			found = true
		}
	}
	if !found {
		t.Fatal("no delete event")
	}
}

func TestCase9Identity(t *testing.T) {
	f := newFx(t)
	f.ok(f.SetUserActive(ctx, f.uid["eva"], false))
	for name, call := range map[string]func() error{
		"unbound":      func() error { _, err := f.Principal(ctx, "dev-nobody", true); return err },
		"deactivated":  func() error { _, err := f.Principal(ctx, "dev-eva", true); return err },
		"not approved": func() error { _, err := f.Principal(ctx, "dev-ana", false); return err },
		"empty device": func() error { _, err := f.Principal(ctx, "", true); return err },
		"stale (bound)": func() error {
			_, err := f.ListProjects(ctx, Principal{UserID: f.uid["eva"], DeviceID: "dev-eva"}, false)
			return err
		},
		"wrong user": func() error {
			_, err := f.ListProjects(ctx, Principal{UserID: f.uid["bor"], DeviceID: "dev-ana"}, false)
			return err
		},
	} {
		err := call()
		if !errors.Is(err, ErrNoProjectAccess) {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if _, err := f.Principal(ctx, "dev-ana2", true); err != nil { // second device of ana
		t.Fatal(err)
	}
}

// Case 10, the sequential core part: state changes between two calls. The barrier version (an operation paused
// before authorization while the revocation commits) comes with the gateway.
func TestCase10SequentialCorePart(t *testing.T) {
	changes := map[string]func(f *fx) (user string, check func() error){
		"device removed": func(f *fx) (string, func() error) {
			f.ok(f.DeviceRevoked(ctx, "dev-cene"))
			return "cene", func() error { _, err := f.Principal(ctx, "dev-cene", false); return err }
		},
		"binding changed": func(f *fx) (string, func() error) {
			f.ok(f.SetBinding(ctx, "dev-cene", f.uid["dana"]))
			return "cene", func() error {
				_, err := f.ListNotes(ctx, Principal{f.uid["cene"], "dev-cene"}, f.pid["eureko"], "", "", 0)
				return err
			}
		},
		"user deactivated": func(f *fx) (string, func() error) {
			f.ok(f.SetUserActive(ctx, f.uid["cene"], false))
			return "cene", func() error {
				_, err := f.ListNotes(ctx, Principal{f.uid["cene"], "dev-cene"}, f.pid["eureko"], "", "", 0)
				return err
			}
		},
		"membership removed": func(f *fx) (string, func() error) {
			f.ok(f.RemoveMember(ctx, f.pid["eureko"], f.uid["cene"]))
			return "cene", func() error {
				_, err := f.ListNotes(ctx, Principal{f.uid["cene"], "dev-cene"}, f.pid["eureko"], "", "", 0)
				if errors.Is(err, ErrNotFound) { // not visible any more
					return ErrNoProjectAccess
				}
				return err
			}
		},
		"role lowered": func(f *fx) (string, func() error) {
			f.ok(f.SetMember(ctx, f.pid["eureko"], f.uid["cene"], Reader))
			return "cene", func() error {
				_, _, err := f.CreateNote(ctx, Principal{f.uid["cene"], "dev-cene"}, f.pid["eureko"], "k1", NoteInput{Class: Data, Title: "x"})
				if errors.Is(err, ErrForbidden) {
					return ErrNoProjectAccess // stand-in: the write is refused now
				}
				return err
			}
		},
		"grant removed": func(f *fx) (string, func() error) {
			f.ok(f.RemoveGrant(ctx, f.pid["fakture"], f.pid["eureko"], Structure))
			return "cene", func() error {
				_, err := f.GetNote(ctx, Principal{f.uid["cene"], "dev-cene"}, f.pid["fakture"], f.mk("ana", "fakture", Structure).ID)
				if errors.Is(err, ErrNotFound) {
					return ErrNoProjectAccess
				}
				return err
			}
		},
		"note reclassified to data": func(f *fx) (string, func() error) {
			n := f.mk("ana", "fakture", Structure)
			if _, err := f.GetNote(ctx, f.p("cene"), f.pid["fakture"], n.ID); err != nil {
				t.Fatal(err)
			}
			_, err := f.UpdateNote(ctx, f.p("ana"), f.pid["fakture"], n.ID, 1, NoteInput{Class: Data, Title: "x"})
			f.ok(err)
			return "cene", func() error {
				_, err := f.GetNote(ctx, f.p("cene"), f.pid["fakture"], n.ID)
				if errors.Is(err, ErrNotFound) {
					return ErrNoProjectAccess
				}
				return err
			}
		},
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			f := newFx(t)
			cene := RunCap{UserID: f.uid["cene"], DeviceID: "dev-cene", ProjectID: f.pid["eureko"], Epoch: f.epoch.Load()}
			_, err := f.RunListNotes(ctx, cene, true, "", "", 0) // admitted before: works
			f.ok(err)
			before, _ := f.Epoch(ctx)
			_, check := change(f)
			if after, _ := f.Epoch(ctx); after <= before {
				t.Fatalf("epoch %d not above %d", after, before)
			}
			is(t, check(), ErrNoProjectAccess) // started after: fails
			_, err = f.RunListNotes(ctx, cene, true, "", "", 0)
			is(t, err, ErrNoProjectAccess) // the old capability is dead
		})
	}
}

func TestEpochRules(t *testing.T) {
	f := newFx(t)
	f.project("xx")
	f.pid["x"] = f.pid["xx"]
	f.ok(f.SetMember(ctx, f.pid["x"], f.uid["cene"], Developer))
	n := f.mk("fani", "fakture", Structure) // an admin may move own notes into private
	pn := f.mk("fani", "fakture", Private)
	up := func(id int64, rev int64, to Class) func() error {
		return func() error {
			_, err := f.UpdateNote(ctx, f.p("fani"), f.pid["fakture"], id, rev, NoteInput{Class: to, Title: "x"})
			return err
		}
	}
	steps := []struct {
		name string
		bump bool
		do   func() error
	}{
		{"new binding", false, func() error { return f.SetBinding(ctx, "dev-new", f.uid["eva"]) }},
		{"same binding", false, func() error { return f.SetBinding(ctx, "dev-new", f.uid["eva"]) }},
		{"binding changed", true, func() error { return f.SetBinding(ctx, "dev-new", f.uid["bor"]) }},
		{"binding removed", true, func() error { return f.RemoveBinding(ctx, "dev-new") }},
		{"rename", false, func() error { return f.RenameUser(ctx, f.uid["eva"], "eva2") }},
		{"owner mark", false, func() error { return f.SetUserOwner(ctx, f.uid["eva"], true) }},
		{"deactivate", true, func() error { return f.SetUserActive(ctx, f.uid["eva"], false) }},
		{"deactivate again", false, func() error { return f.SetUserActive(ctx, f.uid["eva"], false) }},
		{"activate", false, func() error { return f.SetUserActive(ctx, f.uid["eva"], true) }},
		{"member added", false, func() error { return f.SetMember(ctx, f.pid["x"], f.uid["bor"], Reader) }},
		{"role raised", false, func() error { return f.SetMember(ctx, f.pid["x"], f.uid["bor"], Admin) }},
		{"role same", false, func() error { return f.SetMember(ctx, f.pid["x"], f.uid["bor"], Admin) }},
		{"role lowered", true, func() error { return f.SetMember(ctx, f.pid["x"], f.uid["bor"], Developer) }},
		{"member removed", true, func() error { return f.RemoveMember(ctx, f.pid["x"], f.uid["bor"]) }},
		{"grant added", false, func() error { return f.AddGrant(ctx, f.pid["x"], f.pid["eureko"], Data) }},
		{"grant removed", true, func() error { return f.RemoveGrant(ctx, f.pid["x"], f.pid["eureko"], Data) }},
		{"archive", false, func() error { return f.SetProjectArchived(ctx, f.pid["x"], true) }},
		{"device revoked", true, func() error { return f.DeviceRevoked(ctx, "dev-cene") }},
		{"structure -> data", true, up(n.ID, 1, Data)},
		{"data -> structure", true, up(n.ID, 2, Structure)},
		{"structure -> private", true, up(n.ID, 3, Private)},
		{"private -> structure", true, up(n.ID, 4, Structure)},
		{"same class", false, up(pn.ID, 1, Private)},
	}
	for _, s := range steps {
		before, _ := f.Epoch(ctx)
		f.ok(s.do())
		after, _ := f.Epoch(ctx)
		if (after > before) != s.bump {
			t.Errorf("%s: epoch %d -> %d, bump wanted %v", s.name, before, after, s.bump)
		}
	}
}

func TestCase11NoResurrection(t *testing.T) {
	f := newFx(t)
	old := RunCap{UserID: f.uid["cene"], DeviceID: "dev-cene", ProjectID: f.pid["eureko"], Epoch: f.epoch.Load()}
	f.mk("cene", "eureko", Structure)
	f.ok(f.DeviceRevoked(ctx, "dev-cene")) // removed from the approved list; later approved again
	f.ok(f.RemoveBinding(ctx, "dev-cene"))
	f.ok(f.SetBinding(ctx, "dev-cene", f.uid["cene"])) // re-created
	f.ok(f.SetUserActive(ctx, f.uid["cene"], false))
	f.ok(f.SetUserActive(ctx, f.uid["cene"], true))
	_, err := f.RunListNotes(ctx, old, true, "", "", 0)
	is(t, err, ErrNoProjectAccess)
	fresh := old
	fresh.Epoch = f.epoch.Load()
	if pg, err := f.RunListNotes(ctx, fresh, true, "", "", 0); err != nil || len(pg.Notes) != 1 {
		t.Fatalf("%v %v", pg, err)
	}
	_, err = f.RunListNotes(ctx, fresh, false, "", "", 0) // not approved now
	is(t, err, ErrNoProjectAccess)
}

func TestCase12RunScope(t *testing.T) {
	f := newFx(t)
	f.project("side")
	f.ok(f.SetMember(ctx, f.pid["side"], f.uid["cene"], Developer))
	f.ok(f.AddGrant(ctx, f.pid["side"], f.pid["eureko"], Data)) // data of side flows to eureko's runs
	own := f.mk("cene", "eureko", Data)
	ownPriv := f.mk("cene", "eureko", Private)
	fs, fd := f.mk("ana", "fakture", Structure), f.mk("ana", "fakture", Data)
	sideData := f.mk("cene", "side", Data)
	sideStruct := f.mk("cene", "side", Structure)
	c := RunCap{UserID: f.uid["cene"], DeviceID: "dev-cene", ProjectID: f.pid["eureko"], Epoch: f.epoch.Load()}
	pg, err := f.RunListNotes(ctx, c, true, "", "", 100)
	f.ok(err)
	got := map[int64]bool{}
	for _, n := range pg.Notes {
		got[n.ID] = true
	}
	for id, want := range map[int64]bool{own.ID: true, fs.ID: true, sideData.ID: true, fd.ID: false, ownPriv.ID: false, sideStruct.ID: false} {
		if got[id] != want {
			t.Errorf("list: note %d present=%v want %v", id, got[id], want)
		}
		_, err := f.RunGetNote(ctx, c, true, id)
		if want {
			f.ok(err)
		} else {
			is(t, err, ErrNotFound)
		}
	}
	// a member of other projects only: no run scope at all
	c2 := RunCap{UserID: f.uid["bor"], DeviceID: "dev-bor", ProjectID: f.pid["eureko"], Epoch: f.epoch.Load()}
	_, err = f.RunListNotes(ctx, c2, true, "", "", 0)
	is(t, err, ErrNoProjectAccess)
}

func TestCase14EpochBetweenReads(t *testing.T) {
	f := newFx(t)
	c := RunCap{UserID: f.uid["cene"], DeviceID: "dev-cene", ProjectID: f.pid["eureko"], Epoch: f.epoch.Load()}
	_, err := f.RunListNotes(ctx, c, true, "", "", 0)
	f.ok(err)
	_, err = f.BumpEpoch(ctx, "test")
	f.ok(err)
	_, err = f.RunGetNote(ctx, c, true, 1)
	is(t, err, ErrNoProjectAccess)
}

func TestCase18API(t *testing.T) {
	f := newFx(t)
	fk := f.pid["fakture"]
	a := f.p("ana")
	_, _, err := f.CreateNote(ctx, a, fk, "k", NoteInput{Class: "bogus", Title: "x"})
	is(t, err, ErrInvalid{"class"})
	_, _, err = f.CreateNote(ctx, a, fk, "", NoteInput{Class: Data, Title: "x"})
	is(t, err, ErrInvalid{"idempotency key"})
	_, _, err = f.CreateNote(ctx, a, fk, strings.Repeat("k", 65), NoteInput{Class: Data, Title: "x"})
	is(t, err, ErrInvalid{"idempotency key"})
	_, _, err = f.CreateNote(ctx, a, fk, "k", NoteInput{Class: Data, Title: " "})
	is(t, err, ErrInvalid{"title"})
	for _, lim := range []int{-1, 101} {
		_, err = f.ListNotes(ctx, a, fk, "", "", lim)
		is(t, err, ErrInvalid{"limit"})
	}
	_, err = f.ListNotes(ctx, a, fk, "bogus", "", 0)
	is(t, err, ErrInvalid{"class"})
	_, err = f.ListNotes(ctx, a, fk, "", "not-a-cursor", 0)
	is(t, err, ErrInvalid{"cursor"})

	// idempotency
	in := NoteInput{Class: Data, Title: "once", Body: "b"}
	n1, rep, err := f.CreateNote(ctx, a, fk, "same", in)
	f.ok(err)
	n2, rep2, err := f.CreateNote(ctx, a, fk, "same", in)
	f.ok(err)
	if rep || !rep2 || n1.ID != n2.ID {
		t.Fatalf("replay: %v %v %v %v", rep, rep2, n1.ID, n2.ID)
	}
	_, _, err = f.CreateNote(ctx, a, fk, "same", NoteInput{Class: Data, Title: "other"})
	is(t, err, ErrIdempotency)
	if len(f.ids("ana", "fakture", "")) != 1 {
		t.Fatal("replay created a second note")
	}
	f.clock = f.clock.Add(25 * time.Hour) // the key is forgotten after 24 h
	n3, rep3, err := f.CreateNote(ctx, a, fk, "same", in)
	f.ok(err)
	if rep3 || n3.ID == n1.ID {
		t.Fatal("expired key replayed")
	}
	f.ok(f.RemoveMember(ctx, fk, f.uid["ana"])) // a replay still needs current access
	_, _, err = f.CreateNote(ctx, a, fk, "same", in)
	is(t, err, ErrNotFound)
	f.ok(f.SetMember(ctx, fk, f.uid["ana"], Developer))

	// sizes
	_, _, err = f.CreateNote(ctx, a, fk, "big", NoteInput{Class: Data, Title: "x", Body: strings.Repeat("x", 65<<10)})
	is(t, err, ErrTooLarge)
	_, _, err = f.CreateNote(ctx, a, fk, "big2", NoteInput{Class: Data, Title: strings.Repeat("é", 129)}) // 258 bytes
	is(t, err, ErrTooLarge)
	_, _, err = f.CreateNote(ctx, a, fk, "ok64", NoteInput{Class: Data, Title: "x", Body: strings.Repeat("x", 64<<10)})
	f.ok(err)
	_, _, err = f.CreateNote(ctx, f.p("dana"), fk, "big3", NoteInput{Class: Data, Title: "x", Body: strings.Repeat("x", 65<<10)})
	is(t, err, ErrNotFound) // a caller who may not see the project learns nothing about limits
}

func TestNoteCountLimit(t *testing.T) { // case 18: note 10 001, with the real limit
	f := newFx(t)
	fk := f.pid["fakture"]
	f.ok(f.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`WITH RECURSIVE c(i) AS (SELECT 1 UNION ALL SELECT i+1 FROM c WHERE i<?) INSERT INTO notes(project_id,class,title,body,bytes,author_id,created,updated) SELECT ?,'data','t','b',2,?,1,1 FROM c`, maxNotes, fk, f.uid["ana"])
		return err
	}))
	_, _, err := f.CreateNote(ctx, f.p("ana"), fk, "k", NoteInput{Class: Data, Title: "x"})
	is(t, err, ErrTooLarge)
	pg, err := f.ListNotes(ctx, f.p("ana"), fk, "", "", 100) // listing 10 000 notes pages fine
	f.ok(err)
	if len(pg.Notes) != 100 || pg.Next == "" {
		t.Fatal("paging")
	}
}

func TestBytesLimit(t *testing.T) {
	f := newFx(t)
	old := maxBytes
	maxBytes = 100
	defer func() { maxBytes = old }()
	n := f.mk("ana", "fakture", Data) // 6+4 bytes
	_, _, err := f.CreateNote(ctx, f.p("ana"), f.pid["fakture"], "k", NoteInput{Class: Data, Title: "x", Body: strings.Repeat("x", 100)})
	is(t, err, ErrTooLarge)
	_, err = f.UpdateNote(ctx, f.p("ana"), f.pid["fakture"], n.ID, 1, NoteInput{Class: Data, Title: "x", Body: strings.Repeat("x", 100)})
	is(t, err, ErrTooLarge)
	f.ok(f.DeleteNote(ctx, f.p("ana"), f.pid["fakture"], n.ID, 1)) // deleted notes free their space
	_, _, err = f.CreateNote(ctx, f.p("ana"), f.pid["fakture"], "k2", NoteInput{Class: Data, Title: "x", Body: strings.Repeat("x", 90)})
	f.ok(err)
}

func TestPagingWhileNotesAreAdded(t *testing.T) {
	f := newFx(t)
	fk := f.pid["fakture"]
	var want []int64
	for i := 0; i < 7; i++ {
		want = append([]int64{f.mk("ana", "fakture", Data).ID}, want...) // newest first
	}
	f.mk("ana", "fakture", Private) // hidden from bor: must not be counted into a page
	pg, err := f.ListNotes(ctx, f.p("bor"), fk, "", "", 3)
	f.ok(err)
	got := []int64{}
	for _, n := range pg.Notes {
		got = append(got, n.ID)
	}
	f.mk("ana", "fakture", Data) // arrives while paging: sorts before the cursor, not seen again, nothing skipped
	for pg.Next != "" {
		if pg, err = f.ListNotes(ctx, f.p("bor"), fk, "", pg.Next, 3); err != nil {
			t.Fatal(err)
		}
		for _, n := range pg.Notes {
			got = append(got, n.ID)
		}
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestCursorBoundToUserAndProject(t *testing.T) {
	f := newFx(t)
	for i := 0; i < 3; i++ {
		f.mk("ana", "fakture", Structure)
		f.mk("cene", "eureko", Structure)
	}
	pg, err := f.ListNotes(ctx, f.p("ana"), f.pid["fakture"], "", "", 1)
	f.ok(err)
	_, err = f.ListNotes(ctx, f.p("bor"), f.pid["fakture"], "", pg.Next, 1)
	is(t, err, ErrInvalid{"cursor"})
	_, err = f.ListNotes(ctx, f.p("cene"), f.pid["eureko"], "", pg.Next, 1)
	is(t, err, ErrInvalid{"cursor"})
	_, err = f.ListNotes(ctx, f.p("ana"), f.pid["fakture"], Data, pg.Next, 1) // other filter
	is(t, err, ErrInvalid{"cursor"})
	_, err = f.ListNotes(ctx, f.p("ana"), f.pid["fakture"], "", pg.Next, 1)
	f.ok(err)
}

func TestRateLimit(t *testing.T) {
	f := newFx(t)
	writesPerMin, readsPerMin = 3, 5
	a := f.p("ana")
	for i := 0; i < 3; i++ {
		_, _, err := f.CreateNote(ctx, a, f.pid["fakture"], fmt.Sprint("r", i), NoteInput{Class: Data, Title: "x"})
		f.ok(err)
	}
	f.clock = f.clock.Add(20 * time.Second)
	_, _, err := f.CreateNote(ctx, a, f.pid["fakture"], "r4", NoteInput{Class: Data, Title: "x"})
	var lim ErrLimit
	if !errors.As(err, &lim) || lim.RetryAfter != 40*time.Second {
		t.Fatalf("got %v", err)
	}
	for i := 0; i < 5; i++ { // reads are counted apart; the refused write above counted too
		_, err = f.ListNotes(ctx, a, f.pid["fakture"], "", "", 0)
		if i < 5 && err != nil {
			t.Fatal(i, err)
		}
	}
	_, err = f.ListNotes(ctx, a, f.pid["fakture"], "", "", 0)
	if !errors.As(err, &lim) {
		t.Fatalf("read limit: %v", err)
	}
	if _, err = f.ListNotes(ctx, f.p("bor"), f.pid["fakture"], "", "", 0); err != nil {
		t.Fatal("limit is per user", err)
	}
	f.clock = f.clock.Add(41 * time.Second) // new window
	if _, err = f.ListNotes(ctx, a, f.pid["fakture"], "", "", 0); err != nil {
		t.Fatal(err)
	}
}

func TestCase22Leave(t *testing.T) {
	f := newFx(t)
	fk := f.pid["fakture"]
	mine := f.mk("bor", "fakture", Private)
	pub := f.mk("ana", "fakture", Data)
	f.ok(f.RemoveMember(ctx, fk, f.uid["bor"]))
	b := f.p("bor")
	_, err := f.ListNotes(ctx, b, fk, "", "", 0)
	is(t, err, ErrNotFound)
	_, err = f.GetNote(ctx, b, fk, pub.ID)
	is(t, err, ErrNotFound)
	_, err = f.GetNote(ctx, b, fk, mine.ID)
	is(t, err, ErrNotFound)
	_, err = f.GetProject(ctx, b, "fakture")
	is(t, err, ErrNotFound)
	_, _, err = f.CreateNote(ctx, b, fk, "k", NoteInput{Class: Data, Title: "x"})
	is(t, err, ErrNotFound)
	if !f.ids("fani", "fakture", "")[mine.ID] {
		t.Fatal("admin must still see his private note")
	}
	f.ok(f.SetMember(ctx, fk, f.uid["bor"], Developer))
	if _, err = f.GetNote(ctx, b, fk, mine.ID); err != nil {
		t.Fatal("restored membership must restore access", err)
	}
	f.ok(f.SetUserActive(ctx, f.uid["bor"], false)) // the author's name stays readable after deactivation
	n, err := f.GetNote(ctx, f.p("ana"), fk, mine.ID)
	is(t, err, ErrNotFound)
	n, err = f.GetNote(ctx, f.p("fani"), fk, mine.ID)
	f.ok(err)
	if n.Author != "bor" {
		t.Fatal(n.Author)
	}
}

func TestSlugGrammar(t *testing.T) {
	f := newFx(t)
	for slug, ok := range map[string]bool{
		"ab": true, "a-b": true, "0a": true, strings.Repeat("a", 63): true, strings.Repeat("a", 64): false,
		"a": false, "-ab": false, "Ab": false, "a_b": false, "a b": false, "": false, "12345": false, "č": false,
	} {
		_, err := f.CreateProject(ctx, slug, "n")
		if (err == nil) != ok {
			t.Errorf("slug %q: err %v, valid %v", slug, err, ok)
		}
	}
	_, err := f.CreateProject(ctx, "fakture", "dup")
	is(t, err, ErrInvalid{"slug already used"})
	// lookup by id and by slug give the same project
	a, _ := f.GetProject(ctx, f.p("ana"), "fakture")
	b, err := f.GetProject(ctx, f.p("ana"), fmt.Sprint(a.ID))
	f.ok(err)
	if a.ID != b.ID {
		t.Fatal("lookup")
	}
}

func TestConcurrentCreatesRespectNoteLimit(t *testing.T) {
	f := newFx(t)
	old := maxNotes
	maxNotes = 20
	defer func() { maxNotes = old }()
	var wg sync.WaitGroup
	var mu sync.Mutex
	okN, tooLarge := 0, 0
	for g := 0; g < 8; g++ {
		user := []string{"ana", "bor", "fani"}[g%3]
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				_, _, err := f.CreateNote(ctx, f.p(user), f.pid["fakture"], fmt.Sprint("c", g, "-", i), NoteInput{Class: Data, Title: "x"})
				mu.Lock()
				switch {
				case err == nil:
					okN++
				case errors.Is(err, ErrTooLarge):
					tooLarge++
				default:
					t.Error(err)
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	var n int
	f.DB().QueryRow(`SELECT count(*) FROM notes`).Scan(&n)
	if okN != 20 || n != 20 || tooLarge != 60 {
		t.Fatalf("created %d, rows %d, refused %d", okN, n, tooLarge)
	}
}

func TestAdminValidationAndAudit(t *testing.T) {
	f := newFx(t)
	_, err := f.CreateUser(ctx, "ANA") // names are unique, case-insensitive
	is(t, err, ErrInvalid{"name already used"})
	_, err = f.CreateUser(ctx, " ")
	is(t, err, ErrInvalid{"name"})
	is(t, f.SetBinding(ctx, "d", 999), ErrNotFound)
	is(t, f.RemoveBinding(ctx, "nope"), ErrNotFound)
	is(t, f.SetMember(ctx, f.pid["fakture"], f.uid["ana"], "boss"), ErrInvalid{"role"})
	is(t, f.AddGrant(ctx, f.pid["fakture"], f.pid["eureko"], Private), ErrInvalid{"class"})
	is(t, f.AddGrant(ctx, f.pid["fakture"], f.pid["fakture"], Data), ErrInvalid{"grant to the same project"})
	is(t, f.SetProjectArchived(ctx, 9999, true), ErrNotFound)
	ev, _ := f.AuditEvents(ctx, 0, 500)
	seen := map[string]bool{}
	for _, e := range ev {
		seen[e.Action] = true
	}
	for _, a := range []string{"user.create", "binding.set", "project.create", "member.set", "grant.add"} {
		if !seen[a] {
			t.Errorf("no %s audit event", a)
		}
	}
	us, _ := f.ListUsers(ctx)
	bs, _ := f.ListBindings(ctx)
	ms, _ := f.ListMembers(ctx, f.pid["fakture"])
	gs, _ := f.ListGrants(ctx)
	ps, _ := f.AdminListProjects(ctx)
	if len(us) != 6 || len(bs) != 7 || len(ms) != 4 || len(gs) != 2 || len(ps) != 3 {
		t.Fatalf("%d %d %d %d %d", len(us), len(bs), len(ms), len(gs), len(ps))
	}
}

func TestPolicySurvivesRestore(t *testing.T) { // case 21, policy part: revoked member comes back, epoch above
	f := newFx(t)
	dir := f.dir
	bak := t.TempDir() + "/b.db"
	f.ok(f.Backup(ctx, bak))
	f.ok(f.RemoveMember(ctx, f.pid["fakture"], f.uid["bor"]))
	f.project("later")
	last := f.pid["later"]
	before, _ := f.Epoch(ctx)
	f.Close()
	f.ok(Restore(dir, bak))
	s, err := Open(dir, Options{})
	f.ok(err)
	defer s.Close()
	p, err := s.Principal(ctx, "dev-bor", true)
	f.ok(err)
	if _, err := s.ListNotes(ctx, p, f.pid["fakture"], "", "", 0); err != nil {
		t.Fatal("backup state not back:", err)
	}
	if e, _ := s.Epoch(ctx); e <= before {
		t.Fatal("epoch")
	}
	np, err := s.CreateProject(ctx, "after", "After")
	f.ok(err)
	if np.ID <= last {
		t.Fatalf("project id %d not above %d", np.ID, last)
	}
}

func TestEpochBumpsWhenAuthorLosesPrivateNote(t *testing.T) {
	f := newFx(t)
	fk := f.pid["fakture"]
	n := f.mk("ana", "fakture", Private)
	f.ok(f.SetMember(ctx, fk, f.uid["ana"], Reader))
	if _, err := f.GetNote(ctx, f.p("ana"), fk, n.ID); err != nil { // a reader still reads her own private note
		t.Fatal(err)
	}
	before, _ := f.Epoch(ctx)
	_, err := f.UpdateNote(ctx, f.p("fani"), fk, n.ID, 1, NoteInput{Class: Data, Title: "x"})
	f.ok(err)
	if after, _ := f.Epoch(ctx); after <= before {
		t.Fatal("epoch did not grow")
	}
	_, err = f.GetNote(ctx, f.p("ana"), fk, n.ID)
	is(t, err, ErrNotFound)
}

func TestResolveForLinkNeedsDirectMembership(t *testing.T) { // core-fix 5
	f := newFx(t)
	_, err := f.ResolveForLink(ctx, f.p("cene"), "fakture") // grant only: may read, may not link
	is(t, err, ErrForbidden)
	if pr, err := f.ResolveForLink(ctx, f.p("cene"), "eureko"); err != nil || pr.Role != Developer {
		t.Fatalf("%+v %v", pr, err)
	}
	if _, err := f.ResolveForLink(ctx, f.p("eva"), fmt.Sprint(f.pid["fakture"])); err != nil { // by id, reader is a member
		t.Fatal(err)
	}
	_, err = f.ResolveForLink(ctx, f.p("dana"), "fakture")
	is(t, err, ErrNotFound)
	// a run for a project the user only reaches through a grant is still refused
	c := RunCap{UserID: f.uid["cene"], DeviceID: "dev-cene", ProjectID: f.pid["fakture"], Epoch: f.epoch.Load()}
	_, err = f.RunListNotes(ctx, c, true, "", "", 0)
	is(t, err, ErrNoProjectAccess)
}

func TestIdempotencyHashIsUnambiguous(t *testing.T) { // core-fix 6: the collision of the review
	f := newFx(t)
	a, fk := f.p("ana"), f.pid["fakture"]
	_, _, err := f.CreateNote(ctx, a, fk, "k", NoteInput{Class: Data, Title: "a", Body: "b\x00c"})
	f.ok(err)
	_, rep, err := f.CreateNote(ctx, a, fk, "k", NoteInput{Class: Data, Title: "a\x00b", Body: "c"})
	is(t, err, ErrIdempotency)
	if rep {
		t.Fatal("replayed")
	}
}

func TestArchivedBeforeReplay(t *testing.T) { // core-fix 9
	f := newFx(t)
	a, fk := f.p("ana"), f.pid["fakture"]
	in := NoteInput{Class: Data, Title: "x"}
	_, _, err := f.CreateNote(ctx, a, fk, "k", in)
	f.ok(err)
	f.ok(f.SetProjectArchived(ctx, fk, true))
	_, _, err = f.CreateNote(ctx, a, fk, "k", in)
	is(t, err, ErrArchived)
}

func TestDBHandleIsReadOnly(t *testing.T) { // core-fix 10
	f := newFx(t)
	if _, err := f.DB().Exec(`INSERT INTO meta VALUES('x','y')`); err == nil {
		t.Fatal("DB() accepted a write")
	}
}

func TestListsCarryNoBody(t *testing.T) { // core-fix 11
	f := newFx(t)
	n := f.mk("ana", "fakture", Data)
	pg, err := f.ListNotes(ctx, f.p("bor"), f.pid["fakture"], "", "", 0)
	f.ok(err)
	if len(pg.Notes) != 1 || pg.Notes[0].Body != "" || pg.Notes[0].Title == "" {
		t.Fatalf("%+v", pg.Notes)
	}
	run, err := f.RunListNotes(ctx, RunCap{UserID: f.uid["ana"], DeviceID: "dev-ana", ProjectID: f.pid["fakture"], Epoch: f.epoch.Load()}, true, "", "", 0)
	f.ok(err)
	if len(run.Notes) != 1 || run.Notes[0].Body != "" {
		t.Fatalf("%+v", run.Notes)
	}
	if got, _ := f.GetNote(ctx, f.p("bor"), f.pid["fakture"], n.ID); got.Body != "body" {
		t.Fatal("GetNote lost the body")
	}
}

// core-fix 7: an exhausted user still gets the answer of the check that comes first.
func TestRateLimitComesAfterTheChecks(t *testing.T) {
	f := newFx(t)
	fk := f.pid["fakture"]
	theirs := f.mk("ana", "fakture", Data)
	mine := f.mk("bor", "fakture", Data)
	writesPerMin, readsPerMin = 1, 1
	run := RunCap{UserID: f.uid["cene"], DeviceID: "dev-cene", ProjectID: f.pid["eureko"], Epoch: f.epoch.Load()}
	in := NoteInput{Class: Data, Title: "x"}
	bor, dana, eva, fani := f.p("bor"), f.p("dana"), f.p("eva"), f.p("fani")
	f.ok(f.SetProjectArchived(ctx, f.pid["import"], true))
	f.ok(f.SetMember(ctx, f.pid["import"], f.uid["eva"], Developer))
	f.ok(f.SetMember(ctx, f.pid["import"], f.uid["bor"], Developer))
	f.ok(f.SetMember(ctx, f.pid["import"], f.uid["fani"], Developer))
	for _, u := range []Principal{bor, dana, eva, fani} { // use up the one write of each
		f.rate(u.UserID, true)
		f.rate(u.UserID, true)
	}
	f.rate(run.UserID, false)
	f.rate(run.UserID, false)
	f.rate(bor.UserID, false)
	f.rate(bor.UserID, false)
	_, err := f.UpdateNote(ctx, dana, fk, theirs.ID, 1, in)
	is(t, err, ErrNotFound)
	_, err = f.UpdateNote(ctx, bor, fk, theirs.ID, 1, in)
	is(t, err, ErrForbidden)
	_, err = f.UpdateNote(ctx, bor, fk, mine.ID, 99, in)
	is(t, err, ErrRevision{Current: 1})
	is(t, f.DeleteNote(ctx, dana, fk, theirs.ID, 1), ErrNotFound)
	is(t, f.DeleteNote(ctx, bor, fk, mine.ID, 99), ErrRevision{Current: 1})
	_, _, err = f.CreateNote(ctx, eva, fk, "k", in)
	is(t, err, ErrForbidden)
	_, _, err = f.CreateNote(ctx, dana, fk, "k", in)
	is(t, err, ErrNotFound)
	_, _, err = f.CreateNote(ctx, f.p("fani"), f.pid["import"], "k", in)
	is(t, err, ErrArchived)
	_, err = f.GetNote(ctx, dana, fk, theirs.ID)
	is(t, err, ErrNotFound)
	_, err = f.ListNotes(ctx, dana, fk, "", "", 0)
	is(t, err, ErrNotFound)
	_, err = f.GetProject(ctx, dana, "fakture")
	is(t, err, ErrNotFound)
	_, err = f.ListProjects(ctx, Principal{UserID: f.uid["bor"], DeviceID: "dev-eva"}, false)
	is(t, err, ErrNoProjectAccess)
	f.ok(f.DeviceRevoked(ctx, "dev-cene"))
	_, err = f.RunListNotes(ctx, run, true, "", "", 0)
	is(t, err, ErrNoProjectAccess) // a revoked capability is not a 429
	_, err = f.RunGetNote(ctx, run, true, theirs.ID)
	is(t, err, ErrNoProjectAccess)
	// and where every check passes, the limit is returned
	var lim ErrLimit
	_, err = f.UpdateNote(ctx, bor, fk, mine.ID, 1, in)
	if !errors.As(err, &lim) {
		t.Fatalf("update: %v", err)
	}
	_, _, err = f.CreateNote(ctx, bor, fk, "k9", in)
	if !errors.As(err, &lim) {
		t.Fatalf("create: %v", err)
	}
	_, err = f.GetNote(ctx, bor, fk, theirs.ID)
	if !errors.As(err, &lim) {
		t.Fatalf("get: %v", err)
	}
	_, err = f.ListNotes(ctx, bor, fk, "", "", 0)
	if !errors.As(err, &lim) {
		t.Fatalf("list: %v", err)
	}
	_, _, err = f.CreateNote(ctx, bor, fk, "k9", in) // a replay of a first create is limited as well
	if !errors.As(err, &lim) {
		t.Fatalf("replay: %v", err)
	}
}
