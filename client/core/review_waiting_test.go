package core

import "testing"

// ReviewWaiting counts what ListReview lists and is refused exactly where ListReview is.

func (f *fx) waiting(user string, project int64) int {
	f.t.Helper()
	n, e := f.ReviewWaiting(ctx, f.p(user), project)
	f.ok(e)
	return n
}

func TestReviewWaitingFollowsTheListing(t *testing.T) {
	f := newFx(t)
	pid := f.pid["fakture"]
	if f.waiting("fani", pid) != 0 || f.waiting("fani", 0) != 0 {
		t.Fatal("nothing waits yet")
	}
	a := f.submit("ana", pid, "a1", Data)
	b := f.submit("bor", pid, "b1", Data)
	f.submit("fani", pid, "f1", Data) // the reviewer's own never counts
	if n := f.waiting("fani", pid); n != 2 {
		t.Fatal(n)
	}
	if n := f.waiting("fani", 0); n != 2 {
		t.Fatal("project 0 counts only what fani reviews", n)
	}
	_, e := f.MemberReview(ctx, f.p("fani"), pid, a.ID, true, "", f.seen(a.ID))
	f.ok(e)
	if n := f.waiting("fani", pid); n != 1 {
		t.Fatal("accepted", n)
	}
	_, e = f.MemberReview(ctx, f.p("fani"), pid, b.ID, false, "no", "")
	f.ok(e)
	if n := f.waiting("fani", pid); n != 0 {
		t.Fatal("rejected", n)
	}
	f.submit("ana", pid, "a2", Data)
	f.ok(f.SetProjectArchived(ctx, pid, true))
	_, e = f.ReviewWaiting(ctx, f.p("fani"), pid)
	is(t, e, ErrArchived)
	if n := f.waiting("fani", 0); n != 0 {
		t.Fatal("an archived project is not counted", n)
	}
}

func TestReviewWaitingNonReviewerLearnsNothing(t *testing.T) {
	f := newFx(t)
	pid := f.pid["fakture"]
	const none = 987654                                    // a project that does not exist: the answer for one without submissions
	users := []string{"ana", "bor", "eva", "cene", "dana"} // developer, other developer, reader, grant reader, admin of another project
	all := map[string]int{}
	for _, u := range users {
		all[u] = f.waiting(u, 0)
	}
	f.submit("ana", pid, "a1", Data)
	f.submit("bor", pid, "b1", Data)
	for _, u := range users {
		n, e := f.ReviewWaiting(ctx, f.p(u), pid)
		_, e2 := f.ReviewWaiting(ctx, f.p(u), none)
		is(t, e, ErrNotFound)
		is(t, e2, ErrNotFound)
		if n != 0 || e.Error() != e2.Error() {
			t.Fatal("differs from a project without submissions", u, n, e, e2)
		}
		if f.waiting(u, 0) != all[u] {
			t.Fatal("project 0 changed", u)
		}
	}
}

func TestReviewWaitingTeamNotes(t *testing.T) {
	f := newSeg(t)
	f.ok(f.SetMember(ctx, f.pid["fakture"], f.uid["bor"], Admin))
	f.ok(f.AddTeamMember(ctx, f.acme, f.uid["bor"]))
	f.submit("bor", f.seg, "s1", Data)
	if n := f.waiting("fani", f.seg); n != 1 {
		t.Fatal(n)
	}
	for _, u := range []string{"ana", "eva", "dana", "bor"} { // roster reader, other team's admin, no roster entry; the writer's own
		if u == "bor" {
			if n := f.waiting(u, f.seg); n != 0 {
				t.Fatal("own", n)
			}
			continue
		}
		_, e := f.ReviewWaiting(ctx, f.p(u), f.seg)
		is(t, e, ErrNotFound)
	}
	f.ok(f.SetTeamArchived(ctx, f.acme, true))
	if n := f.waiting("fani", 0); n != 0 {
		t.Fatal("archived team counted", n)
	}
}
