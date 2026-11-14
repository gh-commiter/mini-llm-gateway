package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

type gitWatchCase struct {
	Name    string
	Start   gitState
	End     gitState
	Commits []gitCommit
	Lines   []string
	Reset   []string
}

func TestGitWatchReportMatchesTheSharedTable(t *testing.T) {
	var f struct{ Cases []gitWatchCase }
	if err := json.Unmarshal(srcFile(t, "testdata/git-watch.json"), &f); err != nil || len(f.Cases) == 0 {
		t.Fatal(err)
	}
	for _, c := range f.Cases {
		lines, reset := gitWatchReport(c.Start, c.End, c.Commits, protectedPath)
		if !slices.Equal(lines, c.Lines) || !slices.Equal(reset, c.Reset) {
			t.Errorf("%s:\n got %q reset %q\nwant %q reset %q", c.Name, lines, reset, c.Lines, c.Reset)
		}
	}
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.name=n", "-c", "user.email=a@b", "-c", "commit.gpgsign=false"}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return string(out)
}

// A real repository: what an agent does through the share (commit and stage a protected path, start a merge) is reported, the staged path is taken out of
// the index, the work tree and the commit are left alone.
func TestGitWatchRealRepository(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("needs git")
	}
	root := t.TempDir()
	gitIn(t, root, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(root, "a.txt"), []byte("a"), 0o644)
	gitIn(t, root, "add", "a.txt")
	gitIn(t, root, "commit", "-qm", "init")
	gitIn(t, root, "branch", "side")
	w := gitWatchStart(root)
	if w == nil {
		t.Fatal("no watch")
	}
	// the agent: a commit with a protected file, a protected file in the index, a new branch and a merge in progress
	os.WriteFile(filepath.Join(root, ".envrc"), []byte("echo hi"), 0o644)
	gitIn(t, root, "add", ".envrc")
	gitIn(t, root, "commit", "-qm", "envrc")
	gitIn(t, root, "branch", "feature")
	os.MkdirAll(filepath.Join(root, ".vscode"), 0o755)
	os.WriteFile(filepath.Join(root, ".vscode", "tasks.json"), []byte("{}"), 0o644)
	gitIn(t, root, "add", ".vscode/tasks.json")
	os.WriteFile(filepath.Join(root, ".git", "MERGE_HEAD"), []byte(strings.TrimSpace(gitIn(t, root, "rev-parse", "side"))+"\n"), 0o644)
	r, wr, _ := os.Pipe()
	saved := os.Stderr
	os.Stderr = wr
	w.report()
	wr.Close()
	os.Stderr = saved
	buf := make([]byte, 1<<16)
	n, _ := r.Read(buf)
	out := string(buf[:n])
	for _, want := range []string{"1 new commit(s)", "new branches: feature", "committed a protected path: .envrc", "staged a protected path: .vscode/tasks.json", "merge, cherry-pick or revert is in progress"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if st := gitIn(t, root, "ls-files", "-s", ".vscode/tasks.json"); st != "" {
		t.Errorf("the staged protected path is still in the index: %q", st)
	}
	if _, err := os.Stat(filepath.Join(root, ".vscode", "tasks.json")); err != nil {
		t.Error("the work tree file was touched")
	}
	if !strings.Contains(gitIn(t, root, "log", "--oneline"), "envrc") {
		t.Error("the commit was rewritten")
	}
	if _, err := os.Stat(filepath.Join(root, ".git", "MERGE_HEAD")); err != nil {
		t.Error("MERGE_HEAD was deleted")
	}
}

func TestGitWatchIgnoresWhatItCannotWatch(t *testing.T) {
	root := t.TempDir()
	if gitWatchStart(root) != nil { // not a repository
		t.Error("watching a folder without a repository")
	}
	os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: ../elsewhere\n"), 0o644) // a gitdir: file
	if gitWatchStart(root) != nil {
		t.Error("watching a repository behind a gitdir: file")
	}
}
