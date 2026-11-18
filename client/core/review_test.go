package core

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Member review: only a project admin (direct or through a team assignment) reviews, never their own submission.

func (f *fx) submit(user string, project int64, key string, c Class) Submission {
	f.t.Helper()
	v, _, e := f.Submit(ctx, f.p(user), project, key, SubmissionInput{NoteInput: NoteInput{Class: c, Title: "T " + key, Body: "B " + key}})
	f.ok(e)
	return v
}

func TestReviewOnlyProjectAdmins(t *testing.T) {
	f := newFx(t)
	pid := f.pid["fakture"]
	v := f.submit("ana", pid, "a1", Data)
	// developer, other developer, reader, grant reader, admin of another project: the answer for an unseen project
	for _, u := range []string{"ana", "bor", "eva", "cene", "dana"} {
		_, e := f.ListReview(ctx, f.p(u), pid, 0)
		is(t, e, ErrNotFound)
		_, _, e = f.GetReview(ctx, f.p(u), pid, v.ID)
		is(t, e, ErrNotFound)
		_, _, e = f.GetReview(ctx, f.p(u), 0, v.ID)
		is(t, e, ErrNotFound)
		_, e = f.MemberReview(ctx, f.p(u), 0, v.ID, true, "", f.seen(v.ID))
		is(t, e, ErrNotFound)
		_, e = f.MemberReview(ctx, f.p(u), pid, v.ID, false, "no", "")
		is(t, e, ErrNotFound)
		all, e := f.ListReview(ctx, f.p(u), 0, 0)
		f.ok(e)
		if len(all) != 0 {
			t.Fatal("list all leaked", u, all)
		}
	}
	// a missing id is the same 404
	_, _, e := f.GetReview(ctx, f.p("fani"), 0, 9999)
	is(t, e, ErrNotFound)
	// the admin: list (project and all), read with the body
	for _, project := range []int64{pid, 0} {
		vs, e := f.ListReview(ctx, f.p("fani"), project, 0)
		f.ok(e)
		if len(vs) != 1 || vs[0].ID != v.ID {
			t.Fatal(project, vs)
		}
	}
	g, w, e := f.GetReview(ctx, f.p("fani"), pid, v.ID)
	f.ok(e)
	if g.Body == nil || *g.Body != "B a1" || g.User != f.uid["ana"] || w != "ana" {
		t.Fatal(g)
	}
	// a submission id under another project's path is 404
	_, _, e = f.GetReview(ctx, f.p("fani"), f.pid["eureko"], v.ID)
	is(t, e, ErrNotFound)
}

func TestReviewAcceptRecordsWriterAndReviewer(t *testing.T) {
	f := newFx(t)
	pid := f.pid["fakture"]
	v := f.submit("ana", pid, "a1", Data)
	acc, e := f.MemberReview(ctx, f.p("fani"), 0, v.ID, true, "", f.seen(v.ID))
	f.ok(e)
	reviewer := "user:" + i64(f.uid["fani"])
	n, e := f.GetNote(ctx, f.p("ana"), pid, acc.NoteID)
	f.ok(e)
	if n.AuthorID != f.uid["ana"] || n.Provenance.User != f.uid["ana"] || n.Provenance.Reviewer != reviewer || n.Provenance.Reviewed == nil {
		t.Fatal(n)
	}
	r := f.submit("bor", pid, "b1", Data)
	rej, e := f.MemberReview(ctx, f.p("fani"), pid, r.ID, false, "not now", "")
	f.ok(e)
	if rej.State != "rejected" || rej.Reason != "not now" || rej.Provenance.Reviewer != reviewer {
		t.Fatal(rej)
	}
	evs, e := f.AuditEvents(ctx, 0, 1000)
	f.ok(e)
	seen := map[string]string{}
	for _, ev := range evs {
		if ev.Action == "kb.accepted" || ev.Action == "kb.rejected" {
			seen[ev.Action+" "+ev.Subject] = ev.Actor
		}
	}
	if seen["kb.accepted "+i64(v.ID)] != reviewer || seen["kb.rejected "+i64(r.ID)] != reviewer {
		t.Fatal("audit", seen)
	}
}

