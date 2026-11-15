//go:build windows

package main

import (
	"strconv"

	"golang.org/x/sys/windows"
)

// gitWatchProcStart: the creation time of the process ("" = not known).
func gitWatchProcStart(pid int) string {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)
	var created, exit, kernel, user windows.Filetime
	if windows.GetProcessTimes(h, &created, &exit, &kernel, &user) != nil {
		return ""
	}
	return strconv.FormatInt(created.Nanoseconds(), 10)
}

// gitWatchPidAlive: whether the process that wrote the state file still runs: a process with this id is running and, when the start time was stored and can be read, it is the
// same one (a reused process id is another process).
func gitWatchPidAlive(pid int, started string) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	var code uint32
	running := windows.GetExitCodeProcess(h, &code) == nil && code == 259 // STILL_ACTIVE
	windows.CloseHandle(h)
	now := gitWatchProcStart(pid)
	return running && (started == "" || now == "" || now == started)
}
