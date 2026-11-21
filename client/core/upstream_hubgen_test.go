package core

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The hub's generation: an upstream restored from a backup hands out a new one, and a linked gateway marks every link of that upstream.

// genUp: okUp whose project answer carries *gen ("" = an older upstream that sends none).
func genUp(gen *string) *fakeUp {
	inner := okUp()
	return &fakeUp{handler: func(c upCall) (int, string, time.Duration) {
		if c.method == "GET" && c.path == "/minigw/v1/projects/shop" && *gen != "" {
			return 200, fmt.Sprintf(`{"id":7,"slug":"shop","generation":%q}`, *gen), 0
		}
		return inner.handler(c)
	}}
}

const genA, genB, genC = "aaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbb", "cccccccccccccccc"

func storedGen(f *fx) (g string) {
	f.t.Helper()
	_ = f.DB().QueryRow(`SELECT value FROM meta WHERE key=?`, hubSeenPrefix+"hub").Scan(&g)
	return
}

func hubRestoredAudits(f *fx) (n int, details string) {
	f.t.Helper()
	f.ok(f.DB().QueryRow(`SELECT count(*), coalesce(group_concat(detail,' '),'') FROM audit_events WHERE action='upstream.hub-restored'`).Scan(&n, &details))
	return
}

func push1(f *fx, up UpstreamTransport, project int64, dry bool) UpstreamPush {
	f.t.Helper()
	r, e := f.PushUpstream(ctx, up, project, 0, "hub", 0, dry, noSleep)
	f.ok(e)
	return r[0]
}

// First contact stores the generation and changes nothing else; an older upstream (no generation) is not checked at all.
func TestHubGenerationFirstContactAndOlderUpstream(t *testing.T) {
	for _, gen := range []string{genA, "", "NOT-HEX-and-long-enough", "abc"} {
		t.Run(gen, func(t *testing.T) {
			f, notes := upFx(t)
			fk := f.pid["fakture"]
			f.ok(f.LinkUpstreamWith(ctx, LinkSpec{Project: fk, Upstream: "hub", Remote: "shop"}))
			g := gen
			up := genUp(&g)
			if r := push1(f, up, fk, false); r.Blocked || r.Created != 2 {
				t.Fatalf("%+v", r)
			}
			want := ""
			if gen == genA {
				want = genA
			}
			if storedGen(f) != want || f.marked() != 0 {
				t.Fatalf("stored %q, %d marked", storedGen(f), f.marked())
			}
			// an older upstream stays unchecked: what it sends next (none, or garbage) marks nothing and a push goes on as today
			f.edit("ana", fk, notes["Structure one"], "edited")
			if gen != genA {
				g = ""
			}
			if r := push1(f, up, fk, false); r.Blocked || r.Created+r.Replaced != 1 {
				t.Fatalf("%+v", r)
			}
			if n, _ := hubRestoredAudits(f); n != 0 || f.marked() != 0 {
				t.Fatal("marked without a change of generation")
			}
		})
	}
}

