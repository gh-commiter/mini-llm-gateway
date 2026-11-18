package core

import (
	"database/sql"
	"strconv"
	"strings"
	"testing"
	"time"
)

// sec-upc4 1: an acceptance is over the EFFECTIVE reach. Sources are never restored from an earlier acceptance: reading off forgets them,
// an explicit empty list means none, and only an omitted list keeps the ones the link reads now.
func TestAcceptanceNeverRestoresSourcesThatWereNotShown(t *testing.T) {
	f, fk := readFx(t)
	yes, no := true, false
	link := func(sp LinkSpec) {
		sp.Project, sp.Upstream, sp.Remote, sp.ReadOnly = fk, "hub", "shop", true
		f.ok(f.LinkUpstreamWith(ctx, sp))
	}
	link(LinkSpec{Read: &yes, ReadClasses: "structure,data", ReadSources: ptr("team:sensitive"), AcceptReach: true})
	if l := linkOf(f, fk); strings.Join(l.ReadSources, ",") != "team:sensitive" {
		t.Fatalf("%+v", l)
	}
	link(LinkSpec{Read: &no})
	link(LinkSpec{Read: &yes, ReadSources: ptr(""), AcceptReach: true}) // the panel: "no extra sources"
	l := linkOf(f, fk)
	if len(l.ReadSources) != 0 || len(l.ReadClasses) != 1 || l.ReadClasses[0] != Structure {
		t.Fatalf("reading came back with what was not shown: %+v", l)
	}
	var reach string
	f.ok(f.read(ctx, func(tx *sql.Tx) error { return tx.QueryRow(`SELECT read_reach FROM upstream_links`).Scan(&reach) }))
	if reach != "hub|shop|0|structure|" {
		t.Fatal(reach)
	}
	// off and on again with no list at all: the default reach, not the old one
	link(LinkSpec{Read: &yes, ReadSources: ptr("team:acme"), AcceptReach: true})
	link(LinkSpec{Read: &no})
	link(LinkSpec{Read: &yes, AcceptReach: true})
	if l = linkOf(f, fk); len(l.ReadSources) != 0 {
		t.Fatalf("%+v", l)
	}
	// while on, an explicit empty list clears the sources (a new acceptance, the mirror is refilled), an omitted one keeps them
	link(LinkSpec{ReadSources: ptr("team:acme"), AcceptReach: true})
	link(LinkSpec{Read: &yes, AcceptReach: true})
	if l = linkOf(f, fk); strings.Join(l.ReadSources, ",") != "team:acme" {
		t.Fatalf("omitted must keep: %+v", l)
	}
	link(LinkSpec{ReadSources: ptr(""), AcceptReach: true})
	if l = linkOf(f, fk); len(l.ReadSources) != 0 || !l.Read {
		t.Fatalf("empty must clear: %+v", l)
	}
	// a re-link to another remote turns reading off and forgets the lists too
	link(LinkSpec{ReadSources: ptr("team:acme"), AcceptReach: true})
	f.ok(f.LinkUpstreamWith(ctx, LinkSpec{Project: fk, Upstream: "hub", Remote: "other", ReadOnly: false}))
	link(LinkSpec{Read: &yes, AcceptReach: true})
	if l = linkOf(f, fk); len(l.ReadSources) != 0 {
		t.Fatalf("%+v", l)
	}
}

// sec-upc4 2: a failure in an early slice of a traversal is not forgotten by a later slice that succeeds: the pull does not end fresh, it
// backs off, and the next traversal (from the start) fetches the notes that failed.
func TestTraversalFailureIsCarriedToItsEnd(t *testing.T) {
	f, fk, h := autoPullFx(t)
	h.notes = nil
	for i := int64(1); i <= 5; i++ {
		h.notes = append(h.notes, structNote(i, "Orbit note"))
	}
	broken := true
	h.note = func(id int64, def string) (int, string, time.Duration) {
		if broken && id <= 3 {
			return 500, `{"error":{"message":"down"}}`, 0
		}
		return 200, def, 0
	}
	up := h.up()
	r := f.autoPull(up)
	if !r.Ran || r.Pull.Added != 0 || r.Pull.Next != 3 {
		t.Fatalf("slice 1: %+v", r.Pull)
	}
	f.clock = f.clock.Add(time.Minute)
	r = f.autoPull(up)
	l := linkOf(f, fk)
	if !r.Ran || r.Pull.Added != 2 || r.Pull.More || l.LastPullOK != nil || l.PullFailCount != 2 || l.PullError == "" || l.Stale && false {
		t.Fatalf("slice 2 ended fresh: %+v %+v", r.Pull, l)
	}
	if got := len(mirror(f)); got != 2 {
		t.Fatal(got)
	}
	broken = false
	f.clock = f.clock.Add(2 * time.Minute)
	r = f.autoPull(up)
	l = linkOf(f, fk)
	if !r.Ran || r.Pull.Added != 3 || l.LastPullOK == nil || l.PullFailCount != 0 || l.PullError != "" || len(mirror(f)) != 5 {
		t.Fatalf("the next traversal: %+v %+v", r.Pull, l)
	}
}

