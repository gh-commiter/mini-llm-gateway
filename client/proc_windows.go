package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"unsafe"
)

const createNoWindow = 0x08000000 // CREATE_NO_WINDOW

// hideConsole: otrok dobi skrito konzolo (tudi za svoje otroke, npr. conpty), ne vidnega okna; obstoječe zastavice
// (CREATE_SUSPENDED za Job, CREATE_NEW_PROCESS_GROUP …) ostanejo. Ne HideWindow (SW_HIDE): ta skrije tudi okna
// programov GUI (msiexec /passive, prvo sporočilo pladnja), ki bi nato nevidno čakali.
func hideConsole(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= createNoWindow
}

// refreshPath: dosedanjemu PATH doda manjkajoče mape iz registra (sistemski in uporabnikov, razširjen) in ~\.local\bin.
// Pladenj po namestitvi zažene Raziskovalec (trayRun); tu le manjkajoče mape za pladenj ali ukazno vrstico iz lupine,
// odprte pred namestitvijo CLI. Velja za LookPath in za vse otroke.
func refreshPath() {
	sys := regString(0x80000002, `SYSTEM\CurrentControlSet\Control\Session Manager\Environment`) // HKLM
	usr := regString(0x80000001, `Environment`)                                                  // HKCU
	home, _ := os.UserHomeDir()
	os.Setenv("PATH", mergePath(";", os.Getenv("PATH"), sys, usr, filepath.Join(home, ".local", "bin")))
}

// regString: vrednost Path ključa, razširjena (%USERPROFILE% …); "" ob napaki.
func regString(root uintptr, key string) string {
	get := syscall.NewLazyDLL("advapi32.dll").NewProc("RegGetValueW")
	const flags = 0x00000002 | 0x00000004 | 0x10000000 // RRF_RT_REG_SZ | RRF_RT_REG_EXPAND_SZ | RRF_NOEXPAND
	var size uint32
	k, v := u16(key), u16("Path")
	if r, _, _ := get.Call(root, uintptr(unsafe.Pointer(k)), uintptr(unsafe.Pointer(v)), flags, 0, 0, uintptr(unsafe.Pointer(&size))); r != 0 || size == 0 {
		return ""
	}
	buf := make([]uint16, size/2+1)
	if r, _, _ := get.Call(root, uintptr(unsafe.Pointer(k)), uintptr(unsafe.Pointer(v)), flags, 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size))); r != 0 {
		return ""
	}
	raw := syscall.UTF16ToString(buf)
	exp := syscall.NewLazyDLL("kernel32.dll").NewProc("ExpandEnvironmentStringsW")
	out := make([]uint16, 32768)
	if n, _, _ := exp.Call(uintptr(unsafe.Pointer(u16(raw))), uintptr(unsafe.Pointer(&out[0])), uintptr(len(out))); n == 0 || int(n) > len(out) {
		return raw
	}
	return syscall.UTF16ToString(out)
}
