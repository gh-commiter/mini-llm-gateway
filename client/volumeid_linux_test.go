package main

import "testing"

func TestMountVolumeIDUsesSourceAndFilesystemType(t *testing.T) {
	a := mountVolumeID("/mnt/share/folder", []byte("server-a:/share /mnt/share fuse.sshfs rw 0 0\n"))
	b := mountVolumeID("/mnt/share/folder", []byte("server-b:/share /mnt/share fuse.sshfs rw 0 0\n"))
	otherType := mountVolumeID("/mnt/share/folder", []byte("server-a:/share /mnt/share nfs rw 0 0\n"))
	if a == 0 || a == b || a == otherType {
		t.Fatalf("mount identities collide or are missing: %x %x %x", a, b, otherType)
	}
}
