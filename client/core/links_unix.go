//go:build !windows

package core

import (
	"fmt"
	"os"
	"syscall"
)

// linkInfo: the number of names the file has (st_nlink; 0 when it cannot be told) and its identity (device and inode).
func linkInfo(path string, fi os.FileInfo) (uint64, string) {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Nlink), fmt.Sprint(st.Dev, ":", st.Ino)
	}
	return 0, ""
}
