package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
	_, err := tx.Exec(`UPDATE upstream_links SET dirty_since=CASE WHEN dirty_since=0 THEN ?1 ELSE dirty_since END, last_change=max(?1,last_change+1) WHERE project_id=?2 AND auto=1`, ms(s.now()), project)
	return err
}

// AutoRun is the result of one call of AutoPush.
type AutoRun struct {
	Ran  bool          `json:"ran"`
	Push *UpstreamPush `json:"push,omitempty"`
	Pull *UpstreamPull `json:"pull,omitempty"`
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
	var cursor int64
	var found bool
	err := s.read(ctx, func(tx *sql.Tx) error {
		switch e := tx.QueryRow(`SELECT project_id,upstream,push_cursor FROM upstream_links WHERE auto=1 AND dirty_since>0 AND next_attempt<=?1 AND (last_change<=?2 OR dirty_since<=?3) ORDER BY dirty_since LIMIT 1`,
			now, now-t.Debounce.Milliseconds(), now-t.MaxWait.Milliseconds()).Scan(&project, &upstream, &cursor); e {
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
		res, err := s.pushLink(ctx, tr, l.UpstreamLink, l.Label, cursor, false, sleep, true) // records the outcome (afterRun) too
		var busy errPushRunning
		if errors.As(err, &busy) { // a manual push of this link is running: it records the outcome; the timer comes back
			run.Next = s.autoNext(ctx, t, ms(s.now()))
			run.NextSeconds = int(run.Next / time.Second)
			return run, nil
		}
		if err != nil {
			return run, err
		}
		run.Ran, run.Push = true, &res
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

// beginTraversal remembers the dirty generation before the first batch sends anything.
func (s *Store) beginTraversal(ctx context.Context, l UpstreamLink, after int64) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		if after == 0 {
			_, e := tx.Exec(`UPDATE upstream_links SET push_generation=last_change, push_failed=0, push_cursor=0 WHERE project_id=? AND upstream=?`, l.Project, l.Upstream)
			return e
		}
		// An unknown continuation cannot prove that the start of this traversal was clean.
		_, e := tx.Exec(`UPDATE upstream_links SET push_generation=0, push_failed=1 WHERE project_id=? AND upstream=? AND push_cursor<>?`, l.Project, l.Upstream, after)
		return e
	})
}

// afterRun records the outcome of a real run on the link: success (the outbox is cleared unless a change arrived while it ran), a failure
// (the back-off), or "more". A "more" that waits for the upstream's write limit is not a failure and comes back no sooner than the wait;
// a "more" that got somewhere (a note sent) goes on soon; a "more" that got nowhere (every note refused, a time budget used for nothing)
// is a failure, so the back-off applies. A "more" keeps the cursor (the last note dealt with) for the next run; any other end resets it.
func (s *Store) afterRun(ctx context.Context, l UpstreamLink, res UpstreamPush) error {
	t := s.autoTiming()
	now := ms(s.now())
	return s.write(ctx, func(tx *sql.Tx) error {
		var generation int64
		var priorFailed, n int
		if e := tx.QueryRow(`SELECT push_generation,push_failed,fail_count FROM upstream_links WHERE project_id=? AND upstream=?`, l.Project, l.Upstream).Scan(&generation, &priorFailed, &n); e != nil {
			return notFound(e)
		}
		traversalFailed := priorFailed != 0 || res.Refused > 0 || (res.Stopped != "" && !res.More)
		failed := traversalFailed
		var cursor int64
		if res.More {
			cursor = res.Next
			// Preserve the traversal failure, but continue a batch that made progress or waits for a write limit.
			failed = res.Wait == 0 && res.Created+res.Replaced == 0
		}
		savedFailure := 0
		if res.More && traversalFailed {
			savedFailure = 1
		}
		if !res.More {
			generation = 0
		}
		if res.More && !failed {
			wait := time.Duration(res.Wait) * time.Second
			if wait < time.Second {
				wait = time.Second
			}
			_, e := tx.Exec(`UPDATE upstream_links SET next_attempt=?,push_cursor=?,push_failed=? WHERE project_id=? AND upstream=?`, now+wait.Milliseconds(), cursor, savedFailure, l.Project, l.Upstream)
			return e
		}
		if failed {
			n++
			_, e := tx.Exec(`UPDATE upstream_links SET fail_count=?,last_fail=?,next_attempt=?,push_cursor=?,push_generation=?,push_failed=? WHERE project_id=? AND upstream=?`, n, now, now+t.backoff(n, s.jitterFn()).Milliseconds(), cursor, generation, savedFailure, l.Project, l.Upstream)
			return e
		}
		var waiting int
		if e := tx.QueryRow(waitingSQL, l.Project, l.Upstream).Scan(&waiting); e != nil {
			return e
		}
		if waiting > 0 { // notes wait for a review upstream: not a failure; the link stays due and looks again at a quiet pace (5 debounces, at least a second), no back-off
			poll := 5 * t.Debounce
			if poll < time.Second {
				poll = time.Second
			}
			_, e := tx.Exec(`UPDATE upstream_links SET last_ok=?,fail_count=0,next_attempt=CASE WHEN auto=1 THEN ? ELSE 0 END,push_cursor=0,push_generation=0,push_failed=0,dirty_since=CASE WHEN dirty_since=0 AND auto=1 THEN ? ELSE dirty_since END WHERE project_id=? AND upstream=?`, now, now+poll.Milliseconds(), now, l.Project, l.Upstream)
			return e
		}
		_, e := tx.Exec(`UPDATE upstream_links SET dirty_since=CASE WHEN last_change=push_generation THEN 0 ELSE dirty_since END,last_ok=?,fail_count=0,next_attempt=0,push_cursor=0,push_generation=0,push_failed=0 WHERE project_id=? AND upstream=?`, now, l.Project, l.Upstream)
		return e
	})
}

// auditAutoRun runs before the push lock is released. A send always writes a row, even if the link disappeared.
func (s *Store) auditAutoRun(ctx context.Context, l UpstreamLink, label string, res UpstreamPush) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		var fails int
		var last string
		e := tx.QueryRow(`SELECT fail_count,last_audit FROM upstream_links WHERE project_id=? AND upstream=?`, l.Project, l.Upstream).Scan(&fails, &last)
		if e != nil && e != sql.ErrNoRows {
			return e
		}
		sig := fmt.Sprintf("%v/%v/%v/%d", res.Created+res.Replaced > 0, res.Refused > 0, res.More, fails)
		if e == nil && sig == last && res.Created+res.Replaced == 0 {
			return nil
		}
		if _, e := tx.Exec(`UPDATE upstream_links SET last_audit=? WHERE project_id=? AND upstream=?`, sig, l.Project, l.Upstream); e != nil {
			return e
		}
		return Audit(tx, "gateway", "upstream.auto-push", l.Upstream, map[string]any{"project": label, "created": res.Created, "replaced": res.Replaced, "skipped": res.Skipped, "refused": res.Refused, "more": res.More, "stopped": res.Stopped != ""})
	})
}