// A new generation marks EVERY link of that upstream (not another upstream's), before anything is sent, with its own cause and one audit
// row; a second flip marks again (the marks stay); the hub going back to an earlier generation is a change too. Nothing clears a mark.
func TestHubGenerationChangeMarksTheUpstreamsLinksAndSendsNothing(t *testing.T) {
	f, notes := upFx(t)
	fk, eu := f.pid["fakture"], f.pid["eureko"]
	f.ok(f.AddUpstream(ctx, "other", "other.example:8765"))
	for _, l := range []LinkSpec{{Project: fk, Upstream: "hub", Remote: "shop"}, {Project: eu, Upstream: "hub", Remote: "shop"}, {Project: fk, Upstream: "other", Remote: "shop"}} {
		f.ok(f.LinkUpstreamWith(ctx, l))
	}
	g := genA
	up := genUp(&g)
	push1(f, up, fk, false)
	f.edit("ana", fk, notes["Structure one"], "edited after the hub's backup")
	posts := up.count("POST")

	g = genB
	r := push1(f, up, fk, false)
	if !r.Blocked || up.count("POST") != posts || !strings.Contains(strings.Join(r.Lines, "\n"), "blocked: "+RestoredState+" ("+HubRestoredCause+")") {
		t.Fatalf("%+v", r)
	}
	ls, e := f.UpstreamLinks(ctx, 0, 0)
	f.ok(e)
	for _, l := range ls {
		if (l.Upstream == "hub") != (l.Restored != nil) || l.Upstream == "hub" && l.RestoredCause != "upstream" {
			t.Fatalf("link %s/%s: %+v %q", l.Label, l.Upstream, l.Restored, l.RestoredCause)
		}
	}
	n, detail := hubRestoredAudits(f)
	if n != 1 || storedGen(f) != genB || !strings.Contains(detail, `"links":2`) || !strings.Contains(detail, genA[:8]) || !strings.Contains(detail, genB[:8]) || strings.Contains(detail, "edited") {
		t.Fatalf("%d audit rows: %s; stored %q", n, detail, storedGen(f))
	}
	// the dry run and the other link of the upstream: blocked too, and still nothing sent
	if r = push1(f, up, eu, false); !r.Blocked || up.count("POST") != posts {
		t.Fatalf("%+v", r)
	}
	if r = push1(f, up, fk, true); !r.Blocked || !r.DryRun {
		t.Fatalf("%+v", r)
	}
	// flips again, and back to a generation it handed out before: a link the upstream's restore marked is never contacted by a push or a
	// dry run (the gate comes first), so what notices is a pull or reconcile (here: reconcile, whose resolve comes before its manifest read)
	for i, next := range []string{genC, genA} {
		g = next
		_, _ = f.ReconcileUpstream(ctx, up, ReconcileSpec{Project: fk, Upstream: "hub"})
		if r = push1(f, up, fk, false); !r.Blocked || up.count("POST") != posts {
			t.Fatalf("%s: %+v", next, r)
		}
		if n, _ := hubRestoredAudits(f); n != 2+i || storedGen(f) != next || f.marked() != 2 {
			t.Fatalf("%s: %d audit rows, stored %q, %d marked", next, n, storedGen(f), f.marked())
		}
	}
}

// A dry run notices it too (it sends nothing anyway) and says so; the mark it sets blocks the real push.
func TestHubGenerationChangeNoticedByADryRun(t *testing.T) {
	f, _ := upFx(t)
	fk := f.pid["fakture"]
	f.ok(f.LinkUpstreamWith(ctx, LinkSpec{Project: fk, Upstream: "hub", Remote: "shop"}))
	g := genA
	up := genUp(&g)
	push1(f, up, fk, true)
	g = genB
	if r := push1(f, up, fk, true); !r.Blocked || !strings.Contains(r.Lines[0], HubRestoredCause) {
		t.Fatalf("%+v", r)
	}
	if r := push1(f, up, fk, false); !r.Blocked || up.count("POST") != 0 {
		t.Fatalf("%+v", r)
	}
}

// Publish mode: the same gate, nothing is written on the upstream.
func TestHubGenerationChangeBlocksAPublishLink(t *testing.T) {
	f, notes := upFx(t)
	fk := f.pid["fakture"]
	f.ok(f.LinkUpstreamWith(ctx, LinkSpec{Project: fk, Upstream: "hub", Remote: "shop", Mode: "publish"}))
	g := genA
	up := genUp(&g)
	push1(f, up, fk, false)
	f.edit("ana", fk, notes["Structure one"], "edited")
	writes := up.count("POST") + up.count("PUT") + up.count("PATCH")
	g = genB
	if r := push1(f, up, fk, false); !r.Blocked || up.count("POST")+up.count("PUT")+up.count("PATCH") != writes {
		t.Fatalf("%+v", r)
	}
}

