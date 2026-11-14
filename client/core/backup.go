package core

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// BackupFolder is the protected folder of the backups: inside the core folder, so every deny of the core folder (the
// Mac sandbox lists, the Linux mask of the config folder, the Windows box) covers it. A backup anywhere else would be a
// full copy of all notes that agents can read (#106).
const BackupFolder = "backups"

var backupName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// BackupNamed writes a backup called name (letters, digits, . _ - ; no path) into <core>/backups and returns its path.
// It never overwrites. The only way to make a backup through the API: no destination outside the protected folder.
func (s *Store) BackupNamed(ctx context.Context, name string) (string, error) {
	if !backupName.MatchString(name) {
		return "", fmt.Errorf("backup name %q: letters, digits, dot, underscore and dash only (at most 64), no path: backups are kept in the core folder's %s folder", name, BackupFolder)
	}
	dir := filepath.Join(s.dir, BackupFolder)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if s.opts.ProtectDir != nil {
		if err := s.opts.ProtectDir(dir); err != nil {
			return "", err
		}
	}
	dest := filepath.Join(dir, name)
	return dest, s.Backup(ctx, dest)
}

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
