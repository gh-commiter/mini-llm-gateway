package main

import (
	"os"
	"path/filepath"
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
