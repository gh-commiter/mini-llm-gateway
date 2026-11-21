package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"minigw/core"
)

// The hub (the gateway that RECEIVES) is restored from a backup: everything below is about that, not about a restore of the pushing gateway
// (upstream_restore_e2e_test.go).

// restoreHub restores the hub's core from bak, as `gw core restore` does (the hub is stopped meanwhile).
func (f *upFx) restoreHub(bak string) {
	f.t.Helper()
	dir := f.remote.core.Dir()
	f.remote.closeCore()
	f.must(core.Restore(dir, bak))
	f.remote.openCore()
	if f.remote.core == nil {
		f.t.Fatal("the restored hub did not open:", f.remote.coreErr)
	}
}

func (f *upFx) hubNotes() (n int) {
	f.t.Helper()
	f.must(f.remote.core.DB().QueryRow(`SELECT count(*) FROM notes WHERE project_id=?`, f.rproj).Scan(&n))
	return
}

func (f *upFx) acceptAll() {
	f.t.Helper()
	rc := f.remote.core
	rows, err := rc.DB().Query(`SELECT id FROM kb_submissions WHERE state='pending' ORDER BY id`)
	f.must(err)
	var ids []int64
	for rows.Next() {
		var id int64
		f.must(rows.Scan(&id))
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		if _, err := rc.ReviewSubmission(t0, id, true, "", seenHash(rc, id)); err != nil {
			f.t.Fatal(err)
		}
	}
}

// A: notes the hub accepted after its backup are gone there after the restore. The pushing gateway still holds its receipts: the push must
// not say "unchanged" (it did, forever); the link is marked, nothing is sent, status names the cause, reconcile lists the lost notes as
// unknown and --all --new then push recreates them: exactly one copy of each on the hub.
func TestUpstreamPushAfterTheHubIsRestored(t *testing.T) {
	f := newUpFx(t, core.Developer)
	f.add("hub")
	f.allow()
	f.seed()
	f.must(func() error { _, err := f.cmd(coreOpts{}, "link", "shop", "hub"); return err }())
	bak := filepath.Join(t.TempDir(), "hub-before-the-push.db")
	f.must(f.remote.core.Backup(t0, bak))
	if out, err := f.cmd(coreOpts{}, "push", "shop"); err != nil || !strings.Contains(out, "created 2") {
		t.Fatal(out, err)
	}
	f.acceptAll()
	if n := f.hubNotes(); n != 2 {
		t.Fatalf("%d notes on the hub", n)
	}
	f.restoreHub(bak)
	if n := f.hubNotes(); n != 0 {
		t.Fatalf("the restored hub has %d notes", n)
	}
	out, err := f.cmd(coreOpts{}, "push", "shop")
	subs, _ := f.remoteSubmissions()
	if err != nil || !strings.Contains(out, "blocked: ") || !strings.Contains(out, "the upstream was restored from a backup") || subs != 0 {
		t.Fatalf("A: after the hub's restore it has %d notes and %d submissions, and the push says: %q %v", f.hubNotes(), subs, out, err)
	}
	if st, err := f.cmd(coreOpts{}, "status", "shop"); err != nil || !strings.Contains(st, "the upstream was restored from a backup") {
		t.Fatalf("status: %q %v", st, err)
	}
	out, err = f.cmd(coreOpts{}, "reconcile", "shop")
	if err != nil || !strings.Contains(out, "candidates 0, unknown 2") {
		t.Fatalf("reconcile: %q %v", out, err)
	}
	if out, err = f.cmd(coreOpts{reconcileAll: true, reconcileNew: true}, "reconcile", "shop"); err != nil || !strings.Contains(out, "restored mark cleared: true") {
		t.Fatalf("reconcile --all --new: %q %v", out, err)
	}
	if out, err = f.cmd(coreOpts{}, "push", "shop"); err != nil || !strings.Contains(out, "created 2") {
		t.Fatalf("push after reconcile: %q %v", out, err)
	}
	f.acceptAll()
	var n, titles int
	f.must(f.remote.core.DB().QueryRow(`SELECT count(*), count(DISTINCT title) FROM notes WHERE project_id=?`, f.rproj).Scan(&n, &titles))
	if n != 2 || titles != 2 {
		t.Fatalf("the hub has %d notes with %d titles", n, titles)
	}
	if out, err = f.cmd(coreOpts{}, "push", "shop"); err != nil || strings.Contains(out, "blocked") {
		t.Fatalf("a push after that: %q %v", out, err)
	}
}

