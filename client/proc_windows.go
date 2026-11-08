package main

import (
	"os/exec"
	"syscall"
)

const createNoWindow = 0x08000000 // CREATE_NO_WINDOW

// hideConsole: otrok dobi skrito konzolo (tudi za svoje otroke, npr. conpty), ne vidnega okna; obstoječe zastavice
// (CREATE_SUSPENDED za Job, CREATE_NEW_PROCESS_GROUP …) ostanejo.
func hideConsole(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.HideWindow = true
	cmd.SysProcAttr.CreationFlags |= createNoWindow
}
