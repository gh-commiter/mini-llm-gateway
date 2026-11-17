package core

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"
)

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
