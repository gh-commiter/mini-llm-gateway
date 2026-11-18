package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestSearchFTS5AndAtomicWrite(t *testing.T) {
	f := newFx(t)
	var enabled int
	f.ok(f.DB().QueryRow(`SELECT sqlite_compileoption_used('ENABLE_FTS5')`).Scan(&enabled))
	if enabled != 1 {
		t.Fatal("SQLite must include FTS5")
	}
	// An unavailable index must roll back the note, not publish unsearchable content.
	f.ok(f.write(ctx, func(tx *sql.Tx) error {
		_, e := tx.Exec(`DROP TABLE note_search`)
		return e
	}))
	_, _, e := f.CreateNote(ctx, f.p("ana"), f.pid["fakture"], "index-failure", NoteInput{Class: Structure, Title: "comet", Body: "reference"})
	if e == nil {
		t.Fatal("write succeeded without index")
	}
	var count int
	f.ok(f.DB().QueryRow(`SELECT count(*) FROM notes WHERE title='comet'`).Scan(&count))
	if count != 0 {
		t.Fatal("failed index write published note")
	}
}

func TestSearchPermissionsAndMutations(t *testing.T) {
	f := newFx(t)
	project := f.pid["fakture"]
	p := f.p("ana")
	create := func(cl Class, title, body string) Note {
		keySeq++
		n, _, e := f.CreateNote(ctx, p, project, i64(int64(keySeq)), NoteInput{Class: cl, Title: title, Body: body})
		f.ok(e)
		return n
	}
	shared := create(Structure, "visible comet", "shared body")
	data := create(Data, "hidden nebula", "hidden body")
	private := create(Private, "private pulsar", "author body")
	search := func(user, q string, want ...int64) {
		t.Helper()
		pg, e := f.SearchNotes(ctx, f.p(user), project, q)
		f.ok(e)
		if len(pg.Hits) != len(want) {
			t.Fatalf("%s %s: %+v want %v", user, q, pg, want)
		}
		for i, id := range want {
			if pg.Hits[i].ID != id {
				t.Fatal(pg, want)
			}
		}
	}
	search("eva", "comet", shared.ID)
	search("eva", "nebula")
	search("bor", "pulsar")
	search("ana", "pulsar", private.ID)
	search("fani", "pulsar", private.ID)
	search("cene", "comet", shared.ID)
	search("cene", "nebula")
	_, denied := f.SearchNotes(ctx, f.p("dana"), project, "comet")
	is(t, denied, ErrNotFound)
	f.ok(f.SetMember(ctx, f.pid["eureko"], f.uid["eva"], Reader))
	f.ok(f.AddGrant(ctx, project, f.pid["eureko"], Data))
	search("eva", "nebula", data.ID)
	cap := RunCap{UserID: f.uid["cene"], DeviceID: "dev-cene", ProjectID: f.pid["eureko"], Epoch: f.epoch.Load()}
	pg, e := f.RunSearchNotes(ctx, cap, true, "nebula")
	f.ok(e)
	if len(pg.Hits) != 1 {
		t.Fatal(pg)
	}
	pg, e = f.RunSearchNotes(ctx, cap, true, "pulsar")
	f.ok(e)
	if len(pg.Hits) != 0 {
		t.Fatal(pg)
	}
	// Other memberships do not enlarge a run's capability.
	f.ok(f.SetMember(ctx, f.pid["import"], f.uid["ana"], Developer))
	cap = RunCap{UserID: p.UserID, DeviceID: p.DeviceID, ProjectID: f.pid["import"], Epoch: f.epoch.Load()}
	pg, e = f.RunSearchNotes(ctx, cap, true, "comet")
	f.ok(e)
	if len(pg.Hits) != 0 {
		t.Fatal(pg)
	}
	f.ok(f.RemoveGrant(ctx, project, f.pid["eureko"], Data))
	_, e = f.RunSearchNotes(ctx, cap, true, "comet")
	is(t, e, ErrNoProjectAccess)
	updated, e := f.UpdateNote(ctx, p, project, shared.ID, shared.Revision, NoteInput{Class: Structure, Title: "new meteor", Body: "replacement"})
	f.ok(e)
	search("eva", "comet")
	search("eva", "meteor", shared.ID)
	updated, e = f.UpdateNote(ctx, f.p("fani"), project, shared.ID, updated.Revision, NoteInput{Class: Private, Title: "new meteor", Body: "replacement"})
	f.ok(e)
	search("eva", "meteor")
	search("ana", "meteor", shared.ID)
	f.ok(f.DeleteNote(ctx, p, project, shared.ID, updated.Revision))
	search("ana", "meteor")
	var indexed int
	f.ok(f.DB().QueryRow(`SELECT count(*) FROM note_search WHERE note_search MATCH '"meteor"'`).Scan(&indexed))
	if indexed != 0 {
		t.Fatal("deleted postings retained", indexed)
	}
	v, _, e := f.Submit(ctx, p, project, "search-submission", SubmissionInput{NoteInput: NoteInput{Class: Structure, Title: "review quasar", Body: "pending"}})
	f.ok(e)
	search("ana", "quasar")
	accepted, e := f.ReviewSubmission(ctx, v.ID, true, "", f.seen(v.ID))
	f.ok(e)
	search("eva", "quasar", accepted.NoteID)
	v, _, e = f.Submit(ctx, p, project, "search-replacement", SubmissionInput{NoteInput: NoteInput{Class: Data, Title: "approved galaxy", Body: "reviewed"}, Replaces: accepted.NoteID, Revision: 1})
	f.ok(e)
	_, e = f.ReviewSubmission(ctx, v.ID, true, "", f.seen(v.ID))
	f.ok(e)
	search("ana", "quasar")
	search("ana", "galaxy", accepted.NoteID)
	search("eva", "galaxy")
	before, e := f.SearchNotes(ctx, p, project, "galaxy")
	f.ok(e)
	f.ok(f.RebuildSearch(ctx))
	after, e := f.SearchNotes(ctx, p, project, "galaxy")
	f.ok(e)
	a, _ := json.Marshal(before)
	b, _ := json.Marshal(after)
	if string(a) != string(b) {
		t.Fatal(string(a), string(b))
	}
	var trusted int
	f.ok(f.DB().QueryRow("PRAGMA trusted_schema").Scan(&trusted))
	if trusted != 0 {
		t.Fatal(trusted)
	}
}

