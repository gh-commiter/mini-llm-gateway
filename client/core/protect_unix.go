//go:build !windows

package core

import "os"

// protectFile makes a file readable by its owner only.
func protectFile(path string) error { return os.Chmod(path, 0o600) }
