package core

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// hubFake is a scripted hub that keeps the state a reviewer would see: submissions by id and the notes (id -> revision, 0 = gone).
type hubFake struct {
	subs  map[int]string // id -> JSON of the submission
	notes map[int]int    // note id -> revision
	posts []upCall
	next  int
	fake  *fakeUp
}

func newHubFake() *hubFake {
	h := &hubFake{subs: map[int]string{}, notes: map[int]int{}}
	h.fake = &fakeUp{handler: func(c upCall) (int, string, time.Duration) {
		var id int
		switch {
		case c.path == "/minigw/v1/projects/shop":
			return 200, `{"id":7,"slug":"shop"}`, 0
		case c.method == "POST":
			h.posts = append(h.posts, c)
			h.next++
			h.subs[h.next] = `{"id":%d,"state":"pending","note":0}`
			return 201, fmt.Sprintf(`{"id":%d,"state":"pending"}`, h.next), 0
		case c.method == "GET" && sscan(c.path, "/minigw/v1/projects/7/kb/submissions/%d", &id):
			if s, ok := h.subs[id]; ok {
				return 200, fmt.Sprintf(s, id), 0
			}
		case c.method == "GET" && sscan(c.path, "/minigw/v1/projects/7/notes/%d", &id):
			if r := h.notes[id]; r > 0 {
				return 200, fmt.Sprintf(`{"id":%d,"revision":%d}`, id, r), 0
			}
		}
		return 404, `{"error":{"message":"not found"}}`, 0
	}}
	return h
}

func sscan(path, format string, id *int) bool {
	n, e := fmt.Sscanf(path, format, id)
	return e == nil && n == 1
}

func (h *hubFake) accept(sub, note, rev int) {
	h.subs[sub] = fmt.Sprintf(`{"id":%%d,"state":"accepted","note":%d}`, note)
	h.notes[note] = rev
}

func (h *hubFake) reject(sub int, reason string) {
	h.subs[sub] = fmt.Sprintf(`{"id":%%d,"state":"rejected","note":0,"reason":%q}`, reason)
}

func (h *hubFake) lastPost() upCall { return h.posts[len(h.posts)-1] }

