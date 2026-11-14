package core

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// #106 S3: a core file with a name outside the core folder is refused at Open; names inside it (a restore's snapshot)
// are not an alias.
func TestOpenRefusesAHardLinkFromOutside(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "core")
	st, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	if err := CheckLinks(dir); err != nil {
		t.Fatal(err)
	}
	inside := filepath.Join(dir, "snapshot.db") // a name inside the folder: fine
	if err := os.Link(filepath.Join(dir, dbFile), inside); err != nil {
		t.Fatal(err)
	}
	if err := CheckLinks(dir); err != nil {
		t.Fatalf("a name inside the core folder is refused: %v", err)
	}
	os.Remove(inside)
	for _, name := range []string{dbFile, auditFile, stateFile} { // the database, an audit log and the sidecar
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			continue
		}
		alias := filepath.Join(t.TempDir(), "alias")
		if err := os.Link(filepath.Join(dir, name), alias); err != nil {
			t.Fatal(err)
		}
		if _, err := Open(dir, Options{}); !errors.Is(err, ErrHardLinked) {
			t.Fatalf("%s with an outside name: %v", name, err)
		}
		os.Remove(alias)
	}
	// a backup too
	if err := os.MkdirAll(filepath.Join(dir, BackupFolder), 0o700); err != nil {
		t.Fatal(err)
	}
	b := filepath.Join(dir, BackupFolder, "b.db")
	if err := os.WriteFile(b, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(t.TempDir(), "b-alias")
	if err := os.Link(b, alias); err != nil {
		t.Fatal(err)
	}
	if err := CheckLinks(dir); !errors.Is(err, ErrHardLinked) {
		t.Fatalf("backup alias: %v", err)
	}
	os.Remove(alias)
	st, err = Open(dir, Options{})
	if err != nil {
		t.Fatalf("clean again: %v", err)
	}
	st.Close()
}
