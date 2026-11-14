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

// copyTo writes a consistent copy to a new file. A snapshot goes straight into the private folder. An explicit
// destination may be anywhere (a shared folder): the copy is made in the private folder first, then the destination
// is created by us (O_EXCL, never a path somebody else swapped in), protected for this user, and only THEN filled
// through our own open descriptor. So no content ever sits in a file with a wider ACL, and nothing is written
// through a path that could have been replaced after the check (L3).
func (s *Store) copyTo(ctx context.Context, dest string, explicit bool) error {
	dest, err := filepath.Abs(dest)
	if err != nil {
		return err
	}
	if !explicit {
		if err := createPrivate(dest); err != nil {
			return err
		}
		if err := s.vacuumInto(ctx, dest); err != nil {
			os.Remove(dest)
			return err
		}
		return nil
	}
	tmp := filepath.Join(s.dir, ".backup-"+randID()+".tmp")
	if err := createPrivate(tmp); err != nil {
		return err
	}
	defer os.Remove(tmp)
	if err := s.vacuumInto(ctx, tmp); err != nil {
		return err
	}
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) // refuses an existing file or link
	if err != nil {
		return err
	}
	protect := s.opts.ProtectFile
	if protect == nil {
		protect = protectFile
	}
	if err := protect(dest); err != nil {
		out.Close()
		os.Remove(dest)
		return fmt.Errorf("cannot protect %s: %w", dest, err)
	}
	in, err := os.Open(tmp)
	if err == nil {
		_, err = out.ReadFrom(in)
		in.Close()
	}
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(dest)
	}
	return err
}

func (s *Store) vacuumInto(ctx context.Context, path string) error {
	_, err := s.db.ExecContext(ctx, `VACUUM INTO '`+strings.ReplaceAll(path, "'", "''")+`'`)
	return err
}
