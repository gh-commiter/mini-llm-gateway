package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func reconcileFixture(t *testing.T, mode string, key bool) (*fx, int64, UpstreamNote, *hub) {
	f, notes := upFx(t)
	project := f.pid["fakture"]
	f.ok(f.LinkUpstreamWith(ctx, LinkSpec{Project: project, Upstream: "hub", Remote: "shop", Classes: "structure", Mode: mode}))
	n := notes["Structure one"]
	local := UpstreamNote{ID: n.ID, Revision: n.Revision, Class: n.Class, EntryKind: n.EntryKind, Title: n.Title, Body: n.Body, Author: n.Author, Hash: upstreamHash(n)}
	h := &hub{}
	if key {
		secret, e := f.upstreamKey(ctx)
		f.ok(e)
		p := pusher{secret: secret, up: "hub"}
		h.notes = []hubNote{{id: 41, src: 7, rev: 3, class: n.Class, kind: n.EntryKind, title: n.Title, body: n.Body + p.trailer(project, local, 0), device: ownDeviceID}}
	}
	f.markAll()
	return f, project, local, h
}

// Snapshot every application table, including audit and meta. The read-only
// assertions must catch more than an accidentally cleared restore mark.
func reconcileTables(t *testing.T, f *fx) map[string]string {
	t.Helper()
	out := map[string]string{}
	f.ok(f.read(ctx, func(tx *sql.Tx) error {
		rows, e := tx.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
		if e != nil {
			return e
		}
		var names []string
		for rows.Next() {
			var name string
			if e = rows.Scan(&name); e != nil {
				rows.Close()
				return e
			}
			names = append(names, name)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
		for _, name := range names {
			r, e := tx.Query(`SELECT * FROM "` + strings.ReplaceAll(name, `"`, `""`) + `"`)
			if e != nil {
				return e
			}
			cols, e := r.Columns()
			if e != nil {
				r.Close()
				return e
			}
			var all [][]any
			for r.Next() {
				values := make([]any, len(cols))
				ptr := make([]any, len(cols))
				for i := range values {
					ptr[i] = &values[i]
				}
				if e = r.Scan(ptr...); e != nil {
					r.Close()
					return e
				}
				all = append(all, values)
			}
			e = r.Err()
			r.Close()
			if e != nil {
				return e
			}
			b, e := json.Marshal(all)
			if e != nil {
				return e
			}
			out[name] = string(b)
		}
		return nil
	}))
	return out
}

func TestReconcileCandidateEqualAndUnknownBaseConflict(t *testing.T) {
	for _, mode := range []string{"submit", "publish"} {
		for _, different := range []bool{false, true} {
			t.Run(fmt.Sprint(mode, different), func(t *testing.T) {
				f, project, n, h := reconcileFixture(t, mode, true)
				if different {
					h.notes[0].title = "Changed on hub"
				}
				r, e := f.ReconcileUpstream(ctx, h.up(), ReconcileSpec{Project: project})
				f.ok(e)
				if r.Candidates != 1 || r.Unknown != 0 || r.Cleared || r.Notes[0].Remote != 41 {
					t.Fatalf("%+v", r)
				}
				r, e = f.ReconcileUpstream(ctx, h.up(), ReconcileSpec{Project: project, Note: n.ID, Confirm: 41})
				f.ok(e)
				if !r.Cleared || f.marked() != 0 {
					t.Fatal(r)
				}
				var hash, content string
				var rev, localRev, remote int64
				var conflict int
				f.ok(f.DB().QueryRow(`SELECT hash,sent_content,remote_rev,revision,remote_note,conflict FROM upstream_sent WHERE note_id=?`, n.ID).Scan(&hash, &content, &rev, &localRev, &remote, &conflict))
				if localRev != n.Revision || remote != 41 {
					t.Fatal("receipt target or revision lost")
				}
				if different {
					if hash != "" || content != "" || rev != 0 || conflict != 1 {
						t.Fatalf("invented base: %q %q %d %d", hash, content, rev, conflict)
					}
					f.ok(f.ResolveUpstream(ctx, project, 0, n.ID, true, "hub"))
				} else if want := map[string]int64{"submit": 3, "publish": 0}[mode]; hash != n.Hash || content == "" || rev != want || conflict != 0 {
					t.Fatalf("bad receipt: %q %q %d %d", hash, content, rev, conflict)
				}
			})
		}
	}
}

func TestReconcileMissingKeySkipAndNew(t *testing.T) {
	f, project, n, h := reconcileFixture(t, "submit", false)
	r, e := f.ReconcileUpstream(ctx, h.up(), ReconcileSpec{Project: project})
	f.ok(e)
	if r.Unknown != 1 || r.Candidates != 0 {
		t.Fatal(r)
	}
	var keys int
	f.ok(f.DB().QueryRow(`SELECT count(*) FROM upstream_key`).Scan(&keys))
	if keys != 0 {
		t.Fatal("reconcile generated a key")
	}
	_, e = f.ReconcileUpstream(ctx, h.up(), ReconcileSpec{Project: project, All: true, Skip: true, Listed: []int64{n.ID + 99}})
	if e == nil {
		t.Fatal("accepted an unlisted id")
	}
	r, e = f.ReconcileUpstream(ctx, h.up(), ReconcileSpec{Project: project, All: true, Skip: true, Listed: r.Unknowns})
	f.ok(e)
	if !r.Cleared {
		t.Fatal(r)
	}
	f.reopen()
	up := okUp()
	p, e := f.PushUpstream(ctx, up, project, 0, "hub", 0, false, noSleep)
	f.ok(e)
	if up.count("POST") != 0 || p[0].Created != 0 {
		t.Fatal("skip did not survive restart")
	}
	// A later restore still requires a fresh decision; --new clears the skip.
	f.clock = f.clock.Add(time.Hour)
	f.markAll()
	_, e = f.ReconcileUpstream(ctx, h.up(), ReconcileSpec{Project: project})
	f.ok(e)
	_, e = f.ReconcileUpstream(ctx, h.up(), ReconcileSpec{Project: project, Note: n.ID, New: true})
	f.ok(e)
	p, e = f.PushUpstream(ctx, up, project, 0, "hub", 0, false, noSleep)
	f.ok(e)
	if p[0].Created != 1 {
		t.Fatal(p)
	}
}

func TestReconcileExactTrailersAndDestinationOnly(t *testing.T) {
	for _, variant := range []string{"malformed", "author", "copied", "duplicate", "suffix", "dependency", "orphan"} {
		t.Run(variant, func(t *testing.T) {
			f, project, n, h := reconcileFixture(t, "submit", true)
			want := 0
			switch variant {
			case "malformed":
				h.notes[0].body += "extra"
			case "author":
				h.notes[0].body = strings.Replace(h.notes[0].body, ", author ", ", author other ", 1)
			case "copied":
				secret, e := f.upstreamKey(ctx)
				f.ok(e)
				h.notes[0].body = n.Body + (&pusher{secret: secret, up: "hub"}).trailer(project+1, n, 0)
			case "duplicate":
				copy := h.notes[0]
				copy.id = 42
				h.notes = append(h.notes, copy)
			case "suffix":
				h.notes[0].body = strings.TrimSuffix(strings.TrimSuffix(h.notes[0].body, "\n"), " -->") + "; supersedes submission 19 -->"
				want = 1
			case "dependency":
				h.notes[0].src = hubSeg
			case "orphan":
				h.notes[0].body = n.Body + "\n\n" + upstreamRefMark + "abcdefabcdef, author " + trailerAuthor(n.Author) + " -->\n"
			}
			r, e := f.ReconcileUpstream(ctx, h.up(), ReconcileSpec{Project: project})
			f.ok(e)
			if r.Candidates != want || r.Unknown != 1-want || r.Cleared {
				t.Fatal(r)
			}
			if variant == "dependency" && len(h.asked) != 0 {
				t.Fatal("read dependency notes")
			}
			if (variant == "copied" || variant == "orphan") && !strings.Contains(strings.Join(r.Lines, "\n"), "orphan remote 41") {
				t.Fatal("orphan not reported", r)
			}
		})
	}
}

func TestReconcileIncompleteAndDryRunChangeNoTables(t *testing.T) {
	for _, variant := range []string{"dry", "truncated", "forbidden", "partial", "sanitized", "missing-body", "changed-manifest"} {
		t.Run(variant, func(t *testing.T) {
			f, project, _, h := reconcileFixture(t, "submit", true)
			switch variant {
			case "truncated":
				h.truncated = true
			case "forbidden":
				h.proj = 403
			case "partial":
				h.note = func(int64, string) (int, string, time.Duration) { return 403, `{"error":{"message":"forbidden"}}`, 0 }
			case "sanitized":
				h.notes[0].body = "\u200b" + h.notes[0].body
			case "missing-body":
				h.note = func(_ int64, def string) (int, string, time.Duration) {
					var v map[string]any
					json.Unmarshal([]byte(def), &v)
					delete(v, "body")
					b, _ := json.Marshal(v)
					return 200, string(b), 0
				}
			case "changed-manifest":
				calls := 0
				h.manifest = func() (int, string) { calls++; h.notes[0].rev = int64(calls); return 200, h.manifestJSON() }
			}
			before := reconcileTables(t, f)
			_, e := f.ReconcileUpstream(ctx, h.up(), ReconcileSpec{Project: project, DryRun: variant == "dry"})
			if (e == nil) != (variant == "dry") {
				t.Fatalf("%v", e)
			}
			if !reflect.DeepEqual(before, reconcileTables(t, f)) {
				t.Fatal("read changed application tables")
			}
		})
	}
}

func TestReconcileStaleListingAndReadRaceRefused(t *testing.T) {
	for _, race := range []bool{false, true} {
		t.Run(fmt.Sprint(race), func(t *testing.T) {
			f, project, n, h := reconcileFixture(t, "submit", true)
			_, e := f.ReconcileUpstream(ctx, h.up(), ReconcileSpec{Project: project})
			f.ok(e)
			edit := func() {
				f.ok(f.write(ctx, func(tx *sql.Tx) error {
					_, e := tx.Exec(`UPDATE notes SET revision=revision+1,body='new local body' WHERE id=?`, n.ID)
					return e
				}))
			}
			if race {
				h.note = func(_ int64, def string) (int, string, time.Duration) { edit(); return 200, def, 0 }
			} else {
				edit()
			}
			_, e = f.ReconcileUpstream(ctx, h.up(), ReconcileSpec{Project: project, Note: n.ID, Confirm: 41})
			if e == nil {
				t.Fatal("stale decision accepted")
			}
			var receipts int
			f.ok(f.DB().QueryRow(`SELECT count(*) FROM upstream_sent`).Scan(&receipts))
			if receipts != 0 || f.marked() != 1 {
				t.Fatal("stale decision wrote a receipt or cleared the mark")
			}
		})
	}
}

func TestReconcileKeepsSurvivingReceipts(t *testing.T) {
	for _, pending := range []bool{false, true} {
		t.Run(fmt.Sprint(pending), func(t *testing.T) {
			f, project, n, h := reconcileFixture(t, "submit", true)
			if pending {
				h.notes = nil
				f.ok(f.upstreamRecordSent(ctx, project, "hub", n, 9, 0))
				nt, e := f.GetNote(ctx, f.p("ana"), project, n.ID)
				f.ok(e)
				registerSubmitted(t, 9, ourSubmission(f, project, nt)) // the hub still holds that submission, pending
			} else {
				f.ok(f.upstreamRecordPublished(ctx, project, "hub", n, 41, 1))
			}
			var before string
			f.ok(f.DB().QueryRow(`SELECT hash||'|'||revision||'|'||remote_sub||'|'||remote_note||'|'||sent||'|'||remote_rev||'|'||sent_content FROM upstream_sent`).Scan(&before))
			r, e := f.ReconcileUpstream(ctx, h.up(), ReconcileSpec{Project: project})
			f.ok(e)
			if !r.Cleared || r.Notes[0].Class != "known" {
				t.Fatal(r)
			}
			if lines := strings.Join(r.Lines, "\n"); strings.Contains(lines, "remote 0") || pending && !strings.Contains(lines, "sent as a submission") {
				t.Fatalf("a receipt that knows no note number printed a misleading remote id: %q", lines)
			}
			var after string
			f.ok(f.DB().QueryRow(`SELECT hash||'|'||revision||'|'||remote_sub||'|'||remote_note||'|'||sent||'|'||remote_rev||'|'||sent_content FROM upstream_sent`).Scan(&after))
			if before != after {
				t.Fatal("known receipt silently refreshed")
			}
		})
	}
}

// A skip holds across local edits (the push must not create the note) until the owner releases it with --new.
func TestReconcileSkipHoldsAcrossEditsUntilReleased(t *testing.T) {
	for _, edit := range []bool{false, true} {
		t.Run(fmt.Sprint(edit), func(t *testing.T) {
			f, project, n, h := reconcileFixture(t, "submit", false)
			_, e := f.ReconcileUpstream(ctx, h.up(), ReconcileSpec{Project: project})
			f.ok(e)
			_, e = f.ReconcileUpstream(ctx, h.up(), ReconcileSpec{Project: project, Note: n.ID, Skip: true})
			f.ok(e)
			if edit {
				_, e = f.UpdateNote(ctx, f.p("ana"), project, n.ID, n.Revision, NoteInput{Class: n.Class, Title: n.Title, Body: "changed after skip"})
				f.ok(e)
			}
			up := okUp()
			r, e := f.PushUpstream(ctx, up, project, 0, "hub", 0, false, noSleep)
			f.ok(e)
			if up.count("POST") != 0 || r[0].Created != 0 || r[0].Skipped != 1 || !strings.Contains(strings.Join(r[0].Lines, "\n"), "--new") {
				t.Fatal("the skip did not hold (or the push does not say how to release it)", r)
			}
			// listed without a mark, and released by name
			_, e = f.ReconcileUpstream(ctx, h.up(), ReconcileSpec{Project: project})
			f.ok(e)
			_, e = f.ReconcileUpstream(ctx, h.up(), ReconcileSpec{Project: project, Note: n.ID, New: true})
			f.ok(e)
			r, e = f.PushUpstream(ctx, up, project, 0, "hub", 0, false, noSleep)
			f.ok(e)
			if r[0].Created != 1 {
				t.Fatal("skip was not released", r)
			}
		})
	}
}

func TestReconcileCanReplaceACorruptListingAndReportsCounts(t *testing.T) {
	f, project, n, h := reconcileFixture(t, "submit", true)
	f.ok(f.write(ctx, func(tx *sql.Tx) error {
		_, e := tx.Exec(`INSERT INTO meta(key,value) VALUES(?,'not JSON')`, reconcileKey(project, "hub", "listing"))
		return e
	}))
	_, e := f.ReconcileUpstream(ctx, h.up(), ReconcileSpec{Project: project})
	f.ok(e)
	l := f.link(project)
	if l.ReconcileCounts == nil || l.ReconcileCounts.Candidates != 1 || l.ReconcileCounts.Unknown != 0 {
		t.Fatal(l)
	}
	push, e := f.PushUpstream(ctx, h.up(), project, 0, "hub", 0, false, noSleep)
	f.ok(e)
	if !strings.Contains(push[0].Lines[0], "last listing: candidates 1, unknown 0") {
		t.Fatal(push)
	}
	_, e = f.ReconcileUpstream(ctx, h.up(), ReconcileSpec{Project: project, Note: n.ID, Confirm: 41})
	f.ok(e)
	var decisions, links int
	f.ok(f.DB().QueryRow(`SELECT count(*) FROM audit_events WHERE action='upstream.reconcile.confirm'`).Scan(&decisions))
	f.ok(f.DB().QueryRow(`SELECT count(*) FROM audit_events WHERE action='upstream.reconcile'`).Scan(&links))
	if decisions != 1 || links != 2 {
		t.Fatal("missing reconcile audit", decisions, links)
	}
}

func TestReconcileAllCarriesOnlyTheSavedUnknownIds(t *testing.T) {
	f, project, n, h := reconcileFixture(t, "submit", false)
	if _, e := f.ReconcileListedUnknowns(ctx, ReconcileSpec{Project: project}); e == nil {
		t.Fatal("invented a listing")
	}
	_, e := f.ReconcileUpstream(ctx, h.up(), ReconcileSpec{Project: project})
	f.ok(e)
	ids, e := f.ReconcileListedUnknowns(ctx, ReconcileSpec{Project: project})
	f.ok(e)
	if !reflect.DeepEqual(ids, []int64{n.ID}) {
		t.Fatal(ids)
	}
	_, e = f.UpdateNote(ctx, f.p("ana"), project, n.ID, n.Revision, NoteInput{Class: n.Class, Title: n.Title, Body: "changed after listing"})
	f.ok(e)
	before := reconcileTables(t, f)
	_, e = f.ReconcileUpstream(ctx, h.up(), ReconcileSpec{Project: project, All: true, New: true, Listed: ids})
	if e == nil || !reflect.DeepEqual(before, reconcileTables(t, f)) {
		t.Fatal("stale batch was not refused without writes", e)
	}
}

func TestReconcileFailureDuringConfirmationKeepsTheListing(t *testing.T) {
	f, project, n, h := reconcileFixture(t, "submit", true)
	_, e := f.ReconcileUpstream(ctx, h.up(), ReconcileSpec{Project: project})
	f.ok(e)
	before := reconcileTables(t, f)
	h.note = func(int64, string) (int, string, time.Duration) { return 403, `{"error":{"message":"forbidden"}}`, 0 }
	_, e = f.ReconcileUpstream(ctx, h.up(), ReconcileSpec{Project: project, Note: n.ID, Confirm: 41})
	if e == nil || !reflect.DeepEqual(before, reconcileTables(t, f)) {
		t.Fatal("failed confirmation changed tables", e)
	}
}

func TestReconcileTeamLink(t *testing.T) {
	f := newSeg(t)
	f.ok(f.AddUpstream(ctx, "hub", "127.0.0.1:8766"))
	n := f.put("fani", Structure, "Team rollout")
	f.ok(f.LinkUpstream(ctx, 0, f.acme, "hub", "remote", "structure", false))
	key, e := f.upstreamKey(ctx)
	f.ok(e)
	local := UpstreamNote{ID: n.ID, Author: n.Author}
	h := &hub{notes: []hubNote{{id: 41, src: hubSeg, rev: 1, class: n.Class, kind: n.EntryKind, title: n.Title, body: n.Body + (&pusher{secret: key, up: "hub"}).trailer(f.seg, local, 0), device: ownDeviceID}}}
	base := h.up()
	up := &fakeUp{handler: func(c upCall) (int, string, time.Duration) {
		if c.path == "/minigw/v1/teams" {
			return 200, fmt.Sprintf(`[{"name":"remote","segment":%d}]`, hubSeg), 0
		}
		c.path = strings.Replace(c.path, fmt.Sprintf("/projects/%d/", hubSeg), "/projects/7/", 1)
		return base.handler(c)
	}}
	f.markAll()
	r, e := f.ReconcileUpstream(ctx, up, ReconcileSpec{Team: f.acme})
	f.ok(e)
	if r.Candidates != 1 || r.Unknown != 0 {
		t.Fatal(r)
	}
	r, e = f.ReconcileUpstream(ctx, up, ReconcileSpec{Team: f.acme, Note: n.ID, Confirm: 41})
	f.ok(e)
	if !r.Cleared {
		t.Fatal(r)
	}
}

func TestReconcileNeverUsesACachedMirrorAsEvidence(t *testing.T) {
	f, project, _, h := reconcileFixture(t, "submit", true)
	readOn(f, project, "", "")
	_, e := f.PullUpstream(ctx, h.up(), project, 0, "hub", false, noSleep)
	f.ok(e)
	before := reconcileTables(t, f)
	h.proj = 403
	_, e = f.ReconcileUpstream(ctx, h.up(), ReconcileSpec{Project: project})
	if e == nil || !reflect.DeepEqual(before, reconcileTables(t, f)) {
		t.Fatal("used or changed the cached mirror", e)
	}
}

// The brief's hard rules in one run: a candidate is confirmed only with the remote id named, the mark stays while any note is
// undecided and goes with the last decision, and no audit row carries note text.
func TestReconcileConfirmNamesTheRemoteIdKeepsTheMarkUntilAllDecidedAndAuditsNoText(t *testing.T) {
	f, notes := upFx(t)
	project := f.pid["fakture"]
	f.ok(f.LinkUpstreamWith(ctx, LinkSpec{Project: project, Upstream: "hub", Remote: "shop", Classes: "structure,data", Mode: "submit"}))
	secret, e := f.upstreamKey(ctx)
	f.ok(e)
	p := pusher{secret: secret, up: "hub"}
	h := &hub{}
	var ids []int64
	for i, title := range []string{"Structure one", "Data two"} {
		n := notes[title]
		local := UpstreamNote{ID: n.ID, Author: n.Author}
		h.notes = append(h.notes, hubNote{id: int64(41 + i), src: 7, rev: 1, class: n.Class, kind: n.EntryKind, title: n.Title, body: n.Body + p.trailer(project, local, 0), device: ownDeviceID})
		ids = append(ids, n.ID)
	}
	f.markAll()
	r, e := f.ReconcileUpstream(ctx, h.up(), ReconcileSpec{Project: project})
	f.ok(e)
	if r.Candidates != 2 || r.Cleared {
		t.Fatal(r)
	}
	before := reconcileTables(t, f)
	// the wrong remote id, the other note's id, and a decision that does not fit the class are all refused without a write
	for _, bad := range []ReconcileSpec{
		{Project: project, Note: ids[0], Confirm: 99},
		{Project: project, Note: ids[0], Confirm: 42},
		{Project: project, Note: ids[0], All: true, New: true, Listed: ids},
	} {
		if _, e := f.ReconcileUpstream(ctx, h.up(), bad); e == nil {
			t.Fatalf("accepted %+v", bad)
		}
		if !reflect.DeepEqual(before, reconcileTables(t, f)) {
			t.Fatalf("%+v changed tables", bad)
		}
	}
	r, e = f.ReconcileUpstream(ctx, h.up(), ReconcileSpec{Project: project, Note: ids[0], Confirm: 41})
	f.ok(e)
	if r.Cleared || f.marked() != 1 || r.Candidates != 1 {
		t.Fatal("mark cleared with a candidate left", r)
	}
	r, e = f.ReconcileUpstream(ctx, h.up(), ReconcileSpec{Project: project, Note: ids[1], Confirm: 42})
	f.ok(e)
	if !r.Cleared || f.marked() != 0 {
		t.Fatal(r)
	}
	rows, e := f.DB().Query(`SELECT action,detail FROM audit_events WHERE action LIKE 'upstream.reconcile%'`)
	f.ok(e)
	defer rows.Close()
	n := 0
	for rows.Next() {
		var action, detail string
		f.ok(rows.Scan(&action, &detail))
		n++
		for _, title := range []string{"Structure one", "Data two", "body of"} {
			if strings.Contains(detail, title) {
				t.Fatalf("%s audit row carries note text: %s", action, detail)
			}
		}
	}
	if n == 0 {
		t.Fatal("no reconcile audit rows")
	}
}

func TestReconcileRefusesAPartialOrUnstatedView(t *testing.T) {
	none, onlyStructure := []string{}, []string{"structure"}
	for name, set := range map[string]func(h *hub){
		"older upstream, nothing stated": func(h *hub) { h.noFull = true },
		"nothing read in full":           func(h *hub) { h.full = &none },
		"data missing":                   func(h *hub) { h.full = &onlyStructure },
	} {
		t.Run(name, func(t *testing.T) {
			f, project, _, h := reconcileFixture(t, "submit", true)
			f.ok(f.LinkUpstreamWith(ctx, LinkSpec{Project: project, Upstream: "hub", Remote: "shop", Classes: "structure,data", Mode: "submit"}))
			set(h)
			before := reconcileTables(t, f)
			_, e := f.ReconcileUpstream(ctx, h.up(), ReconcileSpec{Project: project})
			if e == nil {
				t.Fatal("accepted a view that may be partial")
			}
			if name == "older upstream, nothing stated" && !strings.Contains(e.Error(), "0.9.3") {
				t.Fatal(e)
			}
			if name == "data missing" && !strings.Contains(e.Error(), "data") {
				t.Fatal(e)
			}
			if !reflect.DeepEqual(before, reconcileTables(t, f)) || f.marked() != 1 {
				t.Fatal("a refused read changed something")
			}
		})
	}
}

// A match without the upstream's proof of its writer is shown as such and may still be bound; the binding is ALWAYS a conflict with
// unknown base (equal content or not, on both modes), so the first push goes through `resolve` and nothing is replaced or created directly.
func TestReconcileUnprovenMatchCanBeBoundOnlyAsAConflictWithUnknownBase(t *testing.T) {
	for _, mode := range []string{"submit", "publish"} {
		for _, device := range []string{"", "UNKNOWN", "dev-someone-else"} {
			t.Run(mode+device, func(t *testing.T) {
				f, project, n, h := reconcileFixture(t, mode, true)
				h.notes[0].device = device
				r, e := f.ReconcileUpstream(ctx, h.up(), ReconcileSpec{Project: project})
				f.ok(e)
				if r.Candidates != 1 || r.Notes[0].Proven || !strings.Contains(strings.Join(r.Lines, "\n"), "no proof: last written by x") {
					t.Fatal(r)
				}
				r, e = f.ReconcileUpstream(ctx, h.up(), ReconcileSpec{Project: project, Note: n.ID, Confirm: 41})
				f.ok(e)
				if !r.Cleared || !strings.Contains(strings.Join(r.Lines, "\n"), "WITHOUT proof") {
					t.Fatal(r)
				}
				var hash string
				var rev int64
				var conflict int
				f.ok(f.DB().QueryRow(`SELECT hash,remote_rev,conflict FROM upstream_sent WHERE note_id=?`, n.ID).Scan(&hash, &rev, &conflict))
				if hash != "" || rev != 0 || conflict != 1 {
					t.Fatalf("not an unknown-base conflict: %q %d %d", hash, rev, conflict)
				}
				_, e = f.UpdateNote(ctx, f.p("ana"), project, n.ID, n.Revision, NoteInput{Class: n.Class, Title: n.Title, Body: "edited after the binding"})
				f.ok(e)
				up := h.up()
				res, e := f.PushUpstream(ctx, up, project, 0, "hub", 0, false, noSleep)
				f.ok(e)
				for _, c := range up.calls {
					if c.method == "PUT" || c.method == "POST" {
						t.Fatalf("wrote to the upstream: %s %s", c.method, c.path)
					}
				}
				if res[0].Conflicts != 1 || res[0].Created != 0 || res[0].Replaced != 0 {
					t.Fatal(res)
				}
				// --ours: a submission for the upstream's reviewers that REPLACES the bound note (never a second note)
				f.ok(f.ResolveUpstream(ctx, project, 0, n.ID, true, "hub"))
				up2 := h.up()
				hubHandler := up2.handler
				up2.handler = func(c upCall) (int, string, time.Duration) {
					if c.method == "POST" {
						return 201, `{"id":9}`, 0
					}
					return hubHandler(c)
				}
				res, e = f.PushUpstream(ctx, up2, project, 0, "hub", 0, false, noSleep)
				f.ok(e)
				if res[0].Created+res[0].Replaced != 1 {
					t.Fatal(res)
				}
				replaced := false
				for _, c := range up2.calls {
					if c.method == "POST" && fmt.Sprint(c.body["replaces"]) == "41" {
						replaced = true
					}
				}
				if !replaced {
					t.Fatal("the submission does not replace the bound note 41 (it would be a second note)", up2.calls)
				}
			})
		}
	}
}

func TestReconcileListingShowsWhatIsConfirmedAndACandidateCanBeRejected(t *testing.T) {
	for _, reject := range []string{"new", "skip"} {
		f, project, n, h := reconcileFixture(t, "submit", true)
		h.notes[0].title = "Changed on hub"
		r, e := f.ReconcileUpstream(ctx, h.up(), ReconcileSpec{Project: project})
		f.ok(e)
		line := strings.Join(r.Lines, "\n")
		for _, want := range []string{"remote 41", `"Changed on hub"`, "written by this gateway's device", "remote revision 3", "differs from the local note"} {
			if !strings.Contains(line, want) {
				t.Fatalf("listing lacks %q: %s", want, line)
			}
		}
		r, e = f.ReconcileUpstream(ctx, h.up(), ReconcileSpec{Project: project, Note: n.ID, New: reject == "new", Skip: reject == "skip"})
		f.ok(e)
		if !r.Cleared || r.Notes[0].Class != reject {
			t.Fatal(reject, r)
		}
	}
}

func TestReconcileConfirmIsBoundToTheListedRemoteVersion(t *testing.T) {
	for _, change := range []string{"revision", "content"} {
		t.Run(change, func(t *testing.T) {
			f, project, n, h := reconcileFixture(t, "submit", true)
			h.notes[0].title = "Different"
			_, e := f.ReconcileUpstream(ctx, h.up(), ReconcileSpec{Project: project})
			f.ok(e)
			if change == "revision" {
				h.notes[0].rev++
			} else {
				h.notes[0].title = n.Title // now equal to the local note, with the same revision
			}
			before := reconcileTables(t, f)
			_, e = f.ReconcileUpstream(ctx, h.up(), ReconcileSpec{Project: project, Note: n.ID, Confirm: 41})
			if e == nil || !strings.Contains(e.Error(), "list again") || !reflect.DeepEqual(before, reconcileTables(t, f)) {
				t.Fatal("confirmed an unseen remote version", e)
			}
		})
	}
}

// The review's sequence: two unknowns, --all --skip clears the mark, then each skip is released by --note --new with no restore mark.
func TestReconcileSkipsAreReleasedOneByOneAfterTheMarkIsGone(t *testing.T) {
	f, notes := upFx(t)
	project := f.pid["fakture"]
	f.ok(f.LinkUpstreamWith(ctx, LinkSpec{Project: project, Upstream: "hub", Remote: "shop", Classes: "structure,data", Mode: "submit"}))
	a, b := notes["Structure one"], notes["Data two"]
	f.markAll()
	h := &hub{}
	r, e := f.ReconcileUpstream(ctx, h.up(), ReconcileSpec{Project: project})
	f.ok(e)
	if r.Unknown != 2 {
		t.Fatal(r)
	}
	r, e = f.ReconcileUpstream(ctx, h.up(), ReconcileSpec{Project: project, All: true, Skip: true, Listed: r.Unknowns})
	f.ok(e)
	if !r.Cleared || f.marked() != 0 {
		t.Fatal(r)
	}
	r, e = f.ReconcileUpstream(ctx, h.up(), ReconcileSpec{Project: project, DryRun: true})
	f.ok(e)
	if len(r.Notes) != 2 || r.Notes[0].Class != "skip" || r.Notes[1].Class != "skip" {
		t.Fatal("skips are not listed without a mark", r)
	}
	for i, id := range []int64{a.ID, b.ID} {
		_, e = f.ReconcileUpstream(ctx, h.up(), ReconcileSpec{Project: project}) // list again, as the CLI tells
		f.ok(e)
		r, e = f.ReconcileUpstream(ctx, h.up(), ReconcileSpec{Project: project, Note: id, New: true})
		f.ok(e)
		released := 0
		for _, row := range r.Notes {
			if row.Note == id && row.Class == "new" {
				released++
			}
		}
		if released != 1 {
			t.Fatal(i, r)
		}
	}
	up := okUp()
	p, e := f.PushUpstream(ctx, up, project, 0, "hub", 0, false, noSleep)
	f.ok(e)
	if p[0].Created != 2 {
		t.Fatal("both released notes should be created", p)
	}
}

// A receipt made by confirmation never lets a publish link overwrite the upstream note directly: the first later change is a conflict.
func TestReconcileConfirmedReceiptDoesNotEnableADirectOverwriteOnPublish(t *testing.T) {
	f, project, n, h := reconcileFixture(t, "publish", true)
	_, e := f.ReconcileUpstream(ctx, h.up(), ReconcileSpec{Project: project})
	f.ok(e)
	_, e = f.ReconcileUpstream(ctx, h.up(), ReconcileSpec{Project: project, Note: n.ID, Confirm: 41})
	f.ok(e)
	_, e = f.UpdateNote(ctx, f.p("ana"), project, n.ID, n.Revision, NoteInput{Class: n.Class, Title: n.Title, Body: "edited after the confirmation"})
	f.ok(e)
	up := h.up()
	res, e := f.PushUpstream(ctx, up, project, 0, "hub", 0, false, noSleep)
	f.ok(e)
	for _, c := range up.calls {
		if c.method == "PUT" || c.method == "POST" {
			t.Fatalf("wrote to the upstream: %s %s", c.method, c.path)
		}
	}
	if res[0].Conflicts != 1 || res[0].Replaced != 0 {
		t.Fatal(res)
	}
}

// A --with-private link: private notes are never in the manifest, so they are "unchecked": --all never covers them, named decisions do.
func TestReconcilePrivateNotesAreUncheckedAndNeedNamedDecisions(t *testing.T) {
	f, notes := upFx(t)
	project := f.pid["fakture"]
	f.ok(f.LinkUpstreamWith(ctx, LinkSpec{Project: project, Upstream: "hub", Remote: "shop", Classes: "structure,data,private", WithPrivate: true, Mode: "submit"}))
	priv := notes["Private three"]
	f.markAll()
	h := &hub{}
	r, e := f.ReconcileUpstream(ctx, h.up(), ReconcileSpec{Project: project})
	f.ok(e)
	if r.Unknown != 2 || r.Unchecked != 1 || !strings.Contains(strings.Join(r.Lines, "\n"), "unchecked 1: private notes are not visible") {
		t.Fatal(r)
	}
	for _, row := range r.Notes {
		if row.Note == priv.ID && (row.Class != "unchecked" || !row.Private) {
			t.Fatal(row)
		}
	}
	// --all with the unknowns decides them but cannot clear the mark; --all naming the private id is refused
	before := reconcileTables(t, f)
	if _, e = f.ReconcileUpstream(ctx, h.up(), ReconcileSpec{Project: project, All: true, New: true, Listed: append(append([]int64{}, r.Unknowns...), priv.ID)}); e == nil || !reflect.DeepEqual(before, reconcileTables(t, f)) {
		t.Fatal("--all covered an unchecked row", e)
	}
	r2, e := f.ReconcileUpstream(ctx, h.up(), ReconcileSpec{Project: project, All: true, New: true, Listed: r.Unknowns})
	f.ok(e)
	if r2.Cleared || f.marked() != 1 || r2.Unchecked != 1 {
		t.Fatal("--all cleared the mark of a link with unchecked private notes", r2)
	}
	r2, e = f.ReconcileUpstream(ctx, h.up(), ReconcileSpec{Project: project, Note: priv.ID, Skip: true})
	f.ok(e)
	if !r2.Cleared || f.marked() != 0 {
		t.Fatal(r2)
	}
}

// A skipped note whose receipt becomes "known" again stays listed as skipped and can be released.
func TestReconcileSkipSurvivesTheNoteBecomingKnown(t *testing.T) {
	f, project, n, h := reconcileFixture(t, "submit", true)
	f.ok(f.upstreamRecordPublished(ctx, project, "hub", n, 41, 3))
	good := h.notes[0].body
	h.notes[0].body += " altered" // the trailer is no longer the exact one: the receipted note is unknown
	r, e := f.ReconcileUpstream(ctx, h.up(), ReconcileSpec{Project: project})
	f.ok(e)
	if r.Unknown != 1 {
		t.Fatal(r)
	}
	r, e = f.ReconcileUpstream(ctx, h.up(), ReconcileSpec{Project: project, Note: n.ID, Skip: true})
	f.ok(e)
	if !r.Cleared {
		t.Fatal(r)
	}
	h.notes[0].body = good // the trailer is back: by the receipt the note is known again
	r, e = f.ReconcileUpstream(ctx, h.up(), ReconcileSpec{Project: project, DryRun: true})
	f.ok(e)
	if len(r.Notes) != 1 || r.Notes[0].Class != "skip" {
		t.Fatal("the skip vanished from the listing", r)
	}
	_, e = f.ReconcileUpstream(ctx, h.up(), ReconcileSpec{Project: project})
	f.ok(e)
	r, e = f.ReconcileUpstream(ctx, h.up(), ReconcileSpec{Project: project, Note: n.ID, New: true})
	f.ok(e)
	if len(r.Notes) != 1 || r.Notes[0].Class != "new" {
		t.Fatal(r)
	}
}

// limitUp wraps a fake upstream: every request takes `delay` (and ends early with the context), and 429s can be injected.
type limitUp struct {
	*fakeUp
	delay time.Duration
	limit int // 429 (retry after 1s) when more than limit requests came since the last wait; 0 = no limit
	count int
	hits  int // the 429 answers given
}

func (u *limitUp) Do(ctx context.Context, up, method, path string, hdr map[string]string, body []byte) (int, []byte, time.Duration, error) {
	if u.delay > 0 {
		t := time.NewTimer(u.delay)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
			return 0, nil, 0, ctx.Err()
		}
	}
	u.count++
	if u.limit > 0 && u.count > u.limit {
		u.hits++
		return 429, []byte(`{"error":{"message":"rate limit"}}`), time.Second, nil
	}
	return u.fakeUp.Do(ctx, up, method, path, hdr, body)
}

