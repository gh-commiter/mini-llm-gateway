package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDavRefusesJunctionInsideRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "d"), 0700); err != nil {
		t.Fatal(err)
	}
	junction(t, filepath.Join(root, "d"), filepath.Join(root, "link"))
	for _, method := range []string{"PUT", "GET", "MKCOL"} {
		r := davHandle(method, "/s/link/x", nil, []byte("bad"), root, "/s")
		if r.Status < 400 || !strings.Contains(string(r.Body), linkRefusal) {
			t.Fatalf("%s: %+v", method, r)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "d", "x")); !os.IsNotExist(err) {
		t.Fatalf("junction target changed: %v", err)
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
