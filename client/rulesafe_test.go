package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// #111 S5: a folder path that cannot go into a permission rule as it is is refused at save and skipped at run without "Allow local tools".
func TestRuleSafe(t *testing.T) {
	for _, ok := range []string{"/Users/a/src", "/Users/a/my project", "/Users/a/x-y_z.1", "/Users/a/ä", "C:/Users/a/src"} {
		if !ruleSafe(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"/a/b)", "/a/(b", "/a/*", "/a/b?", "/a/[x]", "/a/{x}", "/a/ x", "/a/x ", "/a/b,c", "/a/b\nc", "/a/b\\c", "/a/b!", "", " /a"} {
		if ruleSafe(bad) {
			t.Errorf("%q accepted", bad)
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
		for _, g := range gitDenies {
			for _, want := range []string{"Edit(" + d + "/.git/" + g + ")", "Edit(" + d + "/.git/" + g + "/**)", "Edit(" + d + "/**/.git/" + g + ")", "Edit(" + d + "/**/.git/" + g + "/**)"} {
				if !slices.Contains(deny, want) {
					t.Errorf("missing %s", want)
				}
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
		for _, want := range []string{"Edit(" + d + "/.git/modules/**)", "Edit(" + d + "/**/.git/modules/**)", "Edit(" + d + "/.claude/**)", "Edit(" + d + "/**/.claude/**)"} {
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
