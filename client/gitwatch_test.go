package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
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
	w.report(true)
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
	if l := gitWatchRecover(root, os.Getpid()+1, func(int, string) bool { return true }); len(l) != 0 {
		t.Errorf("a live run was reported: %v", l)
	}
	lines := gitWatchRecover(root, os.Getpid()+1, func(int, string) bool { return false })
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"from the previous run that did not finish: git in this folder", "1 new commit(s)", "new branches: feature", "committed a protected path: .envrc"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in:\n%s", want, joined)
		}
	}
	if _, err := os.Stat(w.file); err == nil {
		t.Error("the state file was not removed")
	}
	if l := gitWatchRecover(root, os.Getpid()+1, func(int, string) bool { return false }); len(l) != 0 {
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
	l := gitWatchRecover(root, 1, func(int, string) bool { return false })
	if len(l) != 1 || !strings.Contains(l[0], "ignoring a damaged git watch state file") {
		t.Errorf("lines: %v", l)
	}
	if _, err := os.Stat(f); err == nil {
		t.Error("the damaged file stays")
	}
}

// watchRepo: a repository with one commit, the watch started on it.
func watchRepo(t *testing.T) string {
	t.Helper()
	isolateGitWatchState(t)
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("needs git")
	}
	root := t.TempDir()
	gitIn(t, root, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(root, "a.txt"), []byte("a"), 0o644)
	gitIn(t, root, "add", "a.txt")
	gitIn(t, root, "commit", "-qm", "init")
	return root
}

// captureStderr: what the watch prints while f runs.
func captureStderr(t *testing.T, f func()) string {
	t.Helper()
	r, wr, _ := os.Pipe()
	saved := os.Stderr
	os.Stderr = wr
	f()
	wr.Close()
	os.Stderr = saved
	b, _ := io.ReadAll(r)
	return string(b)
}

func wantAll(t *testing.T, out string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q in:\n%s", w, out)
		}
	}
}

// Review finding 2: a replacement commit with a protected path, made reachable only through refs/replace/<HEAD> (loose, then packed). The watch reads without replacements:
// the commit is reported although its ref is a replacement, and so is the ref.
func TestGitWatchReportsReplacementRefs(t *testing.T) {
	for _, packed := range []bool{false, true} {
		root := watchRepo(t)
		w := gitWatchStart(root)
		head := strings.TrimSpace(gitIn(t, root, "rev-parse", "HEAD"))
		os.WriteFile(filepath.Join(root, ".envrc"), []byte("echo hi"), 0o644)
		gitIn(t, root, "add", ".envrc")
		tree := strings.TrimSpace(gitIn(t, root, "write-tree"))
		repl := strings.TrimSpace(gitIn(t, root, "commit-tree", tree, "-m", "replacement")) // on no branch
		gitIn(t, root, "reset", "-q")
		os.Remove(filepath.Join(root, ".envrc"))
		gitIn(t, root, "update-ref", "refs/replace/"+head, repl)
		if packed {
			gitIn(t, root, "pack-refs", "--all")
		}
		out := captureStderr(t, func() { w.report(true) })
		wantAll(t, out, "replacement ref ("+head+")", "committed a protected path: .envrc", "other refs changed: refs/replace/"+head)
	}
}

// Review finding 6: the agent commits a protected path on a detached HEAD (no ref moves) and leaves it there, the index matching the commit.
func TestGitWatchSeesACommitOnADetachedHead(t *testing.T) {
	for _, detachedAtStart := range []bool{false, true} {
		root := watchRepo(t)
		if detachedAtStart {
			gitIn(t, root, "checkout", "-q", "--detach")
		}
		w := gitWatchStart(root)
		gitIn(t, root, "checkout", "-q", "--detach")
		os.MkdirAll(filepath.Join(root, ".claude"), 0o755)
		os.WriteFile(filepath.Join(root, ".claude", "settings.json"), []byte("{}"), 0o644)
		gitIn(t, root, "add", ".claude/settings.json")
		gitIn(t, root, "commit", "-qm", "settings")
		out := captureStderr(t, func() { w.report(true) })
		wantAll(t, out, "1 new commit(s)", "committed a protected path: .claude/settings.json")
		if detachedAtStart {
			wantAll(t, out, "detached HEAD moved")
		}
	}
}

