package core

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
)

// The review hash of a submission that replaces nothing is the hash 0.8.7 computed: [id, class, title, kind, body,
// replaces, revision, provenance] as JSON (Go escapes < > &), SHA-256. The constant was computed outside this code
// (Python, the same JSON) and checked against reviewHash BEFORE this step changed it; it must never change.
func TestReviewHashNewNoteUnchanged(t *testing.T) {
	f := newFx(t)
	const prov = `{"actorKind":"member","user":3,"device":"dev-ana","project":1,"sourceKind":"pushed note","sourceReference":"","contentHash":"h","authored":"2026-11-14T10:00:00Z","received":"2026-11-14T10:00:00Z","reviewer":"","reviewed":null}`
	exec(t, f.Store, `INSERT INTO kb_submissions(id,project_id,user_id,device_id,class,title,body,bytes,replaces,revision,provenance,key,hash,received) VALUES(9,1,1,'d','structure',?,?,1,0,0,?,'k','h',1)`, "Titlé <&>", "body é\n<b>", prov)
	const want = "66e6dcb849865d7038df36c9098856f38e75490aed0ec8dba13d72a07e700794"
	var got string
	f.ok(f.read(ctx, func(tx *sql.Tx) error { var e error; got, e = reviewHash(tx, 9); return e }))
	if got != want {
		t.Fatal(got)
	}
}

// replacement: a note of ana and a pending submission of hers that replaces it; fani (admin) reviews.
func replacement(f *fx, key string) (Note, Submission) {
	f.t.Helper()
	ana, pid := f.p("ana"), f.pid["fakture"]
	n, _, e := f.CreateNote(ctx, ana, pid, key+"-note", NoteInput{Class: Structure, Title: "Current title", Body: "current body"})
	f.ok(e)
	v, _, e := f.Submit(ctx, ana, pid, key, SubmissionInput{NoteInput: NoteInput{Class: Structure, Title: "New title", Body: "new body"}, Replaces: n.ID, Revision: n.Revision})
	f.ok(e)
	return n, v
}

func ownerRead(f *fx, id int64) Submission {
	f.t.Helper()
	v, e := f.GetSubmission(ctx, Principal{}, 0, id, true)
	f.ok(e)
	return v
}

// Catches: a replacement's hash not binding the target's text, revision, class or existence.
func TestReviewHashReplacementBindsTheTarget(t *testing.T) {
	f := newFx(t)
	n, v := replacement(f, "bind")
	ana, fani, pid := f.p("ana"), f.p("fani"), f.pid["fakture"]
	last := ownerRead(f, v.ID).ReviewHash
	step := func(what string, change func()) {
		t.Helper()
		change()
		if now := ownerRead(f, v.ID).ReviewHash; now == last {
			t.Fatal(what, "did not change the hash")
		} else {
			last = now
		}
	}
	step("an edit", func() {
		var e error
		n, e = f.UpdateNote(ctx, ana, pid, n.ID, n.Revision, NoteInput{Class: Structure, Title: "Current title", Body: "edited"})
		f.ok(e)
	})
	step("a class change", func() {
		var e error
		n, e = f.UpdateNote(ctx, ana, pid, n.ID, n.Revision, NoteInput{Class: Data, Title: "Current title", Body: "edited"})
		f.ok(e)
	})
	step("a restore", func() {
		var e error
		n, e = f.RestoreNote(ctx, &fani, pid, n.ID, 1, n.Revision, Data)
		f.ok(e)
	})
	step("a delete", func() { f.ok(f.DeleteNote(ctx, ana, pid, n.ID, n.Revision)) })
	// the version-bound detail of the old hash answers "changed"
	if _, e := f.GetSubmissionVersion(ctx, v.ID, "0"+last[1:]); !errors.Is(e, ErrSubmissionChanged) {
		t.Fatal(e)
	}
}