// The automatic push notices it in its run: it marks, sends nothing, and the timer does not pick the link again.
func TestHubGenerationChangeStopsTheAutomaticPush(t *testing.T) {
	f, fk, notes := autoFx(t)
	g := genA
	up := genUp(&g)
	f.clock = f.clock.Add(61 * time.Second)
	if r, e := f.AutoPush(ctx, up, noSleep); e != nil || !r.Ran || r.Push.Created != 2 {
		t.Fatalf("%+v %v", r, e)
	}
	f.edit("ana", fk, notes["Structure one"], "edited")
	posts := up.count("POST")
	g = genB
	f.clock = f.clock.Add(61 * time.Second)
	r, e := f.AutoPush(ctx, up, noSleep)
	f.ok(e)
	if !r.Ran || r.Push == nil || !r.Push.Blocked || up.count("POST") != posts || f.link(fk).Restored == nil || f.link(fk).DirtySince == nil {
		t.Fatalf("%+v %+v", r, f.link(fk))
	}
	for i := 0; i < 3; i++ {
		f.clock = f.clock.Add(61 * time.Second)
		if r, e = f.AutoPush(ctx, up, noSleep); e != nil || r.Ran || up.count("POST") != posts {
			t.Fatalf("the timer pushed a marked link: %+v %v", r, e)
		}
	}
}

// The learning step binds a receipt only to the submission we sent: a submission with the same number and someone else's text (a restored
// hub hands numbers out again) teaches nothing, accepted or rejected, and the member's hub note it became is shown.
func TestPullLearningRefusesAForeignSubmissionWithTheSameNumber(t *testing.T) {
	for _, state := range []string{"accepted", "rejected"} {
		t.Run(state, func(t *testing.T) {
			f, fk := readFx(t)
			receipts(f, fk, 1)
			registerSubmitted(t, 1, map[string]any{"class": "structure", "title": "Orbit by a member", "body": "their text"})
			answer := accepted(41)
			if state == "rejected" {
				answer = [2]string{"200", `{"state":"rejected","note":0,"reason":"no"}`}
			}
			h := &learnHub{hub: &hub{notes: []hubNote{structNote(41, "Orbit by a member")}}, subs: map[int64][2]string{1: answer}}
			pull(f, fk, h.up())
			if learnedCount(f) != 0 || rejectedMarkers(f) != 0 || learnAudits(f) != 0 {
				t.Fatalf("learned %d, rejected markers %d", learnedCount(f), rejectedMarkers(f))
			}
			if got := f.search("ana", fk, "member"); len(got) != 1 {
				t.Fatalf("the member's note is not shown: %v", got)
			}
		})
	}
}

// While a link is marked, a receipt alone hides nothing (the hub copy is shown; reconcile settles it) and the pull learns nothing.
func TestMarkedLinkNeitherHidesByReceiptNorLearns(t *testing.T) {
	f, fk := readFx(t)
	ids := receipts(f, fk, 2)
	h := &learnHub{hub: &hub{notes: []hubNote{structNote(41, "Orbit local 1")}}, subs: map[int64][2]string{1: accepted(41)}}
	pull(f, fk, h.up())
	if got := f.search("ana", fk, "orbit"); len(got) != 2 || learnedCount(f) != 1 {
		t.Fatalf("before the mark: %v, learned %d", got, learnedCount(f))
	}
	f.markAll()
	if got := f.search("ana", fk, "orbit"); len(got) != 3 {
		t.Fatalf("a marked link hid a hub note by its receipt: %v", got)
	}
	h.hub.notes = append(h.hub.notes, structNote(42, "Orbit local 2"))
	h.subs[2] = accepted(42)
	h.asked = nil
	pull(f, fk, h.up())
	if len(h.asked) != 0 || learnedCount(f) != 1 {
		t.Fatalf("a marked link learned: asked %v, learned %d (local %v)", h.asked, learnedCount(f), ids)
	}
}

