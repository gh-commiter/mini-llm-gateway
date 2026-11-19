package core

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func historyCount(t *testing.T, s *Store, id int64) int {
	t.Helper()
	var count int
	if err := s.DB().QueryRow(`SELECT count(*) FROM note_history WHERE note_id=?`, id).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func checkHistory(t *testing.T, s *Store, old Note) {
	t.Helper()
	var title, body, class, kind, provenance, original string
	var updated, charge int64
	if err := s.DB().QueryRow(`SELECT title,body,class,entry_kind,updated,bytes,provenance FROM note_history WHERE note_id=? AND revision=?`, old.ID, old.Revision).
		Scan(&title, &body, &class, &kind, &updated, &charge, &provenance); err != nil {
		t.Fatal(err)
	}
	if err := s.DB().QueryRow(`SELECT provenance FROM note_revisions WHERE note_id=? AND revision=?`, old.ID, old.Revision).Scan(&original); err != nil {
		t.Fatal(err)
	}
	want, err := json.Marshal(old.Provenance)
	if err != nil {
		t.Fatal(err)
	}
	if title != old.Title || body != old.Body || class != string(old.Class) || kind != old.EntryKind || updated != ms(old.Updated) || provenance != original || provenance != string(want) {
		t.Fatalf("snapshot differs: %q %q %s %s %d %s; old %+v", title, body, class, kind, updated, provenance, old)
	}
	if charge != int64(len(title)+len(body)+len(class)+len(kind)+len(provenance)+32) {
		t.Fatalf("wrong byte charge: %d", charge)
	}
}

// Each public overwrite path uses the same snapshot and transaction rules.
func TestNoteHistoryOverwritePaths(t *testing.T) {
	for _, path := range []string{"edit", "review", "import"} {
		t.Run(path, func(t *testing.T) {
			f := newFx(t)
			p, pid := f.p("fani"), f.pid["fakture"]
			input := NoteInput{Class: Structure, Title: "Prvotni čaj", Body: "oldneedle\nexact café", EntryKind: KindFinding}
			var n Note
			var err error
			if path == "import" {
				r, e := f.ImportMarkdown(ctx, p, pid, MarkdownInput{Path: "history.md", Class: input.Class, Markdown: string(Markdown(Note{Title: input.Title, Body: input.Body, Class: input.Class}, ""))})
				f.ok(e)
				n, err = f.GetNote(ctx, p, pid, r.NoteID)
			} else {
				n, _, err = f.CreateNote(ctx, p, pid, "history", input)
			}
			f.ok(err)
			write := func(in NoteInput, revision int64) error {
				f.clock = f.clock.Add(11 * time.Minute) // each edit starts a new burst
				switch path {
				case "edit":
					_, e := f.UpdateNote(ctx, p, pid, n.ID, revision, in)
					return e
				case "review":
					v, _, e := f.Submit(ctx, p, pid, fmt.Sprintf("replace-%d-%s", revision, in.Title), SubmissionInput{NoteInput: in, Replaces: n.ID, Revision: revision})
					if e != nil {
						return e
					}
					_, e = f.ReviewSubmission(ctx, v.ID, true, "", f.seen(v.ID))
					return e
				default:
					_, e := f.ImportMarkdown(ctx, p, pid, MarkdownInput{Path: "history.md", Class: in.Class, Markdown: string(Markdown(Note{Title: in.Title, Body: in.Body, Class: in.Class}, ""))})
					return e
				}
			}
			// An unchanged write may retain existing revision semantics, but saves no history.
			beforeNoop := n.Revision
			f.ok(write(input, n.Revision))
			if historyCount(t, f.Store, n.ID) != 0 {
				t.Fatal("unchanged write captured history")
			}
			n, err = f.GetNote(ctx, p, pid, n.ID)
			f.ok(err)
			wantRevision := beforeNoop + 1
			if path == "import" {
				wantRevision = beforeNoop
			}
			if n.Revision != wantRevision {
				t.Fatal("unchanged write revision semantics changed", n.Revision, wantRevision)
			}
			bad := input
			bad.Title = strings.Repeat("x", maxTitle+1)
			if e := write(bad, n.Revision); e == nil {
				t.Fatal("invalid overwrite succeeded")
			}
			if historyCount(t, f.Store, n.ID) != 0 {
				t.Fatal("failed validation saved history")
			}
			for i := 0; i < 2; i++ {
				old := n
				in := NoteInput{Class: Data, Title: fmt.Sprintf("Current %d", i), Body: fmt.Sprintf("currentneedle %d", i), EntryKind: n.EntryKind}
				f.ok(write(in, n.Revision))
				checkHistory(t, f.Store, old)
				n, err = f.GetNote(ctx, p, pid, n.ID)
				f.ok(err)
				if n.Title != in.Title || n.Body != in.Body || n.Class != in.Class || n.Revision != old.Revision+1 || historyCount(t, f.Store, n.ID) != i+1 {
					t.Fatalf("overwrite: %+v", n)
				}
			}
			pg, e := f.SearchNotes(ctx, p, pid, "oldneedle")
			f.ok(e)
			if len(pg.Hits) != 0 || strings.Contains(string(Markdown(n, "")), "oldneedle") {
				t.Fatal("history leaked into search or export")
			}
			pg, e = f.SearchNotes(ctx, p, pid, "currentneedle")
			f.ok(e)
			if len(pg.Hits) != 1 {
				t.Fatal("current content not searchable")
			}
			f.ok(f.DeleteNote(ctx, p, pid, n.ID, n.Revision))
			if historyCount(t, f.Store, n.ID) != 0 {
				t.Fatal("delete left history behind")
			}
		})
	}
}

func TestNoteHistoryConflictAndInsertFailure(t *testing.T) {
	f := newFx(t)
	n := f.mk("ana", "fakture", Structure)
	in := NoteInput{Class: Structure, Title: "changed", Body: "changed"}
	_, e := f.UpdateNote(ctx, f.p("ana"), n.ProjectID, n.ID, n.Revision+1, in)
	var conflict ErrRevision
	if !errors.As(e, &conflict) || historyCount(t, f.Store, n.ID) != 0 {
		t.Fatal("revision conflict", e)
	}
	// A duplicate key makes the snapshot insert fail after the checks, before the overwrite.
	exec(t, f.Store, `INSERT INTO note_history SELECT id,revision,title,body,class,entry_kind,updated,1,'{}',0,0 FROM notes WHERE id=?`, n.ID)
	_, e = f.UpdateNote(ctx, f.p("ana"), n.ProjectID, n.ID, n.Revision, in)
	if e == nil {
		t.Fatal("insert failure ignored")
	}
	after, e := f.GetNote(ctx, f.p("ana"), n.ProjectID, n.ID)
	f.ok(e)
	if !reflect.DeepEqual(after, n) || historyCount(t, f.Store, n.ID) != 1 {
		t.Fatal("failed insert changed note or history")
	}
}

func TestNoteHistoryMigration25(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint("fail=", fail), func(t *testing.T) {
			base := migrations
			t.Cleanup(func() { migrations = base })
			migrations = base[:25]
			f := newFx(t)
			n := f.mk("ana", "fakture", Data)
			dir := f.dir
			f.ok(f.Close())
			migrations = append([]func(*sql.Tx) error{}, base...)
			if fail {
				migrations[25] = func(tx *sql.Tx) error {
					if e := migrate26(tx); e != nil {
						return e
					}
					return errors.New("injected migration failure")
				}
				if s, e := Open(dir, Options{}); e == nil {
					s.Close()
					t.Fatal("migration succeeded")
				}
				migrations = base[:25]
			}
			s := openT(t, dir)
			defer s.Close()
			want := 26
			if fail {
				want = 25
			}
			if v, e := schemaVersion(ctx, s.DB()); e != nil || v != want {
				t.Fatal("schema", v, e)
			}
			var body string
			if e := s.DB().QueryRow(`SELECT body FROM notes WHERE id=?`, n.ID).Scan(&body); e != nil || body != n.Body {
				t.Fatal("old content lost", body, e)
			}
			var tables int
			if e := s.DB().QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='note_history'`).Scan(&tables); e != nil || (tables == 1) == fail {
				t.Fatal("migration table", tables, e)
			}
			if snaps, _ := filepath.Glob(filepath.Join(dir, "core-25-*.db")); len(snaps) != 1 {
				t.Fatal("missing pre-upgrade snapshot", snaps)
			}
			if !fail && historyCount(t, s, n.ID) != 0 {
				t.Fatal("history was backfilled")
			}
			if fail {
				f.ok(s.Close())
				migrations = base
				retried := openT(t, dir)
				defer retried.Close()
				if v, err := schemaVersion(ctx, retried.DB()); err != nil || v != 26 || historyCount(t, retried, n.ID) != 0 {
					t.Fatal("migration retry", v, err)
				}
			}
		})
	}
}

func TestNoteHistoryBackupAndCurrentBundle(t *testing.T) {
	f := newSeg(t)
	n := f.mk("fani", "fakture", Structure)
	_, e := f.UpdateNote(ctx, f.p("fani"), n.ProjectID, n.ID, n.Revision, NoteInput{Class: Structure, Title: "current", Body: "current"})
	f.ok(e)
	file := filepath.Join(t.TempDir(), "bundle.zip")
	f.ok(f.ExportBundle(ctx, BundleExport{Team: "acme", Path: file}))
	b, e := readBundle(file)
	f.ok(e)
	for name, data := range b.files {
		if strings.HasSuffix(name, ".md") && strings.Contains(string(data), n.Body) {
			t.Fatal("bundle contains outgoing body", name)
		}
	}
	backup := filepath.Join(t.TempDir(), "history.db")
	f.ok(f.Backup(ctx, backup))
	dir := t.TempDir()
	f.ok(Restore(dir, backup))
	s := openT(t, dir)
	defer s.Close()
	checkHistory(t, s, n)
}

func TestNoteHistoryStaleReviewAndLocalImport(t *testing.T) {
	f := newFx(t)
	p, pid := f.p("ana"), f.pid["fakture"]
	r, e := f.ImportMarkdown(ctx, p, pid, MarkdownInput{Path: "stale.md", Class: Structure, Markdown: "initial"})
	f.ok(e)
	n, e := f.GetNote(ctx, p, pid, r.NoteID)
	f.ok(e)
	v, _, e := f.Submit(ctx, p, pid, "stale", SubmissionInput{NoteInput: NoteInput{Class: Structure, Title: "proposal", Body: "stale"}, Replaces: n.ID, Revision: n.Revision})
	f.ok(e)
	_, e = f.UpdateNote(ctx, p, pid, n.ID, n.Revision, NoteInput{Class: Structure, Title: n.Title, Body: "local edit"})
	f.ok(e)
	_, e = f.ReviewSubmission(ctx, v.ID, true, "", f.seen(v.ID))
	var conflict ErrRevision
	if !errors.As(e, &conflict) || historyCount(t, f.Store, n.ID) != 1 {
		t.Fatal("stale replacement added history", e)
	}
	r, e = f.ImportMarkdown(ctx, p, pid, MarkdownInput{Path: "stale.md", Class: Structure, Markdown: "changed remotely"})
	f.ok(e)
	if r.Action != "skip" || r.Reason != "local edit" || historyCount(t, f.Store, n.ID) != 1 {
		t.Fatal("conflicting import added history", r)
	}
}

func TestNoteHistoryBundleReimport(t *testing.T) {
	source := newSeg(t)
	n := source.mk("fani", "fakture", Structure)
	file := filepath.Join(t.TempDir(), "bundle.zip")
	source.ok(source.ExportBundle(ctx, BundleExport{Team: "acme", Path: file}))
	target := openT(t, t.TempDir())
	defer target.Close()
	_, e := target.ImportBundle(ctx, BundleImport{Path: file, Source: "history"})
	source.ok(e)
	var id int64
	source.ok(target.DB().QueryRow(`SELECT id FROM notes`).Scan(&id))
	var old Note
	source.ok(target.read(ctx, func(tx *sql.Tx) error { var err error; old, err = getNote(tx, id); return err }))
	_, e = source.UpdateNote(ctx, source.p("fani"), n.ProjectID, n.ID, n.Revision, NoteInput{Class: Data, Title: "replacement", Body: "new bundle body"})
	source.ok(e)
	newFile := filepath.Join(t.TempDir(), "new.zip")
	source.ok(source.ExportBundle(ctx, BundleExport{Team: "acme", Path: newFile}))
	_, e = target.ImportBundle(ctx, BundleImport{Path: newFile, Source: "history"})
	source.ok(e)
	checkHistory(t, target, old)
	var body string
	source.ok(target.DB().QueryRow(`SELECT body FROM notes WHERE id=?`, id).Scan(&body))
	if body != "new bundle body" || old.Provenance.ActorKind != ActorImport {
		t.Fatal("bundle content or import identity", body, old.Provenance)
	}
}

func TestNoteHistoryCopiesStoredProvenanceVerbatim(t *testing.T) {
	f := newFx(t)
	n := f.mk("ana", "fakture", Structure)
	const stored = "{ \"actorKind\": \"import\", \"user\": 0, \"device\": \"UNKNOWN\", \"extra\": \"opaque identity\" }"
	exec(t, f.Store, `UPDATE note_revisions SET provenance=? WHERE note_id=? AND revision=?`, stored, n.ID, n.Revision)
	_, e := f.UpdateNote(ctx, f.p("ana"), n.ProjectID, n.ID, n.Revision, NoteInput{Class: Data, Title: n.Title, Body: n.Body})
	f.ok(e)
	var provenance string
	f.ok(f.DB().QueryRow(`SELECT provenance FROM note_history WHERE note_id=? AND revision=?`, n.ID, n.Revision).Scan(&provenance))
	if provenance != stored {
		t.Fatal("stored provenance was reinterpreted", provenance)
	}
}

func historyRevisions(t *testing.T, s *Store, id int64) []int64 {
	t.Helper()
	rows, err := s.DB().Query(`SELECT revision FROM note_history WHERE note_id=? ORDER BY revision`, id)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var r int64
		if err := rows.Scan(&r); err != nil {
			t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

func limitsRestored(t *testing.T) {
	r, n, p, b := maxHistoryRevisions, maxHistoryNoteBytes, maxHistoryProjectBytes, historyBurst
	t.Cleanup(func() { maxHistoryRevisions, maxHistoryNoteBytes, maxHistoryProjectBytes, historyBurst = r, n, p, b })
}

// Catches: a storm pushing the original out of the window, or an edit being refused.
func TestNoteHistoryStormKeepsTheTextFromBeforeIt(t *testing.T) {
	f := newFx(t)
	n := f.mk("ana", "fakture", Structure)
	pid := n.ProjectID
	cur := n
	for i := 0; i < 200; i++ {
		cur = f.edit("ana", pid, cur, fmt.Sprint("storm ", i))
		f.clock = f.clock.Add(300 * time.Millisecond)
	}
	if got := historyRevisions(t, f.Store, n.ID); len(got) != 1 || got[0] != n.Revision {
		t.Fatal("storm rows", got)
	}
	checkHistory(t, f.Store, n)
	if cur.Revision != n.Revision+200 {
		t.Fatal("not every edit succeeded", cur.Revision)
	}
}

// Catches: the burst measured from the last edit instead of from the saved row, a window that never ends, and
// authors sharing a burst.
func TestNoteHistoryBurstBoundaries(t *testing.T) {
	f := newFx(t)
	n := f.mk("ana", "fakture", Structure)
	pid := n.ProjectID
	cur := f.edit("ana", pid, n, "one") // saves revision 1 at t0
	f.clock = f.clock.Add(9 * time.Minute)
	cur = f.edit("ana", pid, cur, "two") // inside the burst
	f.clock = f.clock.Add(2 * time.Minute)
	cur = f.edit("ana", pid, cur, "three") // 11 minutes after the row was saved, though 2 after the last edit
	if got := historyRevisions(t, f.Store, n.ID); !reflect.DeepEqual(got, []int64{1, 3}) {
		t.Fatal("rows", got)
	}
	cur = f.edit("fani", pid, cur, "four") // another author: always a row
	cur = f.edit("ana", pid, cur, "five")
	cur = f.edit("fani", pid, cur, "six")
	if got := historyRevisions(t, f.Store, n.ID); !reflect.DeepEqual(got, []int64{1, 3, 4, 5, 6}) {
		t.Fatal("alternating authors", got)
	}
	f.clock = f.clock.Add(11 * time.Minute)
	f.edit("fani", pid, cur, "seven")
	if got := historyRevisions(t, f.Store, n.ID); len(got) != 6 {
		t.Fatal("11 minutes later", got)
	}
}

// Catches: an accepted replacement being coalesced into a burst, or the next edit comparing with an older row.
func TestNoteHistoryReplacementStartsANewBurst(t *testing.T) {
	f := newFx(t)
	p := f.p("ana")
	n := f.mk("ana", "fakture", Structure)
	pid := n.ProjectID
	cur := f.edit("ana", pid, n, "one") // row for revision 1
	v, _, e := f.Submit(ctx, p, pid, "burst-replace", SubmissionInput{NoteInput: NoteInput{Class: Structure, Title: n.Title, Body: "replaced"}, Replaces: n.ID, Revision: cur.Revision})
	f.ok(e)
	_, e = f.ReviewSubmission(ctx, v.ID, true, "", f.seen(v.ID))
	f.ok(e)
	f.clock = f.clock.Add(time.Second)
	cur, e = f.GetNote(ctx, p, pid, n.ID)
	f.ok(e)
	f.edit("ana", pid, cur, "after") // within seconds of the same author's earlier row, but after the replacement
	if got := historyRevisions(t, f.Store, n.ID); !reflect.DeepEqual(got, []int64{1, 2, 3}) {
		t.Fatal("rows", got)
	}
}

// Catches: the 101st version being refused, a gap in the kept revisions, or the newest being dropped.
func TestNoteHistoryKeepsTheNewestHundred(t *testing.T) {
	f := newFx(t)
	n := f.mk("ana", "fakture", Structure)
	cur := n
	for i := 0; i < 105; i++ {
		f.clock = f.clock.Add(11 * time.Minute)
		cur = f.edit("ana", n.ProjectID, cur, fmt.Sprint("v", i))
	}
	got := historyRevisions(t, f.Store, n.ID)
	if len(got) != 100 || got[0] != 6 || got[99] != 105 || got[99]-got[0] != 99 {
		t.Fatal("window", len(got), got[0], got[len(got)-1])
	}
}

// Catches: byte caps refusing, dropping the new row, dropping the wrong end, or ignoring the note/project scope.
func TestNoteHistoryByteCapsDropOldest(t *testing.T) {
	limitsRestored(t)
	f := newFx(t)
	pid := f.pid["fakture"]
	a, b := f.mk("ana", "fakture", Structure), f.mk("ana", "fakture", Structure)
	step := func(n Note, body string) Note {
		f.clock = f.clock.Add(11 * time.Minute)
		return f.edit("ana", pid, n, body)
	}
	a = step(a, "1")
	a = step(a, "2") // saved revision 2, one character: every later row has this size
	var charge int64
	f.ok(f.DB().QueryRow(`SELECT bytes FROM note_history WHERE note_id=? AND revision=2`, a.ID).Scan(&charge))
	maxHistoryNoteBytes = 2 * charge
	for _, body := range []string{"3", "4", "5"} {
		a = step(a, body)
	}
	if got := historyRevisions(t, f.Store, a.ID); !reflect.DeepEqual(got, []int64{4, 5}) {
		t.Fatal("note cap", got)
	}
	// project cap: a's two rows fill it exactly; the new row of another note pushes out the project's oldest
	maxHistoryNoteBytes = 1 << 30
	maxHistoryProjectBytes = 2 * charge
	b = step(b, "1")
	if len(historyRevisions(t, f.Store, b.ID)) != 1 {
		t.Fatal("new row was dropped for the project cap")
	}
	for _, r := range historyRevisions(t, f.Store, a.ID) {
		if r == 4 {
			t.Fatal("project cap kept the oldest row")
		}
	}
	var total int64
	f.ok(f.DB().QueryRow(`SELECT sum(bytes) FROM note_history`).Scan(&total))
	if total > maxHistoryProjectBytes {
		t.Fatal("project over its cap", total)
	}
	// a snapshot larger than any budget is not stored, and the edit still succeeds
	maxHistoryProjectBytes = 1
	n := historyCount(t, f.Store, b.ID)
	b2 := step(b, "2")
	if b2.Revision != b.Revision+1 || historyCount(t, f.Store, b.ID) != n {
		t.Fatal("oversize snapshot", b2.Revision)
	}
}

// Catches: an old database's note without a provenance row failing its first edit.
func TestNoteHistoryNoteWithoutProvenance(t *testing.T) {
	f := newFx(t)
	n := f.mk("ana", "fakture", Structure)
	exec(t, f.Store, `DELETE FROM note_revisions WHERE note_id=?`, n.ID)
	_, e := f.UpdateNote(ctx, f.p("ana"), n.ProjectID, n.ID, n.Revision, NoteInput{Class: Structure, Title: n.Title, Body: "edited"})
	f.ok(e)
	var prov string
	f.ok(f.DB().QueryRow(`SELECT provenance FROM note_history WHERE note_id=? AND revision=?`, n.ID, n.Revision).Scan(&prov))
	if prov != "" {
		t.Fatal("provenance", prov)
	}
}
