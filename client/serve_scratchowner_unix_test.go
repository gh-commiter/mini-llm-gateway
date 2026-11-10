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
