package core

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---- C4: the automatic pull. The clock is the fixture's (f.clock); nothing here sleeps.

func pullAudits(f *fx, action string) (n int) {
	f.t.Helper()
	f.ok(f.read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT count(*) FROM audit_events WHERE action=?`, action).Scan(&n)
	}))
	return
}

func autoPullFx(t *testing.T) (*fx, int64, *hub) {
	f, fk := readFx(t)
	f.Store.jitter = func() float64 { return 0.5 } // no jitter: the back-off is exact
	h := &hub{notes: []hubNote{structNote(1, "Orbit plan"), structNote(2, "Orbit two")}}
	return f, fk, h
}

func (f *fx) autoPull(up UpstreamTransport) AutoRun {
	f.t.Helper()
	r, e := f.AutoPull(ctx, up, noSleep)
	f.ok(e)
	return r
}

// The timer pulls a link with reading on when it is due, every pull_every, and a change on the hub arrives with no command typed.
func TestAutoPullRunsEveryIntervalAndPicksUpHubChanges(t *testing.T) {
	f, fk, h := autoPullFx(t)
	up := h.up()
	r := f.autoPull(up)
	if !r.Ran || r.Pull == nil || r.Pull.Added != 2 || len(mirror(f)) != 2 {
		t.Fatalf("first run: %+v", r)
	}
	l := linkOf(f, fk)
	if l.PullEvery != DefaultPullEvery || l.PullNext == nil || !l.PullNext.Equal(f.clock.Add(15*time.Minute)) || l.PullFailCount != 0 || l.PullTimerOff {
		t.Fatalf("after a pull: %+v", l)
	}
	if r.Next != 15*time.Minute {
		t.Fatalf("next %v", r.Next)
	}
	h.notes[0].rev, h.notes[0].body = 2, "changed on the hub"
	h.notes = h.notes[:1] // and the second note is gone there
	f.clock = f.clock.Add(15*time.Minute - time.Second)
	if r = f.autoPull(up); r.Ran {
		t.Fatal("pulled before the interval")
	}
	f.clock = f.clock.Add(time.Second)
	if r = f.autoPull(up); !r.Ran || r.Pull.Updated != 1 || r.Pull.Removed != 1 {
		t.Fatalf("second run: %+v", r.Pull)
	}
	if got := strings.Join(mirror(f), ","); got != "1/2/structure/finding/Orbit plan" {
		t.Fatal(got)
	}
}

// The interval is the owner's, 5 minutes to 24 hours; a shorter one brings the next pull forward, a longer one leaves it.
func TestAutoPullIntervalSettingIsBoundedAndApplies(t *testing.T) {
	f, fk, h := autoPullFx(t)
	f.autoPull(h.up())
	set := func(minutes int) error {
		sec := minutes * 60
		return f.LinkUpstreamWith(ctx, LinkSpec{Project: fk, Upstream: "hub", Remote: "shop", PullEvery: &sec, ReadOnly: true})
	}
	for _, bad := range []int{0, 4, 24*60 + 1, -5} {
		if err := set(bad); err == nil {
			t.Fatalf("%d minutes accepted", bad)
		}
	}
	f.ok(set(5))
	if l := linkOf(f, fk); l.PullEvery != 300 || !l.PullNext.Equal(f.clock.Add(5*time.Minute)) {
		t.Fatalf("shorter: %+v", l)
	}
	f.ok(set(24 * 60))
	l := linkOf(f, fk)
	if l.PullEvery != 86400 || !l.PullNext.Equal(f.clock.Add(5*time.Minute)) { // the pending one stays; the next after it is a day later
		t.Fatalf("longer: %+v", l)
	}
	f.clock = f.clock.Add(5 * time.Minute)
	f.autoPull(h.up())
	if l := linkOf(f, fk); !l.PullNext.Equal(f.clock.Add(24 * time.Hour)) {
		t.Fatalf("after a run with the long interval: %+v", l)
	}
}

// A run that ends with "more" goes on at the next tick with its cursor; a run that made no progress counts as a failure.
func TestAutoPullMoreContinuesAndNoProgressIsAFailure(t *testing.T) {
	old := maxPullFetch
	t.Cleanup(func() { maxPullFetch = old })
	f, fk, h := autoPullFx(t)
	for i := int64(3); i <= 5; i++ {
		h.notes = append(h.notes, structNote(i, "Orbit more"))
	}
	maxPullFetch = 2
	up := h.up()
	r := f.autoPull(up)
	if !r.Ran || !r.Pull.More || r.Pull.Added != 2 || r.Pull.Next != 2 {
		t.Fatalf("%+v", r.Pull)
	}
	if l := linkOf(f, fk); l.PullFailCount != 0 || !l.PullNext.Equal(f.clock.Add(time.Second)) {
		t.Fatalf("more with progress is soon, not a failure: %+v", l)
	}
	if r = f.autoPull(up); r.Ran {
		t.Fatal("the continuation came before its second")
	}
	f.clock = f.clock.Add(time.Second)
	if r = f.autoPull(up); !r.Pull.More || r.Pull.Added != 2 {
		t.Fatalf("%+v", r.Pull)
	}
	f.clock = f.clock.Add(time.Second)
	if r = f.autoPull(up); r.Pull.More || r.Pull.Added != 1 || len(mirror(f)) != 5 {
		t.Fatalf("%+v", r.Pull)
	}
	if l := linkOf(f, fk); l.PullFailCount != 0 || !l.PullNext.Equal(f.clock.Add(15*time.Minute)) {
		t.Fatalf("complete: %+v", l)
	}
	// no progress: nothing is fetched although there is something to fetch
	h.notes[0].rev = 2
	maxPullFetch = 0
	f.clock = f.clock.Add(15 * time.Minute)
	r = f.autoPull(up)
	if !r.Ran || !r.Pull.More || r.Pull.Stopped == "" {
		t.Fatalf("%+v", r.Pull)
	}
	if l := linkOf(f, fk); l.PullFailCount != 1 || !l.PullNext.Equal(f.clock.Add(time.Minute)) {
		t.Fatalf("a run without progress must back off: %+v", l)
	}
}

// Failures back off from a minute, doubling up to an hour; jitter is the store's (fixed here); a success clears it.
func TestAutoPullBacksOffAndRecovers(t *testing.T) {
	f, fk, h := autoPullFx(t)
	down := true
	h.manifest = func() (int, string) {
		if down {
			return 500, `{"error":{"message":"down"}}`
		}
		return 200, h.manifestJSON()
	}
	up := h.up()
	want := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 32 * time.Minute, time.Hour, time.Hour}
	for i, w := range want {
		r := f.autoPull(up)
		if !r.Ran {
			t.Fatalf("attempt %d did not run", i+1)
		}
		l := linkOf(f, fk)
		if l.PullFailCount != i+1 || !l.PullNext.Equal(f.clock.Add(w)) || l.PullError == "" {
			t.Fatalf("attempt %d: %+v", i+1, l)
		}
		calls := len(up.calls)
		f.clock = f.clock.Add(w - time.Second)
		if f.autoPull(up).Ran || len(up.calls) != calls {
			t.Fatalf("attempt %d: retried before its back-off", i+1)
		}
		f.clock = f.clock.Add(time.Second)
	}
	down = false
	if r := f.autoPull(up); !r.Ran || r.Pull.Added != 2 {
		t.Fatalf("%+v", r.Pull)
	}
	if l := linkOf(f, fk); l.PullFailCount != 0 || l.PullError != "" || l.Stale {
		t.Fatalf("%+v", l)
	}
}

// 401, 403 and a gone project purge at once (as C2) and switch the timer off for that link; the error stays visible; only the owner's
// new acceptance (or a pull by hand that goes through) switches it on again.
func TestAutoPullRefusalPurgesAndSwitchesTheTimerOff(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(h *hub)
	}{
		{"manifest 403", func(h *hub) { h.manifest = func() (int, string) { return 403, `{"error":{"message":"not bound"}}` } }},
		{"manifest 401", func(h *hub) { h.manifest = func() (int, string) { return 401, `{"error":{"message":"token"}}` } }},
		{"project gone", func(h *hub) { h.proj = 404 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, fk, h := autoPullFx(t)
			up := h.up()
			f.autoPull(up)
			if len(mirror(f)) != 2 {
				t.Fatal(mirror(f))
			}
			f.clock = f.clock.Add(15 * time.Minute)
			tc.set(h)
			r := f.autoPull(up)
			if !r.Ran || !r.Pull.Purged || len(mirror(f)) != 0 || postings(f) != 0 {
				t.Fatalf("%+v %v", r.Pull, mirror(f))
			}
			l := linkOf(f, fk)
			if !l.PullTimerOff || l.PullNext != nil || l.PullError == "" || !l.Read {
				t.Fatalf("%+v", l)
			}
			calls := len(up.calls)
			for i := 0; i < 3; i++ {
				f.clock = f.clock.Add(48 * time.Hour)
				if f.autoPull(up).Ran {
					t.Fatal("the timer went on after the upstream withdrew the right")
				}
			}
			if len(up.calls) != calls {
				t.Fatal("the upstream was contacted again")
			}
			if n := pullAudits(f, "upstream.mirror-purge"); n != 1 {
				t.Fatal("purge audit rows:", n)
			}
			// the owner accepts the reach again: the timer is on and the next run fills the mirror
			h.manifest, h.proj = nil, 0
			readOn(f, fk, "", "")
			if l := linkOf(f, fk); l.PullTimerOff {
				t.Fatal("still off after a new acceptance")
			}
			if r := f.autoPull(up); !r.Ran || r.Pull.Added != 2 {
				t.Fatalf("%+v", r.Pull)
			}
		})
	}
}

// An item of the plan: "turns the timer off with a visible error" holds for a pull by hand too, and a pull by hand that goes through
// switches it on again.
func TestManualPullThatGoesThroughSwitchesTheTimerOn(t *testing.T) {
	f, fk, h := autoPullFx(t)
	up := h.up()
	h.proj = 404
	f.autoPull(up)
	if !linkOf(f, fk).PullTimerOff {
		t.Fatal("not off")
	}
	h.proj = 0
	pull(f, fk, up)
	if l := linkOf(f, fk); l.PullTimerOff || l.PullFailCount != 0 || l.PullNext == nil {
		t.Fatalf("%+v", l)
	}
}

// Audit rows by the push's rule: one for a run that did something, otherwise one per change of outcome; never a title or a body.
func TestAutoPullAuditRows(t *testing.T) {
	f, fk, h := autoPullFx(t)
	up := h.up()
	f.autoPull(up) // adds 2
	if n := pullAudits(f, "upstream.auto-pull"); n != 1 {
		t.Fatal("rows after the first run:", n)
	}
	f.clock = f.clock.Add(15 * time.Minute)
	f.autoPull(up) // nothing changed: same outcome, no row
	f.clock = f.clock.Add(15 * time.Minute)
	f.autoPull(up)
	if n := pullAudits(f, "upstream.auto-pull"); n != 1 {
		t.Fatal("rows for runs that did nothing:", n)
	}
	h.notes[0].rev = 2
	f.clock = f.clock.Add(15 * time.Minute)
	f.autoPull(up) // updated 1
	if n := pullAudits(f, "upstream.auto-pull"); n != 2 {
		t.Fatal("rows after a change:", n)
	}
	// a failure that goes on: a row for each change of the count (capped), not one per second of waiting
	h.manifest = func() (int, string) { return 500, `{"error":{"message":"down"}}` }
	for i := 0; i < 8; i++ {
		f.clock = f.clock.Add(2 * time.Hour)
		f.autoPull(up)
	}
	n := pullAudits(f, "upstream.auto-pull")
	if n != 2+5 { // fail/1 .. fail/5, then fail/5 repeated: no more rows
		t.Fatal("rows for a failing upstream:", n)
	}
	var detail string
	f.ok(f.read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT detail FROM audit_events WHERE action='upstream.auto-pull' ORDER BY id DESC LIMIT 1`).Scan(&detail)
	}))
	if strings.Contains(detail, "Orbit") || strings.Contains(detail, "body of") {
		t.Fatal("the audit row names a note:", detail)
	}
	// a pull by hand keeps its own action
	h.manifest = nil
	f.clock = f.clock.Add(2 * time.Hour)
	pull(f, fk, up)
	if pullAudits(f, "upstream.pull") != 1 {
		t.Fatal("a manual pull must audit as before")
	}
}

