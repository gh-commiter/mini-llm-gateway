package core

import (
	"database/sql"
	"fmt"
	"os"
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

// populate writes a small but complete state into the database at dir (opened at schema v): users, projects, teams, team projects,
// memberships (direct and through a team), grants, dependencies, notes, a team note and the search postings, with raw SQL because the
// store's own calls write the current schema. Only what the schema has is written.
func populate(t *testing.T, dir string, v int) {
	t.Helper()
	raw, err := sql.Open("sqlite", dsn(filepath.Join(dir, dbFile), ""))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	has := func(table string) bool {
		var n int
		raw.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&n)
		return n > 0
	}
	run := func(q string, a ...any) {
		t.Helper()
		if _, err := raw.Exec(q, a...); err != nil {
			t.Fatal(q, err)
		}
	}
	run(`INSERT INTO users(id,name) VALUES(1,'ana'),(2,'bor')`)
	run(`INSERT INTO projects(id,slug,name) VALUES(1,'alpha','Alpha'),(2,'beta','Beta'),(3,'gamma','Gamma')`)
	run(`INSERT INTO memberships(project_id,user_id,role) VALUES(1,1,'developer')`)
	run(`INSERT INTO grants(source_id,reader_id,class) VALUES(1,2,'structure')`)
	note := func(project int64, class string) {
		run(`INSERT INTO notes(project_id,class,title,body,bytes,author_id,created,updated) VALUES(?,?,'needle title','needle body',11,1,1,1)`, project, class)
	}
	note(1, "structure")
	note(2, "data")
	if has("teams") {
		run(`INSERT INTO teams(id,name) VALUES(1,'core')`)
		run(`INSERT INTO team_members(team_id,user_id) VALUES(1,2)`)
		run(`INSERT INTO team_projects(team_id,project_id) VALUES(1,2)`)
	}
	if has("direct_memberships") {
		run(`INSERT INTO direct_memberships(project_id,user_id,role) VALUES(1,1,'developer')`)
		run(`INSERT INTO team_assignments(team_id,project_id,user_id,role) VALUES(1,2,2,'reader')`)
		run(`INSERT INTO memberships(project_id,user_id,role) VALUES(2,2,'reader')`)
	}
	if v >= 15 {
		run(`INSERT OR IGNORE INTO projects(id,slug,name) VALUES(?,'@team-1','')`, segmentBase+1)
		note(segmentBase+1, "structure") // a team note
	}
	if has("project_depends") {
		run(`INSERT INTO project_depends(project_id,depends_on) VALUES(1,3)`)
	}
	if has("project_team_depends") {
		run(`INSERT INTO project_team_depends(project_id,team_id) VALUES(3,1)`)
	}
	if has("note_search") { // stale postings: the backup's index is emptied on purpose, a restore must rebuild it
		run(`DELETE FROM note_search`)
	}
}

// A POPULATED backup of every schema the program can migrate from (8 to the current one) restores (it is migrated and validated as a
// current database before it replaces the live one), opens, keeps its rows, passes the invariant pass and is searchable.
func TestRestoreAcceptsAPopulatedBackupOfEverySchemaVersion(t *testing.T) {
	base := migrations
	t.Cleanup(func() { migrations = base })
	for v := 8; v <= len(base); v++ {
		t.Run(fmt.Sprint("schema", v), func(t *testing.T) {
			dir := t.TempDir()
			migrations = base[:v]
			openT(t, dir).Close()
			populate(t, dir, v)
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
			if n, err := schemaVersion(ctx, s.DB()); err != nil || n != len(base) {
				t.Fatal("schema after restore", n, err)
			}
			if m := s.Migrated(); m != nil {
				t.Fatal("the restore left a migration for the next Open", m)
			}
			var notes int
			if err := s.DB().QueryRow(`SELECT count(*) FROM notes WHERE title='needle title'`).Scan(&notes); err != nil || notes < 2 {
				t.Fatal("notes", notes, err)
			}
			if err := s.CheckMemberships(ctx); err != nil {
				t.Fatal(err)
			}
			var hits int
			if err := s.DB().QueryRow(`SELECT count(*) FROM note_search WHERE note_search MATCH 'needle'`).Scan(&hits); err != nil || hits != notes {
				t.Fatal("search postings after restore", hits, "of", notes, err)
			}
		})
	}
}

// A schema-14 backup with an ordinary project on a team's segment id is refused, and the previous database stays.
func TestRestoreRefusesAReservedSegmentIDInAnOldBackup(t *testing.T) {
	base := migrations
	t.Cleanup(func() { migrations = base })
	dir := t.TempDir()
	openT(t, dir).Close()
	lraw, err := sql.Open("sqlite", dsn(filepath.Join(dir, dbFile), ""))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lraw.Exec(`INSERT OR REPLACE INTO meta VALUES('marker','live')`); err != nil {
		t.Fatal(err)
	}
	var gw string
	lraw.QueryRow(`SELECT value FROM meta WHERE key='gateway_id'`).Scan(&gw)
	lraw.Close()
	src := t.TempDir()
	migrations = base[:14]
	openT(t, src).Close()
	raw, err := sql.Open("sqlite", dsn(filepath.Join(src, dbFile), ""))
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		fmt.Sprintf(`UPDATE meta SET value='%s' WHERE key='gateway_id'`, gw), // the same gateway
		`INSERT INTO users(id,name) VALUES(1,'ana')`,
		`INSERT INTO teams(id,name) VALUES(1,'core')`,
		`INSERT INTO team_members(team_id,user_id) VALUES(1,1)`,
		fmt.Sprintf(`INSERT INTO projects(id,slug,name) VALUES(%d,'ordinary','Ordinary')`, segmentBase+1),
		fmt.Sprintf(`INSERT INTO notes(project_id,class,title,body,bytes,author_id,created,updated) VALUES(%d,'private','secret','secret',6,1,1,1)`, segmentBase+1),
	} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatal(q, err)
		}
	}
	raw.Close()
	st, err := os.ReadFile(filepath.Join(dir, stateFile))
	if err == nil {
		err = os.WriteFile(filepath.Join(src, stateFile), st, 0o600)
	}
	if err != nil {
		t.Fatal(err)
	}
	s := openT(t, src)
	bak := filepath.Join(t.TempDir(), "b.db")
	if err := s.Backup(ctx, bak); err != nil {
		t.Fatal(err)
	}
	s.Close()
	migrations = base
	if err := Restore(dir, bak); err == nil || !strings.Contains(err.Error(), "reserved") {
		t.Fatal("must be refused as reserved:", err)
	}
	s = openT(t, dir)
	defer s.Close()
	if got := marker(t, s); got != "live" {
		t.Fatal("the previous database was replaced", got)
	}
}
