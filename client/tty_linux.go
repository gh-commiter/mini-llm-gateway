package main

import (
	"os"

	"golang.org/x/sys/unix"
)

// stdinIsTTY is isatty(0), as Swift asks it: a terminal, never another character device (/dev/zero, /dev/null).
func stdinIsTTY() bool {
	_, err := unix.IoctlGetTermios(int(os.Stdin.Fd()), unix.TCGETS)
	return err == nil
}