func TestReviewFourEyes(t *testing.T) {
	f := newFx(t)
	pid := f.pid["fakture"]
	own := f.submit("fani", pid, "f1", Data)
	vs, e := f.ListReview(ctx, f.p("fani"), 0, 0)
	f.ok(e)
	if len(vs) != 0 {
		t.Fatal("own submission listed", vs)
	}
	_, e = f.MemberReview(ctx, f.p("fani"), pid, own.ID, true, "", f.seen(own.ID))
	is(t, e, ErrOwnSubmission)
	_, e = f.MemberReview(ctx, f.p("fani"), pid, own.ID, false, "", "")
	is(t, e, ErrOwnSubmission)
	// a second admin may; so may the owner (here: the second admin)
	f.ok(f.SetMember(ctx, pid, f.uid["bor"], Admin))
	acc, e := f.MemberReview(ctx, f.p("bor"), pid, own.ID, true, "", f.seen(own.ID))
	f.ok(e)
	if acc.State != "accepted" {
		t.Fatal(acc)
	}
	own2 := f.submit("fani", pid, "f2", Data)
	acc, e = f.ReviewSubmission(ctx, own2.ID, true, "", f.seen(own2.ID))
	f.ok(e)
	if acc.Provenance.Reviewer != adminActor {
		t.Fatal(acc)
	}
}

func TestReviewThroughTeamAssignment(t *testing.T) {
	f := newSeg(t)
	pid := f.pid["fakture"]
	v := f.submit("ana", pid, "a1", Data)
	_, _, e := f.GetReview(ctx, f.p("eva"), 0, v.ID)
	is(t, e, ErrNotFound)
	f.ok(f.SetTeamAssignment(ctx, f.acme, pid, f.uid["eva"], Admin))
	_, e = f.MemberReview(ctx, f.p("eva"), 0, v.ID, true, "", f.seen(v.ID))
	f.ok(e)
}

func TestReviewTeamNotes(t *testing.T) {
	f := newSeg(t)
	pid := f.pid["fakture"]
	f.ok(f.SetMember(ctx, pid, f.uid["bor"], Admin))
	f.ok(f.AddTeamMember(ctx, f.acme, f.uid["bor"]))
	v := f.submit("bor", f.seg, "s1", Data)
	// roster reader, admin of another team, admin of the project without the roster entry
	for _, u := range []string{"ana", "eva", "dana"} {
		_, _, e := f.GetReview(ctx, f.p(u), f.seg, v.ID)
		is(t, e, ErrNotFound)
	}
	_, e := f.MemberReview(ctx, f.p("bor"), f.seg, v.ID, true, "", f.seen(v.ID))
	is(t, e, ErrOwnSubmission)
	vs, e := f.ListReview(ctx, f.p("fani"), f.seg, 0)
	f.ok(e)
	if len(vs) != 1 {
		t.Fatal(vs)
	}
	// an archived team: no review (its notes are not visible at all)
	f.ok(f.SetTeamArchived(ctx, f.acme, true))
	_, e = f.MemberReview(ctx, f.p("fani"), f.seg, v.ID, true, "", f.seen(v.ID))
	is(t, e, ErrNotFound)
	vs, e = f.ListReview(ctx, f.p("fani"), 0, 0)
	f.ok(e)
	if len(vs) != 0 {
		t.Fatal("archived team listed", vs)
	}
	f.ok(f.SetTeamArchived(ctx, f.acme, false))
	acc, e := f.MemberReview(ctx, f.p("fani"), 0, v.ID, true, "", f.seen(v.ID))
	f.ok(e)
	if acc.NoteID == 0 {
		t.Fatal(acc)
	}
}

