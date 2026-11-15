package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// volumeIDOf (B1): statfs f_fsid of the filesystem holding p, 0 if unavailable. The kernel derives it from the
// filesystem UUID for ext4, xfs and btrfs (stable across reboots and reconnects). For other filesystems, use the
// filesystem type and mount source so that different network shares do not fall back to the same inode-only identity.
func volumeIDOf(p string) uint64 {
	var st syscall.Statfs_t
	if syscall.Statfs(p, &st) != nil {
		return 0
	}
	id := uint64(uint32(st.Fsid.X__val[0]))<<32 | uint64(uint32(st.Fsid.X__val[1]))
	// Only these filesystems derive f_fsid from a persistent on-disk UUID.
	// tmpfs, overlay and FUSE may report a nonzero per-mount value instead.
	if id != 0 && (st.Type == unix.EXT4_SUPER_MAGIC || st.Type == unix.XFS_SUPER_MAGIC || st.Type == unix.BTRFS_SUPER_MAGIC) {
		return id
	}
	data, err := os.ReadFile("/proc/self/mounts")
	if err != nil {
		return 0
	}
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return 0
	}
	return mountVolumeID(real, data)
}

// mountVolumeID finds the deepest mount containing p and derives its fallback identity from the mount table.
func mountVolumeID(p string, data []byte) uint64 {
	path, err := filepath.Abs(p)
	if err != nil {
		return 0
	}
	unescape := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace
	best, fsType, source := "", "", ""
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		mount := filepath.Clean(unescape(fields[1]))
		prefix := strings.TrimSuffix(mount, string(filepath.Separator)) + string(filepath.Separator)
		if (path != mount && !strings.HasPrefix(path, prefix)) || len(mount) < len(best) {
			continue
		}
		best, fsType, source = mount, unescape(fields[2]), unescape(fields[0])
	}
	if best == "" {
		return 0
	}
	return hashVolumeID(fsType, source)
}