// TestUpstreamRejectedReplacementStaysAReplacement: #166. A replacement the hub rejected (stale at review) must not turn into a new note
// on the next push: the link to the hub's note survives, and the push replaces that note at its current revision.
func TestUpstreamRejectedReplacementStaysAReplacement(t *testing.T) {
	f, notes := upFx(t)
	fk := f.pid["fakture"]
	f.ok(f.LinkUpstream(ctx, fk, 0, "hub", "shop", "", false))
	h := newHubFake()
	push := func(dry bool) UpstreamPush {
		t.Helper()
		res, e := f.PushUpstream(ctx, h.fake, fk, 0, "", 0, dry, noSleep)
		f.ok(e)
		return res[0]
	}
	edit := func(body string) {
		t.Helper()
		f.clock = f.clock.Add(time.Second)
		n, e := f.GetNote(ctx, f.p("ana"), fk, notes["Structure one"].ID)
		f.ok(e)
		_, e = f.UpdateNote(ctx, f.p("ana"), fk, n.ID, n.Revision, NoteInput{Class: Structure, Title: "Structure one", Body: body})
		f.ok(e)
	}
	push(false) // submissions 1 and 2 (Structure one, Data two)
	structure := 1
	h.accept(structure, 55, 3) // the hub accepts it as note 55
	edit("v2")
	h.notes[55] = 4 // the hub's own edit: the replacement below is stale at review
	r := push(false)
	last := h.lastPost()
	if r.Replaced != 1 || last.body["replaces"].(float64) != 55 || last.body["revision"].(float64) != 4 {
		t.Fatalf("first replacement: %+v %+v", r, last)
	}
	rep := h.next
	h.reject(rep, "note 55 changed after this was written")
	h.notes[55] = 5
	posts := len(h.posts)
	if r = push(false); len(h.posts) != posts || r.Skipped != 2 { // rejected, nothing changed here: nothing is sent again
		t.Fatalf("nothing to send: %+v", r)
	}
	edit("v3")
	if r = push(true); r.Replaced != 1 || r.Created != 0 || !strings.Contains(r.Lines[0], "would replace") || !strings.Contains(r.Lines[0], "note 55") || !strings.Contains(r.Lines[0], "revision 5") || !strings.Contains(r.Lines[0], "rejected") || !strings.Contains(r.Lines[0], "changed after") {
		t.Fatalf("dry run: %+v", r)
	}
	r = push(false)
	last = h.lastPost()
	if r.Replaced != 1 || r.Created != 0 || last.body["replaces"].(float64) != 55 || last.body["revision"].(float64) != 5 || !strings.Contains(last.body["body"].(string), "v3") {
		t.Fatalf("second replacement is not a replacement of note 55: %+v %+v", r, last)
	}
	// and again after another reject: the link still holds
	h.reject(h.next, "no")
	edit("v4")
	push(false)
	if last = h.lastPost(); last.body["replaces"].(float64) != 55 || last.body["revision"].(float64) != 5 {
		t.Fatalf("third: %+v", last)
	}
	// the hub's note is gone: nothing is created, the conflict is listed, a dry run says so
	h.reject(h.next, "no")
	h.notes[55] = 0
	edit("v5")
	posts = len(h.posts)
	if r = push(true); r.Conflicts != 1 || !strings.Contains(r.Lines[0], "would conflict") || !strings.Contains(r.Lines[0], "deleted on the upstream") {
		t.Fatalf("gone dry run: %+v", r)
	}
	if r = push(false); r.Conflicts != 1 || len(h.posts) != posts || r.Created != 0 || r.Replaced != 0 {
		t.Fatalf("gone: %+v", r)
	}
	ls, e := f.UpstreamLinks(ctx, fk, 0)
	f.ok(e)
	if len(ls[0].Conflicts) != 1 || !ls[0].Conflicts[0].Gone || ls[0].Conflicts[0].Note != notes["Structure one"].ID {
		t.Fatalf("status: %+v", ls[0].Conflicts)
	}
	if r = push(false); len(h.posts) != posts || r.Conflicts != 1 { // and it stays so until resolved
		t.Fatalf("gone again: %+v", r)
	}
}

// TestAutoPushDoesNotLoopOnARejectedReplacement: after the hub rejects a replacement, the automatic push sends nothing until the note
// changes here again, and that next send is again a replacement of the same hub note.
func TestAutoPushDoesNotLoopOnARejectedReplacement(t *testing.T) {
	f, fk, notes := autoFx(t)
	h := newHubFake()
	n := notes["Structure one"]
	run := func() AutoRun {
		t.Helper()
		f.clock = f.clock.Add(2 * time.Minute)
		r, e := f.AutoPush(ctx, h.fake, noSleep)
		f.ok(e)
		return r
	}
	f.edit("ana", fk, n, "v1")
	if r := run(); !r.Ran || r.Push.Created != 2 {
		t.Fatalf("first: %+v", r)
	}
	h.accept(1, 55, 3)
	f.edit("ana", fk, n, "v2")
	if r := run(); !r.Ran || r.Push.Replaced != 1 || h.lastPost().body["replaces"].(float64) != 55 {
		t.Fatalf("replacement: %+v", r)
	}
	h.reject(h.next, "note 55 changed after this was written")
	posts := len(h.posts)
	for i := 0; i < 3; i++ { // nothing changed here: no run, no send
		if r := run(); r.Ran || len(h.posts) != posts {
			t.Fatalf("looped: %+v", r)
		}
	}
	f.edit("ana", fk, n, "v3")
	if r := run(); !r.Ran || r.Push.Replaced != 1 || len(h.posts) != posts+1 || h.lastPost().body["replaces"].(float64) != 55 {
		t.Fatalf("after the local change: %+v", r)
	}
	if l := f.link(fk); l.FailCount != 0 {
		t.Fatalf("%+v", l)
	}
}
