package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestGitignoreNoticeNamesWhatOnlyTheRootGitignoreLeavesOut(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{".omc", "build", "src", ".git"} { // build and .git are the built-in list's, src is not ignored
		os.MkdirAll(filepath.Join(root, d), 0o755)
	}
	os.WriteFile(filepath.Join(root, "secret.env"), []byte("x"), 0o600)
	os.WriteFile(filepath.Join(root, "a.go"), []byte("x"), 0o600)
	os.WriteFile(filepath.Join(root, ".gitignore"), []byte(".omc/\nbuild/\n*.env\ndeep/x.txt\n"), 0o600)
	got := gitignoreHidden(root)
	if strings.Join(got, ",") != ".omc,secret.env" {
		t.Fatal(got)
	}
	if msg := gitignoreNotice(got); msg != "sync: not copied because .gitignore lists them: .omc, secret.env (use direct mode or put what the agent needs in the prompt)" {
		t.Fatal(msg)
	}
	if gitignoreNotice(nil) != "" {
		t.Fatal("a notice without entries")
	}
	if msg := gitignoreNotice([]string{"a", "b", "c", "d", "e", "f"}); !strings.Contains(msg, "a, b, c, d, e, … (use direct") || strings.Contains(msg, "f") {
		t.Fatal(msg)
	}
	// Only the built-in list, or no .gitignore at all: nothing to say.
	os.WriteFile(filepath.Join(root, ".gitignore"), []byte("build/\n"), 0o600)
	if got := gitignoreHidden(root); len(got) != 0 {
		t.Fatal(got)
	}
}

// The shared fixture (client/testdata/sync-gitignore.json, asserted by SyncGitignoreNoticeTests.swift too): what the real push of a sync
// sends to the gateway and what it says, in every mode (read-write, read-only) and folder kind (plain, git repository).
func TestSyncPushHonoursTheRootGitignoreInEveryMode(t *testing.T) {
	data := srcFile(t, "testdata/sync-gitignore.json")
	var fx struct {
		Cases []struct {
			Name   string
			Tree   map[string]string
			Synced []string
			Notice *string
		}
	}
	if err := json.Unmarshal(data, &fx); err != nil {
		t.Fatal(err)
	}
	var manifest []syncEntry
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		manifest = nil
		json.Unmarshal(body, &manifest)
		json.NewEncoder(w).Encode(map[string]any{"need": []string{}, "kept": []string{}})
	}))
	defer srv.Close()
	for _, c := range fx.Cases {
		for _, readOnly := range []bool{false, true} {
			for _, git := range []bool{false, true} {
				root := t.TempDir()
				for p, content := range c.Tree {
					os.MkdirAll(filepath.Dir(filepath.Join(root, filepath.FromSlash(p))), 0o755)
					os.WriteFile(filepath.Join(root, filepath.FromSlash(p)), []byte(content), 0o644)
				}
				if git {
					os.MkdirAll(filepath.Join(root, ".git"), 0o755)
					os.WriteFile(filepath.Join(root, ".git", "HEAD"), []byte("ref: refs/heads/main\n"), 0o644)
				}
				name := c.Name + " ro=" + map[bool]string{true: "yes", false: "no"}[readOnly] + " git=" + map[bool]string{true: "yes", false: "no"}[git]
				first, second := runSyncPush(t, &shareClient{c: connection{URL: srv.URL}, root: root, sync: true, readOnly: readOnly,
					synced: map[string]syncEntry{}, reported: map[string]bool{}})
				var paths []string
				for _, e := range manifest {
					paths = append(paths, e.Path)
				}
				sort.Strings(paths)
				if strings.Join(paths, "\n") != strings.Join(c.Synced, "\n") {
					t.Errorf("%s: sent %v, want %v", name, paths, c.Synced)
				}
				want := ""
				if c.Notice != nil {
					want = "minigw: " + *c.Notice + "\n"
				}
				if first != want {
					t.Errorf("%s: said %q, want %q", name, first, want)
				}
				if second != "" {
					t.Errorf("%s: the second push said %q", name, second)
				}
			}
		}
	}
}

// runSyncPush: the stderr text of the first push and of the second one.
func runSyncPush(t *testing.T, s *shareClient) (string, string) {
	t.Helper()
	old := os.Stderr
	defer func() { os.Stderr = old }()
	push := func() string {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		os.Stderr = w
		ok := s.syncPush("k")
		w.Close()
		os.Stderr = old
		out, _ := io.ReadAll(r)
		if !ok {
			t.Fatal("push failed")
		}
		return string(out)
	}
	return push(), push()
}
