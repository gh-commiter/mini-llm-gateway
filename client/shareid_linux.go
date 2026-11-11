package main

import (
	"os"
	"strings"
)

// mountRoot (#84): dir (kanonična pot) je točka priklopa v /proc/self/mountinfo (5. polje, tudi bind mount). Ne po
// napravi: podnosilec btrfs ima drugo napravo, a ni priklop. Brez mountinfo napaka: ne vemo.
func mountRoot(dir string, _ fileID) (bool, error) {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return false, err
	}
	unescape := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	for _, l := range strings.Split(string(data), "\n") {
		if f := strings.Fields(l); len(f) > 4 && unescape.Replace(f[4]) == dir {
			return true, nil
		}
	}
	return false, nil
}