// A timer that is cancelled (shutdown) pulls nothing and contacts nothing.
func TestAutoStepDoesNothingWhenCancelled(t *testing.T) {
	f, _, h := autoPullFx(t)
	up := h.up()
	c, cancel := context.WithCancel(ctx)
	cancel()
	r, e := f.AutoStep(c, up, noSleep)
	f.ok(e)
	if r.Ran || len(up.calls) != 0 {
		t.Fatalf("%+v %d", r, len(up.calls))
	}
}

// Nothing reads unless the owner turned reading on and accepted the reach: no link, a link with reading off and a link whose timer is off
// are never contacted.
func TestAutoPullOnlyReadsAcceptedLinks(t *testing.T) {
	f, fk := pullFx(t) // linked, reading off
	up := (&hub{notes: []hubNote{structNote(1, "Orbit")}}).up()
	if r := f.autoPull(up); r.Ran || r.Next != 0 || len(up.calls) != 0 {
		t.Fatalf("%+v", r)
	}
	readOn(f, fk, "", "")
	no := false
	f.ok(f.LinkUpstreamWith(ctx, LinkSpec{Project: fk, Upstream: "hub", Remote: "shop", Read: &no, ReadOnly: true}))
	if r := f.autoPull(up); r.Ran || len(up.calls) != 0 {
		t.Fatal("a link with reading turned off was pulled")
	}
}