// Reconcile: a waiting receipt whose submission is gone, or is now someone else's, is unknown; --new drops it so the push creates the note.
func TestReconcileWaitingReceiptOfALostSubmissionIsUnknown(t *testing.T) {
	for _, lost := range []string{"gone", "foreign"} {
		t.Run(lost, func(t *testing.T) {
			f, project, n, h := reconcileFixture(t, "submit", false)
			f.ok(f.upstreamRecordSent(ctx, project, "hub", n, 9, 0))
			if lost == "foreign" {
				registerSubmitted(t, 9, map[string]any{"class": "structure", "title": n.Title, "body": n.Body})
			}
			r, e := f.ReconcileUpstream(ctx, h.up(), ReconcileSpec{Project: project})
			f.ok(e)
			if r.Cleared || len(r.Notes) != 1 || r.Notes[0].Class != "unknown" {
				t.Fatalf("%+v", r)
			}
			if _, e = f.ReconcileUpstream(ctx, h.up(), ReconcileSpec{Project: project, Note: n.ID, New: true}); e != nil {
				t.Fatal(e)
			}
			var receipts int
			f.ok(f.DB().QueryRow(`SELECT count(*) FROM upstream_sent WHERE note_id=?`, n.ID).Scan(&receipts))
			if receipts != 0 || f.marked() != 0 {
				t.Fatalf("%d receipts left, %d marked", receipts, f.marked())
			}
			up := okUp()
			if r := push1(f, up, project, false); r.Created != 1 || up.count("POST") != 1 {
				t.Fatalf("%+v", r)
			}
		})
	}
}

// The hub's own side: one generation until a restore; `core restore` and the restore pending file each replace it.
func TestHubGenerationIsReplacedByEveryRestore(t *testing.T) {
	f := newFx(t)
	g1, e := f.HubGeneration(ctx)
	f.ok(e)
	if again, _ := f.HubGeneration(ctx); !validGeneration(g1) || again != g1 {
		t.Fatalf("%q %q", g1, again)
	}
	bak := filepath.Join(t.TempDir(), "b.db")
	f.ok(f.Backup(ctx, bak))
	f.restoreFrom(bak)
	g2, e := f.HubGeneration(ctx)
	f.ok(e)
	if g2 == g1 || !validGeneration(g2) {
		t.Fatalf("core restore kept the generation: %q %q", g1, g2)
	}
	dir := f.Store.dir
	f.ok(f.Close())
	f.ok(os.WriteFile(filepath.Join(dir, restorePendingFile), nil, 0o600))
	s, e := Open(dir, Options{})
	f.ok(e)
	f.Store = s
	g3, e := f.HubGeneration(ctx)
	f.ok(e)
	if g3 == g2 || g3 == g1 || !validGeneration(g3) {
		t.Fatalf("the restore pending file kept the generation: %q %q %q", g1, g2, g3)
	}
}

// M1: the owner's override (--after-restore) clears a mark this gateway's restore set, never one the upstream's restore set.
func TestAfterRestoreRefusesAMarkTheUpstreamsRestoreSet(t *testing.T) {
	f, _ := upFx(t)
	fk := f.pid["fakture"]
	f.ok(f.LinkUpstreamWith(ctx, LinkSpec{Project: fk, Upstream: "hub", Remote: "shop"}))
	g := genA
	up := genUp(&g)
	push1(f, up, fk, true)
	g = genB
	r := push1(f, up, fk, false)
	if !r.Blocked || strings.Contains(r.Lines[0], "--after-restore") || !strings.Contains(r.Lines[0], "reconcile") || !strings.Contains(r.Lines[0], "unlink and link again") {
		t.Fatalf("%+v", r)
	}
	e := f.ClearRestoreMark(ctx, fk, 0, "hub")
	if e == nil || !strings.Contains(e.Error(), "reconcile") || f.marked() != 1 {
		t.Fatalf("%v, %d marked", e, f.marked())
	}
	var rows int
	f.ok(f.DB().QueryRow(`SELECT count(*) FROM audit_events WHERE action='upstream.after-restore'`).Scan(&rows))
	if rows != 0 {
		t.Fatal("a refused override wrote its audit row")
	}
	// this gateway's own restore: the override works as before
	f.ok(f.write(ctx, func(tx *sql.Tx) error {
		_, e := tx.Exec(`DELETE FROM meta WHERE key=?`, restoredKey(fk, "hub"))
		return e
	}))
	f.markAll()
	if e = f.ClearRestoreMark(ctx, fk, 0, "hub"); e != nil || f.marked() != 0 {
		t.Fatalf("%v, %d marked", e, f.marked())
	}
}

