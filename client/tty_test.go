package main

import (
	"os"
	"testing"
)

// The null device (a script's or a service's stdin on every OS; NUL on Windows) is never a terminal.
func TestStdinIsTTYFalseForTheNullDevice(t *testing.T) {
	saved := os.Stdin
	defer func() { os.Stdin = saved }()
	f, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	os.Stdin = f
	if stdinIsTTY() {
		t.Fatal(os.DevNull, "counted as a terminal")
	}
}
