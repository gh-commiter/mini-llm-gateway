package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"minigw/core"
)

// Two real gateways: the lost receipt is recovered as a confirmed target,
// and the edited local version replaces that target through hub review.
func TestUpstreamReconcileAfterRestoreBetweenTwoGateways(t *testing.T) {
	f := newUpFx(t, core.Developer)
	f.add("hub")
	f.allow()
	p, project, notes := f.seed()
	_, err := f.cmd(coreOpts{class: "structure"}, "link", "shop", "hub")
	f.must(err)
	// A backup can retain the reference key while losing a later receipt.
	_, err = f.cmd(coreOpts{dryRun: true}, "push", "shop")
	f.must(err)
	bak := filepath.Join(t.TempDir(), "before-receipt.db")
	f.must(f.local.core.Backup(t0, bak))
	_, err = f.cmd(coreOpts{}, "push", "shop")
	f.must(err)
	rc := f.remote.core
	_, err = rc.ReviewSubmission(t0, 1, true, "", seenHash(rc, 1))
	f.must(err)
	var remote int64
	f.must(rc.DB().QueryRow(`SELECT note_id FROM kb_submissions WHERE id=1`).Scan(&remote))
	dir := f.local.core.Dir()
	f.local.closeCore()
	f.must(core.Restore(dir, bak))
	f.local.openCore()
	if f.local.core == nil {
		t.Fatal(f.local.coreErr)
	}
	n := notes["Invoice numbering"]
	_, err = f.local.core.UpdateNote(t0, p, project, n.ID, n.Revision, core.NoteInput{Class: n.Class, Title: n.Title, Body: "edited after restore"})
	f.must(err)
	assertOne := func() {
		t.Helper()
		var count int
		f.must(rc.DB().QueryRow(`SELECT count(*) FROM notes WHERE project_id=?`, f.rproj).Scan(&count))
		if count != 1 {
			t.Fatalf("hub has %d notes", count)
		}
	}
	assertOne()
	_, err = f.cmd(coreOpts{}, "push", "shop")
	f.must(err)
	// Give the real automatic lane several opportunities while marked.
	f.local.core.SetAutoTiming(core.AutoTiming{Debounce: 100 * time.Millisecond, MaxWait: time.Second, BackoffBase: 100 * time.Millisecond, BackoffMax: 500 * time.Millisecond})
	fastAutoTick(f.local)
	_, err = f.cmd(coreOpts{auto: true, class: "structure"}, "link", "shop", "hub")
	f.must(err)
	time.Sleep(1500 * time.Millisecond)
	if count, _ := f.remoteSubmissions(); count != 1 {
		t.Fatal("marked push created a submission")
	}
	assertOne()
	// Stop automatic sending before clearing the mark, so the assertions below
	// observe the manual replacement and its review deterministically.
	_, err = f.cmd(coreOpts{manual: true, class: "structure"}, "link", "shop", "hub")
	f.must(err)
	out, err := f.cmd(coreOpts{}, "reconcile", "shop")
	f.must(err)
	if !strings.Contains(out, "candidate note "+fmt.Sprint(n.ID)) {
		t.Fatal(out)
	}
	out, err = f.cmd(coreOpts{reconcileNote: fmt.Sprint(n.ID), reconcileConfirm: fmt.Sprint(remote)}, "reconcile", "shop")
	f.must(err)
	if !strings.Contains(out, "restored mark cleared: true") {
		t.Fatal(out)
	}
	var base, conflict int
	f.must(f.local.core.DB().QueryRow(`SELECT remote_rev,conflict FROM upstream_sent WHERE note_id=?`, n.ID).Scan(&base, &conflict))
	if base != 0 || conflict != 1 {
		t.Fatal("reconcile invented a last-sent base", base, conflict)
	}
	_, err = f.cmd(coreOpts{ours: true}, "resolve", "shop", fmt.Sprint(n.ID))
	f.must(err)
	_, err = f.cmd(coreOpts{}, "push", "shop")
	f.must(err)
	var replacement int64
	f.must(rc.DB().QueryRow(`SELECT replaces FROM kb_submissions WHERE id=2`).Scan(&replacement))
	if replacement != remote {
		t.Fatal("replacement lost its confirmed target")
	}
	_, err = rc.ReviewSubmission(t0, 2, true, "", seenHash(rc, 2))
	f.must(err)
	assertOne()
	var body string
	f.must(rc.DB().QueryRow(`SELECT body FROM notes WHERE id=?`, remote).Scan(&body))
	if !strings.HasPrefix(body, "edited after restore\n\n") {
		t.Fatal("replacement content not accepted")
	}
}
