//go:build unix

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// #80 L3 (kot Swift main.swift): manjkajoči fd 0–2 (2>&-) je /dev/null, preden minigw odpre datoteko, zato diagnostika
// na stderr ne pristane v njej. To naredi že izvajalnik Go za vsak program (runtime/fds_unix.go checkfds, klican iz
// schedinit v runtime/proc.go), minigw zato tega ne ponovi; test varuje, da tako ostane.
func TestClosedStdFdsNotReused(t *testing.T) {
	for _, redir := range []string{"2>&-", "0<&- 1>&- 2>&-"} {
		f := filepath.Join(t.TempDir(), "opened")
		cmd := exec.Command("sh", "-c", `exec "$0" `+redir, os.Args[0])
		cmd.Env = append(os.Environ(), "MINIGW_TEST_FDS="+f)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v %s", redir, err, out)
		}
		if b, err := os.ReadFile(f); err != nil || len(b) != 0 {
			t.Errorf("%s: diagnostic landed in an opened file: %q %v", redir, b, err)
		}
	}
}
