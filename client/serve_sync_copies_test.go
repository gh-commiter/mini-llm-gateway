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
	copies, status, why := syncCopies(root, device, "", nil)
	if status != 200 || why != "" || len(copies) != maxCopies {
		t.Fatalf("list: %v %d %s", copies, status, why)
	}
	_, status, why = syncCopies(root, device, copies[0], nil)
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
		if _, status, _ := syncCopies(root, device, id, nil); status != 400 {
			t.Fatalf("accepted %q", id)
		}
	}
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
		t.Skip("symlinks unavailable")
	}
	if _, status, _ := syncCopies(root, "linked", "", nil); status != 400 {
		t.Fatal("accepted linked owner")
	}
}

func TestSyncCopiesAdminBusy(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s, _ := testServer(t, nil)
	mk := func(id string) string {
		p := filepath.Join(s.conf().scratch("sync"), "local", id)
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
		return p
	}
	used, other := mk("0000000000000001"), mk("0000000000000002")
	s.hub.register("session", "/folder", "key", false, used, "local")
	s.agentsRunning.Add(1) // an agent running anywhere does not block a copy it does not use
	defer s.agentsRunning.Add(-1)
	if status, body := s.adminDo("DELETE", "/sync/0000000000000002?device=local", nil); status != 200 {
		t.Fatalf("unrelated session blocked the removal: %d %v", status, body)
	}
	if _, err := os.Stat(other); !os.IsNotExist(err) {
		t.Fatal("copy not removed")
	}
	status, body := s.adminDo("DELETE", "/sync/0000000000000001?device=local", nil)
	if status != 409 {
		t.Fatalf("copy of an active session: %d", status)
	}
	want := srcFile(t, "testdata/sync_copy_in_use.txt")
	if msg := fmt.Sprint(body); !strings.Contains(msg, string(want)) {
		t.Fatalf("message %q lacks the shared text %q", msg, want)
	}
	if _, err := os.Stat(used); err != nil {
		t.Fatal("copy of an active session was removed")
	}
	s.hub.unregister(s.hub.bySession["session"].key, "local")
	if status, _ := s.adminDo("DELETE", "/sync/0000000000000001?device=local", nil); status != 200 {
		t.Fatalf("after the session ended: %d", status)
	}
	if status, _ := s.adminDo("GET", "/sync?device=..", nil); status != 400 {
		t.Fatalf("invalid device: %d", status)
	}
}

