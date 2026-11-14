package main

import (
	"errors"
	"os"
)

// Windows never carries an auth file back (restoreAuth returns first); these only satisfy the build.
func openNoFollow(string) (*os.File, error) { return nil, errors.New("not supported") }

func ownedSingleLink(os.FileInfo) bool { return false }

type authGen struct{ mtimeNs, size int64 }

func fileGen(path string) authGen {
	fi, err := os.Stat(path)
	if err != nil {
		return authGen{}
	}
	return authGen{fi.ModTime().UnixNano(), fi.Size()}
}
