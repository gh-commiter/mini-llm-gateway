package core

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestAutoTraversalRetainsEditBelowCursor(t *testing.T) {
	old := maxPushNotes
	maxPushNotes = 1
	t.Cleanup(func() { maxPushNotes = old })
	f, fk, notes := autoFx(t)
	f.clock = f.clock.Add(61 * time.Second)
	up := okUp()
	r, e := f.AutoPush(ctx, up, noSleep)
	f.ok(e)
	if !r.Push.More {
		t.Fatal("expected a continuation")
	}
	reopenAutoFixture(f)
	f.edit("ana", fk, notes["Structure one"], "edited below cursor")
	f.clock = f.clock.Add(61 * time.Second)
	r, e = f.AutoPush(ctx, up, noSleep)
	f.ok(e)
	if r.Push.More || f.link(fk).DirtySince == nil {
		t.Fatal("continuation forgot the edit below its cursor")
	}
	f.clock = f.clock.Add(61 * time.Second)
	r, e = f.AutoPush(ctx, up, noSleep)
	f.ok(e)
	if !r.Ran || r.Push.Created+r.Push.Replaced != 1 {
		t.Fatalf("edit was not sent: %+v", r)
	}
}

func TestAutoTraversalRetainsEarlierRefusal(t *testing.T) {
	old := maxPushNotes
	maxPushNotes = 2
	t.Cleanup(func() { maxPushNotes = old })
	f, fk, _ := autoFx(t)
	keySeq++
	_, _, e := f.CreateNote(ctx, f.p("ana"), fk, fmt.Sprint("uk", keySeq), NoteInput{Class: Structure, Title: "Last batch", Body: "body"})
	f.ok(e)
	f.clock = f.clock.Add(61 * time.Second)
	up := okUp()
	good := up.handler
	up.handler = func(c upCall) (int, string, time.Duration) {
		if c.method == "POST" && c.body["title"] == "Structure one" {
			return 400, `{"error":{"message":"no"}}`, 0
		}
		return good(c)
	}
	r, e := f.AutoPush(ctx, up, noSleep)
	f.ok(e)
	if !r.Push.More || r.Push.Refused != 1 || r.Push.Created != 1 || f.link(fk).FailCount != 0 {
		t.Fatalf("%+v", r)
	}
	reopenAutoFixture(f)
	f.clock = f.clock.Add(time.Minute)
	r, e = f.AutoPush(ctx, up, noSleep)
	f.ok(e)
	l := f.link(fk)
	// a traversal that sent notes is a success for the link even though one note was refused in its first batch (no escalating back-off);
	// the refused note is not retried by the automatic push until something changes
	if r.Push.Refused != 0 || l.DirtySince != nil || l.FailCount != 0 || l.NextAttempt != nil || l.LastOK == nil {
		t.Fatalf("a refused note made the traversal a failure: %+v %+v", r, l)
	}
	count := up.count("POST")
	for i := 0; i < 5; i++ {
		r, e = f.AutoPush(ctx, up, noSleep)
		f.ok(e)
		if r.Ran {
			t.Fatal("retried although nothing changed")
		}
	}
	if up.count("POST") != count {
		t.Fatal("resent in a tight loop")
	}
}

func reopenAutoFixture(f *fx) {
	f.t.Helper()
	dir := f.Store.dir
	f.ok(f.Close())
	s, e := Open(dir, Options{})
	f.ok(e)
	f.Store = s
	f.Store.now = func() time.Time { return f.clock }
	f.Store.jitter = func() float64 { return 0.5 }
}

func TestAutoTraversalSeesChangeInSameMillisecond(t *testing.T) {
	f, fk, notes := autoFx(t)
	f.clock = f.clock.Add(61 * time.Second)
	// The generation and the edit have the same wall-clock timestamp.
	f.ok(f.write(ctx, func(tx *sql.Tx) error { return f.markUpstreamDirty(tx, fk) }))
	f.SetAutoTiming(AutoTiming{Debounce: time.Millisecond, MaxWait: time.Millisecond})
	f.clock = f.clock.Add(time.Millisecond)
	up := okUp()
	good := up.handler
	edited := false
	up.handler = func(c upCall) (int, string, time.Duration) {
		if c.method == "POST" && !edited {
			edited = true
			n := notes["Structure one"]
			f.clock = f.clock.Add(-time.Millisecond)
			_, e := f.UpdateNote(ctx, f.p("ana"), fk, n.ID, n.Revision, NoteInput{Class: n.Class, Title: n.Title, Body: "same millisecond edit"})
			f.ok(e)
			f.clock = f.clock.Add(time.Millisecond)
		}
		return good(c)
	}
	_, e := f.AutoPush(ctx, up, noSleep)
	f.ok(e)
	if f.link(fk).DirtySince == nil {
		t.Fatal("same millisecond edit was forgotten")
	}
}