// Catches: the fields missing from a review read, the body in a list, or a target of another project / a mirror read.
func TestReviewReadCarriesTheTargetState(t *testing.T) {
	f := newFx(t)
	n, v := replacement(f, "fields")
	ana, pid := f.p("ana"), f.pid["fakture"]
	plain, _, e := f.Submit(ctx, ana, pid, "plain", SubmissionInput{NoteInput: NoteInput{Class: Structure, Title: "Plain", Body: "plain"}})
	f.ok(e)
	rows, e := f.ListSubmissions(ctx, Principal{}, 0, 0, true)
	f.ok(e)
	for _, r := range rows {
		if r.CurrentBody != nil || r.CurrentRevision == nil {
			t.Fatal("list row", r)
		}
		switch r.ID {
		case v.ID:
			if r.TargetState != "ok" || *r.CurrentRevision != n.Revision || r.CurrentTitle != "Current title" || r.CurrentClass != Structure || r.ReviewCheck != "ok" || r.ProposalCheck != "ok" || r.CurrentReviewCheck != "ok" {
				t.Fatal(r)
			}
		case plain.ID:
			if r.TargetState != "none" || *r.CurrentRevision != 0 || r.CurrentTitle != "" || r.ReviewCheck != "ok" {
				t.Fatal(r)
			}
		}
	}
	d := ownerRead(f, v.ID)
	if d.CurrentBody == nil || *d.CurrentBody != "current body" {
		t.Fatal(d)
	}
	// the member detail carries the same
	g, _, e := f.GetReview(ctx, f.p("fani"), pid, v.ID)
	f.ok(e)
	if g.TargetState != "ok" || g.CurrentBody == nil || *g.CurrentBody != "current body" || g.ReviewHash != d.ReviewHash {
		t.Fatal(g)
	}
	// stale
	n2, e := f.UpdateNote(ctx, ana, pid, n.ID, n.Revision, NoteInput{Class: Structure, Title: "Current title", Body: "second"})
	f.ok(e)
	if _, e := f.GetSubmissionVersion(ctx, v.ID, d.ReviewHash); !errors.Is(e, ErrSubmissionChanged) {
		t.Fatal("the old detail must be invalid after a target edit", e)
	}
	d = ownerRead(f, v.ID)
	if d.TargetState != "stale" || *d.CurrentRevision != n2.Revision || d.ReviewCheck != "target" || d.ProposalCheck != "ok" {
		t.Fatal(d)
	}
	// missing: deleted
	f.ok(f.DeleteNote(ctx, ana, pid, n.ID, n2.Revision))
	d = ownerRead(f, v.ID)
	if d.TargetState != "missing" || *d.CurrentRevision != 0 || d.CurrentTitle != "" || d.CurrentBody != nil || d.ReviewCheck != "target" {
		t.Fatal(d)
	}
	// missing: a note of another project, and a mirror id (inserted directly; no body is read)
	other := f.mk("cene", "eureko", Structure)
	for _, target := range []int64{other.ID, mirrorBase + 1} {
		exec(t, f.Store, `UPDATE kb_submissions SET replaces=? WHERE id=?`, target, v.ID)
		d = ownerRead(f, v.ID)
		if d.TargetState != "missing" || d.CurrentTitle != "" || d.CurrentBody != nil {
			t.Fatal(target, d)
		}
	}
}

// Catches: a reviewer who cannot read the target being shown it (no oracle), at the function level.
func TestReviewTargetUnreadableByTheReviewerIsMissing(t *testing.T) {
	f := newFx(t)
	n, v := replacement(f, "unreadable")
	fani := f.p("fani")
	exec(t, f.Store, `UPDATE notes SET class='private' WHERE id=?`, n.ID)
	old := matrix[Admin][Private]
	matrix[Admin][Private] = old &^ pRead // a test-only matrix in which an admin cannot read private notes
	t.Cleanup(func() { matrix[Admin][Private] = old })
	var state string
	f.ok(f.read(ctx, func(tx *sql.Tx) error {
		r, e := readReplaced(tx, &Submission{Project: v.Project, Replaces: n.ID, Revision: n.Revision}, &fani)
		state = r.state
		if r.title != "" || r.raw != "" {
			t.Error("the target text was read")
		}
		return e
	}))
	if state != "missing" {
		t.Fatal(state)
	}
}