func TestReviewArchivedProject(t *testing.T) {
	f := newFx(t)
	pid := f.pid["fakture"]
	v := f.submit("ana", pid, "a1", Data)
	f.ok(f.SetProjectArchived(ctx, pid, true))
	_, e := f.ListReview(ctx, f.p("fani"), pid, 0)
	is(t, e, ErrArchived)
	_, e = f.MemberReview(ctx, f.p("fani"), 0, v.ID, true, "", f.seen(v.ID))
	is(t, e, ErrArchived)
	_, e = f.MemberReview(ctx, f.p("fani"), 0, v.ID, false, "", "")
	is(t, e, ErrArchived)
	vs, e := f.ListReview(ctx, f.p("fani"), 0, 0)
	f.ok(e)
	if len(vs) != 0 {
		t.Fatal(vs)
	}
}

func TestReviewLostAtOnce(t *testing.T) {
	f := newFx(t)
	pid := f.pid["fakture"]
	v := f.submit("ana", pid, "a1", Data)
	p := f.p("fani")
	f.ok(f.SetUserActive(ctx, f.uid["fani"], false))
	_, e := f.ListReview(ctx, p, 0, 0)
	is(t, e, ErrNoProjectAccess)
	_, e = f.MemberReview(ctx, p, 0, v.ID, true, "", f.seen(v.ID))
	is(t, e, ErrNoProjectAccess)
	f.ok(f.SetUserActive(ctx, f.uid["fani"], true))
	f.ok(f.RemoveBinding(ctx, "dev-fani"))
	_, _, e = f.GetReview(ctx, p, 0, v.ID)
	is(t, e, ErrNoProjectAccess)
	_, e = f.MemberReview(ctx, p, pid, v.ID, true, "", f.seen(v.ID))
	is(t, e, ErrNoProjectAccess)
	// demoted: the role, not a grant, decides
	f.ok(f.SetBinding(ctx, "dev-fani", f.uid["fani"]))
	f.ok(f.SetMember(ctx, pid, f.uid["fani"], Developer))
	_, e = f.MemberReview(ctx, f.p("fani"), pid, v.ID, true, "", f.seen(v.ID))
	is(t, e, ErrNotFound)
}

// Private submissions follow the private rule: the author and the project admins (not per writer); other members never.
func TestReviewPrivate(t *testing.T) {
	f := newFx(t)
	pid := f.pid["fakture"]
	v := f.submit("ana", pid, "p1", Private)
	_, _, e := f.GetReview(ctx, f.p("bor"), pid, v.ID)
	is(t, e, ErrNotFound)
	_, e = f.GetSubmission(ctx, f.p("bor"), pid, v.ID, false)
	is(t, e, ErrNotFound)
	g, _, e := f.GetReview(ctx, f.p("fani"), pid, v.ID)
	f.ok(e)
	if g.Class != Private || g.Body == nil {
		t.Fatal(g)
	}
	acc, e := f.MemberReview(ctx, f.p("fani"), pid, v.ID, true, "", f.seen(v.ID))
	f.ok(e)
	_, e = f.GetNote(ctx, f.p("bor"), pid, acc.NoteID)
	is(t, e, ErrNotFound)
}

// Accept never publishes what the reviewer could not write: the class (create) and the replaced note (change). With the
// shipped matrix an admin writes every class, so the backstop is shown with a narrowed admin row.
func TestReviewReviewerMustWrite(t *testing.T) {
	f := newFx(t)
	pid := f.pid["fakture"]
	old := matrix[Admin]
	t.Cleanup(func() { matrix[Admin] = old })
	narrowed := map[Class]perm{}
	for c, p := range old {
		narrowed[c] = p
	}
	narrowed[Private] &^= pCreate
	narrowed[Data] &^= pWriteAny
	matrix[Admin] = narrowed
	priv := f.submit("ana", pid, "p1", Private)
	_, e := f.MemberReview(ctx, f.p("fani"), pid, priv.ID, true, "", f.seen(priv.ID))
	is(t, e, ErrForbidden)
	n, _, e := f.CreateNote(ctx, f.p("ana"), pid, "n1", NoteInput{Class: Data, Title: "mine", Body: "x"})
	f.ok(e)
	rep, _, e := f.Submit(ctx, f.p("ana"), pid, "r1", SubmissionInput{NoteInput: NoteInput{Class: Data, Title: "mine2", Body: "y"}, Replaces: n.ID, Revision: n.Revision})
	f.ok(e)
	_, e = f.MemberReview(ctx, f.p("fani"), pid, rep.ID, true, "", f.seen(rep.ID))
	is(t, e, ErrForbidden)
	// rejecting needs no write; the owner still accepts
	_, e = f.MemberReview(ctx, f.p("fani"), pid, priv.ID, false, "no", "")
	f.ok(e)
	_, e = f.ReviewSubmission(ctx, rep.ID, true, "", f.seen(rep.ID))
	f.ok(e)
}

