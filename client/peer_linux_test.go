package main

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// N5: pomočnik ob koncu agenta ubije, kar je ostalo (tudi osirotelo), in se konča takoj.
func TestAgentReaperKillsLeftovers(t *testing.T) {
	pidFile := t.TempDir() + "/pid"
	cmd := exec.Command("/bin/sh", "-c", "sh -c 'sleep 60 & echo $! > "+pidFile+"' ; exit 3")
	start := time.Now()
	if err := startAgent(cmd); err != nil {
		t.Fatal(err)
	}
	err := cmd.Wait()
	if time.Since(start) > 5*time.Second || cmd.ProcessState.ExitCode() != 3 {
		t.Fatalf("helper: %v after %v", err, time.Since(start))
	}
	b, _ := os.ReadFile(pidFile)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if pid == 0 || syscall.Kill(pid, 0) == nil {
		t.Fatalf("orphan %d still runs", pid)
	}
}