// Catches: an accept of a replacement that skips a refusal, or says the wrong thing.
func TestReviewAcceptRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(f *fx, n Note)
		cur    func(n Note) int64
		want   string
	}{
		{"missing", func(f *fx, n Note) { f.ok(f.DeleteNote(ctx, f.p("ana"), n.ProjectID, n.ID, n.Revision)) }, nil, "the note this submission replaces no longer exists: reject it"},
		{"stale", func(f *fx, n Note) {
			_, e := f.UpdateNote(ctx, f.p("ana"), n.ProjectID, n.ID, n.Revision, NoteInput{Class: Structure, Title: n.Title, Body: "newer"})
			f.ok(e)
		}, nil, "note 1 changed since this submission was written (it expected revision 1, the note is at 2): reject it and ask for a new submission"},
		{"no currentRevision", func(f *fx, n Note) {}, func(Note) int64 { return 0 }, ErrOldClientReplacement},
		{"wrong currentRevision", func(f *fx, n Note) {}, func(n Note) int64 { return n.Revision + 5 }, "the submission changed since you read it: review it again"},
	} {
		for _, who := range []string{"owner", "member"} {
			t.Run(tc.name+" "+who, func(t *testing.T) {
				f := newFx(t)
				n, v := replacement(f, "refuse")
				seen := ownerRead(f, v.ID).ReviewHash
				tc.change(f, n)
				cur := n.Revision
				if tc.cur != nil {
					cur = tc.cur(n)
				}
				var e error
				before := historyCount(t, f.Store, n.ID)
				if who == "owner" {
					_, e = f.ReviewSubmissionAt(ctx, v.ID, true, "", seen, cur)
				} else {
					_, e = f.MemberReviewAt(ctx, f.p("fani"), n.ProjectID, v.ID, true, "", seen, cur)
				}
				var ref ErrRefusal
				if e == nil || !errors.As(e, &ref) || e.Error() != tc.want {
					t.Fatalf("%v, want %q", e, tc.want)
				}
				if historyCount(t, f.Store, n.ID) != before {
					t.Fatal("a refused accept saved history")
				}
			})
		}
	}
	t.Run("a hash of the old formula", func(t *testing.T) {
		f := newFx(t)
		n, v := replacement(f, "oldhash")
		var class, title, kind, body, prov string
		var replaces, revision int64
		f.ok(f.DB().QueryRow(`SELECT class,title,entry_kind,body,replaces,revision,provenance FROM kb_submissions WHERE id=?`, v.ID).Scan(&class, &title, &kind, &body, &replaces, &revision, &prov))
		b, _ := json.Marshal([]any{v.ID, class, title, kind, body, replaces, revision, prov})
		sum := sha256.Sum256(b)
		_, e := f.ReviewSubmissionAt(ctx, v.ID, true, "", hex.EncodeToString(sum[:]), n.Revision)
		if !errors.Is(e, ErrSubmissionChanged) {
			t.Fatal(e)
		}
	})
}

// Catches: an eligible replacement not being accepted, the old text not kept, or a long current text being refused.
func TestReviewAcceptReplacement(t *testing.T) {
	for _, who := range []string{"owner", "member", "long current"} {
		t.Run(who, func(t *testing.T) {
			f := newFx(t)
			n, v := replacement(f, "accept")
			if who == "long current" {
				exec(t, f.Store, `UPDATE notes SET body=? WHERE id=?`, strings.Repeat("x", reviewPanelLimit+10), n.ID)
				if d := ownerRead(f, v.ID); d.ReviewCheck != "long" || d.CurrentReviewCheck != "long" {
					t.Fatal(d)
				}
			}
			seen := ownerRead(f, v.ID).ReviewHash
			var e error
			reviewer := "owner"
			if who == "member" {
				_, e = f.MemberReviewAt(ctx, f.p("fani"), n.ProjectID, v.ID, true, "", seen, n.Revision)
				reviewer = actor(f.p("fani"))
			} else {
				_, e = f.ReviewSubmissionAt(ctx, v.ID, true, "", seen, n.Revision)
			}
			f.ok(e)
			got, e := f.GetNote(ctx, f.p("ana"), n.ProjectID, n.ID)
			f.ok(e)
			if got.Title != "New title" || got.Body != "new body" || got.Revision != n.Revision+1 || got.Provenance.Reviewer != reviewer {
				t.Fatal(got)
			}
			var detail string
			f.ok(f.DB().QueryRow(`SELECT detail FROM audit_events WHERE action='kb.accepted' ORDER BY rowid DESC LIMIT 1`).Scan(&detail))
			if !strings.Contains(detail, `"replaced":true`) || !strings.Contains(detail, fmt.Sprintf(`"savedRevision":%d`, n.Revision)) || strings.Contains(detail, "body") || strings.Contains(detail, "title") {
				t.Fatal("audit detail", detail)
			}
			var old, title string
			f.ok(f.DB().QueryRow(`SELECT title,body FROM note_history WHERE note_id=? AND revision=?`, n.ID, n.Revision).Scan(&title, &old))
			if title != "Current title" || (who != "long current" && old != "current body") {
				t.Fatal("the old text was not kept", title, len(old))
			}
		})
	}
}

