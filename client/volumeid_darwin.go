package main

import (
	"strings"
	"syscall"
)

// volumeIDOf (B1): macOS f_fsid is the device number, which changes with mount order. For network mounts,
// use the filesystem type and mount source instead. Local device names have the same instability, so they return 0.
func volumeIDOf(p string) uint64 {
	var st syscall.Statfs_t
	if syscall.Statfs(p, &st) != nil {
		return 0
	}
	source := volumeIDString(st.Mntfromname[:])
	if strings.HasPrefix(source, "/dev/") {
		return 0
	}
	return hashVolumeID(volumeIDString(st.Fstypename[:]), source)
}

func volumeIDString(s []int8) string {
	b := make([]byte, 0, len(s))
	for _, c := range s {
		if c == 0 {
			break
		}
		b = append(b, byte(c))
	}
	return string(b)
}
