//go:build !windows && !darwin && !linux

package main

// mountRoot (#84): koren priklopa = druga naprava kot nadrejena mapa (st_dev).
func mountRoot(dir string, id fileID) (bool, error) {
	parent, err := fileIDOf(dir + "/..")
	return err == nil && parent.dev != id.dev, err
}