// Review finding 4: a merge commit brings a protected path in from a side branch that was there at the start (so its own commit is not new). The merge's tree is diffed
// against its parents.
func TestGitWatchSeesWhatAMergeBringsIn(t *testing.T) {
	root := watchRepo(t)
	gitIn(t, root, "checkout", "-q", "-b", "side")
	os.WriteFile(filepath.Join(root, ".envrc"), []byte("echo hi"), 0o644)
	gitIn(t, root, "add", ".envrc")
	gitIn(t, root, "commit", "-qm", "envrc on side")
	gitIn(t, root, "checkout", "-q", "main")
	os.WriteFile(filepath.Join(root, "b.txt"), []byte("b"), 0o644)
	gitIn(t, root, "add", "b.txt")
	gitIn(t, root, "commit", "-qm", "b")
	w := gitWatchStart(root)
	gitIn(t, root, "merge", "-q", "--no-ff", "--no-edit", "side")
	out := captureStderr(t, func() { w.report(true) })
	wantAll(t, out, "1 new commit(s)", "committed a protected path: .envrc") // one commit, not one per parent
}

// Review finding 5: over the limits the report says the check is incomplete, and the stored start state stays for the next start.
func TestGitWatchSaysWhenALimitWasReached(t *testing.T) {
	defer func(c int, o int64) { gitWatchMaxCommits, gitWatchMaxOutput = c, o }(gitWatchMaxCommits, gitWatchMaxOutput)
	t.Run("commits", func(t *testing.T) {
		root := watchRepo(t)
		gitWatchMaxCommits = 3
		w := gitWatchStart(root)
		os.WriteFile(filepath.Join(root, ".envrc"), []byte("echo hi"), 0o644)
		gitIn(t, root, "add", ".envrc")
		gitIn(t, root, "commit", "-qm", "envrc")
		for i := 0; i < 4; i++ {
			gitIn(t, root, "commit", "-q", "--allow-empty", "-m", "more")
		}
		file := w.file
		out := captureStderr(t, func() { w.report(true) })
		wantAll(t, out, "INCOMPLETE", "more than 3 new commits")
		if _, err := os.Stat(file); err != nil {
			t.Error("the stored start state is gone after an incomplete check")
		}
	})
	t.Run("output", func(t *testing.T) {
		root := watchRepo(t)
		gitWatchMaxCommits = 200
		w := gitWatchStart(root)
		for i := 0; i < 30; i++ {
			os.WriteFile(filepath.Join(root, fmt.Sprintf("file-with-a-long-name-%02d.txt", i)), []byte("x"), 0o644)
		}
		gitIn(t, root, "add", ".")
		gitWatchMaxOutput = 300 // the index listing is longer
		file := w.file
		out := captureStderr(t, func() { w.report(true) })
		wantAll(t, out, "INCOMPLETE", "index not read in full")
		if _, err := os.Stat(file); err != nil {
			t.Error("the stored start state is gone after an incomplete check")
		}
	})
	t.Run("complete", func(t *testing.T) {
		root := watchRepo(t)
		gitWatchMaxCommits, gitWatchMaxOutput = 200, 1<<20
		w := gitWatchStart(root)
		gitIn(t, root, "commit", "-q", "--allow-empty", "-m", "one")
		file := w.file
		out := captureStderr(t, func() { w.report(true) })
		if strings.Contains(out, "INCOMPLETE") {
			t.Errorf("a complete check says incomplete:\n%s", out)
		}
		if _, err := os.Stat(file); err == nil {
			t.Error("the stored start state stays after a complete check")
		}
	})
}

