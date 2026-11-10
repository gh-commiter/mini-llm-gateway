package main

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// killInside (L4, N1): pred finalize procesi agenta s trenutno mapo v klonu ali z njegovim GIT_WORK_TREE v okolju, ki so
// ostali v ozadju. Le procesi v seji
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
		if len(f) < 4 {
			continue
		}
		// N1: tudi proces, ki je zapustil sejo (setsid) in klon (cd /): okolje agenta nosi GIT_WORK_TREE tega klona
		// (worktreeHarnessEnv); environ je berljiv le za procese istega uporabnika.
		env, _ := os.ReadFile("/proc/" + e.Name() + "/environ")
		marked := false
		for _, k := range []string{"GIT_WORK_TREE=", "MINIGW_CLONE="} { // #40: MINIGW_CLONE pri agentih z ovojem git (brez GIT_WORK_TREE)
			marked = marked || strings.Contains("\x00"+string(env), "\x00"+k+wt+"\x00") || strings.Contains("\x00"+string(env), "\x00"+k+root+"\x00")
		}
		if !marked && (f[3] != strconv.Itoa(int(mySid)) || f[3] == e.Name()) { // brez znaka le v seji prehoda, ne njen vodja
			continue
		}
		fd, _, errno := syscall.Syscall(434 /* pidfd_open */, uintptr(pid), 0, 0)
		if errno != 0 {
			continue
		}
		if cwd, err := os.Readlink("/proc/" + e.Name() + "/cwd"); marked || err == nil && (cwd == root || strings.HasPrefix(cwd, root+"/")) {
			syscall.Syscall6(424 /* pidfd_send_signal */, fd, uintptr(syscall.SIGKILL), 0, 0, 0, 0)
		}
		syscall.Close(int(fd))
	}
}
