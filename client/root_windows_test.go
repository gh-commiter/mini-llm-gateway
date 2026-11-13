package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A junction always has an absolute target; one that stays inside the share works (pnpm, npm link).
func TestDavFollowsJunctionInsideRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "d"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "d", "old"), []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	junction(t, filepath.Join(root, "d"), filepath.Join(root, "link"))
	if r := davHandle("PUT", "/s/link/x", nil, []byte("inside"), root, "/s"); r.Status != 201 {
		t.Fatalf("PUT: %+v", r)
	}
	if b, err := os.ReadFile(filepath.Join(root, "d", "x")); err != nil || string(b) != "inside" {
		t.Fatalf("file: %q %v", b, err)
	}
	if r := davHandle("GET", "/s/link/old", nil, nil, root, "/s"); r.Status != 200 || string(r.Body) != "old" {
		t.Fatalf("GET: %+v", r)
	}
	if r := davHandle("MKCOL", "/s/link/sub", nil, nil, root, "/s"); r.Status != 201 {
		t.Fatalf("MKCOL: %+v", r)
	}
	if r := davHandle("PROPFIND", "/s/link", map[string]string{"depth": "1"}, nil, root, "/s"); r.Status != 207 || !strings.Contains(string(r.Body), "/s/link/old") {
		t.Fatalf("PROPFIND: %+v", r)
	}
	data := []byte("sync")
	notes, done := syncApply(syncChanges{Changed: []syncEntry{{Path: "link/new", SHA256: sha256Hex(data), Size: int64(len(data))}}}, root,
		map[string]syncEntry{}, func(string) []byte { return data })
	if len(notes) != 0 || len(done) != 1 {
		t.Fatalf("notes=%v done=%v", notes, done)
	}
	if b, err := os.ReadFile(filepath.Join(root, "d", "new")); err != nil || string(b) != "sync" {
		t.Fatalf("sync file: %q %v", b, err)
	}
	// DELETE removes the junction, not its target.
	if r := davHandle("DELETE", "/s/link", nil, nil, root, "/s"); r.Status != 204 {
		t.Fatalf("DELETE: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(root, "d", "old")); err != nil {
		t.Fatalf("target changed: %v", err)
	}
}

// The target is spelled in upper case: still the same share.
func TestDavFollowsJunctionInsideRootOtherSpelling(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "d"), 0700); err != nil {
		t.Fatal(err)
	}
	junction(t, strings.ToUpper(filepath.Join(root, "d")), filepath.Join(root, "link"))
	if r := davHandle("PUT", "/s/link/x", nil, []byte("inside"), root, "/s"); r.Status != 201 {
		t.Fatalf("PUT: %+v", r)
	}
	if _, err := os.Stat(filepath.Join(root, "d", "x")); err != nil {
		t.Fatal(err)
	}
}

func TestDavRefusesJunctionOutsideRoot(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	junction(t, outside, filepath.Join(root, "link"))
	for _, method := range []string{"PUT", "GET", "MKCOL", "PROPFIND"} {
		r := davHandle(method, "/s/link/secret", nil, []byte("bad"), root, "/s")
		if r.Status < 400 {
			t.Fatalf("%s: %+v", method, r)
		}
	}
	if _, err := os.Stat(filepath.Join(outside, "x")); !os.IsNotExist(err) {
		t.Fatalf("junction target changed: %v", err)
	}
	data := []byte("gateway")
	notes, _ := syncApply(syncChanges{Changed: []syncEntry{{Path: "link/new", SHA256: sha256Hex(data)}}}, root,
		map[string]syncEntry{}, func(string) []byte { return data })
	if len(notes) == 0 {
		t.Fatal("sync wrote through a junction that leaves the share")
	}
	if _, err := os.Stat(filepath.Join(outside, "new")); !os.IsNotExist(err) {
		t.Fatalf("outside changed: %v", err)
	}
}

func TestDavRefusesJunctionIntoProtectedFolder(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, ".claude"), 0700); err != nil {
		t.Fatal(err)
	}
	junction(t, filepath.Join(root, ".claude"), filepath.Join(root, "link"))
	for _, c := range []struct{ method, path string }{
		{"PUT", "/s/link/settings.json"}, {"MKCOL", "/s/link/hooks"}, {"PUT", "/s/link/commands/x"},
	} {
		if r := davHandle(c.method, c.path, nil, []byte("{}"), root, "/s"); r.Status < 400 {
			t.Fatalf("%s %s: %+v", c.method, c.path, r)
		}
	}
	data := []byte("{}")
	notes, _ := syncApply(syncChanges{Changed: []syncEntry{{Path: "link/settings.json", SHA256: sha256Hex(data)}}}, root,
		map[string]syncEntry{}, func(string) []byte { return data })
	if len(notes) == 0 {
		t.Fatal("sync wrote through a junction into .claude")
	}
	if _, err := os.Stat(filepath.Join(root, ".claude", "settings.json")); !os.IsNotExist(err) {
		t.Fatalf("settings created: %v", err)
	}
}

func TestDavMovesReadOnlyFileAcrossParents(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"a", "b"} {
		if err := os.Mkdir(filepath.Join(root, d), 0700); err != nil {
			t.Fatal(err)
		}
	}
	src, dst := filepath.Join(root, "a", "file"), filepath.Join(root, "b", "x")
	if err := os.WriteFile(src, []byte("safe"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(src, 0400); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(src, 0600); os.Chmod(dst, 0600) })
	r := davHandle("MOVE", "/s/a/file", map[string]string{"destination": "/s/b/x"}, nil, root, "/s")
	if r.Status != 201 {
		t.Fatalf("MOVE: %+v", r)
	}
	if b, err := os.ReadFile(dst); err != nil || string(b) != "safe" {
		t.Fatalf("destination: %q %v", b, err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("source: %v", err)
	}
}
