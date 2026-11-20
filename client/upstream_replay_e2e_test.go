package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"minigw/core"
)

// TestUpstreamReplayedSubmissionIsNotCreate: two real gateways. The hub answers a POST that repeats an earlier Idempotency-Key (same note, same
// text, same link: the key is a MAC of exactly that) with the EARLIER submission, status 201 and the header Idempotent-Replay, in whatever state
// it is now. A push that sends the identical text again after the receipt was lost (a restore, or unlink and link again) must not report that
// as "create" when the earlier submission was rejected or accepted: nothing new reached the hub's reviewers.
func TestUpstreamReplayedSubmissionIsNotCreate(t *testing.T) {
	for _, way := range []string{"restore", "relink"} {
		for _, outcome := range []string{"rejected", "accepted"} {
			t.Run(way+"-"+outcome, func(t *testing.T) {
				f := newUpFx(t, core.Developer)
				f.add("hub")
				f.allow()
				_, _, notes := f.seed()
				n := notes["Invoice numbering"]
				_, err := f.cmd(coreOpts{class: "structure"}, "link", "shop", "hub")
				f.must(err)
				_, err = f.cmd(coreOpts{dryRun: true}, "push", "shop") // makes the reference key, which a backup then retains (as in the reconcile test)
				f.must(err)
				bak := filepath.Join(t.TempDir(), "before-push.db")
				f.must(f.local.core.Backup(t0, bak))
				out, err := f.cmd(coreOpts{}, "push", "shop")
				f.must(err)
				if !strings.Contains(out, "created 1") {
					t.Fatal(out)
				}
				rc := f.remote.core
				if outcome == "rejected" {
					_, err = rc.ReviewSubmission(t0, 1, false, "not wanted", seenHash(rc, 1))
				} else {
					_, err = rc.ReviewSubmission(t0, 1, true, "", seenHash(rc, 1))
				}
				f.must(err)
				// what the hub answers to a repeated key: the same submission, in its present state
				hubUser, err := rc.Principal(t0, f.localDev, true)
				f.must(err)
				in := core.SubmissionInput{NoteInput: core.NoteInput{Class: core.Structure, Title: "probe", Body: "probe"}}
				first, _, err := rc.Submit(t0, hubUser, f.rproj, "probe-key", in)
				f.must(err)
				f.must(func() error {
					_, e := rc.ReviewSubmission(t0, first.ID, false, "probe reason", seenHash(rc, first.ID))
					return e
				}())
				again, replay, err := rc.Submit(t0, hubUser, f.rproj, "probe-key", in)
				f.must(err)
				raw, _ := json.Marshal(again)
				t.Logf("repeated key: replay=%v id %d (first %d) %s", replay, again.ID, first.ID, raw)
				if !replay || again.ID != first.ID || again.State != "rejected" {
					t.Fatal("the hub does not answer a repeated key with the earlier rejected submission")
				}
				switch way {
				case "restore":
					dir := f.local.core.Dir()
					f.local.closeCore()
					f.must(core.Restore(dir, bak))
					f.local.openCore()
					if f.local.core == nil {
						t.Fatal(f.local.coreErr)
					}
					if _, err = f.cmd(coreOpts{}, "reconcile", "shop"); err != nil {
						t.Fatal(err)
					}
					out, err = f.cmd(coreOpts{reconcileNote: fmt.Sprint(n.ID), reconcileNew: true}, "reconcile", "shop")
					f.must(err)
					if !strings.Contains(out, "restored mark cleared: true") {
						t.Fatal(out)
					}
				default:
					_, err = f.cmd(coreOpts{}, "unlink", "shop", "hub")
					f.must(err)
					_, err = f.cmd(coreOpts{class: "structure"}, "link", "shop", "hub")
					f.must(err)
				}
				subs, _ := f.remoteSubmissions()
				out, err = f.cmd(coreOpts{}, "push", "shop")
				f.must(err)
				t.Logf("push after %s of a %s submission:\n%s", way, outcome, out)
				if now, _ := f.remoteSubmissions(); now != subs {
					t.Fatalf("the hub got a new submission (%d, was %d): the key was not replayed", now, subs)
				}
				var sub, note int64
				f.must(f.local.core.DB().QueryRow(`SELECT remote_sub,remote_note FROM upstream_sent WHERE note_id=?`, n.ID).Scan(&sub, &note))
				t.Logf("receipt: remote_sub %d remote_note %d", sub, note)
				// the report: never "create", never counted as created
				if strings.Contains(out, "create ") || strings.Contains(out, "created 1") {
					t.Errorf("a replayed %s submission is reported as created:\n%s", outcome, out)
				}
			})
		}
	}
}
