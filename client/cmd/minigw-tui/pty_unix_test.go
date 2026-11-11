//go:build darwin || linux

package main

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// A terminal that never answers queries (serial lines, some SSH clients): the first frame comes at once, nothing
// asks the terminal (OSC 11 background, CSI 6n cursor position), and q quits with 0.
func TestStartsWithoutTerminalReplies(t *testing.T) {
	u, _ := fake(t)
	ptm, name, err := openPTY()
	if err != nil {
		t.Skip("no pty: ", err)
	}
	defer ptm.Close()
	tty, err := os.OpenFile(name, os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "--minigw", u.minigw)
	for _, e := range os.Environ() { // CI=… would make termenv skip the query anyway: test without it
		if !strings.HasPrefix(e, "CI=") && !strings.HasPrefix(e, "TERM=") {
			cmd.Env = append(cmd.Env, e)
		}
	}
	cmd.Env = append(cmd.Env, "MINIGW_TUI_RUN=1", "TERM=xterm-256color")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = tty, tty, tty
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0} // foreground on its own terminal
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	tty.Close()
	var mu sync.Mutex
	var out bytes.Buffer
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := ptm.Read(buf)
			mu.Lock()
			out.Write(buf[:n])
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	seen := func(s string) bool { mu.Lock(); defer mu.Unlock(); return strings.Contains(out.String(), s) }
	deadline := time.Now().Add(2 * time.Second)
	for !seen("Mini Gateway settings") {
		if time.Now().After(deadline) {
			cmd.Process.Kill()
			t.Fatalf("no first frame within 2 s: %q", out.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	ptm.Write([]byte("q"))
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("exit: %v", err)
		}
	case <-time.After(3 * time.Second):
		cmd.Process.Kill()
		t.Fatal("q did not quit")
	}
	if seen("\x1b]11;") || seen("\x1b[6n") {
		t.Fatalf("queried the terminal: %q", out.String())
	}
}
