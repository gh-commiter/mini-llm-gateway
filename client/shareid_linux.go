package main

import (
	"os"
	"path/filepath"
	"strings"
)

// mountRoot (#84): dir (kanonična pot) je točka priklopa v /proc/self/mountinfo istega datotečnega sistema (major:minor)
// kot priklop z domom ali /, in pokaže vsaj njegov del (koren priklopa, 4. polje, je / ali nad korenom tega priklopa):
// bazen btrfs ob @home da; /workspaces/<projekt> v devcontainerju, nosilec Docker ali bind mount projekta ne. Brez
// mountinfo napaka: ne vemo (deljenje zavrnjeno).
func mountRoot(dir string, _ fileID, home string) (bool, error) {
	data, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return false, err
	}
	unescape := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	var mounts [][3]string // major:minor, koren, točka
	for _, l := range strings.Split(string(data), "\n") {
		if f := strings.Fields(l); len(f) > 4 {
			mounts = append(mounts, [3]string{f[2], unescape.Replace(f[3]), unescape.Replace(f[4])})
		}
	}
	// holding: priklop, v katerem je p (najdaljša točka; kasnejši na isti točki prekrije prejšnjega)
	holding := func(p string) (m [3]string) {
		for _, x := range mounts {
			if (x[2] == "/" || p == x[2] || strings.HasPrefix(p, x[2]+"/")) && len(x[2]) >= len(m[2]) {
				m = x
			}
		}
		return m
	}
	d := holding(dir)
	if d[2] != dir {
		return false, nil
	}
	if r, err := filepath.EvalSymlinks(home); err == nil {
		home = r
	}
	for _, p := range []string{home, "/"} {
		if h := holding(p); h[0] == d[0] && (d[1] == "/" || h[1] == d[1] || strings.HasPrefix(h[1], d[1]+"/")) {
			return true, nil
		}
	}
	return false, nil
}
