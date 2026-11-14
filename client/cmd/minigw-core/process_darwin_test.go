package main

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"minigw/coreapi"
)

// These tests run the real binary the way the app does: a socketpair on fd 3, the secret on a pipe at fd 4.

var helperBin string

func TestMain(m *testing.M) {
	if h := os.Getenv("MGCORE_TEST_APP"); h != "" { // re-executed as a stand-in app: the parent and creator of the helper
		appStandIn(h, os.Getenv("MGCORE_TEST_STORE"))
		return
	}
	dir, err := os.MkdirTemp("", "minigw-core-test")
	if err != nil {
		panic(err)
	}
	helperBin = filepath.Join(dir, "minigw-core")
	if out, err := exec.Command("go", "build", "-o", helperBin, ".").CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "go build: %v\n%s", err, out)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func socketpair() (mine net.Conn, theirs *os.File) {
	syscall.ForkLock.RLock()
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err == nil {
		syscall.CloseOnExec(fds[0])
		syscall.CloseOnExec(fds[1])
	}
	syscall.ForkLock.RUnlock()
	if err != nil {
		panic(err)
	}
	f := os.NewFile(uintptr(fds[0]), "mine")
	c, err := net.FileConn(f)
	f.Close()
	if err != nil {
		panic(err)
	}
	return c, os.NewFile(uintptr(fds[1]), "theirs")
}

// launch starts cmd with the helper's two descriptors (cmd.ExtraFiles: fd 3 and fd 4) and delivers the secret.
func launch(cmd *exec.Cmd, secret []byte) (net.Conn, error) {
	mine, theirs := socketpair()
	pr, pw, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	cmd.ExtraFiles = []*os.File{theirs, pr}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	theirs.Close()
	pr.Close()
	pw.Write(secret)
	pw.Close()
	return mine, nil
}

func appStandIn(helper, store string) {
	cmd := exec.Command(helper, "-store", store)
	cmd.Stderr = os.Stderr
	conn, err := launch(cmd, testSecret)
	if err != nil {
		panic(err)
	}
	fmt.Println(cmd.Process.Pid)
	buf := make([]byte, 1)
	conn.Read(buf) // keep the connection open until killed
}

func wait(t *testing.T, cmd *exec.Cmd) int {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-done:
		return cmd.ProcessState.ExitCode()
	case <-time.After(10 * time.Second):
		cmd.Process.Kill()
		t.Fatal("the helper did not exit")
		return -1
	}
}

func TestRealHelperRoundTripAndCleanEOF(t *testing.T) {
	var stderr bytes.Buffer
	cmd := exec.Command(helperBin, "-store", filepath.Join(t.TempDir(), "core"))
	cmd.Stderr = &stderr
	conn, err := launch(cmd, testSecret)
	if err != nil {
		t.Fatal(err)
	}
	if r := clientHandshake(t, conn, testSecret, 1, 1); r.Type != "ready" {
		t.Fatalf("handshake: %+v", r)
	}
	cl := coreapi.NewClient(conn)
	r, err := cl.Do(context.Background(), coreapi.Context{Kind: coreapi.KindInternal}, coreapi.Request{Method: "POST", Path: "/internal/epoch", Body: []byte(`{}`)})
	if err != nil || r.Status != 200 {
		t.Fatalf("epoch: %v %+v", err, r)
	}
	// no network socket, and the secret is nowhere in argv, environment or log
	out, _ := exec.Command("lsof", "-nP", "-a", "-p", strconv.Itoa(cmd.Process.Pid), "-i").Output() // stdout only: lsof warns on stderr
	if strings.TrimSpace(string(out)) != "" {
		t.Fatalf("the helper has network sockets:\n%s", out)
	}
	ps, _ := exec.Command("ps", "eww", "-p", strconv.Itoa(cmd.Process.Pid)).CombinedOutput()
	if bytes.Contains(ps, testSecret) || bytes.Contains(stderr.Bytes(), testSecret) {
		t.Fatal("the secret is visible")
	}
	conn.Close() // EOF
	if code := wait(t, cmd); code != 0 {
		t.Fatalf("exit %d after EOF, stderr: %s", code, stderr.String())
	}
}

func TestPeerThatIsNotTheParentIsRefused(t *testing.T) {
	// The socketpair is made by this process, but the helper is started by a shell: its parent is the shell, so the
	// peer (this process, same user, with the secret) is not its parent. The helper exits before it reads anything.
	var stderr bytes.Buffer
	cmd := exec.Command("/bin/sh", "-c", `"$0" "$@"; exit $?`, helperBin, "-store", filepath.Join(t.TempDir(), "core"))
	cmd.Stderr = &stderr
	conn, err := launch(cmd, testSecret)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if code := wait(t, cmd); code != exitRefused {
		t.Fatalf("exit %d, want %d; stderr: %s", code, exitRefused, stderr.String())
	}
	if !strings.Contains(stderr.String(), "peer is not the parent") {
		t.Fatalf("stderr: %s", stderr.String())
	}
	if err := coreapi.WriteRequest(conn, 1, coreapi.Context{Kind: coreapi.KindInternal}, coreapi.Request{Method: "POST", Path: "/internal/epoch", Body: []byte(`{}`)}); err == nil {
		if _, _, err := coreapi.ReadResponse(conn); err == nil {
			t.Fatal("a refused peer was served")
		}
	}
}

func TestHelperExitsWhenItsParentDies(t *testing.T) {
	app := exec.Command(os.Args[0])
	app.Env = append(os.Environ(), "MGCORE_TEST_APP="+helperBin, "MGCORE_TEST_STORE="+filepath.Join(t.TempDir(), "core"))
	out, err := app.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Start(); err != nil {
		t.Fatal(err)
	}
	var pid int
	if _, err := fmt.Fscan(out, &pid); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Kill(pid, 0); err != nil {
		t.Fatalf("helper %d is not running: %v", pid, err)
	}
	app.Process.Kill() // our own child, the stand-in app
	app.Wait()
	for i := 0; i < 100; i++ {
		if syscall.Kill(pid, 0) != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("the helper %d is still running 5 s after its parent died", pid)
}

func TestStoreThatCannotOpenKeepsTheHelperUpAnd503s(t *testing.T) {
	file := filepath.Join(t.TempDir(), "core")
	if err := os.WriteFile(file, []byte("not a folder"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd := exec.Command(helperBin, "-store", file)
	cmd.Stderr = &stderr
	conn, err := launch(cmd, testSecret)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { conn.Close(); wait(t, cmd) }()
	if r := clientHandshake(t, conn, testSecret, 1, 1); r.Type != "ready" {
		t.Fatalf("handshake: %+v", r)
	}
	cl := coreapi.NewClient(conn)
	for _, c := range []struct {
		cx coreapi.Context
		rq coreapi.Request
	}{
		{coreapi.Context{Kind: coreapi.KindMember, DeviceID: "d", Approved: true}, coreapi.Request{Method: "GET", Path: "/minigw/v1/projects"}},
		{coreapi.Context{Kind: coreapi.KindAdmin}, coreapi.Request{Method: "GET", Path: "/users"}},
		{coreapi.Context{Kind: coreapi.KindInternal}, coreapi.Request{Method: "POST", Path: "/internal/epoch", Body: []byte(`{}`)}},
	} {
		r, err := cl.Do(context.Background(), c.cx, c.rq)
		if err != nil || r.Status != 503 || !strings.Contains(string(r.Body), "not available") {
			t.Fatalf("%s %s: %v %d %s", c.rq.Method, c.rq.Path, err, r.Status, r.Body)
		}
	}
}
