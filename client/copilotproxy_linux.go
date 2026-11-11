package main

import (
	"os/exec"
	"syscall"
)

// proxyDies (#81 M1, L3): posrednik D-Bus umre z routerjem (ne ostane odprt dostop do shrambe ključev).
func proxyDies(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL} }