// Review finding 7: the end report waits for the requests that are running (they hold the folder lock), and a write that comes after it started is refused: what a running
// request stages is in the report and restored, and nothing can stage after it.
func TestGitWatchEndReportWaitsForRunningRequestsAndStopsWrites(t *testing.T) {
	root := watchRepo(t)
	s := &shareClient{root: root}
	s.git = gitWatchStart(root)
	running := s.lockFolder(false, time.Second) // a request in flight
	if running == nil {
		t.Fatal("no lock")
	}
	done := make(chan string)
	go func() { done <- captureStderr(t, s.unregister) }()
	time.Sleep(200 * time.Millisecond)
	select {
	case <-done:
		t.Fatal("the end report did not wait for the running request")
	default:
	}
	// the running request finishes its write
	os.WriteFile(filepath.Join(root, ".envrc"), []byte("echo hi"), 0o644)
	gitIn(t, root, "add", ".envrc")
	running()
	out := <-done
	wantAll(t, out, "staged a protected path: .envrc")
	if gitIn(t, root, "ls-files", "-s", ".envrc") != "" {
		t.Error("the staged path was not restored")
	}
	if r := s.handle(davJob{Method: "PUT", Path: "/x.txt", Body: []byte("x")}, ""); r.Status != 503 {
		t.Errorf("a write after the end report: status %d", r.Status)
	}
	if r := s.handle(davJob{Method: "GET", Path: "/a.txt"}, ""); r.Status != 200 {
		t.Errorf("a read after the end report: status %d", r.Status)
	}
}

// The other direction: a new commit with a protected path is hidden by a replacement of it with an old, harmless commit (git shows the replacement everywhere).
func TestGitWatchSeesACommitThatIsReplacedByAnotherOne(t *testing.T) {
	root := watchRepo(t)
	w := gitWatchStart(root)
	old := strings.TrimSpace(gitIn(t, root, "rev-parse", "HEAD"))
	os.WriteFile(filepath.Join(root, ".envrc"), []byte("echo hi"), 0o644)
	gitIn(t, root, "add", ".envrc")
	gitIn(t, root, "commit", "-qm", "envrc")
	n := strings.TrimSpace(gitIn(t, root, "rev-parse", "HEAD"))
	gitIn(t, root, "update-ref", "refs/replace/"+n, old)
	out := captureStderr(t, func() { w.report(true) })
	wantAll(t, out, "1 new commit(s)", "committed a protected path: .envrc", "replacement ref ("+n+")")
}

// Round 2, H3: the drain timed out (a request holds the folder lock): nothing is restored without the lock, the report says so, the baseline stays.
func TestGitWatchDrainTimeoutRestoresNothing(t *testing.T) {
	defer func(d time.Duration) { gitWatchDrainWait = d }(gitWatchDrainWait)
	gitWatchDrainWait = 150 * time.Millisecond
	root := watchRepo(t)
	s := &shareClient{root: root}
	s.git = gitWatchStart(root)
	file := s.git.file
	running := s.lockFolder(false, time.Second) // a request that outlasts the wait
	if running == nil {
		t.Fatal("no lock")
	}
	defer running()
	os.WriteFile(filepath.Join(root, ".envrc"), []byte("echo hi"), 0o644)
	gitIn(t, root, "add", ".envrc")
	out := captureStderr(t, s.unregister)
	wantAll(t, out, "INCOMPLETE", "requests of the agent were still running", "a protected path looks staged: .envrc", "NOT removed from the index")
	if gitIn(t, root, "ls-files", "-s", ".envrc") == "" {
		t.Error("the index was restored without the lock")
	}
	if _, err := os.Stat(file); err != nil {
		t.Error("the baseline is gone")
	}
}

