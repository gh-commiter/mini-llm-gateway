package main

import (
	"os"
	"syscall"
	"unsafe"
)

// computerName: ime računalnika DNS (GetComputerNameExW, ComputerNamePhysicalDnsHostname), sicer COMPUTERNAME.
func computerName() string {
	const physicalDNSHostname = 5
	p := syscall.NewLazyDLL("kernel32.dll").NewProc("GetComputerNameExW")
	buf := make([]uint16, 256)
	n := uint32(len(buf))
	if r, _, _ := p.Call(physicalDNSHostname, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&n))); r != 0 && n > 0 {
		return syscall.UTF16ToString(buf[:n])
	}
	return os.Getenv("COMPUTERNAME")
}
