package core

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func versionRevs(h NoteHistory) []int64 {
	out := []int64{}
	for _, v := range h.History {
		out = append(out, v.Revision)
	}
	return out
}

func TestNoteHistoryAccess(t *testing.T) {
	for _, tc := range []struct {
		name, user                       string
		current                          Class
		own, grant, deactivated, removed bool
		want                             []int64
		denied                           bool
	}{
		{name: "reader other", user: "eva", current: Structure, want: []int64{1}},
		{name: "reader author", user: "ana", current: Structure, own: true, want: []int64{3, 1}},
		{name: "developer author", user: "ana", current: Data, want: []int64{3, 2, 1}},
		{name: "developer other", user: "bor", current: Data, want: []int64{2, 1}},
		{name: "admin", user: "fani", current: Private, want: []int64{3, 2, 1}},
		{name: "grant", user: "cene", current: Data, grant: true, want: []int64{2, 1}},
		{name: "data to private", user: "bor", current: Private, denied: true},
		{name: "reader invisible", user: "eva", current: Data, denied: true},
		{name: "deactivated", user: "ana", current: Data, deactivated: true, denied: true},
		{name: "removed", user: "ana", current: Data, removed: true, denied: true},
		{name: "owner", current: Private, want: []int64{3, 2, 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFx(t)
			pid := f.pid["fakture"]
			author := f.p("ana")
			admin := f.p("fani")
			n, _, e := f.CreateNote(ctx, author, pid, "versions", NoteInput{Class: Structure, Title: "first", Body: "one"})
			f.ok(e)
			for i, c := range []Class{Data, Private, tc.current} {
				n, e = f.UpdateNote(ctx, admin, pid, n.ID, n.Revision, NoteInput{Class: c, Title: fmt.Sprint("title", i), Body: "body"})
				f.ok(e)
			}
			var p *Principal
			if tc.user != "" {
				v := f.p(tc.user)
				p = &v
			}
			if tc.own {
				f.ok(f.SetMember(ctx, pid, f.uid[tc.user], Reader))
			}
			if tc.grant {
				f.ok(f.AddGrant(ctx, pid, f.pid["eureko"], Data))
				pid = f.pid["eureko"]
			}
			if tc.deactivated {
				f.ok(f.SetUserActive(ctx, p.UserID, false))
			}
			if tc.removed {
				f.ok(f.RemoveMember(ctx, pid, p.UserID))
			}
			h, e := f.ListNoteHistory(ctx, p, pid, n.ID)
			if tc.denied {
				if e == nil {
					t.Fatal("invisible history returned")
				}
				if !tc.deactivated {
					is(t, e, ErrNotFound)
				}
				_, e = f.GetNoteVersion(ctx, p, pid, n.ID, 1)
				if e == nil {
					t.Fatal("invisible row returned")
				}
				if !tc.deactivated {
					is(t, e, ErrNotFound)
				}
				return
			}
			f.ok(e)
			if !reflect.DeepEqual(versionRevs(h), tc.want) {
				t.Fatalf("got %v want %v", versionRevs(h), tc.want)
			}
			for rev := int64(1); rev <= 3; rev++ {
				v, e := f.GetNoteVersion(ctx, p, pid, n.ID, rev)
				allowed := false
				for _, r := range tc.want {
					allowed = allowed || r == rev
				}
				if allowed {
					f.ok(e)
					if v.Body == nil || v.Bytes != int64(len(v.Title)+len(*v.Body)) {
						t.Fatal("wrong row", v)
					}
				} else {
					is(t, e, ErrNotFound)
				}
			}
			// Hidden rows do not affect any returned field.
			for rev := int64(1); rev <= 3; rev++ {
				allowed := false
				for _, r := range tc.want {
					allowed = allowed || r == rev
				}
				if !allowed {
					exec(t, f.Store, `UPDATE note_history SET title='hidden changed',body='hidden',saved=99,saved_by=0 WHERE note_id=? AND revision=?`, n.ID, rev)
				}
			}
			after, e := f.ListNoteHistory(ctx, p, pid, n.ID)
			f.ok(e)
			if !reflect.DeepEqual(h, after) {
				t.Fatal("filtered row changed answer")
			}
			_, e = f.GetNoteVersion(ctx, p, pid, n.ID, 999)
			is(t, e, ErrNotFound)
			_, e = f.ListNoteHistory(ctx, p, pid, mirrorBase)
			is(t, e, ErrNotFound)
		})
	}
	t.Run("team", func(t *testing.T) {
		f := newSeg(t)
		n := f.put("fani", Structure, "old")
		p := f.p("fani")
		_, e := f.UpdateNote(ctx, p, f.seg, n.ID, n.Revision, NoteInput{Class: Data, Title: "new", Body: "new"})
		f.ok(e)
		reader := f.p("eva")
		h, e := f.ListNoteHistory(ctx, &reader, f.seg, n.ID)
		is(t, e, ErrNotFound)
		dev := f.p("ana")
		h, e = f.ListNoteHistory(ctx, &dev, f.pid["fakture"], n.ID)
		f.ok(e)
		if !reflect.DeepEqual(versionRevs(h), []int64{1}) || h.Note.Team == "" {
			t.Fatal(h)
		}
		// Even a malformed old segment row cannot grant private history access.
		exec(t, f.Store, `UPDATE note_history SET class='private' WHERE note_id=?`, n.ID)
		h, e = f.ListNoteHistory(ctx, &p, f.seg, n.ID)
		f.ok(e)
		if len(h.History) != 0 {
			t.Fatal("private team version listed")
		}
		_, e = f.GetNoteVersion(ctx, &p, f.seg, n.ID, 1)
		is(t, e, ErrNotFound)
		_, e = f.RestoreNote(ctx, &p, f.seg, n.ID, 1, 2, Data)
		is(t, e, ErrForbidden)
	})
}

