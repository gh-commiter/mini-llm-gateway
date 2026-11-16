package core

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"time"
)

// Scope is random, never a credential or digest. It is not in public history.
type QuotaProbe struct {
	ID           string       `json:"id"`
	Provider     string       `json:"provider"`
	AccountScope string       `json:"accountScope,omitempty"`
	ProbedAt     time.Time    `json:"probedAt"`
	Status       string       `json:"status"`
	ErrorKind    string       `json:"errorKind,omitempty"`
	Values       []QuotaValue `json:"values"`
}
type QuotaValue struct {
	Name       string     `json:"name"`
	Unit       string     `json:"unit"`
	Value      float64    `json:"value"`
	ObservedAt time.Time  `json:"observedAt,omitempty"`
	ResetsAt   *time.Time `json:"resetsAt,omitempty"`
	Source     string     `json:"source,omitempty"`
}

const QuotaHistoryLimit = 1000

func migrate8(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS quota_probes(id TEXT PRIMARY KEY, provider TEXT NOT NULL, account_scope TEXT NOT NULL, probed_at INTEGER NOT NULL, status TEXT NOT NULL CHECK(status IN('ok','error')), error_kind TEXT NOT NULL);
CREATE INDEX IF NOT EXISTS quota_probes_time ON quota_probes(probed_at,id);
CREATE TABLE IF NOT EXISTS quota_values(probe_id TEXT NOT NULL REFERENCES quota_probes(id) ON DELETE CASCADE, name TEXT NOT NULL, unit TEXT NOT NULL, value REAL NOT NULL, observed_at INTEGER NOT NULL, resets_at INTEGER, source TEXT NOT NULL, PRIMARY KEY(probe_id,name));
CREATE TABLE IF NOT EXISTS quota_gaps(id TEXT PRIMARY KEY, count INTEGER NOT NULL, from_ms INTEGER NOT NULL, to_ms INTEGER NOT NULL);`)
	return err
}
func (p QuotaProbe) Valid() bool {
	if len(p.ID) < 8 || len(p.ID) > 64 || len(p.Provider) == 0 || len(p.Provider) > 256 || len(p.AccountScope) < 8 || len(p.AccountScope) > 64 || p.ProbedAt.IsZero() || len(p.Values) > 32 {
		return false
	}
	if p.Status == "error" {
		if len(p.Values) != 0 {
			return false
		}
		switch p.ErrorKind {
		case "http", "unreachable", "invalid", "unsafe", "probe", "unavailable":
			return true
		}
		return false
	}
	if p.Status != "ok" || p.ErrorKind != "" || len(p.Values) == 0 {
		return false
	}
	names := map[string]bool{}
	for _, v := range p.Values {
		if len(v.Name) == 0 || len(v.Name) > 128 || names[v.Name] || (v.Unit != "percent" && v.Unit != "usd") || math.IsNaN(v.Value) || math.IsInf(v.Value, 0) || v.ObservedAt.IsZero() || len(v.Source) > 64 {
			return false
		}
		names[v.Name] = true
	}
	return true
}
func (s *Store) ReplayQuota(ctx context.Context, probes []QuotaProbe, gaps []UsageGapRecord) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		for _, p := range probes {
			if !p.Valid() {
				return fmt.Errorf("invalid quota probe")
			}
			if p.ProbedAt.Before(s.now().UTC().AddDate(0, 0, -usageRetentionDays)) {
				continue
			}
			r, err := tx.Exec(`INSERT OR IGNORE INTO quota_probes VALUES(?,?,?,?,?,?)`, p.ID, p.Provider, p.AccountScope, ms(p.ProbedAt), p.Status, p.ErrorKind)
			if err != nil {
				return err
			}
			n, _ := r.RowsAffected()
			if n == 0 {
				continue
			}
			for _, v := range p.Values {
				var reset any
				if v.ResetsAt != nil {
					reset = ms(*v.ResetsAt)
				}
				if _, err = tx.Exec(`INSERT INTO quota_values VALUES(?,?,?,?,?,?,?)`, p.ID, v.Name, v.Unit, v.Value, ms(v.ObservedAt), reset, v.Source); err != nil {
					return err
				}
			}
		}
		for _, g := range gaps {
			if _, err := tx.Exec(`INSERT OR IGNORE INTO quota_gaps VALUES(?,?,?,?)`, g.ID, g.Count, ms(g.From), ms(g.To)); err != nil {
				return err
			}
		}
		return nil
	})
}

// Newest first; the API signals a capped result with HTTP 206.
func (s *Store) QuotaHistory(ctx context.Context, days int, provider string) (map[string]any, bool, error) {
	if days < 1 || days > 400 {
		return nil, false, fmt.Errorf("days: 1 to 400")
	}
	cut := ms(s.now().UTC().AddDate(0, 0, -days))
	tx, err := s.rdb.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id,provider,probed_at,status,error_kind FROM quota_probes WHERE probed_at>=? AND (?='' OR provider=?) ORDER BY probed_at DESC,id DESC LIMIT ?`, cut, provider, provider, QuotaHistoryLimit+1)
	if err != nil {
		return nil, false, err
	}
	probes := []QuotaProbe{}
	for rows.Next() {
		var p QuotaProbe
		var at int64
		if err = rows.Scan(&p.ID, &p.Provider, &at, &p.Status, &p.ErrorKind); err != nil {
			break
		}
		p.ProbedAt = time.UnixMilli(at).UTC()
		probes = append(probes, p)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return nil, false, err
	}
	capped := len(probes) > QuotaHistoryLimit
	if capped {
		probes = probes[:QuotaHistoryLimit]
	}
	out := []any{}
	for _, p := range probes {
		vals := []any{}
		vr, e := tx.QueryContext(ctx, `SELECT name,unit,value,resets_at FROM quota_values WHERE probe_id=? ORDER BY name`, p.ID)
		if e != nil {
			return nil, false, e
		}
		for vr.Next() {
			var name, unit string
			var value float64
			var reset sql.NullInt64
			if err = vr.Scan(&name, &unit, &value, &reset); err != nil {
				break
			}
			v := map[string]any{"name": name, "unit": unit, "value": value}
			if reset.Valid {
				v["resetsAt"] = time.UnixMilli(reset.Int64).UTC().Format(time.RFC3339Nano)
			}
			vals = append(vals, v)
		}
		if err == nil {
			err = vr.Err()
		}
		vr.Close()
		if err != nil {
			return nil, false, err
		}
		pjson := map[string]any{"id": p.ID, "provider": p.Provider, "probedAt": p.ProbedAt.Format(time.RFC3339Nano), "status": p.Status, "values": vals}
		if p.ErrorKind != "" {
			pjson["errorKind"] = p.ErrorKind
		}
		out = append(out, pjson)
	}
	gaps := []any{}
	gr, err := tx.QueryContext(ctx, `SELECT from_ms,to_ms FROM quota_gaps WHERE to_ms>=? ORDER BY from_ms DESC,id DESC LIMIT ?`, cut, QuotaHistoryLimit+1)
	if err != nil {
		return nil, false, err
	}
	for gr.Next() {
		var a, b int64
		if err = gr.Scan(&a, &b); err != nil {
			break
		}
		if len(gaps) == QuotaHistoryLimit {
			capped = true
			break
		}
		gaps = append(gaps, map[string]any{"from": time.UnixMilli(a).UTC().Format(time.RFC3339Nano), "to": time.UnixMilli(b).UTC().Format(time.RFC3339Nano)})
	}
	if err == nil {
		err = gr.Err()
	}
	gr.Close()
	return map[string]any{"probes": out, "gaps": gaps}, capped, err
}
