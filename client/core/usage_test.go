package core

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
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
	rep, err := f.AdminUsage(ctx, 7, UsageFilter{}) // the owner
	f.ok(err)
	if len(rep.Gaps) != 1 || rep.Gaps[0].Count != 7 || !rep.Gaps[0].From.Equal(a.Truncate(time.Millisecond)) {
		t.Fatalf("%+v", rep.Gaps)
	}
	// review L5: members see gaps only as admin of the project they asked about
	for _, c := range []struct {
		user    string
		project int64
		want    int
	}{{"ana", 0, 0}, {"ana", f.pid["fakture"], 0}, {"bor", f.pid["fakture"], 0}, {"fani", f.pid["fakture"], 1}} {
		r, err := f.Usage(ctx, f.p(c.user), c.project, 7)
		f.ok(err)
		if len(r.Gaps) != c.want {
			t.Errorf("%s project %d: %d gaps, want %d", c.user, c.project, len(r.Gaps), c.want)
		}
	}
}

func TestUsageCountsAgainstTheReadLimit(t *testing.T) { // review L5
	f := newFx(t)
	readsPerMin = 3
	defer func() { readsPerMin = 600 }()
	p := f.p("ana")
	for i := 0; i < 3; i++ {
		_, err := f.Usage(ctx, p, 0, 7)
		f.ok(err)
	}
	_, err := f.Usage(ctx, p, 0, 7)
	var lim ErrLimit
	if !errors.As(err, &lim) {
		t.Fatalf("got %v", err)
	}
	_, err = f.Usage(ctx, f.p("dana"), f.pid["fakture"], 7) // the checks still come first: a stranger gets 404, not 429
	is(t, err, ErrNotFound)
}

func TestUsageStringsAreCapped(t *testing.T) { // review L6
	f := newFx(t)
	long := strings.Repeat("é", 400) // 800 bytes
	f.ok(f.BeginEvent(ctx, UsageEvent{ID: "c1", Start: f.now(), DeviceID: strings.Repeat("d", 500), Provider: strings.Repeat("p", 100), Model: long}))
	f.ok(f.FinishEvent(ctx, UsageEvent{ID: "c1", End: f.now(), Outcome: OutcomeOK, TokensIn: -1, TokensOut: -1, TokenSource: "unknown", Provider: "x"}))
	var model, dev, prov string
	f.DB().QueryRow(`SELECT model, device_id, provider FROM usage_events WHERE id='c1'`).Scan(&model, &dev, &prov)
	if len(model) > 256 || len(model) < 250 || !utf8.ValidString(model) || len(dev) != 256 || len(prov) != 64 {
		t.Fatalf("%d %d %d", len(model), len(dev), len(prov))
	}
}

func TestFoldedTotalsKeepFailedNestedAndUnknownCounts(t *testing.T) { // review (should-fix)
	f := newFx(t)
	clock := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	f.Store.now = func() time.Time { return clock }
	old := clock.AddDate(0, 0, -(usageRetentionDays + 5))
	f.event("p", "", f.uid["ana"], f.pid["fakture"], "m", "ok", 10, 1, old)
	f.event("n", "p", f.uid["ana"], f.pid["fakture"], "m", "error", -1, -1, old)
	f.event("c", "", f.uid["ana"], f.pid["fakture"], "m", "cancelled", 5, 5, old)
	_, err := f.FoldOldUsage(ctx)
	f.ok(err)
	rep, err := f.AdminUsage(ctx, 3650, UsageFilter{})
	f.ok(err)
	if len(rep.Rows) != 1 {
		t.Fatalf("%+v", rep.Rows)
	}
	r := rep.Rows[0]
	if r.Events != 3 || r.Failed != 2 || r.Nested != 1 || r.UnknownTokens != 1 {
		t.Fatalf("%+v", r)
	}
}

