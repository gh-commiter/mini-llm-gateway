package main

import (
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

// openPTY: the controlling side and the terminal's path (unlockpt, ptsname).
func openPTY() (*os.File, string, error) {
	fd, err := unix.Open("/dev/ptmx", unix.O_RDWR|unix.O_NOCTTY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, "", err
	}
	n, err := unix.IoctlGetUint32(fd, unix.TIOCGPTN)
	if err == nil {
		err = unix.IoctlSetPointerInt(fd, unix.TIOCSPTLCK, 0)
	}
	if err != nil {
		unix.Close(fd)
		return nil, "", err
	}
	return os.NewFile(uintptr(fd), "ptmx"), "/dev/pts/" + strconv.Itoa(int(n)), nil
}
