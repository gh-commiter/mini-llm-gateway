package main

import "syscall"

// mountRoot (#84): dir je koren nosilca, ki hrani dom ali / (statfs f_mntonname je ta mapa in f_fsid je dom ali /):
// /System/Volumes/Data da, /Volumes/<Disk> ne. Primerja istovetnost, ne niza: /System/Volumes/Data in / imata isti
// st_dev, zato naprava ne zadošča.
func mountRoot(dir string, id fileID, home string) (bool, error) {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(dir, &fs); err != nil {
		return false, err
	}
	b := make([]byte, 0, len(fs.Mntonname))
	for _, c := range fs.Mntonname {
		if c == 0 {
			break
		}
		b = append(b, byte(c))
	}
	if m, err := fileIDOf(string(b)); err != nil || m != id {
		return false, err
	}
	for _, p := range []string{home, "/"} {
		var o syscall.Statfs_t
		if err := syscall.Statfs(p, &o); err != nil {
			return false, err
		}
		if o.Fsid == fs.Fsid {
			return true, nil
		}
	}
	return false, nil
}
