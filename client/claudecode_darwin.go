package main

import (
	"encoding/binary"
	"strings"

	"golang.org/x/sys/unix"
)

// procLookup (macOS): starš in ime iz sysctl kern.proc.pid (tudi procesi drugih uporabnikov), pot in argumenti iz
// kern.procargs2 (le procesi tega uporabnika; sicer ime).
func procLookup() procFunc {
	return func(pid int) (int, string, []string, error) {
		kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
		if err != nil {
			return 0, "", nil, err
		}
		exe, args := unix.ByteSliceToString(kp.Proc.P_comm[:]), []string(nil)
		if b, err := unix.SysctlRaw("kern.procargs2", pid); err == nil && len(b) > 4 {
			argc := int(binary.LittleEndian.Uint32(b))
			parts := strings.Split(string(b[4:]), "\x00") // pot programa, ničle za poravnavo, argv, okolje
			exe = parts[0]
			for _, s := range parts[1:] {
				if len(args) == argc {
					break
				}
				if s != "" || len(args) > 0 {
					args = append(args, s)
				}
			}
		}
		return int(kp.Eproc.Ppid), exe, args, nil
	}
}