// A link of an archived project is not read by the timer (as a push sends nothing for one), and reads again when it is unarchived.
func TestAutoPullSkipsArchivedProjects(t *testing.T) {
	f, fk, h := autoPullFx(t)
	up := h.up()
	f.ok(f.SetProjectArchived(ctx, fk, true))
	if r := f.autoPull(up); r.Ran || r.Next != 0 || len(up.calls) != 0 {
		t.Fatalf("%+v", r)
	}
	f.ok(f.SetProjectArchived(ctx, fk, false))
	if r := f.autoPull(up); !r.Ran || r.Pull.Added != 2 {
		t.Fatalf("%+v", r.Pull)
	}
}

// Push and pull of one link never run at the same time, and the timer alternates them so neither starves the other.
func TestAutoStepAlternatesPushAndPull(t *testing.T) {
	f, notes := upFx(t)
	_ = notes
	f.Store.jitter = func() float64 { return 0.5 }
	fk := f.pid["fakture"]
	yes := true
	f.ok(f.LinkUpstreamWith(ctx, LinkSpec{Project: fk, Upstream: "hub", Remote: "shop", Auto: &yes}))
	readOn(f, fk, "", "")
	h := &hub{notes: []hubNote{structNote(1, "Orbit")}}
	hubUp := h.up()
	push := okUp()
	both := &fakeUp{handler: func(c upCall) (int, string, time.Duration) {
		if c.method == "GET" {
			return hubUp.handler(c)
		}
		return push.handler(c)
	}}
	dirty := func() {
		f.clock = f.clock.Add(2 * time.Minute)
		f.ok(f.write(ctx, func(tx *sql.Tx) error { return f.markUpstreamDirty(tx, fk) }))
		f.clock = f.clock.Add(2 * time.Minute)
	}
	var seq []string
	for i := 0; i < 4; i++ {
		dirty()
		// the pull is due every second: make it so
		f.ok(f.write(ctx, func(tx *sql.Tx) error { _, e := tx.Exec(`UPDATE upstream_links SET pull_next=0`); return e }))
		r, e := f.AutoStep(ctx, both, noSleep)
		f.ok(e)
		switch {
		case r.Push != nil:
			seq = append(seq, "push")
		case r.Pull != nil:
			seq = append(seq, "pull")
		default:
			seq = append(seq, "none")
		}
	}
	if got := strings.Join(seq, " "); got != "push pull push pull" {
		t.Fatal(got)
	}
}

