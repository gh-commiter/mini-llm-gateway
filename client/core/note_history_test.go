package core

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
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
	if charge != int64(len(title)+len(body)+len(class)+len(kind)+len(provenance)+56) {
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
	exec(t, f.Store, `INSERT INTO note_history(note_id,revision,title,body,class,entry_kind,updated,bytes,provenance,saved,saved_by,project_id) SELECT id,revision,title,body,class,entry_kind,updated,1,'{}',0,0,project_id FROM notes WHERE id=?`, n.ID)
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
			if e := s.DB().QueryRow(`SELECT count(*) FROM sqlite_master WHERE name IN ('note_history','note_history_totals','note_history_project','note_history_total_ins','note_history_total_del')`).Scan(&tables); e != nil || (tables == 5) == fail || (fail && tables != 0) {
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
	var refusal ErrRefusal
	if !errors.As(e, &refusal) || !strings.Contains(e.Error(), "changed since this submission was written") || historyCount(t, f.Store, n.ID) != 1 {
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

func historyTotal(t *testing.T, s *Store, project int64) (kept, sum int64) {
	t.Helper()
	if err := s.DB().QueryRow(`SELECT coalesce((SELECT bytes FROM note_history_totals WHERE project_id=?),0),coalesce((SELECT sum(bytes) FROM note_history WHERE project_id=?),0)`, project, project).Scan(&kept, &sum); err != nil {
		t.Fatal(err)
	}
	return
}

// Catches: a failure after the history insert leaving the snapshot (or the new text) behind.
func TestNoteHistoryRollsBackWithTheOverwrite(t *testing.T) {
	f := newFx(t)
	n := f.mk("ana", "fakture", Structure)
	exec(t, f.Store, `CREATE TRIGGER failing BEFORE UPDATE ON notes BEGIN SELECT RAISE(ABORT,'injected'); END`)
	_, e := f.UpdateNote(ctx, f.p("ana"), n.ProjectID, n.ID, n.Revision, NoteInput{Class: Structure, Title: n.Title, Body: "changed"})
	if e == nil {
		t.Fatal("injected failure ignored")
	}
	after, e := f.GetNote(ctx, f.p("ana"), n.ProjectID, n.ID)
	f.ok(e)
	if !reflect.DeepEqual(after, n) || historyCount(t, f.Store, n.ID) != 0 {
		t.Fatal("history row or note survived the rollback")
	}
	if kept, sum := historyTotal(t, f.Store, n.ProjectID); kept != 0 || sum != 0 {
		t.Fatal("total survived the rollback", kept, sum)
	}
}

// Documented limitation, asserted so a change is deliberate: alternating authors push the original out of the 100.
func TestNoteHistoryAlternatingAuthorsEvictTheOriginal(t *testing.T) {
	f := newFx(t)
	n := f.mk("ana", "fakture", Structure)
	cur := n
	for i := 0; i < 200; i++ {
		who := "ana"
		if i%2 == 1 {
			who = "fani"
		}
		cur = f.edit(who, n.ProjectID, cur, fmt.Sprint("alt ", i))
	}
	got := historyRevisions(t, f.Store, n.ID)
	if len(got) != 100 || got[0] != 101 || got[99] != 200 {
		t.Fatal("window", len(got), got[0], got[len(got)-1])
	}
}

// Catches: one project's noise evicting another's rows, and totals drifting from the rows (also on cascade).
func TestNoteHistoryProjectsAreIsolated(t *testing.T) {
	limitsRestored(t)
	f := newFx(t)
	other := f.mk("cene", "eureko", Structure)
	f.edit("cene", other.ProjectID, other, "e1")
	var c int64
	f.ok(f.DB().QueryRow(`SELECT bytes FROM note_history WHERE note_id=?`, other.ID).Scan(&c))
	maxHistoryProjectBytes = c
	n := f.mk("ana", "fakture", Structure)
	cur := n
	for i := 0; i < 5; i++ {
		f.clock = f.clock.Add(11 * time.Minute)
		cur = f.edit("ana", n.ProjectID, cur, fmt.Sprint("n", i))
	}
	if historyCount(t, f.Store, other.ID) != 1 {
		t.Fatal("another project's history was evicted")
	}
	for _, p := range []int64{n.ProjectID, other.ProjectID} {
		if kept, sum := historyTotal(t, f.Store, p); kept != sum || kept > maxHistoryProjectBytes {
			t.Fatal("total", p, kept, sum)
		}
	}
	f.ok(f.DeleteNote(ctx, f.p("ana"), n.ProjectID, n.ID, cur.Revision))
	if kept, sum := historyTotal(t, f.Store, n.ProjectID); kept != 0 || sum != 0 {
		t.Fatal("note delete left a total", kept, sum)
	}
}

// Catches: a kind-only change losing the old kind, and an equivalent CRLF write counting as a change.
func TestNoteHistoryKindOnlyAndCRLFNoop(t *testing.T) {
	f := newFx(t)
	n, _, e := f.CreateNote(ctx, f.p("ana"), f.pid["fakture"], "crlf", NoteInput{Class: Structure, Title: "t", Body: "a\nb"})
	f.ok(e)
	_, e = f.UpdateNote(ctx, f.p("ana"), n.ProjectID, n.ID, n.Revision, NoteInput{Class: Structure, Title: "t", Body: "a\r\nb"})
	f.ok(e)
	if historyCount(t, f.Store, n.ID) != 0 {
		t.Fatal("CRLF-equivalent write saved history")
	}
	f.ok(f.write(ctx, func(tx *sql.Tx) error {
		return saveNoteHistory(tx, n.ID, NoteInput{Class: Structure, Title: "t", Body: "a\nb", EntryKind: "faq"}, f.now(), 0)
	}))
	var kind string
	f.ok(f.DB().QueryRow(`SELECT entry_kind FROM note_history WHERE note_id=?`, n.ID).Scan(&kind))
	if kind != n.EntryKind || n.EntryKind == "faq" {
		t.Fatal("kind", kind)
	}
}

// Catches: INSERT OR REPLACE (the old row would go without the totals trigger and the total would drift).
func TestNoteHistoryUsesPlainInsert(t *testing.T) {
	src, err := os.ReadFile("note_history.go")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToUpper(string(src)), "INSERT OR") {
		t.Fatal("history rows must be written with a plain INSERT")
	}
}

// Catches: totals drifting after a restore (of a schema 25 backup migrated on restore, and of a schema 26 backup).
func TestNoteHistoryAccountingAfterRestore(t *testing.T) {
	for _, old := range []bool{true, false} {
		t.Run(fmt.Sprint("schema25=", old), func(t *testing.T) {
			base := migrations
			t.Cleanup(func() { migrations = base })
			if old {
				migrations = base[:25]
			}
			f := newFx(t)
			n := f.mk("ana", "fakture", Structure)
			if !old {
				n = f.edit("ana", n.ProjectID, n, "before backup")
			}
			backup := filepath.Join(t.TempDir(), "b.db")
			f.ok(f.Backup(ctx, backup))
			migrations = base
			dir := t.TempDir()
			f.ok(Restore(dir, backup))
			s := openT(t, dir)
			defer s.Close()
			g := &fx{Store: s, t: t, clock: f.clock, uid: f.uid, pid: f.pid}
			s.now = func() time.Time { return g.clock }
			for i := 0; i < 3; i++ {
				g.clock = g.clock.Add(11 * time.Minute)
				n = g.edit("ana", n.ProjectID, n, fmt.Sprint("after ", i))
			}
			if kept, sum := historyTotal(t, s, n.ProjectID); kept != sum || sum == 0 {
				t.Fatal("total after edits", kept, sum)
			}
			g.ok(g.DeleteNote(ctx, g.p("ana"), n.ProjectID, n.ID, n.Revision))
			if kept, sum := historyTotal(t, s, n.ProjectID); kept != 0 || sum != 0 {
				t.Fatal("total after delete", kept, sum)
			}
		})
	}
}

// Catches: eviction order not falling back to note id, revision when saved times tie.
func TestNoteHistoryTiedSavedEvictsLowestNoteFirst(t *testing.T) {
	limitsRestored(t)
	f := newFx(t)
	a, b := f.mk("ana", "fakture", Structure), f.mk("ana", "fakture", Structure)
	pid := a.ProjectID
	a = f.edit("ana", pid, a, "e1")
	f.edit("ana", pid, b, "e1")
	exec(t, f.Store, `UPDATE note_history SET saved=5`)
	var c int64
	f.ok(f.DB().QueryRow(`SELECT bytes FROM note_history WHERE note_id=?`, a.ID).Scan(&c))
	maxHistoryProjectBytes = 2*c + 100 // two rows fit, three do not
	f.clock = f.clock.Add(11 * time.Minute)
	f.edit("ana", pid, a, "e2")
	if got := historyRevisions(t, f.Store, a.ID); !reflect.DeepEqual(got, []int64{2}) || historyCount(t, f.Store, b.ID) != 1 {
		t.Fatal("tie order", got)
	}
}

// Catches: an eviction surviving the rollback of the overwrite that caused it.
func TestNoteHistoryRollbackAfterEviction(t *testing.T) {
	limitsRestored(t)
	f := newFx(t)
	a, b := f.mk("ana", "fakture", Structure), f.mk("ana", "fakture", Structure)
	pid := a.ProjectID
	a = f.edit("ana", pid, a, "e1")
	f.edit("ana", pid, b, "e1")
	var kept0, rows0 int64
	f.ok(f.DB().QueryRow(`SELECT (SELECT bytes FROM note_history_totals WHERE project_id=?),count(*) FROM note_history`, pid).Scan(&kept0, &rows0))
	maxHistoryProjectBytes = kept0 // the next row must evict
	exec(t, f.Store, `CREATE TRIGGER failing BEFORE UPDATE ON notes BEGIN SELECT RAISE(ABORT,'injected'); END`)
	f.clock = f.clock.Add(11 * time.Minute)
	if _, e := f.UpdateNote(ctx, f.p("ana"), pid, a.ID, a.Revision, NoteInput{Class: Structure, Title: a.Title, Body: "e2"}); e == nil {
		t.Fatal("injected failure ignored")
	}
	var kept1, rows1 int64
	f.ok(f.DB().QueryRow(`SELECT (SELECT bytes FROM note_history_totals WHERE project_id=?),count(*) FROM note_history`, pid).Scan(&kept1, &rows1))
	if kept0 != kept1 || rows0 != rows1 {
		t.Fatal("eviction survived the rollback", kept0, kept1, rows0, rows1)
	}
}

// Catches: the middle of a short series being thrown away at write time (a human edit then an agent's edit).
func TestNoteHistoryEverySuccessiveEditIsSaved(t *testing.T) {
	f := newFx(t)
	n := f.mk("ana", "fakture", Structure)
	b1 := f.edit("ana", n.ProjectID, n, "B1")
	f.clock = f.clock.Add(2 * time.Minute)
	f.edit("ana", n.ProjectID, b1, "B2")
	got := historyRevisions(t, f.Store, n.ID)
	if !reflect.DeepEqual(got, []int64{1, 2}) {
		t.Fatal("rows", got)
	}
	var body string
	f.ok(f.DB().QueryRow(`SELECT body FROM note_history WHERE note_id=? AND revision=2`, n.ID).Scan(&body))
	if body != "B1" {
		t.Fatal("the in-between text was not kept", body)
	}
}

// Catches: a storm pushing the original out, an edit refused, or the wrong rows dropped.
func TestNoteHistoryStormEvictsItsInteriorFirst(t *testing.T) {
	f := newFx(t)
	n := f.mk("ana", "fakture", Structure)
	cur := n
	for i := 0; i < 200; i++ {
		cur = f.edit("ana", n.ProjectID, cur, fmt.Sprint("storm ", i))
		f.clock = f.clock.Add(300 * time.Millisecond)
	}
	got := historyRevisions(t, f.Store, n.ID)
	// the merged gaps of evicted neighbours make the survivors spread over the storm; the first and the newest stay
	if len(got) != 100 || got[0] != 1 || got[99] != 200 {
		t.Fatal("rows", len(got), got[:3], got[len(got)-1])
	}
	checkHistory(t, f.Store, n)
	if cur.Revision != n.Revision+200 {
		t.Fatal("not every edit succeeded", cur.Revision)
	}
}

// Catches: spaced edits not being plain oldest-first.
func TestNoteHistorySpacedEditsAreOldestFirst(t *testing.T) {
	f := newFx(t)
	n := f.mk("ana", "fakture", Structure)
	cur := n
	for i := 0; i < 150; i++ {
		f.clock = f.clock.Add(11 * time.Minute)
		cur = f.edit("ana", n.ProjectID, cur, fmt.Sprint("v", i))
	}
	got := historyRevisions(t, f.Store, n.ID)
	if len(got) != 100 || got[0] != 51 || got[99] != 150 {
		t.Fatal("rows", len(got), got[0], got[len(got)-1])
	}
}

// Catches: a sparse row going before the interior rows of an earlier storm.
func TestNoteHistoryInteriorGoesBeforeSparseRows(t *testing.T) {
	limitsRestored(t)
	maxHistoryRevisions = 6
	f := newFx(t)
	n := f.mk("ana", "fakture", Structure)
	cur := n
	for i := 0; i < 4; i++ { // rows 1..4, one second apart
		f.clock = f.clock.Add(time.Second)
		cur = f.edit("ana", n.ProjectID, cur, fmt.Sprint("q", i))
	}
	sparse := func() {
		f.clock = f.clock.Add(11 * time.Minute)
		cur = f.edit("ana", n.ProjectID, cur, fmt.Sprint("s", cur.Revision))
	}
	for _, want := range [][]int64{{1, 2, 3, 4, 5}, {1, 2, 3, 4, 5, 6}, {1, 3, 4, 5, 6, 7}, {1, 3, 5, 6, 7, 8}, {1, 5, 6, 7, 8, 9}, {5, 6, 7, 8, 9, 10}} {
		sparse()
		if got := historyRevisions(t, f.Store, n.ID); !reflect.DeepEqual(got, want) {
			t.Fatal("rows", got, "want", want)
		}
	}
}

// Catches: the byte budget using another victim order than the row limit.
func TestNoteHistoryByteBudgetEvictsInteriorFirst(t *testing.T) {
	limitsRestored(t)
	f := newFx(t)
	n := f.mk("ana", "fakture", Structure)
	cur := f.edit("ana", n.ProjectID, n, "q1")
	cur = f.edit("ana", n.ProjectID, cur, "q2")
	var sum int64
	f.ok(f.DB().QueryRow(`SELECT sum(bytes) FROM note_history WHERE note_id=?`, n.ID).Scan(&sum))
	maxHistoryNoteBytes = sum + 50 // a third row does not fit
	f.edit("ana", n.ProjectID, cur, "q3")
	if got := historyRevisions(t, f.Store, n.ID); !reflect.DeepEqual(got, []int64{1, 3}) {
		t.Fatal("rows", got)
	}
}

// Catches: wrong interior rules: replacement rows (author 0), the exact ten-minute edge, zero and negative gaps.
func TestPickVictim(t *testing.T) {
	ms := func(d time.Duration) int64 { return d.Milliseconds() }
	row := func(rev int64, saved time.Duration, by int64) historyRow {
		return historyRow{rev: rev, saved: ms(saved), by: by}
	}
	cases := []struct {
		name string
		rows []historyRow
		want int
	}{
		{"interior goes first", []historyRow{row(1, 0, 7), row(2, time.Minute, 7), row(3, 2*time.Minute, 7), row(4, time.Hour, 9)}, 1},
		{"author 0 is never interior", []historyRow{row(1, 0, 0), row(2, time.Second, 0), row(3, 2*time.Second, 0)}, 0},
		{"different authors alternate", []historyRow{row(1, 0, 7), row(2, time.Second, 8), row(3, 2*time.Second, 7), row(4, 3*time.Second, 8)}, 0},
		{"9:59.999 is interior", []historyRow{row(1, 0, 7), row(2, historyBurst-time.Millisecond, 7), row(3, time.Hour, 7)}, 1},
		{"10:00.000 is not", []historyRow{row(1, 0, 7), row(2, historyBurst, 7), row(3, 2*time.Hour, 7)}, 0},
		{"zero gap is not", []historyRow{row(1, 0, 7), row(2, 0, 7), row(3, time.Hour, 7)}, 0},
		{"clock went back is not", []historyRow{row(1, time.Hour, 7), row(2, 0, 7), row(3, time.Minute, 8)}, 0},
		{"the newest row is never chosen", []historyRow{row(1, 0, 7), row(2, time.Second, 7)}, 0},
		{"only the newest left", []historyRow{row(1, 0, 7)}, -1},
	}
	for _, c := range cases {
		if got := pickVictim(c.rows); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}

// Catches: a version that lived two minutes being evicted before the ones that lived 100 ms.
func TestNoteHistoryLongerLivedVersionOutlivesAStorm(t *testing.T) {
	f := newFx(t)
	n := f.mk("ana", "fakture", Structure)
	cur := f.edit("ana", n.ProjectID, n, "B1") // row 1: the original
	f.clock = f.clock.Add(2 * time.Minute)
	cur = f.edit("ana", n.ProjectID, cur, "storm start") // row 2: B1, lived two minutes
	for i := 0; i < 150; i++ {
		f.clock = f.clock.Add(100 * time.Millisecond)
		cur = f.edit("ana", n.ProjectID, cur, fmt.Sprint("storm ", i))
	}
	got := historyRevisions(t, f.Store, n.ID)
	if len(got) != 100 || got[0] != 1 || got[1] != 2 || got[99] != cur.Revision-1 {
		t.Fatal("rows", len(got), got[:3], got[len(got)-1])
	}
	var body string
	f.ok(f.DB().QueryRow(`SELECT body FROM note_history WHERE note_id=? AND revision=2`, n.ID).Scan(&body))
	if body != "B1" {
		t.Fatal("B1 lost", body)
	}
}

func TestPickVictimShortestLived(t *testing.T) {
	row := func(rev int64, saved time.Duration, by int64) historyRow {
		return historyRow{rev: rev, saved: saved.Milliseconds(), by: by}
	}
	cases := []struct {
		name string
		rows []historyRow
		want int
	}{
		{"shortest gap beats older", []historyRow{row(1, 0, 7), row(2, 2*time.Minute, 7), row(3, 2*time.Minute+100*time.Millisecond, 7), row(4, time.Hour, 9)}, 2},
		{"equal gaps: the oldest", []historyRow{row(1, 0, 7), row(2, time.Second, 7), row(3, 2*time.Second, 7), row(4, time.Hour, 9)}, 1},
		{"9:59 loses to 1 s", []historyRow{row(1, 0, 7), row(2, 9*time.Minute+59*time.Second, 7), row(3, 9*time.Minute+60*time.Second, 7), row(4, time.Hour, 9)}, 2},
		{"non-interior untouched while an interior exists", []historyRow{row(1, 0, 8), row(2, time.Hour, 7), row(3, time.Hour+time.Second, 7), row(4, 5*time.Hour, 9)}, 2},
	}
	for _, c := range cases {
		if got := pickVictim(c.rows); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}

func historyBytes(t *testing.T, s *Store, ids ...int64) (sum int64) {
	t.Helper()
	for _, id := range ids {
		var b int64
		if err := s.DB().QueryRow(`SELECT coalesce(sum(bytes),0) FROM note_history WHERE note_id=?`, id).Scan(&b); err != nil {
			t.Fatal(err)
		}
		sum += b
	}
	return
}

// #164: a writer's edit loop over the project budget evicts the writer's own versions first (the note's own, then the writer's in
// other notes, each note's newest kept) and only then the project's oldest; other people's versions survive the loop.
func TestNoteHistoryOverBudgetEvictsTheWritersOwnFirst(t *testing.T) {
	limitsRestored(t)
	f := newFx(t)
	pid := f.pid["fakture"]
	hist := func(user string, n int) Note { // a note of this user with n saved versions of equal size
		note := f.mk(user, "fakture", Structure)
		for i := 0; i < n; i++ {
			f.clock = f.clock.Add(11 * time.Minute)
			note = f.edit(user, pid, note, fmt.Sprintf("v%04d", i))
		}
		return note
	}
	bor, fani, a2 := hist("bor", 3), hist("fani", 3), hist("ana", 3)
	a1 := hist("ana", 1)
	f.clock = f.clock.Add(11 * time.Minute)
	others := historyBytes(t, f.Store, bor.ID, fani.ID)
	var c int64
	f.ok(f.DB().QueryRow(`SELECT max(bytes) FROM note_history`).Scan(&c))
	check := func(label string) {
		t.Helper()
		if kept, sum := historyTotal(t, f.Store, pid); kept != sum {
			t.Fatalf("%s: total %d, rows %d", label, kept, sum)
		}
		if historyCount(t, f.Store, bor.ID) != 3 || historyCount(t, f.Store, fani.ID) != 3 {
			t.Fatalf("%s: another user's saved versions were evicted", label)
		}
	}
	// phase 1: the loop's own note gives way; the others and the writer's other note are untouched
	maxHistoryProjectBytes = others + historyBytes(t, f.Store, a2.ID) + 2*c
	cur := a1
	for i := 0; i < 20; i++ {
		f.clock = f.clock.Add(11 * time.Minute)
		cur = f.edit("ana", pid, cur, fmt.Sprintf("loop%02d", i))
		check("phase 1")
		if kept, _ := historyTotal(t, f.Store, pid); kept > maxHistoryProjectBytes {
			t.Fatal("over budget after the write", kept)
		}
		if historyCount(t, f.Store, a2.ID) != 3 {
			t.Fatal("the writer's other note was evicted before the loop's own note")
		}
		if got := historyRevisions(t, f.Store, a1.ID); len(got) == 0 || got[len(got)-1] != cur.Revision-1 {
			t.Fatal("the newest saved version of the loop's note is gone", got)
		}
	}
	// phase 2: the loop's note keeps only its newest; the writer's older rows in other notes go next, each note's newest stays
	var newest2, newest1 int64
	f.ok(f.DB().QueryRow(`SELECT bytes FROM note_history WHERE note_id=? AND revision=?`, a2.ID, a2.Revision-1).Scan(&newest2))
	f.ok(f.DB().QueryRow(`SELECT bytes FROM note_history WHERE note_id=? AND revision=?`, a1.ID, cur.Revision-1).Scan(&newest1))
	maxHistoryProjectBytes = others + newest2 + newest1 + 3 // the new row is as big as the loop's rows
	f.clock = f.clock.Add(11 * time.Minute)
	cur = f.edit("ana", pid, cur, "loop99")
	check("phase 2")
	if got := historyRevisions(t, f.Store, a2.ID); len(got) != 1 || got[0] != a2.Revision-1 {
		t.Fatal("the writer's other note keeps only its newest saved version", got)
	}
	if got := historyRevisions(t, f.Store, a1.ID); len(got) != 1 || got[0] != cur.Revision-1 {
		t.Fatal("the loop's note keeps only its newest saved version", got)
	}
	// phase 3: the writer's own cannot free enough: the project's oldest go, as before; the row just saved stays and totals agree
	maxHistoryProjectBytes = newest1 + 3 // room for the row just saved only
	f.clock = f.clock.Add(11 * time.Minute)
	cur = f.edit("ana", pid, cur, "loop98")
	check3 := historyCount(t, f.Store, bor.ID) + historyCount(t, f.Store, fani.ID)
	if check3 != 0 {
		t.Fatal("the project's oldest rows were not evicted when the writer's own could not free enough", check3)
	}
	if got := historyRevisions(t, f.Store, a1.ID); len(got) != 1 || got[0] != cur.Revision-1 {
		t.Fatal("the row just saved was evicted", got)
	}
	if kept, sum := historyTotal(t, f.Store, pid); kept != sum {
		t.Fatal("total", kept, sum)
	}
}
