package core

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Backup writes a consistent snapshot of the open database to dest (a new file, mode 0600). It never overwrites.
// The sidecar is not part of a backup: the epoch and id marks must survive a restore, not be restored.
func (s *Store) Backup(ctx context.Context, dest string) error {
	return s.copyTo(ctx, dest, true)
}

// copyTo writes a consistent copy to a new file. With explicit = true the destination may be outside the private
// folder, so it is protected (ACL or mode) before any content is written, and removed if that fails.
func (s *Store) copyTo(ctx context.Context, dest string, explicit bool) error {
	dest, err := filepath.Abs(dest)
	if err != nil {
		return err
	}
	if err := createPrivate(dest); err != nil { // O_EXCL: refuses an existing file; VACUUM INTO accepts an empty one
		return err
	}
	if explicit {
		protect := s.opts.ProtectFile
		if protect == nil {
			protect = protectFile
		}
		if err := protect(dest); err != nil {
			os.Remove(dest)
			return fmt.Errorf("cannot protect %s: %w", dest, err)
		}
	}
	_, err = s.db.ExecContext(ctx, `VACUUM INTO '`+strings.ReplaceAll(dest, "'", "''")+`'`)
	if err != nil {
		os.Remove(dest)
	}
	return err
}