// While a pull of a link runs, a push of it, a second pull by the timer and a pull by hand are refused (not queued behind it), and the link
// is free again after it: they never run over each other.
func TestPullHoldsTheLinkLockAgainstPushAndPull(t *testing.T) {
	f, fk, h := autoPullFx(t)
	yes := true
	f.ok(f.LinkUpstreamWith(ctx, LinkSpec{Project: fk, Upstream: "hub", Remote: "shop", Auto: &yes}))
	in, release := make(chan struct{}), make(chan struct{})
	up := h.up()
	inner := up.handler
	first := true
	blocked := &fakeUp{handler: func(c upCall) (int, string, time.Duration) {
		if first && strings.HasSuffix(c.path, "/kb/manifest") {
			first = false
			close(in)
			<-release
		}
		return inner(c)
	}}
	done := make(chan AutoRun, 1)
	go func() { r, _ := f.AutoPull(ctx, blocked, noSleep); done <- r }()
	<-in
	if r := f.autoPull(blocked); r.Ran {
		t.Fatal("two pulls of one link at once")
	}
	var busy errPushRunning
	if _, err := f.PullUpstream(ctx, blocked, fk, 0, "", false, noSleep); !errors.As(err, &busy) {
		t.Fatal("a pull by hand ran over the timer's")
	}
	f.f2dirty(fk)
	if r, e := f.AutoPush(ctx, okUp(), noSleep); e != nil || r.Ran {
		t.Fatalf("a push ran while the link was being pulled: %+v %v", r, e)
	}
	close(release)
	if r := <-done; !r.Ran || r.Pull.Added != 2 {
		t.Fatalf("%+v", r.Pull)
	}
	f.clock = f.clock.Add(15 * time.Minute)
	if r := f.autoPull(up); !r.Ran {
		t.Fatal("the lock was not released")
	}
}

