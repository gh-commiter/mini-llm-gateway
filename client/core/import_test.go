package core

import (
	"archive/zip"
	"bytes"
	"database/sql"
	"encoding/json"
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
	report, e := target.ImportBundle(ctx, BundleImport{Path: file, DryRun: true, Map: map[string]string{"ana": "Ana Target"}})
	source.ok(e)
	if countTable(t, target, "users") != 0 || countTable(t, target, "audit_events") != 0 || !strings.Contains(strings.Join(report.Lines, "\n"), "create") {
		t.Fatal(report)
	}
	report, e = target.ImportBundle(ctx, BundleImport{Path: file, Map: map[string]string{"ana": "Ana Target"}})
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
	if n.Provenance.Device != "UNKNOWN" || n.Provenance.OriginGateway != source.GatewayID() || n.Provenance.Exported == "" {
		t.Fatal(n.Provenance)
	}
	beforeAudit := countTable(t, target, "audit_events")
	if beforeAudit != 1 {
		t.Fatal("extra audits", beforeAudit)
	}
	_, e = target.ImportBundle(ctx, BundleImport{Path: file, Map: map[string]string{"ana": "ana target"}})
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
	_, e = target.ImportBundle(ctx, BundleImport{Path: newer, Map: map[string]string{"ana": "Ana Target"}})
	source.ok(e)
	source.ok(target.read(ctx, func(tx *sql.Tx) error { var e error; n, e = getNote(tx, id); return e }))
	if n.Body != "upstream" || n.Revision != 2 {
		t.Fatal(n)
	}
	source.ok(target.write(ctx, func(tx *sql.Tx) error {
		_, e := tx.Exec(`UPDATE notes SET body='local',bytes=length(title)+5 WHERE id=?`, id)
		return e
	}))
	sn, e = source.UpdateNote(ctx, source.p("ana"), sn.ProjectID, sn.ID, sn.Revision, NoteInput{Class: Structure, Title: sn.Title, Body: "upstream two"})
	source.ok(e)
	latest := filepath.Join(t.TempDir(), "latest.zip")
	source.ok(source.ExportBundle(ctx, BundleExport{Team: "Team", Path: latest}))
	report, e = target.ImportBundle(ctx, BundleImport{Path: latest, Map: map[string]string{"ana": "Ana Target"}})
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
	_, e = target.ImportBundle(ctx, BundleImport{Path: file, Map: map[string]string{"ana": "Ana Target"}})
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
				report, e := target.ImportBundle(ctx, BundleImport{Path: bad, DryRun: dry})
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
		_, e = target.ImportBundle(ctx, BundleImport{Path: bad})
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
		_, e = target.ImportBundle(ctx, BundleImport{Path: file})
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
		_, e = target.ImportBundle(ctx, BundleImport{Path: file})
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
		_, e := target.ImportBundle(ctx, BundleImport{Path: file})
		if e == nil {
			t.Fatal("byte cap")
		}
		maxBytes = oldBytes
		maxNotes = 0
		_, e = target.ImportBundle(ctx, BundleImport{Path: file})
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
	_, e := target.ImportBundle(ctx, BundleImport{Path: file})
	f.ok(e)
	before, _ := target.Epoch(ctx)
	var source Note
	f.ok(f.read(ctx, func(tx *sql.Tx) error { var e error; source, e = getNote(tx, 1); return e }))
	_, e = f.UpdateNote(ctx, f.p("fani"), source.ProjectID, source.ID, source.Revision, NoteInput{Class: Private, Title: source.Title, Body: "new private body"})
	f.ok(e)
	file = filepath.Join(t.TempDir(), "private-changed.zip")
	f.ok(f.ExportBundle(ctx, BundleExport{Team: "Team", Projects: []string{"fakture"}, Path: file, WithPrivate: true}))
	_, e = target.ImportBundle(ctx, BundleImport{Path: file})
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
	if imported.Provenance.User != f.uid["fani"] || imported.Provenance.Project != source.ProjectID {
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
		report, e := target.ImportBundle(ctx, BundleImport{Path: file})
		target.ok(e)
		if !strings.Contains(strings.Join(report.Lines, "\n"), "local submission") {
			t.Fatal(report)
		}
	}
	if countTable(t, target.Store, "notes") != 0 || countTable(t, target.Store, "kb_submissions") != 1 {
		t.Fatal("duplicated local proposal")
	}
}