func (s *Store) jitterFn() func() float64 {
	if s.jitter != nil {
		return s.jitter
	}
	return rand.Float64
}

// autoPullSlice: the longest one automatic pull holds the link's lock (a push of the same link that finds it busy comes back after a second:
// a pull never keeps it for its whole five minutes). A run that is not through ends with "more" and its cursor and goes on at the next
// tick. A variable so that a test can look at the deadline.
var autoPullSlice = 45 * time.Second

// pullDue: the links the timer reads (reading on, timer on), never one of an archived project or team (as a push sends nothing for one);
// its one argument is segmentBase.
const pullDue = `read=1 AND pull_timer=1 AND NOT EXISTS (SELECT 1 FROM projects p WHERE p.id=upstream_links.project_id AND p.archived=1) AND NOT EXISTS (SELECT 1 FROM teams t WHERE t.id=upstream_links.project_id-?1 AND t.archived=1)`

// AutoPull pulls ONE due link: reading on, the timer on, next attempt reached (the one that waited longest). It is the same run as a pull
// by hand (the same bounds per run: note GETs, time, the upstream's read limit), under the same lock as a push; it only ever contacts an
// upstream the owner added and a link whose reach the owner accepted. A cancelled ctx (the gateway is shutting down) pulls nothing.
func (s *Store) AutoPull(ctx context.Context, tr UpstreamTransport, sleep func(time.Duration) bool) (AutoRun, error) {
	var run AutoRun
	if ctx.Err() != nil {
		return run, nil
	}
	now := ms(s.now())
	var project int64
	var upstream string
	var found bool
	err := s.read(ctx, func(tx *sql.Tx) error {
		switch e := tx.QueryRow(`SELECT project_id,upstream FROM upstream_links WHERE `+pullDue+` AND pull_next<=? ORDER BY pull_next,project_id,upstream LIMIT 1`, segmentBase, now).Scan(&project, &upstream); e {
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
	next := func() { run.Next = s.autoPullNext(ctx, ms(s.now())); run.NextSeconds = int(run.Next / time.Second) }
	if !found {
		next()
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
		sctx, cancel := context.WithTimeout(ctx, autoPullSlice)
		res, err := s.pullLink(sctx, tr, l.UpstreamLink, l.Label, sleep, true)
		cancel()
		var busy errPushRunning
		if errors.As(err, &busy) { // a push or a pull by hand of this link is running: it records its outcome; the timer comes back
			next()
			return run, nil
		}
		if err != nil {
			return run, err
		}
		run.Ran, run.Pull = true, &res
		break
	}
	next()
	return run, nil
}

// autoPullNext: how long until the soonest link with the timer on is due (never below a second; 0 = no link is read by the timer).
func (s *Store) autoPullNext(ctx context.Context, now int64) time.Duration {
	var soonest sql.NullInt64
	s.read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT min(pull_next) FROM upstream_links WHERE `+pullDue, segmentBase).Scan(&soonest)
	})
	if !soonest.Valid {
		return 0
	}
	return max(time.Duration(soonest.Int64-now)*time.Millisecond, time.Second)
}

// AutoStep is one step of the gateway's timer: ONE link, pushed or pulled. The two are tried in turn (after a push the pull is tried
// first, after a pull the push), so a link that is always due for one cannot keep the other from its turn. When nothing ran, Next is the
// shortest wait of the two kinds.
func (s *Store) AutoStep(ctx context.Context, tr UpstreamTransport, sleep func(time.Duration) bool) (AutoRun, error) {
	s.autoMu.Lock()
	steps := []func(context.Context, UpstreamTransport, func(time.Duration) bool) (AutoRun, error){s.AutoPush, s.AutoPull}
	if s.autoPushed {
		steps[0], steps[1] = steps[1], steps[0]
	}
	s.autoMu.Unlock()
	var out AutoRun
	for _, step := range steps {
		r, err := step(ctx, tr, sleep)
		if err != nil {
			return out, err
		}
		if r.Ran {
			s.autoMu.Lock()
			s.autoPushed = r.Push != nil
			s.autoMu.Unlock()
			return r, nil
		}
		out.Next = shorter(out.Next, r.Next)
	}
	out.NextSeconds = int(out.Next / time.Second)
	return out, nil
}

// shorter: the smaller of two waits, 0 meaning "none".
func shorter(a, b time.Duration) time.Duration {
	if a == 0 || b != 0 && b < a {
		return b
	}
	return a
}
