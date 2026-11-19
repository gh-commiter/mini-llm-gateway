package core

import (
	"database/sql"
	"strings"
	"testing"
)

func baseOf(f *fx, fk int64, title string) (b int64) {
	f.ok(f.read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT remote_rev FROM upstream_sent WHERE project_id=? AND note_id=?`, fk, noteID(f, fk, title)).Scan(&b)
	}))
	return
}

func pushOnce(t *testing.T, f *fx, fk int64, h *hubFake) UpstreamPush {
	t.Helper()
	res, e := f.PushUpstream(ctx, h.fake, fk, 0, "", 0, false, noSleep)
	f.ok(e)
	return res[0]
}

// The upstream note changed since the version our text corresponds to (its base): nothing is sent, the note is a conflict; the owner decides.
// theirs: our text stays unsent and the upstream's revision is the new base; ours: a replacement at the upstream's CURRENT revision.
func TestHubChangeSinceOurVersionIsAConflict(t *testing.T) {
	for _, ours := range []bool{false, true} {
		f, fk, h := twoNotes(t) // both accepted (base 1: the receipts are "legacy": the base is read from the accepted submissions), both changed here
		h.notes[55] = 2         // the upstream's own change (or a restore, or a class change: a revision bump we cannot tell apart)
		posts := len(h.posts)
		r := pushOnce(t, f, fk, h)
		if r.Conflicts != 1 || r.Replaced != 1 || len(h.postsFor("Structure one", posts)) != 0 || !strings.Contains(strings.Join(r.Lines, "\n"), "since our last version (revision 2 there)") {
			t.Fatalf("%+v", r)
		}
		if baseOf(f, fk, "Structure one") != 1 { // the base was written down when the accepted submission was read
			t.Fatalf("base %d", baseOf(f, fk, "Structure one"))
		}
		ls, e := f.UpstreamLinks(ctx, fk, 0)
		f.ok(e)
		if len(ls[0].Conflicts) != 1 || ls[0].Conflicts[0].Gone || ls[0].Conflicts[0].RemoteRev != 2 {
			t.Fatalf("%+v", ls[0].Conflicts)
		}
		if again := pushOnce(t, f, fk, h); again.Conflicts != 1 || len(h.postsFor("Structure one", posts)) != 0 { // and it stays until resolved
			t.Fatalf("%+v", again)
		}
		id := noteID(f, fk, "Structure one")
		f.ok(f.ResolveUpstream(ctx, fk, 0, id, ours))
		h.notes[55] = 3 // it moved again meanwhile
		r = pushOnce(t, f, fk, h)
		got := h.postsFor("Structure one", posts)
		if ours {
			if len(got) != 1 || got[0].body["replaces"].(float64) != 55 || got[0].body["revision"].(float64) != 3 || !strings.Contains(strings.Join(r.Lines, "\n"), "resolved as ours") {
				t.Fatalf("ours: %+v %v", r, got)
			}
			continue
		}
		if len(got) != 0 || r.Conflicts != 0 { // theirs: our text stays unsent; the upstream's revision is the base
			t.Fatalf("theirs: %+v %v", r, got)
		}
		if baseOf(f, fk, "Structure one") != 2 {
			t.Fatalf("base %d", baseOf(f, fk, "Structure one"))
		}
	}
}

// Our own accepted replacement bumps the upstream revision: that is the new base, not a conflict. The upstream changing the note while our text
// did not change here: nothing to send, no conflict.
func TestOwnAcceptedReplacementIsTheNewBase(t *testing.T) {
	f, fk, h := twoNotes(t)
	r := pushOnce(t, f, fk, h) // replacements of 55 and 56 at revision 1
	if r.Replaced != 2 || r.Conflicts != 0 {
		t.Fatalf("%+v", r)
	}
	h.accept(3, 55, 2) // ours was accepted: the note is at revision 2 with our text (the submissions 3 and 4)
	h.accept(4, 56, 2)
	h.notes[56] = 3 // the upstream changes Data two afterwards; we change nothing there
	f.edit("ana", fk, Note{ID: noteID(f, fk, "Structure one")}, "again")
	posts := len(h.posts)
	r = pushOnce(t, f, fk, h)
	got := h.postsFor("Structure one", posts)
	if r.Conflicts != 0 || len(got) != 1 || got[0].body["revision"].(float64) != 2 || r.Skipped != 1 || baseOf(f, fk, "Structure one") != 2 {
		t.Fatalf("%+v %v", r, got)
	}
}

// A replacement of ours that is pending keeps the base; when the upstream note changes meanwhile, the next push is a conflict, not a rebase onto
// the newest revision (that is what made a rejected-as-stale replacement come back unseen by its author).
func TestPendingOrRejectedReplacementKeepsTheBase(t *testing.T) {
	for _, outcome := range []string{"pending", "rejected"} {
		f, fk, h := twoNotes(t)
		pushOnce(t, f, fk, h) // replacement S3 of 55 at revision 1; the base stays 1
		if baseOf(f, fk, "Structure one") != 1 {
			t.Fatalf("base %d", baseOf(f, fk, "Structure one"))
		}
		h.notes[55] = 2 // the upstream changes the note after our replacement
		if outcome == "rejected" {
			h.reject(3, "note 55 changed after this was written")
		}
		f.edit("ana", fk, Note{ID: noteID(f, fk, "Structure one")}, "once more")
		posts := len(h.posts)
		r := pushOnce(t, f, fk, h)
		if r.Conflicts != 1 || len(h.postsFor("Structure one", posts)) != 0 {
			t.Fatalf("%s: %+v", outcome, r)
		}
	}
}

// A first submission that is pending waits; after its review the rule applies: accepted and the upstream changed since: a conflict.
func TestWaitingFirstSubmissionThenConflict(t *testing.T) {
	f, fk, h := twoNotes(t)
	h.subs[1] = `{"id":%d,"state":"pending","note":0}`
	h.notes = map[int]int{}
	r := pushOnce(t, f, fk, h)
	if r.Waiting != 1 {
		t.Fatalf("%+v", r)
	}
	h.accept(1, 55, 1)
	h.notes[55] = 2
	posts := len(h.posts)
	r = pushOnce(t, f, fk, h)
	if r.Conflicts < 1 || len(h.postsFor("Structure one", posts)) != 0 {
		t.Fatalf("%+v", r)
	}
}

// A receipt whose base is not known (the last submission was rejected before this release, or an upstream that does not say what a replacement
// declared) behaves as before: the note is rebased onto the newest revision, and the base is learned from the next accepted submission.
func TestUnknownBaseBehavesAsBefore(t *testing.T) {
	f, fk, h := twoNotes(t)
	h.subs[1] = `{"id":%d,"state":"accepted","note":55,"replaces":55}` // an upstream that does not say the revision it declared
	h.notes[55] = 4
	posts := len(h.posts)
	r := pushOnce(t, f, fk, h)
	got := h.postsFor("Structure one", posts)
	if r.Conflicts != 0 || len(got) != 1 || got[0].body["revision"].(float64) != 4 {
		t.Fatalf("%+v %v", r, got)
	}
}
