package core

import (
	"database/sql"
	"fmt"
	"math"
	"path/filepath"
	"testing"
	"time"
)

func quotaSample(at time.Time) QuotaProbe {
	reset := at.Add(24 * time.Hour)
	return QuotaProbe{ID: "qp_sample", Provider: "codex", AccountScope: "qs_random", ProbedAt: at, Status: "ok", Values: []QuotaValue{
		{Name: "weekly", Unit: "percent", Value: 82, ObservedAt: at.Add(-time.Hour), ResetsAt: &reset, Source: "codex-session-log"},
		{Name: "5h", Unit: "percent", Value: 0, ObservedAt: at, Source: "codex-session-log"},
	}}
}

func TestSchema7QuotaMigration(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, dbFile))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations[:7] {
		if err = m(tx); err != nil {
			t.Fatal(err)
		}
	}
	if err = setMeta(tx, "schema", "7"); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(`INSERT INTO usage_events(id,start,end,device_id,provider,model,outcome,token_source) VALUES('old',?,?,'d','p','m','ok','unknown')`, ms(time.Now()), ms(time.Now())); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s := openT(t, dir)
	if snaps, _ := filepath.Glob(filepath.Join(dir, "core-7-*.db")); len(snaps) != 1 {
		t.Fatal(snaps)
	}
	if err = checkSchemaObjects(ctx, s.DB(), 8); err != nil {
		t.Fatal(err)
	}
	if err = s.write(ctx, migrate8); err != nil {
		t.Fatal("idempotent migration", err)
	}
	p := quotaSample(time.Now().UTC().Truncate(time.Millisecond))
	if err = s.ReplayQuota(ctx, []QuotaProbe{p}, nil); err != nil {
		t.Fatal(err)
	}
	check := func(s *Store) {
		t.Helper()
		var n int
		if e := s.DB().QueryRow(`SELECT count(*) FROM usage_events WHERE id='old'`).Scan(&n); e != nil || n != 1 {
			t.Fatal(n, e)
		}
		r, c, e := s.QuotaHistory(ctx, 7, "")
		if e != nil || c || len(r["probes"].([]any)) != 1 {
			t.Fatal(r, c, e)
		}
	}
	check(s)
	s.Close()
	s = openT(t, dir)
	check(s)
	if snaps, _ := filepath.Glob(filepath.Join(dir, "core-7-*.db")); len(snaps) != 1 {
		t.Fatal("duplicate migration", snaps)
	}
	backup := filepath.Join(t.TempDir(), "q.db")
	if err = s.Backup(ctx, backup); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if err = Restore(dir, backup); err != nil {
		t.Fatal(err)
	}
	s = openT(t, dir)
	defer s.Close()
	check(s)
}

func TestQuotaReplayFiltersRetentionAndCap(t *testing.T) {
	f := newFx(t)
	now := f.now().UTC().Truncate(time.Millisecond)
	f.Store.now = func() time.Time { return now }
	p := quotaSample(now)
	f.ok(f.ReplayQuota(ctx, []QuotaProbe{p}, nil))
	p.Values[0].Value = 99
	f.ok(f.ReplayQuota(ctx, []QuotaProbe{p}, nil))
	var v float64
	f.ok(f.DB().QueryRow(`SELECT value FROM quota_values WHERE name='weekly'`).Scan(&v))
	if v != 82 {
		t.Fatal("replay changed value", v)
	}
	var observed int64
	f.ok(f.DB().QueryRow(`SELECT observed_at FROM quota_values WHERE name='weekly'`).Scan(&observed))
	if observed != ms(now.Add(-time.Hour)) {
		t.Fatal(observed)
	}
	errProbe := QuotaProbe{ID: "qp_failed", Provider: "remote", AccountScope: "qs_new_id", ProbedAt: now.Add(-time.Hour), Status: "error", ErrorKind: "http", Values: []QuotaValue{}}
	f.ok(f.ReplayQuota(ctx, []QuotaProbe{errProbe}, []UsageGapRecord{{ID: "qgap_sample", Count: 2, From: now, To: now}}))
	rep, c, err := f.QuotaHistory(ctx, 7, "remote")
	f.ok(err)
	if c || len(rep["probes"].([]any)) != 1 {
		t.Fatal(rep)
	}
	row := rep["probes"].([]any)[0].(map[string]any)
	if row["errorKind"] != "http" || row["accountScope"] != nil || len(row["values"].([]any)) != 0 {
		t.Fatal(row)
	}
	for _, days := range []int{0, -1, 401} {
		if _, _, err = f.QuotaHistory(ctx, days, ""); err == nil {
			t.Fatal(days)
		}
	}
	// Exact 400-day boundary survives; older rows and their values are pruned.
	f.ok(f.write(ctx, func(tx *sql.Tx) error {
		_, e := tx.Exec(`UPDATE quota_probes SET probed_at=? WHERE id=?`, ms(now.AddDate(0, 0, -400)), p.ID)
		return e
	}))
	f.ok(f.write(ctx, func(tx *sql.Tx) error {
		_, e := tx.Exec(`UPDATE quota_probes SET probed_at=? WHERE id=?`, ms(now.AddDate(0, 0, -401)), errProbe.ID)
		return e
	}))
	_, err = f.FoldOldUsage(ctx)
	f.ok(err)
	var n int
	f.ok(f.DB().QueryRow(`SELECT count(*) FROM quota_probes`).Scan(&n))
	if n != 1 {
		t.Fatal(n)
	}
	f.ok(f.DB().QueryRow(`SELECT count(*) FROM quota_values`).Scan(&n))
	if n != 2 {
		t.Fatal(n)
	}
	// A stale observation is retained at the acquisition time, not observed_at.
	p.ID = "qp_stale"
	p.ProbedAt = now
	p.Values[0].ObservedAt = now.AddDate(0, 0, -500)
	many := []QuotaProbe{p}
	for i := 0; i < QuotaHistoryLimit; i++ {
		p.ID = fmt.Sprintf("qp_cap_%04d", i)
		many = append(many, p)
	}
	f.ok(f.ReplayQuota(ctx, many, nil))
	rep, c, err = f.QuotaHistory(ctx, 7, "codex")
	f.ok(err)
	if !c || len(rep["probes"].([]any)) != QuotaHistoryLimit {
		t.Fatal(c, len(rep["probes"].([]any)))
	}
	// No resurrection by an old spool after pruning.
	p.ProbedAt = now.AddDate(0, 0, -401)
	p.ID = "qp_expired"
	f.ok(f.ReplayQuota(ctx, []QuotaProbe{p}, nil))
	f.ok(f.DB().QueryRow(`SELECT count(*) FROM quota_probes WHERE id='qp_expired'`).Scan(&n))
	if n != 0 {
		t.Fatal(n)
	}
}