func (u *limitUp) resetWindow() { u.count = 0 }

func manyHubNotes(h *hub, n int) {
	for i := 0; i < n; i++ {
		h.notes = append(h.notes, hubNote{id: int64(100 + i), src: 7, rev: 1, class: Structure, kind: KindFinding, title: fmt.Sprint("other ", i), body: "someone else's note"})
	}
}

func setReconcileTimes(t *testing.T, call, base, per time.Duration) {
	oc, ob, op := reconcileCallTimeout, reconcileBaseBudget, reconcilePerNote
	reconcileCallTimeout, reconcileBaseBudget, reconcilePerNote = call, base, per
	t.Cleanup(func() { reconcileCallTimeout, reconcileBaseBudget, reconcilePerNote = oc, ob, op })
}

// The budget of the whole read grows with the number of notes, and an exhausted budget or a slow request is never "the upstream is down".
func TestReconcileTimeBudgetGrowsWithTheNotesAndIsNeverReportedAsDown(t *testing.T) {
	f, project, _, h := reconcileFixture(t, "submit", true)
	manyHubNotes(h, 6)
	// 9 requests of 25 ms (project, manifest, 7 notes, manifest) = 225 ms: a fixed budget of 100 ms fails, one that grows with the 7 notes does not
	setReconcileTimes(t, time.Second, 100*time.Millisecond, 0)
	before := reconcileTables(t, f)
	_, e := f.ReconcileUpstream(ctx, &limitUp{fakeUp: h.up(), delay: 25 * time.Millisecond}, ReconcileSpec{Project: project})
	if e == nil || !strings.Contains(e.Error(), "time budget") || !strings.Contains(e.Error(), "read ") || !strings.Contains(e.Error(), "run it again") || strings.Contains(e.Error(), "no usable answer") {
		t.Fatal("a spent budget must say so, with the progress", e)
	}
	if !reflect.DeepEqual(before, reconcileTables(t, f)) {
		t.Fatal("a spent budget changed something")
	}
	setReconcileTimes(t, time.Second, 100*time.Millisecond, 100*time.Millisecond)
	r, e := f.ReconcileUpstream(ctx, &limitUp{fakeUp: h.up(), delay: 25 * time.Millisecond}, ReconcileSpec{Project: project})
	f.ok(e)
	if r.Candidates != 1 {
		t.Fatal(r)
	}
}