// Catches: a rule for hidden characters that differs between the proposal side and the current side.
func TestReviewEvaluatorIsTheSameOnBothSides(t *testing.T) {
	data, err := os.ReadFile("../testdata/review-text.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct{ Name, In, Out, Side string }
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		if _, b, _, _ := reviewEval("t", c.In); b != c.Out {
			t.Errorf("%s: %q, want %q", c.Name, b, c.Out)
		}
	}
	f := newFx(t)
	n, v := replacement(f, "eval")
	for _, c := range cases {
		exec(t, f.Store, `UPDATE notes SET body=? WHERE id=?`, c.In, n.ID)
		exec(t, f.Store, `UPDATE kb_submissions SET body=? WHERE id=?`, c.In, v.ID)
		d := ownerRead(f, v.ID)
		if d.Body == nil || d.CurrentBody == nil || *d.Body != c.Out || *d.CurrentBody != c.Out || d.CurrentHidden != d.Hidden {
			t.Errorf("%s: proposal %v current %v hidden %d/%d", c.Name, d.Body, d.CurrentBody, d.Hidden, d.CurrentHidden)
		}
	}
}

// Catches: two pending replacements of one note both being accepted (the second must be stale).
func TestReviewSecondReplacementOfOneNoteIsStale(t *testing.T) {
	f := newFx(t)
	n, a := replacement(f, "two-a")
	ana, pid := f.p("ana"), f.pid["fakture"]
	b, _, e := f.Submit(ctx, ana, pid, "two-b", SubmissionInput{NoteInput: NoteInput{Class: Structure, Title: "Other title", Body: "other body"}, Replaces: n.ID, Revision: n.Revision})
	f.ok(e)
	seenB := ownerRead(f, b.ID).ReviewHash
	_, e = f.ReviewSubmissionAt(ctx, a.ID, true, "", ownerRead(f, a.ID).ReviewHash, n.Revision)
	f.ok(e)
	_, e = f.ReviewSubmissionAt(ctx, b.ID, true, "", seenB, n.Revision)
	var ref ErrRefusal
	if !errors.As(e, &ref) || e.Error() != "note 1 changed since this submission was written (it expected revision 1, the note is at 2): reject it and ask for a new submission" {
		t.Fatal(e)
	}
	if got, e := f.GetNote(ctx, ana, pid, n.ID); e != nil || got.Title != "New title" {
		t.Fatal(got, e)
	}
}

// Catches: a currentRevision on a submission that replaces nothing being taken.
func TestReviewCurrentRevisionOnANewNoteIsRefused(t *testing.T) {
	f := newFx(t)
	ana, pid := f.p("ana"), f.pid["fakture"]
	v, _, e := f.Submit(ctx, ana, pid, "plain-cur", SubmissionInput{NoteInput: NoteInput{Class: Structure, Title: "Plain", Body: "plain"}})
	f.ok(e)
	seen := ownerRead(f, v.ID).ReviewHash
	var inv ErrInvalid
	if _, e = f.ReviewSubmissionAt(ctx, v.ID, true, "", seen, 3); !errors.As(e, &inv) {
		t.Fatal("owner", e)
	}
	if _, e = f.MemberReviewAt(ctx, f.p("fani"), pid, v.ID, true, "", seen, 3); !errors.As(e, &inv) {
		t.Fatal("member", e)
	}
	if _, e = f.ReviewSubmissionAt(ctx, v.ID, true, "", seen, 0); e != nil {
		t.Fatal(e)
	}
}

