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