func TestBoundUserIsAMemorySnapshot(t *testing.T) { // review A3
	f := newFx(t)
	f.ok(f.SetBinding(ctx, "dev-new", f.uid["eva"]))
	if id, _ := f.BoundUser(ctx, "dev-new"); id != f.uid["eva"] {
		t.Fatalf("%d", id)
	}
	f.rdb.Close() // no database I/O is needed to attribute a request
	if id, err := f.BoundUser(ctx, "dev-new"); id != f.uid["eva"] || err != nil {
		t.Fatalf("%d %v", id, err)
	}
	if id, _ := f.BoundUser(ctx, "unknown"); id != 0 {
		t.Fatal("unbound device attributed")
	}
}

func TestUsageCostsProviderAndRetentionBoundary(t *testing.T) {
	f := newFx(t)
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	f.Store.now = func() time.Time { return now }
	cut := now.AddDate(0, 0, -usageRetentionDays)
	zero, paid := int64(0), int64(6000000)
	event := func(id, provider string, at time.Time, cost *int64) UsageEvent {
		return UsageEvent{ID: id, Start: at, End: at, Provider: provider, Model: "alias", UpstreamModel: "routed", Outcome: OutcomeOK, TokensIn: 0, TokensOut: 0, TokenSource: "reported", CostUsdNanos: cost, CostSource: "reported"}
	}
	old := event("old", "openrouter", cut.Add(-time.Millisecond), &paid)
	live := event("live", "openrouter", cut, &zero)
	unknown := event("unknown", "openrouter", cut.Add(time.Second), nil)
	other := event("other", "litellm", cut, &paid)
	f.ok(f.BeginEvent(ctx, old))
	changed := old
	changed.UpstreamModel = "changed"
	f.ok(f.FinishEvent(ctx, changed))
	// Neither first-finish replay nor begin replay can replace cost/model attribution.
	changed.CostUsdNanos = &zero
	f.ok(f.ReplayUsage(ctx, []UsageEvent{changed, live, unknown, other}, nil))
	f.ok(f.ReplayUsage(ctx, []UsageEvent{changed, live, unknown, other}, nil))
	var upstream string
	f.ok(f.DB().QueryRow(`SELECT upstream_model FROM usage_events WHERE id='old'`).Scan(&upstream))
	if upstream != "routed" {
		t.Fatal(upstream)
	}
	n, err := f.FoldOldUsage(ctx)
	f.ok(err)
	if n != 1 {
		t.Fatalf("folded %d", n)
	}
	rep, err := f.AdminUsage(ctx, 401, UsageFilter{Provider: "openrouter", Model: "alias"})
	f.ok(err)
	if len(rep.Rows) != 1 {
		t.Fatalf("%+v", rep)
	}
	r := rep.Rows[0]
	if r.Provider != "openrouter" || r.Events != 3 || r.UnknownTokens != 0 || r.UnknownCost != 1 || r.ReportedCost == nil || *r.ReportedCost != paid || r.EstimatedCost != nil {
		t.Fatalf("%+v", r)
	}
	rep, err = f.AdminUsage(ctx, 401, UsageFilter{})
	f.ok(err)
	if len(rep.Rows) != 2 {
		t.Fatalf("providers merged: %+v", rep)
	}
	n, err = f.FoldOldUsage(ctx)
	f.ok(err)
	if n != 0 {
		t.Fatal("fold replay", n)
	}
}

