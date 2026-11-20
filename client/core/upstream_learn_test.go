package core

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

// learnHub: the manifest hub plus the submissions a hub would answer for (sub id -> status and body; absent: 404).
type learnHub struct {
	*hub
	subs  map[int64][2]string
	asked []int64
	fail  func(sub int64) (int, string, bool)
}

func (h *learnHub) up() *fakeUp {
	inner := h.hub.up()
	return &fakeUp{handler: func(c upCall) (int, string, time.Duration) {
		if c.method == "GET" && strings.HasPrefix(c.path, "/minigw/v1/projects/7/kb/submissions/") {
			id, _ := strconv.ParseInt(strings.TrimPrefix(c.path, "/minigw/v1/projects/7/kb/submissions/"), 10, 64)
			h.asked = append(h.asked, id)
			if h.fail != nil {
				if st, b, ok := h.fail(id); ok {
					return st, b, 0
				}
			}
			if s, ok := h.subs[id]; ok {
				st, _ := strconv.Atoi(s[0])
				return st, s[1], 0
			}
			return 404, `{"error":{"message":"not found"}}`, 0
		}
		st, b, _, _ := inner.Do(ctx, "hub", c.method, c.path, nil, nil)
		return st, string(b), 0
	}}
}

func accepted(note int64) [2]string {
	return [2]string{"200", fmt.Sprintf(`{"state":"accepted","note":%d,"replaces":0,"revision":0}`, note)}
}

// receipts: n receipts of submissions (remote_sub 1..n), remote_note 0, oldest first.
func receipts(f *fx, fk int64, n int) (ids []int64) {
	f.t.Helper()
	for i := 1; i <= n; i++ {
		f.clock = f.clock.Add(time.Second)
		keySeq++
		nt, _, e := f.CreateNote(ctx, f.p("ana"), fk, fmt.Sprint("lr", keySeq), NoteInput{Class: Structure, Title: fmt.Sprint("Orbit local ", i), Body: "b"})
		f.ok(e)
		ids = append(ids, nt.ID)
		f.ok(f.write(ctx, func(tx *sql.Tx) error {
			_, e := tx.Exec(`INSERT INTO upstream_sent(project_id,upstream,note_id,hash,revision,remote_sub,remote_note,sent) VALUES(?,'hub',?,'h',1,?,0,?)`, fk, nt.ID, i, ms(f.clock))
			return e
		}))
	}
	return
}

func learnedCount(f *fx) (n int) {
	f.t.Helper()
	f.ok(f.DB().QueryRow(`SELECT count(*) FROM upstream_sent WHERE remote_note>0`).Scan(&n))
	return
}

func learnAudits(f *fx) int { return pullAudits(f, "upstream.mirror-learn") }

// The hub copy of an accepted submission is hidden in the very pull that brings it: the receipt is learned before the mirror rows are written.
func TestPullLearnsAcceptedSubmissionBeforeTheCopyShows(t *testing.T) {
	f, fk := readFx(t)
	ids := receipts(f, fk, 1)
	h := &learnHub{hub: &hub{notes: []hubNote{structNote(41, "Orbit local 1"), structNote(42, "Orbit by a member")}}, subs: map[int64][2]string{1: accepted(41)}}
	res := pull(f, fk, h.up())
	if res.Added != 2 {
		t.Fatalf("%+v", res)
	}
	got := f.search("ana", fk, "orbit")
	if len(got) != 2 || !hasID(got, ids[0]) {
		t.Fatalf("the accepted note shows twice, or the member's note is gone: %v", got)
	}
	var note, rev int64
	f.ok(f.DB().QueryRow(`SELECT remote_note,remote_rev FROM upstream_sent WHERE note_id=?`, ids[0]).Scan(&note, &rev))
	if note != 41 || rev != 1 {
		t.Fatalf("receipt: note %d base %d", note, rev)
	}
	if learnAudits(f) != 1 {
		t.Fatal("no audit row")
	}
	var detail string
	f.ok(f.DB().QueryRow(`SELECT detail FROM audit_events WHERE action='upstream.mirror-learn'`).Scan(&detail))
	if strings.Contains(detail, "Orbit") {
		t.Fatal("note text in the audit row", detail)
	}
	h.asked = nil
	pull(f, fk, h.up())
	if len(h.asked) != 0 || learnAudits(f) != 1 {
		t.Fatal("a learned receipt is asked about again", h.asked)
	}
}

