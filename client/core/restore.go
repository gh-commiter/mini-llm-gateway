package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Restore replaces <dir>/core.db with the backup src. The gateway must be stopped: it takes the folder lock an
// open store holds. src is validated first (integrity, schema not newer, same gateway id). The old file is kept as
// a snapshot, the epoch is set above both the old and the restored value, the project-id mark is the maximum of
// all, and the result goes to core.audit.log. Any failure leaves the previous database in place.
func Restore(dir, src string) (err error) {
	detail := map[string]any{"source": src}
	defer func() {
		if err != nil {
			detail["error"] = err.Error()
			appendAudit(dir, "restore.failed", detail)
		}
	}()
	lock, err := lockDir(dir)
	if err != nil {
		return err
	}
	defer lock.Close()
	ctx := context.Background()

	side, err := readState(dir)
	if err != nil {
		return err
	}
	cur, oldSchema, err := currentState(ctx, dir, side)
	if err != nil {
		return err
	}
	in, err := validate(ctx, src, cur.GatewayID)
	if err != nil {
		return err
	}
	next := state{
		GatewayID:   in.GatewayID,
		Epoch:       max(cur.Epoch, in.Epoch) + 1,
		ProjectHigh: max(cur.ProjectHigh, in.ProjectHigh),
	}
	detail["epoch_from"], detail["epoch_to"], detail["project_high"] = max(cur.Epoch, in.Epoch), next.Epoch, next.ProjectHigh

	// Build the new file next to the old one, so the final step is a rename on the same volume.
	tmp := filepath.Join(dir, ".core-restore.tmp")
	cleanup := func() { os.Remove(tmp); os.Remove(tmp + "-wal"); os.Remove(tmp + "-shm") }
	cleanup()
	defer cleanup()
	if err := copyFile(src, tmp); err != nil {
		return err
	}
	if err := stamp(ctx, tmp, next, src); err != nil {
		return err
	}
	if err := writeState(dir, next); err != nil { // before the swap: ahead of the database is harmless
		return err
	}

	db := filepath.Join(dir, dbFile)
	snap := ""
	if _, err := os.Lstat(db); err == nil {
		snap = filepath.Join(dir, snapshotName(oldSchema))
		detail["snapshot"] = filepath.Base(snap)
		if err := os.Rename(db, snap); err != nil {
			return err
		}
		// The old WAL belongs to the old file: it goes with the snapshot, and the SHM (a cache) is dropped,
		// so neither can ever be applied to the new file.
		if err := os.Rename(db+"-wal", snap+"-wal"); err != nil && !errors.Is(err, os.ErrNotExist) {
			os.Rename(snap, db)
			return err
		}
		os.Remove(db + "-shm")
	}
	if err := os.Rename(tmp, db); err != nil {
		if snap != "" { // put the previous database back
			os.Rename(snap, db)
			os.Rename(snap+"-wal", db+"-wal")
		}
		return err
	}
	syncDir(dir)
	pruneSnapshots(dir)
	return appendAudit(dir, "restore", detail)
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
	fi, err := os.Lstat(src)
	if err != nil {
		return state{}, err
	}
	if !fi.Mode().IsRegular() {
		return state{}, errors.New(src + " is not a regular file")
	}
	db, err := sql.Open("sqlite", dsn(src, "mode=ro&immutable=1"))
	if err != nil {
		return state{}, err
	}
	defer db.Close()
	var res string
	if err := db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&res); err != nil || res != "ok" {
		return state{}, fmt.Errorf("%s is not a valid core database (integrity check: %v %v)", src, res, err)
	}
	v, err := schemaVersion(ctx, db)
	if err != nil || v == 0 {
		return state{}, fmt.Errorf("%s is not a core database", src)
	}
	if v > len(migrations) {
		return state{}, fmt.Errorf("%w (schema %d, program %d)", ErrNewerSchema, v, len(migrations))
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

// stamp writes the new state and the restore event into the copy, and folds its WAL into the file.
func stamp(ctx context.Context, path string, st state, src string) error {
	db, err := openDB(path)
	if err != nil {
		return err
	}
	defer db.Close()
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
