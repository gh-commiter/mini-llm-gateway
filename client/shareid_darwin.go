package main

import "syscall"

// mountRoot (#84): dir je koren priklopa (statfs f_mntonname je ta mapa: /, /System/Volumes/Data, /Volumes/X).
// Primerja istovetnost, ne niza: /System/Volumes/Data in / imata isti st_dev, zato naprava ne zadošča.
func mountRoot(dir string, id fileID) (bool, error) {
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
	m, err := fileIDOf(string(b))
	return err == nil && m == id, err
}
