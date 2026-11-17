//go:build !windows

package main

import (
	"os"
	"syscall"
)

// openMarker: brez sledenja povezavi (router bi jo lahko podtaknil) in brez čakanja na FIFO.
func openMarker(p string) (*os.File, error) {
	return os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}

func openRootNote(root *os.Root, name string) (*os.File, error) {
	return root.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
}

// protectExported: mode 0600 is set when the file is created through the held folder handle.
func protectExported(string) error { return nil }
