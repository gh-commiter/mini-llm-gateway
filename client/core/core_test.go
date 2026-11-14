package core

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

var ctx = context.Background()

func openT(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// withMigrations swaps the migration list for one test (tests using it must not run in parallel).
func withMigrations(t *testing.T, m ...func(*sql.Tx) error) {
	old := migrations
	migrations = append(append([]func(*sql.Tx) error{}, old...), m...)
	t.Cleanup(func() { migrations = old })
}

func exec(t *testing.T, s *Store, q string, args ...any) {
	t.Helper()
	if err := s.write(ctx, func(tx *sql.Tx) error { _, err := tx.Exec(q, args...); return err }); err != nil {
		t.Fatal(err)
	}
}

func newProject(t *testing.T, s *Store) (id int64) {
	t.Helper()
	if err := s.write(ctx, func(tx *sql.Tx) (err error) { id, err = s.NextProjectID(tx); return }); err != nil {
		t.Fatal(err)
	}
	return id
}

func auditLog(t *testing.T, dir string) string {
	b, _ := os.ReadFile(filepath.Join(dir, auditFile))
	return string(b)
}

func snapshots(dir string) (n []string) {
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if snapRe.MatchString(e.Name()) {
			n = append(n, e.Name())
		}
	}
	return
}

func hash(t *testing.T, p string) [32]byte {
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return sha256.Sum256(b)
}

func TestOpenReopen(t *testing.T) {
	dir := t.TempDir()
	s := openT(t, dir)
	gw := s.GatewayID()
	if len(gw) != 32 {
		t.Fatalf("gateway id %q", gw)
	}
	if e, _ := s.Epoch(ctx); e != 1 {
		t.Fatalf("epoch %d", e)
	}
	exec(t, s, `INSERT INTO meta VALUES('x','1')`)
	if _, err := Open(dir, Options{}); !errors.Is(err, ErrLocked) {
		t.Fatalf("second open: %v", err)
	}
	s.Close()
	s = openT(t, dir)
	defer s.Close()
	if s.GatewayID() != gw {
		t.Fatal("gateway id changed")
	}
	var v string
	if err := s.DB().QueryRow(`SELECT value FROM meta WHERE key='x'`).Scan(&v); err != nil || v != "1" {
		t.Fatalf("row lost: %v", err)
	}
	if len(snapshots(dir)) != 0 {
		t.Fatal("snapshot without a migration")
	}
}

func TestMigrationSnapshotAndPrune(t *testing.T) {
	dir := t.TempDir()
	openT(t, dir).Close()
	base := migrations
	t.Cleanup(func() { migrations = base })
	for i := 1; i <= 5; i++ {
		n := i
		var more []func(*sql.Tx) error // test-only migrations 2..n: adding one is one more list entry
		for k := 1; k <= n; k++ {
			k := k
			more = append(more, func(tx *sql.Tx) error {
				_, err := tx.Exec(fmt.Sprintf(`CREATE TABLE tt%d(a)`, k))
				return err
			})
		}
		migrations = append(append([]func(*sql.Tx) error{}, base...), more...)
		s := openT(t, dir)
		if v, _ := schemaVersion(ctx, s.DB()); v != len(base)+n {
			t.Fatalf("schema %d, want %d", v, len(base)+n)
		}
		s.Close()
	}
	got := snapshots(dir)
	if len(got) != keepSnaps {
		t.Fatalf("snapshots %v", got)
	}
	if !strings.Contains(auditLog(t, dir), `"action":"migrate"`) {
		t.Fatal("no migrate line in the audit file")
	}
	// the snapshot is a valid database of the old schema
	if _, err := validate(ctx, filepath.Join(dir, got[len(got)-1]), ""); err != nil {
		t.Fatal(err)
	}
}

func TestFailedMigrationKeepsOldFile(t *testing.T) {
	dir := t.TempDir()
	openT(t, dir).Close()
	base := migrations
	before := hash(t, filepath.Join(dir, dbFile))
	withMigrations(t,
		func(tx *sql.Tx) error { _, err := tx.Exec(`CREATE TABLE ok2(a)`); return err },
		func(tx *sql.Tx) error { _, err := tx.Exec(`CREATE TABLE nope(`); return err })
	if _, err := Open(dir, Options{}); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("migration %d", len(base)+2)) {
		t.Fatalf("want migration error, got %v", err)
	}
	if hash(t, filepath.Join(dir, dbFile)) != before {
		t.Fatal("database file changed by a failed migration")
	}
	if len(snapshots(dir)) != 1 || !strings.Contains(auditLog(t, dir), "migrate.failed") {
		t.Fatalf("snapshots %v, audit %q", snapshots(dir), auditLog(t, dir))
	}
	migrations = base
	s := openT(t, dir) // the lock was released and the old file works
	defer s.Close()
	if v, _ := schemaVersion(ctx, s.DB()); v != len(base) {
		t.Fatalf("schema %d", v)
	}
}

