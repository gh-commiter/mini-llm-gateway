package core

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"
)

// hubFake is a scripted hub that keeps the state a reviewer would see: submissions by id and the notes (id -> revision, 0 = gone).
type hubFake struct {
	subs  map[int]string    // id -> JSON of the submission
	notes map[int]int       // note id -> revision
	text  map[int][3]string // note id -> class, title, body, when a read should return the text too
	posts []upCall
	next  int
	fake  *fakeUp
	hook  func(c upCall) (int, string, bool) // overrides an answer
}

func newHubFake() *hubFake {
	h := &hubFake{subs: map[int]string{}, notes: map[int]int{}, text: map[int][3]string{}}
	h.fake = &fakeUp{handler: func(c upCall) (int, string, time.Duration) {
		var id int
		if h.hook != nil {
			if st, b, ok := h.hook(c); ok {
				return st, b, 0
			}
		}
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
				if tx, ok := h.text[id]; ok {
					return 200, fmt.Sprintf(`{"id":%d,"revision":%d,"class":%q,"title":%q,"body":%q,"entryKind":"finding"}`, id, r, tx[0], tx[1], tx[2]), 0
				}
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

// accept: the submission was accepted and the hub note is at revision rev with our text (a new note at 1; else a replacement declared at rev-1).
func (h *hubFake) accept(sub, note, rev int) {
	if rev > 1 {
		h.subs[sub] = fmt.Sprintf(`{"id":%%d,"state":"accepted","note":%d,"replaces":%d,"revision":%d}`, note, note, rev-1)
	} else {
		h.subs[sub] = fmt.Sprintf(`{"id":%%d,"state":"accepted","note":%d,"replaces":0,"revision":0}`, note)
	}
	h.notes[note] = rev
}

func (h *hubFake) reject(sub int, reason string) {
	h.subs[sub] = fmt.Sprintf(`{"id":%%d,"state":"rejected","note":0,"reason":%q}`, reason)
}

func (h *hubFake) lastPost() upCall { return h.posts[len(h.posts)-1] }

// TestUpstreamRejectedReplacementStaysAReplacement: #166. A replacement the hub rejected must not turn into a new note on the next push: the
// link to the hub's note survives, and the push replaces that note again (the hub note did not change, so our base still holds).
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
	h.accept(structure, 55, 3) // the hub accepts it as note 55 (revision 3 with our text)
	edit("v2")
	r := push(false)
	last := h.lastPost()
	if r.Replaced != 1 || last.body["replaces"].(float64) != 55 || last.body["revision"].(float64) != 3 {
		t.Fatalf("first replacement: %+v %+v", r, last)
	}
	rep := h.next
	h.reject(rep, "not this time")
	posts := len(h.posts)
	if r = push(false); len(h.posts) != posts || r.Skipped != 2 { // rejected, nothing changed here: nothing is sent again
		t.Fatalf("nothing to send: %+v", r)
	}
	edit("v3")
	if r = push(true); r.Replaced != 1 || r.Created != 0 || !strings.Contains(r.Lines[0], "would replace") || !strings.Contains(r.Lines[0], "note 55") || !strings.Contains(r.Lines[0], "revision 3") || !strings.Contains(r.Lines[0], "rejected") || !strings.Contains(r.Lines[0], "not this time") {
		t.Fatalf("dry run: %+v", r)
	}
	r = push(false)
	last = h.lastPost()
	if r.Replaced != 1 || r.Created != 0 || last.body["replaces"].(float64) != 55 || last.body["revision"].(float64) != 3 || !strings.Contains(last.body["body"].(string), "v3") {
		t.Fatalf("second replacement is not a replacement of note 55: %+v %+v", r, last)
	}
	// and again after another reject: the link still holds
	h.reject(h.next, "no")
	edit("v4")
	push(false)
	if last = h.lastPost(); last.body["replaces"].(float64) != 55 || last.body["revision"].(float64) != 3 {
		t.Fatalf("third: %+v", last)
	}
	// the hub's note is gone: nothing is created, the conflict is listed, a dry run says so
	h.reject(h.next, "no")
	h.notes[55] = 0
	edit("v5")
	posts = len(h.posts)
	if r = push(true); r.Conflicts != 1 || !strings.Contains(r.Lines[0], "would conflict") || !strings.Contains(r.Lines[0], "deleted or no longer visible") {
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

// twoNotes: both notes pushed (submissions 1 and 2), accepted as hub notes 55 and 56, then both changed here.
func twoNotes(t *testing.T) (*fx, int64, *hubFake) {
	f, notes := upFx(t)
	fk := f.pid["fakture"]
	f.ok(f.LinkUpstream(ctx, fk, 0, "hub", "shop", "", false))
	h := newHubFake()
	_, e := f.PushUpstream(ctx, h.fake, fk, 0, "", 0, false, noSleep)
	f.ok(e)
	h.accept(1, 55, 1)
	h.accept(2, 56, 1)
	for _, title := range []string{"Structure one", "Data two"} {
		f.clock = f.clock.Add(time.Second)
		n, e := f.GetNote(ctx, f.p("ana"), fk, notes[title].ID)
		f.ok(e)
		_, e = f.UpdateNote(ctx, f.p("ana"), fk, n.ID, n.Revision, NoteInput{Class: n.Class, Title: title, Body: "changed " + title})
		f.ok(e)
	}
	return f, fk, h
}

func (h *hubFake) failNoteRead(note, status int, body string) {
	h.hook = func(c upCall) (int, string, bool) {
		if c.method == "GET" && c.path == fmt.Sprintf("/minigw/v1/projects/7/notes/%d", note) {
			return status, body, true
		}
		return 0, "", false
	}
}

// A refusal to read ONE note (403, a 5xx, a malformed answer) refuses that note with its id and goes on with the others; nothing becomes a new note.
func TestUpstreamNoteReadFailureRefusesOnlyThatNote(t *testing.T) {
	for _, c := range []struct {
		status int
		body   string
		want   string
	}{{403, `{"error":{"type":"forbidden","message":"Not allowed"}}`, "may not read it"}, {500, `{"error":{"message":"boom"}}`, "cannot be read"}, {200, `not json`, "cannot be read"}} {
		f, fk, h := twoNotes(t)
		h.failNoteRead(55, c.status, c.body)
		posts := len(h.posts)
		res, e := f.PushUpstream(ctx, h.fake, fk, 0, "", 0, false, noSleep)
		f.ok(e)
		r := res[0]
		if r.Refused != 1 || r.Stopped != "" || r.Replaced != 1 || len(h.posts) != posts+1 || h.lastPost().body["replaces"].(float64) != 56 || !strings.Contains(r.Lines[0], "note 55 upstream") || !strings.Contains(r.Lines[0], c.want) {
			t.Fatalf("%d: %+v", c.status, r)
		}
	}
}

// A 429 on the note read ends the run and keeps what was sent; a 401 stops it too.
func TestUpstreamNoteRead429AndAuthStopTheRun(t *testing.T) {
	for _, st := range []int{429, 401} {
		f, fk, h := twoNotes(t)
		h.failNoteRead(56, st, `{"error":{"message":"no"}}`) // the second note's read
		posts := len(h.posts)
		res, e := f.PushUpstream(ctx, h.fake, fk, 0, "", 0, false, func(time.Duration) bool { return false })
		f.ok(e)
		r := res[0]
		if len(h.posts) != posts+1 || r.Replaced != 1 || (r.Stopped == "" && !r.More) {
			t.Fatalf("%d: %+v", st, r)
		}
	}
}

// A first submission that was accepted on the hub but never learned of: the pusher learns it from the submission and writes it down before
// asking for the note, so even a gone note leaves the link in the receipt; and an unreadable or deleted earlier submission with no known hub
// note is never sent again as a new one.
func TestUpstreamAcceptanceIsRememberedAndUnknownIsNotResent(t *testing.T) {
	f, fk, h := twoNotes(t)
	var sent UpstreamSent
	remoteNote := func(title string) int64 {
		f.ok(f.read(ctx, func(tx *sql.Tx) error {
			return tx.QueryRow(`SELECT remote_note FROM upstream_sent WHERE project_id=? AND note_id=(SELECT id FROM notes WHERE title=? AND project_id=?)`, fk, title, fk).Scan(&sent.RemoteNote)
		}))
		return sent.RemoteNote
	}
	if remoteNote("Structure one") != 0 {
		t.Fatal("the acceptance was known before the pusher read it")
	}
	h.notes[55] = 0 // gone: the push stops for this note, but what it learned stays
	posts := len(h.posts)
	// the second note's earlier submission is deleted on the hub and was never learned of: not sent again as a new note
	delete(h.subs, 2)
	res, e := f.PushUpstream(ctx, h.fake, fk, 0, "", 0, false, noSleep)
	f.ok(e)
	if remoteNote("Structure one") != 55 || res[0].Conflicts != 2 || len(h.posts) != posts {
		t.Fatalf("%d %+v", remoteNote("Structure one"), res[0])
	}
	ls, e := f.UpstreamLinks(ctx, fk, 0)
	f.ok(e)
	if len(ls[0].Conflicts) != 2 {
		t.Fatalf("%+v", ls[0].Conflicts)
	}
	for _, c := range ls[0].Conflicts {
		if c.Gone == c.Submission { // one note is gone, the other's submission cannot be read
			t.Fatalf("%+v", ls[0].Conflicts)
		}
	}
	// the owner decides: "ours" sends it as a submission (a new note, the old one's fate is not known)
	var data Note
	f.ok(f.read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT id FROM notes WHERE title='Data two' AND project_id=?`, fk).Scan(&data.ID)
	}))
	f.ok(f.ResolveUpstream(ctx, fk, 0, data.ID, true))
	res, e = f.PushUpstream(ctx, h.fake, fk, 0, "", 0, false, noSleep)
	f.ok(e)
	if len(h.posts) != posts+1 || h.lastPost().body["replaces"].(float64) != 0 || res[0].Created != 1 { // a new submission is reported as create
		t.Fatalf("%+v", res[0])
	}
}

// An earlier submission that cannot be read right now (5xx) is refused, not resent; a pending first one is waited for.
func TestUpstreamPendingOrUnreadableFirstSubmissionIsNotResent(t *testing.T) {
	f, fk, h := twoNotes(t)
	h.subs[1] = `{"id":%d,"state":"pending","note":0}`
	h.notes = map[int]int{}
	h.subs[2] = `{"id":%d,"state":"weird","note":0}`
	h.hook = func(c upCall) (int, string, bool) {
		if c.method == "GET" && c.path == "/minigw/v1/projects/7/kb/submissions/2" {
			return 500, `{}`, true
		}
		return 0, "", false
	}
	posts := len(h.posts)
	res, e := f.PushUpstream(ctx, h.fake, fk, 0, "", 0, false, noSleep)
	f.ok(e)
	if res[0].Refused != 1 || res[0].Waiting != 1 || len(h.posts) != posts || res[0].Conflicts != 0 {
		t.Fatalf("%+v", res[0])
	}
}

// Stale at submit: the hub refuses a replacement whose revision is already old (409 with the current one): once more with that revision.
func TestUpstreamStaleAtSubmitIsAConflictUnlessOurs(t *testing.T) {
	f, fk, h := twoNotes(t)
	racing := true
	h.hook = func(c upCall) (int, string, bool) {
		if c.method == "POST" && c.body["replaces"].(float64) == 55 && racing { // the hub's note moved on between the read and the submit
			racing = false
			h.notes[55]++
			return 409, fmt.Sprintf(`{"error":{"type":"revision_mismatch","message":"The note has changed","revision":%d}}`, h.notes[55]), true
		}
		return 0, "", false
	}
	revs := func() (out []float64) {
		for _, c := range h.fake.calls {
			if c.method == "POST" && c.body["replaces"].(float64) == 55 {
				out = append(out, c.body["revision"].(float64))
			}
		}
		return
	}
	// a revision mismatch at submit is "changed there since our last version": no retry at the newer revision, nothing accepted
	res, e := f.PushUpstream(ctx, h.fake, fk, 0, "", 0, false, noSleep)
	f.ok(e)
	if r := res[0]; r.Conflicts != 1 || r.Replaced != 1 || r.Refused != 0 || len(revs()) != 1 || revs()[0] != 1 {
		t.Fatalf("%+v %v", r, revs())
	}
	// the owner decided "ours": one retry at the revision the upstream answers
	f.ok(f.ResolveUpstream(ctx, fk, 0, noteID(f, fk, "Structure one"), true))
	racing = true
	res, e = f.PushUpstream(ctx, h.fake, fk, 0, "", 0, false, noSleep)
	f.ok(e)
	got := revs()
	if r := res[0]; r.Replaced != 1 || r.Conflicts != 0 || len(got) != 3 || got[1] != 2 || got[2] != 3 {
		t.Fatalf("%+v %v", r, got)
	}
}

// Relinking to another project on the upstream would keep the old destination's submission and note numbers: refused while receipts exist.
func TestUpstreamRelinkToAnotherDestinationNeedsUnlink(t *testing.T) {
	f, notes := upFx(t)
	_ = notes
	fk := f.pid["fakture"]
	f.ok(f.LinkUpstream(ctx, fk, 0, "hub", "shop", "", false))
	f.ok(f.LinkUpstream(ctx, fk, 0, "hub", "shop", "", false))  // the same destination: fine
	f.ok(f.LinkUpstream(ctx, fk, 0, "hub", "shop2", "", false)) // no receipts yet: fine
	f.ok(f.LinkUpstream(ctx, fk, 0, "hub", "shop", "", false))
	_, e := f.PushUpstream(ctx, okUp(), fk, 0, "", 0, false, noSleep)
	f.ok(e)
	if e = f.LinkUpstream(ctx, fk, 0, "hub", "other", "", false); e == nil || !strings.Contains(e.Error(), "unlink it and link it again") {
		t.Fatal(e)
	}
}
