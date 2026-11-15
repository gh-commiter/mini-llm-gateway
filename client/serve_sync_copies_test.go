package main

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestSyncCopiesManagement(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	device := "local"
	owner := filepath.Join(root, device)
	for i := 0; i < maxCopies; i++ {
		p := filepath.Join(owner, fmt.Sprintf("%016x", i))
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
		touchUsed(p)
	}
	next := filepath.Join(owner, "ffffffffffffffff")
	if !tooManyCopies(next) {
		t.Fatal("expected quota")
	}
	copies, status, why := syncCopies(root, device, "")
	if status != 200 || why != "" || len(copies) != maxCopies {
		t.Fatalf("list: %v %d %s", copies, status, why)
	}
	_, status, why = syncCopies(root, device, copies[0])
	if status != 200 || why != "" {
		t.Fatalf("remove: %d %s", status, why)
	}
	if tooManyCopies(next) {
		t.Fatal("removal did not reclaim quota")
	}
	if _, err := os.Stat(filepath.Join(owner, copies[0]) + ".used"); !os.IsNotExist(err) {
		t.Fatal("marker remains")
	}
	for _, id := range []string{"../outside", "0000000000000000/..", "bad"} {
		if _, status, _ := syncCopies(root, device, id); status != 400 {
			t.Fatalf("accepted %q", id)
		}
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Skip("symlinks unavailable")
	}
	if _, status, _ := syncCopies(root, "linked", ""); status != 400 {
		t.Fatal("accepted linked owner")
	}
}

func TestSyncCopiesAdminBusy(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s, _ := testServer(t, nil)
	s.hub.register("session", "/folder", "key", false, "", "local")
	status, _ := s.adminDo("DELETE", "/sync/0000000000000000?device=local", nil)
	if status != 409 {
		t.Fatalf("active session: %d", status)
	}
	status, _ = s.adminDo("GET", "/sync?device=..", nil)
	if status != 400 {
		t.Fatalf("invalid device: %d", status)
	}
}

func TestSyncCopiesAdminRoutes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s, _ := testServer(t, nil)
	id := "0123456789abcdef-ro"
	copy := filepath.Join(s.conf().scratch("sync"), "local", id)
	if err := os.MkdirAll(copy, 0700); err != nil {
		t.Fatal(err)
	}
	status, body := s.adminDo("GET", "/sync?device=local", nil)
	if status != 200 {
		t.Fatalf("list: %d %v", status, body)
	}
	copies := body.(map[string]any)["copies"].([]string)
	if len(copies) != 1 || copies[0] != id {
		t.Fatalf("list: %v", copies)
	}
	status, body = s.adminDo("DELETE", "/sync/"+id+"?device=local", nil)
	if status != 200 {
		t.Fatalf("remove: %d %v", status, body)
	}
	status, _ = s.adminDo("DELETE", "/sync/"+id+"?device=local", nil)
	if status != 404 {
		t.Fatalf("missing: %d", status)
	}
}
