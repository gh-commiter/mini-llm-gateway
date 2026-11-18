package core

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"time"
)

// SessionMetric distinguishes recorded zero from missing and partial counts.
type SessionMetric struct {
	Value   *int64 `json:"value"`
	Unknown int64  `json:"unknownEvents"`
}

func (m *SessionMetric) add(v sql.NullInt64) error {
	if !v.Valid {
		m.Unknown++
		return nil
	}
	if v.Int64 < 0 || m.Value != nil && *m.Value > math.MaxInt64-v.Int64 {
		return fmt.Errorf("session total overflow")
	}
	if m.Value == nil {
		n := int64(0)
		m.Value = &n
	}
	*m.Value += v.Int64
	return nil
}

type SessionRow struct {
	ID            string        `json:"id"`
	ProjectID     int64         `json:"projectId"`
	UserID        int64         `json:"userId"`
	UserName      string        `json:"userName"`
	DeviceID      string        `json:"deviceId"`
	DeviceName    string        `json:"deviceName"`
	Models        []string      `json:"models"`
	Start         time.Time     `json:"start"`
	End           *time.Time    `json:"end"`
	DurationMs    *int64        `json:"durationMs"`
	Requests      int64         `json:"requests"`
	TokensIn      SessionMetric `json:"tokensIn"`
	TokensOut     SessionMetric `json:"tokensOut"`
	CacheRead     SessionMetric `json:"cacheReadTokens"`
	CacheWrite    SessionMetric `json:"cacheWriteTokens"`
	Cost          *string       `json:"costUsd"`
	ReportedCost  *string       `json:"reportedCostUsd"`
	EstimatedCost *string       `json:"estimatedCostUsd"`
	UnknownCost   int64         `json:"eventsWithoutCost"`
	Outcome       string        `json:"outcome"`
	Incomplete    bool          `json:"incomplete"`
	// Labels of the session's first request (data, never permissions).
	Phase          string `json:"phase,omitempty"`
	Activity       string `json:"activity,omitempty"`
	Task           string `json:"task,omitempty"`
	PhaseSource    string `json:"phaseSource,omitempty"`
	ActivitySource string `json:"activitySource,omitempty"`
	TaskSource     string `json:"taskSource,omitempty"`
}
type SessionPage struct {
	Sessions []SessionRow `json:"sessions"`
	Next     string       `json:"next,omitempty"`
}
type SessionFilter struct {
	Days, Limit           int
	Model, Device, After  string
	Phase, Activity, Task string // only sessions whose first request carries this label
}
type sessionCursor struct {
	Start int64
	ID    string
}

// Sessions rechecks current authority before reading recorded attribution.
func (s *Store) Sessions(ctx context.Context, p Principal, project int64, f SessionFilter) (page SessionPage, err error) {
	lim := s.rate(p.UserID, false)
	err = s.read(ctx, func(tx *sql.Tx) error {
		if err := checkPrincipal(tx, p); err != nil {
			return err
		}
		if isSegment(project) {
			return ErrNotFound
		}
		a, err := loadAccess(tx, p.UserID, project)
		if err != nil {
			return err
		}
		if a.Role != Reader && a.Role != Developer && a.Role != Admin {
			return ErrNotFound
		}
		if lim != nil {
			return lim
		}
		user := p.UserID
		if a.Role == Admin {
			user = 0
		}
		page, err = s.sessionRows(tx, project, user, f)
		return err
	})
	return
}

// AdminSessions is called only through the authenticated admin channel.
func (s *Store) AdminSessions(ctx context.Context, project int64, f SessionFilter) (page SessionPage, err error) {
	err = s.read(ctx, func(tx *sql.Tx) error { page, err = s.sessionRows(tx, project, 0, f); return err })
	return
}

type sessionEvent struct {
	id, parent, device, model, outcome, name, source string
	labels                                           [6]string
	user, start                                      int64
	end, in, out, read, write, cost                  sql.NullInt64
}

