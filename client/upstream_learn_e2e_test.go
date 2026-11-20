package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"minigw/core"
)

// TestUpstreamPullLearnsAnAcceptedSubmissionAndTheCopyShowsOnce: two real gateways (127.0.0.1 only). A note is submitted, the hub accepts it with NO
// change of the note here and no further push, and the next pull makes the member's search show the note once (the local one): the pull learns the
// hub's note id from the submission before the hub copy becomes visible. A hub note that copies our exact trailer but has no receipt is shown. The
// "restore" way loses the receipt (a backup from before the push): `gw upstream reconcile` confirms the candidate and the hub copy is hidden again.
func TestUpstreamPullLearnsAnAcceptedSubmissionAndTheCopyShowsOnce(t *testing.T) {
	for _, way := range []string{"plain", "restore"} {
		t.Run(way, func(t *testing.T) {
			f := newUpFx(t, core.Developer)
			f.add("hub")
			f.allow()
			p, project, notes := f.seed()
			n := notes["Invoice numbering"]
			_, err := f.cmd(coreOpts{class: "structure"}, "link", "shop", "hub")
			f.must(err)
			_, err = f.cmd(coreOpts{dryRun: true}, "push", "shop") // makes the reference key, which a backup then retains
			f.must(err)
			bak := filepath.Join(t.TempDir(), "before-push.db")
			f.must(f.local.core.Backup(t0, bak))
			out, err := f.cmd(coreOpts{}, "push", "shop")
			f.must(err)
			if !strings.Contains(out, "created 1") {
				t.Fatal(out)
			}
			rc := f.remote.core
			_, err = rc.ReviewSubmission(t0, 1, true, "", seenHash(rc, 1))
			f.must(err)
			if way == "restore" {
				dir := f.local.core.Dir()
				f.local.closeCore()
				f.must(core.Restore(dir, bak))
				f.local.openCore()
				if f.local.core == nil {
					t.Fatal(f.local.coreErr)
				}
				var remote int64
				f.must(rc.DB().QueryRow(`SELECT note_id FROM kb_submissions WHERE id=1`).Scan(&remote))
				if out, err = f.cmd(coreOpts{}, "reconcile", "shop"); err != nil || !strings.Contains(out, "candidate note "+fmt.Sprint(n.ID)) {
					t.Fatal(out, err)
				}
				out, err = f.cmd(coreOpts{reconcileNote: fmt.Sprint(n.ID), reconcileConfirm: fmt.Sprint(remote)}, "reconcile", "shop")
				f.must(err)
				if !strings.Contains(out, "restored mark cleared: true") {
					t.Fatal(out)
				}
			}
			c := f.local.core
			hits := func() (local, hub []core.SearchHit) {
				t.Helper()
				pg, err := c.SearchNotes(t0, p, project, "invoice")
				f.must(err)
				for _, h := range pg.Hits {
					if h.Upstream != "" {
						hub = append(hub, h)
					} else {
						local = append(local, h)
					}
				}
				return
			}
			_, err = f.cmd(coreOpts{read: true, acceptReach: true}, "link", "shop", "hub")
			f.must(err)
			if out, err = f.cmd(coreOpts{}, "pull", "shop"); err != nil || !strings.Contains(out, "mirror 1 notes") {
				t.Fatalf("%q %v", out, err)
			}
			if l, h := hits(); len(l) != 1 || l[0].ID != n.ID || len(h) != 0 {
				t.Fatalf("the accepted note must show once: local %+v hub %+v", l, h)
			}
			var sub, note, rev int64
			f.must(c.DB().QueryRow(`SELECT remote_sub,remote_note,remote_rev FROM upstream_sent WHERE note_id=?`, n.ID).Scan(&sub, &note, &rev))
			if note == 0 {
				t.Fatal("the receipt does not name the hub's note", sub, note, rev)
			}
			// a hub member copies our trailer onto a note of their own: no receipt names it, so it is shown
			var body string
			f.must(rc.DB().QueryRow(`SELECT body FROM notes WHERE id=?`, note).Scan(&body))
			i := strings.LastIndex(body, "\n\n")
			if i < 0 {
				t.Fatalf("the hub note has no trailer: %q", body)
			}
			rp, err := rc.Principal(t0, f.localDev, true)
			f.must(err)
			_, _, err = rc.CreateNote(t0, rp, f.rproj, "copied-trailer", core.NoteInput{Class: core.Structure, Title: "Invoice numbering by a member", Body: "their words" + body[i:]})
			f.must(err)
			if out, err = f.cmd(coreOpts{}, "pull", "shop"); err != nil || !strings.Contains(out, "mirror 2 notes") {
				t.Fatalf("%q %v", out, err)
			}
			if l, h := hits(); len(l) != 1 || len(h) != 1 {
				t.Fatalf("the member's copy of the trailer must be shown, ours hidden: local %+v hub %+v", l, h)
			}
		})
	}
}

// A note accepted on the hub with no change here: the receipt is learned by the pull (the submit-mode default), also when the push never ran again.
func TestUpstreamPullLearningNeedsNoPush(t *testing.T) {
	f := newUpFx(t, core.Developer)
	f.add("hub")
	f.allow()
	_, _, notes := f.seed()
	n := notes["Invoice numbering"]
	_, err := f.cmd(coreOpts{class: "structure", read: true, acceptReach: true}, "link", "shop", "hub")
	f.must(err)
	_, err = f.cmd(coreOpts{}, "push", "shop")
	f.must(err)
	if out, err := f.cmd(coreOpts{}, "pull", "shop"); err != nil || !strings.Contains(out, "mirror 0 notes") {
		t.Fatalf("%q %v", out, err)
	}
	rc := f.remote.core
	_, err = rc.ReviewSubmission(t0, 1, true, "", seenHash(rc, 1))
	f.must(err)
	subs, _ := f.remoteSubmissions()
	if out, err := f.cmd(coreOpts{}, "pull", "shop"); err != nil || !strings.Contains(out, "mirror 1 notes") {
		t.Fatalf("%q %v", out, err)
	}
	var note int64
	f.must(f.local.core.DB().QueryRow(`SELECT remote_note FROM upstream_sent WHERE note_id=?`, n.ID).Scan(&note))
	if note == 0 {
		t.Fatal("not learned")
	}
	if now, _ := f.remoteSubmissions(); now != subs {
		t.Fatal("the pull submitted something")
	}
}