func TestNewerSchemaRefused(t *testing.T) {
	dir := t.TempDir()
	base := migrations
	withMigrations(t, func(tx *sql.Tx) error { _, err := tx.Exec(`CREATE TABLE t2(a)`); return err })
	openT(t, dir).Close()
	migrations = base
	before := hash(t, filepath.Join(dir, dbFile))
	if _, err := Open(dir, Options{}); !errors.Is(err, ErrNewerSchema) {
		t.Fatalf("got %v", err)
	}
	if hash(t, filepath.Join(dir, dbFile)) != before || len(snapshots(dir)) != 0 {
		t.Fatal("something was written")
	}
	if err := Restore(t.TempDir(), filepath.Join(dir, dbFile)); !errors.Is(err, ErrNewerSchema) {
		t.Fatalf("restore of a newer schema: %v", err)
	}
}

func TestEpochAndProjectIDsSurviveRestore(t *testing.T) { // contracts case 21
	dir := t.TempDir()
	s := openT(t, dir)
	newProject(t, s)
	if _, err := s.BumpEpoch(ctx, "device removed"); err != nil {
		t.Fatal(err)
	}
	bak := filepath.Join(t.TempDir(), "b.db")
	if err := s.Backup(ctx, bak); err != nil {
		t.Fatal(err)
	}
	var last int64
	for i := 0; i < 4; i++ {
		last = newProject(t, s)
	}
	epochBefore, _ := s.BumpEpoch(ctx, "member removed")
	exec(t, s, `INSERT INTO meta VALUES('marker','after-backup')`)
	if err := Restore(dir, bak); !errors.Is(err, ErrLocked) {
		t.Fatalf("restore with an open store: %v", err)
	}
	s.Close()

	if err := Restore(dir, bak); err != nil {
		t.Fatal(err)
	}
	s = openT(t, dir)
	defer s.Close()
	var n int
	s.DB().QueryRow(`SELECT count(*) FROM meta WHERE key='marker'`).Scan(&n)
	if n != 0 {
		t.Fatal("state after the backup is still there")
	}
	if e, _ := s.Epoch(ctx); e <= epochBefore {
		t.Fatalf("epoch %d not above %d", e, epochBefore)
	}
	if id := newProject(t, s); id <= last {
		t.Fatalf("project id %d reused (highest ever %d)", id, last)
	}
	log := auditLog(t, dir)
	if !strings.Contains(log, `"action":"restore"`) || len(snapshots(dir)) != 1 {
		t.Fatalf("audit %q snapshots %v", log, snapshots(dir))
	}
	ev, err := s.AuditEvents(ctx, 0, 100)
	if err != nil || len(ev) == 0 || ev[len(ev)-1].Action != "restore" {
		t.Fatalf("restore event missing in the table: %v %v", ev, err)
	}
}

// Even if core.state is gone, the restored database cannot lower the marks below the old database's.
func TestMarksFromSidecarOrDatabase(t *testing.T) {
	dir := t.TempDir()
	s := openT(t, dir)
	bak := filepath.Join(t.TempDir(), "b.db")
	s.Backup(ctx, bak)
	for i := 0; i < 3; i++ {
		newProject(t, s)
	}
	s.BumpEpoch(ctx, "x")
	s.Close()
	os.Remove(filepath.Join(dir, stateFile))
	if err := Restore(dir, bak); err != nil { // old database still has the marks
		t.Fatal(err)
	}
	s = openT(t, dir)
	defer s.Close()
	if id := newProject(t, s); id != 4 {
		t.Fatalf("id %d", id)
	}
	if e, _ := s.Epoch(ctx); e != 3 {
		t.Fatalf("epoch %d", e)
	}
}

func TestBackup(t *testing.T) {
	dir := t.TempDir()
	s := openT(t, dir)
	defer s.Close()
	dest := filepath.Join(t.TempDir(), "b.db")
	if err := s.Backup(ctx, dest); err != nil {
		t.Fatal(err)
	}
	if _, err := validate(ctx, dest, s.GatewayID()); err != nil {
		t.Fatal(err)
	}
	if err := s.Backup(ctx, dest); !errors.Is(err, os.ErrExist) {
		t.Fatalf("overwrite: %v", err)
	}
	if err := s.Backup(ctx, filepath.Join(t.TempDir(), "it's.db")); err != nil { // quote in the name
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(dest); fi.Mode().Perm() != 0o600 {
			t.Fatalf("mode %v", fi.Mode())
		}
	}
}

