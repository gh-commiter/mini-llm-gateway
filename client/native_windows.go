package main

import (
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

var wNetGetConnection = syscall.NewLazyDLL("mpr.dll").NewProc("WNetGetConnectionW")

// smbMount: pot UNC (\\strežnik\shema\…) ali omrežni pogon (Z: → \\strežnik\shema).
func smbMount(cwd string) (source, mountPoint string, ok bool) {
	vol := filepath.VolumeName(cwd)
	if strings.HasPrefix(vol, `\\`) {
		return vol, vol, true
	}
	if len(vol) != 2 || vol[1] != ':' {
		return "", "", false
	}
	name, _ := syscall.UTF16PtrFromString(vol)
	buf := make([]uint16, 1024)
	n := uint32(len(buf))
	if r, _, _ := wNetGetConnection.Call(uintptr(unsafe.Pointer(name)), uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&n))); r != 0 {
		return "", "", false // ni omrežni pogon
	}
	return syscall.UTF16ToString(buf), vol + `\`, true
}