func TestQuotaValidation(t *testing.T) {
	p := quotaSample(time.Now())
	if !p.Valid() {
		t.Fatal(p)
	}
	for _, v := range []float64{math.Inf(1), math.NaN()} {
		q := quotaSample(time.Now())
		q.Values[0].Value = v
		if q.Valid() {
			t.Fatal(v)
		}
	}
	p.Values = append(p.Values, p.Values[0])
	if p.Valid() {
		t.Fatal("duplicate window")
	}
	p = quotaSample(time.Now())
	p.Status = "error"
	p.ErrorKind = "secret raw error"
	p.Values = nil
	if p.Valid() {
		t.Fatal("raw error")
	}
}

func TestQuotaRetentionSurvivesUsageOverflow(t *testing.T) {
	f := newFx(t)
	now := f.now().UTC()
	f.Store.now = func() time.Time { return now }
	p := quotaSample(now)
	f.ok(f.ReplayQuota(ctx, []QuotaProbe{p}, nil))
	old := now.AddDate(0, 0, -401)
	f.ok(f.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE quota_probes SET probed_at=?`, ms(old))
		return err
	}))
	cost := int64(math.MaxInt64)
	for _, id := range []string{"huge1", "huge2"} {
		f.ok(finishHuge(f.Store, UsageEvent{ID: id, Start: old, End: old, Provider: "p", Model: "m", Outcome: OutcomeOK, TokensIn: -1, TokensOut: -1, TokenSource: "unknown", CostUsdNanos: &cost, CostSource: "reported"}))
	}
	n, err := f.FoldOldUsage(ctx)
	f.ok(err)
	if n != 0 {
		t.Fatal("expected overflow to retain usage detail", n)
	}
	var left int
	f.ok(f.DB().QueryRow(`SELECT count(*) FROM quota_probes`).Scan(&left))
	if left != 0 {
		t.Fatal("quota retention rolled back", left)
	}
	f.ok(f.DB().QueryRow(`SELECT count(*) FROM quota_values`).Scan(&left))
	if left != 0 {
		t.Fatal("orphan values", left)
	}
}

// One account stays one series: the scope is stable per provider (also across a reopen), changes only on rescope, and is random.
func TestQuotaScopeIsStablePerProviderAndRandom(t *testing.T) {
	dir := t.TempDir()
	s := openT(t, dir)
	at := time.Now().UTC().Add(-time.Hour)
	probe := func(id, provider string, rescope bool) QuotaProbe {
		p := quotaSample(at)
		p.ID, p.Provider, p.AccountScope, p.Rescope = id, provider, "", rescope
		return p
	}
	scopes := func(s *Store) []string {
		rows, err := s.DB().Query(`SELECT account_scope FROM quota_probes ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var v string
			rows.Scan(&v)
			out = append(out, v)
		}
		return out
	}
	if err := s.ReplayQuota(ctx, []QuotaProbe{probe("qp_a0001", "LiteLLM", false), probe("qp_a0002", "LiteLLM", false), probe("qp_b0001", "codex", false)}, nil); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s = openT(t, dir) // a restart keeps the scope
	defer s.Close()
	if err := s.ReplayQuota(ctx, []QuotaProbe{probe("qp_a0003", "LiteLLM", false), probe("qp_a0004", "LiteLLM", true), probe("qp_a0004", "LiteLLM", true)}, nil); err != nil {
		t.Fatal(err)
	}
	got := scopes(s) // a0001 a0002 a0003 a0004 b0001
	if len(got) != 5 || got[0] != got[1] || got[1] != got[2] || got[3] == got[2] || got[4] == got[0] || len(got[0]) != 35 || got[0][:3] != "qs_" {
		t.Fatal(got)
	}
}
