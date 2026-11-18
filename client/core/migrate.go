package core

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// migrate brings the schema to len(migrations): one snapshot of the old file first, then one transaction per
// migration. A failing migration rolls back alone; the schema version stays at the last one that succeeded.
func (s *Store) migrate(ctx context.Context) error {
	cur, err := schemaVersion(ctx, s.db)
	if err != nil {
		return err
	}
	if cur > len(migrations) {
		return fmt.Errorf("%w (schema %d, program %d)", ErrNewerSchema, cur, len(migrations))
	}
	if cur == len(migrations) {
		return nil
	}
	snap := ""
	if cur > 0 && s.opts.OnUpgrade != nil {
		s.opts.OnUpgrade(cur, len(migrations))
	}
	if cur > 0 { // nothing to protect in a new file
		if snap, err = s.snapshot(ctx, cur); err != nil {
			return fmt.Errorf("snapshot before migration: %w", err)
		}
	}
	detail := map[string]any{"from": cur, "to": len(migrations), "snapshot": filepath.Base(snap)}
	for i := cur; i < len(migrations); i++ { // one transaction per migration; a failure keeps 1..i applied
		err = s.write(ctx, func(tx *sql.Tx) error {
			if err := migrations[i](tx); err != nil {
				return err
			}
			return setMeta(tx, "schema", strconv.Itoa(i+1))
		})
		if err != nil {
			err = fmt.Errorf("migration %d: %w", i+1, err)
			detail["error"], detail["applied"] = err.Error(), i
			appendAudit(s.dir, "migrate.failed", detail)
			return err
		}
	}
	s.migrated = &Migration{From: cur, To: len(migrations), Snapshot: snap}
	return appendAudit(s.dir, "migrate", detail)
}

// Migration is what the last Open did to an older database: the schema it came from and the snapshot of the old file (in the
// core folder itself, named core-<schema>-<time>.db; not in backups/, which holds the owner's explicit backups).
type Migration struct {
	From, To int
	Snapshot string // full path; "" for a new database
}

// Migrated reports the migration this Open ran, nil when the database was already current.
func (s *Store) Migrated() *Migration { return s.migrated }

var snapRe = regexp.MustCompile(`^core-(\d+)-(\d{8}T\d{6}\.\d{3}Z)\.db$`)

const partialSuffix = ".partial"

var partialRe = regexp.MustCompile(`^core-\d+-\d{8}T\d{6}\.\d{3}Z\.db\.partial(-wal|-shm|-journal)?$`)

// removeStalePartials removes the unfinished snapshot copies of a killed earlier start: only files of exactly the name this code gives them.
func removeStalePartials(dir string) {
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if e.Type().IsRegular() && partialRe.MatchString(e.Name()) {
			os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// snapshotName is core-<schema>-<UTC time>.db.
func snapshotName(schema int) string {
	return fmt.Sprintf("core-%d-%s.db", schema, time.Now().UTC().Format("20060102T150405.000Z"))
}

// snapshot writes a consistent copy of the open database as a snapshot in the folder and prunes old ones. A copy
// identical to an existing snapshot of the same schema (a failed start repeated) is not kept: that one is returned.
func (s *Store) snapshot(ctx context.Context, schema int) (string, error) {
	p := filepath.Join(s.dir, snapshotName(schema))
	// The copy is written under a name that is not a snapshot's and renamed when complete: a helper killed in the middle (a very large
	// database, a slow disk) leaves a file that never counts as one of the kept snapshots; removeStalePartials removes it at the next Open.
	if err := s.copyTo(ctx, p+partialSuffix, false); err != nil {
		return "", err
	}
	if err := os.Rename(p+partialSuffix, p); err != nil {
		os.Remove(p + partialSuffix)
		return "", err
	}
	if old := sameSnapshot(s.dir, p, schema); old != "" {
		os.Remove(p)
		os.Remove(p + "-wal")
		return old, nil
	}
	pruneSnapshots(s.dir)
	return p, nil
}

// sameSnapshot returns an earlier snapshot of the schema whose content equals the file at p, or "".
func sameSnapshot(dir, p string, schema int) string {
	want, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		m := snapRe.FindStringSubmatch(e.Name())
		if m == nil || m[1] != strconv.Itoa(schema) || e.Name() == filepath.Base(p) {
			continue
		}
		if got, err := os.ReadFile(filepath.Join(dir, e.Name())); err == nil && bytes.Equal(got, want) {
			return filepath.Join(dir, e.Name())
		}
	}
	return ""
}

// failedSeriesSince returns the time of the last completed migration when a migration has failed since (an upgrade
// that is still unfinished), else ok false.
func failedSeriesSince(dir string) (since time.Time, ok bool) {
	b, err := os.ReadFile(filepath.Join(dir, auditFile))
	if err != nil {
		return since, false
	}
	for _, line := range strings.Split(string(b), "\n") {
		var e struct {
			Time   time.Time
			Action string
		}
		if json.Unmarshal([]byte(line), &e) != nil {
			continue
		}
		switch e.Action {
		case "migrate":
			since, ok = e.Time, false
		case "migrate.failed":
			ok = true
		}
	}
	return since, ok
}

// pruneSnapshots keeps the newest keepSnaps files (by the time in the name). A snapshot made by a restore is
// subject to the same rule. While an upgrade has failed and not completed, the oldest snapshot of the lowest schema made
// since the last completed migration (the file from before the upgrade) is kept in addition: repeated failing
// starts must not push it out. Best effort.
func pruneSnapshots(dir string) {
	type snap struct {
		name, at string
		schema   int
	}
	ents, _ := os.ReadDir(dir)
	var all []snap
	for _, e := range ents {
		if m := snapRe.FindStringSubmatch(e.Name()); m != nil {
			n, _ := strconv.Atoi(m[1])
			all = append(all, snap{e.Name(), m[2], n})
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].at < all[j].at })
	keep := ""
	if since, failed := failedSeriesSince(dir); failed {
		low := -1
		for _, sn := range all {
			if t, err := time.Parse("20060102T150405.000Z", sn.at); err != nil || t.Before(since) {
				continue
			}
			if low < 0 || sn.schema < low { // strictly lower: the oldest of equal schemas stays
				low, keep = sn.schema, sn.name
			}
		}
	}
	for len(all) > keepSnaps {
		sn := all[0]
		all = all[1:]
		if sn.name == keep {
			continue
		}
		p := filepath.Join(dir, sn.name)
		os.Remove(p)
		os.Remove(p + "-wal")
	}
}
