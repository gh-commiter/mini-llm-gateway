//go:build !windows

package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The file is opened once, without following a link, and only a regular file is read.
func TestReadNoteFileNoFollow(t *testing.T) {
	dir := t.TempDir()
	secret := filepath.Join(dir, "secret.md")
	if e := os.WriteFile(secret, []byte("secret"), 0600); e != nil {
		t.Fatal(e)
	}
	link := filepath.Join(dir, "link.md")
	if e := os.Symlink(secret, link); e != nil {
		t.Logf("no symlinks: %v", e)
	} else if _, e = readNoteFile(link); e == nil {
		t.Fatal("followed a link")
	}
	fifo := filepath.Join(dir, "fifo.md")
	if e := syscall.Mkfifo(fifo, 0600); e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() {
		_, e := readNoteFile(fifo)
		done <- e
	}()
	select {
	case e := <-done:
		if e == nil {
			t.Fatal("read a fifo")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("blocked on a fifo")
	}
	if data, e := readNoteFile(secret); e != nil || string(data) != "secret" {
		t.Fatal(e, data)
	}
	big := filepath.Join(dir, "big.md")
	if e := os.WriteFile(big, make([]byte, 96<<10), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := readNoteFile(big); e == nil {
		t.Fatal("read past the limit")
	}
}

func TestFolderImportHeldRootAndTraversalLimits(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "child"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "child", "note.md"), []byte("note"), 0600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	files, err := collectNoteFiles(root)
	if err != nil || len(files) != 1 {
		t.Fatal(files, err)
	}
	// Swap a parent after enumeration; the held root cannot read the external secret.
	outside := t.TempDir()
	if err = os.WriteFile(filepath.Join(outside, "note.md"), []byte("secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.RemoveAll(filepath.Join(dir, "child")); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(outside, filepath.Join(dir, "child")); err != nil {
		t.Fatal(err)
	}
	if f, err := openRootNote(root, files[0]); err == nil {
		f.Close()
		t.Fatal("escaped the held root")
	}
	if err = os.Remove(filepath.Join(dir, "child")); err != nil {
		t.Fatal(err)
	}
	deep := dir
	for i := 0; i <= maxImportDepth; i++ {
		deep = filepath.Join(deep, "d")
		if err = os.Mkdir(deep, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = collectNoteFiles(root); err == nil {
		t.Fatal("accepted deep empty directories")
	}
	if err = os.RemoveAll(filepath.Join(dir, "d")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i <= maxImportFiles; i++ {
		if err = os.WriteFile(filepath.Join(dir, fmt.Sprintf("%d.txt", i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = collectNoteFiles(root); err == nil {
		t.Fatal("accepted too many non-Markdown entries")
	}
}

// A folder import refuses a link before posting anything.
func TestMarkdownImportRefusesFolderLinks(t *testing.T) {
	dir := t.TempDir()
	if e := os.WriteFile(filepath.Join(dir, "note.md"), []byte("body"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink(filepath.Join(dir, "note.md"), filepath.Join(dir, "link.md")); e != nil {
		t.Fatal(e)
	}
	var posts int
	env := &projectEnv{out: io.Discard, connect: func() (connection, string) { return connection{}, "" },
		do: func(_ connection, m, p string, _ map[string]string, _ []byte) (int, []byte, http.Header) {
			if m == "GET" {
				return 200, []byte(`{"id":1}`), nil
			}
			posts++
			return 200, []byte(`{"action":"create","key":"path:note.md"}`), nil
		}}
	if e := env.kb([]string{"import", "demo", dir}); e == nil || !strings.Contains(e.Error(), "not a regular file") {
		t.Fatal(e)
	}
	if posts != 0 {
		t.Fatal(posts)
	}
}

func exportEnv() *projectEnv {
	return &projectEnv{out: io.Discard, connect: func() (connection, string) { return connection{}, "" },
		do: func(_ connection, _, p string, _ map[string]string, _ []byte) (int, []byte, http.Header) {
			switch {
			case p == apiProjects+"/demo":
				return 200, []byte(`{"id":1}`), nil
			case strings.Contains(p, "/notes?"):
				return 200, []byte(`{"notes":[{"id":5}]}`), nil
			}
			return 200, []byte(`{"id":5,"title":"T","body":"secret"}`), nil
		}}
}

// Swapping the destination for a link after the check must not send notes elsewhere.
func TestKBExportSurvivesDirectorySwap(t *testing.T) {
	tmp := t.TempDir()
	out, moved, victim := filepath.Join(tmp, "out"), filepath.Join(tmp, "out-moved"), filepath.Join(tmp, "victim")
	if e := os.Mkdir(victim, 0700); e != nil {
		t.Fatal(e)
	}
	exportHook = func() {
		if e := os.Rename(out, moved); e != nil {
			t.Fatal(e)
		}
		if e := os.Symlink(victim, out); e != nil {
			t.Fatal(e)
		}
	}
	defer func() { exportHook = nil }()
	if e := exportEnv().kb([]string{"export", "demo", out}); e != nil {
		t.Fatal(e)
	}
	if got, _ := os.ReadDir(victim); len(got) != 0 {
		t.Fatal("notes written through the swapped link")
	}
	if _, e := os.Stat(filepath.Join(moved, "INDEX.md")); e != nil {
		t.Fatal(e)
	}
	if _, e := os.Stat(filepath.Join(moved, "5.md")); e != nil {
		t.Fatal(e)
	}
}

func TestKBExportRefusesSymlinkDestination(t *testing.T) {
	tmp := t.TempDir()
	victim, out := filepath.Join(tmp, "victim"), filepath.Join(tmp, "out")
	if e := os.Mkdir(victim, 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink(victim, out); e != nil {
		t.Fatal(e)
	}
	if e := exportEnv().kb([]string{"export", "demo", out}); e == nil || !strings.Contains(e.Error(), "symbolic link") {
		t.Fatal(e)
	}
	if got, _ := os.ReadDir(victim); len(got) != 0 {
		t.Fatal("wrote into the link target")
	}
}
