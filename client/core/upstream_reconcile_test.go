package core

import (
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
			var after string
			f.ok(f.DB().QueryRow(`SELECT hash||'|'||revision||'|'||remote_sub||'|'||remote_note||'|'||sent||'|'||remote_rev||'|'||sent_content FROM upstream_sent`).Scan(&after))
			if before != after {
				t.Fatal("known receipt silently refreshed")
			}
		})
	}
}

func TestReconcileSkipCanBeClearedOrReleasedByAnEdit(t *testing.T) {
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
			} else {
				// The last decision cleared the restore mark; --new must still
				// let the owner release that exact skipped revision.
				_, e = f.ReconcileUpstream(ctx, h.up(), ReconcileSpec{Project: project, Note: n.ID, New: true})
				f.ok(e)
			}
			up := okUp()
			r, e := f.PushUpstream(ctx, up, project, 0, "hub", 0, false, noSleep)
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

func TestReconcileAMatchNeedsTheUpstreamsWordForItsWriter(t *testing.T) {
	for _, device := range []string{"", "UNKNOWN", "dev-someone-else"} {
		f, project, n, h := reconcileFixture(t, "publish", true)
		h.notes[0].device = device
		r, e := f.ReconcileUpstream(ctx, h.up(), ReconcileSpec{Project: project})
		f.ok(e)
		if r.Candidates != 0 || r.Unknown != 1 || r.Notes[0].Matches != "41" {
			t.Fatal(device, r)
		}
		if !strings.Contains(strings.Join(r.Lines, "\n"), "information only): remote 41") {
			t.Fatal(r.Lines)
		}
		before := reconcileTables(t, f)
		if _, e = f.ReconcileUpstream(ctx, h.up(), ReconcileSpec{Project: project, Note: n.ID, Confirm: 41}); e == nil || !reflect.DeepEqual(before, reconcileTables(t, f)) {
			t.Fatal("confirmed an unattested match", e)
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
		for _, want := range []string{"remote 41", `"Changed on hub"`, "written by x", "remote revision 3", "differs from the local note"} {
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
