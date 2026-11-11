//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Zaprto: neberljiv lastnik (cev, povezava) je tuj; mapa brez lastnika, ki jo drži drug proces, ni prevzeta; prevzem
// le prave mape tega uporabnika.
func TestScratchOwnerFailClosed(t *testing.T) {
	d := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(d, scratchOwnerFile), 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan string)
	go func() { done <- scratchState(d, "gw-me") }()
	select {
	case st := <-done:
		if st != scratchStale {
			t.Fatalf("fifo owner: %q", st)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("owner fifo blocks")
	}
	if st, _ := claimScratch(d, "gw-me"); st == "" {
		t.Fatal("claimed a folder with an unreadable owner")
	}
	held := filepath.Join(t.TempDir(), "held") // 0.5.23 brez lastnika, drug proces drži zaklep
	os.MkdirAll(held, 0o700)
	lockScratch(held)
	defer lockScratch(t.TempDir())
	if st := func() string { // drug »proces«: zaklep drži scratchLock tega testa
		st, _ := claimScratch(held, "gw-me")
		return st
	}(); st != scratchInUse || scratchOwner(held) != "" {
		t.Fatalf("held folder adopted: %q", st)
	}
	base := t.TempDir()
	real := filepath.Join(base, "real")
	writeOwner(t, real, "gw-gone")
	link := filepath.Join(base, "link")
	os.Symlink(real, link)
	if _, _, err := takeScratch(link, "", "gw-me", time.Now()); err == nil || !strings.Contains(err.Error(), "real folder") {
		t.Fatalf("link taken over: %v", err)
	}
	if o := scratchSiblings(filepath.Join(base, "gw-me"), "gw-me"); len(o) != 1 || o[0].Name != "real" {
		t.Fatalf("siblings: %v", o)
	}
}

// H1: minigw gw brez routerja (npr. admin.secret manjka), a prehod te nastavitve teče in drži zaklep Scratch: premik in
// prevzem 409, nič se ne premakne.
func TestScratchOfflineWhileRunning(t *testing.T) {
	cfg, cur := offlineScratchConfig(t)
	scratchFixture(t, cur)
	other := filepath.Join(filepath.Dir(cur), "gw-bbbbbbbbbbbb")
	writeOwner(t, other, "gw-bbbbbbbbbbbb")
	if err := lockScratch(cur); err != nil { // prehod teče
		t.Fatal(err)
	}
	defer lockScratch(t.TempDir())
	s, err := offlineServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	to := filepath.ToSlash(filepath.Join(nonTempDir(t), "mgw-test"))
	if st, obj := s.adminDo("POST", "/general/scratch/move", []byte(`{"to":"`+to+`"}`)); st != 409 {
		t.Fatalf("move: %d %v", st, obj)
	}
	if st, obj := s.adminDo("POST", "/general/scratch/take", []byte(`{"name":"gw-bbbbbbbbbbbb"}`)); st != 409 {
		t.Fatalf("take: %d %v", st, obj)
	}
	checkFixture(t, cur)
	if c, _ := loadServeConfig(cfg); c.Scratch != cur || scratchOwner(other) != "gw-bbbbbbbbbbbb" {
		t.Fatalf("changed: %q", c.Scratch)
	}
}

// H1 (b): takeScratch ne briše ali premakne trenutne mape, ki jo drži drug proces; lasten zaklep ne šteje.
func TestTakeScratchCurHeld(t *testing.T) {
	base := t.TempDir()
	old, cur := filepath.Join(base, "gw-bbbbbbbbbbbb"), filepath.Join(base, "gw-aaaaaaaaaaaa")
	writeOwner(t, old, "gw-bbbbbbbbbbbb")
	writeOwner(t, cur, "gw-aaaaaaaaaaaa")
	f, err := os.OpenFile(filepath.Join(cur, scratchLockFile), os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) // drug proces
	if _, _, err := takeScratch(old, cur, "gw-aaaaaaaaaaaa", time.Now()); err == nil || !strings.Contains(err.Error(), "running") {
		t.Fatalf("cur held: %v", err)
	}
	if scratchOwner(old) != "gw-bbbbbbbbbbbb" || scratchOwner(cur) != "gw-aaaaaaaaaaaa" {
		t.Fatal("folders changed")
	}
	f.Close()
	if err := lockScratch(cur); err != nil { // ta prehod sam
		t.Fatal(err)
	}
	defer lockScratch(t.TempDir())
	if dst, _, err := takeScratch(old, cur, "gw-aaaaaaaaaaaa", time.Now()); err != nil || dst != cur || scratchOwner(cur) != "gw-aaaaaaaaaaaa" {
		t.Fatalf("own lock: %q %v", dst, err)
	}
}

// Kopija deskriptorja zaklepa (kot jo ima otrok fork/exec do exec) ne sme zadržati zaklepa po premiku na drugo mapo
// (Swift: scratchOwnerClaimLockAndTakeOver).
func TestScratchLockReleasedDespiteCopy(t *testing.T) {
	a, other := t.TempDir(), t.TempDir()
	if err := lockScratch(a); err != nil {
		t.Fatal(err)
	}
	defer lockScratch(t.TempDir())
	scratchLock.Lock()
	cp, err := syscall.Dup(int(scratchLock.f.Fd()))
	scratchLock.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	defer syscall.Close(cp)
	if err := lockScratch(other); err != nil {
		t.Fatal(err)
	}
	if scratchLocked(a) {
		t.Fatal("old folder still locked after the lock moved")
	}
	if err := lockScratch(a); err != nil { // in znova ista mapa
		t.Fatalf("relock: %v", err)
	}
}
