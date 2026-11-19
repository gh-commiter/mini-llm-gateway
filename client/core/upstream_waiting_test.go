package core

import (
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

// Waiting for a review is not a failure: no back-off, the link looks again at a quiet pace; accepted without another local edit: the latest text
// goes as a replacement; rejected: it goes as a new submission because it differs from the rejected one.
func TestAutoPushWaitsForAReviewWithoutFailing(t *testing.T) {
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
			if l.Waiting != 1 || l.FailCount != 0 || l.DirtySince == nil || l.LastError != "" {
				t.Fatalf("waiting is a failure or not shown: %+v", l)
			}
			// the quiet pace: not due again within 5 debounces, and then again without failing, nothing sent
			if r := run(2 * time.Minute); r.Ran {
				t.Fatal("looked again too soon")
			}
			for i := 0; i < 3; i++ {
				if r := run(6 * time.Minute); !r.Ran || r.Push.Waiting != 1 || len(h.posts) != 2 {
					t.Fatalf("round %d: %+v", i, r)
				}
				if l = f.link(fk); l.FailCount != 0 || l.Waiting != 1 {
					t.Fatalf("round %d: %+v", i, l)
				}
			}
			// the review: with no further local edit the latest text still goes
			if outcome == "accepted" {
				h.accept(1, 55, 1)
			} else {
				h.reject(1, "no")
			}
			r := run(6 * time.Minute)
			if !r.Ran || r.Push.Waiting != 0 || len(h.posts) != 3 || !strings.Contains(h.lastPost().body["body"].(string), "v2") {
				t.Fatalf("after the review: %+v %d", r.Push, len(h.posts))
			}
			want := float64(0)
			if outcome == "accepted" {
				want = 55
			}
			if h.lastPost().body["replaces"].(float64) != want {
				t.Fatalf("%+v", h.lastPost().body)
			}
			if l = f.link(fk); l.Waiting != 0 || l.DirtySince != nil || l.FailCount != 0 {
				t.Fatalf("%+v", l)
			}
			if r = run(6 * time.Minute); r.Ran || len(h.posts) != 3 { // and it is quiet again
				t.Fatalf("%+v", r)
			}
		})
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
	}{{"forbidden", 403, false}, {"not_found", 404, false}, {"no_project_access", 403, true}, {"archived", 403, true}, {"", 403, true}} {
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
