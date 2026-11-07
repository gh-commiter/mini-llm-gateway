package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// killInside (L4): pred finalize procesi agenta s trenutno mapo v klonu, ki so ostali v ozadju. Le procesi v seji
// prehoda (agenti so njegovi potomci), razen vodje seje (lupina, v kateri teče minigw serve); prek pidfd, da ponovno
// uporabljen PID ne dobi signala (pidfd_open → preveri mapo → pidfd_send_signal).
func killInside(wt string) {
	root, err := filepath.EvalSymlinks(wt)
	if err != nil {
		return
	}
	mySid, _, _ := syscall.RawSyscall(syscall.SYS_GETSID, 0, 0, 0)
	ents, _ := os.ReadDir("/proc")
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 1 || pid == os.Getpid() {
			continue
		}
		stat, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		i := strings.LastIndexByte(string(stat), ')')
		if err != nil || i < 0 {
			continue
		}
		f := strings.Fields(string(stat)[i+1:]) // stanje, ppid, pgrp, seja …
		if len(f) < 4 || f[3] != strconv.Itoa(int(mySid)) || f[3] == e.Name() {
			continue
		}
		fd, _, errno := syscall.Syscall(434 /* pidfd_open */, uintptr(pid), 0, 0)
		if errno != 0 {
			continue
		}
		if cwd, err := os.Readlink("/proc/" + e.Name() + "/cwd"); err == nil && (cwd == root || strings.HasPrefix(cwd, root+"/")) {
			syscall.Syscall6(424 /* pidfd_send_signal */, fd, uintptr(syscall.SIGKILL), 0, 0, 0, 0)
		}
		syscall.Close(int(fd))
	}
}
