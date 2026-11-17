package core

import (
	"archive/zip"
	"bytes"
	"database/sql"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestBundleSourceNameValidation(t *testing.T) {
	for _, s := range []string{"remote", strings.Repeat("é", 64)} {
		if !validSourceName(s) {
			t.Fatalf("refused %q", s)
		}
	}
	for _, s := range []string{"", " ", strings.Repeat("a", 65), "line\nfeed", "bad\xff", "hidden\u200b"} {
		if validSourceName(s) {
			t.Fatalf("accepted %q", s)
		}
	}
}

func TestBundleEntryLimitsBeforeBodies(t *testing.T) {
	f, file := bundleFixture(t)
	data, err := os.ReadFile(file)
	f.ok(err)
	eocd := bytes.LastIndex(data, []byte{'P', 'K', 5, 6})
	dir := int(binary.LittleEndian.Uint32(data[eocd+16:]))
	// A later declared oversize entry must be rejected even if the first body is corrupt.
	first := dir
	for dir+46 < eocd {
		next := dir + 46 + int(binary.LittleEndian.Uint16(data[dir+28:])) + int(binary.LittleEndian.Uint16(data[dir+30:])) + int(binary.LittleEndian.Uint16(data[dir+32:]))
		if next == eocd {
			break
		}
		dir = next
	}
	binary.LittleEndian.PutUint32(data[dir+24:], uint32(maxBytes+1))
	local := int(binary.LittleEndian.Uint32(data[first+42:]))
	body := local + 30 + int(binary.LittleEndian.Uint16(data[local+26:])) + int(binary.LittleEndian.Uint16(data[local+28:]))
	data[body] ^= 0xff
	bad := filepath.Join(t.TempDir(), "oversize.zip")
	f.ok(os.WriteFile(bad, data, 0600))
	if _, err = readBundle(bad); !errors.Is(err, ErrTooLarge) {
		t.Fatal(err)
	}
	// The actual-output reader enforces its own limit too.
	z, err := zip.OpenReader(file)
	f.ok(err)
	defer z.Close()
	if _, err = readEntry(z.File[0], 1); !errors.Is(err, ErrTooLarge) {
		t.Fatal(err)
	}
}

func TestBundleSourcesHaveIndependentReceipts(t *testing.T) {
	f, file := bundleFixture(t)
	s := openT(t, t.TempDir())
	defer s.Close()
	for _, source := range []string{"one", "two"} {
		_, err := s.ImportBundle(ctx, BundleImport{Path: file, Source: source})
		f.ok(err)
	}
	if countTable(t, s, "notes") != 2 || countTable(t, s, "note_imports") != 2 {
		t.Fatal("shared receipt namespace")
	}
}

func TestBundleDirectoryCannotLie(t *testing.T) {
	f, file := bundleFixture(t)
	data, err := os.ReadFile(file)
	f.ok(err)
	eocd := bytes.LastIndex(data, []byte{'P', 'K', 5, 6})
	for _, field := range []int{8, 12, 16} {
		t.Run(i64(int64(field)), func(t *testing.T) {
			bad := append([]byte(nil), data...)
			if field == 8 {
				// Both counts agree but lie about the actual records.
				binary.LittleEndian.PutUint16(bad[eocd+8:], 1)
				binary.LittleEndian.PutUint16(bad[eocd+10:], 1)
			} else {
				binary.LittleEndian.PutUint32(bad[eocd+field:], 0)
			}
			name := filepath.Join(t.TempDir(), "bad.zip")
			f.ok(os.WriteFile(name, bad, 0600))
			in, err := os.Open(name)
			f.ok(err)
			defer in.Close()
			if err = checkBundleDirectory(in, int64(len(bad))); err == nil {
				t.Fatal("trusted a forged footer")
			}
		})
	}
}

func TestManifestCollectionsBoundedBeforeDecode(t *testing.T) {
	old := maxBundleProjects
	defer func() { maxBundleProjects = old }()
	maxBundleProjects = 1
	// Invalid element types must not reach typed decoding and allocate the collection.
	for _, key := range []string{"projects", "PROJECTS"} {
		if err := preflightManifest([]byte(`{"` + key + `":[0,0]}`)); !errors.Is(err, ErrTooLarge) {
			t.Fatal(err)
		}
	}
}

func TestBundleUnicodeFileCollisions(t *testing.T) {
	f, file := bundleFixture(t)
	b, err := readBundle(file)
	f.ok(err)
	b.files["notes/Ä.md"] = []byte("one")
	b.files["notes/A\u0308.md"] = []byte("two")
	_, err = readBundle(rewriteZip(t, b, nil))
	if err == nil || !strings.Contains(err.Error(), "ambiguous zip entry") {
		t.Fatal(err)
	}
}

func TestBundleDisplayAuthorHasNoOwnership(t *testing.T) {
	f, file := bundleFixture(t)
	target := newFx(t)
	_, err := target.ImportBundle(ctx, BundleImport{Path: file, Source: "remote"})
	f.ok(err)
	var n Note
	f.ok(target.read(ctx, func(tx *sql.Tx) error { var e error; n, e = getNote(tx, 1); return e }))
	if n.Author != "ana" || n.AuthorID != 0 {
		t.Fatal(n)
	}
	// Developers may write their own structure notes, but not a bundle's display attribution.
	_, err = target.UpdateNote(ctx, target.p("ana"), n.ProjectID, n.ID, n.Revision, NoteInput{Class: Structure, Title: n.Title, Body: "local"})
	is(t, err, ErrForbidden)
	// A private imported note remains visible to admins, not to its display author.
	_, err = target.UpdateNote(ctx, target.p("fani"), n.ProjectID, n.ID, n.Revision, NoteInput{Class: Private, Title: "comet", Body: "private comet"})
	f.ok(err)
	_, err = target.GetNote(ctx, target.p("ana"), n.ProjectID, n.ID)
	is(t, err, ErrNotFound)
	page, err := target.ListNotes(ctx, target.p("ana"), n.ProjectID, "", "", 100)
	f.ok(err)
	if len(page.Notes) != 0 {
		t.Fatal(page)
	}
	f.ok(target.RebuildSearch(ctx))
	hits, err := target.SearchNotes(ctx, target.p("ana"), n.ProjectID, "comet")
	f.ok(err)
	if len(hits.Hits) != 0 {
		t.Fatal(hits)
	}
	var scope string
	f.ok(target.DB().QueryRow(`SELECT scope FROM note_search WHERE rowid=1`).Scan(&scope))
	if !strings.Contains(scope, "privateu0") {
		t.Fatal(scope)
	}
}

func TestBundleReceiptBackupRestore(t *testing.T) {
	f, file := bundleFixture(t)
	dir := t.TempDir()
	s := openT(t, dir)
	_, err := s.ImportBundle(ctx, BundleImport{Path: file, Source: "remote"})
	f.ok(err)
	bak := filepath.Join(t.TempDir(), "backup.db")
	f.ok(s.Backup(ctx, bak))
	f.ok(s.Close())
	f.ok(Restore(dir, bak))
	s = openT(t, dir)
	defer s.Close()
	_, err = s.ImportBundle(ctx, BundleImport{Path: file, Source: "remote"})
	f.ok(err)
	if countTable(t, s, "notes") != 1 || countTable(t, s, "note_imports") != 1 || countTable(t, s, "import_sources") != 1 {
		t.Fatal("lost import state")
	}
	var n Note
	f.ok(s.read(ctx, func(tx *sql.Tx) error { var e error; n, e = getNote(tx, 1); return e }))
	if n.AuthorID != 0 || n.UnverifiedSource == "" || n.Provenance.ActorKind != ActorImport {
		t.Fatal(n)
	}
}