func TestAutoAuditDoesNotRequireLinkRow(t *testing.T) {
	f, fk, _ := autoFx(t)
	l := f.link(fk)
	f.ok(f.UnlinkUpstream(ctx, fk, 0, "hub"))
	f.ok(f.auditAutoRun(ctx, l.UpstreamLink, l.Label, UpstreamPush{Created: 1}))
	if autoAudits(f) != 1 {
		t.Fatal("missing audit for a sent note")
	}
}

func TestTraversalMigrationRestartsOldCursor(t *testing.T) {
	saved := migrations
	migrations = saved[:22]
	t.Cleanup(func() { migrations = saved })
	f, fk, _ := autoFx(t)
	f.ok(f.write(ctx, func(tx *sql.Tx) error {
		_, e := tx.Exec(`UPDATE upstream_links SET push_cursor=1000,fail_count=2,next_attempt=123 WHERE project_id=?`, fk)
		return e
	}))
	migrations = saved
	reopenAutoFixture(f)
	var cursor, generation, failed int64
	f.ok(f.read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT push_cursor,push_generation,push_failed FROM upstream_links WHERE project_id=?`, fk).Scan(&cursor, &generation, &failed)
	}))
	if cursor != 0 || generation != 0 || failed != 0 || f.link(fk).DirtySince == nil || f.link(fk).FailCount != 2 {
		t.Fatalf("unsafe migrated traversal: %d %d %d %+v", cursor, generation, failed, f.link(fk))
	}
}

func TestDisablingAutoDiscardsTraversal(t *testing.T) {
	old := maxPushNotes
	maxPushNotes = 1
	t.Cleanup(func() { maxPushNotes = old })
	f, fk, notes := autoFx(t)
	f.clock = f.clock.Add(61 * time.Second)
	r, e := f.AutoPush(ctx, okUp(), noSleep)
	f.ok(e)
	if !r.Push.More {
		t.Fatal("expected continuation")
	}
	no, yes := false, true
	f.ok(f.LinkUpstreamWith(ctx, LinkSpec{Project: fk, Upstream: "hub", Remote: "shop", Auto: &no}))
	f.edit("ana", fk, notes["Structure one"], "edited while manual")
	f.ok(f.LinkUpstreamWith(ctx, LinkSpec{Project: fk, Upstream: "hub", Remote: "shop", Auto: &yes}))
	f.clock = f.clock.Add(61 * time.Second)
	up := okUp()
	r, e = f.AutoPush(ctx, up, noSleep)
	f.ok(e)
	var title any
	for _, c := range up.calls {
		if c.method == "POST" {
			title = c.body["title"]
			break
		}
	}
	if !r.Ran || title != "Structure one" {
		t.Fatal("auto resumed a discarded traversal")
	}
}

func TestUnknownManualContinuationKeepsOutbox(t *testing.T) {
	f, fk, notes := autoFx(t)
	_, e := f.PushUpstream(ctx, okUp(), fk, 0, "hub", notes["Structure one"].ID, false, noSleep)
	f.ok(e)
	l := f.link(fk)
	if l.DirtySince == nil || l.FailCount != 1 {
		t.Fatalf("partial traversal cleared the outbox: %+v", l)
	}
}

func TestUnlinkAndRemoveRefuseDuringAutoPushAndAuditSurvives(t *testing.T) {
	for _, remove := range []bool{false, true} {
		t.Run(fmt.Sprint(remove), func(t *testing.T) {
			f, fk, _ := autoFx(t)
			f.clock = f.clock.Add(61 * time.Second)
			inside, release := make(chan struct{}), make(chan struct{})
			up := okUp()
			good := up.handler
			first := true
			up.handler = func(c upCall) (int, string, time.Duration) {
				if c.method == "POST" && first {
					first = false
					close(inside)
					<-release
				}
				return good(c)
			}
			done := make(chan error, 1)
			go func() { _, e := f.AutoPush(ctx, up, noSleep); done <- e }()
			<-inside
			var e error
			if remove {
				e = f.RemoveUpstream(ctx, "hub")
			} else {
				e = f.UnlinkUpstream(ctx, fk, 0, "hub")
			}
			close(release)
			pushErr := <-done
			if e == nil || !strings.Contains(e.Error(), "is running") {
				t.Fatalf("removal during send was not refused: %v (push: %v)", e, pushErr)
			}
			f.ok(pushErr)
			if n := autoAudits(f); n != 1 {
				t.Fatalf("%d audits", n)
			}
			if remove {
				f.ok(f.RemoveUpstream(ctx, "hub"))
			} else {
				f.ok(f.UnlinkUpstream(ctx, fk, 0, "hub"))
			}
			if n := autoAudits(f); n != 1 {
				t.Fatalf("lost audit after removal: %d", n)
			}
		})
	}
}

func autoFx(t *testing.T) (*fx, int64, map[string]Note) {
	f, notes := upFx(t)
	f.Store.jitter = func() float64 { return 0.5 } // no jitter: the back-off is exact
	fk := f.pid["fakture"]
	yes := true
	f.ok(f.LinkUpstreamWith(ctx, LinkSpec{Project: fk, Upstream: "hub", Remote: "shop", Auto: &yes}))
	return f, fk, notes
}

func (f *fx) edit(user string, project int64, n Note, body string) Note {
	f.t.Helper()
	f.clock = f.clock.Add(time.Millisecond)
	cur, e := f.GetNote(ctx, f.p(user), project, n.ID)
	f.ok(e)
	out, e := f.UpdateNote(ctx, f.p(user), project, n.ID, cur.Revision, NoteInput{Class: cur.Class, Title: cur.Title, Body: body})
	f.ok(e)
	return out
}

func (f *fx) link(project int64) UpstreamLinkInfo {
	f.t.Helper()
	ls, e := f.UpstreamLinks(ctx, project, 0)
	f.ok(e)
	return ls[0]
}

func TestAutoPushWaitsForTheDebounceAndSendsOnce(t *testing.T) {
	f, fk, notes := autoFx(t)
	up := okUp()
	f.ok(f.write(ctx, func(tx *sql.Tx) error { return f.markUpstreamDirty(tx, fk) })) // as the setup notes did (the link was made after them)
	if r, e := f.AutoPush(ctx, up, noSleep); e != nil || r.Ran || r.Next == 0 {
		t.Fatalf("pushed at once: %+v %v", r, e)
	}
	f.clock = f.clock.Add(59 * time.Second)
	if r, _ := f.AutoPush(ctx, up, noSleep); r.Ran {
		t.Fatal("pushed before the debounce")
	}
	// a change restarts the quiet time
	f.edit("ana", fk, notes["Structure one"], "changed")
	f.clock = f.clock.Add(59 * time.Second)
	if r, _ := f.AutoPush(ctx, up, noSleep); r.Ran {
		t.Fatal("a change did not restart the quiet time")
	}
	f.clock = f.clock.Add(2 * time.Second)
	r, e := f.AutoPush(ctx, up, noSleep)
	f.ok(e)
	if !r.Ran || r.Push.Created != 2 || up.count("POST") != 2 {
		t.Fatalf("%+v %v", r, up.calls)
	}
	l := f.link(fk)
	if l.DirtySince != nil || l.LastOK == nil || l.FailCount != 0 || l.NextAttempt != nil {
		t.Fatalf("%+v", l)
	}
	if r, _ = f.AutoPush(ctx, up, noSleep); r.Ran || r.Next != 0 {
		t.Fatalf("a second push with nothing new: %+v", r)
	}
	// an audit row for the run: the link and the counts, never a title or a body
	var detail string
	f.ok(f.read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT detail FROM audit_events WHERE action='upstream.auto-push'`).Scan(&detail)
	}))
	if !strings.Contains(detail, `"created":2`) || strings.Contains(detail, "Structure") || strings.Contains(detail, "changed") {
		t.Fatal(detail)
	}
}

