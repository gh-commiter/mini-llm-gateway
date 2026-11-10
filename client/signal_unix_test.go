//go:build unix

package main

import (
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"testing"
)

// N3: po catchInterrupt podproces (claude) dobi SIGINT s privzetim dejanjem (s signal.Ignore bi ga ignoriral).
func TestCatchInterruptChildKeepsDefault(t *testing.T) {
	catchInterrupt()
	defer signal.Reset(os.Interrupt)
	err := exec.Command("/bin/sh", "-c", "kill -INT $$; sleep 1").Run()
	ee, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("child was not stopped by SIGINT: %v", err)
	}
	if ws := ee.Sys().(syscall.WaitStatus); !ws.Signaled() || ws.Signal() != syscall.SIGINT {
		t.Fatalf("child ended with %v, want SIGINT", ws)
	}
}
