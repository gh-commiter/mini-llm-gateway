package main

import (
	"errors"
	"os"
	"strconv"
	"strings"
)

// procLookup (Linux): starš iz /proc/<pid>/stat (procPPID), program iz /proc/<pid>/exe (procesi drugih uporabnikov:
// comm), argumenti iz /proc/<pid>/cmdline.
func procLookup() procFunc {
	return func(pid int) (int, string, []string, error) {
		p := "/proc/" + strconv.Itoa(pid)
		ppid := procPPID(pid)
		if ppid < 0 {
			return 0, "", nil, errors.New("cannot read " + p + "/stat")
		}
		exe, err := os.Readlink(p + "/exe")
		if err != nil {
			b, _ := os.ReadFile(p + "/comm")
			exe = strings.TrimSpace(string(b))
		}
		b, _ := os.ReadFile(p + "/cmdline")
		return ppid, exe, strings.Split(strings.TrimRight(string(b), "\x00"), "\x00"), nil
	}
}