func TestReviewPendingCount(t *testing.T) {
	f := newFx(t)
	pid := f.pid["fakture"]
	f.submit("ana", pid, "a1", Data)
	f.submit("fani", pid, "f1", Data)
	for u, want := range map[string]int{"fani": 1, "ana": 0, "eva": 0} {
		ps, e := f.ListProjects(ctx, f.p(u), false)
		f.ok(e)
		for _, p := range ps {
			if p.ID == pid && p.Pending != want {
				t.Fatal(u, p.Pending)
			}
		}
	}
}

// A replacement's kind limits are checked only for a note the caller may change: another project's faq, another
// project's finding and a missing id give the same answer (no oracle for which ids exist or are faqs).
func TestSubmitReplacesNoKindOracle(t *testing.T) {
	f := newFx(t)
	other := f.pid["eureko"]
	faq, _, e := f.Submit(ctx, f.p("cene"), other, "q", SubmissionInput{NoteInput: NoteInput{Class: Structure, Title: "Q?", Body: "A", EntryKind: KindFAQ}})
	f.ok(e)
	faq, e = f.ReviewSubmission(ctx, faq.ID, true, "", f.seen(faq.ID))
	f.ok(e)
	finding := f.mk("cene", "eureko", Structure)
	long := strings.Repeat("x", maxFAQBody+1)
	var answers []string
	for i, id := range []int64{faq.NoteID, finding.ID, 999999} {
		_, _, e := f.Submit(ctx, f.p("ana"), f.pid["fakture"], fmt.Sprint("o", i), SubmissionInput{NoteInput: NoteInput{Class: Data, Title: "t", Body: long}, Replaces: id, Revision: 1})
		answers = append(answers, fmt.Sprint(e))
	}
	if answers[0] != answers[1] || answers[1] != answers[2] || !strings.Contains(answers[0], ErrNotFound.Error()) {
		t.Fatal(answers)
	}
}

// seen is the review hash an owner's read of submission id gives now (what an accept must name).
func (f *fx) seen(id int64) string {
	f.t.Helper()
	v, e := f.GetSubmission(ctx, Principal{}, 0, id, true)
	if e != nil {
		return "" // the accept under test then fails on its own terms
	}
	return v.ReviewHash
}

// M1: an accept names the content its reviewer read; the author's rewrite in between (kb import merges into the pending
// submission) is refused, for a project admin and for the owner alike.
func TestReviewAcceptBoundToWhatWasRead(t *testing.T) {
	f := newFx(t)
	pid := f.pid["fakture"]
	first, e := f.ImportMarkdown(ctx, f.p("ana"), pid, MarkdownInput{Markdown: "harmless", Path: "p.md", Class: Data, Submit: true})
	f.ok(e)
	read, _, e := f.GetReview(ctx, f.p("fani"), 0, first.SubmissionID)
	f.ok(e)
	if read.ReviewHash == "" {
		t.Fatal("no review hash")
	}
	again, e := f.ImportMarkdown(ctx, f.p("ana"), pid, MarkdownInput{Markdown: "swapped", Path: "p.md", Class: Data, Submit: true})
	f.ok(e)
	if again.SubmissionID != first.SubmissionID || again.Action != "merge" {
		t.Fatal(again)
	}
	_, e = f.MemberReview(ctx, f.p("fani"), 0, first.SubmissionID, true, "", read.ReviewHash)
	is(t, e, ErrSubmissionChanged)
	_, e = f.MemberReview(ctx, f.p("fani"), 0, first.SubmissionID, true, "", "")
	is(t, e, ErrReviewHashMissing)
	_, e = f.ReviewSubmission(ctx, first.SubmissionID, true, "", read.ReviewHash)
	is(t, e, ErrSubmissionChanged)
	_, e = f.ReviewSubmission(ctx, first.SubmissionID, true, "", "")
	is(t, e, ErrReviewHashMissing)
	now, _, e := f.GetReview(ctx, f.p("fani"), 0, first.SubmissionID)
	f.ok(e)
	acc, e := f.MemberReview(ctx, f.p("fani"), 0, first.SubmissionID, true, "", now.ReviewHash)
	f.ok(e)
	n, e := f.GetNote(ctx, f.p("ana"), pid, acc.NoteID)
	f.ok(e)
	if !strings.Contains(n.Body, "swapped") {
		t.Fatal(n.Body)
	}
}

