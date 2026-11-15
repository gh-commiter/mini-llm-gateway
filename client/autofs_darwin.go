package main

import "syscall"

func isAutofs(p string) (bool, error) {
	var st syscall.Statfs_t
	err := syscall.Statfs(p, &st)
	return volumeIDString(st.Fstypename[:]) == "autofs", err
}
