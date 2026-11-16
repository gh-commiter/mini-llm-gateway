package core

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const restoreTmp = ".core-restore.tmp"

// ErrRestoreApplied: the new database is in place, but a later step failed; the previous database is NOT in place.
var ErrRestoreApplied = errors.New("restore applied, cleanup pending")

var removeFn = os.Remove // tests make removals fail

// Test hooks: a hook that returns an error simulates a crash at that stage (nothing is cleaned up or audited).
var (
	restoreHook = func(stage string) error { return nil }
	auditWriter = appendAudit // tests replace it to fail an append
)

// Restore replaces <dir>/core.db with the backup src. The gateway must be stopped: it takes the folder lock an
// open store holds. src is validated first (integrity, schema not newer, same gateway id). A database of schema 12 or
// newer has its search index rebuilt from the notes before the swap; if that fails, the restore fails with the old
// database in place. The epoch is set above both the old and the restored value, the project-id mark is the maximum of all.
//
// Crash safety: core.db is never absent. The new file is prepared as a temp file, the old database is hard-linked
// (or copied) to its snapshot, an intent line is fsynced to core.audit.log, and ONE rename puts the new file in
// place. On Windows os.Rename replaces an existing file (MoveFileEx with REPLACE_EXISTING) but fails if another
// process has core.db open without share-delete; the folder lock keeps our own processes out, and a failure just
// leaves the old database in place. recoverRestore (called by Open and Restore) finishes or rolls back an
// unfinished restore, so any failure leaves either the old or the new database, never a mix.
func Restore(dir, src string) (err error) {
	dir, err = filepath.Abs(dir)
	if err != nil {
		return err
	}
	if src, err = filepath.Abs(src); err != nil {
		return err
	}
	id := randID()
	detail := map[string]any{"id": id, "source": src}
	crashed := false
	tmp := filepath.Join(dir, restoreTmp)
	owned, swapped := false, false // owned: this call holds the folder lock; swapped: the new file is in place
	defer func() {
		if err == nil || crashed {
			return
		}
		detail["error"] = err.Error()
		if swapped { // applied: the intent stays open, the next Open finishes the cleanup
			return
		}
		auditWriter(dir, "restore.failed", detail) // first: after this the unfinished restore counts as resolved
		if owned {                                 // the temp file is shared by name: only the lock holder may remove it
			removeFiles(tmp, tmp+"-wal", tmp+"-shm")
		}
	}()
	stage := func(name string) error {
		if err := restoreHook(name); err != nil {
			crashed = true
			return err
		}
		return nil
	}
	lock, err := lockDir(dir)
	if err != nil {
		return err
	}
	defer lock.Close()
	owned = true
	if err := recoverRestore(dir); err != nil {
		return err
	}
	ctx := context.Background()

	side, err := readState(dir)
	if err != nil {
		return err
	}
	cur, oldSchema, err := currentState(ctx, dir, side)
	if err != nil {
		return err
	}
	// R5: the file that is validated is the file that is installed: the source is copied once into the exclusively created private
	// staging file (0600, in the core folder), and that copy is validated, stamped and renamed; a swap of the source after the copy
	// has no effect, and a hostile or corrupt copy is refused and removed (the deferred cleanup).
	if fi, err := os.Lstat(src); err != nil || !fi.Mode().IsRegular() {
		if err == nil {
			err = errors.New(src + " is not a regular file")
		}
		return err
	}
	removeFiles(tmp, tmp+"-wal", tmp+"-shm")
	if err := copyFile(src, tmp); err != nil {
		return err
	}
	if err := stage("staged"); err != nil {
		return err
	}
	in, err := validateAs(ctx, tmp, src, cur.GatewayID)
	if err != nil {
		return err
	}
	next := state{
		GatewayID:   in.GatewayID,
		Epoch:       max(cur.Epoch, in.Epoch) + 1,
		ProjectHigh: max(cur.ProjectHigh, in.ProjectHigh),
	}
	detail["epoch_from"], detail["epoch_to"], detail["project_high"] = max(cur.Epoch, in.Epoch), next.Epoch, next.ProjectHigh

	// The staged file is next to the old one, so the final step is a rename on the same volume.
	if err := stamp(ctx, tmp, next, src); err != nil {
		return err
	}
	if err := stage("prepared"); err != nil {
		return err
	}
	if err := writeState(dir, next); err != nil { // before the swap: ahead of the database is harmless
		return err
	}

	db := filepath.Join(dir, dbFile)
	if fi, err := os.Lstat(db); err == nil && fi.Size() > 0 {
		snap := filepath.Join(dir, snapshotName(oldSchema))
		detail["snapshot"] = filepath.Base(snap)
		if err := os.Link(db, snap); err != nil { // no hard links (FAT, some shares): copy
			if err := copyFile(db, snap); err != nil {
				return err
			}
		}
		if fi, err := os.Stat(db + "-wal"); err == nil && fi.Size() > 0 { // checkpoint failed: keep the WAL with it
			if err := copyFile(db+"-wal", snap+"-wal"); err != nil {
				return err
			}
		}
	}
	if err := stage("snapshot"); err != nil {
		return err
	}
	if err := auditWriter(dir, "restore.intent", detail); err != nil {
		return err
	}
	if err := stage("intent"); err != nil {
		return err
	}
	if err := os.Rename(tmp, db); err != nil {
		return err
	}
	swapped = true
	syncDir(dir)
	if err := stage("swapped"); err != nil {
		return err
	}
	// The old WAL and SHM must never meet the new file.
	if err := removeFiles(db+"-wal", db+"-shm"); err != nil {
		return fmt.Errorf("%w: the old WAL could not be removed (the next start retries it): %v", ErrRestoreApplied, err)
	}
	pruneSnapshots(dir)
	if err := stage("cleaned"); err != nil {
		return err
	}
	// A failed append here does not make the restore a failure: the new database is in place, and the next Open
	// sees the intent without a completion and writes it.
	auditWriter(dir, "restore", detail)
	return nil
}