// L1: the all-projects list reads only the projects the caller reviews: a broken row elsewhere is never touched.
func TestReviewListAllReadsOnlyReviewedProjects(t *testing.T) {
	f := newFx(t)
	f.submit("cene", f.pid["eureko"], "c1", Structure)
	mine := f.submit("ana", f.pid["fakture"], "a1", Data)
	f.ok(f.write(ctx, func(tx *sql.Tx) error {
		_, e := tx.Exec(`UPDATE kb_submissions SET provenance='not json' WHERE project_id=?`, f.pid["eureko"])
		return e
	}))
	vs, e := f.ListReview(ctx, f.p("fani"), 0, 0)
	f.ok(e)
	if len(vs) != 1 || vs[0].ID != mine.ID {
		t.Fatal(vs)
	}
}

// L2: when the WRITER lost the right to write (here: their device is unbound), the reviewer gets a refusal that says so,
// never the writer's 403 as if it were the reviewer's own.
func TestReviewWriterLostIsItsOwnAnswer(t *testing.T) {
	f := newFx(t)
	v := f.submit("ana", f.pid["fakture"], "a1", Data)
	f.ok(f.RemoveBinding(ctx, "dev-ana"))
	_, e := f.MemberReview(ctx, f.p("fani"), 0, v.ID, true, "", f.seen(v.ID))
	var lost ErrWriterLost
	if !errors.As(e, &lost) {
		t.Fatal(e)
	}
	_, e = f.MemberReview(ctx, f.p("fani"), 0, v.ID, false, "writer gone", "")
	f.ok(e)
}

// L3 (an existing rule changed): an archived project no longer makes its admins writers, or reviewers, of the team's notes.
func TestArchivedProjectConfersNoTeamRights(t *testing.T) {
	f := newSeg(t)
	pid := f.pid["fakture"]
	f.ok(f.SetMember(ctx, pid, f.uid["bor"], Admin))
	f.ok(f.AddTeamMember(ctx, f.acme, f.uid["bor"]))
	v := f.submit("bor", f.seg, "s1", Data)
	f.ok(f.SetProjectArchived(ctx, pid, true))
	_, _, e := f.CreateNote(ctx, f.p("fani"), f.seg, "w1", NoteInput{Class: Data, Title: "t", Body: "b"})
	is(t, e, ErrForbidden)
	_, _, e = f.GetReview(ctx, f.p("fani"), f.seg, v.ID)
	is(t, e, ErrNotFound)
	vs, e := f.ListReview(ctx, f.p("fani"), 0, 0)
	f.ok(e)
	if len(vs) != 0 {
		t.Fatal(vs)
	}
	f.ok(f.SetProjectArchived(ctx, pid, false))
	_, _, e = f.CreateNote(ctx, f.p("fani"), f.seg, "w2", NoteInput{Class: Data, Title: "t", Body: "b"})
	f.ok(e)
}