func (f *fx) f2dirty(fk int64) {
	f.t.Helper()
	f.clock = f.clock.Add(2 * time.Minute)
	f.ok(f.write(ctx, func(tx *sql.Tx) error { return f.markUpstreamDirty(tx, fk) }))
	f.clock = f.clock.Add(2 * time.Minute)
}

// One automatic pull holds the link for a slice, not for its whole five minutes: the deadline of every request it makes is within
// autoPullSlice, so a push that finds the link busy gets it soon.
func TestAutoPullIsBoundedToASlice(t *testing.T) {
	old := autoPullSlice
	autoPullSlice = 7 * time.Second
	t.Cleanup(func() { autoPullSlice = old })
	f, _, h := autoPullFx(t)
	var deadlines []time.Duration
	tr := ctxUp{inner: h.up(), saw: func(c context.Context) {
		if dl, ok := c.Deadline(); ok {
			deadlines = append(deadlines, time.Until(dl))
		} else {
			deadlines = append(deadlines, -1)
		}
	}}
	r := f.autoPull(tr)
	if !r.Ran || len(deadlines) == 0 {
		t.Fatalf("%+v", r)
	}
	for _, d := range deadlines {
		if d <= 0 || d > 7*time.Second {
			t.Fatalf("a request with %v left: the run is not bounded to the slice (%v)", d, deadlines)
		}
	}
	// a manual pull keeps its own, longer budget
	deadlines = nil
	f.clock = f.clock.Add(15 * time.Minute)
	f.ok(func() error { _, e := f.PullUpstream(ctx, tr, f.pid["fakture"], 0, "", false, noSleep); return e }())
	longest := time.Duration(0)
	for _, d := range deadlines {
		longest = max(longest, d)
	}
	if longest <= 7*time.Second {
		t.Fatal("a pull by hand must not be cut to the timer's slice:", deadlines)
	}
}

