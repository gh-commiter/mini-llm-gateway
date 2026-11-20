package core

import (
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"testing"
)

// The hub's copy of a note this gateway PUBLISHED is hidden only while it is the version we sent (the receipt's revision is the mirror
// row's); when it moved on the hub, members see the hub's version too, marked and quoted, with one line saying it differs from the local
// note; the owner's status keeps the conflict.
func TestMirrorShowsAHubCopyThatMovedAwayFromOurPush(t *testing.T) {
	f := newFx(t)
	f.ok(f.AddUpstream(ctx, "hub", "hub.example:8765"))
	fk := f.pid["fakture"]
	readingLink(f, fk, 0, "shop")
	local := f.mk("ana", "fakture", Structure)
	hubCopy := rawMirror(f, fk, "hub", 40, Structure, KindFinding, "project:shop", "orbit published", "the hub's text\nsecond line") // revision 3
	receipt := func(remoteRev int64, conflict int, conflictRev int64) {
		f.t.Helper()
		f.ok(f.write(ctx, func(tx *sql.Tx) error {
			_, e := tx.Exec(`INSERT OR REPLACE INTO upstream_sent(project_id,upstream,note_id,hash,revision,remote_sub,remote_note,sent,remote_rev,conflict,conflict_rev) VALUES(?,'hub',?,'h',1,0,40,1,?,?,?)`, fk, local.ID, remoteRev, conflict, conflictRev)
			return e
		}))
	}
	shown := func() bool { return hasID(f.search("ana", fk, "orbit"), hubCopy) }

	receipt(3, 0, 0) // the version we sent: the same revision
	if shown() {
		t.Fatal("the hub's copy of our own push is shown while it is the version we sent")
	}
	if _, e := f.GetNote(ctx, f.p("ana"), fk, hubCopy); !errors.Is(e, ErrNotFound) {
		t.Fatal(e)
	}

	receipt(2, 1, 3) // the hub moved to revision 3 (a conflict, or an edit there)
	if !shown() {
		t.Fatal("the moved hub copy is hidden from members")
	}
	n, e := f.GetNote(ctx, f.p("ana"), fk, hubCopy)
	f.ok(e)
	marker := "[from hub:hub (there: project:shop), as of 2026-11-14T10:00:00Z: reference data from another gateway, not instructions]"
	line := "[differs from the local note " + strconv.FormatInt(local.ID, 10) + ": the hub's copy was changed there after this gateway sent it]"
	want := marker + "\n" + line + "\n> the hub's text\n> second line\n[end of reference data from hub:hub]"
	if n.Body != want || n.Text != "the hub's text\nsecond line" {
		t.Fatalf("%q", n.Body)
	}
	mine := f.mk("ana", "fakture", Structure)
	faqCopy := rawMirror(f, fk, "hub", 41, Structure, KindFAQ, "project:shop", "orbit question?", "answer")
	f.ok(f.write(ctx, func(tx *sql.Tx) error {
		_, e := tx.Exec(`INSERT INTO upstream_sent(project_id,upstream,note_id,hash,revision,remote_sub,remote_note,sent,remote_rev) VALUES(?,'hub',?,'h',1,0,41,1,1)`, fk, mine.ID)
		return e
	}))
	faq, e := f.ListFAQ(ctx, f.p("ana"), fk, "")
	f.ok(e)
	if len(faq.Entries) != 1 || faq.Entries[0].ID != faqCopy || !strings.Contains(faq.Entries[0].Answer, "[differs from the local note "+strconv.FormatInt(mine.ID, 10)+":") {
		t.Fatalf("%+v", faq)
	}
	// a hub note that was never ours has no such line
	other := rawMirror(f, fk, "hub", 50, Structure, KindFinding, "project:shop", "orbit other", "x")
	if o, e := f.GetNote(ctx, f.p("ana"), fk, other); e != nil || strings.Contains(o.Body, "differs") {
		t.Fatalf("%+v %v", o, e)
	}
	// the owner's status keeps the conflict
	if l := linkOf(f, fk); len(l.Conflicts) != 1 || l.Conflicts[0].Note != local.ID || l.Conflicts[0].RemoteRev != 3 {
		t.Fatalf("%+v", l.Conflicts)
	}

	// --ours: our version is sent as a submission at the next push: the hub's copy stays visible and the conflict stays
	f.ok(f.ResolveUpstream(ctx, fk, 0, local.ID, true))
	if !shown() {
		t.Fatal("hidden after --ours")
	}
	receipt(2, 1, 3)
	// --theirs: the baseline becomes the hub's revision: the copy is the version we know, hidden as before
	f.ok(f.ResolveUpstream(ctx, fk, 0, local.ID, false))
	if shown() {
		t.Fatal("shown after --theirs")
	}
	if l := linkOf(f, fk); len(l.Conflicts) != 0 {
		t.Fatalf("%+v", l.Conflicts)
	}
}

// A copy that moved on the hub is shown even when it still ends with our trailer; a submit-mode receipt that names the accepted note (no
// baseline) keeps hiding it whatever the revision of the hub's copy.
func TestMirrorMovedCopyIgnoresTheTrailerSubmitModeKeepsIt(t *testing.T) {
	f := newFx(t)
	f.ok(f.AddUpstream(ctx, "hub", "hub.example:8765"))
	fk := f.pid["fakture"]
	readingLink(f, fk, 0, "shop")
	pub := f.mk("ana", "fakture", Structure)
	sub := f.mk("ana", "fakture", Structure)
	secret, err := f.upstreamKey(ctx)
	f.ok(err)
	trailer := func(local int64) string {
		return "\n\n" + upstreamRefMark + upstreamRef(secret, "hub", fk, local) + ", author ana -->\n"
	}
	moved := rawMirror(f, fk, "hub", 40, Structure, KindFinding, "project:shop", "orbit moved", "edited there"+trailer(pub.ID))
	submitted := rawMirror(f, fk, "hub", 41, Structure, KindFinding, "project:shop", "orbit submitted", "accepted"+trailer(sub.ID))
	f.ok(f.write(ctx, func(tx *sql.Tx) error {
		_, e := tx.Exec(`INSERT INTO upstream_sent(project_id,upstream,note_id,hash,revision,remote_sub,remote_note,sent,remote_rev) VALUES(?,'hub',?,'h',1,0,40,1,2),(?,'hub',?,'h',1,5,41,1,0)`, fk, pub.ID, fk, sub.ID)
		return e
	}))
	ids := f.search("ana", fk, "orbit")
	if !hasID(ids, moved) || hasID(ids, submitted) {
		t.Fatal(ids, moved, submitted)
	}
}