func (s *Store) sessionRows(tx *sql.Tx, project, user int64, f SessionFilter) (SessionPage, error) {
	page := SessionPage{Sessions: []SessionRow{}}
	if project <= 0 {
		return page, ErrInvalid{"project"}
	}
	if f.Days < 1 || f.Days > 400 {
		return page, ErrInvalid{"days"}
	}
	if f.Limit == 0 {
		f.Limit = 100
	}
	if f.Limit < 1 || f.Limit > 500 {
		return page, ErrInvalid{"limit"}
	}
	impossible := false
	if f.Phase, f.Activity, f.Task, impossible = normalizeLabelFilters(f.Phase, f.Activity, f.Task); impossible {
		return page, nil // a label that cannot exist matches nothing
	}
	var cursor sessionCursor
	if f.After != "" {
		b, e := base64.RawURLEncoding.DecodeString(f.After)
		if e != nil || json.Unmarshal(b, &cursor) != nil || cursor.ID == "" {
			return page, ErrInvalid{"after"}
		}
	}
	now := s.now().UTC()
	from := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -(f.Days - 1)).UnixMilli()
	// Edges are resolved only within authorized rows and frozen attribution. Read the window (one day more, so a chain
	// that began just before it is joined) and the ancestors of its rows: a chain can be older than that, and its root
	// still decides whether the session is shown.
	q := `WITH RECURSIVE scoped(id) AS (
  SELECT id FROM usage_events WHERE project_id=?`
	args := []any{project}
	if user != 0 {
		q += ` AND user_id=?`
		args = append(args, user)
	}
	scope := ` WHERE p.project_id=?`
	if user != 0 {
		scope += ` AND p.user_id=?`
	}
	q += ` AND start>=?
  UNION
  SELECT p.id FROM scoped s JOIN usage_events c ON c.id=s.id JOIN usage_events p ON p.id=c.parent_id` + scope + `)
SELECT e.id,COALESCE(e.parent_id,''),e.device_id,e.model,e.outcome,COALESCE(u.name,''),e.cost_source,COALESCE(e.user_id,0),e.start,e.end,e.tokens_in,e.tokens_out,e.cache_read_tokens,e.cache_write_tokens,e.cost_usd_nanos,e.phase,e.activity,e.task,e.phase_source,e.activity_source,e.task_source FROM usage_events e JOIN scoped s ON s.id=e.id LEFT JOIN users u ON u.id=e.user_id`
	args = append(args, from-86400000, project)
	if user != 0 {
		args = append(args, user)
	}
	q += ` LIMIT 100001`
	rows, err := tx.Query(q, args...)
	if err != nil {
		return page, err
	}
	events := map[string]*sessionEvent{}
	for rows.Next() {
		e := new(sessionEvent)
		if err = rows.Scan(&e.id, &e.parent, &e.device, &e.model, &e.outcome, &e.name, &e.source, &e.user, &e.start, &e.end, &e.in, &e.out, &e.read, &e.write, &e.cost, &e.labels[0], &e.labels[1], &e.labels[2], &e.labels[3], &e.labels[4], &e.labels[5]); err != nil {
			rows.Close()
			return page, err
		}
		events[e.id] = e
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return page, err
	}
	if len(events) > 100000 {
		return page, fmt.Errorf("session traversal limit exceeded")
	}
	parents := map[string]string{}
	for id, e := range events {
		if p := events[e.parent]; p != nil && p.user == e.user && p.device == e.device {
			parents[id] = p.id
		}
	}
	roots := map[string]string{}
	for id := range events {
		path := []string{}
		seen := map[string]bool{}
		cur := id
		for roots[cur] == "" {
			if seen[cur] {
				return page, fmt.Errorf("cyclic session history")
			}
			if len(path) >= 10000 {
				return page, fmt.Errorf("session depth limit exceeded")
			}
			seen[cur] = true
			path = append(path, cur)
			if parents[cur] == "" {
				roots[cur] = cur
				break
			}
			cur = parents[cur]
		}
		root := roots[cur]
		for _, v := range path {
			roots[v] = root
		}
	}
	groups := map[string][]*sessionEvent{}
	for id, e := range events {
		groups[roots[id]] = append(groups[roots[id]], e)
	}
	for id, chain := range groups {
		root := events[id]
		if root.start < from || f.Device != "" && root.device != f.Device {
			continue
		}
		if f.Phase != "" && root.labels[0] != f.Phase || f.Activity != "" && root.labels[1] != f.Activity || f.Task != "" && root.labels[2] != f.Task {
			continue
		}
		if f.After != "" && (root.start > cursor.Start || root.start == cursor.Start && id >= cursor.ID) {
			continue
		}
		r := SessionRow{ID: id, ProjectID: project, UserID: root.user, UserName: root.name, DeviceID: root.device, DeviceName: root.device, Start: fromMs(root.start), Outcome: "ok", Incomplete: root.parent != "", Models: []string{}, Phase: root.labels[0], Activity: root.labels[1], Task: root.labels[2], PhaseSource: root.labels[3], ActivitySource: root.labels[4], TaskSource: root.labels[5]}
		open, match, interrupted := false, f.Model == "", false
		models := map[string]bool{}
		var cost, reported, estimated SessionMetric
		var latest int64
		for _, e := range chain {
			if e.outcome == OutcomeOpen {
				open = true
			}
			if e.model == f.Model {
				match = true
			}
			models[e.model] = true
			r.Requests++
			for _, v := range []struct {
				m *SessionMetric
				n sql.NullInt64
			}{{&r.TokensIn, e.in}, {&r.TokensOut, e.out}, {&r.CacheRead, e.read}, {&r.CacheWrite, e.write}, {&cost, e.cost}} {
				if err := v.m.add(v.n); err != nil {
					return page, err
				}
			}
			if e.cost.Valid {
				if e.source == "reported" {
					err = reported.add(e.cost)
				} else if e.source == "estimated" {
					err = estimated.add(e.cost)
				}
				if err != nil {
					return page, err
				}
			} else {
				r.UnknownCost++
			}
			if e.end.Valid && e.end.Int64 > latest {
				latest = e.end.Int64
			}
			if !e.end.Valid || e.outcome == OutcomeInterrupted {
				interrupted = true
			}
			if e.outcome == OutcomeError || e.outcome == OutcomeInterrupted {
				r.Outcome = "failed"
			} else if e.outcome == OutcomeCancelled && r.Outcome != "failed" {
				r.Outcome = "cancelled"
			}
		}
		if open || !match {
			continue
		}
		for m := range models {
			r.Models = append(r.Models, m)
		}
		sort.Strings(r.Models)
		if latest != 0 {
			t := fromMs(latest)
			r.End = &t
		}
		if !interrupted && !r.Incomplete && latest >= root.start {
			n := latest - root.start
			r.DurationMs = &n
		}
		if r.Incomplete {
			r.Outcome = "incomplete"
		}
		r.Cost = sessionDollars(cost.Value)
		r.ReportedCost = sessionDollars(reported.Value)
		r.EstimatedCost = sessionDollars(estimated.Value)
		page.Sessions = append(page.Sessions, r)
	}
	sort.Slice(page.Sessions, func(i, j int) bool {
		a, b := page.Sessions[i], page.Sessions[j]
		if a.Start.Equal(b.Start) {
			return a.ID > b.ID
		}
		return a.Start.After(b.Start)
	})
	if len(page.Sessions) > f.Limit {
		page.Sessions = page.Sessions[:f.Limit]
		last := page.Sessions[len(page.Sessions)-1]
		b, _ := json.Marshal(sessionCursor{last.Start.UnixMilli(), last.ID})
		page.Next = base64.RawURLEncoding.EncodeToString(b)
	}
	return page, nil
}
func sessionDollars(n *int64) *string {
	if n == nil {
		return nil
	}
	v := fmt.Sprintf("%d.%09d", *n/1e9, *n%1e9)
	return &v
}
