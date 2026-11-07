//go:build linux

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// H1 (2. plast) in L1 pod pravim bwrap: dovoljena mapa okoli Scratch ne pokaže zrcal (le klon in objekti), .git klona
// ni mogoče preimenovati, .git/config je le za branje, objekti so pisljivi.
func TestBwrapWorktreeMounts(t *testing.T) {
	base := t.TempDir()
	scratch := filepath.Join(base, "scratch")
	mirror := filepath.Join(scratch, "repos", "d", "r.git")
	wt := filepath.Join(scratch, "worktrees", "d", "r", "codex-x-0")
	for _, d := range []string{filepath.Join(mirror, "objects"), filepath.Join(wt, ".git", "objects"), filepath.Join(wt, ".git", "hooks")} {
		os.MkdirAll(d, 0o700)
	}
	os.WriteFile(filepath.Join(mirror, "config"), []byte("MIRROR"), 0o600)
	os.WriteFile(filepath.Join(wt, ".git", "config"), []byte("CLONE"), 0o600)
	cfg := t.TempDir()
	m := agentMounts{cfgDir: cfg, hide: scratch, rw: []string{wt, base}, ro: []string{filepath.Join(mirror, "objects")},
		pins: []string{filepath.Join(wt, ".git")}, protect: []string{filepath.Join(wt, ".git", "config"), filepath.Join(wt, ".git", "hooks")}}
	script := "cat " + mirror + "/config 2>/dev/null; echo; ls " + mirror + "/objects >/dev/null && echo objects-visible; " +
		"cd " + wt + " && (mv .git .g 2>/dev/null && echo mv-ok || echo mv-denied); " +
		"(echo x >> .git/config 2>/dev/null && echo cfg-written || echo cfg-denied); " +
		"(echo o > .git/objects/o && echo objects-writable); echo end"
	name, args := agentWrap("/bin/sh", []string{"-c", script}, m)
	if name == "/bin/sh" {
		t.Skip("no usable bwrap (Linux with user namespaces)")
	}
	out, err := exec.Command(name, args...).CombinedOutput()
	s := string(out)
	for _, bad := range []string{"MIRROR", "mv-ok", "cfg-written"} {
		if strings.Contains(s, bad) {
			t.Fatalf("%s under bwrap: %q (%v)", bad, s, err)
		}
	}
	for _, want := range []string{"objects-visible", "mv-denied", "cfg-denied", "objects-writable", "end"} {
		if !strings.Contains(s, want) {
			t.Fatalf("missing %s: %q (%v)", want, s, err)
		}
	}
}

// L4: killInside ubije proces v klonu iz seje prehoda, ne procesa v drugi seji.
func TestKillInside(t *testing.T) {
	wt := t.TempDir()
	same := exec.Command("sleep", "30")
	same.Dir = wt
	other := exec.Command("sleep", "30")
	other.Dir = wt
	other.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if same.Start() != nil || other.Start() != nil {
		t.Skip("cannot start sleep")
	}
	defer other.Process.Kill()
	killInside(wt)
	done := make(chan error, 1)
	go func() { done <- same.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		same.Process.Kill()
		t.Fatal("process in the gateway's session survived")
	}
	if err := other.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatal("process in another session was killed")
	}
}