func TestReconcileSlowRequestIsATimeoutNotAnUpstreamFailure(t *testing.T) {
	f, project, _, h := reconcileFixture(t, "submit", true)
	setReconcileTimes(t, 20*time.Millisecond, time.Minute, time.Minute)
	_, e := f.ReconcileUpstream(ctx, &limitUp{fakeUp: h.up(), delay: 300 * time.Millisecond}, ReconcileSpec{Project: project})
	if e == nil || !strings.Contains(e.Error(), "took longer than") || strings.Contains(e.Error(), "no usable answer") || strings.Contains(e.Error(), "did not answer") {
		t.Fatal(e)
	}
}

// A 429 with a retry-after is waited out and the read goes on; the waits are reported; when the wait does not fit the budget the answer says so.
func TestReconcileWaitsOutTheReadLimit(t *testing.T) {
	waits := 0
	var u *limitUp
	old := reconcileWait
	reconcileWait = func(context.Context, time.Duration) bool { waits++; u.resetWindow(); return true }
	t.Cleanup(func() { reconcileWait = old })
	f, project, _, h := reconcileFixture(t, "submit", true)
	manyHubNotes(h, 5)
	u = &limitUp{fakeUp: h.up(), limit: 4}
	r, e := f.ReconcileUpstream(ctx, u, ReconcileSpec{Project: project})
	f.ok(e)
	if waits == 0 || u.hits == 0 || r.Candidates != 1 || !strings.Contains(strings.Join(r.Lines, "\n"), "waited") {
		t.Fatal("did not wait out the limit", waits, u.hits, r)
	}
	// a wait that does not fit the budget: nothing changed, the answer names the limit and the progress
	setReconcileTimes(t, time.Second, 100*time.Millisecond, 0)
	reconcileWait = old
	f2, project2, _, h2 := reconcileFixture(t, "submit", true)
	manyHubNotes(h2, 5)
	before := reconcileTables(t, f2)
	_, e = f2.ReconcileUpstream(ctx, &limitUp{fakeUp: h2.up(), limit: 3}, ReconcileSpec{Project: project2})
	if e == nil || !strings.Contains(e.Error(), "read limit") || !strings.Contains(e.Error(), "run it again in") || !reflect.DeepEqual(before, reconcileTables(t, f2)) {
		t.Fatal(e)
	}
}

