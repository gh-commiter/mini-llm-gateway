//go:build !windows

package main

import "syscall"

// fileIDOf (#84): istovetnost mape (st_dev, st_ino; sledi povezavam), napaka = ni je mogoče ugotoviti.
func fileIDOf(p string) (fileID, error) {
	var st syscall.Stat_t
	if err := syscall.Stat(p, &st); err != nil {
		return fileID{}, err
	}
	return fileID{uint64(st.Dev), uint64(st.Ino)}, nil
}