// Catches: a target of another project being read or accepted.
func TestReviewTargetOfAnotherProjectCannotBeAccepted(t *testing.T) {
	f := newFx(t)
	n, v := replacement(f, "other-project")
	other := f.mk("cene", "eureko", Structure)
	exec(t, f.Store, `UPDATE kb_submissions SET replaces=? WHERE id=?`, other.ID, v.ID)
	d := ownerRead(f, v.ID)
	if d.TargetState != "missing" || d.CurrentBody != nil || d.ReviewCheck != "target" {
		t.Fatal(d)
	}
	_, e := f.ReviewSubmissionAt(ctx, v.ID, true, "", d.ReviewHash, n.Revision)
	var ref ErrRefusal
	if !errors.As(e, &ref) || e.Error() != "the note this submission replaces no longer exists: reject it" {
		t.Fatal(e)
	}
	if got, e := f.GetNote(ctx, f.p("cene"), other.ProjectID, other.ID); e != nil || got.Title != other.Title {
		t.Fatal("the other project's note changed", got, e)
	}
}

// Catches: hidden characters in the CURRENT text blocking an accept, the new text not being clean, or the history not keeping
// the replaced text exactly as it was.
func TestReviewAcceptWithHiddenCharactersInTheCurrentText(t *testing.T) {
	for _, who := range []string{"owner", "member"} {
		t.Run(who, func(t *testing.T) {
			f := newFx(t)
			n, v := replacement(f, "hiddencur")
			raw := "cur\u202erent\u200b body"
			exec(t, f.Store, `UPDATE notes SET body=? WHERE id=?`, raw, n.ID)
			d := ownerRead(f, v.ID)
			if d.CurrentHidden != 2 || d.ReviewCheck != "ok" || d.CurrentBody == nil || *d.CurrentBody != "current body" || d.TargetState != "ok" {
				t.Fatalf("%+v", d)
			}
			var e error
			if who == "owner" {
				_, e = f.ReviewSubmissionAt(ctx, v.ID, true, "", d.ReviewHash, n.Revision)
			} else {
				_, e = f.MemberReviewAt(ctx, f.p("fani"), n.ProjectID, v.ID, true, "", d.ReviewHash, n.Revision)
			}
			f.ok(e)
			got, e := f.GetNote(ctx, f.p("ana"), n.ProjectID, n.ID)
			f.ok(e)
			if got.Body != "new body" || got.Title != "New title" {
				t.Fatal(got)
			}
			var old string
			f.ok(f.DB().QueryRow(`SELECT body FROM note_history WHERE note_id=? AND revision=?`, n.ID, n.Revision).Scan(&old))
			if old != raw {
				t.Fatalf("history %q, want the raw old text %q", old, raw)
			}
		})
	}
}

// Catches: the sizes of the two texts missing from a review read (list rows included), or counted on the raw text.
func TestReviewReadCarriesTheSizes(t *testing.T) {
	f := newFx(t)
	n, v := replacement(f, "sizes")
	exec(t, f.Store, `UPDATE notes SET body=? WHERE id=?`, "one\ntwo\u200b\nthree\n", n.ID)
	exec(t, f.Store, `UPDATE kb_submissions SET body=? WHERE id=?`, "alpha\nbeta", v.ID)
	rows, e := f.ListSubmissions(ctx, Principal{}, 0, 0, true)
	f.ok(e)
	for _, d := range []Submission{rows[0], ownerRead(f, v.ID)} {
		if d.CurrentLines == nil || *d.CurrentLines != 3 || *d.CurrentChars != 14 || *d.NewLines != 2 || *d.NewChars != 10 {
			t.Fatalf("%+v %v %v %v %v", d, d.CurrentLines, d.CurrentChars, d.NewLines, d.NewChars)
		}
		if d.CurrentBody != nil && d.Body == nil {
			t.Fatal("list row with a body")
		}
	}
	// a new note carries none; a missing target carries only the new side
	plain, _, e := f.Submit(ctx, f.p("ana"), f.pid["fakture"], "sizes-plain", SubmissionInput{NoteInput: NoteInput{Class: Structure, Title: "P", Body: "p"}})
	f.ok(e)
	if d := ownerRead(f, plain.ID); d.NewChars != nil || d.CurrentChars != nil {
		t.Fatalf("%+v", d)
	}
	f.ok(f.DeleteNote(ctx, f.p("ana"), n.ProjectID, n.ID, n.Revision))
	if d := ownerRead(f, v.ID); d.CurrentChars != nil || d.NewLines == nil || *d.NewLines != 2 {
		t.Fatalf("%+v", d)
	}
}

