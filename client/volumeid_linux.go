package main

import "syscall"

// volumeIDOf (B1): statfs f_fsid of the filesystem holding p, 0 if unavailable. The kernel derives it from the
// filesystem UUID for ext4, xfs and btrfs (stable across reboots and reconnects); tmpfs, FUSE and some network
// filesystems report 0 or a per-mount value (then only the inode is compared, which is the best that can be done).
func volumeIDOf(p string) uint64 {
	var st syscall.Statfs_t
	if syscall.Statfs(p, &st) != nil {
		return 0
	}
	return uint64(uint32(st.Fsid.X__val[0]))<<32 | uint64(uint32(st.Fsid.X__val[1]))
}
