package core

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

// A schema 6 database (0.6.4 to 0.6.6) migrates forward: snapshot first, old rows keep an unknown duration (NULL,
// never zero), a reopen changes nothing, and backup and restore still work.
func TestSchema6DurationMigration(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, dbFile))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations[:6] {
		if err = m(tx); err != nil {
			t.Fatal(err)
		}
	}
	if err = setMeta(tx, "schema", "6"); err != nil {
		t.Fatal(err)
	}
	at := time.Now()
	if _, err = tx.Exec(`INSERT INTO usage_events(id,start,end,device_id,provider,model,outcome,token_source) VALUES('old',?,?,'d','p','m','error','unknown')`, ms(at), ms(at)); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(`INSERT INTO usage_daily(day,user_id,project_id,provider,model,events,tokens_in,tokens_out) VALUES('2020-01-01',0,0,'p','m',4,0,0)`); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s := openT(t, dir)
	check := func(s *Store) {
		var dur, status sql.NullInt64
		var k sql.NullString
		if err := s.DB().QueryRow(`SELECT duration_ms,upstream_status,error_kind FROM usage_events WHERE id='old'`).Scan(&dur, &status, &k); err != nil || dur.Valid || status.Valid || k.Valid {
			t.Fatal(dur, status, k, err)
		}
		rep, err := s.AdminUsage(ctx, 3650, UsageFilter{})
		if err != nil || len(rep.Rows) != 2 {
			t.Fatalf("%+v %v", rep, err)
		}
		for _, r := range rep.Rows { // unknown, never zero minutes
			if r.DurationMs != 0 || r.UnknownDuration != r.Events || r.FailedDurationMs != 0 {
				t.Fatalf("%+v", r)
			}
		}
	}
	check(s)
	if snaps, _ := filepath.Glob(filepath.Join(dir, "core-6-*.db")); len(snaps) != 1 {
		t.Fatal("no core-6 snapshot", snaps)
	}
	if err = checkSchemaObjects(ctx, s.DB(), len(migrations)); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s = openT(t, dir) // idempotent
	check(s)
	if snaps, _ := filepath.Glob(filepath.Join(dir, "core-6-*.db")); len(snaps) != 1 {
		t.Fatal("second snapshot", snaps)
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

// Duration and failure kind are stored once (first finish wins), summed per day, folded without loss; a run
// recovered after a crash has no duration; invalid values become unknown instead of refusing the event.
func TestUsageDurationOutcomesAndFold(t *testing.T) {
	f := newFx(t)
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	f.Store.now = func() time.Time { return now }
	dur := func(n int64) *int64 { return &n }
	ev := func(id string, at time.Time, outcome string, d *int64, kind string) UsageEvent {
		return UsageEvent{ID: id, Start: at, End: at, Provider: "codex", Model: "claude-codex", Outcome: outcome, TokensIn: -1, TokensOut: -1, TokenSource: "unknown", DurationMs: d, ErrorKind: kind, UpstreamStatus: 200}
	}
	for _, at := range []time.Time{now, now.AddDate(0, 0, -usageRetentionDays-1)} {
		evs := []UsageEvent{
			ev("ok"+at.String(), at, OutcomeOK, dur(60000), ""),
			ev("err"+at.String(), at, OutcomeError, dur(180000), "timeout"),
			ev("badkind"+at.String(), at, OutcomeError, dur(-5), "raw error text"),
			ev("cancel"+at.String(), at, OutcomeCancelled, dur(1000), ""),
			ev("intr"+at.String(), at, OutcomeInterrupted, dur(7), ""),
		}
		f.ok(f.ReplayUsage(ctx, evs, nil))
		f.ok(f.ReplayUsage(ctx, evs, nil)) // replay counts once
	}
	var kind sql.NullString
	f.ok(f.DB().QueryRow(`SELECT error_kind FROM usage_events WHERE id LIKE 'badkind%' LIMIT 1`).Scan(&kind))
	if kind.Valid {
		t.Fatal("raw text stored as error kind", kind)
	}
	check := func(label string) {
		rep, err := f.AdminUsage(ctx, 3650, UsageFilter{})
		f.ok(err)
		var sum UsageRow
		for _, r := range rep.Rows {
			sum.Events += r.Events
			sum.DurationMs += r.DurationMs
			sum.FailedDurationMs += r.FailedDurationMs
			sum.UnknownDuration += r.UnknownDuration
			sum.ErrorEvents += r.ErrorEvents
			sum.CancelledEvents += r.CancelledEvents
			sum.InterruptedEvents += r.InterruptedEvents
		}
		// per day: 60000+180000+1000 known; the bad duration and the interrupted run are unknown
		if sum.Events != 10 || sum.DurationMs != 2*241000 || sum.FailedDurationMs != 2*180000 || sum.UnknownDuration != 4 || sum.ErrorEvents != 4 || sum.CancelledEvents != 2 || sum.InterruptedEvents != 2 {
			t.Fatalf("%s: %+v", label, sum)
		}
	}
	check("detail")
	n, err := f.FoldOldUsage(ctx)
	f.ok(err)
	if n != 5 {
		t.Fatal("folded", n)
	}
	check("folded")
	// an open event cut off by a crash: no duration
	f.ok(f.BeginEvent(ctx, UsageEvent{ID: "cut", Start: now, Provider: "codex", Model: "m"}))
	f.ok(f.write(ctx, markInterrupted))
	var d sql.NullInt64
	f.ok(f.DB().QueryRow(`SELECT duration_ms FROM usage_events WHERE id='cut'`).Scan(&d))
	if d.Valid {
		t.Fatal("recovered run has a duration", d)
	}
}
