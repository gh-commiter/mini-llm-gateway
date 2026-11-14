//go:build !windows

package main

import "syscall"

// gitWatchPidAlive: whether a process with this id exists (signal 0).
func gitWatchPidAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
