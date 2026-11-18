package core

import (
	"testing"
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
		_, e = f.MemberReview(ctx, f.p(u), 0, v.ID, true, "")
		is(t, e, ErrNotFound)
		_, e = f.MemberReview(ctx, f.p(u), pid, v.ID, false, "no")
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
	acc, e := f.MemberReview(ctx, f.p("fani"), 0, v.ID, true, "")
	f.ok(e)
	reviewer := "user:" + i64(f.uid["fani"])
	n, e := f.GetNote(ctx, f.p("ana"), pid, acc.NoteID)
	f.ok(e)
	if n.AuthorID != f.uid["ana"] || n.Provenance.User != f.uid["ana"] || n.Provenance.Reviewer != reviewer || n.Provenance.Reviewed == nil {
		t.Fatal(n)
	}
	r := f.submit("bor", pid, "b1", Data)
	rej, e := f.MemberReview(ctx, f.p("fani"), pid, r.ID, false, "not now")
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
	_, e = f.MemberReview(ctx, f.p("fani"), pid, own.ID, true, "")
	is(t, e, ErrOwnSubmission)
	_, e = f.MemberReview(ctx, f.p("fani"), pid, own.ID, false, "")
	is(t, e, ErrOwnSubmission)
	// a second admin may; so may the owner (here: the second admin)
	f.ok(f.SetMember(ctx, pid, f.uid["bor"], Admin))
	acc, e := f.MemberReview(ctx, f.p("bor"), pid, own.ID, true, "")
	f.ok(e)
	if acc.State != "accepted" {
		t.Fatal(acc)
	}
	own2 := f.submit("fani", pid, "f2", Data)
	acc, e = f.ReviewSubmission(ctx, own2.ID, true, "")
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
	_, e = f.MemberReview(ctx, f.p("eva"), 0, v.ID, true, "")
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
	_, e := f.MemberReview(ctx, f.p("bor"), f.seg, v.ID, true, "")
	is(t, e, ErrOwnSubmission)
	vs, e := f.ListReview(ctx, f.p("fani"), f.seg, 0)
	f.ok(e)
	if len(vs) != 1 {
		t.Fatal(vs)
	}
	// an archived team: no review (its notes are not visible at all)
	f.ok(f.SetTeamArchived(ctx, f.acme, true))
	_, e = f.MemberReview(ctx, f.p("fani"), f.seg, v.ID, true, "")
	is(t, e, ErrNotFound)
	vs, e = f.ListReview(ctx, f.p("fani"), 0, 0)
	f.ok(e)
	if len(vs) != 0 {
		t.Fatal("archived team listed", vs)
	}
	f.ok(f.SetTeamArchived(ctx, f.acme, false))
	acc, e := f.MemberReview(ctx, f.p("fani"), 0, v.ID, true, "")
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
	_, e = f.MemberReview(ctx, f.p("fani"), 0, v.ID, true, "")
	is(t, e, ErrArchived)
	_, e = f.MemberReview(ctx, f.p("fani"), 0, v.ID, false, "")
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
	_, e = f.MemberReview(ctx, p, 0, v.ID, true, "")
	is(t, e, ErrNoProjectAccess)
	f.ok(f.SetUserActive(ctx, f.uid["fani"], true))
	f.ok(f.RemoveBinding(ctx, "dev-fani"))
	_, _, e = f.GetReview(ctx, p, 0, v.ID)
	is(t, e, ErrNoProjectAccess)
	_, e = f.MemberReview(ctx, p, pid, v.ID, true, "")
	is(t, e, ErrNoProjectAccess)
	// demoted: the role, not a grant, decides
	f.ok(f.SetBinding(ctx, "dev-fani", f.uid["fani"]))
	f.ok(f.SetMember(ctx, pid, f.uid["fani"], Developer))
	_, e = f.MemberReview(ctx, f.p("fani"), pid, v.ID, true, "")
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
	acc, e := f.MemberReview(ctx, f.p("fani"), pid, v.ID, true, "")
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
	_, e := f.MemberReview(ctx, f.p("fani"), pid, priv.ID, true, "")
	is(t, e, ErrForbidden)
	n, _, e := f.CreateNote(ctx, f.p("ana"), pid, "n1", NoteInput{Class: Data, Title: "mine", Body: "x"})
	f.ok(e)
	rep, _, e := f.Submit(ctx, f.p("ana"), pid, "r1", SubmissionInput{NoteInput: NoteInput{Class: Data, Title: "mine2", Body: "y"}, Replaces: n.ID, Revision: n.Revision})
	f.ok(e)
	_, e = f.MemberReview(ctx, f.p("fani"), pid, rep.ID, true, "")
	is(t, e, ErrForbidden)
	// rejecting needs no write; the owner still accepts
	_, e = f.MemberReview(ctx, f.p("fani"), pid, priv.ID, false, "no")
	f.ok(e)
	_, e = f.ReviewSubmission(ctx, rep.ID, true, "")
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