func TestAnEditStormIsOnePush(t *testing.T) {
	f, fk, notes := autoFx(t)
	up := okUp()
	f.ok(f.write(ctx, func(tx *sql.Tx) error { return f.markUpstreamDirty(tx, fk) }))
	n := notes["Structure one"]
	for i := 0; i < 200; i++ { // a member edits again and again within a minute
		n = f.edit("ana", fk, n, fmt.Sprint("version ", i))
		f.clock = f.clock.Add(100 * time.Millisecond)
		if r, _ := f.AutoPush(ctx, up, noSleep); r.Ran {
			t.Fatal("pushed in the middle of a storm")
		}
	}
	f.clock = f.clock.Add(61 * time.Second)
	runs := 0
	for i := 0; i < 5; i++ {
		if r, _ := f.AutoPush(ctx, up, noSleep); r.Ran {
			runs++
		}
	}
	if runs != 1 || up.count("POST") != 2 { // one push: the two notes, once, with the last text
		t.Fatalf("%d runs, %d posts", runs, up.count("POST"))
	}
	for _, c := range up.calls {
		if c.method == "POST" && c.body["title"] == "Structure one" && !strings.Contains(c.body["body"].(string), "version 199") {
			t.Fatal("not the last text")
		}
	}
	// a stream of edits that never pauses cannot postpone the push for ever: at most MaxWait
	g, gk, gnotes := autoFx(t)
	up2 := okUp()
	g.ok(g.write(ctx, func(tx *sql.Tx) error { return g.markUpstreamDirty(tx, gk) }))
	gn := gnotes["Structure one"]
	pushes := 0
	for i := 0; i < 90; i++ { // 15 minutes, a change every 10 s
		gn = g.edit("ana", gk, gn, fmt.Sprint("v", i))
		g.clock = g.clock.Add(10 * time.Second)
		if r, _ := g.AutoPush(ctx, up2, noSleep); r.Ran {
			pushes++
		}
	}
	if pushes != 1 {
		t.Fatalf("%d pushes in 15 minutes of steady edits (one at the 10 minute mark expected)", pushes)
	}
}

