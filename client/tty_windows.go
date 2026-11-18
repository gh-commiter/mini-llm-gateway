package main

import (
	"os"

	"golang.org/x/sys/windows"
)

// stdinIsTTY: standard input is a console (the isatty of Windows), never a file, pipe or the NUL device.
func stdinIsTTY() bool {
	var mode uint32
	return windows.GetConsoleMode(windows.Handle(os.Stdin.Fd()), &mode) == nil
}
