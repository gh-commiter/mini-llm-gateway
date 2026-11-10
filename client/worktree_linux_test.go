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

// N3c: zamenjava .git, ki jo agent takoj vrne, se ne skrije (inotify); premik druge datoteke ne sproži nič.
func TestWatchGitMovesSwapAndRevert(t *testing.T) {
	wt := t.TempDir()
	g := filepath.Join(wt, ".git")
	os.MkdirAll(g, 0o700)
	moved := make(chan struct{}, 1)
	stop, ok := watchGitMoves([]string{g}, func() { moved <- struct{}{} })
	if !ok {
		t.Fatal("inotify watch failed")
	}
	defer stop()
	os.WriteFile(filepath.Join(g, "index"), []byte("x"), 0o600) // git commit piše v .git
	os.Rename(filepath.Join(g, "index"), filepath.Join(g, "index2"))
	select {
	case <-moved:
		t.Fatal("a write inside .git counted as a move")
	case <-time.After(200 * time.Millisecond):
	}
	if os.Rename(g, filepath.Join(wt, ".g")) != nil || os.Mkdir(g, 0o700) != nil || os.Remove(g) != nil ||
		os.Rename(filepath.Join(wt, ".g"), g) != nil {
		t.Fatal("swap failed")
	}
	select {
	case <-moved:
	case <-time.After(2 * time.Second):
		t.Fatal("swap and revert not seen")
	}
	// stop: zaprtje ni premik (vsaka druga napaka branja bi bila)
	stopped := make(chan struct{}, 1)
	stop2, ok := watchGitMoves([]string{g}, func() { stopped <- struct{}{} })
	if !ok {
		t.Fatal("second watch failed")
	}
	stop2()
	select {
	case <-stopped:
		t.Fatal("stop counted as a move")
	case <-time.After(200 * time.Millisecond):
	}
}

// #53: Codex, Grok in Copilot pod bwrap ne vidijo korenov TMPDIR tekov Claude CLI (tudi znotraj pisljive mape).
func TestBwrapHidesClaudeTempRoots(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "mgw-x")
	os.MkdirAll(filepath.Join(root, "run"), 0o700)
	os.WriteFile(filepath.Join(root, "run", "secret"), []byte("CLAUDETMP"), 0o600)
	name, args := agentWrap("/bin/sh", []string{"-c", "cat " + root + "/run/secret; echo end"},
		agentMounts{cfgDir: t.TempDir(), rw: []string{base}, tmps: []string{root}})
	if name == "/bin/sh" {
		t.Skip("no usable bwrap (Linux with user namespaces)")
	}
	out, err := exec.Command(name, args...).CombinedOutput()
	if strings.Contains(string(out), "CLAUDETMP") || !strings.Contains(string(out), "end") {
		t.Fatalf("Claude temp root visible under bwrap: %q (%v)", out, err)
	}
}

// #40 M2: pod bwrap ni mogoče preimenovati ne .git ne mape na poti do ugnezdenega klona (nova a/b/.git bi prevzela git
// harnessa); datoteke v njih so pisljive.
func TestBwrapPinsNestedPath(t *testing.T) {
	wt := t.TempDir()
	for _, d := range []string{".git", "a/b/.git", "a/b/c/.git"} {
		os.MkdirAll(filepath.Join(wt, d), 0o700)
	}
	gits := []string{filepath.Join(wt, ".git"), filepath.Join(wt, "a", "b", ".git"), filepath.Join(wt, "a", "b", "c", ".git")}
	m := agentMounts{cfgDir: t.TempDir(), rw: []string{wt}, pins: pinPaths(wt, gits)}
	script := "cd " + wt + "; for p in a a/b a/b/c a/b/.git a/b/c/.git; do mv $p $p.x 2>/dev/null && echo moved-$p; done; " +
		"echo f > a/b/f.txt && echo write-ok"
	name, args := agentWrap("/bin/sh", []string{"-c", script}, m)
	if name == "/bin/sh" {
		t.Skip("no usable bwrap (Linux with user namespaces)")
	}
	out, err := exec.Command(name, args...).CombinedOutput()
	if s := string(out); strings.Contains(s, "moved-") || !strings.Contains(s, "write-ok") {
		t.Fatalf("nested path not pinned: %q (%v)", s, err)
	}
}

// #40: ena instanca inotify za vse poti (pinPaths): premik katerekoli (tudi mape do ugnezdenega klona) se vidi.
func TestWatchGitMovesManyPaths(t *testing.T) {
	wt := t.TempDir()
	dirs := []string{filepath.Join(wt, ".git"), filepath.Join(wt, "a"), filepath.Join(wt, "a", "lib", ".git")}
	for _, d := range dirs {
		os.MkdirAll(d, 0o700)
	}
	moved := make(chan struct{}, 1)
	stop, ok := watchGitMoves(dirs, func() { moved <- struct{}{} })
	if !ok {
		t.Fatal("inotify watch failed")
	}
	defer stop()
	if os.Rename(filepath.Join(wt, "a"), filepath.Join(wt, "b")) != nil {
		t.Fatal("rename")
	}
	select {
	case <-moved:
	case <-time.After(2 * time.Second):
		t.Fatal("move of a/ not seen")
	}
	if _, ok := watchGitMoves([]string{filepath.Join(wt, "missing")}, func() {}); ok {
		t.Fatal("watch of a missing path accepted")
	}
}

// #40 N1: agent z okoljem Claude CLI ali Codex (MINIGW_CLONE, brez GIT_WORK_TREE), ki je zapustil klon in sejo, se ustavi.
func TestKillInsideMinigwClone(t *testing.T) {
	wt := t.TempDir()
	for _, agent := range []string{"claude", "codex"} {
		var mark string
		for _, kv := range worktreeClaudeEnv(wt, wt, wt, filepath.Join(t.TempDir(), "gx"), []string{filepath.Join(wt, ".git")}, agent) {
			if strings.HasPrefix(kv, "MINIGW_CLONE=") {
				mark = kv
			}
		}
		c := exec.Command("sleep", "30")
		c.Dir, c.Env = "/", []string{mark}
		c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if mark == "" || c.Start() != nil {
			t.Fatalf("%s: mark %q", agent, mark)
		}
		killInside(wt)
		done := make(chan error, 1)
		go func() { done <- c.Wait() }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			c.Process.Kill()
			t.Fatalf("%s: agent process with MINIGW_CLONE survived", agent)
		}
	}
}
