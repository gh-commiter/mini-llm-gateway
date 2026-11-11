//go:build !windows && !darwin && !linux

package main

// mountRoot (#84): koren priklopa (druga naprava kot nadrejena mapa) na napravi doma ali /.
func mountRoot(dir string, id fileID, home string) (bool, error) {
	parent, err := fileIDOf(dir + "/..")
	if err != nil || parent.dev == id.dev {
		return false, err
	}
	for _, p := range []string{home, "/"} {
		if o, err := fileIDOf(p); err != nil || o.dev == id.dev {
			return err == nil, err
		}
	}
	return false, nil
}
