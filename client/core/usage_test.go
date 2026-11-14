package core

import (
	"testing"
	"time"
)

func (f *fx) event(id, parent string, user, project int64, model, outcome string, in, out int64, at time.Time) UsageEvent {
	e := UsageEvent{ID: id, ParentID: parent, Start: at, End: at.Add(time.Second), UserID: user, ProjectID: project, DeviceID: "dev", Provider: "codex", Model: model,
		Outcome: outcome, TokensIn: in, TokensOut: out, TokenSource: "reported"}
	f.ok(f.BeginEvent(ctx, UsageEvent{ID: id, ParentID: parent, Start: at, UserID: user, DeviceID: "dev", Provider: "codex", Model: model}))
	f.ok(f.FinishEvent(ctx, e))
	return e
}

func rowsOf(rep UsageReport) (events, in, out, nested int64) {
	for _, r := range rep.Rows {
		events, in, out, nested = events+r.Events, in+r.TokensIn, out+r.TokensOut, nested+r.Nested
	}
	return
}

func TestUsageMatrixAndTotals(t *testing.T) { // contracts section 2 row "Usage of the project", case 19
	f := newFx(t)
	now := f.now()
	fk, eu := f.pid["fakture"], f.pid["eureko"]
	f.event("a1", "", f.uid["ana"], fk, "claude-cli", "ok", 100, 10, now)
	f.event("a2", "a1", f.uid["ana"], fk, "claude-codex", "ok", 50, 5, now) // nested under a1: counted once, in the totals
	f.event("b1", "", f.uid["bor"], fk, "claude-cli", "ok", 7, 3, now)
	f.event("c1", "", f.uid["cene"], eu, "claude-cli", "ok", 1, 1, now)
	f.event("n1", "", 0, 0, "claude-cli", "ok", 1, 1, now) // no user, no project

	got := func(user string, project int64) (int64, int64, int64, int64) {
		t.Helper()
		rep, err := f.Usage(ctx, f.p(user), project, 7)
		f.ok(err)
		return rowsOf(rep)
	}
	if e, in, out, nested := got("ana", fk); e != 2 || in != 150 || out != 15 || nested != 1 { // developer: own events only
		t.Fatalf("ana: %d %d %d %d", e, in, out, nested)
	}
	if e, in, _, _ := got("bor", fk); e != 1 || in != 7 {
		t.Fatalf("bor sees %d events, %d in", e, in)
	}
	if e, in, out, nested := got("fani", fk); e != 3 || in != 157 || out != 18 || nested != 1 { // admin: all of the project, nested once
		t.Fatalf("fani: %d %d %d %d", e, in, out, nested)
	}
	for _, u := range []string{"eva", "cene", "dana"} { // reader, grant only, stranger: the usage resource is not visible
		_, err := f.Usage(ctx, f.p(u), fk, 7)
		is(t, err, ErrNotFound)
	}
	if e, _, _, _ := got("ana", 0); e != 2 { // no project: own events over all projects
		t.Fatalf("ana without a project: %d", e)
	}
	if e, _, _, _ := got("cene", 0); e != 1 {
		t.Fatalf("cene without a project: %d", e)
	}
	_, err := f.Usage(ctx, f.p("ana"), fk, 0)
	is(t, err, ErrInvalid{"days"})
	// the owner sees everything, also the event with no user and no project
	rep, err := f.AdminUsage(ctx, 7, UsageFilter{})
	f.ok(err)
	if e, _, _, _ := rowsOf(rep); e != 5 {
		t.Fatalf("admin: %d events", e)
	}
	rep, _ = f.AdminUsage(ctx, 7, UsageFilter{Project: fk, User: f.uid["bor"]})
	if e, _, _, _ := rowsOf(rep); e != 1 {
		t.Fatalf("admin filter: %d", e)
	}
	// a revoked user gets no usage
	fp := f.p("fani")
	f.ok(f.SetUserActive(ctx, f.uid["fani"], false))
	_, err = f.Usage(ctx, fp, fk, 7)
	is(t, err, ErrNoProjectAccess)
}

func (f *fx) now() time.Time { return f.Store.now() }

func TestUsageEventLifeCycleIsIdempotentAndFrozen(t *testing.T) {
	f := newFx(t)
	at := f.now()
	f.ok(f.BeginEvent(ctx, UsageEvent{ID: "e1", Start: at, UserID: f.uid["ana"], DeviceID: "d1", Provider: "p", Model: "m"}))
	f.ok(f.BeginEvent(ctx, UsageEvent{ID: "e1", Start: at, UserID: f.uid["bor"], DeviceID: "other", Provider: "p", Model: "m"})) // a repeat changes nothing
	fin := UsageEvent{ID: "e1", End: at.Add(time.Second), UserID: f.uid["bor"], ProjectID: f.pid["fakture"], DeviceID: "x", Provider: "p", Model: "m", Outcome: OutcomeOK, TokensIn: 5, TokensOut: 6, TokenSource: "reported"}
	f.ok(f.FinishEvent(ctx, fin))
	again := fin
	again.Outcome, again.TokensIn = OutcomeError, 99
	f.ok(f.FinishEvent(ctx, again)) // the first finish wins: a replay counts once
	var user, in int64
	var outcome, dev string
	f.DB().QueryRow(`SELECT user_id, tokens_in, outcome, device_id FROM usage_events WHERE id='e1'`).Scan(&user, &in, &outcome, &dev)
	if user != f.uid["ana"] || in != 5 || outcome != OutcomeOK || dev != "d1" { // attribution as admitted, never remapped
		t.Fatalf("user %d in %d outcome %s device %s", user, in, outcome, dev)
	}
	// a finish for an event the database never saw inserts the whole row (spool replay)
	f.ok(f.FinishEvent(ctx, UsageEvent{ID: "e2", Start: at, End: at, UserID: f.uid["ana"], DeviceID: "d", Provider: "p", Model: "m", Outcome: OutcomeCancelled, TokensIn: -1, TokensOut: -1, TokenSource: "unknown"}))
	var n int
	f.DB().QueryRow(`SELECT count(*) FROM usage_events WHERE tokens_in IS NULL AND outcome='cancelled'`).Scan(&n)
	if n != 1 {
		t.Fatal("unknown tokens must be NULL")
	}
	f.ok(f.BeginEvent(ctx, UsageEvent{ID: "e3", Start: at, DeviceID: "d", Provider: "p", Model: "m"}))
	f.ok(f.DiscardEvent(ctx, "e3")) // a refusal is not an invocation
	f.ok(f.DiscardEvent(ctx, "e1")) // a finished one stays
	f.DB().QueryRow(`SELECT count(*) FROM usage_events`).Scan(&n)
	if n != 2 {
		t.Fatalf("%d rows", n)
	}
}