// H4: the repository cannot be read at the end (a damaged packed-refs): a warning, and the baseline stays; a folder that was never watchable stays silent.
func TestGitWatchUnreadableAtTheEndWarnsAndKeepsTheBaseline(t *testing.T) {
	root := watchRepo(t)
	w := gitWatchStart(root)
	file := w.file
	os.WriteFile(filepath.Join(root, ".git", "packed-refs"), []byte("garbage without a header\nnot a ref\n"), 0o644)
	out := captureStderr(t, func() { w.report(true) })
	wantAll(t, out, "INCOMPLETE", "could not be read at the end")
	if _, err := os.Stat(file); err != nil {
		t.Error("the baseline is gone after an unreadable end state")
	}
}

// H5: the recovery at the next start removes the baseline only after a complete comparison.
func TestGitWatchRecoveryKeepsAnIncompleteBaseline(t *testing.T) {
	defer func(c int) { gitWatchMaxCommits = c }(gitWatchMaxCommits)
	root := watchRepo(t)
	w := gitWatchStart(root)
	for i := 0; i < 5; i++ {
		gitIn(t, root, "commit", "-q", "--allow-empty", "-m", "more")
	}
	gone := func(int, string) bool { return false }
	gitWatchMaxCommits = 3
	out := strings.Join(gitWatchRecover(root, os.Getpid()+1, gone), "\n")
	wantAll(t, out, "from the previous run that did not finish", "INCOMPLETE")
	if _, err := os.Stat(w.file); err != nil {
		t.Fatal("the baseline was removed after an incomplete comparison")
	}
	gitWatchMaxCommits = 200
	out = strings.Join(gitWatchRecover(root, os.Getpid()+1, gone), "\n")
	if strings.Contains(out, "INCOMPLETE") || !strings.Contains(out, "5 new commit(s)") {
		t.Errorf("complete recovery:\n%s", out)
	}
	if _, err := os.Stat(w.file); err == nil {
		t.Error("the baseline stays after a complete comparison")
	}
}

// H6: an existing tag moved between two commits that were both in the history at the start.
func TestGitWatchReportsAMovedTag(t *testing.T) {
	root := watchRepo(t)
	gitIn(t, root, "commit", "-q", "--allow-empty", "-m", "second")
	gitIn(t, root, "tag", "v1", "HEAD~1")
	w := gitWatchStart(root)
	gitIn(t, root, "tag", "-f", "v1", "HEAD")
	out := captureStderr(t, func() { w.report(true) })
	wantAll(t, out, "moved tags: v1")
}

// Round 3, G-1: the Go watch has the pins the Swift one has (shared list in git-watch.json: both assert it).
func TestGitWatchHasTheSharedPins(t *testing.T) {
	var f struct{ Pins struct{ Args, Env []string } }
	if err := json.Unmarshal(srcFile(t, "testdata/git-watch.json"), &f); err != nil || len(f.Pins.Args) == 0 {
		t.Fatal(err)
	}
	null := func(s string) string { return strings.ReplaceAll(s, "{null}", os.DevNull) }
	for _, a := range f.Pins.Args {
		a = null(a)
		ok := slices.Contains(gitWatchPins, a)
		for i := 1; i < len(gitWatchPins) && !ok; i++ {
			ok = gitWatchPins[i-1] == "-c" && gitWatchPins[i] == a
		}
		if !ok {
			t.Errorf("the watch does not run git with %q", a)
		}
	}
	for _, e := range f.Pins.Env {
		if !slices.Contains(gitWatchEnv, null(e)) {
			t.Errorf("the watch does not run git with %q in its environment", e)
		}
	}
}

