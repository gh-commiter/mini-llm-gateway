//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAllowedFolderPermissionErrorDoesNotMarkMissing(t *testing.T) {
	base := realOf(t, t.TempDir())
	parent := filepath.Join(base, "parent")
	folder := filepath.Join(parent, "child")
	if err := os.MkdirAll(folder, 0700); err != nil {
		t.Fatal(err)
	}
	s, _ := testServer(t, func(c *serveConfig) { c.AllowedFolders = []string{folder} })
	if err := os.Chmod(parent, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(parent, 0700)
	if _, err := os.Stat(folder); err == nil {
		t.Skip("process can bypass directory permissions")
	}
	if got := s.agentFolders(s.conf(), base); len(got) != 0 {
		t.Fatalf("unreadable folder accepted: %v", got)
	}
	if _, marked := s.wasMissing.Load(folder); marked {
		t.Fatal("permission error marked missing")
	}
	if err := os.Chmod(parent, 0700); err != nil {
		t.Fatal(err)
	}
	if got := s.agentFolders(s.conf(), base); len(got) != 1 {
		t.Fatalf("folder did not recover: %v", got)
	}
}

func TestAllowedFolderForeignOwner(t *testing.T) {
	folder := realOf(t, "/usr/bin")
	if os.Getuid() == 0 {
		folder = realOf(t, t.TempDir())
	}
	s, _ := testServer(t, func(c *serveConfig) { c.AllowedFolders = []string{folder} })
	if os.Getuid() == 0 {
		if err := os.Chown(folder, 1, -1); err != nil {
			t.Fatal(err)
		}
		defer os.Chown(folder, 0, -1)
	} else if ownedByMe(folder) {
		t.Fatal("test needs a system folder owned by another user")
	}
	if got := s.agentFolders(s.conf(), t.TempDir()); len(got) != 0 {
		t.Fatalf("foreign folder accepted: %v", got)
	}
}