func TestUsageCostMigration(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, dbFile))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations[:4] {
		if err = m(tx); err != nil {
			t.Fatal(err)
		}
	}
	if err = setMeta(tx, "schema", "4"); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	at := time.Now()
	e := UsageEvent{ID: "old", Start: at, End: at, Provider: "p", Model: "m", Outcome: OutcomeOK, TokensIn: -1, TokensOut: -1, TokenSource: "unknown"}
	// Insert the old schema directly, as the previous executable did.
	if _, err = db.Exec(`INSERT INTO usage_events(id,start,end,device_id,provider,model,outcome,token_source) VALUES(?,?,?,'d','p','m','ok','unknown')`, e.ID, ms(at), ms(at)); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`INSERT INTO usage_daily(day,user_id,project_id,provider,model,events,tokens_in,tokens_out) VALUES(?,0,0,'p','m',2,0,0)`, at.UTC().Format("2006-01-02")); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s := openT(t, dir)
	defer func() { s.Close() }() // s is reopened below: the last store must be closed (Windows cannot delete an open core.db)
	rep, err := s.AdminUsage(ctx, 7, UsageFilter{})
	if err != nil || len(rep.Rows) != 1 {
		t.Fatalf("%+v %v", rep, err)
	}
	r := rep.Rows[0]
	if r.Events != 3 || r.UnknownCost != 3 || r.ReportedCost != nil || r.EstimatedCost != nil {
		t.Fatalf("%+v", r)
	}
	// The old event keeps its data; its cost is unknown (NULL, never zero) and the schema is current.
	check := func(s *Store) {
		var cost, upstream sql.NullString
		var source, model string
		f := s.DB().QueryRow(`SELECT CAST(cost_usd_nanos AS TEXT), cost_source, upstream_model, model FROM usage_events WHERE id='old'`)
		if err := f.Scan(&cost, &source, &upstream, &model); err != nil || cost.Valid || source != "unknown" || upstream.Valid || model != "m" {
			t.Fatalf("%v %v %q %v %q", cost, upstream, source, err, model)
		}
		var v int
		if err := s.DB().QueryRow(`SELECT CAST(value AS INTEGER) FROM meta WHERE key='schema'`).Scan(&v); err != nil || v != len(migrations) {
			t.Fatalf("schema %d %v", v, err)
		}
	}
	check(s)
	// Reopening is idempotent: nothing is migrated or changed twice.
	s.Close()
	s = openT(t, dir)
	check(s)
	rep, err = s.AdminUsage(ctx, 7, UsageFilter{})
	if err != nil || len(rep.Rows) != 1 || rep.Rows[0].Events != 3 || rep.Rows[0].UnknownCost != 3 {
		t.Fatalf("%+v %v", rep, err)
	}
}