// G-1: git gc needs to write the commit graph, so objects/info/commit-graph* stays writable for the agent and the pins above are what keep a forged one from the watch.
// Proved with real git: with the commit graph unwritable, commit, repack and fetch work and gc fails.
func TestGitNeedsTheCommitGraphOnlyForGc(t *testing.T) {
	root := watchRepo(t)
	gitIn(t, root, "commit", "-q", "--allow-empty", "-m", "two")
	if err := os.MkdirAll(filepath.Join(root, ".git", "objects", "info", "commit-graph.lock"), 0o755); err != nil { // a lock git cannot take: the file is not writable
		t.Fatal(err)
	}
	gitIn(t, root, "commit", "-q", "--allow-empty", "-m", "three")
	gitIn(t, root, "repack", "-adq")
	gitIn(t, root, "fetch", "-q", ".")
	cmd := exec.Command("git", "gc", "-q")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	if err := cmd.Run(); err == nil {
		t.Error("gc worked without writing the commit graph: then the cache files could be refused")
	}
	if protectedPathFor(".git/objects/info/commit-graph", true) {
		t.Error("the commit graph is refused although git gc needs it")
	}
}

// G-3: paths with a quote or a newline arrive raw (log -z); G-5: a reused process id is not a running client.
func TestGitWatchNamesParseRaw(t *testing.T) {
	var f struct {
		Logs []struct {
			Name  string
			Out   string
			Files []string
		}
	}
	if err := json.Unmarshal(srcFile(t, "testdata/git-watch.json"), &f); err != nil || len(f.Logs) == 0 {
		t.Fatal(err)
	}
	for _, l := range f.Logs {
		if got := parseGitNames(l.Out); !slices.Equal(got, l.Files) && !(len(got) == 0 && len(l.Files) == 0) {
			t.Errorf("%s: %q, want %q", l.Name, got, l.Files)
		}
	}
}

func TestGitWatchSeesAProtectedPathWithAQuoteInItsName(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("a quote cannot be in a Windows file name")
	}
	root := watchRepo(t)
	w := gitWatchStart(root)
	dir := filepath.Join(root, `a"b`, ".claude")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "settings.json"), []byte("{}"), 0o644)
	gitIn(t, root, "add", ".")
	gitIn(t, root, "commit", "-qm", "quoted")
	out := captureStderr(t, func() { w.report(true) })
	wantAll(t, out, `a"b/.claude/settings.json`)
}

func TestGitWatchRecoveryDoesNotTrustAReusedProcessID(t *testing.T) {
	root := watchRepo(t)
	w := gitWatchStart(root)
	gitIn(t, root, "branch", "feature")
	b, _ := os.ReadFile(w.file)
	if !strings.Contains(string(b), `"started"`) && gitWatchProcStart(os.Getpid()) != "" {
		t.Fatal("the start time of the process is not stored")
	}
	// the file's process id is alive, but it is another process than the one that wrote it
	other := strings.Replace(string(b), `"started":"`, `"started":"other-`, 1)
	os.WriteFile(w.file, []byte(other), 0o600)
	out := strings.Join(gitWatchRecover(root, os.Getpid()+1, gitWatchPidAlive), "\n")
	if gitWatchProcStart(os.Getpid()) != "" {
		wantAll(t, out, "new branches: feature")
	}
}

// G-5: a start state that cannot be stored is said at the start; recovery runs at the start of every share.
func TestGitWatchSaysWhenTheStartStateIsNotStored(t *testing.T) {
	root := watchRepo(t)
	t.Setenv("HOME", filepath.Join(root, "a-file-not-a-folder"))
	os.WriteFile(filepath.Join(root, "a-file-not-a-folder"), nil, 0o644)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "a-file-not-a-folder", "config"))
	t.Setenv("APPDATA", filepath.Join(root, "a-file-not-a-folder", "appdata"))
	out := captureStderr(t, func() { gitWatchStart(root) })
	wantAll(t, out, "could not be stored")
}

func TestShareStartRecoversForEveryMode(t *testing.T) {
	root := watchRepo(t)
	w := gitWatchStart(root)
	gitIn(t, root, "branch", "feature")
	_ = w
	gitWatchGone := func(int, string) bool { return false }
	_ = gitWatchGone
	s := &shareClient{root: root, sync: true}
	// a killed client's file has this process's id: pretend another one by moving it to a dead pid name
	dead := strings.Replace(w.file, "-"+strconv.Itoa(os.Getpid())+".json", "-999999.json", 1)
	os.Rename(w.file, dead)
	out := captureStderr(t, func() { s.start(0) })
	wantAll(t, out, "from the previous run that did not finish", "new branches: feature")
}