// sec-upc4 3: the audit cap is per failure episode, however the outcomes alternate; a run that changed the mirror always has its row.
func TestAutoPullAuditCapHoldsAgainstAlternatingOutcomes(t *testing.T) {
	f, _, h := autoPullFx(t)
	up := h.up()
	f.autoPull(up) // 2 added: one row
	mode := 0
	h.manifest = func() (int, string) {
		if mode%2 == 0 {
			return 500, `{"error":{"message":"down"}}`
		}
		return 429, `{}`
	}
	for i := 0; i < 30; i++ {
		mode = i
		f.clock = f.clock.Add(2 * time.Hour)
		f.autoPull(up)
	}
	if n := pullAudits(f, "upstream.auto-pull"); n > 1+5 {
		t.Fatal("rows in one failure episode:", n)
	}
	// a change of the mirror still has its row, after the cap
	h.manifest = nil
	h.notes[0].rev = 2
	f.clock = f.clock.Add(2 * time.Hour)
	before := pullAudits(f, "upstream.auto-pull")
	f.autoPull(up)
	if pullAudits(f, "upstream.auto-pull") != before+1 {
		t.Fatal("the change after the cap has no row")
	}
	// and a new episode starts from zero
	h.manifest = func() (int, string) { return 500, `{}` }
	f.clock = f.clock.Add(2 * time.Hour)
	before = pullAudits(f, "upstream.auto-pull")
	f.autoPull(up)
	if pullAudits(f, "upstream.auto-pull") != before+1 {
		t.Fatal("a new episode has no row")
	}
}

// sec-upc4 6: a timer that a refusal switched off stays off after a pull by hand that goes through; only the owner's new acceptance (the
// same step as turning reading on) switches it on, and a pull by hand never touches it.
func TestManualPullNeverRestartsARefusedTimer(t *testing.T) {
	f, fk, h := autoPullFx(t)
	up := h.up()
	h.proj = 404
	f.autoPull(up)
	if !linkOf(f, fk).PullTimerOff {
		t.Fatal("not off")
	}
	h.proj = 0
	if r := pull(f, fk, up); r.Added != 2 {
		t.Fatalf("%+v", r)
	}
	if l := linkOf(f, fk); !l.PullTimerOff {
		t.Fatalf("a pull by hand restarted the timer: %+v", l)
	}
	f.clock = f.clock.Add(48 * time.Hour)
	if f.autoPull(up).Ran {
		t.Fatal("the timer ran")
	}
	readOn(f, fk, "", "")
	if l := linkOf(f, fk); l.PullTimerOff {
		t.Fatal("a new acceptance did not switch it on")
	}
}

func pubReceipt(f *fx, fk, local, remoteNote, remoteRev int64, class Class, kind, title, body string) {
	f.t.Helper()
	f.ok(f.write(ctx, func(tx *sql.Tx) error {
		_, e := tx.Exec(`INSERT OR REPLACE INTO upstream_sent(project_id,upstream,note_id,hash,revision,remote_sub,remote_note,sent,remote_rev,sent_content) VALUES(?,'hub',?,'h',1,0,?,1,?,?)`,
			fk, local, remoteNote, remoteRev, sentContentHash(class, kind, title, body))
		return e
	}))
}