func randID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// removeFiles removes the files that exist; the first real error is returned.
func removeFiles(paths ...string) (err error) {
	for _, p := range paths {
		if e := removeFn(p); e != nil && !errors.Is(e, os.ErrNotExist) && err == nil {
			err = e
		}
	}
	return err
}

// unresolvedIntent returns the id of a restore intent in the audit file that has no completion, failure or rollback.
func unresolvedIntent(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, auditFile))
	if err != nil {
		return ""
	}
	open := ""
	for _, line := range strings.Split(string(b), "\n") {
		var e struct {
			Action string
			Detail struct{ ID string }
		}
		if json.Unmarshal([]byte(line), &e) != nil {
			continue
		}
		switch e.Action {
		case "restore.intent":
			open = e.Detail.ID
		case "restore", "restore.failed", "restore.rolledback":
			if e.Detail.ID == open {
				open = ""
			}
		}
	}
	return open
}

// recoverRestore settles an unfinished restore before anything else touches the folder (the lock is held).
// Temp file still there: the swap did not happen, roll back. Temp file gone: the swap happened, so the old WAL and
// SHM are stale; remove them and write the completion.
func recoverRestore(dir string) error {
	tmp := filepath.Join(dir, restoreTmp)
	id := unresolvedIntent(dir)
	_, err := os.Lstat(tmp)
	tmpThere := err == nil
	switch {
	case id != "" && !tmpThere:
		db := filepath.Join(dir, dbFile)
		if err := removeFiles(db+"-wal", db+"-shm"); err != nil {
			return err
		}
		return appendAudit(dir, "restore", map[string]any{"id": id, "recovered": true})
	case id != "":
		if err := appendAudit(dir, "restore.rolledback", map[string]any{"id": id}); err != nil {
			return err
		}
	}
	return removeFiles(tmp, tmp+"-wal", tmp+"-shm")
}