// L4: a dry run of a link the upstream's restore marked lists nothing it would send (its receipts may name other members' notes).
func TestBlockedDryRunAfterTheUpstreamsRestoreListsNothing(t *testing.T) {
	f, notes := upFx(t)
	fk := f.pid["fakture"]
	f.ok(f.LinkUpstreamWith(ctx, LinkSpec{Project: fk, Upstream: "hub", Remote: "shop"}))
	g := genA
	up := genUp(&g)
	push1(f, up, fk, false)
	f.edit("ana", fk, notes["Structure one"], "edited")
	g = genB
	for i := 0; i < 2; i++ { // the run that notices, and a later one
		if r := push1(f, up, fk, true); !r.Blocked || len(r.Lines) != 1 || r.Created+r.Replaced+r.Skipped != 0 {
			t.Fatalf("%d: %+v", i, r)
		}
	}
}

// L1: after the upstream's restore a surviving receipt of a private note (never in a manifest) is not "known": it is unchecked, and the
// mark stays until the owner decides it by name. After this gateway's own restore it stays known, as before.
func TestReconcilePrivateReceiptAfterTheUpstreamsRestoreIsUnchecked(t *testing.T) {
	for _, cause := range []string{"upstream", "gateway"} {
		t.Run(cause, func(t *testing.T) {
			f, notes := upFx(t)
			fk := f.pid["fakture"]
			f.ok(f.LinkUpstreamWith(ctx, LinkSpec{Project: fk, Upstream: "hub", Remote: "shop", Classes: "structure,private", WithPrivate: true}))
			pn := notes["Private three"]
			un := UpstreamNote{ID: pn.ID, Revision: pn.Revision, Class: pn.Class, EntryKind: pn.EntryKind, Title: pn.Title, Body: pn.Body, Author: pn.Author, Hash: upstreamHash(pn)}
			f.ok(f.upstreamRecordPublished(ctx, fk, "hub", un, 41, 1))
			h := &hub{}
			if cause == "upstream" {
				h.gen = genA
				push1(f, h.up(), fk, true) // first contact: stores the generation
				h.gen = genB
				push1(f, h.up(), fk, true) // notices it
			} else {
				f.markAll()
			}
			r, e := f.ReconcileUpstream(ctx, h.up(), ReconcileSpec{Project: fk})
			f.ok(e)
			var row ReconcileRow
			for _, x := range r.Notes {
				if x.Note == pn.ID {
					row = x
				}
			}
			want := map[string]string{"upstream": "unchecked", "gateway": "known"}[cause]
			if row.Class != want {
				t.Fatalf("%+v", r)
			}
		})
	}
}

// L2: a pull that notices the new generation starts the traversal again from the beginning, also when the earlier one stopped midway.
func TestPullThatNoticesTheUpstreamsRestoreStartsFromTheBeginning(t *testing.T) {
	old := maxPullFetch
	maxPullFetch = 2
	defer func() { maxPullFetch = old }()
	f, fk := readFx(t)
	h := &hub{gen: genA}
	for i := int64(1); i <= 4; i++ {
		h.notes = append(h.notes, structNote(i, fmt.Sprint("Plan", i)))
	}
	if res := pull(f, fk, h.up()); !res.More || res.Next != 2 {
		t.Fatalf("%+v", res)
	}
	h.gen = genB
	h.notes[0].rev, h.notes[0].body = 2, "after the restore"
	res := pull(f, fk, h.up())
	if !res.More || res.Next != 2 {
		t.Fatalf("the traversal went on at the old cursor: %+v", res)
	}
	var body string
	f.ok(f.DB().QueryRow(`SELECT body FROM upstream_mirror WHERE remote_note=1`).Scan(&body))
	if !strings.Contains(body, "after the restore") {
		t.Fatalf("%q", body)
	}
	if res = pull(f, fk, h.up()); res.More || len(mirror(f)) != 4 {
		t.Fatalf("%+v %v", res, mirror(f))
	}
}
