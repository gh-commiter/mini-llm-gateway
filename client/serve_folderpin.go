package main

import (
	"fmt"
	"os"
	"runtime"
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
// use adopts it). A legacy record without a volume id compares only the inode; a recorded id must remain available and match.
func (r folderRecord) matches(id fileID) bool {
	// Keep the pre-existing comparison on platforms outside this change.
	missingAllowed := runtime.GOOS != "linux" && runtime.GOOS != "darwin" && id.dev == 0
	return r.Ino == 0 || id.ino == r.Ino && (r.Vol == 0 || missingAllowed || id.dev == r.Vol)
}
