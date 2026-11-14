package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
	isolateGitWatchState(t)
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
	isolateGitWatchState(t)
	root := t.TempDir()
	if gitWatchStart(root) != nil { // not a repository
		t.Error("watching a folder without a repository")
	}
	os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: ../elsewhere\n"), 0o644) // a gitdir: file
	if gitWatchStart(root) != nil {
		t.Error("watching a repository behind a gitdir: file")
	}
}

// isolateGitWatchState: the stored start states go to a folder of the test, not to the user's.
func isolateGitWatchState(t *testing.T) {
	t.Helper()
	d := t.TempDir()
	t.Setenv("HOME", d)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(d, "config"))
	t.Setenv("APPDATA", filepath.Join(d, "appdata"))
}

// X2: a client that was killed (the end of the share never ran) leaves its stored start state; the next start in the folder reports it, prefixed, and removes it.
func TestGitWatchSurvivesAKilledClient(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("needs git")
	}
	isolateGitWatchState(t)
	root := t.TempDir()
	gitIn(t, root, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(root, "a.txt"), []byte("a"), 0o644)
	gitIn(t, root, "add", "a.txt")
	gitIn(t, root, "commit", "-qm", "init")
	w := gitWatchStart(root)
	if w == nil || w.file == "" {
		t.Fatal("no stored state")
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(w.file); fi.Mode().Perm() != 0o600 {
			t.Errorf("state file mode %v", fi.Mode().Perm())
		}
		if fi, _ := os.Stat(filepath.Dir(w.file)); fi.Mode().Perm() != 0o700 {
			t.Errorf("state folder mode %v", fi.Mode().Perm())
		}
	}
	if strings.HasPrefix(w.file, root) {
		t.Error("the state is inside the repository")
	}
	if b, _ := os.ReadFile(w.file); strings.Contains(string(b), "a.txt") && !strings.Contains(string(b), "protected") {
		t.Error("unexpected content")
	}
	// the agent works, the client is killed: no report, no forget
	os.WriteFile(filepath.Join(root, ".envrc"), []byte("echo hi"), 0o644)
	gitIn(t, root, "add", ".envrc")
	gitIn(t, root, "commit", "-qm", "envrc")
	gitIn(t, root, "branch", "feature")
	// another process (alive: the file stays) and then one after the death of the first (the file is reported and removed)
	if l := gitWatchRecover(root, os.Getpid()+1, func(int) bool { return true }); len(l) != 0 {
		t.Errorf("a live run was reported: %v", l)
	}
	lines := gitWatchRecover(root, os.Getpid()+1, func(int) bool { return false })
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"from the previous run that did not finish: git in this folder", "1 new commit(s)", "new branches: feature", "committed a protected path: .envrc"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in:\n%s", want, joined)
		}
	}
	if _, err := os.Stat(w.file); err == nil {
		t.Error("the state file was not removed")
	}
	if l := gitWatchRecover(root, os.Getpid()+1, func(int) bool { return false }); len(l) != 0 {
		t.Errorf("reported twice: %v", l)
	}
}

// A damaged or foreign state file is ignored with one line.
func TestGitWatchIgnoresADamagedStateFile(t *testing.T) {
	isolateGitWatchState(t)
	root := t.TempDir()
	good := gitWatchStored{Version: 1, Root: root, PID: 1, Head: "main", Refs: map[string]string{"refs/heads/main": strings.Repeat("a", 40)}, IndexHash: strings.Repeat("b", 64)}
	for name, mutate := range map[string]func(*gitWatchStored){
		"version":  func(s *gitWatchStored) { s.Version = 2 },
		"root":     func(s *gitWatchStored) { s.Root = "/elsewhere" },
		"ref name": func(s *gitWatchStored) { s.Refs = map[string]string{"../x": strings.Repeat("a", 40)} },
		"ref id":   func(s *gitWatchStored) { s.Refs = map[string]string{"refs/heads/x": "zz"} },
		"entry":    func(s *gitWatchStored) { s.Protected = map[string]string{".envrc": "bad"} },
		"pending":  func(s *gitWatchStored) { s.Pending = []string{"HEAD"} },
		"hash":     func(s *gitWatchStored) { s.IndexHash = "short" },
	} {
		s := good
		mutate(&s)
		data, _ := json.Marshal(s)
		if _, err := parseGitWatchStored(data, root); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := parseGitWatchStored([]byte(`{"version":1,"root":"x","extra":1}`), root); err == nil {
		t.Error("unknown field accepted")
	}
	if _, err := parseGitWatchStored(make([]byte, gitWatchMaxFile+1), root); err == nil {
		t.Error("oversized file accepted")
	}
	data, _ := json.Marshal(good)
	if _, err := parseGitWatchStored(data, root); err != nil {
		t.Errorf("good file refused: %v", err)
	}
	f := gitWatchFile(root, 99999)
	os.MkdirAll(filepath.Dir(f), 0o700)
	os.WriteFile(f, []byte("not json"), 0o600)
	l := gitWatchRecover(root, 1, func(int) bool { return false })
	if len(l) != 1 || !strings.Contains(l[0], "ignoring a damaged git watch state file") {
		t.Errorf("lines: %v", l)
	}
	if _, err := os.Stat(f); err == nil {
		t.Error("the damaged file stays")
	}
}