func TestSearchBoundsAndBodyBoundary(t *testing.T) {
	f := newFx(t)
	p := f.p("ana")
	project := f.pid["fakture"]
	for _, q := range []string{"", "***", strings.Repeat("a", 65), strings.Repeat("a ", 17), strings.Repeat("x", 513), string([]byte{255})} {
		_, e := f.SearchNotes(ctx, p, project, q)
		var invalid ErrInvalid
		if !errors.As(e, &invalid) {
			t.Fatalf("%q: %v", q, e)
		}
	}
	for _, q := range []string{`" OR *`, `title:NEAR(foo bar)`, `a-b`, `"unterminated`} {
		if _, e := f.SearchNotes(ctx, p, project, q); e != nil {
			t.Fatalf("literal %q: %v", q, e)
		}
	}
	for i := 0; i < 30; i++ {
		_, _, e := f.CreateNote(ctx, p, project, i64(int64(i+1)), NoteInput{Class: Data, Title: "comet " + strings.Repeat("<", 240), Body: strings.Repeat("<", 1000)})
		f.ok(e)
	}
	pg, e := f.SearchNotes(ctx, p, project, "comet")
	f.ok(e)
	b, _ := json.Marshal(pg)
	if len(pg.Hits) == 0 || len(pg.Hits) > 20 || len(b)+1 > 8192 {
		t.Fatal(len(pg.Hits), len(b))
	}
	for _, h := range pg.Hits {
		if len([]rune(h.Snippet)) > 160 {
			t.Fatal(h)
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, e = f.SearchNotes(cancelled, p, project, "comet")
	if e == nil {
		t.Fatal("cancel ignored")
	}
	// Denied candidates must never evaluate a body, even for an exact title hit.
	f.ok(f.write(ctx, func(tx *sql.Tx) error {
		_, e := tx.Exec(`ALTER TABLE notes RENAME TO stored_notes; CREATE VIEW notes AS SELECT id,project_id,class,title,json_extract('invalid','$') AS body,bytes,author_id,created,updated,revision,imported,entry_kind FROM stored_notes`)
		return e
	}))
	pg, e = f.SearchNotes(ctx, f.p("eva"), project, "comet")
	f.ok(e)
	if len(pg.Hits) != 0 {
		t.Fatal(pg)
	}
	if _, e = f.SearchNotes(ctx, p, project, "comet"); e == nil {
		t.Fatal("body trap not reached")
	}
}

func TestSearchMigration11(t *testing.T) {
	base := migrations
	defer func() { migrations = base }()
	migrations = base[:11]
	dir := t.TempDir()
	s := openT(t, dir)
	u, e := s.CreateUser(ctx, "Legacy")
	is(t, e, nil)
	p, e := s.CreateProject(ctx, "legacy", "Legacy")
	is(t, e, nil)
	is(t, s.SetBinding(ctx, "device", u.ID), nil)
	is(t, s.SetMember(ctx, p.ID, u.ID, Developer), nil)
	is(t, s.write(ctx, func(tx *sql.Tx) error {
		_, e := tx.Exec(`INSERT INTO notes(project_id,class,title,body,bytes,author_id,created,updated) VALUES(?,?,?,?,?,?,?,?)`, p.ID, Structure, "legacy comet", "body", 16, u.ID, time.Now().UnixMilli(), time.Now().UnixMilli())
		return e
	}), nil)
	s.Close()
	migrations = base
	s = openT(t, dir)
	pg, e := s.SearchNotes(ctx, Principal{u.ID, "device"}, p.ID, "comet")
	is(t, e, nil)
	if len(pg.Hits) != 1 {
		t.Fatal(pg)
	}
	if snaps := snapshots(dir); len(snaps) != 1 || !strings.HasPrefix(snaps[0], "core-11-") {
		t.Fatal(snaps)
	}
	s.Close()
	migrations = base[:11]
	old, e := Open(dir, Options{})
	if old != nil {
		old.Close()
	}
	is(t, e, ErrNewerSchema)
}

// searchPlan is the EXPLAIN QUERY PLAN of a statement, one line per step.
func searchPlan(t *testing.T, f *fx, q string, args []any) (plan []string) {
	t.Helper()
	rows, e := f.DB().Query("EXPLAIN QUERY PLAN "+q, args...)
	f.ok(e)
	defer rows.Close()
	for rows.Next() {
		var id, parent, unused int
		var detail string
		f.ok(rows.Scan(&id, &parent, &unused, &detail))
		plan = append(plan, detail)
	}
	f.ok(rows.Err())
	return
}

// checkSearchPlan is the deterministic guard of the search statement: the MATCH index is used, and the hits come from two
// bounded selections (the asked project's own, then the rest), each its own co-routine with its own top-N. A single ORDER BY
// on an expression over all matches (kb8a's regression: it sorted every match and missed the deadline on a large corpus)
// has neither co-routine.
func checkSearchPlan(plan []string) error {
	joined := strings.Join(plan, "\n")
	for _, want := range []string{"CO-ROUTINE own", "CO-ROUTINE other", "note_search VIRTUAL TABLE INDEX"} {
		if !strings.Contains(joined, want) {
			return fmt.Errorf("the search plan lacks %q", want)
		}
	}
	return nil
}

func TestSearchPlanGuardCatchesASingleOrderByOverAllMatches(t *testing.T) {
	f := newFx(t)
	project := f.pid["fakture"]
	filter, args := where(f.uid["ana"], []readable{{project: project, all: []Class{Structure}}})
	match := searchScope(f.uid["ana"], []readable{{project: project, all: []Class{Structure}}}) + ` AND {title body} : ("common")`
	old := `WITH visible AS MATERIALIZED (
 SELECT n.id,n.project_id,n.class,n.title,n.updated FROM notes n WHERE ` + filter + `),
 hits AS MATERIALIZED (SELECT v.* FROM visible v JOIN note_search ON note_search.rowid=v.id
 WHERE note_search MATCH ? ORDER BY v.project_id<>?,v.updated DESC,v.id LIMIT 20)
 SELECT h.id,h.project_id,h.class,h.title,substr(n.body,1,160),h.updated,n.entry_kind
 FROM hits h JOIN notes n ON n.id=h.id ORDER BY h.project_id<>?,h.updated DESC,h.id`
	if checkSearchPlan(searchPlan(t, f, old, append(append([]any{}, args...), match, project, project))) == nil {
		t.Fatal("the plan guard accepts the old single-ORDER-BY statement")
	}
	if err := checkSearchPlan(searchPlan(t, f, searchSQL(filter), append(append([]any{}, args...), match, project, match, project))); err != nil {
		t.Fatal(err)
	}
}

func TestSearchIndexedScale(t *testing.T) {
	f := newFx(t)
	project := f.pid["fakture"]
	f.ok(f.write(ctx, func(tx *sql.Tx) error {
		_, e := tx.Exec(`WITH RECURSIVE seq(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM seq WHERE x<30000)
 INSERT INTO notes(project_id,class,title,body,bytes,author_id,created,updated)
 SELECT ?, 'structure', CASE WHEN x=15000 THEN 'rare comet' ELSE 'common text' END, 'reference body', 30, ?, 1, x FROM seq`, project, f.uid["ana"])
		return e
	}))
	f.ok(f.RebuildSearch(ctx))
	filter, args := where(f.uid["ana"], []readable{{project: project, all: []Class{Structure}}})
	match := searchScope(f.uid["ana"], []readable{{project: project, all: []Class{Structure}}}) + ` AND {title body} : ("comet")`
	args = append(args, match, project, match, project)
	plan := searchPlan(t, f, searchSQL(filter), args)
	if err := checkSearchPlan(plan); err != nil {
		t.Fatal(err, plan)
	}
	// The time: a single run measures the machine's momentary load as much as the query. A real regression (a sort or scan of
	// all matches) raises every run, so the fastest of several must meet the deadline; each run also has the product's own 250 ms.
	best := time.Hour
	var common SearchPage
	for i := 0; i < 7; i++ {
		start := time.Now()
		pg, e := f.SearchNotes(ctx, f.p("ana"), project, "common")
		if d := time.Since(start); e == nil && d < best {
			best, common = d, pg
		}
	}
	if best > 250*time.Millisecond {
		t.Fatal("even the fastest of 7 runs missed the deadline:", best)
	}
	if len(common.Hits) != 20 {
		t.Fatal("hit cap", len(common.Hits))
	}
	pg, e := f.SearchNotes(ctx, f.p("ana"), project, "comet")
	f.ok(e)
	if len(pg.Hits) != 1 {
		t.Fatal(pg)
	}
	t.Log("30,000-note query plan:", plan)
}

func TestSearchUnicodeAndHiddenCorpus(t *testing.T) {
	f := newFx(t)
	p := f.p("ana")
	project := f.pid["fakture"]
	n, _, e := f.CreateNote(ctx, p, project, "unicode-search", NoteInput{Class: Structure, Title: "cafe\u0301 निर्णय comet", Body: "reference"})
	f.ok(e)
	for _, q := range []string{"cafe\u0301", "निर्णय"} {
		pg, e := f.SearchNotes(ctx, f.p("eva"), project, q)
		f.ok(e)
		if len(pg.Hits) != 1 || pg.Hits[0].ID != n.ID {
			t.Fatalf("%q: %+v", q, pg)
		}
	}
	before, e := f.SearchNotes(ctx, f.p("eva"), project, "comet")
	f.ok(e)
	for _, cl := range []Class{Private, Data} {
		_, _, e = f.CreateNote(ctx, p, project, string(cl)+"-hidden-search", NoteInput{Class: cl, Title: "comet hidden title", Body: "comet hidden body"})
		f.ok(e)
	}
	after, e := f.SearchNotes(ctx, f.p("eva"), project, "comet")
	f.ok(e)
	a, _ := json.Marshal(before)
	b, _ := json.Marshal(after)
	if string(a) != string(b) {
		t.Fatal("hidden corpus changed results", string(a), string(b))
	}
	// Permission tokens cannot be searched as title/body content.
	pg, e := f.SearchNotes(ctx, p, project, "p"+i64(project)+"cstructure")
	f.ok(e)
	if len(pg.Hits) != 0 {
		t.Fatal(pg)
	}
}

func TestSearchDeadline(t *testing.T) {
	f := newFx(t)
	p := f.p("ana")
	project := f.pid["fakture"]
	_, _, e := f.CreateNote(ctx, p, project, "deadline-search", NoteInput{Class: Structure, Title: "comet", Body: "reference"})
	f.ok(e)
	// Make authorized metadata deliberately expensive without relying on hardware speed.
	f.ok(f.write(ctx, func(tx *sql.Tx) error {
		_, e := tx.Exec(`ALTER TABLE notes RENAME TO stored_notes;
 CREATE VIEW notes AS WITH RECURSIVE seq(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM seq WHERE x<100000000)
 SELECT n.id+x*1000 AS id,n.project_id,n.class,n.title,n.body,n.updated,n.author_id,n.imported,n.entry_kind FROM stored_notes n CROSS JOIN seq`)
		return e
	}))
	start := time.Now()
	_, e = f.SearchNotes(ctx, p, project, "comet")
	if !errors.Is(e, context.DeadlineExceeded) || time.Since(start) > 3*time.Second {
		t.Fatalf("query deadline: %v after %v", e, time.Since(start))
	}
}
