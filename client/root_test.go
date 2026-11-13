package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Resolve first, then replace a parent: the operation must enforce confinement again.
func TestRootOperationsRefuseSwappedParent(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "x"), []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "source"), []byte("inside"), 0o600); err != nil {
		t.Fatal(err)
	}
	rooted, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer rooted.Close()
	if _, _, ok := davResolve("/s/d/x", "/s", root); !ok {
		t.Fatal("initial path refused")
	}
	if err := os.Rename(filepath.Join(root, "d"), filepath.Join(root, "old")); err != nil {
		t.Fatal(err)
	}
	target, err := filepath.Rel(root, outside)
	if err != nil {
		t.Fatal(err)
	}
	dirLink(t, target, filepath.Join(root, "d"))
	tests := []struct {
		name string
		op   func() error
	}{
		{"PUT", func() error { return rootWriteAtomic(rooted, "d/x", []byte("bad")) }},
		{"sync write", func() error { return syncWrite(rooted, "d/x", []byte("bad"), 0, false, false) }},
		{"COPY source", func() error { return rootCopyTree(rooted, "d/x", "copy") }},
		{"COPY destination", func() error { return rootCopyTree(rooted, "source", "d/copy") }},
		{"MOVE source", func() error { return rooted.Rename("d/x", "moved") }},
		{"MOVE destination", func() error { return rooted.Rename("source", "d/moved") }},
		{"DELETE", func() error { return rooted.RemoveAll("d/x") }},
		{"MKCOL", func() error { return rooted.Mkdir("d/new", 0o755) }},
		{"LOCK", func() error {
			f, err := rooted.OpenFile("d/new", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if f != nil {
				f.Close()
			}
			return err
		}},
		{"GET", func() error {
			f, err := rooted.Open("d/x")
			if f != nil {
				f.Close()
			}
			return err
		}},
		{"PROPFIND", func() error { _, err := rootReadDir(rooted, "d"); return err }},
		{"mode", func() error { return rootChmod(rooted, "d/x", 0o777) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.op(); err == nil {
				t.Fatal("escaped root after successful path check")
			}
		})
	}
	if _, ok := syncEntryOf(rooted, "d/x", "d/x", nil); ok {
		t.Error("sync read escaped")
	}
	b, err := os.ReadFile(filepath.Join(outside, "x"))
	if err != nil || string(b) != "outside" {
		t.Fatalf("outside changed: %q, %v", b, err)
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 1 {
		t.Fatalf("outside entries: %v, %v", entries, err)
	}
}

func TestSyncApplyRefusesParentSwappedDuringFetch(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	data := []byte("gateway")
	ch := syncChanges{Changed: []syncEntry{{Path: "d/x", SHA256: sha256Hex(data), Size: int64(len(data))}}}
	notes, done := syncApply(ch, root, map[string]syncEntry{}, func(string) []byte {
		if err := os.Rename(filepath.Join(root, "d"), filepath.Join(root, "old")); err != nil {
			t.Fatal(err)
		}
		target, err := filepath.Rel(root, outside)
		if err != nil {
			t.Fatal(err)
		}
		dirLink(t, target, filepath.Join(root, "d"))
		return data
	})
	if len(done) != 0 || !strings.Contains(strings.Join(notes, "\n"), "cannot write") {
		t.Fatalf("notes=%v done=%v", notes, done)
	}
	if _, err := os.Stat(filepath.Join(outside, "x")); !os.IsNotExist(err) {
		t.Fatalf("outside file: %v", err)
	}
}

func TestSyncPutRefusesParentSwappedBeforeWrite(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	h := newShareHub()
	key, ok := h.register("s1", "/c", "", false, root, "dev")
	if !ok {
		t.Fatal("register")
	}
	data := []byte("gateway")
	entry := syncEntry{Path: "d/x", SHA256: sha256Hex(data), Size: int64(len(data))}
	if _, _, ok := h.syncManifest(key, []syncEntry{entry}, true, "dev"); !ok {
		t.Fatal("manifest")
	}
	write := syncPutWrite
	t.Cleanup(func() { syncPutWrite = write })
	syncPutWrite = func(rooted *os.Root, p string, b []byte, m int64, x, k bool) error {
		if err := os.Rename(filepath.Join(root, "d"), filepath.Join(root, "old")); err != nil {
			t.Fatal(err)
		}
		dirLink(t, outside, filepath.Join(root, "d"))
		return write(rooted, p, b, m, x, k)
	}
	if status := h.syncPut(key, "d/x", data, "dev"); status != 500 {
		t.Fatalf("PUT: %d", status)
	}
	if _, err := os.Stat(filepath.Join(outside, "x")); !os.IsNotExist(err) {
		t.Fatalf("outside file: %v", err)
	}
}

func TestDavRelativeLinkInsideRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, "d", filepath.Join(root, "link"))
	if r := davHandle("PUT", "/s/link/x", nil, []byte("inside"), root, "/s"); r.Status != 201 {
		t.Fatalf("PUT: %d", r.Status)
	}
	if r := davHandle("GET", "/s/link/x", nil, nil, root, "/s"); r.Status != 200 || string(r.Body) != "inside" {
		t.Fatalf("GET: %+v", r)
	}
}

func TestDavRefusesAbsoluteLinkInsideRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	dirLink(t, filepath.Join(root, "d"), filepath.Join(root, "link"))
	if r := davHandle("PUT", "/s/link/x", nil, []byte("inside"), root, "/s"); r.Status < 400 {
		t.Fatalf("PUT: %d", r.Status)
	}
	if _, err := os.Stat(filepath.Join(root, "d", "x")); !os.IsNotExist(err) {
		t.Fatalf("unexpected file: %v", err)
	}
}
