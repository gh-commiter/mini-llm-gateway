//go:build !windows

package main

import (
	"fmt"
	"os"
	"testing"
)

// runIsolated: Windows v isolate_windows_test.go; drugje testi sami nastavijo HOME in XDG_*, tu le git.
func runIsolated(m *testing.M) int {
	d, err := os.MkdirTemp("", "minigw-test-git-")
	if err == nil {
		defer os.RemoveAll(d)
		err = isolateGit(d)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "minigw tests: cannot isolate git config:", err)
		return 2
	}
	return m.Run()
}
