package main

import (
	"fmt"
	"os"
)

type checkedFolder struct {
	path string
	id   fileID
}

type checkedFolders []checkedFolder

func (folders checkedFolders) paths() []string {
	var paths []string
	for _, f := range folders {
		paths = append(paths, f.path)
	}
	return paths
}

func (folders checkedFolders) verify() error {
	for _, f := range folders {
		id, err := fileIDOf(f.path)
		if err != nil || id != f.id {
			return fmt.Errorf("allowed folder changed before agent start: %s", f.path)
		}
	}
	return nil
}

func (folders checkedFolders) open() ([]*os.File, error) {
	var held []*os.File
	for _, f := range folders {
		h, err := openCheckedFolder(f)
		if err != nil {
			closeFolders(held)
			return nil, err
		}
		if h != nil {
			held = append(held, h)
		}
	}
	return held, nil
}

func closeFolders(held []*os.File) {
	for _, f := range held {
		f.Close()
	}
}

// matches (B1): id (from recordIDOf) is the recorded folder. No recorded inode = not recorded yet (always true; the first
// use adopts it). The volume ids are compared only when both are known.
func (r folderRecord) matches(id fileID) bool {
	return r.Ino == 0 || id.ino == r.Ino && (r.Vol == 0 || id.dev == 0 || id.dev == r.Vol)
}
