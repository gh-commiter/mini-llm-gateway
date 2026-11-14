//go:build !windows

package main

import (
	"fmt"
	"os"
	"syscall"
)

func openCheckedFolder(f checkedFolder) (*os.File, error) {
	fd, err := syscall.Open(f.path, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	h := os.NewFile(uintptr(fd), f.path)
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil || (fileID{uint64(st.Dev), uint64(st.Ino)}) != f.id || st.Uid != uint32(os.Getuid()) {
		h.Close()
		return nil, fmt.Errorf("allowed folder changed before agent start: %s", f.path)
	}
	return h, nil
}
