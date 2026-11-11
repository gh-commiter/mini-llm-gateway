package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// claudeCodeCases: primeri #79, isti za Swift (RouterCoreTests, claudeCodeCases).
type claudeCodeCases struct {
	Procs []struct {
		Exe    string
		Args   []string
		Claude bool
	}
	Chains []struct {
		Name     string
		Terminal bool
		Claude   bool
		Chain    []struct {
			Exe                    string
			Args                   []string
			Unreadable, Loop, Gone bool
		}
	}
	Policy []struct {
		Name                      string
		Marker, Override, Checked bool
		Chain                     int
	}
	Args []struct {
		Args    []string
		Refused bool
	}
	Continue []struct {
		Args, Out []string
		Last      string
		Dropped   bool
	}
}

func loadClaudeCodeCases(t *testing.T) claudeCodeCases {
	var c claudeCodeCases
	b, err := os.ReadFile("testdata/claude-code-cases.json")
	if err == nil {
		err = json.Unmarshal(b, &c)
	}
	if err != nil || len(c.Chains) == 0 {
		t.Fatal(err)
	}
	return c
}

// fakeChain: lažna veriga prednikov (PID 1000 je starš minigw).
func (c claudeCodeCases) fakeChain(i int) procFunc {
	ch := c.Chains[i].Chain
	return func(pid int) (int, string, []string, error) {
		e := ch[pid-1000]
		switch {
		case e.Unreadable:
			return 0, "", nil, errors.New("unreadable")
		case e.Gone:
			return 0, "", nil, nil
		case e.Loop:
			return 1000, e.Exe, e.Args, nil
		case pid-1000 == len(ch)-1:
			return 0, e.Exe, e.Args, nil
		}
		return pid + 1, e.Exe, e.Args, nil
	}
}

func TestClaudeCodeDetection(t *testing.T) {
	c := loadClaudeCodeCases(t)
	for _, p := range c.Procs {
		if isClaudeProc(p.Exe, p.Args) != p.Claude {
			t.Errorf("isClaudeProc(%q, %q) != %v", p.Exe, p.Args, p.Claude)
		}
	}
	for i, ch := range c.Chains {
		if claudeCaller(1000, c.fakeChain(i), ch.Terminal) != ch.Claude {
			t.Errorf("%s: claude = %v expected", ch.Name, ch.Claude)
		}
	}
	// prava veriga: ta proces ni claude, branje dela (vsi trije sistemi)
	if _, exe, _, err := procLookup()(os.Getpid()); err != nil || isClaudeProc(exe, nil) {
		t.Errorf("own process: %q %v", exe, err)
	}
}

// Možnosti po kontekstu klicatelja: označen (CLAUDECODE ali claude med predniki, markClaudeCode) in brez
// MINIGW_ALLOW_UNSAFE_FLAGS=1 → seznam claudeSafe; sicer vse (D2: človek v terminalu, skripte).
func TestClaudeCodeArgsPolicy(t *testing.T) {
	c := loadClaudeCodeCases(t)
	for _, p := range c.Policy {
		ch := c.Chains[p.Chain]
		env := map[string]string{}
		if p.Marker || claudeCaller(1000, c.fakeChain(p.Chain), ch.Terminal) {
			env["CLAUDECODE"] = "1"
		}
		if p.Override {
			env["MINIGW_ALLOW_UNSAFE_FLAGS"] = "1"
		}
		get := func(k string) string { return env[k] }
		if checkClaudeArgs(get) != p.Checked {
			t.Errorf("%s: checked != %v", p.Name, p.Checked)
		}
		for _, a := range c.Args {
			if refused := unsafeClaudeFlag(a.Args, get) != ""; refused != (p.Checked && a.Refused) {
				t.Errorf("%s: %q refused = %v", p.Name, a.Args, refused)
			}
		}
	}
}

