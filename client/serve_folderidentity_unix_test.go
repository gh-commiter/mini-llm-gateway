//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAllowedFolderPermissionErrorDoesNotMarkMissing(t *testing.T) {
	base := realOf(t, t.TempDir())
	parent := filepath.Join(base, "parent")
	folder := filepath.Join(parent, "child")
	if err := os.MkdirAll(folder, 0700); err != nil {
		t.Fatal(err)
	}
	s, _ := testServer(t, func(c *serveConfig) { c.AllowedFolders = []string{folder} })
	if err := os.Chmod(parent, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(parent, 0700)
	if _, err := os.Stat(folder); err == nil {
		t.Skip("process can bypass directory permissions")
	}
	if got := s.agentFolders(s.conf(), base); len(got) != 0 {
		t.Fatalf("unreadable folder accepted: %v", got)
	}
	if _, marked := s.wasMissing.Load(folder); marked {
		t.Fatal("permission error marked missing")
	}
	if err := os.Chmod(parent, 0700); err != nil {
		t.Fatal(err)
	}
	if got := s.agentFolders(s.conf(), base); len(got) != 1 {
		t.Fatalf("folder did not recover: %v", got)
	}
}

func TestAllowedFolderForeignOwner(t *testing.T) {
	folder := realOf(t, "/usr/bin")
	if os.Getuid() == 0 {
		folder = realOf(t, t.TempDir())
	}
	s, _ := testServer(t, func(c *serveConfig) { c.AllowedFolders = []string{folder} })
	if os.Getuid() == 0 {
		if err := os.Chown(folder, 1, -1); err != nil {
			t.Fatal(err)
		}
		defer os.Chown(folder, 0, -1)
	} else if ownedByMe(folder) {
		t.Fatal("test needs a system folder owned by another user")
	}
	if got := s.agentFolders(s.conf(), t.TempDir()); len(got) != 0 {
		t.Fatalf("foreign folder accepted: %v", got)
	}
}

func TestAllowedFolderOpenDoesNotFollowFinalLink(t *testing.T) {
	path := realOf(t, t.TempDir())
	id, err := fileIDOf(path)
	if err != nil {
		t.Fatal(err)
	}
	checked := checkedFolders{{path, id}}
	if err := os.Rename(path, path+"-old"); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(path + "-old")
	if err := os.Symlink(path+"-old", path); err != nil {
		t.Fatal(err)
	}
	// Even a link to the same inode must not replace the checked final directory.
	held, err := checked.open()
	closeFolders(held)
	if err == nil {
		t.Fatal("followed a final symlink")
	}
}

// B1: the persisted identity is the stable volume id + inode, never st_dev: a folder whose volume id is unchanged is
// accepted, another inode is refused, and without a volume id only the inode counts.
func TestAllowedFolderRecordIgnoresDeviceNumber(t *testing.T) {
	base := realOf(t, t.TempDir())
	folder := filepath.Join(base, "folder")
	if err := os.Mkdir(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	old := volumeIDFn
	t.Cleanup(func() { volumeIDFn = old })
	volumeIDFn = func(string) uint64 { return 0xabc }
	s, _ := testServer(t, func(c *serveConfig) { c.AllowedFolders = []string{folder} })
	path := s.cfgPath
	releaseScratch()
	if file, _ := loadServeConfig(path); file.AllowedRecords[folder].Vol != 0xabc {
		t.Fatalf("record: %+v", file.AllowedRecords[folder])
	}
	// "Reconnected": st_dev is not part of the record, the volume id is the same: accepted.
	s, _ = startFrom(t, path)
	if got := s.agentFolders(s.conf(), base); len(got) != 1 {
		t.Fatalf("same volume id and inode refused: %v", got)
	}
	// Another volume id (a different disk at the same path): refused.
	volumeIDFn = func(string) uint64 { return 0xdef }
	if got := s.agentFolders(s.conf(), base); len(got) != 0 {
		t.Fatalf("other volume accepted: %v", got)
	}
	// No volume id available now or when recorded: the inode alone decides.
	volumeIDFn = func(string) uint64 { return 0 }
	if got := s.agentFolders(s.conf(), base); len(got) != 1 {
		t.Fatalf("inode only: %v", got)
	}
	// Another inode (replaced folder) is refused whatever the volume id says.
	volumeIDFn = func(string) uint64 { return 0xabc }
	if err := os.Rename(folder, folder+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	if got := s.agentFolders(s.conf(), base); len(got) != 0 {
		t.Fatalf("replaced folder accepted: %v", got)
	}
}

// B2: a record without identity (the folder was missing when recorded) adopts the identity at its first accepted use,
// in memory and in serve.json; afterwards a replacement is refused.
func TestAllowedFolderRecordWithoutIdentityIsAdopted(t *testing.T) {
	base := realOf(t, t.TempDir())
	folder := filepath.Join(base, "folder")
	if err := os.Mkdir(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	s, _ := testServer(t, func(c *serveConfig) { c.AllowedFolders = []string{folder} })
	path := s.cfgPath
	releaseScratch()
	file, _ := loadServeConfig(path)
	file.AllowedRecords[folder] = folderRecord{Real: folder}
	if err := writeServeConfig(path, file); err != nil {
		t.Fatal(err)
	}
	s, _ = startFrom(t, path)
	if got := s.agentFolders(s.conf(), base); len(got) != 1 {
		t.Fatalf("folder refused: %v", got)
	}
	want, _ := recordIDOf(folder)
	if got, _ := loadServeConfig(path); got.AllowedRecords[folder] != (folderRecord{Real: folder, Vol: want.dev, Ino: want.ino}) || want.ino == 0 {
		t.Fatalf("not saved: %+v", got.AllowedRecords[folder])
	}
	if s.conf().AllowedRecords[folder].Ino != want.ino {
		t.Fatalf("not adopted in memory: %+v", s.conf().AllowedRecords[folder])
	}
	if err := os.Rename(folder, folder+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(folder, 0o700); err != nil {
		t.Fatal(err)
	}
	if got := s.agentFolders(s.conf(), base); len(got) != 0 {
		t.Fatalf("replacement accepted: %v", got)
	}
}

// B4: the lock file is never opened through a link.
func TestOpenLockFileRefusesLink(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "real"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if f, err := openLockFile(filepath.Join(dir, "link")); err == nil {
		f.Close()
		t.Fatal("lock file opened through a link")
	}
}