// finishHuge stores an event with a cost above the ceiling directly (FinishEvent refuses it), to test the SQL overflow defence.
func finishHuge(s *Store, e UsageEvent) error {
	huge := e.CostUsdNanos
	e.CostUsdNanos = nil
	if err := s.FinishEvent(ctx, e); err != nil {
		return err
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE usage_events SET cost_usd_nanos=?, cost_source='reported' WHERE id=?`, *huge, e.ID)
		return err
	})
}

func TestUsageCostAboveTheCeilingIsUnknown(t *testing.T) {
	f := newFx(t)
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	f.Store.now = func() time.Time { return now }
	at := now.AddDate(0, 0, -usageRetentionDays)
	big, ok := int64(math.MaxInt64), int64(MaxEventCostNanos)
	for i, c := range []*int64{&big, &big, &ok} {
		e := UsageEvent{ID: fmt.Sprint("e", i), Start: at.Add(-time.Millisecond), End: at, Provider: "p", Model: "m", Outcome: OutcomeOK, TokensIn: -1, TokensOut: -1, TokenSource: "unknown", CostUsdNanos: c, CostSource: "reported"}
		f.ok(f.FinishEvent(ctx, e))
	}
	check := func() {
		rep, err := f.AdminUsage(ctx, 401, UsageFilter{})
		if err != nil || len(rep.Rows) != 1 || rep.Rows[0].Events != 3 || rep.Rows[0].UnknownCost != 2 || rep.Rows[0].ReportedCost == nil || *rep.Rows[0].ReportedCost != ok {
			t.Fatalf("%+v %v", rep, err)
		}
	}
	check()
	if n, err := f.FoldOldUsage(ctx); err != nil || n != 3 {
		t.Fatalf("fold %d %v", n, err)
	}
	check()
}

func TestUsageCostOverflowIsAnError(t *testing.T) {
	for _, folded := range []bool{false, true} {
		t.Run(fmt.Sprint(folded), func(t *testing.T) {
			f := newFx(t)
			now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
			f.Store.now = func() time.Time { return now }
			at := now.AddDate(0, 0, -usageRetentionDays)
			cost := int64(math.MaxInt64)
			e := UsageEvent{ID: "one", Start: at.Add(-time.Millisecond), End: at, Provider: "p", Model: "m", Outcome: OutcomeOK, TokensIn: -1, TokensOut: -1, TokenSource: "unknown", CostUsdNanos: &cost, CostSource: "reported"}
			f.ok(finishHuge(f.Store, e))
			if folded {
				_, err := f.FoldOldUsage(ctx)
				f.ok(err)
			}
			e.ID, e.Start = "two", at
			f.ok(finishHuge(f.Store, e))
			if _, err := f.AdminUsage(ctx, 401, UsageFilter{}); err == nil {
				t.Fatal("overflow returned a successful report")
			}
		})
	}
}

func TestUsageCostOverflowRetainsDetailAndAllowsReopen(t *testing.T) {
	for _, folded := range []bool{false, true} {
		t.Run(fmt.Sprint(folded), func(t *testing.T) {
			dir := t.TempDir()
			s := openT(t, dir)
			at := time.Now().UTC().AddDate(0, 0, -usageRetentionDays-1)
			cost := int64(math.MaxInt64)
			e := UsageEvent{ID: "one", Start: at, End: at, Provider: "p", Model: "m", Outcome: OutcomeOK, TokensIn: -1, TokensOut: -1, TokenSource: "unknown", CostUsdNanos: &cost, CostSource: "reported"}
			if err := finishHuge(s, e); err != nil {
				t.Fatal(err)
			}
			if folded {
				if n, err := s.FoldOldUsage(ctx); err != nil || n != 1 {
					t.Fatalf("%d %v", n, err)
				}
			}
			e.ID = "two"
			if err := finishHuge(s, e); err != nil {
				t.Fatal(err)
			}
			if n, err := s.FoldOldUsage(ctx); err != nil || n != 0 {
				t.Fatalf("%d %v", n, err)
			}
			s.Close()
			s = openT(t, dir)
			defer s.Close()
			var count int
			if err := s.DB().QueryRow(`SELECT (SELECT count(*) FROM usage_events)+(SELECT COALESCE(sum(events),0) FROM usage_daily)`).Scan(&count); err != nil || count != 2 {
				t.Fatalf("%d %v", count, err)
			}
			if _, err := s.AdminUsage(ctx, 402, UsageFilter{}); err == nil {
				t.Fatal("overflow returned a successful report")
			}
		})
	}
}

// Go gateways stored provider names ("OpenRouter"), Swift ids ("openrouter"): reports use one lowercase key and the filter ignores case.
func TestUsageProviderKeyIsCaseInsensitive(t *testing.T) {
	f := newFx(t)
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	f.Store.now = func() time.Time { return now }
	cut := now.AddDate(0, 0, -usageRetentionDays)
	ev := func(id, provider string, at time.Time) UsageEvent {
		return UsageEvent{ID: id, Start: at, End: at, Provider: provider, Model: "m", Outcome: OutcomeOK, TokensIn: 1, TokensOut: 1, TokenSource: "reported"}
	}
	f.ok(f.ReplayUsage(ctx, []UsageEvent{ev("a", "OpenRouter", cut.Add(-time.Millisecond)), ev("b", "OpenRouter", cut), ev("c", "openrouter", cut)}, nil))
	n, err := f.FoldOldUsage(ctx)
	f.ok(err)
	if n != 1 {
		t.Fatalf("folded %d", n)
	}
	for _, p := range []string{"", "openrouter", "OPENROUTER", "OpenRouter"} {
		rep, err := f.AdminUsage(ctx, 401, UsageFilter{Provider: p})
		f.ok(err)
		if len(rep.Rows) != 1 || rep.Rows[0].Provider != "openrouter" || rep.Rows[0].Events != 3 {
			t.Fatalf("filter %q: %+v", p, rep.Rows)
		}
	}
}