// B: after the restore the hub hands out note numbers again. A colleague's NEW hub note gets the number a receipt of ours names: it must be
// visible after the pull (it was hidden as "ours"), must never be replaced by our next edit (the dry run said "replaces the note N"), and our
// note ends up as its own new note.
func TestUpstreamPullAfterTheHubIsRestoredShowsAColleaguesNote(t *testing.T) {
	f := newUpFx(t, core.Developer)
	f.add("hub")
	f.allow()
	p, project, notes := f.seed()
	c := f.local.core
	f.must(func() error { _, err := f.cmd(coreOpts{}, "link", "shop", "hub"); return err }())
	f.must(func() error { _, err := f.cmd(coreOpts{read: true, acceptReach: true}, "link", "shop", "hub"); return err }())
	bak := filepath.Join(t.TempDir(), "hub-before-the-push.db")
	f.must(f.remote.core.Backup(t0, bak))
	if out, err := f.cmd(coreOpts{}, "push", "shop"); err != nil || !strings.Contains(out, "created 2") {
		t.Fatal(out, err)
	}
	f.acceptAll()
	if out, err := f.cmd(coreOpts{}, "pull", "shop"); err != nil {
		t.Fatal(out, err)
	}
	inv := notes["Invoice numbering"]
	var ours int64
	f.must(c.DB().QueryRow(`SELECT remote_note FROM upstream_sent WHERE note_id=?`, inv.ID).Scan(&ours))
	if ours == 0 {
		t.Fatal("the pull did not learn the hub note of the receipt")
	}
	f.restoreHub(bak)

	// a colleague writes a new note on the restored hub: it gets a number our receipt names
	rc := f.remote.core
	bob, err := rc.CreateUser(t0, "bob")
	f.must(err)
	f.must(rc.SetMember(t0, f.rproj, bob.ID, core.Developer))
	f.must(rc.SetBinding(t0, "dev-bob", bob.ID))
	bp, err := rc.Principal(t0, "dev-bob", true)
	f.must(err)
	theirs, _, err := rc.CreateNote(t0, bp, f.rproj, "bob-1", core.NoteInput{Class: core.Structure, Title: "Colleague checklist", Body: "the colleague's own text"})
	f.must(err)
	if theirs.ID != ours {
		t.Fatalf("the restored hub gave the colleague's note id %d, the receipt names %d: the reuse did not happen", theirs.ID, ours)
	}
	if out, err := f.cmd(coreOpts{}, "pull", "shop"); err != nil {
		t.Fatal(out, err)
	}
	pg, err := c.SearchNotes(t0, p, project, "checklist")
	f.must(err)
	shown := false
	for _, h := range pg.Hits {
		shown = shown || h.Upstream != "" && h.Title == "Colleague checklist"
	}
	cur, err := c.GetNote(t0, p, project, inv.ID)
	f.must(err)
	_, err = c.UpdateNote(t0, p, project, cur.ID, cur.Revision, core.NoteInput{Class: core.Structure, Title: "Invoice numbering", Body: "edited after the hub's restore"})
	f.must(err)
	dry, err := f.cmd(coreOpts{dryRun: true}, "push", "shop")
	replaces := strings.Contains(dry, fmt.Sprintf("replaces the note %d", ours))
	if !shown || replaces || err != nil {
		t.Fatalf("B: the colleague's note (hub id %d) is shown after the pull: %t; the dry run of an edit replaces it: %t: %q %v", ours, shown, replaces, dry, err)
	}
	if !strings.Contains(dry, "blocked: ") {
		t.Fatalf("the dry run is not blocked: %q", dry)
	}

	// the owner reconciles: our notes are unknown (the hub lost them), --all --new, push: the colleague's note is untouched, ours is new
	out, err := f.cmd(coreOpts{}, "reconcile", "shop")
	if err != nil || !strings.Contains(out, "candidates 0, unknown 2") {
		t.Fatalf("reconcile: %q %v", out, err)
	}
	if out, err = f.cmd(coreOpts{reconcileAll: true, reconcileNew: true}, "reconcile", "shop"); err != nil || !strings.Contains(out, "restored mark cleared: true") {
		t.Fatalf("reconcile --all --new: %q %v", out, err)
	}
	if out, err = f.cmd(coreOpts{}, "push", "shop"); err != nil || !strings.Contains(out, "created 2") || strings.Contains(out, "replace") {
		t.Fatalf("push after reconcile: %q %v", out, err)
	}
	var replaced int
	f.must(rc.DB().QueryRow(`SELECT count(*) FROM kb_submissions WHERE replaces<>0`).Scan(&replaced))
	if replaced != 0 {
		t.Fatalf("%d submissions replace a hub note", replaced)
	}
	f.acceptAll()
	var body string
	var rev int64
	f.must(rc.DB().QueryRow(`SELECT body, revision FROM notes WHERE id=?`, theirs.ID).Scan(&body, &rev))
	if body != "the colleague's own text" || rev != 1 {
		t.Fatalf("the colleague's note changed: %q revision %d", body, rev)
	}
	if n := f.hubNotes(); n != 3 {
		t.Fatalf("the hub has %d notes", n)
	}
}