func TestSyncCopiesRegistrationAndRemovalDoNotInterleave(t *testing.T) {
	h := newShareHub()
	dir := filepath.Join(t.TempDir(), "c")
	ran := false
	if inUse, err := h.ifCopyFree(dir, func() error { ran = true; return nil }); inUse || err != nil || !ran {
		t.Fatalf("free copy: %v %v %v", inUse, err, ran)
	}
	h.register("s", "/f", "k", false, dir, "local")
	ran = false
	if inUse, _ := h.ifCopyFree(dir+"/", func() error { ran = true; return nil }); !inUse || ran {
		t.Fatal("registered copy must be in use and the step must not run")
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
	if status != 404 { // the request of another client does not block the removal (this copy does not exist)
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
	if status, _ = s.adminDo("DELETE", "/sync/0000000000000000?device=local", nil); status != 503 {
		t.Fatalf("remove during Scratch move: %d", status)
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

func TestSyncLeftoverOfACrashedRemoval(t *testing.T) {
	root := t.TempDir()
	owner := filepath.Join(root, "local")
	left := filepath.Join(owner, "0123456789abcdef.removing-1700000000000000000")
	leftRO := filepath.Join(owner, "fedcba9876543210-ro.removing-5")
	decoy := filepath.Join(owner, "0123456789abcdef.removing-x") // not the exact pattern: stays
	for _, d := range []string{left, leftRO, decoy} {
		if err := os.MkdirAll(filepath.Join(d, "sub"), 0700); err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(d, "sub", "f"), []byte("x"), 0600)
	}
	touchUsed(filepath.Join(owner, "0123456789abcdef")) // marker of the copy that is gone
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "keep"), []byte("k"), 0600)
	linkName := filepath.Join(owner, "aaaaaaaaaaaaaaaa.removing-9")
	if err := os.Symlink(outside, linkName); err != nil {
		t.Skip("symlinks unavailable")
	}
	for i := 0; i < maxCopies-1; i++ {
		os.MkdirAll(filepath.Join(owner, fmt.Sprintf("%016x", i)), 0700)
	}
	if tooManyCopies(filepath.Join(owner, "ffffffffffffffff")) {
		t.Fatal("leftovers count towards the copy limit")
	}
	if copies, status, _ := syncCopies(root, "local", "", nil); status != 200 || len(copies) != maxCopies-1 {
		t.Fatalf("list shows leftovers: %v %d", copies, status)
	}
	cleanLeftovers(root, "local")
	for _, d := range []string{left, leftRO} {
		if _, err := os.Lstat(d); !os.IsNotExist(err) {
			t.Fatalf("leftover %s remains", d)
		}
	}
	if _, err := os.Lstat(filepath.Join(owner, "0123456789abcdef.used")); !os.IsNotExist(err) {
		t.Fatal("marker of the gone copy remains")
	}
	if _, err := os.Lstat(decoy); err != nil {
		t.Fatal("a folder that does not match the exact pattern was removed")
	}
	if _, err := os.Lstat(linkName); err != nil {
		t.Fatal("a link was touched")
	}
	if _, err := os.Stat(filepath.Join(outside, "keep")); err != nil {
		t.Fatal("cleanup followed a link")
	}
}

func TestSyncLeftoverNameCannotBeACopyID(t *testing.T) {
	for _, n := range []string{"0123456789abcdef.removing-1", "0123456789abcdef-ro.removing-1"} {
		if validSyncCopyID(n) || !removingName.MatchString(n) {
			t.Fatalf("%q must be a leftover name and no copy ID", n)
		}
	}
	for _, id := range []string{"0123456789abcdef", "0123456789abcdef-ro"} {
		if removingName.MatchString(id) {
			t.Fatalf("copy ID %q matches the leftover pattern", id)
		}
	}
}

// Astra finding 2: an operation of a session that was unregistered meanwhile still works in the copy; the removal must wait for it.
// Controlled interleaving (channels, no sleeps): syncPut is held inside its file write while the session is unregistered.
func TestSyncCopyRemovalWaitsForAnOperationOfAnUnregisteredSession(t *testing.T) {
	h := newShareHub()
	copyDir := filepath.Join(t.TempDir(), "dev", "0123456789abcdef")
	key, _ := h.register("s", "/c", "", false, copyDir, "dev")
	data := []byte("hello")
	if _, _, ok := h.syncManifest(key, []syncEntry{{Path: "a.txt", Size: int64(len(data)), SHA256: sha256Hex(data)}}, true, "dev"); !ok {
		t.Fatal("manifest")
	}
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan int)
	orig := syncPutWrite
	t.Cleanup(func() { syncPutWrite = orig })
	syncPutWrite = func(rooted *os.Root, p string, b []byte, m int64, x, k bool) error {
		close(entered)
		<-release
		return orig(rooted, p, b, m, x, k)
	}
	go func() { done <- h.syncPut(key, "a.txt", data, "dev") }()
	<-entered
	h.unregister(key, "dev")
	ran := false
	if inUse, _ := h.ifCopyFree(copyDir, func() error { ran = true; return nil }); !inUse || ran {
		t.Fatalf("removal ran under a request still writing into the copy (inUse %v, ran %v)", inUse, ran)
	}
	close(release)
	<-done
	if inUse, err := h.ifCopyFree(copyDir, func() error { return nil }); inUse || err != nil {
		t.Fatalf("copy still held after the operation ended: %v %v", inUse, err)
	}
}

func TestSyncCopyOperationCountEndsOnEveryPath(t *testing.T) {
	h := newShareHub()
	copyDir := filepath.Join(t.TempDir(), "dev", "0123456789abcdef")
	key, _ := h.register("s", "/c", "", false, copyDir, "dev")
	h.syncManifest(key, nil, true, "dev")
	h.syncPut(key, "missing.txt", nil, "dev") // 403: not listed
	h.syncChanges(key, "dev")
	h.syncGet(key, "x", "dev")
	h.unregister(key, "dev")
	if inUse, _ := h.ifCopyFree(copyDir, func() error { return nil }); inUse {
		t.Fatal("an operation did not release its hold")
	}
}
