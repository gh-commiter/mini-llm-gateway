//go:build darwin || linux

package main

import (
	"os"
	"testing"
)

// kb accept asks only a real terminal: a character device that is not one (/dev/zero, /dev/null) never counts.
func TestStdinIsTTYOnlyForATerminal(t *testing.T) {
	saved := os.Stdin
	defer func() { os.Stdin = saved }()
	for _, dev := range []string{"/dev/zero", "/dev/null"} {
		f, err := os.Open(dev)
		if err != nil {
			t.Fatal(err)
		}
		os.Stdin = f
		if stdinIsTTY() {
			t.Fatal(dev, "counted as a terminal")
		}
		f.Close()
	}
}