// Catches: a reject of a stale or missing replacement leaving its author without a reason, or a typed reason being replaced.
func TestReviewRejectGivesAStaleOrMissingReplacementADefaultReason(t *testing.T) {
	reasonOf := func(f *fx, id int64) string {
		var r string
		f.ok(f.DB().QueryRow(`SELECT reason FROM kb_submissions WHERE id=?`, id).Scan(&r))
		return r
	}
	for _, who := range []string{"owner", "member"} {
		reject := func(f *fx, n Note, id int64, reason string) {
			var e error
			if who == "owner" {
				_, e = f.ReviewSubmission(ctx, id, false, reason, "")
			} else {
				_, e = f.MemberReview(ctx, f.p("fani"), n.ProjectID, id, false, reason, "")
			}
			f.ok(e)
		}
		t.Run(who, func(t *testing.T) {
			f := newFx(t)
			n, stale := replacement(f, who+"-stale")
			_, e := f.UpdateNote(ctx, f.p("ana"), n.ProjectID, n.ID, n.Revision, NoteInput{Class: Structure, Title: n.Title, Body: "moved on"})
			f.ok(e)
			reject(f, n, stale.ID, "")
			if got := reasonOf(f, stale.ID); got != "note 1 changed after this was written (it expected revision 1, the note is at 2): read the note and propose the change again" {
				t.Fatal(got)
			}
			// a typed reason is kept as it is
			cur, _ := f.GetNote(ctx, f.p("ana"), n.ProjectID, n.ID)
			typed, _, e := f.Submit(ctx, f.p("ana"), n.ProjectID, who+"-typed", SubmissionInput{NoteInput: NoteInput{Class: Structure, Title: "T", Body: "t"}, Replaces: n.ID, Revision: cur.Revision})
			f.ok(e)
			_, e = f.UpdateNote(ctx, f.p("ana"), n.ProjectID, n.ID, cur.Revision, NoteInput{Class: Structure, Title: n.Title, Body: "again"})
			f.ok(e)
			reject(f, n, typed.ID, "my own reason")
			if got := reasonOf(f, typed.ID); got != "my own reason" {
				t.Fatal(got)
			}
			// missing
			cur, _ = f.GetNote(ctx, f.p("ana"), n.ProjectID, n.ID)
			gone, _, e := f.Submit(ctx, f.p("ana"), n.ProjectID, who+"-gone", SubmissionInput{NoteInput: NoteInput{Class: Structure, Title: "G", Body: "g"}, Replaces: n.ID, Revision: cur.Revision})
			f.ok(e)
			f.ok(f.DeleteNote(ctx, f.p("ana"), n.ProjectID, n.ID, cur.Revision))
			reject(f, n, gone.ID, "")
			if got := reasonOf(f, gone.ID); got != "note 1 no longer exists" {
				t.Fatal(got)
			}
			// a reject that is not about a stale target stays empty: a new note, and a replacement of an unchanged note
			plain, _, e := f.Submit(ctx, f.p("ana"), n.ProjectID, who+"-plain", SubmissionInput{NoteInput: NoteInput{Class: Structure, Title: "Pl", Body: "p"}})
			f.ok(e)
			reject(f, n, plain.ID, "")
			if got := reasonOf(f, plain.ID); got != "" {
				t.Fatal(got)
			}
			n2, fine := replacement(f, who+"-fine")
			reject(f, n2, fine.ID, "")
			if got := reasonOf(f, fine.ID); got != "" {
				t.Fatal(got)
			}
		})
	}
}

