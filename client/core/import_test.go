package core

import (
	"archive/zip"
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMarkdownImportDurable(t *testing.T) {
	f := newFx(t)
	p := f.p("ana")
	pid := f.pid["fakture"]
	input := MarkdownInput{Markdown: "raw\r\n---\nbody\x00", Path: "folder/note.md", Class: Data}
	first, e := f.ImportMarkdown(ctx, p, pid, input)
	f.ok(e)
	n, e := f.GetNote(ctx, p, pid, first.NoteID)
	f.ok(e)
	if n.Body != input.Markdown || n.Title != "note" || n.Provenance.SourceKind != SourceImport || n.Provenance.SourceReference != "note.md" {
		t.Fatal(n)
	}
	f.clock = f.clock.Add(48 * time.Hour)
	second, e := f.ImportMarkdown(ctx, p, pid, input)
	f.ok(e)
	if second.Action != "skip" || second.NoteID != first.NoteID {
		t.Fatal(second)
	}
	input.Markdown = "changed"
	second, e = f.ImportMarkdown(ctx, p, pid, input)
	f.ok(e)
	if second.Action != "merge" || second.NoteID != first.NoteID {
		t.Fatal(second)
	}
	n, e = f.GetNote(ctx, p, pid, first.NoteID)
	f.ok(e)
	_, e = f.UpdateNote(ctx, p, pid, n.ID, n.Revision, NoteInput{Class: Data, Title: n.Title, Body: "local"})
	f.ok(e)
	input.Markdown = "third"
	result, e := f.ImportMarkdown(ctx, p, pid, input)
	f.ok(e)
	if result.Action != "skip" || result.Reason != "local edit" {
		t.Fatal(result)
	}
	_, e = f.ImportMarkdown(ctx, f.p("bor"), pid, input)
	is(t, e, ErrForbidden)
	for _, class := range []Class{Data, Structure} {
		input.Class = class
		_, e = f.ImportMarkdown(ctx, f.p("eva"), pid, input)
		is(t, e, ErrForbidden)
	}
	n, e = f.GetNote(ctx, p, pid, n.ID)
	f.ok(e)
	f.ok(f.DeleteNote(ctx, p, pid, n.ID, n.Revision))
	result, e = f.ImportMarkdown(ctx, p, pid, input)
	f.ok(e)
	if result.Action != "create" || result.NoteID == first.NoteID {
		t.Fatal(result)
	}
}

// A member import compares the class too: an incoming class-only change applies, a local class
// change is protected like any other local edit.
func TestMarkdownImportClassChangeRules(t *testing.T) {
	f := newFx(t)
	p := f.p("fani")
	pid := f.pid["fakture"]
	in := MarkdownInput{Markdown: "body", Path: "class.md", Class: Structure}
	first, e := f.ImportMarkdown(ctx, p, pid, in)
	f.ok(e)
	in.Class = Data
	r, e := f.ImportMarkdown(ctx, p, pid, in)
	f.ok(e)
	if r.Action != "merge" || r.NoteID != first.NoteID {
		t.Fatal(r)
	}
	n, e := f.GetNote(ctx, p, pid, first.NoteID)
	f.ok(e)
	if n.Class != Data || n.Revision != 2 {
		t.Fatal(n.Class, n.Revision)
	}
	_, e = f.UpdateNote(ctx, p, pid, n.ID, n.Revision, NoteInput{Class: Private, Title: n.Title, Body: n.Body})
	f.ok(e)
	in.Class = Structure
	r, e = f.ImportMarkdown(ctx, p, pid, in)
	f.ok(e)
	if r.Action != "skip" || r.Reason != "local edit" {
		t.Fatal(r)
	}
	n, e = f.GetNote(ctx, p, pid, n.ID)
	f.ok(e)
	if n.Class != Private {
		t.Fatal(n.Class)
	}
}

func TestMarkdownFrontMatterAndSubmissionReceipt(t *testing.T) {
	f := newFx(t)
	p := f.p("ana")
	pid := f.pid["fakture"]
	n := Note{ID: 17, Title: "Quoted \"title\"\n", Class: Structure, Body: "\nbody\r\n---\n\x00", Author: "ana", Revision: 3, Created: f.clock, Updated: f.clock, Provenance: Provenance{SourceKind: SourceSessionSummary}}
	data := Markdown(n, "")
	fields, body, e := parseMarkdown(data)
	f.ok(e)
	if body != n.Body || fields["hash"] != contentHash(n.Title, n.Body) || fields["kind"] != SourceSessionSummary {
		t.Fatal(fields, body)
	}
	n.Title = "Quoted title"
	data = Markdown(n, "")
	in := MarkdownInput{Markdown: string(data), Path: "one.md", Submit: true}
	first, e := f.ImportMarkdown(ctx, p, pid, in)
	f.ok(e)
	f.clock = f.clock.Add(48 * time.Hour)
	in.Path = "renamed.md"
	again, e := f.ImportMarkdown(ctx, p, pid, in)
	f.ok(e)
	if again.Action != "skip" || again.SubmissionID != first.SubmissionID {
		t.Fatal(again)
	}
	n.Body = "changed pending"
	in.Markdown = string(Markdown(n, ""))
	again, e = f.ImportMarkdown(ctx, p, pid, in)
	f.ok(e)
	if again.Action != "merge" || again.SubmissionID != first.SubmissionID {
		t.Fatal(again)
	}
	accepted, e := f.ReviewSubmission(ctx, first.SubmissionID, true, "")
	f.ok(e)
	again, e = f.ImportMarkdown(ctx, p, pid, in)
	f.ok(e)
	if again.Action != "skip" || again.NoteID != accepted.NoteID || again.SubmissionID != 0 {
		t.Fatal(again)
	}
	n.Body = "proposed replacement"
	in.Markdown = string(Markdown(n, ""))
	again, e = f.ImportMarkdown(ctx, p, pid, in)
	f.ok(e)
	if again.Action != "create" || again.SubmissionID == first.SubmissionID || again.NoteID != 0 {
		t.Fatal(again)
	}
	published, e := f.GetNote(ctx, p, pid, accepted.NoteID)
	f.ok(e)
	if published.Body != "changed pending" {
		t.Fatal("published before review")
	}
	accepted2, e := f.ReviewSubmission(ctx, again.SubmissionID, true, "")
	f.ok(e)
	if accepted2.NoteID != accepted.NoteID {
		t.Fatal("duplicate note")
	}
	for _, bad := range []string{"---\ntitle: bare\n---\nbody", "---\ntitle: null\n---\nbody", "---\ntitle: \"x\"\n", "---\nid: \"x\"\nid: \"y\"\n---\nbody"} {
		_, _, e = parseMarkdown([]byte(bad))
		if e == nil {
			t.Fatal("accepted malformed front matter")
		}
	}
}

func bundleFixture(t *testing.T) (*fx, string) {
	f := newFx(t)
	team, e := f.CreateTeam(ctx, "Team")
	f.ok(e)
	f.ok(f.AddTeamProject(ctx, team.ID, f.pid["fakture"]))
	f.ok(f.AddTeamProject(ctx, team.ID, f.pid["eureko"]))
	f.ok(f.AddTeamMember(ctx, team.ID, f.uid["ana"]))
	f.ok(f.SetTeamAssignment(ctx, team.ID, f.pid["fakture"], f.uid["ana"], Developer))
	for _, c := range []Class{Structure, Private} {
		_, _, e = f.CreateNote(ctx, f.p("ana"), f.pid["fakture"], string(c), NoteInput{Class: c, Title: string(c), Body: "body: role=owner\nusers: evil\n---\n"})
		f.ok(e)
	}
	_, _, e = f.Submit(ctx, f.p("ana"), f.pid["fakture"], "pending", SubmissionInput{NoteInput: NoteInput{Class: Data, Title: "pending", Body: "never export"}})
	f.ok(e)
	file := filepath.Join(t.TempDir(), "bundle.zip")
	f.ok(f.ExportBundle(ctx, BundleExport{Team: "team", Path: file}))
	return f, file
}
func countTable(t *testing.T, s *Store, table string) int {
	t.Helper()
	var n int
	if e := s.DB().QueryRow("SELECT count(*) FROM " + table).Scan(&n); e != nil {
		t.Fatal(e)
	}
	return n
}
func rewriteZip(t *testing.T, b bundle, mutate func(map[string][]byte)) string {
	t.Helper()
	data, e := json.Marshal(b.manifest)
	if e != nil {
		t.Fatal(e)
	}
	b.files["manifest.json"] = data
	if mutate != nil {
		mutate(b.files)
	}
	file := filepath.Join(t.TempDir(), "changed.zip")
	f, e := os.Create(file)
	if e != nil {
		t.Fatal(e)
	}
	z := zip.NewWriter(f)
	for name, data := range b.files {
		h := &zip.FileHeader{Name: name, Method: zip.Deflate}
		h.SetMode(0600)
		w, e := z.CreateHeader(h)
		if e != nil {
			t.Fatal(e)
		}
		if _, e = w.Write(data); e != nil {
			t.Fatal(e)
		}
	}
	if e = z.Close(); e != nil {
		t.Fatal(e)
	}
	if e = f.Close(); e != nil {
		t.Fatal(e)
	}
	return file
}
func TestBundleRoundTripAndConflicts(t *testing.T) {
	source, file := bundleFixture(t)
	b, e := readBundle(file)
	source.ok(e)
	if len(b.manifest.Notes) != 1 || len(b.manifest.Grants) != 1 {
		t.Fatal(b.manifest)
	}
	withPrivate := filepath.Join(t.TempDir(), "private.zip")
	source.ok(source.ExportBundle(ctx, BundleExport{Team: "Team", Path: withPrivate, WithPrivate: true}))
	private, e := readBundle(withPrivate)
	source.ok(e)
	if len(private.manifest.Notes) != 2 {
		t.Fatal(private.manifest)
	}
	target := openT(t, t.TempDir())
	defer target.Close()
	if target.GatewayID() == b.manifest.OriginGateway {
		t.Fatal("same gateway")
	}
	report, e := target.ImportBundle(ctx, BundleImport{Path: file, Source: "testsrc", DryRun: true, Map: map[string]string{"ana": "Ana Target"}})
	source.ok(e)
	if countTable(t, target, "users") != 0 || countTable(t, target, "audit_events") != 0 || !strings.Contains(strings.Join(report.Lines, "\n"), "create") {
		t.Fatal(report)
	}
	report, e = target.ImportBundle(ctx, BundleImport{Path: file, Source: "testsrc", Map: map[string]string{"ana": "Ana Target"}})
	source.ok(e)
	if countTable(t, target, "notes") != 1 || countTable(t, target, "kb_submissions") != 0 || countTable(t, target, "device_bindings") != 0 {
		t.Fatal(report)
	}
	var active, owner int
	source.ok(target.DB().QueryRow(`SELECT active,owner FROM users WHERE name='Ana Target'`).Scan(&active, &owner))
	if active != 0 || owner != 0 {
		t.Fatal(active, owner)
	}
	var id int64
	source.ok(target.DB().QueryRow(`SELECT id FROM notes`).Scan(&id))
	var n Note
	source.ok(target.read(ctx, func(tx *sql.Tx) error { var e error; n, e = getNote(tx, id); return e }))
	// Provenance is the owner's local import action, never what the bundle claims.
	if n.Provenance.ActorKind != ActorImport || n.Provenance.User != 0 || n.Provenance.SourceKind != SourceImport ||
		n.Provenance.SourceReference != "testsrc/fakture/1" || n.Provenance.Project != n.ProjectID ||
		n.Provenance.Device != "UNKNOWN" || n.Provenance.OriginGateway != "" || n.Provenance.Reviewer != "" || n.Provenance.Reviewed != nil {
		t.Fatal(n.Provenance)
	}
	// What the bundle says about its history is kept apart and unread by the provenance logic.
	if n.UnverifiedSource == "" || !strings.Contains(n.UnverifiedSource, source.GatewayID()) {
		t.Fatal(n.UnverifiedSource)
	}
	var key, origin string
	source.ok(target.DB().QueryRow(`SELECT key,origin_gateway FROM note_imports`).Scan(&key, &origin))
	if key != "src:testsrc:fakture:1" || origin != source.GatewayID() {
		t.Fatal(key, origin)
	}
	beforeAudit := countTable(t, target, "audit_events")
	if beforeAudit != 1 {
		t.Fatal("extra audits", beforeAudit)
	}
	_, e = target.ImportBundle(ctx, BundleImport{Path: file, Source: "testsrc", Map: map[string]string{"ana": "ana target"}})
	source.ok(e)
	if countTable(t, target, "notes") != 1 || countTable(t, target, "audit_events") != beforeAudit {
		t.Fatal("not a no-op")
	}
	source.ok(target.CheckMemberships(ctx))
	// A new source revision merges in place.
	var sn Note
	source.ok(source.read(ctx, func(tx *sql.Tx) error { var e error; sn, e = getNote(tx, 1); return e }))
	sn, e = source.UpdateNote(ctx, source.p("ana"), sn.ProjectID, sn.ID, sn.Revision, NoteInput{Class: Structure, Title: sn.Title, Body: "upstream"})
	source.ok(e)
	newer := filepath.Join(t.TempDir(), "newer.zip")
	source.ok(source.ExportBundle(ctx, BundleExport{Team: "Team", Path: newer}))
	_, e = target.ImportBundle(ctx, BundleImport{Path: newer, Source: "testsrc", Map: map[string]string{"ana": "Ana Target"}})
	source.ok(e)
	source.ok(target.read(ctx, func(tx *sql.Tx) error { var e error; n, e = getNote(tx, id); return e }))
	if n.Body != "upstream" || n.Revision != 2 {
		t.Fatal(n)
	}
	_, e = target.UpdateNote(ctx, importAdmin(t, target), n.ProjectID, n.ID, n.Revision, NoteInput{Class: n.Class, Title: n.Title, Body: "local"})
	source.ok(e)
	sn, e = source.UpdateNote(ctx, source.p("ana"), sn.ProjectID, sn.ID, sn.Revision, NoteInput{Class: Structure, Title: sn.Title, Body: "upstream two"})
	source.ok(e)
	latest := filepath.Join(t.TempDir(), "latest.zip")
	source.ok(source.ExportBundle(ctx, BundleExport{Team: "Team", Path: latest}))
	report, e = target.ImportBundle(ctx, BundleImport{Path: latest, Source: "testsrc", Map: map[string]string{"ana": "Ana Target"}})
	source.ok(e)
	if !strings.Contains(strings.Join(report.Lines, "\n"), "local edit") {
		t.Fatal(report)
	}
	if countTable(t, target, "notes") != 1 {
		t.Fatal("duplicate")
	}
	// Membership imports may only raise roles.
	users, e := target.ListUsers(ctx)
	source.ok(e)
	for _, u := range users {
		if u.Name == "Ana Target" {
			source.ok(target.SetMember(ctx, n.ProjectID, u.ID, Admin))
		}
	}
	_, e = target.ImportBundle(ctx, BundleImport{Path: file, Source: "testsrc", Map: map[string]string{"ana": "Ana Target"}})
	source.ok(e)
	var role Role
	source.ok(target.DB().QueryRow(`SELECT role FROM direct_memberships d JOIN users u ON u.id=d.user_id WHERE u.name='Ana Target'`).Scan(&role))
	if role != Admin {
		t.Fatal("lowered role")
	}
}

func TestBundleRefusalsAreAtomic(t *testing.T) {
	f, file := bundleFixture(t)
	original, e := readBundle(file)
	f.ok(e)
	cases := []struct {
		name   string
		change func(map[string][]byte)
	}{
		{"zip slip", func(files map[string][]byte) { files["../escape.md"] = []byte("x") }},
		{"absolute", func(files map[string][]byte) { files["/escape.md"] = []byte("x") }},
		{"backslash", func(files map[string][]byte) { files["a\\b"] = []byte("x") }},
		{"unknown manifest", func(files map[string][]byte) {
			files["manifest.json"] = bytes.Replace(files["manifest.json"], []byte(`"format":1`), []byte(`"format":1,"evil":true`), 1)
		}},
		{"unknown nested manifest", func(files map[string][]byte) {
			files["manifest.json"] = bytes.Replace(files["manifest.json"], []byte(`"slug":`), []byte(`"evil":1,"slug":`), 1)
		}},
		{"bad version", func(files map[string][]byte) {
			files["manifest.json"] = bytes.Replace(files["manifest.json"], []byte(`"format":1`), []byte(`"format":2`), 1)
		}},
		{"hash", func(files map[string][]byte) {
			for name := range files {
				if strings.Contains(name, "/") {
					files[name] = append(files[name], 'x')
					break
				}
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := bundle{manifest: original.manifest, files: map[string][]byte{}}
			for k, v := range original.files {
				b.files[k] = v
			}
			bad := rewriteZip(t, b, tc.change)
			target := openT(t, t.TempDir())
			defer target.Close()
			for _, dry := range []bool{true, false} {
				report, e := target.ImportBundle(ctx, BundleImport{Path: bad, Source: "testsrc", DryRun: dry})
				if e == nil || !strings.Contains(strings.Join(report.Lines, "\n"), "refuse") {
					t.Fatal(report, e)
				}
			}
			for _, table := range []string{"users", "teams", "projects", "notes", "audit_events", "device_bindings"} {
				if countTable(t, target, table) != 0 {
					t.Fatal("wrote", table)
				}
			}
		})
	}
	t.Run("symlink", func(t *testing.T) {
		bad := filepath.Join(t.TempDir(), "link.zip")
		out, e := os.Create(bad)
		f.ok(e)
		z := zip.NewWriter(out)
		h := &zip.FileHeader{Name: "manifest.json"}
		h.SetMode(os.ModeSymlink | 0777)
		w, e := z.CreateHeader(h)
		f.ok(e)
		_, e = w.Write([]byte("elsewhere"))
		f.ok(e)
		f.ok(z.Close())
		f.ok(out.Close())
		target := openT(t, t.TempDir())
		defer target.Close()
		_, e = target.ImportBundle(ctx, BundleImport{Path: bad, Source: "testsrc"})
		if e == nil {
			t.Fatal("accepted link")
		}
	})
	t.Run("team conflict", func(t *testing.T) {
		target := openT(t, t.TempDir())
		defer target.Close()
		p, e := target.CreateProject(ctx, "fakture", "Existing")
		f.ok(e)
		team, e := target.CreateTeam(ctx, "Other")
		f.ok(e)
		f.ok(target.AddTeamProject(ctx, team.ID, p.ID))
		before := countTable(t, target, "audit_events")
		_, e = target.ImportBundle(ctx, BundleImport{Path: file, Source: "testsrc"})
		if e == nil {
			t.Fatal("stole project")
		}
		if countTable(t, target, "users") != 0 || countTable(t, target, "notes") != 0 || countTable(t, target, "audit_events") != before || countTable(t, target, "teams") != 1 {
			t.Fatal("partial transaction")
		}
	})
	t.Run("late failure", func(t *testing.T) {
		target := openT(t, t.TempDir())
		defer target.Close()
		f.ok(target.write(ctx, func(tx *sql.Tx) error {
			_, e := tx.Exec(`CREATE TRIGGER fail_import BEFORE INSERT ON note_imports BEGIN SELECT RAISE(ABORT,'fail'); END`)
			return e
		}))
		_, e = target.ImportBundle(ctx, BundleImport{Path: file, Source: "testsrc"})
		if e == nil {
			t.Fatal("expected failure")
		}
		for _, table := range []string{"users", "teams", "projects", "notes", "note_revisions", "memberships", "audit_events"} {
			if countTable(t, target, table) != 0 {
				t.Fatal("partial write", table)
			}
		}
		if dbHigh := func() int64 {
			var v int64
			target.DB().QueryRow(`SELECT value FROM meta WHERE key='project_high'`).Scan(&v)
			return v
		}(); dbHigh != 0 {
			t.Fatal(dbHigh)
		}
	})
	t.Run("limits", func(t *testing.T) {
		target := openT(t, t.TempDir())
		defer target.Close()
		oldBytes, oldNotes := maxBytes, maxNotes
		defer func() { maxBytes, maxNotes = oldBytes, oldNotes }()
		maxBytes = 10
		_, e := target.ImportBundle(ctx, BundleImport{Path: file, Source: "testsrc"})
		if e == nil {
			t.Fatal("byte cap")
		}
		maxBytes = oldBytes
		maxNotes = 0
		_, e = target.ImportBundle(ctx, BundleImport{Path: file, Source: "testsrc"})
		if e == nil {
			t.Fatal("note cap")
		}
		if countTable(t, target, "users") != 0 {
			t.Fatal("wrote before validation")
		}
	})
}

func TestRejectedImportCanBeCorrected(t *testing.T) {
	f := newFx(t)
	p := f.p("ana")
	pid := f.pid["fakture"]
	in := MarkdownInput{Markdown: "first", Path: "proposal.md", Class: Structure, Submit: true}
	first, e := f.ImportMarkdown(ctx, p, pid, in)
	f.ok(e)
	_, e = f.ReviewSubmission(ctx, first.SubmissionID, false, "correct it")
	f.ok(e)
	same, e := f.ImportMarkdown(ctx, p, pid, in)
	f.ok(e)
	if same.Action != "skip" || same.SubmissionID != first.SubmissionID {
		t.Fatal(same)
	}
	in.Markdown = "corrected"
	next, e := f.ImportMarkdown(ctx, p, pid, in)
	f.ok(e)
	if next.Action != "create" || next.SubmissionID == first.SubmissionID {
		t.Fatal(next)
	}
	accepted, e := f.ReviewSubmission(ctx, next.SubmissionID, true, "")
	f.ok(e)
	in.Markdown = "replacement"
	proposal, e := f.ImportMarkdown(ctx, p, pid, in)
	f.ok(e)
	_, e = f.ReviewSubmission(ctx, proposal.SubmissionID, false, "correct again")
	f.ok(e)
	in.Markdown = "corrected replacement"
	corrected, e := f.ImportMarkdown(ctx, p, pid, in)
	f.ok(e)
	if corrected.Action != "create" || corrected.SubmissionID == proposal.SubmissionID {
		t.Fatal(corrected)
	}
	approved, e := f.ReviewSubmission(ctx, corrected.SubmissionID, true, "")
	f.ok(e)
	if approved.NoteID != accepted.NoteID || countTable(t, f.Store, "notes") != 1 {
		t.Fatal(approved)
	}
	// A local edit made after rejection is still protected.
	in.Markdown = "third proposal"
	proposal, e = f.ImportMarkdown(ctx, p, pid, in)
	f.ok(e)
	_, e = f.ReviewSubmission(ctx, proposal.SubmissionID, false, "needs work")
	f.ok(e)
	n, e := f.GetNote(ctx, p, pid, accepted.NoteID)
	f.ok(e)
	_, e = f.UpdateNote(ctx, p, pid, n.ID, n.Revision, NoteInput{Class: Structure, Title: n.Title, Body: "local edit"})
	f.ok(e)
	in.Markdown = "another corrected version"
	result, e := f.ImportMarkdown(ctx, p, pid, in)
	f.ok(e)
	if result.Action != "skip" || result.Reason != "local edit" {
		t.Fatal(result)
	}
}

func TestBundleClassUpdateInvalidatesCapabilities(t *testing.T) {
	f, file := bundleFixture(t)
	target := openT(t, t.TempDir())
	defer target.Close()
	_, e := target.ImportBundle(ctx, BundleImport{Path: file, Source: "testsrc"})
	f.ok(e)
	before, _ := target.Epoch(ctx)
	var source Note
	f.ok(f.read(ctx, func(tx *sql.Tx) error { var e error; source, e = getNote(tx, 1); return e }))
	_, e = f.UpdateNote(ctx, f.p("fani"), source.ProjectID, source.ID, source.Revision, NoteInput{Class: Private, Title: source.Title, Body: "new private body"})
	f.ok(e)
	file = filepath.Join(t.TempDir(), "private-changed.zip")
	f.ok(f.ExportBundle(ctx, BundleExport{Team: "Team", Projects: []string{"fakture"}, Path: file, WithPrivate: true}))
	_, e = target.ImportBundle(ctx, BundleImport{Path: file, Source: "testsrc"})
	f.ok(e)
	after, _ := target.Epoch(ctx)
	if after <= before || countTable(t, target, "audit_events") != 2 {
		t.Fatal(after, before)
	}
	var class Class
	f.ok(target.DB().QueryRow(`SELECT class FROM notes WHERE id=1`).Scan(&class))
	if class != Private {
		t.Fatal(class)
	}
	var imported Note
	f.ok(target.read(ctx, func(tx *sql.Tx) error { var e error; imported, e = getNote(tx, 1); return e }))
	if imported.Provenance.ActorKind != ActorImport || imported.Provenance.User != 0 || imported.Provenance.Project != imported.ProjectID ||
		imported.Provenance.SourceKind != SourceImport || imported.Provenance.SourceReference != "testsrc/fakture/1" {
		t.Fatal("changed source provenance", imported.Provenance)
	}
	side, e := readState(target.dir)
	f.ok(e)
	if side.Epoch != after {
		t.Fatal(side)
	}
	next, e := target.CreateProject(ctx, "after-import", "After import")
	f.ok(e)
	if next.ID <= 2 {
		t.Fatal("reused project id", next)
	}
}

func TestBundleClassChangeRules(t *testing.T) {
	source, file := bundleFixture(t)
	// A local class change bumps the revision; a later upstream body change must not undo it.
	target := openT(t, t.TempDir())
	defer target.Close()
	_, e := target.ImportBundle(ctx, BundleImport{Path: file, Source: "testsrc"})
	source.ok(e)
	var imported Note
	source.ok(target.read(ctx, func(tx *sql.Tx) error { var e error; imported, e = getNote(tx, 1); return e }))
	if imported.Class != Structure {
		t.Fatal(imported.Class)
	}
	_, e = target.UpdateNote(ctx, importAdmin(t, target), imported.ProjectID, imported.ID, imported.Revision, NoteInput{Class: Private, Title: imported.Title, Body: imported.Body})
	source.ok(e)
	var sn Note
	source.ok(source.read(ctx, func(tx *sql.Tx) error { var e error; sn, e = getNote(tx, 1); return e }))
	_, e = source.UpdateNote(ctx, source.p("ana"), sn.ProjectID, sn.ID, sn.Revision, NoteInput{Class: Structure, Title: sn.Title, Body: "upstream change"})
	source.ok(e)
	newer := filepath.Join(t.TempDir(), "newer.zip")
	source.ok(source.ExportBundle(ctx, BundleExport{Team: "Team", Path: newer}))
	report, e := target.ImportBundle(ctx, BundleImport{Path: newer, Source: "testsrc"})
	source.ok(e)
	if !strings.Contains(strings.Join(report.Lines, "\n"), "local edit") {
		t.Fatal(report)
	}
	var class Class
	var body string
	source.ok(target.DB().QueryRow(`SELECT class,body FROM notes WHERE id=1`).Scan(&class, &body))
	if class != Private || body != imported.Body {
		t.Fatal(class, body)
	}

	// An upstream class-only change is applied to a note that was not changed locally.
	other := openT(t, t.TempDir())
	defer other.Close()
	_, e = other.ImportBundle(ctx, BundleImport{Path: file, Source: "testsrc"})
	source.ok(e)
	sn, e = source.GetNote(ctx, source.p("fani"), sn.ProjectID, sn.ID)
	source.ok(e)
	_, e = source.UpdateNote(ctx, source.p("fani"), sn.ProjectID, sn.ID, sn.Revision, NoteInput{Class: Data, Title: sn.Title, Body: sn.Body})
	source.ok(e)
	classOnly := filepath.Join(t.TempDir(), "class.zip")
	source.ok(source.ExportBundle(ctx, BundleExport{Team: "Team", Path: classOnly}))
	_, e = other.ImportBundle(ctx, BundleImport{Path: classOnly, Source: "testsrc"})
	source.ok(e)
	var class2 Class
	var revision int64
	source.ok(other.DB().QueryRow(`SELECT class,revision FROM notes WHERE id=1`).Scan(&class2, &revision))
	if class2 != Data || revision != 2 {
		t.Fatal(class2, revision)
	}
}

func TestBundleSourceBinding(t *testing.T) {
	source, file := bundleFixture(t)
	target := openT(t, t.TempDir())
	defer target.Close()
	_, e := target.ImportBundle(ctx, BundleImport{Path: file, Source: "one"})
	source.ok(e)
	var origin string
	source.ok(target.DB().QueryRow(`SELECT origin_gateway FROM import_sources WHERE source='one'`).Scan(&origin))
	if origin != source.GatewayID() {
		t.Fatal(origin)
	}
	// A different declared gateway under the same source is refused.
	other, otherFile := bundleFixture(t)
	if other.GatewayID() == source.GatewayID() {
		t.Fatal("test gateways have the same id")
	}
	before := countTable(t, target, "notes")
	_, e = target.ImportBundle(ctx, BundleImport{Path: otherFile, Source: "one"})
	if e == nil || !strings.Contains(e.Error(), "first imported from another gateway") {
		t.Fatal(e)
	}
	if countTable(t, target, "notes") != before {
		t.Fatal("wrote notes")
	}
	// A dry run names a new source and the origin it would record, and refuses a bound source.
	report, e := target.ImportBundle(ctx, BundleImport{Path: file, Source: "two", DryRun: true})
	source.ok(e)
	if !strings.Contains(strings.Join(report.Lines, "\n"), "source two, origin "+source.GatewayID()) {
		t.Fatal(report)
	}
	if _, e = target.ImportBundle(ctx, BundleImport{Path: otherFile, Source: "one", DryRun: true}); e == nil {
		t.Fatal("dry run ignored the source binding")
	}
	// A bundle of this gateway's own export is refused.
	team, e := target.CreateTeam(ctx, "Own")
	source.ok(e)
	project, e := target.CreateProject(ctx, "own", "Own")
	source.ok(e)
	source.ok(target.AddTeamProject(ctx, team.ID, project.ID))
	self := filepath.Join(t.TempDir(), "self.zip")
	source.ok(target.ExportBundle(ctx, BundleExport{Team: "Own", Path: self}))
	rep, e := target.ImportBundle(ctx, BundleImport{Path: self, Source: "three"})
	if e == nil || !strings.Contains(e.Error(), "exported by this gateway") {
		t.Fatal(e)
	}
	// one clear sentence: no glued "refuse invalid" prefix
	if want := "this bundle was exported by this gateway: a gateway cannot import its own bundle"; len(rep.Lines) != 1 || rep.Lines[0] != want || e.Error() != want {
		t.Fatalf("%q %v", rep.Lines, e)
	}
	if _, e = target.ImportBundle(ctx, BundleImport{Path: file, Source: ""}); e == nil {
		t.Fatal("accepted an empty source name")
	}
}

func TestBundleImportedProvenanceIsTrusted(t *testing.T) {
	f, file := bundleFixture(t)
	b, e := readBundle(file)
	f.ok(e)
	reviewed := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	b.manifest.Notes[0].Provenance.Reviewer = "owner"
	b.manifest.Notes[0].Provenance.Reviewed = &reviewed
	b.manifest.Notes[0].Provenance.User = 999
	bad := rewriteZip(t, b, nil)
	target := openT(t, t.TempDir())
	defer target.Close()
	_, e = target.ImportBundle(ctx, BundleImport{Path: bad, Source: "testsrc"})
	f.ok(e)
	var n Note
	f.ok(target.read(ctx, func(tx *sql.Tx) error { var e error; n, e = getNote(tx, 1); return e }))
	if n.Provenance.ActorKind != ActorImport || n.Provenance.User != 0 || n.Provenance.Reviewer != "" ||
		n.Provenance.Reviewed != nil || n.Provenance.SourceKind != SourceImport {
		t.Fatal("bundle provenance became local", n.Provenance)
	}
	// The bundle's claims are kept, unverified and apart.
	if !strings.Contains(n.UnverifiedSource, `"reviewer":"owner"`) || !strings.Contains(n.UnverifiedSource, `"user":999`) {
		t.Fatal(n.UnverifiedSource)
	}
}

func TestBundleUserMappingUnicode(t *testing.T) {
	source, file := bundleFixture(t)
	// Two target accounts that only Unicode-normalize to one name cannot be told apart.
	target := openT(t, t.TempDir())
	defer target.Close()
	if _, e := target.CreateUser(ctx, "Änne"); e != nil {
		t.Fatal(e)
	}
	if _, e := target.CreateUser(ctx, "änne"); e != nil {
		t.Fatal(e)
	}
	if _, e := target.ImportBundle(ctx, BundleImport{Path: file, Source: "u", Map: map[string]string{"ana": "Änne"}}); e == nil || !strings.Contains(e.Error(), "ambiguous target users") {
		t.Fatal(e)
	}
	// An explicit map to a different account resolves the ambiguity.
	if _, e := target.ImportBundle(ctx, BundleImport{Path: file, Source: "u", Map: map[string]string{"ana": "Other"}}); e != nil {
		t.Fatal(e)
	}
	// Two bundle users mapped onto one target account are refused.
	clean := openT(t, t.TempDir())
	defer clean.Close()
	b, e := readBundle(file)
	source.ok(e)
	// bor is already included by the fixture's direct memberships.
	twoUsers := rewriteZip(t, b, nil)
	if _, e := clean.ImportBundle(ctx, BundleImport{Path: twoUsers, Source: "u", Map: map[string]string{"ana": "Ann", "bor": "ann"}}); e == nil || !strings.Contains(e.Error(), "two bundle users map to one target user") {
		t.Fatal(e)
	}
	// A decomposed spelling resolves to the precomposed target account.
	pre := openT(t, t.TempDir())
	defer pre.Close()
	u, e := pre.CreateUser(ctx, "Änne")
	source.ok(e)
	_, e = pre.ImportBundle(ctx, BundleImport{Path: file, Source: "u", Map: map[string]string{"ana": "Änne"}})
	source.ok(e)
	var author int64
	source.ok(pre.DB().QueryRow(`SELECT author_id FROM notes`).Scan(&author))
	if author != u.ID {
		t.Fatal(author, u.ID)
	}
}

func TestBundleArchiveLimits(t *testing.T) {
	f, file := bundleFixture(t)
	cases := []struct {
		name   string
		limit  func()
		change func(map[string][]byte)
	}{
		{"manifest size", func() { maxManifestBytes = 8 }, nil},
		{"central directory", func() { maxBundleDirBytes = 8 }, nil},
		{"manifest nesting", func() { maxBundleDepth = 8 }, func(files map[string][]byte) {
			deep := strings.Repeat("[", 12) + strings.Repeat("]", 12)
			files["manifest.json"] = bytes.Replace(files["manifest.json"], []byte(`"format":1`), []byte(`"format":1,"deep":`+deep), 1)
		}},
		{"projects", func() { maxBundleProjects = 0 }, nil},
		{"users", func() { maxBundleUsers = 0 }, nil},
		{"memberships", func() { maxBundleMemberships = 0 }, nil},
		{"grants", func() { maxBundleGrants = 0 }, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			oldManifest, oldDir, oldDepth := maxManifestBytes, maxBundleDirBytes, maxBundleDepth
			oldProjects, oldUsers := maxBundleProjects, maxBundleUsers
			oldMembers, oldGrants := maxBundleMemberships, maxBundleGrants
			defer func() {
				maxManifestBytes, maxBundleDirBytes, maxBundleDepth = oldManifest, oldDir, oldDepth
				maxBundleProjects, maxBundleUsers = oldProjects, oldUsers
				maxBundleMemberships, maxBundleGrants = oldMembers, oldGrants
			}()
			tc.limit()
			bad := file
			if tc.change != nil {
				b, e := readBundle(file)
				f.ok(e)
				bad = rewriteZip(t, b, tc.change)
			}
			target := openT(t, t.TempDir())
			defer target.Close()
			report, e := target.ImportBundle(ctx, BundleImport{Path: bad, Source: "testsrc", DryRun: true})
			if !errors.Is(e, ErrTooLarge) {
				t.Fatal(report, e)
			}
			if countTable(t, target, "users") != 0 {
				t.Fatal("wrote before refusing")
			}
		})
	}
}

func TestBundleAmbiguousFileNames(t *testing.T) {
	f, file := bundleFixture(t)
	b, e := readBundle(file)
	f.ok(e)
	b.files["FAKTURE/1.md"] = b.files["fakture/1.md"]
	bad := rewriteZip(t, b, nil)
	target := openT(t, t.TempDir())
	defer target.Close()
	if _, e = target.ImportBundle(ctx, BundleImport{Path: bad, Source: "testsrc"}); e == nil || !strings.Contains(e.Error(), "ambiguous zip entry name") {
		t.Fatal(e)
	}
}

func TestBundleSkipsLocalSubmission(t *testing.T) {
	f, file := bundleFixture(t)
	b, e := readBundle(file)
	f.ok(e)
	target := newFx(t)
	first := b.manifest.Notes[0]
	proposal, e := target.ImportMarkdown(ctx, target.p("ana"), target.pid["fakture"], MarkdownInput{Markdown: string(b.files[first.File]), Path: first.File, Submit: true})
	target.ok(e)
	if proposal.SubmissionID == 0 {
		t.Fatal(proposal)
	}
	for i := 0; i < 2; i++ {
		report, e := target.ImportBundle(ctx, BundleImport{Path: file, Source: "testsrc"})
		target.ok(e)
		if !strings.Contains(strings.Join(report.Lines, "\n"), "local submission") {
			t.Fatal(report)
		}
	}
	if countTable(t, target.Store, "notes") != 0 || countTable(t, target.Store, "kb_submissions") != 1 {
		t.Fatal("duplicated local proposal")
	}
}

func importAdmin(t *testing.T, s *Store) Principal {
	t.Helper()
	var id int64
	if e := s.DB().QueryRow(`SELECT id FROM users WHERE name="fani"`).Scan(&id); e != nil {
		t.Fatal(e)
	}
	if e := s.SetUserActive(ctx, id, true); e != nil {
		t.Fatal(e)
	}
	if e := s.SetBinding(ctx, "local-admin", id); e != nil {
		t.Fatal(e)
	}
	p, e := s.Principal(ctx, "local-admin", true)
	if e != nil {
		t.Fatal(e)
	}
	return p
}