func TestAutoPushBacksOffAfterAFailureAndRecovers(t *testing.T) {
	f, fk, _ := autoFx(t)
	f.ok(f.write(ctx, func(tx *sql.Tx) error { return f.markUpstreamDirty(tx, fk) }))
	down := &fakeUp{handler: func(upCall) (int, string, time.Duration) { return 503, `{"error":{"message":"down"}}`, 0 }}
	f.clock = f.clock.Add(61 * time.Second)
	wantWaits := []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 32 * time.Minute, time.Hour, time.Hour}
	for i, w := range wantWaits {
		r, e := f.AutoPush(ctx, down, noSleep)
		f.ok(e)
		if !r.Ran {
			t.Fatalf("attempt %d did not run", i+1)
		}
		l := f.link(fk)
		if l.FailCount != i+1 || l.NextAttempt == nil || l.NextAttempt.Sub(f.clock) != w || l.LastFail == nil || l.DirtySince == nil || l.LastError == "" {
			t.Fatalf("attempt %d: %+v (wait %v, want %v)", i+1, l, l.NextAttempt.Sub(f.clock), w)
		}
		// before the back-off ends: nothing is tried
		f.clock = f.clock.Add(w - time.Second)
		if r, _ = f.AutoPush(ctx, down, noSleep); r.Ran {
			t.Fatal("tried before the back-off ended")
		}
		f.clock = f.clock.Add(time.Second)
	}
	// the upstream is back: the next attempt succeeds, the flag and the back-off are cleared
	r, e := f.AutoPush(ctx, okUp(), noSleep)
	f.ok(e)
	l := f.link(fk)
	if !r.Ran || r.Push.Created != 2 || l.FailCount != 0 || l.DirtySince != nil || l.NextAttempt != nil || l.LastOK == nil {
		t.Fatalf("%+v %+v", r, l)
	}
}

func TestAutoPushJitterStaysWithinTwentyPercent(t *testing.T) {
	tm := DefaultAutoTiming
	for _, j := range []float64{0, 0.25, 0.5, 0.75, 1} {
		got := tm.backoff(3, func() float64 { return j })
		if got < 4*time.Minute*8/10 || got > 4*time.Minute*12/10 {
			t.Fatalf("jitter %v: %v", j, got)
		}
	}
}

