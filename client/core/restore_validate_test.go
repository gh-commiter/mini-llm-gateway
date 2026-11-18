package core

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// craft runs statements on the backup with writable_schema on and foreign keys off, on ONE connection, and folds the WAL
// into the file (the restore copies the main file only).
func craft(t *testing.T, bak string, stmts ...string) {
	t.Helper()
	db, err := sql.Open("sqlite", dsn(bak, ""))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	for _, q := range append(append([]string{`PRAGMA writable_schema=1`, `PRAGMA foreign_keys=OFF`}, stmts...), `PRAGMA writable_schema=0`, `PRAGMA wal_checkpoint(TRUNCATE)`) {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
}

func refuses(t *testing.T, bak, want string, stmts ...string) {
	t.Helper()
	dir, _ := restoreFixture(t)
	craft(t, bak, stmts...)
	before := hash(t, filepath.Join(dir, dbFile))
	if err := Restore(dir, bak); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("want %q, got %v", want, err)
	}
	if hash(t, filepath.Join(dir, dbFile)) != before {
		t.Fatal("the database changed")
	}
}

// A trigger that borrows the name of an automatic index must not pass.
func TestRestoreRefusesATriggerNamedLikeAnAutoindex(t *testing.T) {
	dir, bak := restoreFixture(t)
	craft(t, bak, `INSERT INTO sqlite_master(type,name,tbl_name,rootpage,sql) VALUES('trigger','sqlite_autoindex_evil','meta',0,'CREATE TRIGGER sqlite_autoindex_evil AFTER DELETE ON meta BEGIN SELECT 1; END')`)
	before := hash(t, filepath.Join(dir, dbFile))
	if err := Restore(dir, bak); err == nil || !strings.Contains(err.Error(), `unexpected trigger "sqlite_autoindex_evil"`) {
		t.Fatalf("got %v", err)
	}
	if hash(t, filepath.Join(dir, dbFile)) != before {
		t.Fatal("the database changed")
	}
}

func TestRestoreComparesTableAndIndexDefinitions(t *testing.T) {
	_, bak := restoreFixture(t)
	refuses(t, bak, `table "team_assignments" differs`, `UPDATE sqlite_master SET sql=replace(sql,' ON DELETE CASCADE','') WHERE name='team_assignments'`)
	_, bak = restoreFixture(t)
	refuses(t, bak, `index "teams_name" differs`, `UPDATE sqlite_master SET sql=replace(sql,'UNIQUE ','') WHERE name='teams_name'`)
	_, bak = restoreFixture(t)
	refuses(t, bak, `missing index teams_name`, `DROP INDEX teams_name`)
}

func TestRestoreRefusesForeignKeyViolations(t *testing.T) {
	_, bak := restoreFixture(t)
	refuses(t, bak, "foreign key", `INSERT INTO direct_memberships(project_id,user_id,role) VALUES(999,999,'developer')`)
}

func TestRestoreRefusesCountersOutOfRange(t *testing.T) {
	for _, v := range []string{"9223372036854775807", "-1", "2305843009213693953"} {
		for _, k := range []string{"epoch", "project_high"} {
			_, bak := restoreFixture(t)
			refuses(t, bak, "outside 0..", fmt.Sprintf(`UPDATE meta SET value='%s' WHERE key='%s'`, v, k))
		}
	}
}

// A database upgraded step by step from schema 8 restores, and so does one made fresh (the existing tests).
func TestRestoreAcceptsADatabaseUpgradedFromSchema8(t *testing.T) {
	base := migrations
	t.Cleanup(func() { migrations = base })
	dir := t.TempDir()
	migrations = base[:8]
	openT(t, dir).Close()
	migrations = base
	s := openT(t, dir)
	bak := filepath.Join(t.TempDir(), "b.db")
	if err := s.Backup(ctx, bak); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if err := Restore(dir, bak); err != nil {
		t.Fatal(err)
	}
	openT(t, dir).Close()
}

// Failing starts must not push out the snapshot from before the upgrade.
func TestRepeatedFailingMigrationsKeepThePreUpgradeSnapshot(t *testing.T) {
	for _, change := range []bool{false, true} { // false: identical content each time (deduplicated); true: content differs
		t.Run(fmt.Sprint("changed=", change), func(t *testing.T) {
			dir := t.TempDir()
			openT(t, dir).Close()
			base := migrations
			good := func(tx *sql.Tx) error { _, err := tx.Exec(`CREATE TABLE ok2(a)`); return err }
			bad := func(tx *sql.Tx) error { _, err := tx.Exec(`CREATE TABLE half(a); CREATE TABLE nope(`); return err }
			withMigrations(t, good, bad)
			for i := 0; i < 8; i++ {
				if _, err := Open(dir, Options{}); err == nil {
					t.Fatal("the migration must fail")
				}
				if change {
					db, err := sql.Open("sqlite", dsn(filepath.Join(dir, dbFile), ""))
					if err != nil {
						t.Fatal(err)
					}
					_, err = db.Exec(`INSERT OR REPLACE INTO meta VALUES('marker',?)`, fmt.Sprint(i))
					db.Close()
					if err != nil {
						t.Fatal(err)
					}
				}
			}
			var low, n int
			for _, name := range snapshots(dir) {
				m := snapRe.FindStringSubmatch(name)
				v := 0
				fmt.Sscan(m[1], &v)
				if low == 0 || v < low {
					low = v
				}
				n++
			}
			if low != len(base) {
				t.Fatalf("the snapshot of schema %d is gone: %v", len(base), snapshots(dir))
			}
			if limit := keepSnaps + 1; n > limit || (!change && n != 2) {
				t.Fatalf("%d snapshots: %v", n, snapshots(dir))
			}
		})
	}
}

// A backup of every schema the program can migrate from (8 to the current one) restores, opens (migrates to the
// current schema) and keeps its rows. Each database is built by running the migrations up to that version.
func TestRestoreAcceptsABackupOfEverySchemaVersion(t *testing.T) {
	base := migrations
	t.Cleanup(func() { migrations = base })
	for v := 8; v <= len(base); v++ {
		t.Run(fmt.Sprint("schema", v), func(t *testing.T) {
			dir := t.TempDir()
			migrations = base[:v]
			openT(t, dir).Close()
			raw, err := sql.Open("sqlite", dsn(filepath.Join(dir, dbFile), ""))
			if err != nil {
				t.Fatal(err)
			}
			_, err = raw.Exec(`INSERT OR REPLACE INTO meta VALUES('marker','survives')`)
			raw.Close()
			if err != nil {
				t.Fatal(err)
			}
			s := openT(t, dir)
			bak := filepath.Join(t.TempDir(), "b.db")
			if err := s.Backup(ctx, bak); err != nil {
				t.Fatal(err)
			}
			s.Close()
			migrations = base
			if err := Restore(dir, bak); err != nil {
				t.Fatal(err)
			}
			s = openT(t, dir)
			defer s.Close()
			var got string
			if err := s.DB().QueryRow(`SELECT value FROM meta WHERE key='marker'`).Scan(&got); err != nil || got != "survives" {
				t.Fatal(got, err)
			}
			if n, err := schemaVersion(ctx, s.DB()); err != nil || n != len(base) {
				t.Fatal("schema after open", n, err)
			}
		})
	}
}
