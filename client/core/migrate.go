package core

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"time"
)

// migrate brings the schema to len(migrations). All pending migrations run in ONE transaction (all or nothing),
// after a consistent snapshot of the old file. A failure rolls back, so the old database stays as it was.
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
	if cur > 0 { // nothing to protect in a new file
		if snap, err = s.snapshot(ctx, cur); err != nil {
			return fmt.Errorf("snapshot before migration: %w", err)
		}
	}
	detail := map[string]any{"from": cur, "to": len(migrations), "snapshot": filepath.Base(snap)}
	err = s.write(ctx, func(tx *sql.Tx) error {
		for i := cur; i < len(migrations); i++ {
			if err := migrations[i](tx); err != nil {
				return fmt.Errorf("migration %d: %w", i+1, err)
			}
		}
		return setMeta(tx, "schema", strconv.Itoa(len(migrations)))
	})
	if err != nil {
		detail["error"] = err.Error()
		appendAudit(s.dir, "migrate.failed", detail)
		return err
	}
	return appendAudit(s.dir, "migrate", detail)
}

var snapRe = regexp.MustCompile(`^core-\d+-(\d{8}T\d{6}\.\d{3}Z)\.db$`)

// snapshotName is core-<schema>-<UTC time>.db.
func snapshotName(schema int) string {
	return fmt.Sprintf("core-%d-%s.db", schema, time.Now().UTC().Format("20060102T150405.000Z"))
}

// snapshot writes a consistent copy of the open database as a snapshot in the folder and prunes old ones.
func (s *Store) snapshot(ctx context.Context, schema int) (string, error) {
	p := filepath.Join(s.dir, snapshotName(schema))
	if err := s.copyTo(ctx, p); err != nil {
		return "", err
	}
	pruneSnapshots(s.dir)
	return p, nil
}

// pruneSnapshots keeps the newest keepSnaps files (by the time in the name). A snapshot made by a restore is
// subject to the same rule. Best effort.
func pruneSnapshots(dir string) {
	ents, _ := os.ReadDir(dir)
	var names []string
	for _, e := range ents {
		if snapRe.MatchString(e.Name()) {
			names = append(names, e.Name())
		}
	}
	sort.Slice(names, func(i, j int) bool {
		return snapRe.FindStringSubmatch(names[i])[1] < snapRe.FindStringSubmatch(names[j])[1]
	})
	for len(names) > keepSnaps {
		p := filepath.Join(dir, names[0])
		os.Remove(p)
		os.Remove(p + "-wal")
		names = names[1:]
	}
}
