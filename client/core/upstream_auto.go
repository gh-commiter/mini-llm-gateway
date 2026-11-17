package core

import (
	"context"
	"database/sql"
	"math/rand"
	"time"
)

// The automatic push (step B). An OUTBOX per link, not per note: when a note of a linked project or team is published, edited, imported
// or accepted, the link (if the owner set it to auto) gets `dirty_since` (the first change not sent yet) and `last_change`. Nothing else is
// queued: what to send is found by the same walk as a manual push (the receipts), so the outbox is two timestamps, bounded whatever the
// notes do. A pusher (the gateway's timer; on the Mac the app's) asks AutoPush for the next due link: auto, dirty, quiet for the debounce
// (or dirty for too long, so a stream of edits cannot postpone it for ever) and past its back-off. It contacts only an upstream the owner
// added and a link the owner set to auto: nothing else is ever called on the gateway's own.

// AutoTiming: how long a link must be quiet before it is pushed, at most how long a change waits, and the back-off after a failure
// (base, doubling, up to max, with jitter). The defaults are the plan's: 60 s, 10 min, 1 min to 1 h.
type AutoTiming struct {
	Debounce, MaxWait, BackoffBase, BackoffMax time.Duration
}

var DefaultAutoTiming = AutoTiming{Debounce: 60 * time.Second, MaxWait: 10 * time.Minute, BackoffBase: time.Minute, BackoffMax: time.Hour}

// SetAutoTiming changes the timing of this store (tests and the gateway's settings; zero fields keep the default).
func (s *Store) SetAutoTiming(t AutoTiming) {
	d := DefaultAutoTiming
	if t.Debounce > 0 {
		d.Debounce = t.Debounce
	}
	if t.MaxWait > 0 {
		d.MaxWait = t.MaxWait
	}
	if t.BackoffBase > 0 {
		d.BackoffBase = t.BackoffBase
	}
	if t.BackoffMax > 0 {
		d.BackoffMax = t.BackoffMax
	}
	s.autoMu.Lock()
	s.auto = d
	s.autoMu.Unlock()
}

func (s *Store) autoTiming() AutoTiming {
	s.autoMu.Lock()
	defer s.autoMu.Unlock()
	if s.auto.Debounce == 0 {
		return DefaultAutoTiming
	}
	return s.auto
}

// backoff: base * 2^(n-1), capped, with up to 20 % jitter either way (so gateways that failed together do not come back together).
func (t AutoTiming) backoff(failures int, jitter func() float64) time.Duration {
	d := t.BackoffBase
	for i := 1; i < failures && d < t.BackoffMax; i++ {
		d *= 2
	}
	if d > t.BackoffMax {
		d = t.BackoffMax
	}
	j := (jitter()*2 - 1) * 0.2
	return time.Duration(float64(d) * (1 + j))
}

// markUpstreamDirty is called in the transaction of every write of a published note (create, edit, import, accept): the links of that
// project (or team) that are set to auto get their outbox flag. It is the only effect a member's edit has on an upstream: a flag, and so
// at most one push per debounce window however many edits there are.
func (s *Store) markUpstreamDirty(tx *sql.Tx, project int64) error {
	_, err := tx.Exec(`UPDATE upstream_links SET dirty_since=CASE WHEN dirty_since=0 THEN ?1 ELSE dirty_since END, last_change=?1 WHERE project_id=?2 AND auto=1`, ms(s.now()), project)
	return err
}

// AutoRun is the result of one call of AutoPush.
type AutoRun struct {
	Ran  bool          `json:"ran"`
	Push *UpstreamPush `json:"push,omitempty"`
	// Next: how long until the next link can be due (0: none is dirty), for a timer that wants to sleep; at least a second.
	Next        time.Duration `json:"-"`
	NextSeconds int           `json:"next"`
}