func TestRestoreRefusals(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	sa, sb := openT(t, a), openT(t, b)
	foreign := filepath.Join(t.TempDir(), "f.db")
	sb.Backup(ctx, foreign)
	sa.Close()
	sb.Close()
	before := hash(t, filepath.Join(a, dbFile))
	if err := Restore(a, foreign); !errors.Is(err, ErrForeign) {
		t.Fatalf("foreign: %v", err)
	}
	junk := filepath.Join(t.TempDir(), "junk.db")
	os.WriteFile(junk, []byte("not a database at all, just text padding to look like a file"), 0o600)
	if err := Restore(a, junk); err == nil {
		t.Fatal("junk accepted")
	}
	if err := Restore(a, filepath.Join(t.TempDir(), "missing.db")); err == nil {
		t.Fatal("missing accepted")
	}
	if hash(t, filepath.Join(a, dbFile)) != before {
		t.Fatal("the database changed")
	}
	if n := strings.Count(auditLog(t, a), "restore.failed"); n != 3 {
		t.Fatalf("restore.failed lines: %d\n%s", n, auditLog(t, a))
	}
	if left, _ := filepath.Glob(filepath.Join(a, ".core-restore*")); len(left) != 0 {
		t.Fatal("temp file left", left)
	}
	openT(t, a).Close() // still opens
}

func TestRestoreIntoEmptyFolder(t *testing.T) {
	s := openT(t, t.TempDir())
	bak := filepath.Join(t.TempDir(), "b.db")
	s.Backup(ctx, bak)
	gw := s.GatewayID()
	s.Close()
	dir := t.TempDir()
	if err := Restore(dir, bak); err != nil {
		t.Fatal(err)
	}
	r := openT(t, dir)
	defer r.Close()
	if r.GatewayID() != gw {
		t.Fatal("gateway id")
	}
}

func TestLostDatabaseKeepsIdentityAndMarks(t *testing.T) {
	dir := t.TempDir()
	s := openT(t, dir)
	gw := s.GatewayID()
	newProject(t, s)
	newProject(t, s)
	s.Close()
	os.Remove(filepath.Join(dir, dbFile))
	s = openT(t, dir)
	defer s.Close()
	if s.GatewayID() != gw || newProject(t, s) != 3 {
		t.Fatal("identity or marks lost with the database file")
	}
}

func TestAuditPaging(t *testing.T) {
	s := openT(t, t.TempDir())
	defer s.Close()
	for i := 0; i < 5; i++ {
		if err := s.write(ctx, func(tx *sql.Tx) error { return Audit(tx, "u1", "note.read", fmt.Sprint(i), map[string]any{"n": i}) }); err != nil {
			t.Fatal(err)
		}
	}
	p1, _ := s.AuditEvents(ctx, 0, 2)
	p2, _ := s.AuditEvents(ctx, p1[1].ID, 10)
	if len(p1) != 2 || len(p2) != 3 || p2[0].ID != p1[1].ID+1 || string(p2[0].Detail) != `{"n":2}` {
		t.Fatalf("%v %v", p1, p2)
	}
}

func TestFileModes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("modes are Unix only; Windows relies on the folder ACL (see ProtectDir)")
	}
	dir := t.TempDir()
	s := openT(t, dir)
	newProject(t, s)
	s.BumpEpoch(ctx, "x")
	s.Backup(ctx, filepath.Join(dir, "b.db"))
	defer s.Close()
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if fi, _ := e.Info(); fi.Mode().Perm() != 0o600 {
			t.Errorf("%s is %v", e.Name(), fi.Mode().Perm())
		}
	}
	for _, want := range []string{dbFile, dbFile + "-wal", dbFile + "-shm", stateFile, lockFile, "b.db"} {
		if _, err := os.Stat(filepath.Join(dir, want)); err != nil {
			t.Errorf("%s: %v", want, err)
		}
	}
	if got := auditLog(t, dir); got != "" {
		t.Logf("audit: %s", got)
	}
}

func TestAuditFileMode(t *testing.T) {
	dir := t.TempDir()
	if err := appendAudit(dir, "x", nil); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(filepath.Join(dir, auditFile)); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
}

func TestProtectDirHook(t *testing.T) {
	var got string
	s, err := Open(filepath.Join(t.TempDir(), "sub"), Options{ProtectDir: func(d string) error { got = d; return nil }})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if got == "" {
		t.Fatal("hook not called")
	}
	if _, err := Open(t.TempDir(), Options{ProtectDir: func(string) error { return errors.New("no") }}); err == nil {
		t.Fatal("hook error ignored")
	}
}

func TestConcurrentWriters(t *testing.T) {
	s := openT(t, t.TempDir())
	defer s.Close()
	exec(t, s, `CREATE TABLE c(g INT, i INT)`)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				if err := s.write(ctx, func(tx *sql.Tx) error { _, err := tx.Exec(`INSERT INTO c VALUES(?,?)`, g, i); return err }); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	var n int
	s.DB().QueryRow(`SELECT count(*) FROM c`).Scan(&n)
	if n != 1600 {
		t.Fatalf("%d rows", n)
	}
}