func TestContinueSession(t *testing.T) {
	c := loadClaudeCodeCases(t)
	for _, k := range c.Continue {
		out, dropped := continueSession(slices.Clone(k.Args), k.Last)
		if !slices.Equal(out, k.Out) || dropped != k.Dropped {
			t.Errorf("%q, %q: %q %v", k.Args, k.Last, out, dropped)
		}
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("APPDATA", filepath.Join(home, "AppData"))
	a, b, id := filepath.Join(home, "a"), filepath.Join(home, "b"), "0c3b5a52-4f0e-4c4b-9d7e-2b1f1a6e9c01"
	if lastSession(a) != "" {
		t.Fatal("session without a record")
	}
	saveLastSession(a, id)
	saveLastSession(b, "--dangerously-skip-permissions") // ni ID seje: ne zapiše
	if lastSession(a) != id || lastSession(b) != "" {
		t.Fatalf("records: %q %q", lastSession(a), lastSession(b))
	}
	os.WriteFile(lastSessionFile(), []byte(`{"`+strings.ReplaceAll(b, `\`, `\\`)+`":"-x"}`), 0o600)
	if lastSession(b) != "" {
		t.Fatal("invalid ID in the file passed on")
	}
}

// fakeClaudeMain: testni program kot »claude« (kopija z imenom claude): »--parent prog args…« požene minigw kot otrok
// claude (prednik, ne CLAUDECODE); sicer je to claude, ki ga zažene minigw: zapiše argumente v MINIGW_TEST_CLAUDE_LOG.
func fakeClaudeMain() {
	if len(os.Args) > 2 && os.Args[1] == "--parent" {
		cmd := exec.Command(os.Args[2], os.Args[3:]...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		err := cmd.Run()
		if e, ok := err.(*exec.ExitError); ok {
			os.Exit(e.ExitCode())
		}
		if err != nil {
			os.Exit(125)
		}
		os.Exit(0)
	}
	os.WriteFile(os.Getenv("MINIGW_TEST_CLAUDE_LOG"), []byte(strings.Join(os.Args[1:], " ")), 0o600)
	os.Exit(0)
}

// #79: »env -u CLAUDECODE minigw … --continue« pod pravim procesom claude minigw zavrne, preden zažene claude;
// MINIGW_ALLOW_UNSAFE_FLAGS=1 ga zažene.
func TestClaudeAncestorRefusesBeforeClaude(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "claude")
	if runtime.GOOS == "windows" {
		fake += ".exe"
	}
	b, err := os.ReadFile(os.Args[0])
	if err != nil || os.WriteFile(fake, b, 0o755) != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "claude.log")
	run := func(extra []string, args ...string) (string, bool) {
		cmd := exec.Command(fake, append([]string{"--parent", os.Args[0]}, args...)...)
		if slices.Contains(extra, "direct") { // brez lažnega starša claude
			cmd = exec.Command(os.Args[0], args...)
		}
		cmd.Env = append(slices.DeleteFunc(os.Environ(), func(kv string) bool {
			return strings.HasPrefix(kv, "CLAUDECODE=") || strings.HasPrefix(kv, "MINIGW_ALLOW_UNSAFE_FLAGS=") || strings.HasPrefix(strings.ToUpper(kv), "PATH=")
		}), append(extra, "MINIGW_TEST_MAIN=1", "MINIGW_TEST_CLAUDE_LOG="+log, "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))...)
		os.Remove(log)
		out, _ := cmd.CombinedOutput()
		_, err := os.Stat(log)
		return string(out), err == nil
	}
	for _, a := range [][]string{{"-h", "--continue"}, {"-p", "--continue", "x"}, {"-p", "-c", "x"}, {"-p", "--resume", "0c3b5a52-4f0e-4c4b-9d7e-2b1f1a6e9c01", "x"}} {
		out, spawned := run(nil, a...)
		if spawned || !strings.Contains(out, "is not accepted when minigw runs inside Claude Code") {
			t.Errorf("%q: spawned=%v %s", a, spawned, out)
		}
	}
	if out, spawned := run([]string{"MINIGW_ALLOW_UNSAFE_FLAGS=1"}, "-h", "--continue"); !spawned {
		t.Errorf("override: claude not started: %s", out)
	}
	// kontrola: brez starša claude (test ne teče v Claude Code, npr. CI) minigw claude zažene
	if !claudeCaller(os.Getpid(), procLookup(), false) {
		if out, spawned := run([]string{"direct"}, "-h", "--continue"); !spawned {
			t.Errorf("without a claude parent: claude not started: %s", out)
		}
	}
}