// A named decision reads the manifest and the one note it binds, not the whole destination again; a change of the remote notes since the listing refuses it.
func TestReconcileDecisionReadsOnlyTheManifestAndTheTouchedNote(t *testing.T) {
	f, project, n, h := reconcileFixture(t, "submit", true)
	manyHubNotes(h, 8)
	up := &limitUp{fakeUp: h.up()}
	_, e := f.ReconcileUpstream(ctx, up, ReconcileSpec{Project: project})
	f.ok(e)
	// the project is large: at most 3 requests per window would never allow a second full read, a decision needs only the manifest and the note
	up.limit = 3
	up.resetWindow()
	r, e := f.ReconcileUpstream(ctx, up, ReconcileSpec{Project: project, Note: n.ID, Confirm: 41})
	f.ok(e)
	if up.hits != 0 || !r.Cleared {
		t.Fatal("the decision read more than the manifest and the touched note", up.hits, r)
	}
	// a remote change after the listing (a new note, or another note's new revision) refuses the decision with no write
	for _, change := range []string{"new note", "revision"} {
		f, project, n, h := reconcileFixture(t, "submit", true)
		manyHubNotes(h, 3)
		_, e := f.ReconcileUpstream(ctx, h.up(), ReconcileSpec{Project: project})
		f.ok(e)
		if change == "new note" {
			manyHubNotes(&hub{}, 0)
			h.notes = append(h.notes, hubNote{id: 300, src: 7, rev: 1, class: Structure, kind: KindFinding, title: "late", body: "late note"})
		} else {
			h.notes[2].rev++
		}
		before := reconcileTables(t, f)
		_, e = f.ReconcileUpstream(ctx, h.up(), ReconcileSpec{Project: project, Note: n.ID, Confirm: 41})
		if e == nil || !strings.Contains(e.Error(), "list again") || !reflect.DeepEqual(before, reconcileTables(t, f)) {
			t.Fatal(change, "was accepted after a remote change", e)
		}
	}
}

