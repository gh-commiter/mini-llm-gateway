package core

import (
	"database/sql"
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

func TestRestoreCompetingCallKeepsTheActiveTempFile(t *testing.T) { // review A2
	dir, bak := restoreFixture(t)
	lock, err := lockDir(dir) // the first restore holds the folder lock
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	tmp := filepath.Join(dir, restoreTmp)
	if err := os.WriteFile(tmp, []byte("the first restore's prepared database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Restore(dir, bak); !errors.Is(err, ErrLocked) {
		t.Fatalf("competing restore: %v", err)
	}
	if b, err := os.ReadFile(tmp); err != nil || !strings.Contains(string(b), "first restore") {
		t.Fatalf("the active restore's temp file was removed: %v", err)
	}
}

func TestRestoreCleanupFailureAfterTheSwapIsNotAFailure(t *testing.T) { // review A2
	dir, bak := restoreFixture(t)
	removeFn = func(p string) error {
		if strings.HasSuffix(p, dbFile+"-wal") {
			return errors.New("wal is locked")
		}
		return os.Remove(p)
	}
	defer func() { removeFn = os.Remove }()
	err := Restore(dir, bak)
	if !errors.Is(err, ErrRestoreApplied) {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(auditLog(t, dir), "restore.failed") || unresolvedIntent(dir) == "" {
		t.Fatalf("the intent was resolved as failed: %s", auditLog(t, dir))
	}
	removeFn = os.Remove
	s := openT(t, dir) // the next start finishes it
	defer s.Close()
	if marker(t, s) != "backup" || unresolvedIntent(dir) != "" {
		t.Fatalf("marker %q, audit %s", marker(t, s), auditLog(t, dir))
	}
}

func TestRestoreRefusesExtraSchemaObjects(t *testing.T) { // review L2
	for name, ddl := range map[string]string{
		"trigger": `CREATE TRIGGER evil AFTER INSERT ON meta BEGIN UPDATE meta SET value='x' WHERE key='epoch'; END`,
		"view":    `CREATE VIEW v AS SELECT * FROM meta`,
		"table":   `CREATE TABLE extra(a)`,
		"index":   `CREATE INDEX extra_idx ON meta(value)`,
	} {
		t.Run(name, func(t *testing.T) {
			dir, bak := restoreFixture(t)
			raw, err := sql.Open("sqlite", bak)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := raw.Exec(ddl); err != nil {
				t.Fatal(err)
			}
			raw.Close()
			before := hash(t, filepath.Join(dir, dbFile))
			if err := Restore(dir, bak); err == nil || !strings.Contains(err.Error(), "unexpected "+name) {
				t.Fatalf("got %v", err)
			}
			if hash(t, filepath.Join(dir, dbFile)) != before {
				t.Fatal("the database changed")
			}
		})
	}
	// an untouched backup of every schema this binary knows still restores (autoindexes are allowed)
	dir, bak := restoreFixture(t)
	if err := Restore(dir, bak); err != nil {
		t.Fatal(err)
	}
}

// R5: the file that is validated is the one that is installed.
func TestRestoreInstallsWhatItValidatedWhenTheSourceIsSwapped(t *testing.T) {
	dir, bak := restoreFixture(t)
	evil := filepath.Join(t.TempDir(), "evil.db") // another valid database of this gateway, with another marker
	{
		d2 := t.TempDir()
		s := openT(t, d2)
		exec(t, s, `INSERT INTO meta VALUES('marker','EVIL')`)
		s.Close()
		os.Rename(filepath.Join(d2, dbFile), evil)
	}
	restoreHook = func(stage string) error {
		if stage == "staged" { // after the copy, before the validation: the source is replaced
			b, _ := os.ReadFile(evil)
			return os.WriteFile(bak, b, 0o600)
		}
		return nil
	}
	defer func() { restoreHook = func(string) error { return nil } }()
	err := Restore(dir, bak)
	restoreHook = func(string) error { return nil }
	if err != nil {
		t.Fatal(err)
	}
	s := openT(t, dir)
	defer s.Close()
	if got := marker(t, s); got != "backup" {
		t.Fatalf("installed %q: the swapped source was used", got)
	}
}

func TestRestoreRefusesAndRemovesACorruptStagedCopy(t *testing.T) {
	dir, bak := restoreFixture(t)
	restoreHook = func(stage string) error {
		if stage == "staged" { // the staged copy itself is damaged (a hostile file written into the folder)
			return os.WriteFile(filepath.Join(dir, restoreTmp), []byte("not a database"), 0o600)
		}
		return nil
	}
	defer func() { restoreHook = func(string) error { return nil } }()
	err := Restore(dir, bak)
	restoreHook = func(string) error { return nil }
	if err == nil || !strings.Contains(err.Error(), bak) {
		t.Fatalf("a corrupt staged file was accepted or the error does not name the backup: %v", err)
	}
	for _, n := range []string{restoreTmp, restoreTmp + "-wal", restoreTmp + "-shm"} {
		if _, err := os.Stat(filepath.Join(dir, n)); err == nil {
			t.Fatalf("%s left behind", n)
		}
	}
	s := openT(t, dir)
	defer s.Close()
	if got := marker(t, s); got != "after-backup" {
		t.Fatalf("the old database changed: %q", got)
	}
}

// searchRestoreFixture: a schema-12 folder with two notes of ana, and a backup of it.
func searchRestoreFixture(t *testing.T) (dir, bak string, pr Principal, project int64, alpha, beta Note) {
	t.Helper()
	dir = t.TempDir()
	s := openT(t, dir)
	u, err := s.CreateUser(ctx, "ana")
	if err != nil {
		t.Fatal(err)
	}
	pr = Principal{UserID: u.ID, DeviceID: "dev-ana"}
	p, err := s.CreateProject(ctx, "fakture", "Fakture")
	if err != nil {
		t.Fatal(err)
	}
	project = p.ID
	if err := s.SetBinding(ctx, pr.DeviceID, u.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMember(ctx, project, u.ID, Developer); err != nil {
		t.Fatal(err)
	}
	if alpha, _, err = s.CreateNote(ctx, pr, project, "restore-alpha", NoteInput{Class: Structure, Title: "first", Body: "alpha body"}); err != nil {
		t.Fatal(err)
	}
	if beta, _, err = s.CreateNote(ctx, pr, project, "restore-beta", NoteInput{Class: Structure, Title: "second", Body: "beta body"}); err != nil {
		t.Fatal(err)
	}
	bak = filepath.Join(t.TempDir(), "search.db")
	if err := s.Backup(ctx, bak); err != nil {
		t.Fatal(err)
	}
	s.Close()
	return
}

// damageIndex writes to the backup and folds its WAL into the main file: the restore validates and copies the main
// file only.
func damageIndex(t *testing.T, bak, query string, args ...any) {
	t.Helper()
	db, err := sql.Open("sqlite", dsn(bak, "_pragma=trusted_schema(0)"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		t.Fatal(err)
	}
}

// Issue 136: a backup whose search index disagrees with its notes is installed with the index rebuilt from the notes.
func TestRestoreRebuildsTheSearchIndex(t *testing.T) {
	dir, bak, pr, project, alpha, beta := searchRestoreFixture(t)
	// "zebra" exists only in the damaged index; alpha's text and beta's row are gone.
	damageIndex(t, bak, `UPDATE note_search SET body='zebra' WHERE rowid=?`, alpha.ID)
	damageIndex(t, bak, `DELETE FROM note_search WHERE rowid=?`, beta.ID)
	if err := Restore(dir, bak); err != nil {
		t.Fatal(err)
	}
	s := openT(t, dir)
	defer s.Close()
	search := func(q string) (ids []int64) {
		pg, err := s.SearchNotes(ctx, pr, project, q)
		if err != nil {
			t.Fatal(err)
		}
		for _, h := range pg.Hits {
			ids = append(ids, h.ID)
		}
		return ids
	}
	if got := search("alpha"); len(got) != 1 || got[0] != alpha.ID {
		t.Fatalf("alpha: %v, want [%d]", got, alpha.ID)
	}
	if got := search("beta"); len(got) != 1 || got[0] != beta.ID {
		t.Fatalf("beta: %v, want [%d]", got, beta.ID)
	}
	if got := search("zebra"); len(got) != 0 {
		t.Fatalf("a word only in the damaged index: %v", got)
	}
}

func TestRestoreFailsWhenTheSearchIndexCannotBeRebuilt(t *testing.T) {
	dir, bak, _, _, _, _ := searchRestoreFixture(t)
	before := hash(t, filepath.Join(dir, dbFile))
	damageIndex(t, bak, `DROP TABLE note_search`)
	err := Restore(dir, bak)
	if err == nil || !strings.Contains(err.Error(), "search index") {
		t.Fatalf("got %v", err)
	}
	if hash(t, filepath.Join(dir, dbFile)) != before {
		t.Fatal("the live database changed")
	}
	for _, n := range []string{restoreTmp, restoreTmp + "-wal", restoreTmp + "-shm"} {
		if _, err := os.Stat(filepath.Join(dir, n)); err == nil {
			t.Fatalf("%s left behind", n)
		}
	}
	if !strings.Contains(auditLog(t, dir), "restore.failed") {
		t.Fatalf("the failure is not in the audit: %s", auditLog(t, dir))
	}
}
