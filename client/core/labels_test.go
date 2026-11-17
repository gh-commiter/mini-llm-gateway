package core

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func (f *fx) labelled(id string, user, project int64, model, phase, activity, task, taskSource string, at time.Time) {
	f.t.Helper()
	b := UsageEvent{ID: id, Start: at, UserID: user, ProjectID: project, DeviceID: "dev", Provider: "codex", Model: model, Phase: phase, Activity: activity, Task: task, TaskSource: taskSource}
	f.ok(f.BeginEvent(ctx, b))
	b.End, b.Outcome, b.TokensIn, b.TokensOut, b.TokenSource = at.Add(time.Second), OutcomeOK, 10, 5, "reported"
	f.ok(f.FinishEvent(ctx, b))
}

func TestLabelsNormalizedAtTheGateway(t *testing.T) {
	f := newFx(t)
	now := f.now()
	long := make([]byte, 65)
	for i := range long {
		long[i] = 'x'
	}
	f.labelled("l1", f.uid["ana"], f.pid["fakture"], "m", "Plan", "BugFix", "#149", "branch", now)
	f.labelled("l2", f.uid["ana"], f.pid["fakture"], "m", "not safe!", "a b", string(long), "given", now)
	f.labelled("l3", f.uid["ana"], f.pid["fakture"], "m", "", "", "", "branch", now)
	get := func(id string) (a [6]string) {
		f.ok(f.DB().QueryRow(`SELECT phase,activity,task,phase_source,activity_source,task_source FROM usage_events WHERE id=?`, id).Scan(&a[0], &a[1], &a[2], &a[3], &a[4], &a[5]))
		return
	}
	if g := get("l1"); g != [6]string{"plan", "bugfix", "#149", "given", "given", "branch"} {
		t.Fatal(g)
	}
	if g := get("l2"); g != [6]string{} { // dropped, the event is stored all the same
		t.Fatal(g)
	}
	if g := get("l3"); g != [6]string{} { // a source without a label is nothing
		t.Fatal(g)
	}
}

func TestLabelsMigration17OldRowsEmpty(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, dbFile))
	if err != nil {
		t.Fatal(err)
	}
	tx, _ := db.Begin()
	for _, m := range migrations[:16] {
		if err = m(tx); err != nil {
			t.Fatal(err)
		}
	}
	setMeta(tx, "schema", "16")
	if _, err = tx.Exec(`INSERT INTO usage_events(id,start,end,device_id,provider,model,outcome,token_source) VALUES('old',?,?,'d','p','m','ok','unknown')`, ms(time.Now()), ms(time.Now())); err != nil {
		t.Fatal(err)
	}
	tx.Commit()
	db.Close()
	s := openT(t, dir)
	defer s.Close()
	var a, b string
	if err = s.DB().QueryRow(`SELECT phase,task_source FROM usage_events WHERE id='old'`).Scan(&a, &b); err != nil || a != "" || b != "" {
		t.Fatal(a, b, err)
	}
	if snaps, _ := filepath.Glob(filepath.Join(dir, "core-16-*.db")); len(snaps) != 1 {
		t.Fatal("no snapshot", snaps)
	}
	if err = checkSchemaObjects(ctx, s.DB(), len(migrations)); err != nil {
		t.Fatal(err)
	}
}