func TestNoteRestore(t *testing.T) {
	for _, who := range []string{"fani", "ana", "bor", "eva", "cene", "owner"} {
		t.Run(who, func(t *testing.T) {
			f := newFx(t)
			pid := f.pid["fakture"]
			author := f.p("ana")
			admin := f.p("fani")
			n, _, e := f.CreateNote(ctx, author, pid, "restore", NoteInput{Class: Structure, Title: "original", Body: "oldneedle"})
			f.ok(e)
			old := n
			n, e = f.UpdateNote(ctx, admin, pid, n.ID, n.Revision, NoteInput{Class: Data, Title: "replacement", Body: "newneedle"})
			f.ok(e)
			var p *Principal
			if who != "owner" {
				v := f.p(who)
				p = &v
			}
			f.ok(f.AddUpstream(ctx, "archive", "127.0.0.1:8766"))
			yes := true
			f.ok(f.LinkUpstreamWith(ctx, LinkSpec{Project: pid, Upstream: "archive", Remote: "orchard", Auto: &yes}))
			exec(t, f.Store, `UPDATE upstream_links SET dirty_since=0 WHERE project_id=?`, pid)
			receiptJSON, _ := json.Marshal(n)
			exec(t, f.Store, `UPDATE idempotency SET result=? WHERE note_id=?`, string(receiptJSON), n.ID)
			beforeEpoch, e := f.Epoch(ctx)
			f.ok(e)
			restored, e := f.RestoreNote(ctx, p, pid, n.ID, 1, 2, Data)
			if who != "fani" && who != "owner" {
				if e == nil {
					t.Fatal("non-admin restored")
				}
				return
			}
			f.ok(e)
			if restored.Revision != 3 || restored.AuthorID != old.AuthorID || restored.Body != old.Body || restored.Class != Data || restored.Provenance.SourceKind != "restored" || restored.Provenance.RestoredFrom != 1 {
				t.Fatal(restored)
			}
			checkHistory(t, f.Store, n)
			var dirty int64
			f.ok(f.DB().QueryRow(`SELECT dirty_since FROM upstream_links WHERE project_id=?`, pid).Scan(&dirty))
			if dirty == 0 {
				t.Fatal("upstream not dirty")
			}
			var by int64
			f.ok(f.DB().QueryRow(`SELECT saved_by FROM note_history WHERE note_id=? AND revision=2`, n.ID).Scan(&by))
			if p == nil && by != 0 || p != nil && by != p.UserID {
				t.Fatal("saved_by", by)
			}
			afterEpoch, e := f.Epoch(ctx)
			f.ok(e)
			if beforeEpoch != afterEpoch {
				t.Fatal("same class bumped epoch")
			}
			var receipt string
			f.ok(f.DB().QueryRow(`SELECT result FROM idempotency WHERE note_id=?`, n.ID).Scan(&receipt))
			if receipt == "" {
				t.Fatal("same class cleared receipt")
			}
			for _, q := range []string{"oldneedle", "newneedle"} {
				pg, e := f.SearchNotes(ctx, admin, pid, q)
				f.ok(e)
				want := 1
				if q == "newneedle" {
					want = 0
				}
				if len(pg.Hits) != want {
					t.Fatal(q, pg)
				}
			}
			_, e = f.RestoreNote(ctx, p, pid, n.ID, 1, 2, Data)
			var conflict ErrRevision
			if !errors.As(e, &conflict) || conflict.Current != 3 {
				t.Fatal(e)
			}
			_, e = f.RestoreNote(ctx, p, pid, n.ID, 1, 3, Data)
			var refusal ErrRefusal
			if !errors.As(e, &refusal) || !strings.Contains(e.Error(), "nothing to restore") {
				t.Fatal(e)
			}
			restored, e = f.RestoreNote(ctx, p, pid, n.ID, 1, 3, Structure)
			f.ok(e)
			afterEpoch, e = f.Epoch(ctx)
			f.ok(e)
			if afterEpoch != beforeEpoch+1 {
				t.Fatal("class change did not bump epoch")
			}
			f.ok(f.DB().QueryRow(`SELECT result FROM idempotency WHERE note_id=?`, n.ID).Scan(&receipt))
			if receipt != "" {
				t.Fatal("class change retained receipt")
			}
			var detail string
			f.ok(f.DB().QueryRow(`SELECT detail FROM audit_events WHERE action='note.restore' ORDER BY rowid DESC LIMIT 1`).Scan(&detail))
			var fields map[string]any
			f.ok(json.Unmarshal([]byte(detail), &fields))
			if fields["from"] != float64(1) || fields["revision"] != float64(4) || fields["title"] != nil || fields["body"] != nil {
				t.Fatal(detail)
			}
			f.ok(f.SetProjectArchived(ctx, pid, true))
			_, e = f.RestoreNote(ctx, p, pid, n.ID, 2, 4, Structure)
			is(t, e, ErrArchived)
		})
	}
	t.Run("race", func(t *testing.T) {
		f := newFx(t)
		p := f.p("fani")
		n := f.mk("ana", "fakture", Structure)
		n, e := f.UpdateNote(ctx, p, n.ProjectID, n.ID, n.Revision, NoteInput{Class: Structure, Title: "changed", Body: "changed"})
		f.ok(e)
		var wg sync.WaitGroup
		errs := make(chan error, 2)
		for i := 0; i < 2; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, e := f.RestoreNote(ctx, &p, n.ProjectID, n.ID, 1, n.Revision, Structure)
				errs <- e
			}()
		}
		wg.Wait()
		close(errs)
		wins, conflicts := 0, 0
		for e := range errs {
			var c ErrRevision
			if e == nil {
				wins++
			} else if errors.As(e, &c) {
				conflicts++
			} else {
				t.Fatal(e)
			}
		}
		if wins != 1 || conflicts != 1 {
			t.Fatal(wins, conflicts)
		}
	})
	t.Run("limits and hundred", func(t *testing.T) {
		f := newFx(t)
		p := f.p("fani")
		n := f.mk("ana", "fakture", Structure)
		for i := 0; i < 100; i++ {
			var e error
			n, e = f.UpdateNote(ctx, p, n.ProjectID, n.ID, n.Revision, NoteInput{Class: Structure, Title: fmt.Sprint("title", i), Body: "changed"})
			f.ok(e)
		}
		if historyCount(t, f.Store, n.ID) != 100 {
			t.Fatal("setup")
		}
		n, e := f.RestoreNote(ctx, &p, n.ProjectID, n.ID, 1, n.Revision, Structure)
		f.ok(e)
		if historyCount(t, f.Store, n.ID) != 100 {
			t.Fatal("cap")
		}
		exec(t, f.Store, `UPDATE note_history SET entry_kind='faq',body=? WHERE note_id=? AND revision=100`, strings.Repeat("x", 2049), n.ID)
		_, e = f.RestoreNote(ctx, &p, n.ProjectID, n.ID, 100, n.Revision, Structure)
		var invalid ErrInvalid
		if !errors.As(e, &invalid) {
			t.Fatal("faq limit", e)
		}
		exec(t, f.Store, `UPDATE note_history SET entry_kind='finding',body=? WHERE note_id=? AND revision=100`, strings.Repeat("x", maxBody+1), n.ID)
		_, e = f.RestoreNote(ctx, &p, n.ProjectID, n.ID, 100, n.Revision, Structure)
		is(t, e, ErrTooLarge)
		oldMax := maxBytes
		exec(t, f.Store, `UPDATE note_history SET body='within note limit' WHERE note_id=? AND revision=100`, n.ID)
		maxBytes = 1
		t.Cleanup(func() { maxBytes = oldMax })
		_, e = f.RestoreNote(ctx, &p, n.ProjectID, n.ID, 100, n.Revision, Structure)
		is(t, e, ErrTooLarge)
	})
	t.Run("kind and invalid class", func(t *testing.T) {
		f := newFx(t)
		p := f.p("fani")
		n := f.mk("ana", "fakture", Data)
		n, e := f.UpdateNote(ctx, p, n.ProjectID, n.ID, n.Revision, NoteInput{Class: Data, Title: "new", Body: "new"})
		f.ok(e)
		exec(t, f.Store, `UPDATE note_history SET entry_kind='faq' WHERE note_id=? AND revision=1`, n.ID)
		_, e = f.RestoreNote(ctx, &p, n.ProjectID, n.ID, 1, n.Revision, Private)
		var invalid ErrInvalid
		if !errors.As(e, &invalid) {
			t.Fatal(e)
		}
		restored, e := f.RestoreNote(ctx, &p, n.ProjectID, n.ID, 1, n.Revision, Data)
		f.ok(e)
		if restored.EntryKind != KindFAQ {
			t.Fatal("kind not copied")
		}
	})
	t.Run("team assigned admin", func(t *testing.T) {
		f := newSeg(t)
		n := f.mk("ana", "fakture", Structure)
		f.ok(f.SetTeamAssignment(ctx, f.acme, n.ProjectID, f.uid["ana"], Admin))
		p := f.p("ana")
		n, e := f.UpdateNote(ctx, p, n.ProjectID, n.ID, n.Revision, NoteInput{Class: Structure, Title: "new", Body: "new"})
		f.ok(e)
		_, e = f.RestoreNote(ctx, &p, n.ProjectID, n.ID, 1, n.Revision, Structure)
		f.ok(e)
	})
}
