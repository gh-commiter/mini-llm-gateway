package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

func TestRootThroughSystemLink(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS system links")
	}
	canonical, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(canonical, "/private/var/") && !strings.HasPrefix(canonical, "/private/tmp/") {
		t.Skip("temporary directory is outside the system-linked directories")
	}
	root := strings.TrimPrefix(canonical, "/private")
	system := "/" + strings.Split(strings.TrimPrefix(root, "/"), "/")[0]
	if st, err := os.Lstat(system); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s must be a system link", system)
	}
	if got, err := safeFinalPath(root, []string{root}); err != nil || got != canonical {
		t.Fatalf("safeFinalPath = %q, %v; want %q", got, err, canonical)
	}
	for _, name := range []string{"new.txt", ".git/hooks/new"} {
		full, ok := syncResolve(name, root)
		if !ok {
			t.Fatalf("syncResolve refused %s", name)
		}
		want := name != "new.txt"
		for _, base := range []string{root, canonical} {
			if got := protectedCanonical(full, base); got != want {
				t.Errorf("protectedCanonical(%s, %s) = %v, want %v", full, base, got, want)
			}
		}
	}
	if _, _, ok := davResolve("/new.txt", "", root); !ok {
		t.Fatal("DAV refused root through system link")
	}
}
