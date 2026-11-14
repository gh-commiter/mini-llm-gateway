//go:build windows

package core

import (
	"path/filepath"
	"testing"
)

// The Windows tester runs this: a drive path with spaces round-trips through the file: URI, and the URI forms
// are the ones documented for SQLite on Windows.
func TestWindowsPaths(t *testing.T) {
	if got := dsn(`C:\Users\a b\core.db`, ""); got != "file:///C:/Users/a%20b/core.db" {
		t.Fatalf("drive path: %s", got)
	}
	if got := dsn(`\\host\share\core.db`, ""); got != "file:////host/share/core.db" {
		t.Fatalf("UNC path: %s", got)
	}
	dir := filepath.Join(t.TempDir(), "a b#c")
	s := openT(t, dir)
	exec(t, s, `INSERT INTO meta VALUES('marker','one')`)
	bak := filepath.Join(t.TempDir(), "d e.db")
	if err := s.Backup(ctx, bak); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if err := Restore(dir, bak); err != nil {
		t.Fatal(err)
	}
}