// M1 (Go): the guard compares names without case (normName); the rule globs of Claude Code also get a form that matches any case.
func TestCaseVariantsOfTheDenyRules(t *testing.T) {
	for _, p := range []string{".GIT/config", ".Git/hooks/x", "sub/.GIT/CONFIG", "sub/.Git/Hooks/pre-commit", ".CLAUDE/settings.json", ".Claude/Hooks/x"} {
		if !protectedPathFor(p, true) {
			t.Errorf("the guard lets %q through", p)
		}
	}
	rules := withCaseVariants([]string{"Edit(//d/.git/config)", "Edit(//d/.git/config/**)", "Edit(//d/**/.git)", "Edit(//d/.claude/**)", "Read(//d/x/**)"})
	toRe := func(g string) *regexp.Regexp {
		g = strings.TrimSuffix(strings.TrimPrefix(g, "Edit("), ")")
		g = regexp.QuoteMeta(g)
		g = strings.ReplaceAll(strings.ReplaceAll(g, `\*\*`, `.*`), `\*`, `[^/]*`)
		g = strings.NewReplacer(`\[`, `[`, `\]`, `]`).Replace(g)
		return regexp.MustCompile("^" + g + "$")
	}
	for _, c := range []struct{ path string }{{"//d/.GIT/config"}, {"//d/.Git/CONFIG"}, {"//d/.git/Config/x"}, {"//d/sub/.GIT"}, {"//d/.CLAUDE/settings.json"}, {"//d/.Claude/a/b"}} {
		hit := false
		for _, r := range rules {
			if strings.HasPrefix(r, "Edit(") && toRe(r).MatchString(c.path) {
				hit = true
			}
		}
		if !hit {
			t.Errorf("no rule matches %s in %q", c.path, rules)
		}
	}
	if !slices.Contains(rules, "Edit(//d/.git/config)") || !slices.Contains(rules, "Read(//d/x/**)") || len(rules) != 5+4 {
		t.Errorf("the originals are kept and only the rules that name .git or .claude get a variant: %q", rules)
	}
}

// Astra 5b90d47 finding 1: a name that begins with the commit-header marker is a name, not a commit (also as a later name of a commit).
func TestGitWatchSeesAProtectedNameThatStartsWithTheMarker(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("@@ in a directory name is fine on Windows, but keep the test to the Unix shells")
	}
	root := watchRepo(t)
	w := gitWatchStart(root)
	os.MkdirAll(filepath.Join(root, "@@payload", ".claude"), 0o755)
	os.WriteFile(filepath.Join(root, "00.txt"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(root, "@@payload", ".claude", "settings.json"), []byte("{}"), 0o644)
	gitIn(t, root, "add", ".")
	gitIn(t, root, "commit", "-qm", "marker")
	out := captureStderr(t, func() { w.report(true) })
	wantAll(t, out, "1 new commit(s)", "committed a protected path: @@payload/.claude/settings.json")
}

