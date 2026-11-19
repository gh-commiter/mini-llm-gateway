package main

import (
	"fmt"
	"strings"
	"testing"

	"minigw/core"
)

// TestUpstreamRejectedReplacementBetweenTwoGateways (#166): two real gateways. A pushed update of a note the hub accepted is a replacement;
// when the hub does not take it (rejected, or left pending after a stale accept), the next push is again a replacement of the same hub
// note, and accepting it leaves ONE note holding the pushed text with the old text in its history.
func TestUpstreamRejectedReplacementBetweenTwoGateways(t *testing.T) {
	for _, variant := range []string{"rejected", "stale-accept-pending"} {
		t.Run(variant, func(t *testing.T) {
			f := newUpFx(t, core.Developer)
			f.add("hub")
			f.allow()
			p, project, notes := f.seed()
			f.must(func() error { _, err := f.cmd(coreOpts{}, "link", "shop", "hub"); return err }())
			if _, err := f.cmd(coreOpts{}, "push", "shop"); err != nil {
				t.Fatal(err)
			}
			rc := f.remote.core
			hubUser, err := rc.Principal(t0, f.localDev, true)
			f.must(err)
			if _, err = rc.ReviewSubmission(t0, 1, true, "", seenHash(rc, 1)); err != nil { // the reviewers accept the first note
				t.Fatal(err)
			}
			var hubNote int64
			f.must(rc.DB().QueryRow(`SELECT note_id FROM kb_submissions WHERE id=1`).Scan(&hubNote))
			hubEdit := func(body string) {
				t.Helper()
				cur, err := rc.GetNote(t0, hubUser, f.rproj, hubNote)
				f.must(err)
				_, err = rc.UpdateNote(t0, hubUser, f.rproj, hubNote, cur.Revision, core.NoteInput{Class: core.Structure, Title: "Invoice numbering", Body: body})
				f.must(err)
			}
			localEdit := func(body string) {
				t.Helper()
				cur, err := f.local.core.GetNote(t0, p, project, notes["Invoice numbering"].ID)
				f.must(err)
				_, err = f.local.core.UpdateNote(t0, p, project, cur.ID, cur.Revision, core.NoteInput{Class: core.Structure, Title: "Invoice numbering", Body: body})
				f.must(err)
			}
			lastSub := func() (id, replaces, revision int64, state, body string) {
				t.Helper()
				f.must(rc.DB().QueryRow(`SELECT id,replaces,revision,state,body FROM kb_submissions ORDER BY id DESC LIMIT 1`).Scan(&id, &replaces, &revision, &state, &body))
				return
			}
			hubEdit("edited on the hub")
			localEdit("local two") // a replacement, as the hub's note has a newer revision than the one the first push made
			if out, _ := f.cmd(coreOpts{}, "push", "shop"); !strings.Contains(out, "replaces the accepted note") {
				t.Fatal(out)
			}
			id, rep, rev, _, _ := lastSub()
			hubNow, err := rc.GetNote(t0, hubUser, f.rproj, hubNote)
			f.must(err)
			if rep != hubNote || rev != hubNow.Revision {
				t.Fatalf("first replacement: replaces %d revision %d, hub note %d at %d", rep, rev, hubNote, hubNow.Revision)
			}
			switch variant {
			case "rejected":
				hubEdit("edited on the hub again")
				if _, err = rc.ReviewSubmission(t0, id, false, "note changed after this was written", seenHash(rc, id)); err != nil {
					t.Fatal(err)
				}
			default: // the hub changes the note, a reviewer's accept is stale and fails: the submission stays pending
				hubEdit("edited on the hub again")
				if _, err = rc.ReviewSubmission(t0, id, true, "", seenHash(rc, id)); err == nil {
					t.Fatal("a stale accept went through")
				}
				if _, _, _, st, _ := lastSub(); st != "pending" {
					t.Fatal("not pending after the stale accept:", st)
				}
			}
			before, _ := f.remoteSubmissions()
			if out, _ := f.cmd(coreOpts{}, "push", "shop"); strings.Contains(out, "replaced 1") { // nothing changed here: nothing is sent again
				t.Fatal(out)
			}
			if n, _ := f.remoteSubmissions(); n != before {
				t.Fatal("sent again with no change here")
			}
			localEdit("local three")
			out, _ := f.cmd(coreOpts{}, "push", "shop")
			if !strings.Contains(out, fmt.Sprintf("replaces the note %d upstream", hubNote)) || !strings.Contains(out, "replaced 1") {
				t.Fatal(out)
			}
			if variant == "rejected" && !strings.Contains(out, "rejected upstream (note changed after this was written)") {
				t.Fatal(out)
			}
			id3, rep, rev, _, body := lastSub()
			hubNow, err = rc.GetNote(t0, hubUser, f.rproj, hubNote)
			f.must(err)
			if rep != hubNote || rev != hubNow.Revision || !strings.Contains(body, "local three") {
				t.Fatalf("second replacement: replaces %d (want %d) revision %d (want %d) %q", rep, hubNote, rev, hubNow.Revision, body)
			}
			if _, err = rc.ReviewSubmission(t0, id3, true, "", seenHash(rc, id3)); err != nil {
				t.Fatal(err)
			}
			var count int
			f.must(rc.DB().QueryRow(`SELECT count(*) FROM notes WHERE project_id=? AND title='Invoice numbering'`, f.rproj).Scan(&count))
			cur, err := rc.GetNote(t0, hubUser, f.rproj, hubNote)
			f.must(err)
			if count != 1 || !strings.Contains(cur.Body, "local three") {
				t.Fatalf("%d notes on the hub, text %q", count, cur.Body)
			}
			v, err := rc.GetNoteVersion(t0, nil, f.rproj, hubNote, cur.Revision-1) // the version the replacement overwrote
			f.must(err)
			if v.Body == nil || !strings.Contains(*v.Body, "edited on the hub again") {
				t.Fatalf("the history lacks the previous text: %+v", v)
			}
		})
	}
}
