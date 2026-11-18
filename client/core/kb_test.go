package core

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestSubmissionLifecycle(t *testing.T) {
	f := newFx(t)
	p := f.p("ana")
	project := f.pid["fakture"]
	in := SubmissionInput{NoteInput: NoteInput{Class: Structure, Title: "Proposed", Body: "pending secret"}}
	_, _, e := f.Submit(ctx, f.p("eva"), project, "reader", in)
	is(t, e, ErrForbidden)
	_, _, e = f.Submit(ctx, f.p("cene"), project, "grant", in)
	is(t, e, ErrForbidden)
	v, replay, e := f.Submit(ctx, p, project, "key", in)
	f.ok(e)
	if replay || v.State != "pending" || v.Provenance.Device != p.DeviceID || v.Provenance.User != p.UserID || v.Provenance.ContentHash != contentHash(in.Title, in.Body) {
		t.Fatalf("%+v", v)
	}
	again, replay, e := f.Submit(ctx, p, project, "key", in)
	f.ok(e)
	if !replay || again.ID != v.ID {
		t.Fatal("replay", again)
	}
	changed := in
	changed.Body = "different"
	_, _, e = f.Submit(ctx, p, project, "key", changed)
	is(t, e, ErrIdempotency)
	for _, user := range []string{"ana", "bor", "fani", "eva", "cene"} {
		pg, e := f.ListNotes(ctx, f.p(user), project, "", "", 100)
		f.ok(e)
		if len(pg.Notes) != 0 {
			t.Fatal("pending in notes", user)
		}
		_, e = f.GetNote(ctx, f.p(user), project, v.ID)
		is(t, e, ErrNotFound)
		vs, e := f.ListSubmissions(ctx, f.p(user), project, 0, false)
		f.ok(e)
		if (len(vs) == 1) != (user == "ana") {
			t.Fatal("submission list leaked", user, vs)
		}
		_, e = f.GetSubmission(ctx, f.p(user), project, v.ID, false)
		if user == "ana" {
			f.ok(e)
		} else {
			is(t, e, ErrNotFound)
		}
	}
	cap := RunCap{RunID: "r", UserID: p.UserID, DeviceID: p.DeviceID, ProjectID: project, Epoch: f.epoch.Load()}
	pg, e := f.RunListNotes(ctx, cap, true, "", "", 100)
	f.ok(e)
	if len(pg.Notes) != 0 {
		t.Fatal("pending run count")
	}
	_, e = f.RunGetNote(ctx, cap, true, v.ID)
	is(t, e, ErrNotFound)
	rejected, e := f.ReviewSubmission(ctx, v.ID, false, "Needs work", "")
	f.ok(e)
	if rejected.State != "rejected" {
		t.Fatal(rejected)
	}
	_, e = f.ReviewSubmission(ctx, v.ID, true, "", f.seen(v.ID))
	var inv ErrInvalid
	if !errors.As(e, &inv) {
		t.Fatal(e)
	}
	v, _, e = f.Submit(ctx, p, project, "second", in)
	f.ok(e)
	accepted, e := f.ReviewSubmission(ctx, v.ID, true, "", f.seen(v.ID))
	f.ok(e)
	same, e := f.ReviewSubmission(ctx, v.ID, true, "", f.seen(v.ID))
	f.ok(e)
	if same.NoteID != accepted.NoteID {
		t.Fatal("double accept")
	}
	n, e := f.GetNote(ctx, p, project, accepted.NoteID)
	f.ok(e)
	if n.Body != in.Body || n.Provenance.Reviewer != adminActor || n.Provenance.Reviewed == nil || n.Provenance.Device != p.DeviceID {
		t.Fatal(n)
	}
	replacement := SubmissionInput{NoteInput: NoteInput{Class: Data, Title: "Replacement", Body: "reviewed body"}, Replaces: n.ID, Revision: n.Revision}
	v, _, e = f.Submit(ctx, p, project, "replace", replacement)
	f.ok(e)
	old, e := f.GetNote(ctx, p, project, n.ID)
	f.ok(e)
	if old.Body != in.Body {
		t.Fatal("pending replacement published")
	}
	v, e = f.ReviewSubmission(ctx, v.ID, true, "", f.seen(v.ID))
	f.ok(e)
	n, e = f.GetNote(ctx, p, project, n.ID)
	f.ok(e)
	if n.Revision != 2 || n.Body != replacement.Body || v.NoteID != n.ID {
		t.Fatal(n, v)
	}
	_, replay, e = f.Submit(ctx, p, project, "replace", replacement)
	f.ok(e)
	if !replay {
		t.Fatal("accepted replacement replay")
	}
	var count int
	f.ok(f.DB().QueryRow(`SELECT count(*) FROM note_revisions WHERE note_id=?`, n.ID).Scan(&count))
	if count != 2 {
		t.Fatal("history", count)
	}
	// Direct edits still publish and record the actual writer, independently of note ownership.
	n, e = f.UpdateNote(ctx, f.p("fani"), project, n.ID, 2, NoteInput{Class: Data, Title: "Edited", Body: "direct"})
	f.ok(e)
	if n.AuthorID != p.UserID || n.Provenance.User != f.uid["fani"] || n.Provenance.Reviewer != "" {
		t.Fatal(n)
	}
}