// A hub note that copies our trailer with no receipt naming it is shown; a pending submission leaves the receipt as it is.
func TestPullPendingSubmissionLeavesTheReceiptAndACopiedTrailerIsShown(t *testing.T) {
	f, fk := readFx(t)
	ids := receipts(f, fk, 1)
	secret, err := f.upstreamKey(ctx)
	f.ok(err)
	trailer := "\n\n" + upstreamRefMark + upstreamRef(secret, "hub", fk, ids[0]) + ", author ana -->\n"
	copied := structNote(41, "Orbit copied")
	copied.body = "x" + trailer
	h := &learnHub{hub: &hub{notes: []hubNote{copied}}, subs: map[int64][2]string{1: {"200", `{"state":"pending","note":0}`}}}
	pull(f, fk, h.up())
	if learnedCount(f) != 0 || len(f.search("ana", fk, "orbit")) != 2 {
		t.Fatal("a copied trailer hid a hub note, or a pending submission was learned")
	}
	// pending is asked about again only when the hub lists a note the mirror does not hold
	h.asked = nil
	pull(f, fk, h.up())
	if len(h.asked) != 0 {
		t.Fatal("a pending submission was asked about with nothing new on the hub", h.asked)
	}
	h.hub.notes = append(h.hub.notes, structNote(42, "Orbit local 1"))
	h.subs[1] = accepted(42)
	pull(f, fk, h.up())
	if len(h.asked) != 1 || learnedCount(f) != 1 {
		t.Fatal("a note accepted between two pulls was not learned", h.asked)
	}
	if got := f.search("ana", fk, "orbit"); len(got) != 2 || !hasID(got, ids[0]) {
		t.Fatalf("the accepted note shows twice: %v", got)
	}
}

// More than 50 unlearned receipts: 50 in the first run, the rest in the next. Rejected ones do not starve the ones behind them.
func TestPullLearnsAtMostFiftyReceiptsPerRunOldestFirst(t *testing.T) {
	f, fk := readFx(t)
	receipts(f, fk, 60)
	h := &learnHub{hub: &hub{}, subs: map[int64][2]string{}}
	for i := int64(1); i <= 60; i++ {
		h.subs[i] = accepted(100 + i)
	}
	pull(f, fk, h.up())
	if len(h.asked) != 50 || learnedCount(f) != 50 || h.asked[0] != 1 || h.asked[49] != 50 {
		t.Fatalf("first run: asked %d, learned %d", len(h.asked), learnedCount(f))
	}
	h.asked = nil
	pull(f, fk, h.up())
	if len(h.asked) != 10 || learnedCount(f) != 60 {
		t.Fatalf("second run: asked %d, learned %d", len(h.asked), learnedCount(f))
	}
}

func TestPullRejectedSubmissionsDoNotStarveLaterReceiptsAndAreAskedOncePerHour(t *testing.T) {
	f, fk := readFx(t)
	receipts(f, fk, 52)
	h := &learnHub{hub: &hub{}, subs: map[int64][2]string{}}
	for i := int64(1); i <= 51; i++ {
		h.subs[i] = [2]string{"200", `{"state":"rejected","note":0,"reason":"no"}`}
	}
	h.subs[52] = accepted(200)
	pull(f, fk, h.up())
	if len(h.asked) != 50 || learnedCount(f) != 0 {
		t.Fatalf("asked %d learned %d", len(h.asked), learnedCount(f))
	}
	h.asked = nil
	pull(f, fk, h.up())
	if len(h.asked) != 2 || learnedCount(f) != 1 {
		t.Fatalf("the accepted one behind 50 rejected was not reached: asked %v learned %d", h.asked, learnedCount(f))
	}
	h.asked = nil
	pull(f, fk, h.up())
	if len(h.asked) != 0 {
		t.Fatal("rejected submissions are asked about at every pull", h.asked)
	}
	f.clock = f.clock.Add(learnAskedFor)
	pull(f, fk, h.up())
	if len(h.asked) != 0 || rejectedMarkers(f) != 51 { // a rejection is kept durably (a marker per receipt): there is nothing to ask again after the hour
		t.Fatalf("after the hour asked %d, markers %d", len(h.asked), rejectedMarkers(f))
	}
}

// A failed learning request leaves the mirror and the receipt as before; nothing is deleted; the next pull tries again.
func TestPullLearningFailureChangesNothingAndIsRetried(t *testing.T) {
	cases := map[string]func(int64) (int, string, bool){
		"5xx":      func(int64) (int, string, bool) { return 503, `{"error":{"message":"down"}}`, true },
		"403":      func(int64) (int, string, bool) { return 403, `{"error":{"message":"no"}}`, true },
		"429":      func(int64) (int, string, bool) { return 429, `{"error":{"message":"slow"}}`, true },
		"garbage":  func(int64) (int, string, bool) { return 200, `<html>`, true },
		"not json": func(int64) (int, string, bool) { return 200, `{"state":"accepted","note":"x"}`, true },
	}
	for name, fail := range cases {
		t.Run(name, func(t *testing.T) {
			f, fk := readFx(t)
			pull(f, fk, (&learnHub{hub: &hub{notes: []hubNote{structNote(42, "Orbit two")}}}).up()) // an earlier pull: the mirror holds note 42
			receipts(f, fk, 3)
			h := &learnHub{hub: &hub{notes: []hubNote{structNote(41, "Orbit local 1"), structNote(42, "Orbit two")}}, subs: map[int64][2]string{1: accepted(41)}}
			before := mirror(f)
			h.fail = fail
			res := pull(f, fk, h.up())
			if res.outcome != "ok" || res.More || res.Stopped != "" || learnedCount(f) != 0 || learnAudits(f) != 0 {
				t.Fatalf("%+v", res)
			}
			if len(mirror(f)) != len(before)+1 || !strings.Contains(strings.Join(mirror(f), ","), "42/") {
				t.Fatalf("the mirror lost a note: %v", mirror(f))
			}
			h.fail, h.asked = nil, nil
			pull(f, fk, h.up())
			if learnedCount(f) != 1 || len(h.asked) == 0 || h.asked[0] != 1 {
				t.Fatalf("the next pull did not try again: asked %v learned %d", h.asked, learnedCount(f))
			}
		})
	}
}