func TestAChangeWhileItRunsKeepsTheOutbox(t *testing.T) {
	f, fk, notes := autoFx(t)
	f.ok(f.write(ctx, func(tx *sql.Tx) error { return f.markUpstreamDirty(tx, fk) }))
	f.clock = f.clock.Add(61 * time.Second)
	ok := okUp().handler
	edited := false
	up := &fakeUp{handler: func(c upCall) (int, string, time.Duration) {
		if c.method == "POST" && !edited {
			edited = true
			f.clock = f.clock.Add(time.Second)
			f.edit("ana", fk, notes["Data two"], "changed while sending") // a member edits during the push
		}
		return ok(c)
	}}
	if _, e := f.AutoPush(ctx, up, noSleep); e != nil {
		t.Fatal(e)
	}
	if l := f.link(fk); l.DirtySince == nil {
		t.Fatalf("the change that arrived during the run was forgotten: %+v", l)
	}
	f.clock = f.clock.Add(61 * time.Second)
	r, _ := f.AutoPush(ctx, okUp(), noSleep)
	if !r.Ran {
		t.Fatal("the change was not pushed")
	}
}

func TestAutoPushOnlyForAutoLinksAndNeverWhileShuttingDown(t *testing.T) {
	f, notes := upFx(t)
	fk := f.pid["fakture"]
	f.ok(f.LinkUpstream(ctx, fk, 0, "hub", "shop", "", false)) // manual
	f.edit("ana", fk, notes["Structure one"], "x")
	f.clock = f.clock.Add(time.Hour)
	up := okUp()
	if r, _ := f.AutoPush(ctx, up, noSleep); r.Ran || len(up.calls) != 0 {
		t.Fatal("a manual link was pushed automatically")
	}
	if l := f.link(fk); l.DirtySince != nil || l.Auto {
		t.Fatalf("%+v", l)
	}
	// switching auto on marks what is unsent; off clears the outbox
	yes, no := true, false
	f.ok(f.LinkUpstreamWith(ctx, LinkSpec{Project: fk, Upstream: "hub", Remote: "shop", Auto: &yes}))
	if l := f.link(fk); !l.Auto || l.DirtySince == nil {
		t.Fatalf("%+v", l)
	}
	// a stopping gateway pushes nothing
	f.clock = f.clock.Add(time.Hour)
	stopped, cancel := context.WithCancel(ctx)
	cancel()
	if r, _ := f.AutoPush(stopped, up, noSleep); r.Ran || len(up.calls) != 0 {
		t.Fatal("pushed while shutting down")
	}
	f.ok(f.LinkUpstreamWith(ctx, LinkSpec{Project: fk, Upstream: "hub", Remote: "shop", Auto: &no}))
	if l := f.link(fk); l.Auto || l.DirtySince != nil {
		t.Fatalf("%+v", l)
	}
	// a link that is not mentioned keeps its setting when it is changed otherwise
	f.ok(f.LinkUpstreamWith(ctx, LinkSpec{Project: fk, Upstream: "hub", Remote: "shop", Auto: &yes}))
	f.ok(f.LinkUpstream(ctx, fk, 0, "hub", "shop", "structure", false))
	if l := f.link(fk); !l.Auto || len(l.Classes) != 1 {
		t.Fatalf("%+v", l)
	}
}

func TestAWriteLimitIsNotAFailureForTheAutoPush(t *testing.T) {
	f, fk, _ := autoFx(t)
	f.ok(f.write(ctx, func(tx *sql.Tx) error { return f.markUpstreamDirty(tx, fk) }))
	f.clock = f.clock.Add(61 * time.Second)
	limited := &fakeUp{handler: func(c upCall) (int, string, time.Duration) {
		if c.method == "GET" {
			return 200, `{"id":7,"slug":"shop"}`, 0
		}
		return 429, `{}`, 30 * time.Second
	}}
	r, e := f.AutoPush(ctx, limited, func(time.Duration) bool { return false })
	f.ok(e)
	l := f.link(fk)
	if !r.Ran || !r.Push.More || l.FailCount != 0 || l.LastFail != nil || l.NextAttempt == nil || l.NextAttempt.Sub(f.clock) != 30*time.Second || l.DirtySince == nil {
		t.Fatalf("%+v %+v", r, l)
	}
}

