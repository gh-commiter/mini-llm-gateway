//go:build !windows

package main

import (
	"crypto/sha256"
	"encoding/binary"
	"syscall"
)

// hashVolumeID combines the filesystem type and mount source without ambiguous concatenation.
func hashVolumeID(fsType, source string) uint64 {
	if fsType == "" || source == "" {
		return 0
	}
	sum := sha256.Sum256([]byte(fsType + "\x00" + source))
	return binary.BigEndian.Uint64(sum[:])
}

// fileIDOf (#84): istovetnost mape (st_dev, st_ino; sledi povezavam), napaka = ni je mogoče ugotoviti.
func fileIDOf(p string) (fileID, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(p, &st); err != nil {
		return fileID{}, err
	}
	return fileID{uint64(st.Dev), uint64(st.Ino)}, nil
}

// volumeIDFn: test hook.
var volumeIDFn = volumeIDOf

// recordIDOf (B1): the identity that is persisted and compared across restarts: stable volume id (0 = none) + inode.
// Not st_dev: it is assigned at mount/boot and changes for external and network disks.
func recordIDOf(p string) (fileID, error) {
	id, err := fileIDOf(p)
	if err != nil {
		return fileID{}, err
	}
	return fileID{volumeIDFn(p), id.ino}, nil
}