func TestSubmissionLimitsAndRevalidation(t *testing.T) {
	f := newFx(t)
	p := f.p("ana")
	project := f.pid["fakture"]
	in := SubmissionInput{NoteInput: NoteInput{Class: Data, Title: "t", Body: "b"}}
	v, _, e := f.Submit(ctx, p, project, "k", in)
	f.ok(e)
	oldU, oldP, oldUB, oldPB := maxPendingUser, maxPendingProject, maxPendingUserBytes, maxPendingProjectBytes
	defer func() {
		maxPendingUser, maxPendingProject, maxPendingUserBytes, maxPendingProjectBytes = oldU, oldP, oldUB, oldPB
	}()
	maxPendingUser = 1
	_, _, e = f.Submit(ctx, p, project, "u", in)
	is(t, e, ErrPendingLimit)
	maxPendingUser = oldU
	maxPendingProject = 1
	_, _, e = f.Submit(ctx, f.p("bor"), project, "p", in)
	is(t, e, ErrPendingLimit)
	maxPendingProject = oldP
	maxPendingUserBytes = 2
	_, _, e = f.Submit(ctx, p, project, "ub", in)
	is(t, e, ErrPendingLimit)
	maxPendingUserBytes = oldUB
	maxPendingProjectBytes = 2
	_, _, e = f.Submit(ctx, f.p("bor"), project, "pb", in)
	is(t, e, ErrPendingLimit)
	maxPendingProjectBytes = oldPB
	for _, reason := range []string{strings.Repeat("x", 1025), "bad\nreason", "bad\x1breason"} {
		_, e = f.ReviewSubmission(ctx, v.ID, false, reason, "")
		var inv ErrInvalid
		if !errors.As(e, &inv) {
			t.Fatal(e)
		}
	}
	in.Title = "bad\x1btitle"
	_, _, e = f.Submit(ctx, p, project, "control", in)
	var inv ErrInvalid
	if !errors.As(e, &inv) {
		t.Fatal(e)
	}
	f.ok(f.SetMember(ctx, project, p.UserID, Reader))
	_, e = f.ReviewSubmission(ctx, v.ID, true, "", f.seen(v.ID))
	is(t, e, ErrForbidden)
	in.Title = "t"
	_, _, e = f.Submit(ctx, p, project, "k", in)
	is(t, e, ErrForbidden)
	f.ok(f.SetMember(ctx, project, p.UserID, Developer))
	f.ok(f.SetBinding(ctx, p.DeviceID, f.uid["bor"]))
	_, _, e = f.Submit(ctx, p, project, "spoof", in)
	is(t, e, ErrNoProjectAccess)
	_, e = f.ReviewSubmission(ctx, v.ID, true, "", f.seen(v.ID))
	is(t, e, ErrNoProjectAccess)
}