func TestNotesNoLongerHereAreListedNotPropagated(t *testing.T) {
	f, fk, notes := autoFx(t)
	_, e := f.PushUpstream(ctx, okUp(), fk, 0, "", 0, false, noSleep)
	f.ok(e)
	f.clock = f.clock.Add(time.Second)
	gone := notes["Data two"]
	f.ok(f.write(ctx, func(tx *sql.Tx) error { _, e := tx.Exec(`DELETE FROM notes WHERE id=?`, gone.ID); return e })) // deleted here
	// the other goes private here
	one := notes["Structure one"]
	cur, e := f.GetNote(ctx, f.p("fani"), fk, one.ID)
	f.ok(e)
	_, e = f.UpdateNote(ctx, f.p("fani"), fk, one.ID, cur.Revision, NoteInput{Class: Private, Title: cur.Title, Body: cur.Body})
	f.ok(e)
	l := f.link(fk)
	if len(l.NotPropagated) != 2 || l.NotPropagated[0].Why != "now private here" && l.NotPropagated[1].Why != "now private here" {
		t.Fatalf("%+v", l.NotPropagated)
	}
	whys := fmt.Sprint(l.NotPropagated)
	if !strings.Contains(whys, "deleted here") || !strings.Contains(whys, "now private here") {
		t.Fatal(whys)
	}
}

