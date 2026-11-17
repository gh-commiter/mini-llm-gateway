package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestTakeLabels(t *testing.T) {
	var warns []string
	warn := func(m string) { warns = append(warns, m) }
	l, rest := takeLabels([]string{"-p", "--minigw-phase", "Plan", "--minigw-activity=bugfix", "--minigw-task", "#149", "hello"}, envOf(nil), warn)
	if l != (runLabels{"plan", "bugfix", "#149", "given"}) || !reflect.DeepEqual(rest, []string{"-p", "hello"}) || len(warns) != 0 {
		t.Fatal(l, rest, warns)
	}
	// The environment, overridden by a flag; an unusual but safe value is kept as given.
	l, _ = takeLabels([]string{"--minigw-task=feature/x"}, envOf(map[string]string{"MINIGW_PHASE": "act", "MINIGW_ACTIVITY": "Triage", "MINIGW_TASK": "old"}), warn)
	if l != (runLabels{"act", "triage", "feature/x", "given"}) {
		t.Fatal(l)
	}
	// Anything that is not a safe token is dropped with one warning each; nothing else changes, nothing is refused.
	warns = nil
	l, rest = takeLabels([]string{"--minigw-phase", "not a token!", "--minigw-activity=", "--minigw-task=" + strings.Repeat("x", 65), "--minigw-phase", "!!", "x"}, envOf(nil), warn)
	if l != (runLabels{}) || !reflect.DeepEqual(rest, []string{"x"}) || len(warns) != 2 { // the last phase wins: one warning per final value
		t.Fatal(l, rest, warns)
	}
	// A trailing flag without a value is dropped as well.
	warns = nil
	if l, rest = takeLabels([]string{"a", "--minigw-task"}, envOf(nil), warn); l.task != "" || len(warns) != 1 || !reflect.DeepEqual(rest, []string{"a"}) {
		t.Fatal(l, rest, warns)
	}
}

// A value is one token or nothing: it is never parsed again as another option, so it cannot change the work mode, the tools
// level or the read-only state (those have their own flags, handled elsewhere and refused inside Claude Code).
func TestLabelValuesAreNeverReparsed(t *testing.T) {
	warns := 0
	warn := func(string) { warns++ }
	for _, v := range []string{"x --minigw-work=direct", "x; rm", "$(id)", "a\nb", "x --minigw-tools=edit"} {
		args := []string{"--minigw-task", v, "--minigw-phase=" + v, "-p", "hi"}
		l, rest := takeLabels(args, envOf(nil), warn)
		if l.task != "" && l.task != v || strings.ContainsAny(l.phase+l.activity, " ;$") || !reflect.DeepEqual(rest, []string{"-p", "hi"}) {
			t.Fatalf("%q: %+v %v", v, l, rest)
		}
		for _, r := range rest {
			if strings.Contains(r, "--minigw-") {
				t.Fatalf("%q: re-parsed as %q", v, r)
			}
		}
	}
	if warns == 0 {
		t.Fatal("no warning")
	}
}

func TestLabelsBranchFill(t *testing.T) {
	linked := func(string) string { return "root" }
	l := runLabels{}
	l.withBranch("x", linked, func(string, string) string { return "feature/149-usage" })
	if l.task != "feature/149-usage" || l.taskSource != "branch" {
		t.Fatal(l)
	}
	l = runLabels{task: "#1", taskSource: "given"}
	l.withBranch("x", linked, func(string, string) string { return "main" })
	if l.task != "#1" || l.taskSource != "given" {
		t.Fatal("a given task was replaced", l)
	}
	l = runLabels{}
	l.withBranch("x", func(string) string { return "" }, func(string, string) string { return "main" }) // not linked
	l.withBranch("x", linked, func(string, string) string { return "" })                                // detached HEAD or no git
	l.withBranch("x", linked, func(string, string) string { return "bad branch name" })                 // not a safe token
	if l.task != "" || l.taskSource != "" {
		t.Fatal(l)
	}
	if h := (runLabels{"plan", "", "feature/x", "branch"}).headers(); h != "\nx-claude-mini-phase: plan\nx-claude-mini-task: feature/x\nx-claude-mini-task-source: branch" {
		t.Fatal(h)
	}
}

func TestGitBranchReadsFilesOnly(t *testing.T) {
	dir := t.TempDir()
	repo := filepath.Join(dir, "repo")
	os.MkdirAll(filepath.Join(repo, ".git"), 0o755)
	os.MkdirAll(filepath.Join(repo, "sub", "deep"), 0o755)
	write := func(p, s string) {
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(repo, ".git", "HEAD"), "ref: refs/heads/feature/x\n")
	if b := gitBranch(filepath.Join(repo, "sub", "deep"), repo); b != "feature/x" {
		t.Fatal(b)
	}
	write(filepath.Join(repo, ".git", "HEAD"), "0123456789012345678901234567890123456789\n") // detached
	if b := gitBranch(repo, repo); b != "" {
		t.Fatal(b)
	}
	// A worktree: .git is a file that points at the git dir.
	wt := filepath.Join(dir, "wt")
	os.MkdirAll(wt, 0o755)
	gd := filepath.Join(repo, ".git", "worktrees", "wt")
	os.MkdirAll(gd, 0o755)
	write(filepath.Join(gd, "HEAD"), "ref: refs/heads/topic\n")
	write(filepath.Join(wt, ".git"), "gitdir: "+gd+"\n")
	if b := gitBranch(wt, wt); b != "topic" {
		t.Fatal(b)
	}
	// A repository ABOVE the linked folder gives no branch for a project it is not part of.
	os.WriteFile(filepath.Join(repo, ".git", "HEAD"), []byte("ref: refs/heads/outer\n"), 0o644)
	inner := filepath.Join(repo, "linked", "deeper")
	os.MkdirAll(inner, 0o755)
	if b := gitBranch(inner, filepath.Join(repo, "linked")); b != "" {
		t.Fatal("a repository above the link supplied", b)
	}
	if b := gitBranch(inner, repo); b != "outer" { // the link is made at the repository root: found
		t.Fatal(b)
	}
	if b := gitBranch(t.TempDir(), t.TempDir()); b != "" { // no repository
		t.Fatal(b)
	}
}

func TestAgentAskSkipsLabelFlags(t *testing.T) {
	m, words, bad := agentAskArgs([]string{"-p", "--minigw-phase", "check", "--minigw-task=#1", "--model", "m", "q"})
	if m != "m" || bad != "" || !reflect.DeepEqual(words, []string{"q"}) {
		t.Fatal(m, words, bad)
	}
}

// The shared cases (testdata/labels.json) are asserted by the Swift client too (RunLabelsTests.swift).
func TestTakeLabelsSharedCases(t *testing.T) {
	var fx struct {
		Cases []struct {
			Name                              string
			Args                              []string
			Env                               map[string]string
			Phase, Activity, Task, TaskSource string
			Rest                              []string
			Warnings                          int
		}
	}
	readFixture(t, "labels.json", &fx)
	for _, c := range fx.Cases {
		warns := 0
		l, rest := takeLabels(c.Args, envOf(c.Env), func(string) { warns++ })
		if rest == nil {
			rest = []string{}
		}
		if l != (runLabels{c.Phase, c.Activity, c.Task, c.TaskSource}) || !reflect.DeepEqual(rest, c.Rest) || warns != c.Warnings {
			t.Errorf("%s: %+v %v %d warnings", c.Name, l, rest, warns)
		}
	}
}