func TestSubmissionBodiesAuthorizedFirst(t *testing.T) {
	f := newFx(t)
	project := f.pid["fakture"]
	v, _, e := f.Submit(ctx, f.p("ana"), project, "k", SubmissionInput{NoteInput: NoteInput{Class: Data, Title: "x", Body: "secret"}})
	f.ok(e)
	f.ok(f.write(ctx, func(tx *sql.Tx) error {
		_, e := tx.Exec(`ALTER TABLE kb_submissions RENAME TO stored_submissions;
  CREATE VIEW kb_submissions AS SELECT id,project_id,user_id,device_id,class,title,json_extract('invalid','$') AS body,bytes,replaces,revision,state,provenance,reason,note_id,entry_kind,key,hash,received FROM stored_submissions`)
		return e
	}))
	_, e = f.GetSubmission(ctx, f.p("bor"), project, v.ID, false)
	is(t, e, ErrNotFound)
	vs, e := f.ListSubmissions(ctx, f.p("ana"), project, 0, false)
	f.ok(e)
	if len(vs) != 1 || vs[0].Body != nil {
		t.Fatal(vs)
	}
	_, e = f.GetSubmission(ctx, f.p("ana"), project, v.ID, false)
	if e == nil {
		t.Fatal("body expression did not run")
	}
}

func TestSubmissionInactiveAuthorCannotPublish(t *testing.T) {
	f := newFx(t)
	p, project := f.p("ana"), f.pid["fakture"]
	v, _, err := f.Submit(ctx, p, project, "inactive", SubmissionInput{NoteInput: NoteInput{Class: Structure, Title: "Proposal", Body: "pending"}})
	f.ok(err)
	f.ok(f.SetUserActive(ctx, p.UserID, false))
	_, err = f.ReviewSubmission(ctx, v.ID, true, "", f.seen(v.ID))
	is(t, err, ErrNoProjectAccess)
	// The owner can still reject content whose writer has lost access.
	v, err = f.ReviewSubmission(ctx, v.ID, false, "Writer inactive", "")
	f.ok(err)
	if v.State != "rejected" || v.NoteID != 0 {
		t.Fatal(v)
	}
}

func TestSubmissionStaleReplacement(t *testing.T) {
	f := newFx(t)
	p, project := f.p("ana"), f.pid["fakture"]
	in := NoteInput{Class: Structure, Title: "Original", Body: "published"}
	n, _, e := f.CreateNote(ctx, p, project, "original", in)
	f.ok(e)
	v, _, e := f.Submit(ctx, p, project, "replacement", SubmissionInput{NoteInput: NoteInput{Class: Data, Title: "Proposed", Body: "pending"}, Replaces: n.ID, Revision: n.Revision})
	f.ok(e)
	in.Body = "newer published"
	_, e = f.UpdateNote(ctx, p, project, n.ID, n.Revision, in)
	f.ok(e)
	_, e = f.ReviewSubmission(ctx, v.ID, true, "", f.seen(v.ID))
	var conflict ErrRevision
	if !errors.As(e, &conflict) || conflict.Current != 2 {
		t.Fatal(e)
	}
	got, e := f.GetNote(ctx, p, project, n.ID)
	f.ok(e)
	if got.Body != in.Body || got.Revision != 2 {
		t.Fatal(got)
	}
	v, e = f.GetSubmission(ctx, p, project, v.ID, false)
	f.ok(e)
	if v.State != "pending" || v.Provenance.Reviewed != nil {
		t.Fatal(v)
	}
}