// a link that keeps ending with "more" and getting nowhere backs off, resumes from its cursor, and writes a bounded audit
func autoAudits(f *fx) (n int) {
	f.ok(f.read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT count(*) FROM audit_events WHERE action='upstream.auto-push'`).Scan(&n)
	}))
	return
}

// slowRefuser: every note is refused with a 400 and takes `cost` of virtual time (the clock of the fixture is advanced by it)
func slowRefuser(f *fx, cost time.Duration, budget int) *fakeUp {
	return &fakeUp{handler: func(c upCall) (int, string, time.Duration) {
		if c.method == "GET" {
			return 200, `{"id":7,"slug":"shop"}`, 0
		}
		f.clock = f.clock.Add(cost)
		return 400, `{"error":{"message":"no"}}`, 0
	}}
}

func TestAMoreRunWithNoProgressBacksOff(t *testing.T) {
	f, fk, _ := autoFx(t)
	f.ok(f.write(ctx, func(tx *sql.Tx) error { return f.markUpstreamDirty(tx, fk) }))
	f.clock = f.clock.Add(61 * time.Second)
	up := slowRefuser(f, time.Second, 0)
	// the time budget of every call is used up after the first note: ctx cancelled by the second call's check
	runs := 0
	start := f.clock
	for f.clock.Sub(start) < time.Hour {
		cctx, cancel := context.WithCancel(ctx)
		ucancel := &cancelAfterPost{up: up, cancel: cancel}
		r, e := f.AutoPush(cctx, ucancel, noSleep)
		cancel()
		f.ok(e)
		if r.Ran {
			runs++
		}
		f.clock = f.clock.Add(time.Second)
	}
	if runs > 6 { // base 1 min doubling: about 1,2,4,8,16,32 min within the hour
		t.Fatalf("%d runs in an hour: no back-off", runs)
	}
	if l := f.link(fk); l.FailCount < 2 {
		t.Fatalf("%+v", l)
	}
	if n := autoAudits(f); n > runs {
		t.Fatalf("%d audit rows for %d runs", n, runs)
	}
}

// cancelAfterPost: lets one POST through, then ends the call's time budget (as the Mac helper's 10 s does)
type cancelAfterPost struct {
	up     *fakeUp
	cancel func()
}

func (c *cancelAfterPost) Do(ctx context.Context, up, m, p string, h map[string]string, b []byte) (int, []byte, time.Duration, error) {
	st, d, w, e := c.up.Do(ctx, up, m, p, h, b)
	if m == "POST" {
		c.cancel()
	}
	return st, d, w, e
}

func TestARateLimitIsWaitedOutOnceAndAudited(t *testing.T) {
	f, fk, _ := autoFx(t)
	f.ok(f.write(ctx, func(tx *sql.Tx) error { return f.markUpstreamDirty(tx, fk) }))
	f.clock = f.clock.Add(61 * time.Second)
	limited := &fakeUp{handler: func(c upCall) (int, string, time.Duration) {
		if c.method == "GET" {
			return 200, `{"id":7,"slug":"shop"}`, 0
		}
		return 429, `{}`, 120 * time.Second
	}}
	runs := 0
	start := f.clock
	for f.clock.Sub(start) < time.Hour {
		r, e := f.AutoPush(ctx, limited, func(time.Duration) bool { return false })
		f.ok(e)
		if r.Ran {
			runs++
			if l := f.link(fk); l.FailCount != 0 || l.NextAttempt == nil || l.NextAttempt.Sub(f.clock) < 120*time.Second {
				t.Fatalf("%+v", l)
			}
		}
		f.clock = f.clock.Add(10 * time.Second)
	}
	if runs > 31 || runs < 25 { // one per 120 s, never sooner
		t.Fatalf("%d runs in an hour", runs)
	}
	if n := autoAudits(f); n != 1 {
		t.Fatalf("%d audit rows for %d identical runs", n, runs)
	}
}

func TestMoreThanAThousandRefusedNotesAreNotResentEveryRun(t *testing.T) {
	f, fk, _ := autoFx(t)
	for i := 0; i < maxPushNotes+50; i++ {
		f.clock = f.clock.Add(time.Millisecond)
		keySeq++
		_, _, e := f.CreateNote(ctx, f.p("ana"), fk, fmt.Sprint("uk", keySeq), NoteInput{Class: Structure, Title: fmt.Sprint("bulk ", i), Body: "b"})
		f.ok(e)
	}
	f.clock = f.clock.Add(61 * time.Second)
	up := slowRefuser(f, 0, 0)
	hour := f.clock.Add(time.Hour)
	runs, afterTwo := 0, 0
	for f.clock.Before(hour) {
		r, e := f.AutoPush(ctx, up, noSleep)
		f.ok(e)
		if r.Ran {
			runs++
			if runs == 2 {
				afterTwo = up.count("POST")
			}
		}
		f.clock = f.clock.Add(10 * time.Second)
	}
	// 1052 notes (3 setup notes of which 2 are sent): the first run refuses 1000, the second goes on from the cursor (not from note 0)
	// and sees only the rest, then the cursor resets and the back-off has grown
	if afterTwo != maxPushNotes+52 {
		t.Fatalf("%d POSTs in two runs: the second one did not go on from the cursor", afterTwo)
	}
	if runs > 6 {
		t.Fatalf("%d runs in an hour: no back-off", runs)
	}
}

func TestAMoreRunGoesOnFromTheCursor(t *testing.T) {
	f, fk, _ := autoFx(t)
	for i := 0; i < maxPushNotes+10; i++ {
		f.clock = f.clock.Add(time.Millisecond)
		keySeq++
		_, _, e := f.CreateNote(ctx, f.p("ana"), fk, fmt.Sprint("uk", keySeq), NoteInput{Class: Structure, Title: fmt.Sprint("bulk ", i), Body: "b"})
		f.ok(e)
	}
	f.clock = f.clock.Add(61 * time.Second)
	up := okUp()
	r, e := f.AutoPush(ctx, up, noSleep)
	f.ok(e)
	if !r.Push.More || f.link(fk).FailCount != 0 { // progress: a "more" that sent notes is not a failure
		t.Fatalf("%+v", r)
	}
	f.clock = f.clock.Add(2 * time.Second)
	r, e = f.AutoPush(ctx, up, noSleep)
	f.ok(e)
	if r.Push.More || up.count("POST") != maxPushNotes+10+2 {
		t.Fatalf("%+v posts %d", r, up.count("POST"))
	}
}

func TestTwoPushesOfOneLinkDoNotOverlap(t *testing.T) {
	f, fk, _ := autoFx(t)
	f.ok(f.write(ctx, func(tx *sql.Tx) error { return f.markUpstreamDirty(tx, fk) }))
	f.clock = f.clock.Add(61 * time.Second)
	inside := make(chan struct{})
	release := make(chan struct{})
	slow := okUp()
	inner := slow.handler
	first := true
	slow.handler = func(c upCall) (int, string, time.Duration) {
		if c.method == "POST" && first {
			first = false
			close(inside)
			<-release
		}
		return inner(c)
	}
	done := make(chan error, 1)
	go func() { _, e := f.AutoPush(ctx, slow, noSleep); done <- e }()
	<-inside
	if _, e := f.PushUpstream(ctx, okUp(), fk, 0, "", 0, false, noSleep); e == nil || !strings.Contains(e.Error(), "is running") {
		t.Fatalf("a second push of the link was not refused: %v", e)
	}
	if r, e := f.AutoPush(ctx, okUp(), noSleep); e != nil || r.Ran {
		t.Fatalf("the timer pushed a link that is being pushed: %+v %v", r, e)
	}
	close(release)
	f.ok(<-done)
}
