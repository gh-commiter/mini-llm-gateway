//go:build !windows

package main

import (
	"os"
	"syscall"
)

// fileOwnedByUser: the file belongs to the user running the gateway.
func fileOwnedByUser(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return !ok || int(st.Uid) == os.Getuid()
}
