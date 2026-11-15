package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// L2: a link to a UNC or device path must not make the process connect to that host.
func TestRealTargetNeverResolvesUNC(t *testing.T) {
	calls := 0
	old := finalPathFn
	finalPathFn = func(p string) (string, error) { calls++; return old(p) }
	defer func() { finalPathFn = old }()
	bases := []string{`C:\share`}
	for _, target := range []string{`\\attacker\share\x`, `\\?\UNC\attacker\share\x`, `D:\other`} {
		if got := realTarget(trimFinalPrefix(target), bases); got != trimFinalPrefix(target) {
			t.Fatalf("%s resolved to %s", target, got)
		}
	}
	if calls != 0 {
		t.Fatalf("finalPath called %d times for targets off the root's volume", calls)
	}
	realTarget(`C:\Windows`, bases)
	if calls == 0 {
		t.Fatal("a target on the root's own drive is not resolved")
	}
}

// A same-drive junction can hide a remote link in a target outside the lexical share.
func TestSameDriveJunctionToRemote(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root, outside := t.TempDir(), t.TempDir()
	if err := os.Symlink(`\\minigw-invalid\share\x`, filepath.Join(outside, "remote")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	junction(t, outside, filepath.Join(root, "alias"))
	old := finalPathFn
	calls := 0
	finalPathFn = func(p string) (string, error) { calls++; return old(p) }
	defer func() { finalPathFn = old }()
	full := filepath.Join(root, "alias", "remote", "child")
	if got := realTarget(full, []string{root}); got != full {
		t.Fatalf("resolved remote target: %s", got)
	}
	if _, _, ok := davResolve("/alias/remote/child", "", root); ok {
		t.Fatal("DAV accepted remote junction chain")
	}
	if _, ok := syncResolve("alias/remote/child", root); ok {
		t.Fatal("sync accepted remote junction chain")
	}
	if !protectedCanonical(full, root) {
		t.Fatal("remote junction chain is unprotected")
	}
	if calls != 0 {
		t.Fatalf("finalPath called %d times for remote junction chain", calls)
	}
}

// The local administrative share exercises real UNC handles without a remote host.
func TestDAVMoveCopyUNCRoot(t *testing.T) {
	local := t.TempDir()
	volume := filepath.VolumeName(local)
	if len(volume) != 2 {
		t.Skip("temporary directory is not on a drive")
	}
	root := `\\localhost\` + volume[:1] + `$` + strings.TrimPrefix(local, volume)
	if _, err := os.Stat(root); err != nil {
		t.Skipf("local administrative share unavailable: %v", err)
	}
	t.Setenv("HOME", t.TempDir())
	for _, method := range []string{"MOVE", "COPY"} {
		t.Run(method, func(t *testing.T) {
			if err := os.WriteFile(filepath.Join(local, "source"), []byte("inside"), 0600); err != nil {
				t.Fatal(err)
			}
			dest := strings.ToLower(method)
			r := davHandle(method, "/s/source", map[string]string{"destination": "/s/" + dest}, nil, root, "/s")
			if r.Status != 201 {
				t.Fatalf("%s on UNC root: status %d, body %s", method, r.Status, r.Body)
			}
			if b, err := os.ReadFile(filepath.Join(local, dest)); err != nil || string(b) != "inside" {
				t.Fatalf("destination = %q, %v", b, err)
			}
		})
	}
}
