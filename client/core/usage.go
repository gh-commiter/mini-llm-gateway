package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"modernc.org/sqlite"
)

// Usage events (contracts section 7): one row per model invocation, attribution frozen at admission.

// Retention: detail rows older than this are folded into daily totals.
const usageRetentionDays = 400

const (
	OutcomeOK          = "ok"
	OutcomeError       = "error"
	OutcomeCancelled   = "cancelled"
	OutcomeInterrupted = "interrupted"
	OutcomeOpen        = "open"
)

// UsageEvent: UserID and ProjectID 0 = none (NULL); TokensIn/TokensOut -1 = unknown (NULL).
type UsageEvent struct {
	DurationMs         *int64         `json:"durationMs,omitempty"`     // dispatch to finish; nil = unknown
	UpstreamStatus     int            `json:"upstreamStatus,omitempty"` // HTTP status the upstream answered; 0 = none or unknown
	ErrorKind          string         `json:"errorKind,omitempty"`      // one of ErrorKinds when Outcome is error
	CacheReadTokens    *int64         `json:"cacheReadTokens,omitempty"`
	CacheWriteTokens   *int64         `json:"cacheWriteTokens,omitempty"`
	PriceSnapshotID    string         `json:"priceSnapshotId,omitempty"`
	UnpricedDimensions bool           `json:"unpricedDimensions,omitempty"`
	PriceSnapshot      *PriceSnapshot `json:"priceSnapshot,omitempty"`
	BillingMode        string         `json:"billingMode,omitempty"`
	CostUsdNanos       *int64         `json:"costUsdNanos,omitempty"`
	CostSource         string         `json:"costSource,omitempty"`
	UpstreamModel      string         `json:"upstreamModel,omitempty"`
	ID                 string         `json:"id"`
	ParentID           string         `json:"parentId,omitempty"`
	Start              time.Time      `json:"start"`
	End                time.Time      `json:"end"`
	UserID             int64          `json:"userId,omitempty"`
	ProjectID          int64          `json:"projectId,omitempty"`
	DeviceID           string         `json:"deviceId,omitempty"`
	Provider           string         `json:"provider"`
	Model              string         `json:"model"`
	Outcome            string         `json:"outcome"`
	TokensIn           int64          `json:"tokensIn"`
	TokensOut          int64          `json:"tokensOut"`
	TokenSource        string         `json:"tokenSource"` // reported, estimated, unknown
	// Labels of the run (see labels.go); data for reports, never permissions.
	Phase          string `json:"phase,omitempty"`
	Activity       string `json:"activity,omitempty"`
	Task           string `json:"task,omitempty"`
	PhaseSource    string `json:"phaseSource,omitempty"`
	ActivitySource string `json:"activitySource,omitempty"`
	TaskSource     string `json:"taskSource,omitempty"`
}

func migrate3(tx *sql.Tx) error {
	_, err := tx.Exec(`
CREATE TABLE usage_events(id TEXT PRIMARY KEY, parent_id TEXT, start INTEGER NOT NULL, end INTEGER, user_id INTEGER, device_id TEXT NOT NULL,
  project_id INTEGER, provider TEXT NOT NULL, model TEXT NOT NULL,
  outcome TEXT NOT NULL CHECK(outcome IN('ok','error','cancelled','interrupted','open')),
  tokens_in INTEGER, tokens_out INTEGER, token_source TEXT NOT NULL CHECK(token_source IN('reported','estimated','unknown')));
CREATE INDEX usage_events_start ON usage_events(start);
CREATE INDEX usage_events_project ON usage_events(project_id, start);
CREATE INDEX usage_events_user ON usage_events(user_id, start);
CREATE INDEX usage_events_parent ON usage_events(parent_id);
CREATE TABLE usage_daily(day TEXT NOT NULL, user_id INTEGER NOT NULL, project_id INTEGER NOT NULL, provider TEXT NOT NULL, model TEXT NOT NULL,
  events INTEGER NOT NULL, tokens_in INTEGER NOT NULL, tokens_out INTEGER NOT NULL, PRIMARY KEY(day,user_id,project_id,provider,model));
CREATE TABLE usage_gaps(id TEXT PRIMARY KEY, count INTEGER NOT NULL, from_ms INTEGER NOT NULL, to_ms INTEGER NOT NULL);`)
	return err
}

