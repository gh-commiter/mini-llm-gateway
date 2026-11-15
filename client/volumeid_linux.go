package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// volumeIDOf (B1): statfs f_fsid of the filesystem holding p, 0 if unavailable. The kernel derives it from the
// filesystem UUID for ext4, xfs and btrfs (stable across reboots and reconnects). Preserve every nonzero ID for
// existing records; when it is zero, fall back to the filesystem type and mount source.
func volumeIDOf(p string) uint64 {
	var st syscall.Statfs_t
	if syscall.Statfs(p, &st) != nil {
		return 0
	}
	return volumeIDFromStatfs(p, st)
}

func volumeIDFromStatfs(p string, st syscall.Statfs_t) uint64 {
	id := uint64(uint32(st.Fsid.X__val[0]))<<32 | uint64(uint32(st.Fsid.X__val[1]))
	// Existing records contain the raw nonzero f_fsid, regardless of filesystem type.
	if id != 0 {
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