// A note bound WITHOUT the upstream's proof is restricted for good: after --theirs or --ours, every later change goes out as a submission (manual and
// automatic push alike), never as a direct write over a note this gateway never wrote. A device-proven binding is not restricted.
func TestReconcileUnprovenBindingNeverGetsADirectWriteOnAPublishLink(t *testing.T) {
	for _, ours := range []bool{false, true} {
		for _, auto := range []bool{false, true} {
			t.Run(fmt.Sprint("ours=", ours, " auto=", auto), func(t *testing.T) {
				f, project, n, h := reconcileFixture(t, "publish", true)
				h.notes[0].device = "dev-someone-else"
				yes := true
				f.ok(f.LinkUpstreamWith(ctx, LinkSpec{Project: project, Upstream: "hub", Remote: "shop", Classes: "structure", Mode: "publish", Auto: &yes}))
				_, e := f.ReconcileUpstream(ctx, h.up(), ReconcileSpec{Project: project})
				f.ok(e)
				_, e = f.ReconcileUpstream(ctx, h.up(), ReconcileSpec{Project: project, Note: n.ID, Confirm: 41})
				f.ok(e)
				if l := f.link(project); l.Restricted != 1 {
					t.Fatal("the binding is not restricted", l.Restricted)
				}
				f.ok(f.ResolveUpstream(ctx, project, 0, n.ID, ours, "hub"))
				_, e = f.UpdateNote(ctx, f.p("ana"), project, n.ID, n.Revision, NoteInput{Class: n.Class, Title: n.Title, Body: "edited after the resolution"})
				f.ok(e)
				up := h.up()
				hubHandler := up.handler
				up.handler = func(c upCall) (int, string, time.Duration) {
					if c.method == "POST" {
						return 201, `{"id":9}`, 0
					}
					return hubHandler(c)
				}
				if auto {
					f.clock = f.clock.Add(61 * time.Second)
					_, e = f.AutoPush(ctx, up, noSleep)
				} else {
					_, e = f.PushUpstream(ctx, up, project, 0, "hub", 0, false, noSleep)
				}
				f.ok(e)
				posts := 0
				for _, c := range up.calls {
					if c.method == "PUT" {
						t.Fatal("a direct write over a note this gateway never wrote:", c.path)
					}
					if c.method == "POST" {
						posts++
						if fmt.Sprint(c.body["replaces"]) != "41" {
							t.Fatal("the submission does not replace the bound note", c.body)
						}
					}
				}
				if posts != 1 {
					t.Fatal("expected one submission", up.calls)
				}
				if f.link(project).Restricted != 1 {
					t.Fatal("the restriction went away")
				}
			})
		}
	}
	// proven: no restriction
	f, project, n, h := reconcileFixture(t, "publish", true)
	_, e := f.ReconcileUpstream(ctx, h.up(), ReconcileSpec{Project: project})
	f.ok(e)
	_, e = f.ReconcileUpstream(ctx, h.up(), ReconcileSpec{Project: project, Note: n.ID, Confirm: 41})
	f.ok(e)
	if f.link(project).Restricted != 0 {
		t.Fatal("a proven binding was restricted")
	}
}

