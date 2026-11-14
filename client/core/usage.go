package core

import (
	"context"
	"database/sql"
	"strings"
	"time"
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
	ID, ParentID string
	Start, End   time.Time
	UserID       int64
	ProjectID    int64
	DeviceID     string
	Provider     string
	Model        string
	Outcome      string
	TokensIn     int64
	TokensOut    int64
	TokenSource  string // reported, estimated, unknown
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
		_, err := tx.Exec(`INSERT OR IGNORE INTO usage_events(id,parent_id,start,user_id,device_id,project_id,provider,model,outcome,token_source)
VALUES(?,?,?,?,?,?,?,?,'open','unknown')`, e.ID, nullStr(e.ParentID), ms(e.Start), nullID(e.UserID), e.DeviceID, nullID(e.ProjectID), e.Provider, e.Model)
		return err
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
// Only the end, outcome, tokens and the project of the admitted request are set; user, device, start and parent
// stay as they were frozen.
func (s *Store) FinishEvent(ctx context.Context, e UsageEvent) error {
	return s.write(ctx, func(tx *sql.Tx) error { return finishEvent(tx, e) })
}

func finishEvent(tx *sql.Tx, e UsageEvent) error {
	if _, err := tx.Exec(`INSERT OR IGNORE INTO usage_events(id,parent_id,start,user_id,device_id,project_id,provider,model,outcome,token_source)
VALUES(?,?,?,?,?,?,?,?,'open','unknown')`, e.ID, nullStr(e.ParentID), ms(e.Start), nullID(e.UserID), e.DeviceID, nullID(e.ProjectID), e.Provider, e.Model); err != nil {
		return err
	}
	_, err := tx.Exec(`UPDATE usage_events SET end=?, outcome=?, tokens_in=?, tokens_out=?, token_source=?, project_id=COALESCE(project_id,?)
WHERE id=? AND outcome IN('open','interrupted')`, ms(e.End), e.Outcome, nullTok(e.TokensIn), nullTok(e.TokensOut), e.TokenSource, nullID(e.ProjectID), e.ID)
	return err
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
		if _, err := tx.Exec(`INSERT INTO usage_daily(day,user_id,project_id,provider,model,events,tokens_in,tokens_out)
SELECT strftime('%Y-%m-%d', start/1000, 'unixepoch'), COALESCE(user_id,0), COALESCE(project_id,0), provider, model, count(*), COALESCE(sum(tokens_in),0), COALESCE(sum(tokens_out),0)
FROM usage_events WHERE start<? AND outcome!='open' GROUP BY 1,2,3,4,5
ON CONFLICT(day,user_id,project_id,provider,model) DO UPDATE SET events=events+excluded.events, tokens_in=tokens_in+excluded.tokens_in, tokens_out=tokens_out+excluded.tokens_out`, cut); err != nil {
			return err
		}
		r, err := tx.Exec(`DELETE FROM usage_events WHERE start<? AND outcome!='open'`, cut)
		if err != nil {
			return err
		}
		n, _ = r.RowsAffected()
		return nil
	})
	return n, err
}

// UsageRow: events of one day (UTC) for one model and user. Nested events are counted once, in the totals, like any
// event; Nested says how many of them were asked by another agent (their parent's own tokens exclude them, because
// the provider reports each call separately).
type UsageRow struct {
	Day           string
	Model         string
	UserID        int64 // 0 = none
	UserName      string
	ProjectID     int64 // 0 = none
	Events        int64
	TokensIn      int64
	TokensOut     int64
	UnknownTokens int64 // events without token counts
	Nested        int64
	Failed        int64 // outcome error, cancelled or interrupted
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
}

// Usage is what a member may see (section 2): project given: an admin sees all events of the project, a developer
// the own ones, anyone else (reader, grant only, no role) gets ErrNotFound as for an invisible resource; no project:
// the caller's own events over all projects.
func (s *Store) Usage(ctx context.Context, p Principal, project int64, days int) (rep UsageReport, err error) {
	if days < 1 || days > usageRetentionDays {
		return rep, ErrInvalid{"days"}
	}
	err = s.read(ctx, func(tx *sql.Tx) error {
		if err := checkPrincipal(tx, p); err != nil {
			return err
		}
		f := UsageFilter{User: p.UserID}
		if project != 0 {
			a, err := loadAccess(tx, p.UserID, project)
			if err != nil {
				return err
			}
			switch a.Role {
			case Admin:
				f = UsageFilter{Project: project}
			case Developer:
				f = UsageFilter{Project: project, User: p.UserID}
			default:
				return ErrNotFound
			}
		}
		rep, err = s.usageRows(tx, days, f, false)
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
		rep, err = s.usageRows(tx, days, f, true)
		return err
	})
	return
}

func (s *Store) usageRows(tx *sql.Tx, days int, f UsageFilter, withFolded bool) (UsageReport, error) {
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
	q := `SELECT strftime('%Y-%m-%d', e.start/1000, 'unixepoch') d, e.model, COALESCE(e.user_id,0), COALESCE(u.name,''), COALESCE(e.project_id,0), count(*),
  COALESCE(sum(e.tokens_in),0), COALESCE(sum(e.tokens_out),0), sum(e.tokens_in IS NULL AND e.tokens_out IS NULL), sum(e.parent_id IS NOT NULL),
  sum(e.outcome IN('error','cancelled','interrupted'))
FROM usage_events e LEFT JOIN users u ON u.id=e.user_id WHERE ` + strings.Join(where, " AND ") + ` GROUP BY d, e.model, e.user_id, e.project_id ORDER BY d DESC, e.model, e.user_id`
	rows, err := tx.Query(q, args...)
	if err != nil {
		return UsageReport{}, err
	}
	var rep UsageReport
	for rows.Next() {
		var r UsageRow
		if err := rows.Scan(&r.Day, &r.Model, &r.UserID, &r.UserName, &r.ProjectID, &r.Events, &r.TokensIn, &r.TokensOut, &r.UnknownTokens, &r.Nested, &r.Failed); err != nil {
			rows.Close()
			return rep, err
		}
		rep.Rows = append(rep.Rows, r)
	}
	rows.Close()
	if withFolded {
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
		fr, err := tx.Query(`SELECT d.day, d.model, d.user_id, COALESCE(u.name,''), d.project_id, d.events, d.tokens_in, d.tokens_out FROM usage_daily d LEFT JOIN users u ON u.id=d.user_id WHERE `+strings.Join(fw, " AND ")+` ORDER BY d.day DESC`, fa...)
		if err != nil {
			return rep, err
		}
		for fr.Next() {
			var r UsageRow
			if err := fr.Scan(&r.Day, &r.Model, &r.UserID, &r.UserName, &r.ProjectID, &r.Events, &r.TokensIn, &r.TokensOut); err != nil {
				fr.Close()
				return rep, err
			}
			rep.Rows = append(rep.Rows, r)
		}
		fr.Close()
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
// request, not an access decision.
func (s *Store) BoundUser(ctx context.Context, deviceID string) (id int64) {
	s.rdb.QueryRowContext(ctx, `SELECT user_id FROM device_bindings WHERE device_id=?`, deviceID).Scan(&id)
	return
}

// UsageGapRecord is a gap as the spool keeps it (the id makes a replay idempotent).
type UsageGapRecord struct {
	ID       string
	Count    int64
	From, To time.Time
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
