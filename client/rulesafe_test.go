package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// #111 S5: a folder path that cannot go into a permission rule as it is is refused at save and skipped at run without "Allow local tools".
func TestRuleSafe(t *testing.T) {
	ok, bad := []string{"/Users/a/src", "/Users/a/my project", "/Users/a/x-y_z.1", "/Users/a/ä"},
		[]string{"/a/b)", "/a/(b", "/a/*", "/a/b?", "/a/[x]", "/a/{x}", "/a/ x", "/a/x ", "/a/b,c", "/a/b\nc", "/a/b!", "", " /a", "/a/"}
	if runtime.GOOS == "windows" {
		// the rule text is `//c/Users/a/src`: only a drive path is expressible; UNC, \\?\, drive-relative, current-drive-relative and a stream colon are not
		ok = []string{`C:\Users\a\src`, `C:/Users/a/my project`, `d:\x-y_z.1`, `C:\Users\a\ä`}
		bad = []string{`C:\a\b)`, `C:\a\(b`, `C:\a\*`, `C:\a\b?`, `C:\a\x `, `C:\a\ x`, `C:\a\b,c`, `C:\a\b!`, ``, ` C:\a`, `C:\a\`,
			`\\server\share\x`, `\\?\C:\x`, `C:proj`, `\Users\a`, `C:\a\b:stream`}
	} else {
		bad = append(bad, "/a/b\\c") // a backslash is not a separator here: it could escape in a rule
	}
	for _, p := range ok {
		if !ruleSafe(p) {
			t.Errorf("%q refused", p)
		}
	}
	for _, p := range bad {
		if ruleSafe(p) {
			t.Errorf("%q accepted", p)
		}
	}
	// what goes into a rule never has a backslash and never changes meaning
	for _, p := range ok {
		if r := ruleAbs(p); strings.Contains(r, `\`) || !strings.HasPrefix(r, "//") {
			t.Errorf("%q -> %q", p, r)
		}
	}
}

func TestUnsafeFolderIsRefusedAtSaveAndSkippedAtRun(t *testing.T) {
	s, _, base := liveServer(t, nil)
	admin := func(method, path, body string) (int, string) {
		return adminNoise(t, s, base, s.admin, method, path, body)
	}
	root := t.TempDir()
	good, bad := filepath.Join(root, "good"), filepath.Join(root, "bad (copy)")
	os.MkdirAll(good, 0o700)
	os.MkdirAll(bad, 0o700)
	j := func(p string) string { return strings.ReplaceAll(strings.ReplaceAll(p, `\`, `\\`), `"`, `\"`) }
	if st, b := admin("PATCH", adminPrefix+"/agents/claude", `{"allowLocalTools":false,"folders":"`+j(good)+","+j(bad)+`"}`); st != 400 || !strings.Contains(b, "permission rule") {
		t.Fatalf("unsafe folder saved: %d %s", st, b)
	}
	if st, b := admin("PATCH", adminPrefix+"/agents/claude", `{"allowLocalTools":true,"folders":"`+j(good)+","+j(bad)+`"}`); st != 200 {
		t.Fatalf("with local tools (sandbox) the folder is fine: %d %s", st, b)
	}
	s.update(func(c *serveConfig) error { c.AllowLocalTools = false; return nil }) // a stored configuration: skipped at run
	got := s.agentFolders(s.conf(), filepath.Join(root, "cwd"))
	for _, f := range got {
		if strings.Contains(f, "bad (copy)") {
			t.Fatalf("an unsafe folder is used: %v", got)
		}
	}
	if len(got) != 1 || !strings.HasSuffix(got[0], "good") {
		t.Fatalf("folders: %v", got)
	}
}

func TestRestrictedModeUsesAnEnforcingPermissionMode(t *testing.T) { // S4
	for m, want := range map[string]string{"dontAsk": "dontAsk", "acceptEdits": "acceptEdits", "auto": "dontAsk", "plan": "dontAsk", "": "dontAsk"} {
		if got := enforcingMode(m); got != want {
			t.Errorf("%q -> %q", m, got)
		}
	}
	s, _ := testServer(t, func(c *serveConfig) { c.PermissionMode = "auto" })
	a := s.agentArgs("/copy", false)
	if i := slices.Index(a, "--permission-mode"); i < 0 || a[i+1] != "dontAsk" {
		t.Fatalf("restricted args use %v", a)
	}
}

// S3/S4 in Go: the restricted branch denies Edit on .git config, hooks, info, commondir and worktrees (top-level and nested), and the mode is refused at save.
func TestRestrictedGoGitDenyAndModeRefusal(t *testing.T) {
	s, _ := testServer(t, nil)
	a := s.agentArgs("/copy", false, []string{"/other"})
	i := slices.Index(a, "--disallowedTools")
	if i < 0 {
		t.Fatal("no deny rules")
	}
	deny := strings.Split(a[i+1], ",")
	for _, d := range []string{"//copy", "//other"} {
		// 0.7.0: one rule set for all of .git (the entries config, hooks, info, commondir, worktrees ... and the nested ones are inside it): the command line of claude stays short
		for _, want := range []string{"Edit(" + d + "/.git/**)", "Edit(" + d + "/**/.git/**)"} {
			if !slices.Contains(deny, want) {
				t.Errorf("missing %s", want)
			}
		}
		if !slices.Contains(deny, "Edit("+d+"/.git)") || !slices.Contains(deny, "Edit("+d+"/**/.git)") {
			t.Errorf("missing .git itself for %s", d)
		}
	}
	ls, _, base := liveServer(t, nil)
	admin := func(body string) (int, string) {
		return adminNoise(t, ls, base, ls.admin, "PATCH", adminPrefix+"/agents/claude", body)
	}
	for _, m := range []string{"auto", "plan"} {
		if st, b := admin(`{"allowLocalTools":false,"permissionMode":"` + m + `"}`); st != 400 || !strings.Contains(b, "does not enforce") {
			t.Errorf("%s saved: %d %s", m, st, b)
		}
	}
	if st, b := admin(`{"allowLocalTools":false,"permissionMode":"acceptEdits"}`); st != 200 {
		t.Errorf("acceptEdits refused: %d %s", st, b)
	}
	if st, b := admin(`{"allowLocalTools":true,"permissionMode":"plan"}`); st != 200 {
		t.Errorf("with local tools the mode is not used: %d %s", st, b)
	}
}

// T2, T3: .git/modules and (with setting sources) .claude are denied to the edit tools in Go too.
func TestRestrictedGoModulesAndClaudeSettingsDenied(t *testing.T) {
	s, _ := testServer(t, func(c *serveConfig) { c.SettingSources = "project" })
	deny := func(a []string) []string { return strings.Split(a[slices.Index(a, "--disallowedTools")+1], ",") }
	got := deny(s.agentArgs("/copy", false, []string{"/other"}))
	for _, d := range []string{"//copy", "//other"} {
		for _, want := range []string{"Edit(" + d + "/.git/**)", "Edit(" + d + "/**/.git/**)", "Edit(" + d + "/.claude/**)", "Edit(" + d + "/**/.claude/**)"} {
			if !slices.Contains(got, want) {
				t.Errorf("missing %s", want)
			}
		}
	}
	s2, _ := testServer(t, nil)
	for _, r := range deny(s2.agentArgs("/copy", false, []string{"/other"})) {
		if strings.Contains(r, "/.claude") {
			t.Errorf("no setting sources, yet %s", r)
		}
	}
}

func TestClaudeSettingsDenyOnlyForProjectOrLocalSources(t *testing.T) {
	for src, want := range map[string]bool{"": false, "user": false, "project": true, "user, local": true, "user,project,local": true} {
		if got := readsFolderSettings(src); got != want {
			t.Errorf("%q: %v", src, got)
		}
	}
}

func TestSubmoduleDeniesAreTheTopLevelListAndGitDirsAreFound(t *testing.T) { // V1, V5, V7
	if slices.Contains(gitDenies, "modules") || !slices.Equal(submoduleDenies, gitDenies) {
		t.Fatal("the submodule list is not the top-level list")
	}
	git := filepath.Join(t.TempDir(), ".git")
	for _, p := range []string{"modules/sub/objects/ab", "modules/sub/modules/deep", "modules/other"} {
		os.MkdirAll(filepath.Join(git, p), 0o700)
	}
	for _, p := range []string{"modules/sub/HEAD", "modules/sub/modules/deep/HEAD", "modules/other/HEAD"} {
		os.WriteFile(filepath.Join(git, p), []byte("ref: refs/heads/main\n"), 0o600)
	}
	got := submoduleGitDirs(git)
	want := []string{filepath.Join(git, "modules/sub"), filepath.Join(git, "modules/sub/modules/deep"), filepath.Join(git, "modules/other")}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
	prof := strings.ReplaceAll(strings.Join(codexTreeProfile([]string{}, []string{git}, nil, false, "/usr/bin"), " "), `\\`, "/") // JSON keys carry the OS separator (escaped): compared portably
	for _, w := range []string{`modules"`, "modules/sub", "modules/sub/commondir", "modules/sub/worktrees", "modules/sub/hooks"} {
		if !strings.Contains(prof, w) {
			t.Errorf("profile lacks %s: %s", w, prof)
		}
	}
}

func TestWebToolEntriesAndSettingSources(t *testing.T) { // V6, V8
	s, _ := testServer(t, func(c *serveConfig) {
		c.Tools = "WebFetch(domain:x.org) Bash,WebFetch(domain:ok.org),WebSearch,Read Edit"
	})
	a := s.agentArgs("/copy", false, []string{})
	if got := a[slices.Index(a, "--tools")+1]; got != "WebFetch,WebFetch(domain:ok.org),WebSearch" {
		t.Errorf("tools %q", got)
	}
	for src, want := range map[string]bool{"": false, "user": false, " user ": false, "user,user": false, "project": true, "project\n": true, "local": true, "user, local": true, "Project": true, "bogus": true} {
		if got := readsFolderSettings(src); got != want {
			t.Errorf("%q: %v", src, got)
		}
	}
}

// P1 (0.7.0): the sandbox's denyWrite of a Claude CLI run with Allow local tools holds the Mac's list inside `.git`, and the entry itself where it is a gitdir: file.
func TestSandboxDenyWriteHoldsTheGitListAndAGitdirFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no sandbox settings on Windows")
	}
	root := t.TempDir()
	dir, wtree, bare := filepath.Join(root, "repo"), filepath.Join(root, "linked"), filepath.Join(root, "inrepo")
	for _, d := range []string{filepath.Join(dir, ".git", "objects", "info"), filepath.Join(dir, ".git", "refs"), wtree, bare} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(wtree, ".git"), []byte("gitdir: ../repo/.git/worktrees/linked\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, _ := testServer(t, func(c *serveConfig) { c.AllowLocalTools = true })
	a := s.agentArgs(dir, false, []string{wtree, bare})
	var st struct {
		Sandbox struct{ Filesystem struct{ DenyWrite []string } }
	}
	if err := json.Unmarshal([]byte(a[slices.Index(a, "--settings")+1]), &st); err != nil {
		t.Fatal(err)
	}
	deny := st.Sandbox.Filesystem.DenyWrite
	for _, g := range []string{"config", "hooks", "info", "commondir", "worktrees", "grafts", "rebase-merge", "rebase-apply", "sequencer", "remotes", "branches", "objects/info/alternates", "refs/replace", "modules"} {
		if !slices.Contains(deny, filepath.Join(dir, ".git", filepath.FromSlash(g))) {
			t.Errorf("missing %s in %v", g, deny)
		}
		if slices.Contains(deny, filepath.Join(wtree, ".git", filepath.FromSlash(g))) {
			t.Errorf("%s under a .git file", g)
		}
	}
	// Linux binds /dev/null over a denied path that does not exist: shallow (breaks git gc) only where it exists, logs/refs/replace (a missing logs breaks the first commit) only where logs/refs does
	for _, g := range []string{"shallow", "logs/refs/replace"} {
		if slices.Contains(deny, filepath.Join(dir, ".git", filepath.FromSlash(g))) {
			t.Errorf("%s denied while it and its parent are missing", g)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "shallow"), []byte("0000000000000000000000000000000000000000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	b := s.agentArgs(dir, false, []string{wtree, bare})
	if !strings.Contains(b[slices.Index(b, "--settings")+1], filepath.Join(dir, ".git", "shallow")) {
		t.Error("an existing shallow file is writable")
	}
	if !slices.Contains(deny, filepath.Join(wtree, ".git")) {
		t.Error("a .git file is writable")
	}
	for _, d := range []string{dir, bare} { // a real directory is written by git itself; a missing .git is not bound as /dev/null
		if slices.Contains(deny, filepath.Join(d, ".git")) {
			t.Errorf("%s: the .git entry is denied", d)
		}
	}
	if tools := a[slices.Index(a, "--disallowedTools")+1]; !strings.Contains(tools, "Edit(//"+strings.TrimPrefix(filepath.ToSlash(dir), "/")+"/.git/**)") {
		t.Error("the file tools may edit .git/shallow")
	}
}

// 0.7.0: the rules of the folders ride on the command line of claude, and Windows takes 32,767 characters (0a8e83b put one rule per git entry, tool and case on it and broke every
// Claude CLI start there). A pure function of its inputs, so it runs on every platform: the session folder and three allowed folders with long Windows paths stay under the ceiling the
// launch enforces (maxArgvWindows: more or longer folders are refused with a message), and the size does not depend on how many git entries are closed.
func TestAgentArgsStayUnderTheWindowsCommandLineLimit(t *testing.T) {
	long := func(i int) string {
		return fmt.Sprintf(`C:\Users\someone\Development\projects\client-%02d\a-long-folder-name-%s`, i, strings.Repeat("x", 60))
	}
	for _, allow := range []bool{true, false} {
		s, _ := testServer(t, func(c *serveConfig) { c.AllowLocalTools = allow; c.Tools = "Read,Edit,Write,Bash,Grep,Glob" })
		session := `C:\Users\someone\Development\projects\session-` + strings.Repeat("s", 60)
		n := argvLen("claude.exe", s.agentArgs(session, false, []string{long(1), long(2), long(3)}))
		t.Logf("allowLocalTools=%v: %d characters (ceiling %d, Windows %d)", allow, n, maxArgvWindows, 32767)
		if n > maxArgvWindows {
			t.Errorf("allowLocalTools=%v: %d characters, over the ceiling %d", allow, n, maxArgvWindows)
		}
		if one := argvLen("claude.exe", s.agentArgs(session, false, []string{})); one > 8000 {
			t.Errorf("the session folder alone takes %d characters", one)
		}
	}
}
