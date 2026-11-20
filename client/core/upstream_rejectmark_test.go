package core

import (
	"database/sql"
	"strings"
	"testing"
	"time"
)

func rejectedMarkers(f *fx) (n int) {
	f.t.Helper()
	f.ok(f.DB().QueryRow(`SELECT count(*) FROM meta WHERE key LIKE 'upstream_reconcile|%|rejected|%'`).Scan(&n))
	return
}

// A replayed submission the hub rejected (the receipt was lost): the fact is kept per receipt, every later push says it instead of the bare
// "unchanged", the status counts it, a change of the note sends it anew and clears it, and unlink drops it.
func TestRejectedReplayIsKeptAndShownUntilTheNoteChanges(t *testing.T) {
	f, notes := upFx(t)
	fk := f.pid["fakture"]
	f.ok(f.LinkUpstream(ctx, fk, 0, "hub", "shop", "", false))
	h := newHubFake()
	replay := true
	h.hook = func(c upCall) (int, string, bool) {
		if replay && c.method == "POST" && strings.Contains(c.body["title"].(string), "Structure one") {
			return 201, `{"id":5,"state":"rejected","reason":"too\nvague ‮"}`, true
		}
		return 0, "", false
	}
	push := func() UpstreamPush {
		t.Helper()
		res, e := f.PushUpstream(ctx, h.fake, fk, 0, "", 0, false, noSleep)
		f.ok(e)
		return res[0]
	}
	line := func(r UpstreamPush) string {
		for _, l := range r.Lines {
			if strings.Contains(l, "Structure one") {
				return l
			}
		}
		return ""
	}
	if l := line(push()); !strings.Contains(l, "which it rejected (too vague)") || rejectedMarkers(f) != 1 {
		t.Fatalf("first push: %q markers %d", l, rejectedMarkers(f))
	}
	l := line(push())
	if !strings.Contains(l, "unchanged; the upstream rejected it as submission 5: too vague; change the note to send it again") || strings.ContainsRune(l, '‮') {
		t.Fatalf("second push: %q", l)
	}
	links, e := f.UpstreamLinks(ctx, fk, 0)
	f.ok(e)
	if li := links[0]; li.Rejected != 1 || len(li.RejectedNotes) != 1 || li.RejectedNotes[0].Note != notes["Structure one"].ID || li.RejectedNotes[0].Submission != 5 || li.RejectedNotes[0].Reason != "too vague" {
		t.Fatalf("status: %+v", links[0])
	}
	// the note changes: sent anew (a new submission), the marker is gone
	replay = false
	h.reject(5, "too vague") // the hub knows the earlier submission as rejected (the push reads it back before it sends the changed text)
	f.clock = f.clock.Add(time.Second)
	n, e := f.GetNote(ctx, f.p("ana"), fk, notes["Structure one"].ID)
	f.ok(e)
	_, e = f.UpdateNote(ctx, f.p("ana"), fk, n.ID, n.Revision, NoteInput{Class: Structure, Title: "Structure one", Body: "v2"})
	f.ok(e)
	posts := len(h.posts)
	if r := push(); len(h.posts) != posts+1 || rejectedMarkers(f) != 0 || r.Created+r.Replaced != 1 {
		t.Fatalf("changed note: %+v markers %d", r, rejectedMarkers(f))
	}
	if links, _ = f.UpstreamLinks(ctx, fk, 0); links[0].Rejected != 0 {
		t.Fatalf("status still shows it: %+v", links[0])
	}
	// unlink drops a marker too
	f.ok(f.write(ctx, func(tx *sql.Tx) error { return markRejected(tx, fk, "hub", notes["Data two"].ID, 9, "x") }))
	f.ok(f.UnlinkUpstream(ctx, fk, 0, "hub"))
	if rejectedMarkers(f) != 0 {
		t.Fatal("unlink kept the marker")
	}
}

// The marker names its submission: a receipt that names another one does not show it.
func TestRejectedMarkerOfAnotherSubmissionIsNotShown(t *testing.T) {
	f, notes := upFx(t)
	fk := f.pid["fakture"]
	f.ok(f.LinkUpstream(ctx, fk, 0, "hub", "shop", "", false))
	h := newHubFake()
	_, e := f.PushUpstream(ctx, h.fake, fk, 0, "", 0, false, noSleep)
	f.ok(e)
	f.ok(f.write(ctx, func(tx *sql.Tx) error { return markRejected(tx, fk, "hub", notes["Structure one"].ID, 99, "old") }))
	if links, _ := f.UpstreamLinks(ctx, fk, 0); links[0].Rejected != 0 {
		t.Fatalf("a stale marker is shown: %+v", links[0])
	}
}

// The pull's learning step keeps a rejection durably, writes it once, and does not ask about that receipt again (not even after the hour).
func TestPullLearnsARejectionDurably(t *testing.T) {
	f, fk := readFx(t)
	ids := receipts(f, fk, 2)
	h := &learnHub{hub: &hub{}, subs: map[int64][2]string{1: {"200", `{"state":"rejected","note":0,"reason":"no thanks"}`}, 2: {"200", `{"state":"pending","note":0}`}}}
	pull(f, fk, h.up())
	if rejectedMarkers(f) != 1 {
		t.Fatalf("markers %d", rejectedMarkers(f))
	}
	n, e := f.GetNote(ctx, f.p("ana"), fk, ids[0])
	f.ok(e)
	f.ok(f.write(ctx, func(tx *sql.Tx) error { // the fake receipt holds hash 'h': make it the note's own, so that the note counts as unchanged
		_, e := tx.Exec(`UPDATE upstream_sent SET hash=? WHERE note_id=?`, upstreamHash(n), ids[0])
		return e
	}))
	links, e := f.UpstreamLinks(ctx, fk, 0)
	f.ok(e)
	if len(links) != 1 || links[0].Rejected != 1 || links[0].RejectedNotes[0].Reason != "no thanks" {
		t.Fatalf("status shows nothing of the learned rejection: %+v", links)
	}
	h.asked = nil
	f.clock = f.clock.Add(2 * learnAskedFor)
	pull(f, fk, h.up())
	for _, id := range h.asked {
		if id == 1 {
			t.Fatal("the rejected submission was asked about again", h.asked)
		}
	}
}
