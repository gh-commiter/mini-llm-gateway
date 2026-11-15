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

// Darwin must refuse mixed-case automount names before any filesystem probe.
func TestMixedCaseAutomountTargets(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix automount roots")
	}
	for _, target := range []string{"/NET", "/NET/minigw-invalid/x", "/network", "/nEtWoRk/minigw-invalid/x"} {
		if got := safeTarget(target, nil); got != (runtime.GOOS != "darwin") {
			t.Errorf("safeTarget(%q) = %v", target, got)
		}
		if runtime.GOOS != "darwin" {
			continue
		}
		root := t.TempDir()
		if err := os.Symlink(target, filepath.Join(root, "remote")); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("remote", filepath.Join(root, "chain")); err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{target, filepath.Join(root, "remote", "child"), filepath.Join(root, "chain", "child")} {
			if _, err := safeFinalPath(path, []string{root}); err == nil || !strings.Contains(err.Error(), linkRefusal) {
				t.Errorf("safeFinalPath(%q) = %v", path, err)
			}
		}
	}
}

func TestAutofsStopsBeforeChildren(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix mounts")
	}
	mount, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	old := targetAutofs
	defer func() { targetAutofs = old }()
	var probes []string
	targetAutofs = func(p string) (bool, error) {
		probes = append(probes, p)
		return p == mount, nil
	}
	if safeTarget(filepath.Join(mount, "unprobed", "child"), nil) {
		t.Fatal("accepted autofs target")
	}
	if len(probes) == 0 || probes[len(probes)-1] != mount {
		t.Fatalf("did not stop at mount: %v", probes)
	}
	alias := filepath.Join(t.TempDir(), "alias")
	dirLink(t, mount, alias)
	probes = nil
	if safeTarget(filepath.Join(alias, "unprobed", "child"), nil) {
		t.Fatal("accepted autofs target through a symlink ancestor")
	}
	if len(probes) == 0 || probes[len(probes)-1] != mount {
		t.Fatalf("did not stop at aliased mount: %v", probes)
	}
	for _, p := range probes {
		if strings.HasPrefix(p, mount+string(filepath.Separator)) {
			t.Fatalf("probed below the mount: %v", probes)
		}
	}
}

// A link planted in the share to a place below an autofs mount is refused, and nothing below the mount is probed.
func TestRootResolveRefusesAutofsLinkTarget(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix mounts")
	}
	mount, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	dirLink(t, filepath.Join(mount, "host", "x"), filepath.Join(root, "evil"))
	old := targetAutofs
	defer func() { targetAutofs = old }()
	var probes []string
	targetAutofs = func(p string) (bool, error) {
		probes = append(probes, p)
		return p == mount, nil
	}
	rt, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	if _, err := rootResolve(rt, "evil/f", false); err == nil || !strings.Contains(err.Error(), linkRefusal) {
		t.Fatalf("err = %v", err)
	}
	if len(probes) == 0 || probes[len(probes)-1] != mount {
		t.Fatalf("did not stop at the mount: %v", probes)
	}
	for _, p := range probes {
		if strings.HasPrefix(p, mount+string(filepath.Separator)) {
			t.Fatalf("probed below the mount: %v", probes)
		}
	}
}

func TestLinkDotDotResolvesBeforeMoveGuard(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a/deep", "a/x", "x"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	dirLink(t, "a/deep", filepath.Join(root, "L2"))
	dirLink(t, "L2/../x", filepath.Join(root, "L1"))
	dirLink(t, root+string(filepath.Separator)+"L2/../x", filepath.Join(root, "absolute"))
	rt, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer rt.Close()
	if got, err := rootResolve(rt, "L1/new", false); err != nil || got != filepath.FromSlash("a/x/new") {
		t.Fatalf("resolve = %q, %v", got, err)
	}
	if got, err := safeFinalPath(filepath.Join(root, "L1"), []string{root}); err != nil || got != realTarget(filepath.Join(root, "a/x"), []string{root}) {
		t.Fatalf("absolute resolve = %q, %v", got, err)
	}
	if got, err := rootResolve(rt, "absolute/new", false); err != nil || got != filepath.FromSlash("a/x/new") {
		t.Fatalf("absolute root resolve = %q, %v", got, err)
	}
	if got := realTarget(root+"/L2/../x/new", []string{root}); got != realTarget(filepath.Join(root, "a/x"), []string{root})+string(filepath.Separator)+"new" {
		t.Fatalf("missing suffix resolved to %q", got)
	}
	r := davHandle("MOVE", "/s/a/x", map[string]string{"destination": "/s/L1/new"}, nil, root, "/s")
	if r.Status != 403 {
		t.Fatalf("self-nesting MOVE = %d", r.Status)
	}
	if _, err := os.Stat(filepath.Join(root, "a/x")); err != nil {
		t.Fatal(err)
	}
}

func TestSplitComponentsKeepsDotDot(t *testing.T) {
	got := splitComponents(`share\L2/../x//y`, `\/`)
	if strings.Join(got, "|") != "share|L2|..|x|y" {
		t.Fatalf("windows split = %v", got)
	}
	if got := splitComponents(`a\b/../c`, "/"); strings.Join(got, "|") != `a\b|..|c` {
		t.Fatalf("unix split = %v", got)
	}
}
