package main

import (
	"crypto/sha256"
	"encoding/binary"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// volumeIDOf uses the volume UUID where available, then a qualified network mount source.
// Neither f_fsid nor local device names are stable across macOS mount order changes.
func volumeIDOf(p string) uint64 {
	// getattrlist returns the same volume UUID used by the Mac app.
	name, err := syscall.BytePtrFromString(p)
	if err != nil {
		return 0
	}
	attrs := struct {
		Count                           uint16
		Reserved                        uint16
		Common, Volume, Dir, File, Fork uint32
	}{Count: 5, Volume: unix.ATTR_VOL_INFO | unix.ATTR_VOL_UUID}
	var result [20]byte // length followed by the 16-byte UUID
	_, _, errno := syscall.Syscall6(syscall.SYS_GETATTRLIST, uintptr(unsafe.Pointer(name)),
		uintptr(unsafe.Pointer(&attrs)), uintptr(unsafe.Pointer(&result[0])), uintptr(len(result)), 0, 0)
	if errno == 0 && binary.NativeEndian.Uint32(result[:4]) == uint32(len(result)) && resultUUIDPresent(result[4:]) {
		sum := sha256.Sum256(result[4:])
		return binary.BigEndian.Uint64(sum[:])
	}

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

func resultUUIDPresent(uuid []byte) bool {
	for _, b := range uuid {
		if b != 0 {
			return true
		}
	}
	return false
}
