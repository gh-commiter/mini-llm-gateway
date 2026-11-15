//go:build !windows

package main

import (
	"context"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// gitWatchProcStart: when the process started, as a string that only has to be equal for the same process ("" = not known).
func gitWatchProcStart(pid int) string {
	if runtime.GOOS == "linux" {
		b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if err != nil {
			return ""
		}
		s := string(b)
		if i := strings.LastIndexByte(s, ')'); i >= 0 { // the name in parentheses may hold spaces: field 22 counted after it
			if f := strings.Fields(s[i+1:]); len(f) > 19 {
				return f[19]
			}
		}
		return ""
	}
	out, err := commandContext(context.Background(), "ps", "-o", "lstart=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// gitWatchPidAlive: whether the process that wrote the state file still runs: a process with this id exists (signal 0) and, when the start time was stored and can be read,
// it is the same one (a reused process id is another process).
func gitWatchPidAlive(pid int, started string) bool {
	err := syscall.Kill(pid, 0)
	if err != nil && err != syscall.EPERM {
		return false
	}
	now := gitWatchProcStart(pid)
	return started == "" || now == "" || now == started
}