func TestProvenanceMigration10BackupRestore(t *testing.T) {
	base := migrations
	defer func() { migrations = base }()
	migrations = base[:10]
	dir := t.TempDir()
	s := openT(t, dir)
	u, e := s.CreateUser(ctx, "Legacy")
	is(t, e, nil)
	p, e := s.CreateProject(ctx, "legacy", "Legacy")
	is(t, e, nil)
	is(t, s.SetBinding(ctx, "device", u.ID), nil)
	is(t, s.SetMember(ctx, p.ID, u.ID, Developer), nil)
	input := NoteInput{Class: Data, Title: "Original", Body: "original body"}
	encoded, _ := json.Marshal(input)
	hash := fmt.Sprintf("%x", sha256.Sum256(encoded))
	is(t, s.write(ctx, func(tx *sql.Tx) error {
		_, e := tx.Exec(`INSERT INTO notes(project_id,class,title,body,bytes,author_id,created,updated,revision) VALUES(?,?,?,?,?,?,?,?,?)`, p.ID, Data, "Legacy", "body", 10, u.ID, 100, 200, 3)
		if e != nil {
			return e
		}
		// The schema 10 receipt shape has no provenance and may precede the current revision.
		b, _ := json.Marshal(map[string]any{"ID": 1, "ProjectID": p.ID, "Class": Data, "Title": input.Title, "Body": input.Body, "AuthorID": u.ID, "Author": u.Name, "Created": fromMs(100), "Updated": fromMs(100), "Revision": 1})
		_, e = tx.Exec(`INSERT INTO idempotency(user_id,project_id,key,hash,note_id,result,time) VALUES(?,?,?,?,?,?,?)`, u.ID, p.ID, "legacy", hash, 1, string(b), ms(s.now()))
		return e
	}), nil)
	s.Close()
	migrations = base
	s = openT(t, dir)
	if snaps := snapshots(dir); len(snaps) != 1 || !strings.HasPrefix(snaps[0], "core-10-") {
		t.Fatal(snaps)
	}
	n, e := s.GetNote(ctx, Principal{u.ID, "device"}, p.ID, 1)
	is(t, e, nil)
	if n.Provenance.Device != "UNKNOWN" || n.Provenance.SourceKind != "UNKNOWN" || n.Provenance.SourceReference != "UNKNOWN" || n.Provenance.User != u.ID || n.Provenance.Project != p.ID || !n.Provenance.Authored.Equal(fromMs(100)) || !n.Provenance.Received.Equal(fromMs(200)) || n.Revision != 3 {
		t.Fatal(n)
	}
	is(t, s.write(ctx, migrate11), nil)
	replayed, replay, e := s.CreateNote(ctx, Principal{u.ID, "device"}, p.ID, "legacy", input)
	is(t, e, nil)
	if !replay || replayed.Revision != 1 || replayed.Provenance.Device != "UNKNOWN" || replayed.Provenance.SourceKind != "UNKNOWN" || replayed.Provenance.User != u.ID || replayed.Provenance.ContentHash != contentHash(input.Title, input.Body) {
		t.Fatal(replayed, replay)
	}
	var revisions int
	is(t, s.DB().QueryRow(`SELECT count(*) FROM note_revisions WHERE note_id=1`).Scan(&revisions), nil)
	if revisions != 2 {
		t.Fatal("known legacy revisions", revisions)
	}
	v, _, e := s.Submit(ctx, Principal{u.ID, "device"}, p.ID, "pending", SubmissionInput{NoteInput: NoteInput{Class: Data, Title: "Review", Body: "retained"}})
	is(t, e, nil)
	backup := filepath.Join(dir, "backup.db")
	is(t, s.Backup(ctx, backup), nil)
	s.Close()
	is(t, Restore(dir, backup), nil)
	s = openT(t, dir)
	defer s.Close()
	got, e := s.GetSubmission(ctx, Principal{u.ID, "device"}, p.ID, v.ID, false)
	is(t, e, nil)
	if got.Body == nil || *got.Body != "retained" || got.Provenance.Device != "device" {
		t.Fatal(got)
	}
	n, e = s.GetNote(ctx, Principal{u.ID, "device"}, p.ID, 1)
	is(t, e, nil)
	if n.Provenance.Device != "UNKNOWN" {
		t.Fatal(n)
	}
}