// ErrorKinds is the closed set an event's error_kind may hold; never raw error text.
var ErrorKinds = []string{"http", "limit", "timeout", "unreachable", "stream", "exit", "other"}

// maxEventDurationMs bounds a stored duration (about 115 days) so that sums cannot overflow.
const maxEventDurationMs = 1e10

func migrate7(tx *sql.Tx) error { // execution time and error kind per event; sums per day. Old rows: duration unknown (NULL), never zero.
	_, err := tx.Exec(`ALTER TABLE usage_events ADD COLUMN duration_ms INTEGER CHECK(duration_ms>=0);
ALTER TABLE usage_events ADD COLUMN upstream_status INTEGER CHECK(upstream_status>=100 AND upstream_status<=599);
ALTER TABLE usage_events ADD COLUMN error_kind TEXT CHECK(error_kind IN('http','limit','timeout','unreachable','stream','exit','other'));
ALTER TABLE usage_daily ADD COLUMN duration_ms INTEGER NOT NULL DEFAULT 0;
ALTER TABLE usage_daily ADD COLUMN failed_duration_ms INTEGER NOT NULL DEFAULT 0;
ALTER TABLE usage_daily ADD COLUMN unknown_duration INTEGER NOT NULL DEFAULT 0;
ALTER TABLE usage_daily ADD COLUMN error_events INTEGER NOT NULL DEFAULT 0;
ALTER TABLE usage_daily ADD COLUMN cancelled_events INTEGER NOT NULL DEFAULT 0;
ALTER TABLE usage_daily ADD COLUMN interrupted_events INTEGER NOT NULL DEFAULT 0;
UPDATE usage_daily SET unknown_duration=events;`)
	return err
}

func migrate4(tx *sql.Tx) error { // folded totals keep the counts a report shows besides events and tokens
	for _, c := range []string{"failed", "nested", "unknown_tokens"} {
		if _, err := tx.Exec(`ALTER TABLE usage_daily ADD COLUMN ` + c + ` INTEGER NOT NULL DEFAULT 0`); err != nil {
			return err
		}
	}
	return nil
}

func migrate5(tx *sql.Tx) error {
	_, err := tx.Exec(`ALTER TABLE usage_events ADD COLUMN cost_usd_nanos INTEGER CHECK(cost_usd_nanos>=0);
ALTER TABLE usage_events ADD COLUMN cost_source TEXT NOT NULL DEFAULT 'unknown' CHECK(cost_source IN('reported','estimated','unknown'));
ALTER TABLE usage_events ADD COLUMN upstream_model TEXT;
ALTER TABLE usage_daily ADD COLUMN reported_cost INTEGER;
ALTER TABLE usage_daily ADD COLUMN estimated_cost INTEGER;
ALTER TABLE usage_daily ADD COLUMN unknown_cost INTEGER NOT NULL DEFAULT 0;
UPDATE usage_daily SET unknown_cost=events;`)
	return err
}

// capStr limits a stored string to n bytes (at a character boundary): an event is never refused for its model name.
func capStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

func nullID(n int64) any {
	if n == 0 {
		return nil
	}
	return n
}

func nullTok(n int64) any {
	if n < 0 {
		return nil
	}
	return n
}

// BeginEvent records an admitted invocation as open (idempotent by id).
func (s *Store) BeginEvent(ctx context.Context, e UsageEvent) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		return beginEvent(tx, e)
	})
}

