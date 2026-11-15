package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

func TestSyncCopiesListDuringClientRequest(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s, _ := testServer(t, nil)
	s.scratchGate.RLock() // Client requests, including long polls, hold this lock.
	defer s.scratchGate.RUnlock()
	status, body := s.adminDo("GET", "/sync?device=local", nil)
	if status != 200 {
		t.Fatalf("list during client request: %d %v", status, body)
	}
	status, _ = s.adminDo("DELETE", "/sync/0000000000000000?device=local", nil)
	if status != 409 {
		t.Fatalf("remove during client request: %d", status)
	}
}

func TestSyncCopiesListDuringScratchMove(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s, _ := testServer(t, nil)
	s.scratchGate.Lock()
	defer s.scratchGate.Unlock()
	status, _ := s.adminDo("GET", "/sync?device=local", nil)
	if status != 503 {
		t.Fatalf("list during Scratch move: %d", status)
	}
}

func TestSyncCopiesCLI(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s, cfg, _ := liveServer(t, nil)
	id := "0123456789abcdef"
	copy := filepath.Join(s.conf().scratch("sync"), "local", id)
	if err := os.MkdirAll(copy, 0700); err != nil {
		t.Fatal(err)
	}
	run := func(args ...string) string {
		return stdout(t, func() { routerCommand(append(args, "--config", cfg), strings.NewReader("")) })
	}
	if out := run("sync", "list", "--device", "local"); out != id+"\n" {
		t.Fatalf("list: %q", out)
	}
	if out := run("sync", "remove", id, "--device", "local"); out != "synced copy removed\n" {
		t.Fatalf("remove: %q", out)
	}
	if out := run("sync", "list", "--device", "local"); out != "no synced copies\n" {
		t.Fatalf("empty list: %q", out)
	}
}