// AutoPush pushes ONE due link (the oldest dirty one) and records the outcome: success clears the flag unless a change arrived meanwhile,
// a failure sets the back-off, "more" (a write limit, the time budget) goes on soon without counting as a failure. One audit row per run:
// the link and the counts, never a title or a body. ctx is the time budget of the call; a cancelled ctx (the gateway is shutting down)
// pushes nothing.
func (s *Store) AutoPush(ctx context.Context, tr UpstreamTransport, sleep func(time.Duration) bool) (AutoRun, error) {
	var run AutoRun
	if ctx.Err() != nil {
		return run, nil
	}
	t := s.autoTiming()
	now := ms(s.now())
	var project int64
	var upstream string
	var found bool
	err := s.read(ctx, func(tx *sql.Tx) error {
		switch e := tx.QueryRow(`SELECT project_id,upstream FROM upstream_links WHERE auto=1 AND dirty_since>0 AND next_attempt<=?1 AND (last_change<=?2 OR dirty_since<=?3) ORDER BY dirty_since LIMIT 1`,
			now, now-t.Debounce.Milliseconds(), now-t.MaxWait.Milliseconds()).Scan(&project, &upstream); e {
		case nil:
			found = true
		case sql.ErrNoRows:
		default:
			return e
		}
		return nil
	})
	if err != nil {
		return run, err
	}
	if !found {
		run.Next = s.autoNext(ctx, t, now)
		run.NextSeconds = int(run.Next / time.Second)
		return run, nil
	}
	links, err := s.UpstreamLinks(ctx, 0, 0)
	if err != nil {
		return run, err
	}
	for _, l := range links {
		if l.Project != project || l.Upstream != upstream {
			continue
		}
		res, err := s.pushLink(ctx, tr, l.UpstreamLink, l.Label, 0, false, sleep) // records the outcome (afterRun) too
		if err != nil {
			return run, err
		}
		run.Ran, run.Push = true, &res
		wctx := context.WithoutCancel(ctx)
		if e := s.write(wctx, func(tx *sql.Tx) error {
			return Audit(tx, "gateway", "upstream.auto-push", upstream, map[string]any{"project": l.Label, "created": res.Created, "replaced": res.Replaced, "skipped": res.Skipped, "refused": res.Refused, "more": res.More, "stopped": res.Stopped != ""})
		}); e != nil {
			return run, e
		}
		break
	}
	run.Next = s.autoNext(ctx, t, ms(s.now()))
	run.NextSeconds = int(run.Next / time.Second)
	return run, nil
}

// autoNext: how long until the soonest dirty auto link is due (never below a second; 0 = nothing is dirty).
func (s *Store) autoNext(ctx context.Context, t AutoTiming, now int64) time.Duration {
	var soonest sql.NullInt64
	s.read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT min(max(next_attempt, min(last_change+?1, dirty_since+?2))) FROM upstream_links WHERE auto=1 AND dirty_since>0`, t.Debounce.Milliseconds(), t.MaxWait.Milliseconds()).Scan(&soonest)
	})
	if !soonest.Valid {
		return 0
	}
	d := time.Duration(soonest.Int64-now) * time.Millisecond
	if d < time.Second {
		d = time.Second
	}
	return d
}

// afterRun records the outcome of a real run on the link: success (the outbox is cleared unless a change arrived while it ran), a failure
// (the back-off), or "more" (try again soon: not a failure).
func (s *Store) afterRun(ctx context.Context, l UpstreamLink, res UpstreamPush, started int64) error {
	t := s.autoTiming()
	now := ms(s.now())
	failed := res.Refused > 0 || (res.Stopped != "" && !res.More)
	return s.write(ctx, func(tx *sql.Tx) error {
		switch {
		case res.More:
			wait := time.Duration(res.Wait) * time.Second
			if wait < time.Second {
				wait = time.Second
			}
			_, e := tx.Exec(`UPDATE upstream_links SET next_attempt=? WHERE project_id=? AND upstream=?`, now+wait.Milliseconds(), l.Project, l.Upstream)
			return e
		case failed:
			var n int
			if e := tx.QueryRow(`SELECT fail_count FROM upstream_links WHERE project_id=? AND upstream=?`, l.Project, l.Upstream).Scan(&n); e != nil {
				return notFound(e)
			}
			n++
			_, e := tx.Exec(`UPDATE upstream_links SET fail_count=?, last_fail=?, next_attempt=? WHERE project_id=? AND upstream=?`, n, now, now+t.backoff(n, s.jitterFn()).Milliseconds(), l.Project, l.Upstream)
			return e
		}
		_, e := tx.Exec(`UPDATE upstream_links SET dirty_since=CASE WHEN last_change>? THEN dirty_since ELSE 0 END, last_ok=?, fail_count=0, next_attempt=0 WHERE project_id=? AND upstream=?`, started, now, l.Project, l.Upstream)
		return e
	})
}

func (s *Store) jitterFn() func() float64 {
	if s.jitter != nil {
		return s.jitter
	}
	return rand.Float64
}
