package main

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
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
	name, args := pinWrap(t.TempDir(), "/bin/sh", []string{"-c", "mv f g && ! mv .git .g 2>/dev/null"}, []string{wt + "/.git"}, t.Logf)
	if !nestedBwrap {
		t.Skip("nested bwrap not available: pinWrap is off")
	}
	cmd := exec.Command(name, args...)
	cmd.Dir = wt
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if _, err := os.Stat(wt + "/.git"); err != nil {
		t.Fatal(".git was renamed")
	}
}

// pinWrap: kjer bwrap v bwrap ne deluje (Ubuntu 24.04 AppArmor), Claude teče brez pinWrap in to se zapiše enkrat.
// Lažni bwrap izvede ukaz za »--«; z nested=false odpove, ko teče znotraj sebe.
func TestPinWrapOffWithoutNestedBwrap(t *testing.T) {
	defer func() { bwrapOnce, bwrapPath, nestedOnce, nestedBwrap = sync.Once{}, "", sync.Once{}, false }()
	for _, nested := range []bool{false, true} {
		dir := t.TempDir()
		guard := `[ -n "$FAKE_BWRAP" ] && exit 1` + "\n"
		if nested {
			guard = ""
		}
		os.WriteFile(dir+"/bwrap", []byte("#!/bin/sh\n"+guard+`while [ "$1" != "--" ]; do shift; done; shift; FAKE_BWRAP=1 exec "$@"`+"\n"), 0o755)
		t.Setenv("PATH", dir+":/usr/bin:/bin")
		bwrapOnce, bwrapPath, nestedOnce, nestedBwrap = sync.Once{}, "", sync.Once{}, false
		logs := 0
		for range 2 {
			name, _ := pinWrap(t.TempDir(), "/bin/claude", []string{"-p"}, []string{dir}, func(string, ...any) { logs++ })
			if want := map[bool]string{false: "/bin/claude", true: dir + "/bwrap"}[nested]; name != want {
				t.Fatalf("nested=%v: %s, want %s", nested, name, want)
			}
		}
		if want := map[bool]int{false: 1, true: 0}[nested]; logs != want {
			t.Fatalf("nested=%v: logged %d times", nested, logs)
		}
	}
}
