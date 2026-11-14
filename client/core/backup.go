package core

import (
	"context"
	"os"
	"strings"
)

// Backup writes a consistent snapshot of the open database to dest (a new file, mode 0600). It never overwrites.
// The sidecar is not part of a backup: the epoch and id marks must survive a restore, not be restored.
func (s *Store) Backup(ctx context.Context, dest string) error {
	return s.copyTo(ctx, dest)
}

func (s *Store) copyTo(ctx context.Context, dest string) error {
	if err := createPrivate(dest); err != nil { // O_EXCL: refuses an existing file; VACUUM INTO accepts an empty one
		return err
	}
	_, err := s.db.ExecContext(ctx, `VACUUM INTO '`+strings.ReplaceAll(dest, "'", "''")+`'`)
	if err != nil {
		os.Remove(dest)
	}
	return err
}