func nullStr(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// FinishEvent completes an event; it inserts the whole row if Begin never reached the database (spool replay). It is
// idempotent: a row that is already finished is left as it is (the first finish wins), so a replay counts once.
// Only the end, outcome, tokens, provider and the project of the admitted request are set; user, device, start and parent
// stay as they were frozen.
func (s *Store) FinishEvent(ctx context.Context, e UsageEvent) error {
	return s.write(ctx, func(tx *sql.Tx) error { return finishEvent(tx, e) })
}

// MaxEventCostNanos caps reported and estimated costs at 1,000,000 USD per event.
const MaxEventCostNanos = 1e15

func finishEvent(tx *sql.Tx, e UsageEvent) error {
	if err := beginEvent(tx, e); err != nil {
		return err
	}
	var rates sql.NullString
	var mode string
	if err := tx.QueryRow(`SELECT p.rates_json,e.billing_mode FROM usage_events e LEFT JOIN price_snapshots p ON p.id=e.price_snapshot_id WHERE e.id=?`, e.ID).Scan(&rates, &mode); err != nil {
		return err
	}
	if e.CostUsdNanos == nil && mode == "metered" && !e.UnpricedDimensions && (e.CacheReadTokens == nil || *e.CacheReadTokens == 0) && (e.CacheWriteTokens == nil || *e.CacheWriteTokens == 0) && rates.Valid && e.TokenSource == "reported" {
		var p PriceRates
		if json.Unmarshal([]byte(rates.String), &p) == nil {
			e.CostUsdNanos = p.Estimate(e.TokensIn, e.TokensOut)
			e.CostSource = "estimated"
		}
	}
	if mode == "subscription" || mode == "local" {
		e.CostUsdNanos = nil
	}

	source := e.CostSource
	if e.CostUsdNanos == nil || *e.CostUsdNanos < 0 || *e.CostUsdNanos > MaxEventCostNanos || (source != "reported" && source != "estimated") {
		e.CostUsdNanos = nil
		source = "unknown"
	}
	var dur, status, kind any // a duration or kind that is not valid is unknown, not refused
	if e.DurationMs != nil && *e.DurationMs >= 0 && *e.DurationMs <= maxEventDurationMs && e.Outcome != OutcomeInterrupted {
		dur = *e.DurationMs
	}
	if e.UpstreamStatus >= 100 && e.UpstreamStatus <= 599 {
		status = e.UpstreamStatus
	}
	if e.Outcome == OutcomeError && slices.Contains(ErrorKinds, e.ErrorKind) {
		kind = e.ErrorKind
	}
	_, err := tx.Exec(`UPDATE usage_events SET duration_ms=?, upstream_status=?, error_kind=?, cost_usd_nanos=?, cost_source=?, cache_read_tokens=?, cache_write_tokens=?, upstream_model=COALESCE(upstream_model,?), end=?, outcome=?, tokens_in=?, tokens_out=?, token_source=?, project_id=COALESCE(project_id,?), provider=CASE WHEN provider='' THEN ? ELSE provider END
WHERE id=? AND outcome IN('open','interrupted')`, dur, status, kind, e.CostUsdNanos, source, validCacheTokens(e.CacheReadTokens), validCacheTokens(e.CacheWriteTokens), nullStr(capStr(e.UpstreamModel, 256)), ms(e.End), e.Outcome, nullTok(e.TokensIn), nullTok(e.TokensOut), e.TokenSource, nullID(e.ProjectID), capStr(e.Provider, 64), e.ID)
	return err
}

func validCacheTokens(n *int64) *int64 {
	if n == nil || *n < 0 {
		return nil
	}
	return n
}

// DiscardEvent removes an open event of a request that turned out to be a refusal, not an invocation.
func (s *Store) DiscardEvent(ctx context.Context, id string) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM usage_events WHERE id=? AND outcome='open'`, id)
		return err
	})
}

// AddGap records events that were lost (spool overflow or a full queue); idempotent by id.
func (s *Store) AddGap(ctx context.Context, id string, count int64, from, to time.Time) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`INSERT OR IGNORE INTO usage_gaps(id,count,from_ms,to_ms) VALUES(?,?,?,?)`, id, count, ms(from), ms(to))
		return err
	})
}

// markInterrupted: rows still open belong to an earlier process (the folder lock allows one): they were cut off.
func markInterrupted(tx *sql.Tx) error {
	_, err := tx.Exec(`UPDATE usage_events SET outcome='interrupted', end=start WHERE outcome='open'`)
	return err
}

// FoldOldUsage folds detail rows older than the retention into daily totals (one transaction); returns the rows folded.
func (s *Store) FoldOldUsage(ctx context.Context) (n int64, err error) {
	cut := ms(s.now().UTC().AddDate(0, 0, -usageRetentionDays))
	err = s.write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM quota_probes WHERE probed_at<?`, cut); err != nil {
			return err
		}
		_, err := tx.Exec(`DELETE FROM quota_gaps WHERE to_ms<?`, cut)
		return err
	})
	if err != nil {
		return 0, err
	}
	err = s.write(ctx, func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO usage_daily(day,user_id,project_id,provider,model,events,tokens_in,tokens_out,failed,nested,unknown_tokens,reported_cost,estimated_cost,unknown_cost,estimated_events,subscription_events,unknown_billing_events,duration_ms,failed_duration_ms,unknown_duration,error_events,cancelled_events,interrupted_events)
SELECT strftime('%Y-%m-%d', start/1000, 'unixepoch'), COALESCE(user_id,0), COALESCE(project_id,0), provider, model, count(*), COALESCE(sum(tokens_in),0), COALESCE(sum(tokens_out),0),
  sum(outcome IN('error','cancelled','interrupted')), sum(parent_id IS NOT NULL), sum(tokens_in IS NULL AND tokens_out IS NULL), sum(CASE WHEN cost_source='reported' THEN cost_usd_nanos END), sum(CASE WHEN cost_source='estimated' THEN cost_usd_nanos END), sum(cost_usd_nanos IS NULL), sum(cost_source='estimated'), sum(billing_mode='subscription'), sum(billing_mode='unknown'), COALESCE(sum(duration_ms),0), COALESCE(sum(CASE WHEN outcome='error' THEN duration_ms END),0), sum(duration_ms IS NULL), sum(outcome='error'), sum(outcome='cancelled'), sum(outcome='interrupted')
FROM usage_events WHERE start<? AND outcome!='open' GROUP BY 1,2,3,4,5
ON CONFLICT(day,user_id,project_id,provider,model) DO UPDATE SET events=events+excluded.events, tokens_in=tokens_in+excluded.tokens_in, tokens_out=tokens_out+excluded.tokens_out,
  failed=failed+excluded.failed, nested=nested+excluded.nested, unknown_tokens=unknown_tokens+excluded.unknown_tokens, reported_cost=CASE WHEN reported_cost IS NULL AND excluded.reported_cost IS NULL THEN NULL WHEN COALESCE(reported_cost,0)>9223372036854775807-COALESCE(excluded.reported_cost,0) THEN abs(-9223372036854775808) ELSE COALESCE(reported_cost,0)+COALESCE(excluded.reported_cost,0) END, estimated_cost=CASE WHEN estimated_cost IS NULL AND excluded.estimated_cost IS NULL THEN NULL WHEN COALESCE(estimated_cost,0)>9223372036854775807-COALESCE(excluded.estimated_cost,0) THEN abs(-9223372036854775808) ELSE COALESCE(estimated_cost,0)+COALESCE(excluded.estimated_cost,0) END, unknown_cost=unknown_cost+excluded.unknown_cost, estimated_events=estimated_events+excluded.estimated_events, subscription_events=subscription_events+excluded.subscription_events, unknown_billing_events=unknown_billing_events+excluded.unknown_billing_events, duration_ms=duration_ms+excluded.duration_ms, failed_duration_ms=failed_duration_ms+excluded.failed_duration_ms, unknown_duration=unknown_duration+excluded.unknown_duration, error_events=error_events+excluded.error_events, cancelled_events=cancelled_events+excluded.cancelled_events, interrupted_events=interrupted_events+excluded.interrupted_events`, cut); err != nil {
			return err
		}
		r, err := tx.Exec(`DELETE FROM usage_events WHERE start<? AND outcome!='open'`, cut)
		if err != nil {
			return err
		}
		n, _ = r.RowsAffected()
		return nil
	})
	// An unrepresentable total must not prevent startup. The transaction rolled back,
	// so retain the detail; reports still return the overflow instead of a false total.
	var sqlErr *sqlite.Error
	if errors.As(err, &sqlErr) && sqlErr.Code() == 1 && strings.Contains(sqlErr.Error(), "integer overflow") {
		return 0, nil
	}
	return n, err
}

// UsageRow: events of one day (UTC) for one model and user. Nested events are counted once, in the totals, like any
// event; Nested says how many of them were asked by another agent (their parent's own tokens exclude them, because
// the provider reports each call separately).
type UsageRow struct {
	EstimatedEvents, SubscriptionEvents, UnknownBillingEvents int64
	Provider                                                  string
	ReportedCost, EstimatedCost                               *int64
	UnknownCost                                               int64
	Day                                                       string
	Phase, Activity, Task                                     string // only in a labelled report (UsageFilter.Labels)
	PhaseSource, ActivitySource, TaskSource                   string
	Model                                                     string
	UserID                                                    int64 // 0 = none
	UserName                                                  string
	ProjectID                                                 int64 // 0 = none
	Events                                                    int64
	TokensIn                                                  int64
	TokensOut                                                 int64
	CacheReadTokens                                           int64 // input tokens read from the provider's prompt cache
	CacheWriteTokens                                          int64 // input tokens written to it; folded rows have none
	UnknownTokens                                             int64 // events without token counts
	Nested                                                    int64
	Failed                                                    int64 // outcome error, cancelled or interrupted
	// Execution time (dispatch to finish) of events whose duration is known; UnknownDuration events have none.
	// FailedDurationMs is the time of outcome-error events only.
	DurationMs, FailedDurationMs, UnknownDuration   int64
	ErrorEvents, CancelledEvents, InterruptedEvents int64
}

type UsageGap struct {
	Count    int64
	From, To time.Time
}

type UsageReport struct {
	Rows []UsageRow
	Gaps []UsageGap
}

type UsageFilter struct {
	User, Project int64
	Model         string
	Provider      string
	// Phase, Activity, Task select events by label (data, never a permission: the other filters, user and project,
	// are decided by the caller's rights before this). Labels splits the rows by label for a report grouped by them;
	// folded totals carry no labels and appear as unlabelled rows (none when a label filter is set).
	Phase, Activity, Task string
	Labels                bool
}

// Usage is what a member may see (section 2): project given: an admin sees all events of the project, a developer
// the own ones, anyone else (reader, grant only, no role) gets ErrNotFound as for an invisible resource; no project:
// the caller's own events over all projects.
// LabelQuery selects by label and splits rows by label (see UsageFilter); it never widens whose events are read.
type LabelQuery struct {
	Phase, Activity, Task string
	Labels                bool
}

func (l LabelQuery) apply(f UsageFilter) UsageFilter {
	f.Phase, f.Activity, f.Task, f.Labels = l.Phase, l.Activity, l.Task, l.Labels
	return f
}

func (s *Store) Usage(ctx context.Context, p Principal, project int64, days int, lq ...LabelQuery) (rep UsageReport, err error) {
	var q LabelQuery
	if len(lq) > 0 {
		q = lq[0]
	}
	lim := s.rate(p.UserID, false) // a read like any other: counted now, answered after the checks (L5)
	if days < 1 || days > usageRetentionDays {
		return rep, ErrInvalid{"days"}
	}
	err = s.read(ctx, func(tx *sql.Tx) error {
		if err := checkPrincipal(tx, p); err != nil {
			return err
		}
		gaps := false // lost-event records are for project admins (their project's period) and the owner
		f := UsageFilter{User: p.UserID}
		if project != 0 {
			if isSegment(project) {
				return ErrNotFound
			}
			a, err := loadAccess(tx, p.UserID, project)
			if err != nil {
				return err
			}
			switch a.Role {
			case Admin:
				f, gaps = UsageFilter{Project: project}, true
			case Developer:
				f = UsageFilter{Project: project, User: p.UserID}
			default:
				return ErrNotFound
			}
		}
		if lim != nil {
			return lim
		}
		rep, err = s.usageRows(tx, days, q.apply(f), false, gaps)
		return err
	})
	return
}

// AdminUsage is the owner's view: everything, also events without user or project; older than the detail retention
// from the folded totals; days up to 3650.
func (s *Store) AdminUsage(ctx context.Context, days int, f UsageFilter) (rep UsageReport, err error) {
	if days < 1 || days > 3650 {
		return rep, ErrInvalid{"days"}
	}
	err = s.read(ctx, func(tx *sql.Tx) error {
		rep, err = s.usageRows(tx, days, f, true, true)
		return err
	})
	return
}

func (s *Store) usageRows(tx *sql.Tx, days int, f UsageFilter, withFolded, withGaps bool) (UsageReport, error) {
	now := s.now().UTC()
	from := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -(days - 1))
	where, args := []string{"e.start>=?"}, []any{ms(from)}
	if f.User != 0 {
		where, args = append(where, "e.user_id=?"), append(args, f.User)
	}
	if f.Project != 0 {
		where, args = append(where, "e.project_id=?"), append(args, f.Project)
	}
	if f.Model != "" {
		where, args = append(where, "e.model=?"), append(args, f.Model)
	}
	if f.Provider != "" {
		where, args = append(where, "lower(e.provider)=lower(?)"), append(args, f.Provider)
	}
	for _, l := range []struct{ col, val string }{{"phase", f.Phase}, {"activity", f.Activity}, {"task", f.Task}} {
		if l.val != "" {
			where, args = append(where, "e."+l.col+"=?"), append(args, l.val)
		}
	}
	labelCols, labelGroup := `'','','','','',''`, ""
	if f.Labels {
		labelCols, labelGroup = `e.phase,e.activity,e.task,e.phase_source,e.activity_source,e.task_source`, `, e.phase, e.activity, e.task, e.phase_source, e.activity_source, e.task_source`
	}
	q := `SELECT strftime('%Y-%m-%d', e.start/1000, 'unixepoch') d, lower(e.provider), e.model, COALESCE(e.user_id,0), COALESCE(u.name,''), COALESCE(e.project_id,0), ` + labelCols + `, count(*),
  COALESCE(sum(e.tokens_in),0), COALESCE(sum(e.tokens_out),0), COALESCE(sum(e.cache_read_tokens),0), COALESCE(sum(e.cache_write_tokens),0), sum(e.tokens_in IS NULL AND e.tokens_out IS NULL), sum(e.parent_id IS NOT NULL),
  sum(e.outcome IN('error','cancelled','interrupted')), sum(CASE WHEN e.cost_source='reported' THEN e.cost_usd_nanos END), sum(CASE WHEN e.cost_source='estimated' THEN e.cost_usd_nanos END), sum(e.cost_usd_nanos IS NULL), sum(e.cost_source='estimated'), sum(e.billing_mode='subscription'), sum(e.billing_mode='unknown'), COALESCE(sum(e.duration_ms),0), COALESCE(sum(CASE WHEN e.outcome='error' THEN e.duration_ms END),0), sum(e.duration_ms IS NULL), sum(e.outcome='error'), sum(e.outcome='cancelled'), sum(e.outcome='interrupted')
FROM usage_events e LEFT JOIN users u ON u.id=e.user_id WHERE ` + strings.Join(where, " AND ") + ` GROUP BY d, lower(e.provider), e.model, e.user_id, e.project_id` + labelGroup + ` ORDER BY d DESC, e.model, e.user_id`
	rows, err := tx.Query(q, args...)
	if err != nil {
		return UsageReport{}, err
	}
	var rep UsageReport
	for rows.Next() {
		var r UsageRow
		if err := rows.Scan(&r.Day, &r.Provider, &r.Model, &r.UserID, &r.UserName, &r.ProjectID, &r.Phase, &r.Activity, &r.Task, &r.PhaseSource, &r.ActivitySource, &r.TaskSource, &r.Events, &r.TokensIn, &r.TokensOut, &r.CacheReadTokens, &r.CacheWriteTokens, &r.UnknownTokens, &r.Nested, &r.Failed, &r.ReportedCost, &r.EstimatedCost, &r.UnknownCost, &r.EstimatedEvents, &r.SubscriptionEvents, &r.UnknownBillingEvents, &r.DurationMs, &r.FailedDurationMs, &r.UnknownDuration, &r.ErrorEvents, &r.CancelledEvents, &r.InterruptedEvents); err != nil {
			rows.Close()
			return rep, err
		}
		rep.Rows = append(rep.Rows, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return rep, err
	}
	rows.Close()
	if withFolded && f.Phase == "" && f.Activity == "" && f.Task == "" {
		fw, fa := []string{"d.day>=?"}, []any{from.Format("2006-01-02")}
		if f.User != 0 {
			fw, fa = append(fw, "d.user_id=?"), append(fa, f.User)
		}
		if f.Project != 0 {
			fw, fa = append(fw, "d.project_id=?"), append(fa, f.Project)
		}
		if f.Model != "" {
			fw, fa = append(fw, "d.model=?"), append(fa, f.Model)
		}
		if f.Provider != "" {
			fw, fa = append(fw, "lower(d.provider)=lower(?)"), append(fa, f.Provider)
		}
		fr, err := tx.Query(`SELECT d.day, lower(d.provider), d.model, d.user_id, COALESCE(u.name,''), d.project_id, d.events, d.tokens_in, d.tokens_out, d.unknown_tokens, d.nested, d.failed, d.reported_cost, d.estimated_cost, d.unknown_cost, d.estimated_events, d.subscription_events, d.unknown_billing_events, d.duration_ms, d.failed_duration_ms, d.unknown_duration, d.error_events, d.cancelled_events, d.interrupted_events FROM usage_daily d LEFT JOIN users u ON u.id=d.user_id WHERE `+strings.Join(fw, " AND ")+` ORDER BY d.day DESC`, fa...)
		if err != nil {
			return rep, err
		}
		for fr.Next() {
			var r UsageRow
			if err := fr.Scan(&r.Day, &r.Provider, &r.Model, &r.UserID, &r.UserName, &r.ProjectID, &r.Events, &r.TokensIn, &r.TokensOut, &r.UnknownTokens, &r.Nested, &r.Failed, &r.ReportedCost, &r.EstimatedCost, &r.UnknownCost, &r.EstimatedEvents, &r.SubscriptionEvents, &r.UnknownBillingEvents, &r.DurationMs, &r.FailedDurationMs, &r.UnknownDuration, &r.ErrorEvents, &r.CancelledEvents, &r.InterruptedEvents); err != nil {
				fr.Close()
				return rep, err
			}
			rep.Rows = append(rep.Rows, r)
		}
		if err := fr.Err(); err != nil {
			fr.Close()
			return rep, err
		}
		fr.Close()
	}
	// Folded and live detail can overlap on the retention day.
	type key struct {
		day, provider, model string
		user, project        int64
		labels               [6]string
	}
	merged := map[key]UsageRow{}
	addCost := func(a, b *int64) (*int64, error) {
		if a == nil {
			return b, nil
		}
		if b == nil {
			return a, nil
		}
		if *a > math.MaxInt64-*b {
			return nil, fmt.Errorf("usage cost total overflows nanodollars")
		}
		n := *a + *b
		return &n, nil
	}
	for _, r := range rep.Rows {
		k := key{r.Day, r.Provider, r.Model, r.UserID, r.ProjectID, [6]string{r.Phase, r.Activity, r.Task, r.PhaseSource, r.ActivitySource, r.TaskSource}}
		if a, ok := merged[k]; ok {
			a.Events += r.Events
			a.TokensIn += r.TokensIn
			a.TokensOut += r.TokensOut
			a.CacheReadTokens += r.CacheReadTokens
			a.CacheWriteTokens += r.CacheWriteTokens
			a.UnknownTokens += r.UnknownTokens
			a.Nested += r.Nested
			a.Failed += r.Failed
			a.UnknownCost += r.UnknownCost
			a.EstimatedEvents += r.EstimatedEvents
			a.SubscriptionEvents += r.SubscriptionEvents
			a.UnknownBillingEvents += r.UnknownBillingEvents
			a.DurationMs += r.DurationMs
			a.FailedDurationMs += r.FailedDurationMs
			a.UnknownDuration += r.UnknownDuration
			a.ErrorEvents += r.ErrorEvents
			a.CancelledEvents += r.CancelledEvents
			a.InterruptedEvents += r.InterruptedEvents
			var err error
			if a.ReportedCost, err = addCost(a.ReportedCost, r.ReportedCost); err != nil {
				return rep, err
			}
			if a.EstimatedCost, err = addCost(a.EstimatedCost, r.EstimatedCost); err != nil {
				return rep, err
			}
			merged[k] = a
		} else {
			merged[k] = r
		}
	}
	rep.Rows = nil
	for _, r := range merged {
		if r.ReportedCost != nil && r.EstimatedCost != nil && *r.ReportedCost > math.MaxInt64-*r.EstimatedCost {
			return rep, fmt.Errorf("usage cost total overflows nanodollars")
		}
		rep.Rows = append(rep.Rows, r)
	}
	sort.Slice(rep.Rows, func(i, j int) bool {
		a, b := rep.Rows[i], rep.Rows[j]
		if a.Day != b.Day {
			return a.Day > b.Day
		}
		if a.Provider != b.Provider {
			return a.Provider < b.Provider
		}
		if a.Model != b.Model {
			return a.Model < b.Model
		}
		if a.UserID != b.UserID {
			return a.UserID < b.UserID
		}
		if a.ProjectID != b.ProjectID {
			return a.ProjectID < b.ProjectID
		}
		return a.Phase+"\x00"+a.Activity+"\x00"+a.Task < b.Phase+"\x00"+b.Activity+"\x00"+b.Task
	})
	if !withGaps {
		return rep, nil
	}
	gr, err := tx.Query(`SELECT count, from_ms, to_ms FROM usage_gaps WHERE to_ms>=? ORDER BY from_ms`, ms(from))
	if err != nil {
		return rep, err
	}
	defer gr.Close()
	for gr.Next() {
		var g UsageGap
		var a, b int64
		if err := gr.Scan(&g.Count, &a, &b); err != nil {
			return rep, err
		}
		g.From, g.To = fromMs(a), fromMs(b)
		rep.Gaps = append(rep.Gaps, g)
	}
	return rep, gr.Err()
}

// BoundUser is the user a device is bound to (0 = none), whether or not the user is active: attribution of a model
// request, not an access decision. It reads an in-memory snapshot that is reloaded after every binding change, so a
// model request does no database I/O for it (A3).
func (s *Store) BoundUser(ctx context.Context, deviceID string) (int64, error) {
	if m := s.bound.Load(); m != nil {
		return (*m)[deviceID], nil
	}
	return 0, nil
}

func (s *Store) reloadBound(ctx context.Context) {
	rows, err := s.rdb.QueryContext(ctx, `SELECT device_id, user_id FROM device_bindings`)
	if err != nil {
		return
	}
	defer rows.Close()
	m := map[string]int64{}
	for rows.Next() {
		var d string
		var u int64
		if rows.Scan(&d, &u) == nil {
			m[d] = u
		}
	}
	if rows.Err() == nil {
		s.bound.Store(&m)
	}
}

// UsageGapRecord is a gap as the spool keeps it (the id makes a replay idempotent).
type UsageGapRecord struct {
	ID    string    `json:"id"`
	Count int64     `json:"count"`
	From  time.Time `json:"from"`
	To    time.Time `json:"to"`
}

// ReplayUsage stores spooled events and gaps in one transaction. It is idempotent: events by FinishEvent's rule,
// gaps by id, so a replay that was interrupted before the spool file was cleared can simply run again.
func (s *Store) ReplayUsage(ctx context.Context, evs []UsageEvent, gaps []UsageGapRecord) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		for _, e := range evs {
			if err := finishEvent(tx, e); err != nil {
				return err
			}
		}
		for _, g := range gaps {
			if _, err := tx.Exec(`INSERT OR IGNORE INTO usage_gaps(id,count,from_ms,to_ms) VALUES(?,?,?,?)`, g.ID, g.Count, ms(g.From), ms(g.To)); err != nil {
				return err
			}
		}
		return nil
	})
}
