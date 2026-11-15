package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// L2: a target on the root's own volume is still resolved (a link made with another name of the share).
func TestRealTargetResolvesOwnVolume(t *testing.T) {
	calls := 0
	old := finalPathFn
	finalPathFn = func(p string) (string, error) { calls++; return old(p) }
	defer func() { finalPathFn = old }()
	dir := t.TempDir()
	realTarget(filepath.Join(dir, "missing", "x"), []string{dir})
	if calls == 0 {
		t.Fatal("a target on the root's own volume is not resolved")
	}
}

// Request paths must reject remote link chains before finalPath can open a target.
func TestRemoteLinkResolution(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", t.TempDir())
	target := "/net/minigw-invalid/share"
	if runtime.GOOS == "windows" {
		target = `\\minigw-invalid\share\x`
	}
	if err := os.Symlink(target, filepath.Join(root, "remote")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := os.Symlink("remote", filepath.Join(root, "chain")); err != nil {
		t.Fatal(err)
	}
	old := finalPathFn
	calls := 0
	finalPathFn = func(p string) (string, error) { calls++; return old(p) }
	defer func() { finalPathFn = old }()
	for _, name := range []string{"remote", "remote/child", "chain/child"} {
		full := filepath.Join(root, filepath.FromSlash(name))
		if _, err := safeFinalPath(full, []string{root}); err == nil {
			t.Errorf("safeFinalPath accepted %s", name)
		}
		if _, _, ok := davResolve("/"+name, "", root); ok {
			t.Errorf("DAV accepted %s", name)
		}
		if _, ok := syncResolve(name, root); ok {
			t.Errorf("sync accepted %s", name)
		}
		if !protectedCanonical(full, root) {
			t.Errorf("protectedCanonical accepted %s", name)
		}
		if got := realTarget(full, []string{root}); got != full {
			t.Errorf("realTarget changed refused path: %s", got)
		}
	}
	if calls != 0 {
		t.Fatalf("finalPath called %d times for remote links", calls)
	}
}

func TestAutomountTargetNeverResolved(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix automount roots")
	}
	t.Setenv("HOME", t.TempDir())
	old := finalPathFn
	calls := 0
	finalPathFn = func(p string) (string, error) { calls++; return old(p) }
	defer func() { finalPathFn = old }()
	for _, target := range []string{"/net", "/net/minigw-invalid/x", "/Network", "/Network/minigw-invalid/x"} {
		if got := realTarget(target, []string{t.TempDir()}); got != target {
			t.Errorf("resolved %s to %s", target, got)
		}
	}
	if calls != 0 {
		t.Fatalf("finalPath called %d times for automount roots", calls)
	}
}