// blockUp holds the first request until released, so that the test can act on the store while a reconcile read is in flight.
type blockUp struct {
	*fakeUp
	started chan struct{}
	release chan struct{}
	once    bool
}

func (u *blockUp) Do(ctx context.Context, up, method, path string, hdr map[string]string, body []byte) (int, []byte, time.Duration, error) {
	if !u.once {
		u.once = true
		close(u.started)
		<-u.release
	}
	return u.fakeUp.Do(ctx, up, method, path, hdr, body)
}

func (u *blockUp) OwnDevice(string) string { return ownDeviceID }

// The link lock is not held while the upstream is read; what the decision depends on is revalidated when it is applied.
func TestReconcileDoesNotHoldTheLinkLockDuringTheRead(t *testing.T) {
	f, project, n, h := reconcileFixture(t, "submit", true)
	u := &blockUp{fakeUp: h.up(), started: make(chan struct{}), release: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		_, e := f.ReconcileUpstream(ctx, u, ReconcileSpec{Project: project})
		done <- e
	}()
	<-u.started
	mu := f.linkLock(project, "hub")
	if !mu.TryLock() {
		t.Fatal("the link lock is held during the upstream read: a push, pull or unlink would be refused")
	}
	mu.Unlock()
	_, e := f.UpdateNote(ctx, f.p("ana"), project, n.ID, n.Revision, NoteInput{Class: n.Class, Title: n.Title, Body: "edited during the read"})
	f.ok(e)
	close(u.release)
	if e := <-done; e == nil || !strings.Contains(e.Error(), "list again") {
		t.Fatal("a change during the read must refuse with list again", e)
	}
	var listings int
	f.ok(f.DB().QueryRow(`SELECT count(*) FROM meta WHERE key LIKE 'upstream_reconcile|%|listing'`).Scan(&listings))
	if listings != 0 || f.marked() != 1 {
		t.Fatal("a refused reconcile wrote something")
	}
}

