package core

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// A helper killed in the middle of a snapshot (a very large database) leaves a partial copy: it is not a snapshot (it does not count
// towards keepSnaps and cannot push the pre-upgrade copy out) and the next Open removes it, and only files of exactly that name.
func TestKilledSnapshotLeavesNoCountedSnapshot(t *testing.T) {
	dir := t.TempDir()
	openT(t, dir).Close()
	partial := filepath.Join(dir, "core-8-20260101T000000.000Z.db.partial")
	other := filepath.Join(dir, "core-8-20260101T000000.000Z.db.partial.mine") // not the pattern: stays
	for _, p := range []string{partial, other} {
		if err := os.WriteFile(p, []byte("half a copy"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if n := snapshots(dir); len(n) != 0 {
		t.Fatalf("a partial copy counts as a snapshot: %v", n)
	}
	openT(t, dir).Close()
	if _, err := os.Stat(partial); !os.IsNotExist(err) {
		t.Fatalf("the stale partial copy was not removed: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("a file outside the pattern was removed: %v", err)
	}
}

// The upgrade is announced before the snapshot (the helper tells the app, which then waits for the long step), and a finished
// snapshot is under its final name.
func TestOnUpgradeIsCalledBeforeTheSnapshot(t *testing.T) {
	dir := t.TempDir()
	openT(t, dir).Close()
	withMigrations(t, func(tx *sql.Tx) error { _, err := tx.Exec(`CREATE TABLE up1(a)`); return err })
	var from, to, snapsThen int
	called := 0
	s, err := Open(dir, Options{OnUpgrade: func(f, t int) { called++; from, to, snapsThen = f, t, len(snapshots(dir)) }})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if called != 1 || to != from+1 || from != len(migrations)-1 || snapsThen != 0 {
		t.Fatalf("called %d times, %d to %d, %d snapshots at the call", called, from, to, snapsThen)
	}
	if n := snapshots(dir); len(n) != 1 {
		t.Fatalf("snapshots %v", n)
	}
	if m, _ := filepath.Glob(filepath.Join(dir, "*.partial")); len(m) != 0 {
		t.Fatalf("partial left: %v", m)
	}
	// a current database does not announce anything
	s.Close()
	called = 0
	s2, err := Open(dir, Options{OnUpgrade: func(int, int) { called++ }})
	if err != nil {
		t.Fatal(err)
	}
	s2.Close()
	if called != 0 {
		t.Fatal("announced an upgrade of a current database")
	}
}