func TestOpenEventsBecomeInterrupted(t *testing.T) {
	dir := t.TempDir()
	s := openT(t, dir)
	at := time.Now()
	s.BeginEvent(ctx, UsageEvent{ID: "cut", Start: at, DeviceID: "d", Provider: "p", Model: "m"})
	s.FinishEvent(ctx, UsageEvent{ID: "done", Start: at, End: at, DeviceID: "d", Provider: "p", Model: "m", Outcome: OutcomeOK, TokensIn: -1, TokensOut: -1, TokenSource: "unknown"})
	s.Close() // a crash: the open row stays
	s = openT(t, dir)
	defer s.Close()
	var cut, done string
	s.DB().QueryRow(`SELECT outcome FROM usage_events WHERE id='cut'`).Scan(&cut)
	s.DB().QueryRow(`SELECT outcome FROM usage_events WHERE id='done'`).Scan(&done)
	if cut != OutcomeInterrupted || done != OutcomeOK {
		t.Fatalf("cut %s, done %s", cut, done)
	}
	// a spooled finish that arrives after the reopen still completes the row
	s.FinishEvent(ctx, UsageEvent{ID: "cut", End: at, Outcome: OutcomeOK, TokensIn: 1, TokensOut: 2, TokenSource: "reported"})
	s.DB().QueryRow(`SELECT outcome FROM usage_events WHERE id='cut'`).Scan(&cut)
	if cut != OutcomeOK {
		t.Fatal("late finish ignored:", cut)
	}
}

func TestUsageRetentionFoldsIntoDailyTotals(t *testing.T) {
	f := newFx(t)
	clock := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	f.Store.now = func() time.Time { return clock }
	old := clock.AddDate(0, 0, -(usageRetentionDays + 5))
	f.event("o1", "", f.uid["ana"], f.pid["fakture"], "m", "ok", 10, 1, old)
	f.event("o2", "", f.uid["ana"], f.pid["fakture"], "m", "ok", 20, 2, old.Add(time.Hour))
	f.event("r1", "", f.uid["ana"], f.pid["fakture"], "m", "ok", 5, 5, clock.AddDate(0, 0, -3))
	n, err := f.FoldOldUsage(ctx)
	f.ok(err)
	if n != 2 {
		t.Fatalf("folded %d", n)
	}
	if n, _ = f.FoldOldUsage(ctx); n != 0 { // a second run folds nothing (and adds nothing twice)
		t.Fatalf("second fold: %d", n)
	}
	var left int
	f.DB().QueryRow(`SELECT count(*) FROM usage_events`).Scan(&left)
	if left != 1 {
		t.Fatalf("detail rows left: %d", left)
	}
	rep, err := f.AdminUsage(ctx, 3650, UsageFilter{})
	f.ok(err)
	if e, in, out, _ := rowsOf(rep); e != 3 || in != 35 || out != 8 { // totals survive, once
		t.Fatalf("%d %d %d", e, in, out)
	}
	if rep, _ := f.AdminUsage(ctx, 30, UsageFilter{}); len(rep.Rows) != 1 {
		t.Fatalf("recent window: %+v", rep.Rows)
	}
	if _, err := f.Usage(ctx, f.p("ana"), 0, usageRetentionDays+1); err == nil {
		t.Fatal("members cannot ask beyond the retention")
	}
}

func TestUsageGapsAreListedAndIdempotent(t *testing.T) {
	f := newFx(t)
	a, b := f.now().Add(-2*time.Hour), f.now().Add(-time.Hour)
	f.ok(f.AddGap(ctx, "g1", 7, a, b))
	f.ok(f.AddGap(ctx, "g1", 7, a, b)) // a replay of the same gap adds nothing
	f.ok(f.AddGap(ctx, "g0", 1, a.AddDate(-1, 0, 0), b.AddDate(-1, 0, 0)))
	rep, err := f.Usage(ctx, f.p("ana"), 0, 7)
	f.ok(err)
	if len(rep.Gaps) != 1 || rep.Gaps[0].Count != 7 || !rep.Gaps[0].From.Equal(a.Truncate(time.Millisecond)) {
		t.Fatalf("%+v", rep.Gaps)
	}
}