// 0.7.1: a symbolic link committed as a regular file (what a mounted folder does to it) and an executable file made plain are named by the real git, in the commit and in the index.
func TestGitWatchNamesATypeChangeOfARealRepository(t *testing.T) {
	isolateGitWatchState(t)
	if _, err := exec.LookPath("git"); err != nil || runtime.GOOS == "windows" {
		t.Skip("needs git and symbolic links")
	}
	root := t.TempDir()
	gitIn(t, root, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(root, "a.txt"), []byte("a"), 0o644)
	os.WriteFile(filepath.Join(root, "run.sh"), []byte("#!/bin/sh\n"), 0o755)
	os.Symlink("a.txt", filepath.Join(root, "link"))
	gitIn(t, root, "add", "-A")
	gitIn(t, root, "commit", "-qm", "init")
	w := gitWatchStart(root)
	if w == nil {
		t.Fatal("no watch")
	}
	os.Remove(filepath.Join(root, "link"))
	os.WriteFile(filepath.Join(root, "link"), []byte("a"), 0o644) // as the mount shows it
	os.Chmod(filepath.Join(root, "run.sh"), 0o644)
	gitIn(t, root, "-c", "core.fileMode=true", "add", "-A")
	gitIn(t, root, "commit", "-qm", "agent")
	os.Remove(filepath.Join(root, "a.txt"))
	os.Symlink("link", filepath.Join(root, "a.txt")) // staged, not committed
	gitIn(t, root, "add", "a.txt")
	lines, _ := w.compare(nil, false)
	got := strings.Join(lines, "\n")
	for _, want := range []string{"link 120000->100644", "run.sh 100755->100644", "a.txt 100644->120000"} {
		if !strings.Contains(got, want) {
			t.Errorf("the report lacks %q:\n%s", want, got)
		}
	}
}

// 0.7.1: the client opens commits through the mount only for a repository without tracked symbolic links; the gateway reads the flag as closed when it is missing.
func TestDirectGitOpenSaysNoForATrackedSymlink(t *testing.T) {
	isolateGitWatchState(t)
	if _, err := exec.LookPath("git"); err != nil || runtime.GOOS == "windows" {
		t.Skip("needs git and symbolic links")
	}
	root := t.TempDir()
	if open, why := directGitOpen(root); open || why != "" {
		t.Errorf("a folder without a repository: %v %q", open, why)
	}
	gitIn(t, root, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(root, "a.txt"), []byte("a"), 0o644)
	gitIn(t, root, "add", "-A")
	gitIn(t, root, "commit", "-qm", "init")
	if open, why := directGitOpen(root); !open || why != "" {
		t.Errorf("a plain repository: %v %q", open, why)
	}
	os.Symlink("a.txt", filepath.Join(root, "link"))
	gitIn(t, root, "add", "-A")
	gitIn(t, root, "commit", "-qm", "link")
	if open, why := directGitOpen(root); open || !strings.Contains(why, "tracks symbolic links") {
		t.Errorf("a repository with a tracked symbolic link: %v %q", open, why)
	}
}

func TestParseGitRawReportsLinksAndExecutablesAddedOrDeleted(t *testing.T) {
	out := ":120000 100644 aaa bbb T\x00link\x00:000000 100644 0000 eee A\x00new.txt\x00:100644 000000 hhh 0000 D\x00plain.txt\x00:120000 000000 iii 0000 D\x00old-link\x00:000000 100755 0000 jjj A\x00new.sh\x00"
	files, modes := parseGitRaw(out)
	if len(files) != 5 || !slices.Equal(modes, []string{"link 120000->100644", "old-link 120000->000000", "new.sh 000000->100755"}) {
		t.Errorf("%v %v", files, modes)
	}
}