// M1' and L2': the gateway compares the whole hash (a 12-hex prefix of the current one is refused), and the hash covers
// the stored provenance: the same text sent again from another device (A, B, A) is not the version that was read.
func TestReviewHashWholeAndCoversProvenance(t *testing.T) {
	f := newFx(t)
	pid := f.pid["fakture"]
	first, e := f.ImportMarkdown(ctx, f.p("ana"), pid, MarkdownInput{Markdown: "text A", Path: "p.md", Class: Data, Submit: true})
	f.ok(e)
	read, _, e := f.GetReview(ctx, f.p("fani"), 0, first.SubmissionID)
	f.ok(e)
	_, e = f.MemberReview(ctx, f.p("fani"), 0, first.SubmissionID, true, "", read.ReviewHash[:12])
	is(t, e, ErrSubmissionChanged)
	f.clock = f.clock.Add(time.Second)
	_, e = f.ImportMarkdown(ctx, f.p("ana"), pid, MarkdownInput{Markdown: "text B", Path: "p.md", Class: Data, Submit: true})
	f.ok(e)
	f.clock = f.clock.Add(time.Second)
	other, e := f.Principal(ctx, "dev-ana2", true)
	f.ok(e)
	back, e := f.ImportMarkdown(ctx, other, pid, MarkdownInput{Markdown: "text A", Path: "p.md", Class: Data, Submit: true})
	f.ok(e)
	if back.SubmissionID != first.SubmissionID {
		t.Fatal(back)
	}
	now, _, e := f.GetReview(ctx, f.p("fani"), 0, first.SubmissionID)
	f.ok(e)
	if now.Body == nil || *now.Body != *read.Body || now.Provenance.Device == read.Provenance.Device || now.ReviewHash == read.ReviewHash {
		t.Fatal("same text, other device: the version must differ", now.Provenance.Device, read.Provenance.Device)
	}
	_, e = f.MemberReview(ctx, f.p("fani"), 0, first.SubmissionID, true, "", read.ReviewHash)
	is(t, e, ErrSubmissionChanged)
	// the owner's versioned read (the panel's detail) answers only for the current version
	_, e = f.GetSubmissionVersion(ctx, first.SubmissionID, read.ReviewHash)
	is(t, e, ErrSubmissionChanged)
	v, e := f.GetSubmissionVersion(ctx, first.SubmissionID, now.ReviewHash)
	f.ok(e)
	if v.ID != first.SubmissionID {
		t.Fatal(v)
	}
}