type ctxUp struct {
	inner *fakeUp
	saw   func(context.Context)
}

func (u ctxUp) Do(c context.Context, up, method, path string, h map[string]string, body []byte) (int, []byte, time.Duration, error) {
	u.saw(c)
	return u.inner.Do(c, up, method, path, h, body)
}

// Staleness has a cure: with the timer on a mirror stays fresh for as long as the upstream answers; without it, it goes stale after 7 days.
func TestAutoPullKeepsTheMirrorFresh(t *testing.T) {
	f, fk, h := autoPullFx(t)
	up := h.up()
	f.autoPull(up)
	sec := int(MaxPullEvery)
	f.ok(f.LinkUpstreamWith(ctx, LinkSpec{Project: fk, Upstream: "hub", Remote: "shop", PullEvery: &sec, ReadOnly: true}))
	for day := 1; day <= 9; day++ {
		f.clock = f.clock.Add(24 * time.Hour)
		if r := f.autoPull(up); !r.Ran {
			t.Fatalf("day %d: not pulled", day)
		}
		l := linkOf(f, fk)
		if l.Stale || l.LastPullAge != 0 || len(f.search("ana", fk, "orbit")) == 0 {
			t.Fatalf("day %d: %+v", day, l)
		}
	}
	f.clock = f.clock.Add(8 * 24 * time.Hour) // the timer is off (nobody asks): 8 days later it is stale and hidden
	if l := linkOf(f, fk); !l.Stale || len(f.search("ana", fk, "orbit")) != 0 {
		t.Fatalf("%+v", l)
	}
	f.autoPull(up)
	if l := linkOf(f, fk); l.Stale {
		t.Fatal("a pull did not cure staleness")
	}
}

// Migration 25 on a database of schema 24 with a link that is read: snapshot first, the link keeps everything and is due at once.
func TestMigration24To25KeepsReadLinksDue(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, dbFile))
	if err != nil {
		t.Fatal(err)
	}
	tx, _ := db.Begin()
	for _, m := range migrations[:24] {
		if err = m(tx); err != nil {
			t.Fatal(err)
		}
	}
	setMeta(tx, "schema", "24")
	for _, q := range []string{`INSERT INTO upstreams(name,address,created) VALUES('hub','h:8765',1)`, `INSERT INTO projects(id,slug,name) VALUES(1,'shop','Shop')`,
		`INSERT INTO upstream_links(project_id,upstream,remote,remote_team,classes,with_private,read,read_accepted,read_reach,last_pull_ok) VALUES(1,'hub','shop',0,'structure',0,1,5,'hub|shop|0|structure|',9)`} {
		if _, err = tx.Exec(q); err != nil {
			t.Fatal(q, err)
		}
	}
	tx.Commit()
	db.Close()
	s := openT(t, dir)
	defer s.Close()
	if snaps, _ := filepath.Glob(filepath.Join(dir, "core-24-*.db")); len(snaps) != 1 {
		t.Fatal("no snapshot of schema 24", snaps)
	}
	if v, _ := schemaVersion(ctx, s.DB()); v != 25 || len(migrations) != 25 {
		t.Fatal("schema", v)
	}
	var every, fails, next int64
	var audit string
	var timer, read int
	if err = s.DB().QueryRow(`SELECT pull_every,pull_fail_count,pull_next,last_pull_audit,pull_timer,read FROM upstream_links`).Scan(&every, &fails, &next, &audit, &timer, &read); err != nil ||
		every != 0 || fails != 0 || next != 0 || audit != "" || timer != 1 || read != 1 {
		t.Fatal(every, fails, next, audit, timer, read, err)
	}
	if ls, err := s.UpstreamLinks(ctx, 1, 0); err != nil || ls[0].PullEvery != DefaultPullEvery || ls[0].PullNext == nil {
		t.Fatalf("%+v %v", ls, err)
	}
	if err = checkSchemaObjects(ctx, s.DB(), len(migrations)); err != nil {
		t.Fatal(err)
	}
}
