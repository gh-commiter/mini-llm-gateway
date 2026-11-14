package main

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Direct mode (WebDAV) lets the agent write git's own data and nothing that runs code or redirects git; sync keeps every .git
// refused. The table is client/testdata/gitguard.json, asserted by Swift too (Tests/RouterCoreTests/GitGuardTests.swift).
type gitGuardCases struct {
	Paths [][]any `json:"paths"`
	Setup []string
	Ops   []struct {
		M, P, D string
		Ok      bool
	}
}

func loadGitGuard(t *testing.T) gitGuardCases {
	t.Helper()
	var c gitGuardCases
	if err := json.Unmarshal(srcFile(t, "testdata/gitguard.json"), &c); err != nil || len(c.Paths) == 0 || len(c.Ops) == 0 {
		t.Fatalf("testdata/gitguard.json: %v", err)
	}
	return c
}

func TestGitGuardPaths(t *testing.T) {
	for _, r := range loadGitGuard(t).Paths {
		p, direct, sync := r[0].(string), r[1].(bool), r[2].(bool)
		if got := !protectedPathFor(p, true); got != direct {
			t.Errorf("direct %q: writable %v, want %v", p, got, direct)
		}
		if got := !protectedPath(p); got != sync {
			t.Errorf("sync %q: writable %v, want %v", p, got, sync)
		}
	}
}

// buildGitGuardTree creates the setup of the fixture: d:dir, f:file, l:link=target.
func buildGitGuardTree(t *testing.T, root string, setup []string) {
	t.Helper()
	for _, e := range setup {
		kind, rest, _ := strings.Cut(e, ":")
		switch kind {
		case "d":
			if err := os.MkdirAll(filepath.Join(root, filepath.FromSlash(rest)), 0o755); err != nil {
				t.Fatal(err)
			}
		case "f":
			if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(rest)), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		case "l":
			name, target, _ := strings.Cut(rest, "=")
			dirLink(t, filepath.FromSlash(target), filepath.Join(root, filepath.FromSlash(name)))
		}
	}
}

func TestGitGuardWebDAV(t *testing.T) {
	c := loadGitGuard(t)
	enc := func(rel string) string {
		parts := strings.Split(rel, "/")
		for i, p := range parts {
			parts[i] = url.PathEscape(p)
		}
		return "/s/" + strings.Join(parts, "/")
	}
	for _, o := range c.Ops {
		root := t.TempDir()
		buildGitGuardTree(t, root, c.Setup)
		h := map[string]string{}
		if o.D != "" {
			h["destination"] = enc(o.D)
		}
		r := davHandle(o.M, enc(o.P), h, []byte("body"), root, "/s")
		if refused := r.Status == 403; refused == o.Ok {
			t.Errorf("%s %s %s: status %d, want ok=%v", o.M, o.P, o.D, r.Status, o.Ok)
		}
	}
}

// git's own lock-file-then-rename: index.lock → index works, config.tmp → config does not; sync keeps both refused.
func TestGitGuardSyncStaysClosed(t *testing.T) {
	root := t.TempDir()
	buildGitGuardTree(t, root, []string{"d:.git", "f:.git/index.lock"})
	rooted, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer rooted.Close()
	if err := (mutationRoot{Root: rooted}).Rename(".git/index.lock", ".git/index"); err == nil {
		t.Error("sync mutation renamed inside .git")
	}
	if err := (mutationRoot{Root: rooted, git: true}).Rename(".git/index.lock", ".git/index"); err != nil {
		t.Errorf("direct mutation refused index.lock → index: %v", err)
	}
	if err := syncWrite(rooted, ".git/refs", []byte("x"), 0, false, false); err == nil {
		t.Error("sync wrote under .git")
	}
}
