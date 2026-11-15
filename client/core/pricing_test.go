package core

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestSchema5PricingMigration(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, dbFile))
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations[:5] {
		if err = m(tx); err != nil {
			t.Fatal(err)
		}
	}
	if err = setMeta(tx, "schema", "5"); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(`INSERT INTO usage_events(id,start,device_id,provider,model,outcome,token_source) VALUES('old',?,'','LiteLLM','m','ok','unknown')`, ms(time.Now())); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s := openT(t, dir)
	var mode string
	var price sql.NullString
	if err = s.DB().QueryRow(`SELECT billing_mode,price_snapshot_id FROM usage_events WHERE id='old'`).Scan(&mode, &price); err != nil || mode != "unknown" || price.Valid {
		t.Fatal(mode, price, err)
	}
	snaps, _ := filepath.Glob(filepath.Join(dir, "core-5-*.db"))
	if len(snaps) != 1 {
		t.Fatal("no pre-migration snapshot", snaps)
	}
	if err = checkSchemaObjects(ctx, s.DB(), 6); err != nil {
		t.Fatal(err)
	}
	bak := filepath.Join(t.TempDir(), "pricing.db")
	if err = s.Backup(ctx, bak); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if err = Restore(dir, bak); err != nil {
		t.Fatal(err)
	}
	s = openT(t, dir)
	defer s.Close()
	snaps, _ = filepath.Glob(filepath.Join(dir, "core-5-*.db"))
	if len(snaps) != 1 {
		t.Fatal(snaps)
	}
}

func TestFrozenPricesAndReplay(t *testing.T) {
	f := newFx(t)
	at := f.now()
	p := PriceSnapshot{ID: "ps_old", Provider: "LiteLLM", UpstreamModel: "base", FetchedAt: at, EffectiveDay: at.Format("2006-01-02"), Source: "/model/info", Rates: PriceRates{Unit: "USD/token", Input: "0.000002", Output: "0.000006"}}
	event := func(id string) UsageEvent {
		return UsageEvent{ID: id, Start: at, End: at, Provider: p.Provider, Model: "alias", UpstreamModel: "base", PriceSnapshot: &p, BillingMode: "metered", Outcome: OutcomeOK, TokensIn: 1000, TokensOut: 500, TokenSource: "reported"}
	}
	e := event("frozen")
	f.ok(f.BeginEvent(ctx, e))
	newer := p
	newer.ID = "ps_new"
	newer.Rates.Input = "1"
	e.PriceSnapshot = &newer
	f.ok(f.FinishEvent(ctx, e))
	e.PriceSnapshot = &p
	f.ok(f.ReplayUsage(ctx, []UsageEvent{e}, nil))
	rep, err := f.AdminUsage(ctx, 7, UsageFilter{})
	f.ok(err)
	if len(rep.Rows) != 1 || rep.Rows[0].EstimatedCost == nil || *rep.Rows[0].EstimatedCost != 5000000 || rep.Rows[0].EstimatedEvents != 1 {
		t.Fatalf("%+v", rep)
	}
	zero := int64(0)
	e = event("reported")
	e.CostUsdNanos = &zero
	e.CostSource = "reported"
	f.ok(f.FinishEvent(ctx, e))
	e = event("subscription")
	e.BillingMode = "subscription"
	e.CostUsdNanos = &zero
	e.CostSource = "reported"
	f.ok(f.FinishEvent(ctx, e))
	e = event("cache")
	e.UnpricedDimensions = true
	f.ok(f.FinishEvent(ctx, e))
	e = event("missing")
	e.PriceSnapshot = nil
	f.ok(f.BeginEvent(ctx, e))
	e.PriceSnapshot = &p
	f.ok(f.FinishEvent(ctx, e))
	rep, err = f.AdminUsage(ctx, 7, UsageFilter{})
	f.ok(err)
	r := rep.Rows[0]
	if r.Events != 5 || r.UnknownCost != 3 || r.SubscriptionEvents != 1 || r.ReportedCost == nil || *r.ReportedCost != 0 || r.EstimatedEvents != 1 {
		t.Fatalf("%+v", r)
	}
	// Folding keeps the sums and coverage, while retaining snapshot records.
	f.Store.now = func() time.Time { return at.AddDate(0, 0, 401) }
	n, err := f.FoldOldUsage(ctx)
	f.ok(err)
	if n != 5 {
		t.Fatal(n)
	}
	rep, err = f.AdminUsage(ctx, 500, UsageFilter{})
	f.ok(err)
	if len(rep.Rows) != 1 || rep.Rows[0].EstimatedEvents != 1 || rep.Rows[0].SubscriptionEvents != 1 {
		t.Fatal(rep)
	}
	var count int
	f.ok(f.DB().QueryRow(`SELECT count(*) FROM price_snapshots WHERE id='ps_old'`).Scan(&count))
	if count != 1 {
		t.Fatal(count)
	}
}

