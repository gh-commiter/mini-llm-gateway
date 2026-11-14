//go:build !windows

package main

import (
	"os"
	"syscall"
)

// openNoFollow: a link at path is an error (ELOOP), never followed.
func openNoFollow(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
}

// ownedSingleLink: a file of this user that has no other name.
func ownedSingleLink(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Getuid() && st.Nlink == 1
}