// sec-upc4 4: the hub's copy of a published note is judged by its CONTENT against what we sent, not by revision numbers: a copy with other
// text at a LOWER revision is shown; the same text at a raised revision is not; and the owner's status flags the revision that went backwards.
func TestMirrorDedupesOnContentNotRevision(t *testing.T) {
	f := newFx(t)
	f.ok(f.AddUpstream(ctx, "hub", "hub.example:8765"))
	fk := f.pid["fakture"]
	readingLink(f, fk, 0, "shop")
	local := f.mk("ana", "fakture", Structure)
	secret, err := f.upstreamKey(ctx)
	f.ok(err)
	trailer := "\n\n" + upstreamRefMark + upstreamRef(secret, "hub", fk, local.ID) + ", author ana -->\n"
	// we sent "the text" at hub revision 10; the mirror row is revision 3 (rawMirror) with other text: a rollback with changed content
	changed := rawMirror(f, fk, "hub", 40, Structure, KindFinding, "project:shop", "orbit published", "other text"+trailer)
	pubReceipt(f, fk, local.ID, 40, 10, Structure, KindFinding, "orbit published", "the text")
	if !hasID(f.search("ana", fk, "orbit"), changed) {
		t.Fatal("a replaced copy at a lower revision is hidden")
	}
	n, e := f.GetNote(ctx, f.p("ana"), fk, changed)
	f.ok(e)
	if !strings.Contains(n.Body, "[differs from the local note "+strconv.FormatInt(local.ID, 10)+":") {
		t.Fatalf("%q", n.Body)
	}
	if l := linkOf(f, fk); len(l.RevisionBackwards) != 1 || l.RevisionBackwards[0] != local.ID {
		t.Fatalf("status does not flag the revision that went backwards: %+v", l.RevisionBackwards)
	}
	// the same text (our trailer does not count) at a raised revision (receipt 2, mirror 3): ours, hidden, no false "changed there"
	f.ok(f.write(ctx, func(tx *sql.Tx) error {
		_, e := tx.Exec(`UPDATE upstream_mirror SET body=? WHERE remote_note=40`, "the text"+trailer)
		return e
	}))
	pubReceipt(f, fk, local.ID, 40, 2, Structure, KindFinding, "orbit published", "the text")
	if hasID(f.search("ana", fk, "orbit"), changed) {
		t.Fatal("unchanged text at a raised revision is shown as a second copy")
	}
	if l := linkOf(f, fk); len(l.RevisionBackwards) != 0 {
		t.Fatalf("%+v", l.RevisionBackwards)
	}
}

// sec-upc4 5: the "differs" line names the local note only to a reader of that note.
func TestDiffersLineNamesOnlyANoteTheReaderMayRead(t *testing.T) {
	f := newFx(t)
	f.ok(f.AddUpstream(ctx, "hub", "hub.example:8765"))
	fk := f.pid["fakture"]
	readingLink(f, fk, 0, "shop")
	private := f.mk("ana", "fakture", Private)
	data := f.mk("ana", "fakture", Data)
	hubPrivate := rawMirror(f, fk, "hub", 40, Structure, KindFinding, "project:shop", "orbit one", "edited there")
	hubData := rawMirror(f, fk, "hub", 41, Structure, KindFinding, "project:shop", "orbit two", "edited there")
	pubReceipt(f, fk, private.ID, 40, 1, Structure, KindFinding, "orbit one", "sent text")
	pubReceipt(f, fk, data.ID, 41, 1, Structure, KindFinding, "orbit two", "sent text")
	line := func(user string, id int64) string {
		n, e := f.GetNote(ctx, f.p(user), fk, id)
		f.ok(e)
		for _, l := range strings.Split(n.Body, "\n") {
			if strings.HasPrefix(l, "[differs from the local note") {
				return l
			}
		}
		t.Fatalf("%s: no line in %q", user, n.Body)
		return ""
	}
	generic := "[differs from the local note: the hub's copy was changed there after this gateway sent it]"
	if got := line("ana", hubPrivate); !strings.Contains(got, strconv.FormatInt(private.ID, 10)+":") {
		t.Fatal("the author of a private note does not get its number:", got)
	}
	if got := line("bor", hubPrivate); got != generic {
		t.Fatal("a member got the number of another member's private note:", got)
	}
	if got := line("ana", hubData); !strings.Contains(got, strconv.FormatInt(data.ID, 10)+":") {
		t.Fatal(got)
	}
	if got := line("eva", hubData); got != generic { // a reader has no data class
		t.Fatal("a reader got the number of a data note:", got)
	}
}

// The canonical content is the same on both sides: what a push writes (title, body and its trailer, even with invisible characters and
// trailing blanks) hashes like the local note.
func TestSentContentHashIsTheSameOnBothSides(t *testing.T) {
	p := &pusher{secret: []byte("k"), up: "hub"}
	n := UpstreamNote{ID: 7, Class: Structure, Title: "Plan​ ", Body: "Line one\n\nLine two \n", EntryKind: KindFinding, Author: "ana"}
	hubBody := n.Body + p.trailer(1, n, 0)
	if sentContentHash(n.Class, n.EntryKind, n.Title, n.Body) != sentContentHash(n.Class, n.EntryKind, upText(n.Title, maxTitle), mirrorBody(hubBody)) {
		t.Fatal("the two sides disagree")
	}
	if sentContentHash(n.Class, n.EntryKind, n.Title, n.Body) == sentContentHash(n.Class, n.EntryKind, n.Title, n.Body+"x") {
		t.Fatal("different text, same hash")
	}
	if sentContentHash(Structure, KindFinding, "a", "b") == sentContentHash(Data, KindFinding, "a", "b") || sentContentHash(Structure, KindFinding, "a", "b") == sentContentHash(Structure, KindFAQ, "a", "b") {
		t.Fatal("class or kind ignored")
	}
}
