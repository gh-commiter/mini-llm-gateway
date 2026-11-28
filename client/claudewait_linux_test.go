package main

import (
	"runtime"
	"syscall"
	"testing"
	"time"
)

// #150: SIGSTOP je v niti, ki ga pošlje (tgkill): ustavi se, preden se kill vrne. Signal procesu (kill) obdela poljubna nit, klicoča
// nit pa lahko že izstopi z os.Exit(0): lažni claude je potem končal z 0, ne da bi se ustavil (v Dockerju ~1-2 % zagonov).
func init() {
	fakeStop = func() {
		runtime.LockOSThread()
		syscall.Tgkill(syscall.Getpid(), syscall.Gettid(), syscall.SIGSTOP)
	}
}

// L-b: claude se ustavi (Ctrl+Z) → minigw se ustavi za njim; po SIGCONT minigw nadaljuje claude, oba končata z 0.
func TestClaudeStopFollowed(t *testing.T) {
	dir, log := fakeClaudeDir(t)
	cmd := minigwCmd(dir, log, []string{"MGTEST_CLAUDE_SIGNAL=STOP"}, "-h")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	var ws syscall.WaitStatus
	done := make(chan error, 1)
	go func() { _, err := syscall.Wait4(pid, &ws, syscall.WUNTRACED, nil); done <- err }()
	select {
	case err := <-done:
		if err != nil || !ws.Stopped() {
			t.Fatalf("minigw did not stop: %v %v", ws, err)
		}
	case <-time.After(20 * time.Second):
		cmd.Process.Kill()
		t.Fatal("minigw did not stop")
	}
	syscall.Kill(pid, syscall.SIGCONT)
	go func() { _, err := syscall.Wait4(pid, &ws, 0, nil); done <- err }()
	select {
	case err := <-done:
		if err != nil || !ws.Exited() || ws.ExitStatus() != 0 {
			t.Fatalf("after fg: %v %v", ws, err)
		}
	case <-time.After(20 * time.Second):
		cmd.Process.Kill()
		t.Fatal("minigw did not finish after SIGCONT")
	}
}