// L1: a review read shows the text without what a reviewer cannot see and says how much was left out; an accept is refused while the
// stored text has any (the hash stays over the stored text); a body longer than the owner's panel shows is marked "long" and is still
// accepted by its version (gw kb review prints all of it). CRLF is a line ending: stored as LF from now on, and accepted (published
// with LF) in a row stored before; a lone CR inside a line of such a row is hidden.
func TestReviewReadShowsHiddenCharactersAndAcceptRefusesThem(t *testing.T) {
	f := newFx(t)
	project := f.pid["fakture"]
	in := SubmissionInput{NoteInput: NoteInput{Class: Structure, Title: "Pay \u202eto\u202c", Body: "line one\r\nsend\u200b to\tX\r\n"}}
	v, _, e := f.Submit(ctx, f.p("ana"), project, "hidden", in)
	f.ok(e)
	r, e := f.GetSubmission(ctx, Principal{}, 0, v.ID, true)
	f.ok(e)
	if r.Title != "Pay to" || r.Body == nil || *r.Body != "line one\nsend to\tX\n" || r.Hidden != 3 || r.ReviewCheck != "hidden" {
		t.Fatalf("%q %q %d %q", r.Title, *r.Body, r.Hidden, r.ReviewCheck)
	}
	var stored string
	f.ok(f.DB().QueryRow(`SELECT body FROM kb_submissions WHERE id=?`, v.ID).Scan(&stored))
	if stored != "line one\nsend\u200b to\tX\n" { // line endings normalised, nothing else
		t.Fatalf("stored text: %q", stored)
	}
	rows, e := f.ListSubmissions(ctx, Principal{}, project, 0, true)
	f.ok(e)
	if len(rows) != 1 || rows[0].ReviewCheck != "hidden" || rows[0].ReviewHash != r.ReviewHash {
		t.Fatalf("%+v", rows)
	}
	_, e = f.ReviewSubmission(ctx, v.ID, true, "", r.ReviewHash)
	if e == nil || !strings.Contains(e.Error(), "contains 3 hidden characters") {
		t.Fatal(e)
	}
	if _, e = f.ReviewSubmission(ctx, v.ID, false, "hidden characters", ""); e != nil { // reject stays possible
		t.Fatal(e)
	}
	long := SubmissionInput{NoteInput: NoteInput{Class: Structure, Title: "Long", Body: strings.Repeat("é", reviewPanelLimit+1)}}
	v, _, e = f.Submit(ctx, f.p("ana"), project, "long", long)
	f.ok(e)
	r, e = f.GetSubmission(ctx, Principal{}, 0, v.ID, true)
	f.ok(e)
	if r.ReviewCheck != "long" || r.Hidden != 0 || *r.Body != long.Body {
		t.Fatalf("%q %d", r.ReviewCheck, r.Hidden)
	}
	if _, e = f.ReviewSubmission(ctx, v.ID, true, "", r.ReviewHash); e != nil {
		t.Fatal(e)
	}
	// a new CRLF submission is stored, accepted and published with LF
	v, _, e = f.Submit(ctx, f.p("ana"), project, "crlf", SubmissionInput{NoteInput: NoteInput{Class: Structure, Title: "Windows", Body: "one\r\ntwo\r\n"}})
	f.ok(e)
	a, e := f.ReviewSubmission(ctx, v.ID, true, "", f.seen(v.ID))
	f.ok(e)
	var published string
	f.ok(f.DB().QueryRow(`SELECT body FROM notes WHERE id=?`, a.NoteID).Scan(&published))
	if published != "one\ntwo\n" {
		t.Fatalf("%q", published)
	}
	// rows stored before (CRLF, or a lone CR inside a line): the version line stays the hash of the stored text
	old := func(key, body string) (int64, string) {
		t.Helper()
		v, _, e := f.Submit(ctx, f.p("ana"), project, key, SubmissionInput{NoteInput: NoteInput{Class: Structure, Title: key, Body: "x"}})
		f.ok(e)
		f.ok(f.write(ctx, func(tx *sql.Tx) error {
			_, e := tx.Exec(`UPDATE kb_submissions SET body=? WHERE id=?`, body, v.ID)
			return e
		}))
		var class, title, kind, b, prov string
		var replaces, revision int64
		f.ok(f.DB().QueryRow(`SELECT class,title,entry_kind,body,replaces,revision,provenance FROM kb_submissions WHERE id=?`, v.ID).Scan(&class, &title, &kind, &b, &replaces, &revision, &prov))
		j, _ := json.Marshal([]any{v.ID, class, title, kind, b, replaces, revision, prov})
		h := sha256.Sum256(j)
		return v.ID, fmt.Sprintf("%x", h)
	}
	id, want := old("old-crlf", "a\r\nb\r\n")
	r, e = f.GetSubmission(ctx, Principal{}, 0, id, true)
	f.ok(e)
	if r.ReviewHash != want || r.ReviewCheck != "ok" || r.Hidden != 0 || *r.Body != "a\nb\n" {
		t.Fatalf("%q %q %d %q", r.ReviewHash, r.ReviewCheck, r.Hidden, *r.Body)
	}
	a, e = f.ReviewSubmission(ctx, id, true, "", want)
	f.ok(e)
	f.ok(f.DB().QueryRow(`SELECT body FROM notes WHERE id=?`, a.NoteID).Scan(&published))
	if published != "a\nb\n" {
		t.Fatalf("%q", published)
	}
	id, want = old("old-cr", "a\rb")
	r, e = f.GetSubmission(ctx, Principal{}, 0, id, true)
	f.ok(e)
	if r.ReviewHash != want || r.ReviewCheck != "hidden" || r.Hidden != 1 {
		t.Fatalf("%q %d", r.ReviewCheck, r.Hidden)
	}
	if _, e = f.ReviewSubmission(ctx, id, true, "", want); e == nil {
		t.Fatal("a lone CR inside a line was accepted")
	}
}
