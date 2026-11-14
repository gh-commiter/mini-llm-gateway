//go:build windows

package main

import "os"

// fileOwnedByUser: Windows has no owner in the file info; a link is refused by Lstat (a symlink is not a regular file, a junction not a folder).
func fileOwnedByUser(os.FileInfo) bool { return true }