// G1: a cut index listing, a refs read and a history read over the output limit keep INCOMPLETE but cannot switch the protection off; an unresolvable HEAD or no working read can.
func TestGitWatchResetsWhenOnlyTheLimitsWereReached(t *testing.T) {
	defer func(o int64) { gitWatchMaxOutput = o }(gitWatchMaxOutput)
	stage := func(t *testing.T, root string) {
		os.WriteFile(filepath.Join(root, ".envrc"), []byte("echo hi"), 0o644)
		gitIn(t, root, "add", ".envrc")
	}
	t.Run("listing cut", func(t *testing.T) {
		root := watchRepo(t)
		w := gitWatchStart(root)
		for i := 0; i < 30; i++ {
			os.WriteFile(filepath.Join(root, fmt.Sprintf("file-with-a-long-name-%02d.txt", i)), []byte("x"), 0o644)
		}
		stage(t, root)
		gitIn(t, root, "add", ".")
		gitWatchMaxOutput = 300
		out := captureStderr(t, func() { w.report(true) })
		wantAll(t, out, "INCOMPLETE", "index not read in full", "removed from the index for you")
		if gitIn(t, root, "ls-files", "-s", ".envrc") != "" {
			t.Errorf("still in the index:\n%s", out)
		}
	})
	t.Run("refs cut", func(t *testing.T) {
		root := watchRepo(t)
		w := gitWatchStart(root)
		for i := 0; i < 12; i++ {
			gitIn(t, root, "branch", fmt.Sprintf("a-branch-with-a-long-name-%d", i))
		}
		stage(t, root)
		gitWatchMaxOutput = 600
		out := captureStderr(t, func() { w.report(true) })
		wantAll(t, out, "refs not read in full", "removed from the index for you")
		if gitIn(t, root, "ls-files", "-s", ".envrc") != "" {
			t.Errorf("still in the index:\n%s", out)
		}
	})
	t.Run("no read works", func(t *testing.T) {
		root := watchRepo(t)
		w := gitWatchStart(root)
		stage(t, root)
		gitWatchMaxOutput = 5
		out := captureStderr(t, func() { w.report(true) })
		wantAll(t, out, "may be staged that are not listed")
		if gitIn(t, root, "ls-files", "-s", ".envrc") == "" {
			t.Errorf("the watcher took a path out of the index:\n%s", out)
		}
	})
	t.Run("HEAD does not resolve", func(t *testing.T) {
		root := watchRepo(t)
		gitWatchMaxOutput = 1 << 20
		w := gitWatchStart(root)
		gitIn(t, root, "branch", "other")
		stage(t, root)
		gitIn(t, root, "symbolic-ref", "HEAD", "refs/heads/ghost")
		out := captureStderr(t, func() { w.report(true) })
		wantAll(t, out, "NOT removed from the index")
		if gitIn(t, root, "ls-files", "-s", ".envrc") == "" {
			t.Errorf("the watcher took a path out of the index:\n%s", out)
		}
	})
}

// G2: a printed command only for a conservative path; a hostile name (cmd.exe, PowerShell) gets none.
func TestGitUnstageWarningPrintsNoCommandForAHostilePath(t *testing.T) {
	for _, hostile := range []string{".githooks/x & calc.exe & rem ", ".githooks/x'; Start-Process calc; '", "-rf", ".githooks/x\ty", ".githooks/é"} {
		w := gitUnstageWarning([]string{hostile, ".envrc"}, false)
		if gitCommandSafe(hostile) || strings.Contains(w, "restore --staged -- '"+hostile) || !strings.Contains(w, "restore --staged -- '.envrc'") || !strings.Contains(w, "No command is printed for 1 path(s)") {
			t.Errorf("%q:\n%s", hostile, w)
		}
	}
	if !gitCommandSafe(".github/workflows/a-b_c.yml") || !gitCommandSafe("a@b+c") {
		t.Error("a plain path is refused")
	}
}

// G3: every name the watcher prints is escaped (the fixture rows assert the lines; this the function).
func TestGitEscEscapesWhatCanRewriteATerminalLine(t *testing.T) {
	for in, want := range map[string]string{
		"plain/name.txt": "plain/name.txt", "a\x1b[2K\r": `a\x1b[2K\x0d`, "x\x1b]52;c;AAAA\x07": `x\x1b]52;c;AAAA\x07`, "a‮b": `a\u{202e}b`, "a b": `a\u{2028}b`,
		`back\slash`: `back\\slash`, "c1\u0085": `c1\x85`, "é": "é", "\xff": `\xff`, "del\x7f": `del\x7f`,
	} {
		if got := gitEsc(in); got != want {
			t.Errorf("gitEsc(%q) = %q, want %q", in, got, want)
		}
	}
}