// A dry run asks nothing and writes nothing.
func TestPullDryRunLearnsNothing(t *testing.T) {
	f, fk := readFx(t)
	receipts(f, fk, 2)
	h := &learnHub{hub: &hub{}, subs: map[int64][2]string{1: accepted(41), 2: accepted(42)}}
	before := tableCounts(t, f)
	res, err := f.PullUpstream(ctx, h.up(), fk, 0, "", true, noSleep)
	f.ok(err)
	if len(res) != 1 || len(h.asked) != 0 || learnedCount(f) != 0 || tableCounts(t, f) != before {
		t.Fatalf("%+v asked %v", res, h.asked)
	}
}

// M1: 51 pending receipts, one accepted receipt behind them, and one hub note the pull skips at every run (so the hub always lists a note the
// mirror does not hold). Never-asked receipts go first: the accepted one is asked in the SECOND run (ceil(52/50) runs at most), and its hub copy
// shows once from then on. The pending ones are re-asked only with the slots that are left, the least recently asked first.
func TestPullPendingQueueDoesNotStarveTheReceiptBehindIt(t *testing.T) {
	f, fk := readFx(t)
	ids := receipts(f, fk, 52)
	h := &learnHub{hub: &hub{notes: []hubNote{structNote(900, "Orbit skipped"), structNote(901, "Orbit local 52")}}, subs: map[int64][2]string{52: accepted(901)}}
	h.hub.note = func(id int64, def string) (int, string, time.Duration) {
		if id == 900 {
			return 404, `{"error":{"message":"gone"}}`, 0
		}
		return 200, def, 0
	}
	for i := int64(1); i <= 51; i++ {
		h.subs[i] = [2]string{"200", `{"state":"pending","note":0}`}
	}
	pull(f, fk, h.up())
	if len(h.asked) != 50 || learnedCount(f) != 0 {
		t.Fatalf("first run asked %d learned %d", len(h.asked), learnedCount(f))
	}
	h.asked = nil
	pull(f, fk, h.up())
	if learnedCount(f) != 1 || !hasID(f.search("ana", fk, "orbit local 52"), ids[51]) || len(f.search("ana", fk, "orbit local 52")) != 1 {
		t.Fatalf("the accepted receipt behind 51 pending ones was not learned in the second run: asked %v", h.asked)
	}
	if len(h.asked) != 2+48 || h.asked[0] != 51 || h.asked[1] != 52 {
		t.Fatalf("second run asked %v: never-asked first, then the least recently asked pending ones", h.asked)
	}
	// a third run rotates: the pending ones asked least recently come first, so every one gets its turn
	h.asked = nil
	pull(f, fk, h.up())
	seen := map[int64]bool{}
	for _, id := range h.asked {
		seen[id] = true
	}
	if len(h.asked) != 50 || seen[51] {
		t.Fatalf("third run asked %v", h.asked)
	}
}

// L1: the receipt changes between the read and the write (another submission, or a note id already set): nothing is written.
func TestPullLearningWritesOnlyToTheReceiptItRead(t *testing.T) {
	for _, change := range []string{`remote_sub=99`, `remote_note=77`} {
		t.Run(change, func(t *testing.T) {
			f, fk := readFx(t)
			ids := receipts(f, fk, 1)
			h := &learnHub{hub: &hub{}, subs: map[int64][2]string{1: accepted(41)}}
			h.fail = func(sub int64) (int, string, bool) {
				f.ok(f.write(ctx, func(tx *sql.Tx) error {
					_, e := tx.Exec(`UPDATE upstream_sent SET `+change+` WHERE note_id=?`, ids[0])
					return e
				}))
				return 0, "", false
			}
			pull(f, fk, h.up())
			var sub, note, rev int64
			f.ok(f.DB().QueryRow(`SELECT remote_sub,remote_note,remote_rev FROM upstream_sent WHERE note_id=?`, ids[0]).Scan(&sub, &note, &rev))
			wantSub, wantNote := int64(1), int64(0)
			if change == `remote_sub=99` {
				wantSub = 99
			} else {
				wantNote = 77
			}
			if sub != wantSub || note != wantNote || rev != 0 || learnAudits(f) != 0 {
				t.Fatalf("receipt: sub %d note %d base %d", sub, note, rev)
			}
		})
	}
}
