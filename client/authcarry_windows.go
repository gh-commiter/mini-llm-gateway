package main

import (
	"errors"
	"os"
)

// Windows never carries an auth file back (restoreAuth returns first); these only satisfy the build.
func openNoFollow(string) (*os.File, error) { return nil, errors.New("not supported") }

func ownedSingleLink(os.FileInfo) bool { return false }
