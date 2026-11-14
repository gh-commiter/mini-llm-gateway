package core

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// restoreFixture: a folder whose database has marker "new" in the backup and "old" after it.
func restoreFixture(t *testing.T) (dir, bak string) {
	dir = t.TempDir()
	s := openT(t, dir)
	exec(t, s, `INSERT INTO meta VALUES('marker','backup')`)
	bak = filepath.Join(t.TempDir(), "b.db")
	if err := s.Backup(ctx, bak); err != nil {
		t.Fatal(err)
	}
	exec(t, s, `UPDATE meta SET value='after-backup' WHERE key='marker'`)
	s.Close()
	return
}

func marker(t *testing.T, s *Store) (v string) {
	s.DB().QueryRow(`SELECT value FROM meta WHERE key='marker'`).Scan(&v)
	return
}

var errCrash = errors.New("simulated crash")

func TestRestoreCrashAtEveryStage(t *testing.T) {
	for _, c := range []struct {
		stage string
		want  string // database content after the next Open
	}{
		{"prepared", "after-backup"}, {"snapshot", "after-backup"}, {"intent", "after-backup"},
		{"swapped", "backup"}, {"cleaned", "backup"},
	} {
		t.Run(c.stage, func(t *testing.T) {
			dir, bak := restoreFixture(t)
			restoreHook = func(stage string) error {
				if stage == c.stage {
					return errCrash
				}
				return nil
			}
			defer func() { restoreHook = func(string) error { return nil } }()
			if err := Restore(dir, bak); !errors.Is(err, errCrash) {
				t.Fatal(err)
			}
			if fi, err := os.Stat(filepath.Join(dir, dbFile)); err != nil || fi.Size() == 0 {
				t.Fatalf("core.db missing after a crash at %s: %v", c.stage, err)
			}
			os.WriteFile(filepath.Join(dir, dbFile+"-wal"), []byte("stale wal of the old file"), 0o600) // old WAL left behind
			restoreHook = func(string) error { return nil }
			s := openT(t, dir)
			defer s.Close()
			if got := marker(t, s); got != c.want {
				t.Fatalf("marker %q, want %q", got, c.want)
			}
			if _, err := os.Stat(filepath.Join(dir, restoreTmp)); err == nil {
				t.Fatal("temp file left")
			}
			if id := unresolvedIntent(dir); id != "" {
				t.Fatalf("intent %s still open: %s", id, auditLog(t, dir))
			}
			if c.stage != "prepared" && c.stage != "snapshot" && !strings.Contains(auditLog(t, dir), "restore.intent") {
				t.Fatal("no intent line")
			}
			// a second restore after the recovery works
			s.Close()
			if err := Restore(dir, bak); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestRestoreAuditFailureAfterSwapIsNotAFailure(t *testing.T) {
	dir, bak := restoreFixture(t)
	auditWriter = func(d, action string, detail map[string]any) error {
		if action == "restore" {
			return errors.New("disk full")
		}
		return appendAudit(d, action, detail)
	}
	defer func() { auditWriter = appendAudit }()
	if err := Restore(dir, bak); err != nil {
		t.Fatalf("restore reported failure: %v", err)
	}
	auditWriter = appendAudit
	if unresolvedIntent(dir) == "" {
		t.Fatal("expected an open intent")
	}
	s := openT(t, dir) // recovery writes the completion
	defer s.Close()
	if marker(t, s) != "backup" || unresolvedIntent(dir) != "" || !strings.Contains(auditLog(t, dir), `"recovered":true`) {
		t.Fatalf("marker %q audit %s", marker(t, s), auditLog(t, dir))
	}
}

func TestRestoreIntentFailureKeepsOldDatabase(t *testing.T) {
	dir, bak := restoreFixture(t)
	auditWriter = func(d, action string, detail map[string]any) error {
		if action == "restore.intent" {
			return errors.New("disk full")
		}
		return appendAudit(d, action, detail)
	}
	defer func() { auditWriter = appendAudit }()
	if err := Restore(dir, bak); err == nil {
		t.Fatal("want error")
	}
	auditWriter = appendAudit
	s := openT(t, dir)
	defer s.Close()
	if marker(t, s) != "after-backup" || !strings.Contains(auditLog(t, dir), "restore.failed") {
		t.Fatalf("marker %q audit %s", marker(t, s), auditLog(t, dir))
	}
}

func TestRestoreRenameFailureKeepsOldDatabase(t *testing.T) {
	dir, bak := restoreFixture(t)
	// a directory named core.db.d is irrelevant; make the rename fail by making the temp file vanish at the intent stage
	restoreHook = func(stage string) error {
		if stage == "intent" {
			os.Remove(filepath.Join(dir, restoreTmp))
		}
		return nil
	}
	defer func() { restoreHook = func(string) error { return nil } }()
	if err := Restore(dir, bak); err == nil {
		t.Fatal("want error")
	}
	restoreHook = func(string) error { return nil }
	s := openT(t, dir)
	defer s.Close()
	if marker(t, s) != "after-backup" || unresolvedIntent(dir) != "" {
		t.Fatalf("marker %q audit %s", marker(t, s), auditLog(t, dir))
	}
}
