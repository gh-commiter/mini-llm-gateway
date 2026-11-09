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

// N3: pod pinWrap .git ni mogoče preimenovati (točka priklopa), drugo v mapi pa ja.
func TestPinWrapGitCannotBeRenamed(t *testing.T) {
	if bwrapReady(t.TempDir()) == "" {
		t.Skip("bwrap not available")
	}
	wt := t.TempDir()
	os.Mkdir(wt+"/.git", 0o755)
	os.WriteFile(wt+"/f", []byte("x"), 0o644)
	name, args := pinWrap(t.TempDir(), "/bin/sh", []string{"-c", "mv f g && ! mv .git .g 2>/dev/null"}, []string{wt + "/.git"})
	cmd := exec.Command(name, args...)
	cmd.Dir = wt
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if _, err := os.Stat(wt + "/.git"); err != nil {
		t.Fatal(".git was renamed")
	}
}