func TestPriceSnapshotIdentityCannotCrossProviders(t *testing.T) {
	f := newFx(t)
	p := PriceSnapshot{ID: "same", Provider: "one", UpstreamModel: "model", FetchedAt: f.now(), Rates: PriceRates{Unit: "USD/token", Input: "1", Output: "1"}}
	e := UsageEvent{ID: "one", Start: f.now(), End: f.now(), Provider: p.Provider, UpstreamModel: p.UpstreamModel, PriceSnapshot: &p, BillingMode: "metered", Outcome: OutcomeOK, TokensIn: 1, TokensOut: 1, TokenSource: "reported"}
	f.ok(f.FinishEvent(ctx, e))
	p.Provider = "two"
	e.ID, e.Provider = "two", "two"
	f.ok(f.FinishEvent(ctx, e))
	var id sql.NullString
	var cost sql.NullInt64
	f.ok(f.DB().QueryRow(`SELECT price_snapshot_id,cost_usd_nanos FROM usage_events WHERE id='two'`).Scan(&id, &cost))
	if id.Valid || cost.Valid {
		t.Fatal("reused a foreign price", id, cost)
	}
}

func TestCacheCountersSurviveReplay(t *testing.T) {
	f := newFx(t)
	at := f.now()
	read, write := int64(10), int64(20)
	p := &PriceSnapshot{ID: "cache_price", Provider: "litellm", UpstreamModel: "m", FetchedAt: at, Rates: PriceRates{Unit: "USD/token", Input: "0.1", Output: "0.1"}}
	e := UsageEvent{ID: "cache_counts", Start: at, End: at, Provider: "litellm", Model: "m", UpstreamModel: "m", BillingMode: "metered", PriceSnapshot: p, CacheReadTokens: &read, CacheWriteTokens: &write, TokensIn: 1, TokensOut: 2, TokenSource: "reported", Outcome: OutcomeOK}
	f.ok(f.ReplayUsage(ctx, []UsageEvent{e}, nil))
	var gotRead, gotWrite int64
	var cost sql.NullInt64
	f.ok(f.DB().QueryRow("SELECT cache_read_tokens,cache_write_tokens,cost_usd_nanos FROM usage_events WHERE id=?", e.ID).Scan(&gotRead, &gotWrite, &cost))
	if gotRead != read || gotWrite != write || cost.Valid {
		t.Fatal(gotRead, gotWrite, cost)
	}
}

func TestSnapshotIDCannotChangeRates(t *testing.T) {
	f := newFx(t)
	p := PriceSnapshot{ID: "same", Provider: "litellm", UpstreamModel: "m", FetchedAt: f.now(), Rates: PriceRates{Unit: "USD/token", Input: "0.1", Output: "0.2"}}
	e := UsageEvent{ID: "original", Start: f.now(), End: f.now(), Provider: p.Provider, UpstreamModel: p.UpstreamModel, PriceSnapshot: &p, PriceSnapshotID: p.ID, BillingMode: "metered", TokensIn: 1, TokensOut: 1, TokenSource: "reported", Outcome: OutcomeOK}
	f.ok(f.FinishEvent(ctx, e))
	p.Rates.Input = "0.3"
	e.ID = "collision"
	f.ok(f.FinishEvent(ctx, e))
	var price sql.NullString
	var cost sql.NullInt64
	f.ok(f.DB().QueryRow(`SELECT price_snapshot_id,cost_usd_nanos FROM usage_events WHERE id='collision'`).Scan(&price, &cost))
	if price.Valid || cost.Valid {
		t.Fatal("snapshot ID accepted different rates", price, cost)
	}
}

func TestMixedCostTotalOverflow(t *testing.T) {
	f := newFx(t)
	err := f.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec("INSERT INTO usage_daily(day,user_id,project_id,provider,model,events,tokens_in,tokens_out,reported_cost,estimated_cost) VALUES(?,0,0,'litellm','m',2,0,0,9223372036854775807,1)", f.now().UTC().Format("2006-01-02"))
		return err
	})
	f.ok(err)
	if _, err := f.AdminUsage(ctx, 7, UsageFilter{}); err == nil {
		t.Fatal("mixed total overflow was not rejected")
	}
}
