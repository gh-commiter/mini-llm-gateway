//go:build !windows

package main

import (
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
