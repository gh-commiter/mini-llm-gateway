package core

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"
)

func (h *hubFake) postsFor(title string, from int) (out []upCall) {
	for _, c := range h.posts[from:] {
		if c.body["title"] == title {
			out = append(out, c)
		}
	}
	return
}

func noteID(f *fx, fk int64, title string) (id int64) {
	f.ok(f.read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT id FROM notes WHERE title=? AND project_id=?`, title, fk).Scan(&id)
	}))
	return
}

// resolve --ours looks again before it creates: a hub note that is visible again is replaced, an earlier submission that can be read again
// decides (accepted: replace; pending: wait; rejected: as rejected); only an identity that is still unknown becomes a NEW note.
func TestResolveOursReadsAgainBeforeCreating(t *testing.T) {
	for _, c := range []struct {
		name     string
		hub      func(h *hubFake)
		title    string
		replaces float64
		revision float64
		wait     bool
		detail   string
	}{
		{"note visible again", func(h *hubFake) { h.notes[55] = 7 }, "Structure one", 55, 7, false, "resolved as ours, replaces the accepted note 55 upstream (revision 7)"},
		{"note still out of sight", func(h *hubFake) {}, "Structure one", 0, 0, false, "still deleted or out of sight: sent as a NEW note"},
		{"submission readable again, accepted", func(h *hubFake) { h.accept(2, 56, 4) }, "Data two", 56, 4, false, "replaces the accepted note 56 upstream (revision 4)"},
		{"submission readable again, pending", func(h *hubFake) { h.subs[2] = `{"id":%d,"state":"pending","note":0}` }, "Data two", 0, 0, true, "still pending upstream"},
		{"submission readable again, rejected", func(h *hubFake) { h.reject(2, "no thanks") }, "Data two", 0, 0, false, "rejected upstream (no thanks)"},
		{"submission still unreadable", func(h *hubFake) {}, "Data two", 0, 0, false, "sent as a NEW note (a second note if it did)"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, fk, h := twoNotes(t)
			h.notes[55] = 0 // Structure one: out of sight; Data two: its submission is deleted
			delete(h.subs, 2)
			res, e := f.PushUpstream(ctx, h.fake, fk, 0, "", 0, false, noSleep)
			f.ok(e)
			if res[0].Conflicts != 2 || res[0].Created+res[0].Replaced != 0 {
				t.Fatalf("%+v", res[0])
			}
			for _, title := range []string{"Structure one", "Data two"} {
				f.ok(f.ResolveUpstream(ctx, fk, 0, noteID(f, fk, title), true))
			}
			c.hub(h)
			posts := len(h.posts)
			res, e = f.PushUpstream(ctx, h.fake, fk, 0, "", 0, false, noSleep)
			f.ok(e)
			got := h.postsFor(c.title, posts)
			if c.wait {
				if len(got) != 0 || res[0].Waiting != 1 || res[0].Refused != 0 {
					t.Fatalf("%+v %v", res[0], got)
				}
				return
			}
			if len(got) != 1 || got[0].body["replaces"].(float64) != c.replaces || got[0].body["revision"].(float64) != c.revision {
				t.Fatalf("%+v %+v", res[0], got)
			}
			all := strings.Join(res[0].Lines, "\n")
			if !strings.Contains(all, c.detail) {
				t.Fatalf("no %q in %s", c.detail, all)
			}
		})
	}
}

// Waiting for a review is not a failure and does not keep the link due: nothing runs by itself because notes wait. The wait is looked at when a
// push happens anyway (a local change, a manual push, the periodic pull); then accepted: the latest text goes as a replacement, rejected: it goes
// as a new submission because it differs from the rejected one.
func TestWaitingNotesAreCheckedWhenAPushHappensAnyway(t *testing.T) {
	for _, outcome := range []string{"accepted", "rejected"} {
		t.Run(outcome, func(t *testing.T) {
			f, fk, notes := autoFx(t)
			h := newHubFake()
			n := notes["Structure one"]
			run := func(advance time.Duration) AutoRun {
				t.Helper()
				f.clock = f.clock.Add(advance)
				r, e := f.AutoPush(ctx, h.fake, noSleep)
				f.ok(e)
				return r
			}
			f.edit("ana", fk, n, "v1")
			if r := run(2 * time.Minute); !r.Ran || r.Push.Created != 2 {
				t.Fatalf("first: %+v", r)
			}
			f.edit("ana", fk, n, "v2") // the first submission is still pending
			if r := run(2 * time.Minute); !r.Ran || r.Push.Waiting != 1 || r.Push.Refused != 0 || len(h.posts) != 2 {
				t.Fatalf("wait: %+v", r.Push)
			}
			l := f.link(fk)
			if l.Waiting != 1 || l.FailCount != 0 || l.DirtySince != nil || l.NextAttempt != nil || l.LastError != "" {
				t.Fatalf("a wait is a failure, keeps the link due, or is not shown: %+v", l)
			}
			gets := h.fake.count("GET")
			for i := 0; i < 5; i++ { // no timer of its own: nothing runs, the upstream is not asked
				if r := run(20 * time.Minute); r.Ran || r.Next != 0 {
					t.Fatalf("round %d: %+v", i, r)
				}
			}
			if h.fake.count("GET") != gets {
				t.Fatal("the upstream was asked while only a wait was open")
			}
			if outcome == "accepted" {
				h.accept(1, 55, 1)
			} else {
				h.reject(1, "no")
			}
			if r := run(20 * time.Minute); r.Ran { // the review alone starts nothing
				t.Fatalf("%+v", r)
			}
			res, e := f.PushUpstream(ctx, h.fake, fk, 0, "", 0, false, noSleep) // the next push notices it, with no further local edit
			f.ok(e)
			want := float64(0)
			if outcome == "accepted" {
				want = 55
			}
			if res[0].Waiting != 0 || len(h.posts) != 3 || !strings.Contains(h.lastPost().body["body"].(string), "v2") || h.lastPost().body["replaces"].(float64) != want {
				t.Fatalf("after the review: %+v %+v", res[0], h.lastPost().body)
			}
			if l = f.link(fk); l.Waiting != 0 || l.FailCount != 0 {
				t.Fatalf("%+v", l)
			}
		})
	}
}

// A local change after the wait started is a push that happens anyway: it checks the wait too.
func TestALocalChangeChecksTheWaitingNotes(t *testing.T) {
	f, fk, notes := autoFx(t)
	h := newHubFake()
	n := notes["Structure one"]
	f.edit("ana", fk, n, "v1")
	f.clock = f.clock.Add(2 * time.Minute)
	_, e := f.AutoPush(ctx, h.fake, noSleep)
	f.ok(e)
	f.edit("ana", fk, n, "v2")
	f.clock = f.clock.Add(2 * time.Minute)
	if r, _ := f.AutoPush(ctx, h.fake, noSleep); !r.Ran || r.Push.Waiting != 1 {
		t.Fatalf("%+v", r.Push)
	}
	h.accept(1, 55, 1)
	f.edit("ana", fk, notes["Data two"], "other note changed") // a change elsewhere
	f.clock = f.clock.Add(2 * time.Minute)
	r, e := f.AutoPush(ctx, h.fake, noSleep)
	f.ok(e)
	if !r.Ran || r.Push.Replaced != 1 || r.Push.Waiting != 1 || h.lastPost().body["replaces"].(float64) != 55 { // Data two is still pending
		t.Fatalf("%+v", r.Push)
	}
}

// The periodic pull of a link never pushes: a waiting note is looked at only when a push happens anyway.
func TestThePeriodicPullDoesNotPush(t *testing.T) {
	f, fk, ph := autoPullFx(t)
	yes := true
	f.ok(f.LinkUpstreamWith(ctx, LinkSpec{Project: fk, Upstream: "hub", Remote: "shop", Auto: &yes}))
	hf := newHubFake()
	pull := ph.up()
	both := &fakeUp{handler: func(c upCall) (int, string, time.Duration) {
		var id int
		if c.method == "POST" || strings.Contains(c.path, "/kb/submissions") || sscan(c.path, "/minigw/v1/projects/7/notes/%d", &id) && id >= 50 {
			return hf.fake.handler(c)
		}
		st, b, _, _ := pull.Do(ctx, "hub", c.method, c.path, nil, nil)
		return st, string(b), 0
	}}
	keySeq++
	n, _, e := f.CreateNote(ctx, f.p("ana"), fk, fmt.Sprint("uk", keySeq), NoteInput{Class: Structure, Title: "Wait one", Body: "v1"})
	f.ok(e)
	_, e = f.PushUpstream(ctx, both, fk, 0, "", 0, false, noSleep)
	f.ok(e)
	f.clock = f.clock.Add(time.Second)
	_, e = f.UpdateNote(ctx, f.p("ana"), fk, n.ID, n.Revision, NoteInput{Class: n.Class, Title: n.Title, Body: "v2"})
	f.ok(e)
	if res, e := f.PushUpstream(ctx, both, fk, 0, "", 0, false, noSleep); e != nil || res[0].Waiting != 1 {
		t.Fatalf("%+v %v", res, e)
	}
	hf.accept(1, 55, 1)
	posts, gets := len(hf.posts), hf.fake.count("GET")
	f.clock = f.clock.Add(16 * time.Minute) // the pull interval
	r := f.autoPull(both)
	if !r.Ran || r.Pull == nil || r.Push != nil || len(hf.posts) != posts || hf.fake.count("GET") != gets {
		t.Fatalf("%+v", r)
	}
	if f.link(fk).Waiting != 1 {
		t.Fatalf("%+v", f.link(fk))
	}
}

// resolve --ours finds the earlier submission pending: that wait is counted and resolved like any other, and "ours" stays in force.
func TestResolvedAsOursWaitLifecycle(t *testing.T) {
	for _, outcome := range []string{"accepted", "rejected"} {
		t.Run(outcome, func(t *testing.T) {
			f, fk, h := twoNotes(t)
			delete(h.subs, 2) // Data two: its earlier submission cannot be read: a conflict
			if res, e := f.PushUpstream(ctx, h.fake, fk, 0, "", 0, false, noSleep); e != nil || res[0].Conflicts < 1 {
				t.Fatalf("%+v %v", res, e)
			}
			data := noteID(f, fk, "Data two")
			f.ok(f.ResolveUpstream(ctx, fk, 0, data, true))
			h.subs[2] = `{"id":%d,"state":"pending","note":0}` // readable again, pending
			h.notes[55] = 0                                    // (Structure one stays out of the way)
			posts := len(h.posts)
			res, e := f.PushUpstream(ctx, h.fake, fk, 0, "", 0, false, noSleep)
			f.ok(e)
			if res[0].Waiting != 1 || len(h.posts) != posts || f.link(fk).Waiting != 1 {
				t.Fatalf("%+v %+v", res[0], f.link(fk))
			}
			if outcome == "accepted" {
				h.accept(2, 56, 4)
			} else {
				h.reject(2, "no")
			}
			res, e = f.PushUpstream(ctx, h.fake, fk, 0, "", 0, false, noSleep)
			f.ok(e)
			got := h.postsFor("Data two", posts)
			if len(got) != 1 || f.link(fk).Waiting != 0 {
				t.Fatalf("%+v %v", res[0], got)
			}
			if outcome == "accepted" && (got[0].body["replaces"].(float64) != 56 || got[0].body["revision"].(float64) != 4) || outcome == "rejected" && got[0].body["replaces"].(float64) != 0 {
				t.Fatalf("%+v", got[0].body)
			}
		})
	}
}

// Only notes the link would push now are waiting: a waiting note outside the link's classes is not counted; back in, it is looked at again.
func TestWaitingCountsOnlyNotesInTheLinksScope(t *testing.T) {
	f, fk, notes := autoFx(t)
	h := newHubFake()
	f.edit("ana", fk, notes["Structure one"], "v1")
	_, e := f.PushUpstream(ctx, h.fake, fk, 0, "", 0, false, noSleep)
	f.ok(e)
	f.edit("ana", fk, notes["Structure one"], "v2")
	if res, e := f.PushUpstream(ctx, h.fake, fk, 0, "", 0, false, noSleep); e != nil || res[0].Waiting != 1 || f.link(fk).Waiting != 1 {
		t.Fatalf("%+v %v", res, e)
	}
	f.ok(f.LinkUpstream(ctx, fk, 0, "hub", "shop", "data", false)) // structure leaves the link
	if f.link(fk).Waiting != 0 {
		t.Fatalf("%+v", f.link(fk))
	}
	gets := h.fake.count("GET")
	f.ok(func() error { _, e := f.PushUpstream(ctx, h.fake, fk, 0, "", 0, false, noSleep); return e }())
	if h.fake.count("GET") != gets+1 { // only the project lookup: the note is not looked at
		t.Fatalf("%d requests", h.fake.count("GET")-gets)
	}
	f.ok(f.LinkUpstream(ctx, fk, 0, "hub", "shop", "structure,data", false)) // and back
	if f.link(fk).Waiting != 1 {
		t.Fatalf("%+v", f.link(fk))
	}
	h.accept(1, 55, 1)
	posts := len(h.posts)
	if res, e := f.PushUpstream(ctx, h.fake, fk, 0, "", 0, false, noSleep); e != nil || res[0].Replaced < 1 || len(h.posts) != posts+1 || h.lastPost().body["replaces"].(float64) != 55 {
		t.Fatalf("%+v %v", res, e)
	}
}

// expiringUp: answers `after` requests, then every request waits for its context to end (a time slice used up inside a request).
type expiringUp struct {
	inner UpstreamTransport
	after int
	n     int
}

func (u *expiringUp) Do(c context.Context, up, method, path string, h map[string]string, body []byte) (int, []byte, time.Duration, error) {
	u.n++
	if u.n > u.after {
		<-c.Done()
		return 0, nil, 0, c.Err()
	}
	return u.inner.Do(c, up, method, path, h, body)
}

// A time slice that ends INSIDE a request is not the upstream failing: the run ends with More and the cursor, what was checked counts as progress.
func TestSliceExpiringInsideARequestKeepsTheCursor(t *testing.T) {
	f, fk, h := twoNotes(t)
	h.subs[1] = `{"id":%d,"state":"pending","note":0}`
	h.subs[2] = `{"id":%d,"state":"pending","note":0}`
	h.notes = map[int]int{}
	slow := &expiringUp{inner: h.fake, after: 2} // the project lookup and the first note's submission
	cctx, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
	defer cancel()
	res, e := f.PushUpstream(cctx, slow, fk, 0, "", 0, false, noSleep)
	f.ok(e)
	r := res[0]
	if !r.More || r.Waiting != 1 || r.Refused != 0 || r.Next != noteID(f, fk, "Structure one") || !strings.Contains(r.Stopped, "time budget") {
		t.Fatalf("%+v", r)
	}
	// the next call goes on above the cursor and finds the second one
	res, e = f.PushUpstream(ctx, h.fake, fk, 0, "hub", r.Next, false, noSleep)
	f.ok(e)
	if res[0].Waiting != 1 || res[0].More {
		t.Fatalf("%+v", res[0])
	}
}

// A pending first submission: no POST and the wait state is remembered and shown; the text going back to what was sent ends the wait.
func TestWaitingEndsWhenTheTextIsAsSentAgain(t *testing.T) {
	f, fk, notes := autoFx(t)
	h := newHubFake()
	n := notes["Structure one"]
	if _, e := f.PushUpstream(ctx, h.fake, fk, 0, "", 0, false, noSleep); e != nil {
		t.Fatal(e)
	}
	f.edit("ana", fk, n, "other")
	res, e := f.PushUpstream(ctx, h.fake, fk, 0, "", 0, false, noSleep)
	f.ok(e)
	if res[0].Waiting != 1 || len(h.posts) != 2 || f.link(fk).Waiting != 1 {
		t.Fatalf("%+v", res[0])
	}
	f.edit("ana", fk, n, "body of Structure one") // the text it was sent with
	res, e = f.PushUpstream(ctx, h.fake, fk, 0, "", 0, false, noSleep)
	f.ok(e)
	if res[0].Skipped != 2 || res[0].Waiting != 0 || f.link(fk).Waiting != 0 {
		t.Fatalf("%+v %+v", res[0], f.link(fk))
	}
}

// A refusal that concerns one note (this identity may not write it) refuses that note and goes on; one that concerns the identity ends the run.
func TestUpstreamWriteRefusalOfOneNoteDoesNotStopTheRun(t *testing.T) {
	for _, c := range []struct {
		errType string
		status  int
		stops   bool
	}{{"forbidden", 403, false}, {"not_found", 404, false}, {"no_project_access", 403, true}, {"archived", 403, true}, {"no_submit_access", 403, true}, {"", 403, false}} {
		t.Run(fmt.Sprint(c.errType, c.status), func(t *testing.T) {
			f, fk, h := twoNotes(t)
			h.hook = func(cl upCall) (int, string, bool) {
				if cl.method == "POST" && cl.body["replaces"].(float64) == 55 {
					return c.status, fmt.Sprintf(`{"error":{"type":%q,"message":"no"}}`, c.errType), true
				}
				return 0, "", false
			}
			posts := len(h.posts)
			res, e := f.PushUpstream(ctx, h.fake, fk, 0, "", 0, false, noSleep)
			f.ok(e)
			r := res[0]
			if c.stops {
				if r.Stopped == "" || r.Replaced != 0 || len(h.posts) != posts {
					t.Fatalf("%+v", r)
				}
				return
			}
			if r.Stopped != "" || r.Refused != 1 || r.Replaced != 1 || len(h.posts) != posts+1 || h.lastPost().body["replaces"].(float64) != 56 {
				t.Fatalf("%+v", r)
			}
		})
	}
}

// Publish mode: the same for a note that may be read but not replaced.
func TestPublishWriteRefusalOfOneNoteDoesNotStopTheRun(t *testing.T) {
	f, notes := upFx(t)
	fk := f.pid["fakture"]
	f.ok(f.LinkUpstreamWith(ctx, LinkSpec{Project: fk, Upstream: "hub", Remote: "shop", Mode: "publish"}))
	rev := map[int]int{}
	nextID := 100
	up := &fakeUp{handler: func(c upCall) (int, string, time.Duration) {
		var id int
		switch {
		case c.path == "/minigw/v1/projects/shop":
			return 200, `{"id":7,"slug":"shop"}`, 0
		case c.method == "POST" && c.path == "/minigw/v1/projects/7/notes":
			nextID++
			rev[nextID] = 1
			return 201, fmt.Sprintf(`{"id":%d,"revision":1}`, nextID), 0
		case c.method == "GET" && sscan(c.path, "/minigw/v1/projects/7/notes/%d", &id):
			return 200, fmt.Sprintf(`{"id":%d,"revision":%d}`, id, rev[id]), 0
		case c.method == "PUT" && sscan(c.path, "/minigw/v1/projects/7/notes/%d", &id):
			if id == 101 {
				return 403, `{"error":{"type":"forbidden","message":"Not allowed"}}`, 0
			}
			rev[id]++
			return 200, fmt.Sprintf(`{"id":%d,"revision":%d}`, id, rev[id]), 0
		}
		return 404, `{}`, 0
	}}
	_, e := f.PushUpstream(ctx, up, fk, 0, "", 0, false, noSleep)
	f.ok(e)
	for _, title := range []string{"Structure one", "Data two"} {
		f.clock = f.clock.Add(time.Second)
		n, e := f.GetNote(ctx, f.p("ana"), fk, notes[title].ID)
		f.ok(e)
		_, e = f.UpdateNote(ctx, f.p("ana"), fk, n.ID, n.Revision, NoteInput{Class: n.Class, Title: title, Body: "changed " + title})
		f.ok(e)
	}
	res, e := f.PushUpstream(ctx, up, fk, 0, "", 0, false, noSleep)
	f.ok(e)
	if r := res[0]; r.Stopped != "" || r.Refused != 1 || r.Replaced != 1 {
		t.Fatalf("%+v", r)
	}
}

func fourChanged(t *testing.T) (*fx, int64, *hubFake) {
	f, notes := upFx(t)
	fk := f.pid["fakture"]
	for _, title := range []string{"Data three", "Data four"} {
		keySeq++
		n, _, e := f.CreateNote(ctx, f.p("ana"), fk, fmt.Sprint("uk", keySeq), NoteInput{Class: Data, Title: title, Body: "x"})
		f.ok(e)
		notes[title] = n
	}
	f.ok(f.LinkUpstream(ctx, fk, 0, "hub", "shop", "", false))
	h := newHubFake()
	_, e := f.PushUpstream(ctx, h.fake, fk, 0, "", 0, false, noSleep)
	f.ok(e)
	for i := 1; i <= 4; i++ {
		h.accept(i, 54+i, 1)
	}
	for _, title := range []string{"Structure one", "Data two", "Data three", "Data four"} {
		f.clock = f.clock.Add(time.Second)
		n, e := f.GetNote(ctx, f.p("ana"), fk, notes[title].ID)
		f.ok(e)
		_, e = f.UpdateNote(ctx, f.p("ana"), fk, n.ID, n.Revision, NoteInput{Class: n.Class, Title: title, Body: "changed " + title})
		f.ok(e)
	}
	return f, fk, h
}

// A hub that says the identity may not hand in at all stops the run at the first note.
func TestNoSubmitAccessStopsAtTheFirstNote(t *testing.T) {
	f, fk, h := fourChanged(t)
	h.hook = func(cl upCall) (int, string, bool) {
		if cl.method == "POST" {
			return 403, `{"error":{"type":"no_submit_access","message":"No right to hand in here"}}`, true
		}
		return 0, "", false
	}
	posts := h.fake.count("POST")
	res, e := f.PushUpstream(ctx, h.fake, fk, 0, "", 0, false, noSleep)
	f.ok(e)
	if r := res[0]; r.Refused != 1 || !strings.Contains(r.Stopped, "may no longer hand in") || h.fake.count("POST") != posts+1 {
		t.Fatalf("%+v", r)
	}
}

// A plain 403 forbidden (an old hub's answer for a reader, or a class the identity may not write) is always about one note: every note is
// tried and refused, the run is a failure without progress (back-off) and an automatic link does not loop; writable notes after refused ones
// are sent.
func TestPlainForbiddenRefusesNoteByNote(t *testing.T) {
	f, fk, h := fourChanged(t)
	deny := map[float64]bool{55: true, 56: true, 57: true} // the first three are refused, the fourth is writable
	h.hook = func(cl upCall) (int, string, bool) {
		if cl.method == "POST" && deny[cl.body["replaces"].(float64)] {
			return 403, `{"error":{"type":"forbidden","message":"Not allowed"}}`, true
		}
		return 0, "", false
	}
	posts := h.fake.count("POST")
	res, e := f.PushUpstream(ctx, h.fake, fk, 0, "", 0, false, noSleep)
	f.ok(e)
	if r := res[0]; r.Refused != 3 || r.Replaced != 1 || r.Stopped != "" || h.fake.count("POST") != posts+4 || h.lastPost().body["replaces"].(float64) != 58 {
		t.Fatalf("%+v", r)
	}
	f2, fk2, h2 := fourChanged(t) // every note refused: all are tried, and an automatic link backs off instead of looping
	h2.hook = func(cl upCall) (int, string, bool) {
		if cl.method == "POST" {
			return 403, `{"error":{"type":"forbidden","message":"Not allowed"}}`, true
		}
		return 0, "", false
	}
	yes := true
	f2.ok(f2.LinkUpstreamWith(ctx, LinkSpec{Project: fk2, Upstream: "hub", Remote: "shop", Auto: &yes}))
	f2.clock = f2.clock.Add(2 * time.Minute)
	posts = h2.fake.count("POST")
	r2, e := f2.AutoPush(ctx, h2.fake, noSleep)
	f2.ok(e)
	l := f2.link(fk2)
	if !r2.Ran || r2.Push.Refused != 4 || r2.Push.Stopped != "" || h2.fake.count("POST") != posts+4 || l.FailCount != 1 || l.NextAttempt == nil {
		t.Fatalf("%+v %+v", r2.Push, l)
	}
	f2.clock = f2.clock.Add(10 * time.Second)
	r3, e := f2.AutoPush(ctx, h2.fake, noSleep)
	f2.ok(e)
	if r3.Ran || h2.fake.count("POST") != posts+4 {
		t.Fatalf("looped: %+v", r3)
	}
}

// A first new note refused for its class does not stop the run: the next note is sent.
func TestFirstNewNoteRefusedForItsClassDoesNotStopTheRun(t *testing.T) {
	f, _ := upFx(t)
	fk := f.pid["fakture"]
	f.ok(f.LinkUpstream(ctx, fk, 0, "hub", "shop", "", false))
	h := newHubFake()
	h.hook = func(cl upCall) (int, string, bool) {
		if cl.method == "POST" && cl.body["title"] == "Structure one" {
			return 403, `{"error":{"type":"forbidden","message":"Not allowed"}}`, true
		}
		return 0, "", false
	}
	res, e := f.PushUpstream(ctx, h.fake, fk, 0, "", 0, false, noSleep)
	f.ok(e)
	if r := res[0]; r.Refused != 1 || r.Created != 1 || r.Stopped != "" || h.fake.count("POST") != 2 {
		t.Fatalf("%+v", r)
	}
}
