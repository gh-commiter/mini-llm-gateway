//go:build slowupgrade

package main

import (
	"os"
	"path/filepath"
	"time"
)

// The test build of the app's tests (swift test builds the helper with -tags slowupgrade): the helper announces an upgrade and then
// waits, before it opens the store, until the test creates <store>/.release-upgrade. No shipped build contains this file.
func init() {
	slowOpen = func(store string, announce func(from, to int)) {
		announce(8, 99)
		for {
			if _, err := os.Stat(filepath.Join(store, ".release-upgrade")); err == nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}