// Catches: an owner review read of a replacement of a PRIVATE note leaving no audit row, an audit row that carries text, a row for
// a note that is not private, or a row for a list.
func TestReviewOwnerReadOfAPrivateTargetIsAudited(t *testing.T) {
	f := newFx(t)
	ana, pid := f.p("ana"), f.pid["fakture"]
	n, _, e := f.CreateNote(ctx, ana, pid, "priv-note", NoteInput{Class: Private, Title: "Secret title", Body: "secret body"})
	f.ok(e)
	v, _, e := f.Submit(ctx, ana, pid, "priv-repl", SubmissionInput{NoteInput: NoteInput{Class: Private, Title: "New", Body: "new"}, Replaces: n.ID, Revision: n.Revision})
	f.ok(e)
	reads := func() (n int, last string) {
		rows, err := f.DB().Query(`SELECT detail FROM audit_events WHERE action='kb.owner.read' AND subject=?`, fmt.Sprint(n0(f, v)))
		f.ok(err)
		defer rows.Close()
		for rows.Next() {
			n++
			f.ok(rows.Scan(&last))
		}
		return
	}
	if c, _ := reads(); c != 0 {
		t.Fatal("audited before a read")
	}
	if _, e = f.ListSubmissions(ctx, Principal{}, 0, 0, true); e != nil {
		t.Fatal(e)
	}
	if c, _ := reads(); c != 0 {
		t.Fatal("a list was audited")
	}
	ownerRead(f, v.ID)
	ownerRead(f, v.ID)
	c, last := reads()
	if c != 2 || strings.Contains(last, "secret") || strings.Contains(last, "Secret") || !strings.Contains(last, `"submission":`) {
		t.Fatal(c, last)
	}
	// a note that is not private: no audit row
	n2, v2 := replacement(f, "audit-plain")
	ownerRead(f, v2.ID)
	var count int
	f.ok(f.DB().QueryRow(`SELECT count(*) FROM audit_events WHERE action='kb.owner.read' AND subject=?`, fmt.Sprint(n2.ID)).Scan(&count))
	if count != 0 {
		t.Fatal("a non-private target was audited")
	}
}

func n0(f *fx, v Submission) int64 { return v.Replaces }

// Catches: the default reason telling an author who can no longer see the note its revision (computed with the author's own
// access), or a missing note being described to someone who never could see it.
func TestReviewDefaultReasonUsesTheAuthorsAccess(t *testing.T) {
	reasonOf := func(f *fx, id int64) string {
		var r string
		f.ok(f.DB().QueryRow(`SELECT reason FROM kb_submissions WHERE id=?`, id).Scan(&r))
		return r
	}
	t.Run("author can still read the note", func(t *testing.T) {
		f := newFx(t)
		n, v := replacement(f, "can-read")
		_, e := f.UpdateNote(ctx, f.p("ana"), n.ProjectID, n.ID, n.Revision, NoteInput{Class: Structure, Title: n.Title, Body: "moved on"})
		f.ok(e)
		_, e = f.ReviewSubmission(ctx, v.ID, false, "", "")
		f.ok(e)
		if got := reasonOf(f, v.ID); got != "note 1 changed after this was written (it expected revision 1, the note is at 2): read the note and propose the change again" {
			t.Fatal(got)
		}
	})
	t.Run("author lost the access", func(t *testing.T) {
		f := newFx(t)
		n, v := replacement(f, "lost")
		_, e := f.UpdateNote(ctx, f.p("ana"), n.ProjectID, n.ID, n.Revision, NoteInput{Class: Structure, Title: n.Title, Body: "moved on"})
		f.ok(e)
		f.ok(f.RemoveMember(ctx, n.ProjectID, f.uid["ana"]))
		_, e = f.ReviewSubmission(ctx, v.ID, false, "", "")
		f.ok(e)
		got := reasonOf(f, v.ID)
		if got != "the note this would replace changed or is no longer visible to you: read the project's notes and propose the change again" || strings.Contains(got, "revision") {
			t.Fatal(got)
		}
	})
	t.Run("deleted note", func(t *testing.T) {
		f := newFx(t)
		n, v := replacement(f, "deleted")
		f.ok(f.DeleteNote(ctx, f.p("ana"), n.ProjectID, n.ID, n.Revision))
		_, e := f.ReviewSubmission(ctx, v.ID, false, "", "")
		f.ok(e)
		if got := reasonOf(f, v.ID); got != "note 1 no longer exists" {
			t.Fatal(got)
		}
	})
}