// currentState reads the state of the existing database (best effort: a damaged file is what a restore may be for)
// merged with the sidecar. A missing database with a missing sidecar gives the zero state.
func currentState(ctx context.Context, dir string, side state) (state, int, error) {
	path := filepath.Join(dir, dbFile)
	fi, err := os.Lstat(path)
	if err != nil || fi.Size() == 0 {
		return side, 0, nil
	}
	if !fi.Mode().IsRegular() {
		return side, 0, errors.New(path + " is not a regular file")
	}
	db, err := openDB(path)
	if err != nil {
		return side, 0, nil
	}
	defer db.Close()
	m, err := readMetaFull(ctx, db)
	if err != nil {
		return side, 0, nil
	}
	db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`) // so the snapshot is complete without its WAL
	st := m.state
	st.Epoch, st.ProjectHigh = max(st.Epoch, side.Epoch), max(st.ProjectHigh, side.ProjectHigh)
	if st.GatewayID == "" {
		st.GatewayID = side.GatewayID
	}
	return st, m.Schema, nil
}

// validate opens src read-only and immutable (no WAL or SHM is created next to it) and checks it.
func validate(ctx context.Context, src, gateway string) (state, error) {
	return validateAs(ctx, src, src, gateway)
}

// validateAs checks the file at path; shown is the name in the errors (the staged copy stands for the backup).
func validateAs(ctx context.Context, path, shown, gateway string) (state, error) {
	src := path
	fi, err := os.Lstat(src)
	if err != nil {
		return state{}, err
	}
	if !fi.Mode().IsRegular() {
		return state{}, errors.New(shown + " is not a regular file")
	}
	db, err := sql.Open("sqlite", dsn(src, "mode=ro&immutable=1&_pragma=trusted_schema(0)"))
	if err != nil {
		return state{}, err
	}
	defer db.Close()
	var res string
	if err := db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&res); err != nil || res != "ok" {
		return state{}, fmt.Errorf("%s is not a valid core database (integrity check: %v %v)", shown, res, err)
	}
	v, err := schemaVersion(ctx, db)
	if err != nil || v == 0 {
		return state{}, fmt.Errorf("%s is not a core database", shown)
	}
	if v > len(migrations) {
		return state{}, fmt.Errorf("%w (schema %d, program %d)", ErrNewerSchema, v, len(migrations))
	}
	if err := checkSchemaObjects(ctx, db, v); err != nil {
		return state{}, fmt.Errorf("%s: %w", shown, err)
	}
	if v >= 10 {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return state{}, err
		}
		err = checkMemberships(tx)
		tx.Rollback()
		if err != nil {
			return state{}, fmt.Errorf("%s: %w", shown, err)
		}
	}
	m, err := readMetaFull(ctx, db)
	if err != nil {
		return state{}, err
	}
	if gateway != "" && m.GatewayID != gateway {
		return state{}, fmt.Errorf("%w (backup %s, this %s)", ErrForeign, m.GatewayID, gateway)
	}
	return m.state, nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err = out.ReadFrom(in); err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

// stamp writes the new state and the restore event into the copy, folds its WAL into the file, and, for a database
// of schema 12 or newer, rebuilds the search index from the notes so a damaged index in the backup is not installed.
func stamp(ctx context.Context, path string, st state, src string) error {
	db, err := openDB(path)
	if err != nil {
		return err
	}
	defer db.Close()
	version, err := schemaVersion(ctx, db)
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	for k, v := range map[string]string{"gateway_id": st.GatewayID, "epoch": i64(st.Epoch), "project_high": i64(st.ProjectHigh)} {
		if err = setMeta(tx, k, v); err != nil {
			tx.Rollback()
			return err
		}
	}
	if version >= 12 { // older backups get the index from the migration
		if err = rebuildSearch(tx); err != nil {
			tx.Rollback()
			return fmt.Errorf("the search index could not be rebuilt: %w", err)
		}
	}
	if err = Audit(tx, "core", "restore", filepath.Base(src), map[string]any{"epoch": st.Epoch}); err != nil {
		tx.Rollback()
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	_, err = db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`)
	return err
}

// checkSchemaObjects (L2): a backup may hold exactly the tables and indexes of its schema version, built here from
// this program's own migrations: no trigger, view, virtual table or unknown table that could run code or read data
// when the restored file is opened.
func checkSchemaObjects(ctx context.Context, db *sql.DB, version int) error {
	want, err := expectedObjects(ctx, version)
	if err != nil {
		return err
	}
	rows, err := db.QueryContext(ctx, `SELECT type, name FROM sqlite_master`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var typ, name string
		if err := rows.Scan(&typ, &name); err != nil {
			return err
		}
		if !want[typ+" "+name] && !strings.HasPrefix(name, "sqlite_autoindex_") {
			return fmt.Errorf("unexpected %s %q in a core database of schema %d", typ, name, version)
		}
	}
	return rows.Err()
}

// expectedObjects runs the first version migrations on an empty in-memory database and lists what they made.
func expectedObjects(ctx context.Context, version int) (map[string]bool, error) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	for i := 0; i < version; i++ {
		if err := migrations[i](tx); err != nil {
			return nil, err
		}
	}
	rows, err := tx.Query(`SELECT type, name FROM sqlite_master`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	want := map[string]bool{"table sqlite_sequence": true}
	for rows.Next() {
		var typ, name string
		if err := rows.Scan(&typ, &name); err != nil {
			return nil, err
		}
		want[typ+" "+name] = true
	}
	return want, rows.Err()
}