func TestReconcileCancelEndsARateLimitWaitAtOnce(t *testing.T) {
	f, project, _, h := reconcileFixture(t, "submit", true)
	cctx, cancel := context.WithCancel(ctx)
	time.AfterFunc(50*time.Millisecond, cancel)
	start := time.Now()
	_, e := f.ReconcileUpstream(cctx, &limitUp{fakeUp: h.up(), limit: 1}, ReconcileSpec{Project: project})
	if e == nil || !strings.Contains(e.Error(), "canceled") || time.Since(start) > 700*time.Millisecond {
		t.Fatal("a cancel must end the wait promptly", time.Since(start), e)
	}
}

func TestReconcileRestrictionGoesWithTheLink(t *testing.T) {
	f, project, n, h := reconcileFixture(t, "publish", true)
	h.notes[0].device = "dev-someone-else"
	_, e := f.ReconcileUpstream(ctx, h.up(), ReconcileSpec{Project: project})
	f.ok(e)
	_, e = f.ReconcileUpstream(ctx, h.up(), ReconcileSpec{Project: project, Note: n.ID, Confirm: 41})
	f.ok(e)
	var keys int
	f.ok(f.DB().QueryRow(`SELECT count(*) FROM meta WHERE key LIKE 'upstream_reconcile|%|restricted|%'`).Scan(&keys))
	if keys != 1 {
		t.Fatal("no restriction recorded", keys)
	}
	f.ok(f.UnlinkUpstream(ctx, project, 0, "hub"))
	f.ok(f.DB().QueryRow(`SELECT count(*) FROM meta WHERE key LIKE 'upstream_reconcile|%'`).Scan(&keys))
	if keys != 0 {
		t.Fatal("reconcile keys survived the unlink", keys)
	}
}
