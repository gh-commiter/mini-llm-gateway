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

// authGen: inode, mtime and size of the file a path leads to (zeros when it is not there).
type authGen struct {
	ino           uint64
	mtimeNs, size int64
}

func fileGen(path string) authGen {
	fi, err := os.Stat(path)
	if err != nil {
		return authGen{}
	}
	g := authGen{mtimeNs: fi.ModTime().UnixNano(), size: fi.Size()}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		g.ino = uint64(st.Ino)
	}
	return g
}
