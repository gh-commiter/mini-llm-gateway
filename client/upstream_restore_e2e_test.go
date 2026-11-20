package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"minigw/core"
)

// TestUpstreamPushAfterARestoreBetweenTwoGateways (#167): two real gateways. The local database is backed up before its first push (it
// then holds no receipt), the push is accepted on the hub, the backup is restored, and a note is edited. The restored link is marked: the
// manual push, the dry run and the automatic push create nothing on the hub (no submission, no note), status shows the state, and only the owner's
// override lets the push go again.
func TestUpstreamPushAfterARestoreBetweenTwoGateways(t *testing.T) {
	f := newUpFx(t, core.Developer)
	f.add("hub")
	f.allow()
	p, project, notes := f.seed()
	f.must(func() error { _, err := f.cmd(coreOpts{}, "link", "shop", "hub"); return err }())
	bak := filepath.Join(t.TempDir(), "before-the-first-push.db")
	f.must(f.local.core.Backup(t0, bak))
	if out, err := f.cmd(coreOpts{}, "push", "shop"); err != nil || strings.Contains(out, "blocked") {
		t.Fatal(out, err)
	}
	rc := f.remote.core
	for id := int64(1); id <= 2; id++ {
		if _, err := rc.ReviewSubmission(t0, id, true, "", seenHash(rc, id)); err != nil {
			t.Fatal(err)
		}
	}
	hubCounts := func() (subs, hubNotes int) {
		t.Helper()
		subs, _ = f.remoteSubmissions()
		f.must(rc.DB().QueryRow(`SELECT count(*) FROM notes WHERE project_id=?`, f.rproj).Scan(&hubNotes))
		return
	}
	subs0, notes0 := hubCounts()
	if subs0 != 2 || notes0 != 2 {
		t.Fatalf("%d submissions, %d notes on the hub", subs0, notes0)
	}

	// the owner restores the old database (the gateway is stopped meanwhile) and edits a note that the hub already has
	dir := f.local.core.Dir()
	f.local.closeCore()
	f.must(core.Restore(dir, bak))
	f.local.openCore()
	if f.local.core == nil {
		t.Fatal("the restored database did not open:", f.local.coreErr)
	}
	cur, err := f.local.core.GetNote(t0, p, project, notes["Invoice numbering"].ID)
	f.must(err)
	_, err = f.local.core.UpdateNote(t0, p, project, cur.ID, cur.Revision, core.NoteInput{Class: core.Structure, Title: "Invoice numbering", Body: "edited after the restore"})
	f.must(err)

	nothingNew := func(what string) {
		t.Helper()
		if s, n := hubCounts(); s != subs0 || n != notes0 {
			t.Fatalf("%s: the hub has %d submissions and %d notes, was %d and %d", what, s, n, subs0, notes0)
		}
	}
	for _, args := range [][]string{{"push", "shop"}, {"push", "shop", "--dry-run"}} {
		o := coreOpts{dryRun: len(args) == 3}
		out, err := f.cmd(o, args[:2]...)
		if err != nil || !strings.Contains(out, "blocked: restored: reconcile before pushing") {
			t.Fatalf("%v: %q %v", args, out, err)
		}
		if !o.dryRun && !strings.Contains(out, "created 0, replaced 0") {
			t.Fatal(out)
		}
		nothingNew(strings.Join(args, " "))
	}
	if st, err := f.cmd(coreOpts{}, "status", "shop"); err != nil || !strings.Contains(st, "restored: reconcile before pushing (since ") {
		t.Fatalf("%q %v", st, err)
	}

	// automatic: the link is switched to auto (an ordinary edit: it keeps the mark); the timer must send nothing, however many times it looks
	f.local.core.SetAutoTiming(core.AutoTiming{Debounce: 100 * time.Millisecond, MaxWait: time.Second, BackoffBase: 100 * time.Millisecond, BackoffMax: 500 * time.Millisecond})
	f.local.autoTick.Store(int64(30 * time.Millisecond))
	if _, err := f.cmd(coreOpts{auto: true}, "link", "shop", "hub"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1500 * time.Millisecond)
	nothingNew("automatic push")
	st, _ := f.cmd(coreOpts{}, "status", "shop")
	if !strings.Contains(st, "restored: reconcile before pushing") || strings.Contains(st, "last failure") {
		t.Fatalf("status after the timer: %s", st)
	}

	// the override: refused without a confirmation, then the push is no longer blocked (it may duplicate: only that is asserted)
	if out, err := f.cmd(coreOpts{afterRestore: true}, "push", "shop"); err == nil || strings.Contains(out, "created 1") {
		t.Fatalf("the override without a terminal or --yes: %q %v", out, err)
	}
	if st, _ := f.cmd(coreOpts{}, "status", "shop"); !strings.Contains(st, "restored: reconcile") {
		t.Fatal("a refused override cleared the mark")
	}
	out, err := f.cmd(coreOpts{afterRestore: true, yes: true}, "push", "shop")
	if err != nil || strings.Contains(out, "blocked:") || !strings.Contains(out, "second copies") {
		t.Fatalf("%q %v", out, err)
	}
	if st, _ := f.cmd(coreOpts{}, "status", "shop"); strings.Contains(st, "restored: reconcile") {
		t.Fatalf("the mark is still there: %s", st)
	}
	var rows int
	f.must(f.local.core.DB().QueryRow(`SELECT count(*) FROM audit_events WHERE action='upstream.after-restore'`).Scan(&rows))
	if rows != 1 {
		t.Fatalf("%d audit rows for the override", rows)
	}
}
