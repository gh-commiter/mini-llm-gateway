package main

import (
	"os"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

func mutationHandlePath(f *os.File) (string, error) {
	var buf [4096]byte
	_, _, e := syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), unix.F_GETPATH, uintptr(unsafe.Pointer(&buf[0])))
	if e != 0 {
		return "", e
	}
	return string(buf[:lenUntilZero(buf[:])]), nil
}
func mutationRename(src *os.File, sn string, dst *os.File, dn string) error {
	return unix.Renameat(int(src.Fd()), sn, int(dst.Fd()), dn)
}
func mutationOpenFile(r *os.Root, n string, flags int, mode os.FileMode) (*os.File, error) {
	parent, err := r.Open(".")
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	fd, err := unix.Openat(int(parent.Fd()), n, flags|unix.O_NOFOLLOW|unix.O_CLOEXEC, uint32(mode.Perm()))
	if err != nil {
		return nil, &os.PathError{Op: "openat", Path: n, Err: err}
	}
	return os.NewFile(uintptr(fd), n), nil
}

func lenUntilZero(b []byte) int {
	for i, v := range b {
		if v == 0 {
			return i
		}
	}
	return len(b)
}
