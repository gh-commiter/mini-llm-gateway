package main

import (
	"bytes"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// startFrom: a gateway start from the config file (as minigw serve does); returns the server and what it logged.
func startFrom(t *testing.T, path string) (*server, string) {
	t.Helper()
	c, err := loadServeConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	old := logOut
	logOut = &buf
	defer func() { logOut = old }()
	s, err := newServer(c, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(releaseScratch)
	return s, buf.String()
}

// M2: the record of an allowed folder is saved with it and enforced at every start.
func TestAllowedFolderRecordAcrossRestart(t *testing.T) {
	base := realOf(t, t.TempDir())
	folder, other := filepath.Join(base, "folder"), filepath.Join(base, "other")
	for _, d := range []string{folder, other} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	s, _ := testServer(t, func(c *serveConfig) { c.AllowedFolders = []string{folder} }) // an old config: no records
	path := s.cfgPath
	releaseScratch()
	file, _ := loadServeConfig(path)
	if file.AllowedRecords[folder].Real != folder {
		t.Fatalf("first start did not record: %+v", file.AllowedRecords)
	}
	rec := file.AllowedRecords[folder]

	// An unchanged folder is accepted after a restart, and nothing is recorded again.
	s, logs := startFrom(t, path)
	if strings.Contains(logs, "recorded") || !slices.Equal(s.agentFolders(s.conf(), base), []string{folder}) {
		t.Fatalf("unchanged folder: %q %v", logs, s.agentFolders(s.conf(), base))
	}

	// Retargeted while the gateway is stopped: refused, name and record untouched, logged once.
	if err := os.Remove(folder); err != nil {
		t.Fatal(err)
	}
	dirLink(t, other, folder)
	s, logs = startFrom(t, path)
	if got := s.agentFolders(s.conf(), base); len(got) != 0 {
		t.Fatalf("retargeted folder accepted: %v", got)
	}
	if strings.Count(logs, "is not used: its real path changed to ") != 1 {
		t.Fatalf("log: %q", logs)
	}
	if again, _ := loadServeConfig(path); !slices.Equal(again.AllowedFolders, []string{folder}) || again.AllowedRecords[folder] != rec {
		t.Fatalf("start rewrote the config: %+v", again)
	}
	if got := s.claudeInfo().Refused; !slices.Equal(got, []string{folder}) {
		t.Fatalf("not listed as refused: %v", got)
	}

	// Remove and add again: accepted with the new target.
	patch := func(folders string) {
		t.Helper()
		b, _ := json.Marshal(map[string]string{"folders": folders})
		if st, out := s.adminDo("PATCH", "/agents/claude", b); st != 200 {
			t.Fatalf("PATCH folders %q: %d %v", folders, st, out)
		}
	}
	patch("")
	patch(folder)
	if got := s.agentFolders(s.conf(), base); len(got) != 1 {
		t.Fatalf("re-added folder refused: %v", got)
	}
	s, logs = startFrom(t, path)
	if got := s.agentFolders(s.conf(), base); len(got) != 1 || strings.Contains(logs, "is not used") {
		t.Fatalf("re-added folder after restart: %v %q", got, logs)
	}
}

// M2: a folder replaced by another one at the same path (same real path, new identity) is refused at start.
func TestAllowedFolderIdentityChangedAtStart(t *testing.T) {
	base := realOf(t, t.TempDir())
	folder := filepath.Join(base, "folder")
	if err := os.Mkdir(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	s, _ := testServer(t, func(c *serveConfig) { c.AllowedFolders = []string{folder} })
	releaseScratch()
	if err := os.Rename(folder, folder+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	s, logs := startFrom(t, s.cfgPath)
	if got := s.agentFolders(s.conf(), base); len(got) != 0 || !strings.Contains(logs, "its identity changed") {
		t.Fatalf("replaced folder: %v %q", got, logs)
	}
}

// R1: if saving the records fails at start, a later admin PATCH must not record the folder from its current target.
func TestFailedRecordSaveIsNotReRecordedByPatch(t *testing.T) {
	if os.Getuid() == 0 || runtime.GOOS == "windows" {
		t.Skip("needs a config folder that cannot be written")
	}
	base := realOf(t, t.TempDir())
	folder, other := filepath.Join(base, "folder"), filepath.Join(base, "other")
	for _, d := range []string{folder, other} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	k, _ := ecdh.X25519().GenerateKey(rand.Reader)
	cfgDir := t.TempDir()
	path := filepath.Join(cfgDir, "serve.json")
	if err := writeServeConfig(path, serveConfig{Token: "tok", Key: base64url(k.Bytes()), Tools: defaultTools, PermissionMode: "dontAsk", Scratch: t.TempDir(),
		AllowedFolders: []string{folder}}); err != nil { // an old config: no records
		t.Fatal(err)
	}
	if _, err := adminSecret(cfgDir); err != nil { // created before the folder becomes read-only
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(cfgDir, 0o700) })
	if err := os.Chmod(cfgDir, 0o500); err != nil {
		t.Fatal(err)
	}
	s, _ := startFrom(t, path) // the save fails; the live config has the record
	if err := os.Chmod(cfgDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if file, _ := loadServeConfig(path); len(file.AllowedRecords) != 0 {
		t.Fatalf("save did not fail: %+v", file.AllowedRecords)
	}
	rec := s.cfg.AllowedRecords[folder]
	if !samePath(rec.Real, folder) {
		t.Fatalf("no live record: %+v", s.cfg.AllowedRecords)
	}
	if err := os.Remove(folder); err != nil {
		t.Fatal(err)
	}
	dirLink(t, other, folder)
	if st, out := s.adminDo("PATCH", "/agents/claude", []byte(`{"tools":"Read,Grep"}`)); st != 200 {
		t.Fatalf("PATCH: %d %v", st, out)
	}
	if file, _ := loadServeConfig(path); file.AllowedRecords[folder] != rec {
		t.Fatalf("PATCH re-recorded the folder: %+v want %+v", file.AllowedRecords[folder], rec)
	}
	if got := s.agentFolders(s.conf(), base); len(got) != 0 {
		t.Fatalf("retargeted folder trusted: %v", got)
	}
}

// A1: the persisted identity is enforced on every run, not only at start: a folder swapped for another one at the same
// path is refused (and listed as refused) until the original is back; the same for a folder that was missing at start.
func TestAllowedFolderIdentityEnforcedAtRunTime(t *testing.T) {
	base := realOf(t, t.TempDir())
	folder := filepath.Join(base, "folder")
	if err := os.Mkdir(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	s, _ := testServer(t, func(c *serveConfig) { c.AllowedFolders = []string{folder} })
	path := s.cfgPath
	releaseScratch()
	swap := func(away string) { // the folder goes away, another one takes its place
		t.Helper()
		if err := os.Rename(folder, away); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(folder, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	back := func(away string) { // the replacement goes, the original returns
		t.Helper()
		if err := os.Remove(folder); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(away, folder); err != nil {
			t.Fatal(err)
		}
	}

	s, _ = startFrom(t, path)
	if got := s.agentFolders(s.conf(), base); len(got) != 1 {
		t.Fatalf("original folder refused: %v", got)
	}
	swap(folder + ".old")
	if got := s.agentFolders(s.conf(), base); len(got) != 0 {
		t.Fatalf("replaced folder accepted after start: %v", got)
	}
	if got := s.claudeInfo().Refused; !slices.Equal(got, []string{folder}) {
		t.Fatalf("not listed as refused: %v", got)
	}
	back(folder + ".old")
	if got := s.agentFolders(s.conf(), base); len(got) != 1 || len(s.claudeInfo().Refused) != 0 {
		t.Fatalf("original folder not accepted again: %v %v", got, s.claudeInfo().Refused)
	}

	// Missing at start, another folder appears, then the original returns.
	if err := os.Rename(folder, folder+".old"); err != nil {
		t.Fatal(err)
	}
	s, _ = startFrom(t, path)
	if got := s.agentFolders(s.conf(), base); len(got) != 0 {
		t.Fatalf("missing folder accepted: %v", got)
	}
	if err := os.Mkdir(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	if got := s.agentFolders(s.conf(), base); len(got) != 0 {
		t.Fatalf("a different folder accepted for a folder that was missing at start: %v", got)
	}
	back(folder + ".old")
	if got := s.agentFolders(s.conf(), base); len(got) != 1 {
		t.Fatalf("original folder not accepted again: %v", got)
	}
}

// A5: the record migration waits for another writer of serve.json and then builds on its result (no lost update).
func TestFolderRecordSaveDoesNotLoseConcurrentChange(t *testing.T) {
	base := realOf(t, t.TempDir())
	folder := filepath.Join(base, "folder")
	if err := os.Mkdir(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	s, _ := testServer(t, func(c *serveConfig) { c.AllowedFolders = []string{folder} })
	releaseScratch()
	path := s.cfgPath
	file, _ := loadServeConfig(path)
	delete(file.AllowedRecords, folder) // an old config: the migration still has to save the record
	if err := writeServeConfig(path, file); err != nil {
		t.Fatal(err)
	}
	unlock := lockServeConfig(path) // another process is in the middle of its own read-modify-write
	done := make(chan error)
	go func() {
		done <- saveFolderRecords(path, map[string]folderRecord{folder: {Real: folder}}, []string{folder})
	}()
	select {
	case err := <-done:
		t.Fatalf("migration did not wait for the other writer: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	file.Port = 4321 // the other writer's change, saved after the migration already looked at the file
	if err := writeServeConfig(path, file); err != nil {
		t.Fatal(err)
	}
	unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got, _ := loadServeConfig(path); got.Port != 4321 || got.AllowedRecords[folder].Real != folder {
		t.Fatalf("lost update: port %d, records %+v", got.Port, got.AllowedRecords)
	}
}
