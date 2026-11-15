package core

import (
	"database/sql"
	"encoding/json"
	"math/big"
	"regexp"
	"time"
)

// PriceSnapshot travels with the event so spool replay never needs a live catalog.
// Rates are decimal USD per token; unsupported billing dimensions are not estimated.
type PriceSnapshot struct {
	ID            string     `json:"id"`
	Provider      string     `json:"provider"`
	UpstreamModel string     `json:"upstreamModel"`
	FetchedAt     time.Time  `json:"fetchedAt"`
	EffectiveDay  string     `json:"effectiveDay"`
	Source        string     `json:"source"`
	Rates         PriceRates `json:"rates"`
}
type PriceRates struct {
	Unit   string `json:"unit"`
	Input  string `json:"input"`
	Output string `json:"output"`
}

var priceDecimal = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?([eE][+-]?[0-9]{1,3})?$`)

func rate(s string) *big.Rat {
	if len(s) > 128 || !priceDecimal.MatchString(s) {
		return nil
	}
	r, ok := new(big.Rat).SetString(s)
	if !ok || r.Sign() < 0 || r.Cmp(big.NewRat(1000000, 1)) > 0 {
		return nil
	}
	return r
}
func (p PriceRates) Valid() bool {
	return p.Unit == "USD/token" && rate(p.Input) != nil && rate(p.Output) != nil
}
func (p PriceRates) Estimate(in, out int64) *int64 {
	if !p.Valid() || in < 0 || out < 0 {
		return nil
	}
	// Arbitrary precision guards every product and rounds the sum only once.
	n := new(big.Rat).Mul(rate(p.Input), new(big.Rat).SetInt64(in))
	n.Add(n, new(big.Rat).Mul(rate(p.Output), new(big.Rat).SetInt64(out)))
	n.Mul(n, big.NewRat(1000000000, 1))
	q, rem := new(big.Int), new(big.Int)
	q.QuoRem(n.Num(), n.Denom(), rem)
	if new(big.Int).Lsh(rem, 1).Cmp(n.Denom()) >= 0 {
		q.Add(q, big.NewInt(1))
	}
	if !q.IsInt64() || q.Int64() > MaxEventCostNanos {
		return nil
	}
	v := q.Int64()
	return &v
}
func migrate6(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE price_snapshots(id TEXT PRIMARY KEY, provider TEXT NOT NULL, upstream_model TEXT NOT NULL, fetched_at INTEGER NOT NULL, effective_day TEXT NOT NULL, source TEXT NOT NULL, rates_json TEXT NOT NULL);
ALTER TABLE usage_events ADD COLUMN price_snapshot_id TEXT REFERENCES price_snapshots(id);
ALTER TABLE usage_events ADD COLUMN cache_read_tokens INTEGER;
ALTER TABLE usage_events ADD COLUMN cache_write_tokens INTEGER;
ALTER TABLE usage_events ADD COLUMN billing_mode TEXT NOT NULL DEFAULT 'unknown' CHECK(billing_mode IN('metered','subscription','local','unknown'));
ALTER TABLE usage_daily ADD COLUMN estimated_events INTEGER NOT NULL DEFAULT 0;
ALTER TABLE usage_daily ADD COLUMN subscription_events INTEGER NOT NULL DEFAULT 0;
ALTER TABLE usage_daily ADD COLUMN unknown_billing_events INTEGER NOT NULL DEFAULT 0;
UPDATE usage_daily SET unknown_billing_events=events;`)
	return err
}
func beginEvent(tx *sql.Tx, e UsageEvent) error {
	// A finish/replay of a recorded event must not create a new price basis.
	var exists int
	if err := tx.QueryRow(`SELECT count(*) FROM usage_events WHERE id=?`, e.ID).Scan(&exists); err != nil {
		return err
	}
	if exists != 0 {
		return nil
	}
	var id any
	if e.PriceSnapshotID != "" && e.PriceSnapshot == nil {
		var found string
		if err := tx.QueryRow(`SELECT id FROM price_snapshots WHERE id=? AND provider=? AND upstream_model=?`, e.PriceSnapshotID, e.Provider, e.UpstreamModel).Scan(&found); err == nil {
			id = found
		} else if err != sql.ErrNoRows {
			return err
		}
	}
	if p := e.PriceSnapshot; p != nil && p.ID != "" && len(p.ID) <= 128 && p.Provider == e.Provider && p.UpstreamModel == e.UpstreamModel && p.Rates.Valid() && !p.FetchedAt.IsZero() {
		b, _ := json.Marshal(p.Rates)
		if _, err := tx.Exec(`INSERT OR IGNORE INTO price_snapshots VALUES(?,?,?,?,?,?,?)`, p.ID, p.Provider, p.UpstreamModel, ms(p.FetchedAt), p.EffectiveDay, p.Source, string(b)); err != nil {
			return err
		}
		var found string
		if err := tx.QueryRow(`SELECT id FROM price_snapshots WHERE id=? AND provider=? AND upstream_model=? AND rates_json=?`, p.ID, e.Provider, e.UpstreamModel, string(b)).Scan(&found); err == nil {
			id = found
		} else if err != sql.ErrNoRows {
			return err
		}
	}
	mode := e.BillingMode
	if mode != "metered" && mode != "subscription" && mode != "local" {
		mode = "unknown"
	}
	_, err := tx.Exec(`INSERT OR IGNORE INTO usage_events(id,parent_id,start,user_id,device_id,project_id,provider,model,upstream_model,outcome,token_source,price_snapshot_id,billing_mode) VALUES(?,?,?,?,?,?,?,?,?,'open','unknown',?,?)`, e.ID, nullStr(e.ParentID), ms(e.Start), nullID(e.UserID), capStr(e.DeviceID, 256), nullID(e.ProjectID), capStr(e.Provider, 64), capStr(e.Model, 256), nullStr(capStr(e.UpstreamModel, 256)), id, mode)
	return err
}