// Labels select and group events; they never widen whose events a caller reads.
func TestLabelsUsageAndSessions(t *testing.T) {
	f := newFx(t)
	now := f.now()
	fk := f.pid["fakture"]
	f.labelled("a1", f.uid["ana"], fk, "m", "plan", "feature", "#1", "given", now)
	f.labelled("a2", f.uid["ana"], fk, "m", "do", "bugfix", "feature/x", "branch", now)
	f.labelled("a3", f.uid["ana"], fk, "m", "", "", "", "", now)
	f.labelled("b1", f.uid["bor"], fk, "m", "plan", "feature", "#1", "given", now) // another developer
	rep, e := f.AdminUsage(ctx, 7, UsageFilter{Labels: true})
	f.ok(e)
	got := map[[3]string]int64{}
	for _, r := range rep.Rows {
		got[[3]string{r.Phase, r.Activity, r.Task}] += r.Events
	}
	if got[[3]string{"plan", "feature", "#1"}] != 2 || got[[3]string{"do", "bugfix", "feature/x"}] != 1 || got[[3]string{}] != 1 {
		t.Fatal(got)
	}
	// Without Labels the report is as before: one row per day, model, user and project.
	rep, e = f.AdminUsage(ctx, 7, UsageFilter{})
	f.ok(e)
	if len(rep.Rows) != 2 || rep.Rows[0].Phase != "" {
		t.Fatal(rep.Rows)
	}
	rep, e = f.AdminUsage(ctx, 7, UsageFilter{Phase: "plan"})
	f.ok(e)
	if ev, _, _, _ := rowsOf(rep); ev != 2 {
		t.Fatal(ev)
	}
	// A developer filtering by a label still sees only the own events.
	rep, e = f.Usage(ctx, f.p("ana"), fk, 7, LabelQuery{Phase: "plan", Labels: true})
	f.ok(e)
	if ev, _, _, _ := rowsOf(rep); ev != 1 {
		t.Fatal("a label filter reached another user's events", ev)
	}
	// Sessions: labels of the first request, filter by label; a developer's own only.
	pg, e := f.Sessions(ctx, f.p("ana"), fk, SessionFilter{Days: 7, Phase: "plan"})
	f.ok(e)
	if len(pg.Sessions) != 1 || pg.Sessions[0].Task != "#1" || pg.Sessions[0].TaskSource != "given" || pg.Sessions[0].UserID != f.uid["ana"] {
		t.Fatal(pg.Sessions)
	}
	pg, e = f.AdminSessions(ctx, fk, SessionFilter{Days: 7, Task: "feature/x"})
	f.ok(e)
	if len(pg.Sessions) != 1 || pg.Sessions[0].TaskSource != "branch" {
		t.Fatal(pg.Sessions)
	}
	pg, e = f.Sessions(ctx, f.p("eva"), fk, SessionFilter{Days: 7, Phase: "plan"}) // a reader sees only the own: none
	f.ok(e)
	if len(pg.Sessions) != 0 {
		t.Fatal(pg.Sessions)
	}
}

func TestLabelFiltersAreNormalizedAndNeverAnError(t *testing.T) {
	f := newFx(t)
	now := f.now()
	fk := f.pid["fakture"]
	f.labelled("n1", f.uid["ana"], fk, "m", "do", "bugfix", "Feature/X", "given", now)
	count := func(r UsageReport, e error) int64 {
		f.ok(e)
		ev, _, _, _ := rowsOf(r)
		return ev
	}
	for _, q := range []UsageFilter{{Phase: "Do"}, {Activity: "BugFix"}, {Phase: " DO "}, {Task: "Feature/X"}} {
		if n := count(f.AdminUsage(ctx, 7, q)); n != 1 {
			t.Errorf("%+v: %d events", q, n)
		}
	}
	for _, q := range []UsageFilter{{Phase: "not a token!"}, {Task: "feature/x"}, {Activity: "x;y"}} { // matches nothing, no error
		if n := count(f.AdminUsage(ctx, 7, q)); n != 0 {
			t.Errorf("%+v: %d events", q, n)
		}
	}
	pg, e := f.Sessions(ctx, f.p("ana"), fk, SessionFilter{Days: 7, Phase: "DO"})
	f.ok(e)
	if len(pg.Sessions) != 1 {
		t.Fatal(pg.Sessions)
	}
	pg, e = f.Sessions(ctx, f.p("ana"), fk, SessionFilter{Days: 7, Phase: "bad phase"})
	f.ok(e)
	if len(pg.Sessions) != 0 {
		t.Fatal(pg.Sessions)
	}
}

func TestLabelReportRowCap(t *testing.T) {
	f := newFx(t)
	now := f.now()
	fk := f.pid["fakture"]
	f.ok(f.write(ctx, func(tx *sql.Tx) error {
		_, e := tx.Exec(`WITH RECURSIVE seq(x) AS (VALUES(1) UNION ALL SELECT x+1 FROM seq WHERE x<?)
 INSERT INTO usage_events(id,start,end,user_id,device_id,project_id,provider,model,outcome,token_source,task,task_source)
 SELECT 'cap'||x, ?, ?, ?, 'd', ?, 'p', 'm', 'ok', 'unknown', 'task-'||x, 'given' FROM seq`, maxLabelRows+10, ms(now), ms(now), f.uid["ana"], fk)
		return e
	}))
	rep, e := f.AdminUsage(ctx, 7, UsageFilter{Labels: true})
	f.ok(e)
	if len(rep.Rows) != maxLabelRows || !rep.Truncated {
		t.Fatal(len(rep.Rows), rep.Truncated)
	}
	rep, e = f.AdminUsage(ctx, 7, UsageFilter{}) // an unlabelled report is as before: one row
	f.ok(e)
	if len(rep.Rows) != 1 || rep.Truncated {
		t.Fatal(len(rep.Rows), rep.Truncated)
	}
}
