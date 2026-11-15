package main

import (
	"syscall"
	"testing"
)

func TestMountVolumeIDUsesSourceAndFilesystemType(t *testing.T) {
	a := mountVolumeID("/mnt/share/folder", []byte("server-a:/share /mnt/share fuse.sshfs rw 0 0\n"))
	b := mountVolumeID("/mnt/share/folder", []byte("server-b:/share /mnt/share fuse.sshfs rw 0 0\n"))
	otherType := mountVolumeID("/mnt/share/folder", []byte("server-a:/share /mnt/share nfs rw 0 0\n"))
	if a == 0 || a == b || a == otherType {
		t.Fatalf("mount identities collide or are missing: %x %x %x", a, b, otherType)
	}
}

func TestMountVolumeIDRejectsUnstableSources(t *testing.T) {
	for _, source := range []string{"tmpfs", "none", "/dev/sdb1", "/dev/loop0"} {
		if got := mountVolumeID("/mnt/folder", []byte(source+" /mnt tmpfs rw 0 0\n")); got != 0 {
			t.Fatalf("unstable source %q recorded as %x", source, got)
		}
	}
}

func TestMountVolumeIDMountSelection(t *testing.T) {
	data := []byte("server:/root / nfs rw 0 0\nserver:/outer /mnt nfs rw 0 0\nserver:/inner /mnt/a\\040b nfs rw 0 0\n")
	if got := mountVolumeID("/mnt/a b/folder", data); got != hashVolumeID("nfs", "server:/inner") {
		t.Fatalf("escaped deepest mount: %x", got)
	}
	if got := mountVolumeID("/mnt-other/folder", data); got != hashVolumeID("nfs", "server:/root") {
		t.Fatalf("mount boundary: %x", got)
	}
}

func TestVolumeIDPreservesLegacyUnlistedFilesystemRecord(t *testing.T) {
	// exFAT was not in the UUID-derived filesystem allowlist.
	st := syscall.Statfs_t{Type: 0x2011BAB0}
	st.Fsid.X__val = [2]int32{0x12345678, -1}
	const legacyID uint64 = 0x12345678ffffffff
	record := folderRecord{Vol: legacyID, Ino: 42}
	// A nonzero f_fsid must work without consulting the path or mount source.
	current := volumeIDFromStatfs("/nonexistent-legacy-volume/folder", st)
	if current != legacyID || !record.matches(fileID{dev: current, ino: 42}) {
		t.Fatalf("legacy record refused: recorded %x, current %x", record.Vol, current)
	}
}
