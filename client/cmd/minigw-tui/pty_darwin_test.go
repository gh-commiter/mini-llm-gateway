package main

import (
	"bytes"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// openPTY: the controlling side and the terminal's path (posix_openpt, grantpt, unlockpt, ptsname).
func openPTY() (*os.File, string, error) {
	fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, "", err
	}
	var name [128]byte
	for _, req := range []uintptr{unix.TIOCPTYGRANT, unix.TIOCPTYUNLK, unix.TIOCPTYGNAME} {
		arg := uintptr(0)
		if req == unix.TIOCPTYGNAME {
			arg = uintptr(unsafe.Pointer(&name[0]))
		}
		if _, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), req, arg); e != 0 {
			unix.Close(fd)
			return nil, "", e
		}
	}
	return os.NewFile(uintptr(fd), "ptmx"), string(name[:bytes.IndexByte(name[:], 0)]), nil
}
