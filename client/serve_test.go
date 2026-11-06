package main

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// pipeListener: povezave net.Pipe (oddaljen naslov »pipe« = ni loopback).
type pipeListener struct{ ch chan net.Conn }

func (l pipeListener) Accept() (net.Conn, error) {
	c, ok := <-l.ch
	if !ok {
		return nil, net.ErrClosed
	}
	return c, nil
}
func (l pipeListener) Close() error   { return nil }
func (l pipeListener) Addr() net.Addr { return &net.TCPAddr{} }

func testServer(t *testing.T, mod func(*serveConfig)) (*server, *ecdh.PrivateKey) {
	t.Helper()
	k, _ := ecdh.X25519().GenerateKey(rand.Reader)
	c := serveConfig{Token: "tok", Key: base64url(k.Bytes()), Tools: defaultTools, PermissionMode: "dontAsk", Scratch: t.TempDir()}
	if mod != nil {
		mod(&c)
	}
	s, err := newServer(c)
	if err != nil {
		t.Fatal(err)
	}
	s.log.SetOutput(io.Discard)
	return s, k
}

// pipeRequest: zahteva prek obstoječega pobudnika Noise (kot noiseProxy), odgovor dešifriran.
func pipeRequest(t *testing.T, s *server, routerKey []byte, dev *device, request string) string {
	t.Helper()
	l := pipeListener{make(chan net.Conn, 1)}
	go s.serve(l)
	defer close(l.ch)
	a, b := net.Pipe()
	l.ch <- b
	a.SetDeadline(time.Now().Add(10 * time.Second))
	defer a.Close()
	hs, _ := newInitiator(routerKey)
	first, _ := hs.first()
	if _, err := a.Write(append(append([]byte{}, noiseMagic...), first...)); err != nil {
		return ""
	}
	reply := make([]byte, handshakeLen)
	if _, err := io.ReadFull(a, reply); err != nil {
		return "" // router je zaprl
	}
	sess, err := hs.finish(reply)
	if err != nil {
		t.Fatal(err)
	}
	out := []byte{}
	if dev != nil {
		out = frames(deviceProof(dev.key, dev.name, sess.hash), sess.send)
	}
	go a.Write(append(out, frames([]byte(request), sess.send)...))
	raw, _ := io.ReadAll(a)
	plain, _, err := unframe(raw, sess.receive)
	if err != nil {
		t.Fatal(err)
	}
	return string(plain)
}

func newDevice(name string) *device {
	_, k, _ := ed25519.GenerateKey(rand.Reader)
	return &device{key: k, name: name}
}

func TestServeNoiseHandshakeTokenAndDevices(t *testing.T) {
	mac := newDevice("MacBook")
	macID := base64url(mac.key.Public().(ed25519.PublicKey))
	get := func(tok string) string {
		return "GET /v1/models HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer " + tok + "\r\n\r\n"
	}

	s, k := testServer(t, func(c *serveConfig) { c.Devices = []serveDevice{{ID: macID, Name: "MacBook"}} })
	pub := k.PublicKey().Bytes()
	if r := pipeRequest(t, s, pub, mac, get("tok")); !strings.HasPrefix(r, "HTTP/1.1 200") || !strings.Contains(r, `"data"`) {
		t.Fatalf("allowed device: %q", r)
	}
	if r := pipeRequest(t, s, pub, mac, get("wrong")); !strings.HasPrefix(r, "HTTP/1.1 401") {
		t.Fatalf("wrong token: %q", r)
	}
	if r := pipeRequest(t, s, pub, newDevice("Other"), get("tok")); !strings.HasPrefix(r, "HTTP/1.1 403") || !strings.Contains(r, "minigw serve allow") {
		t.Fatalf("unknown device: %q", r)
	}
	if r := pipeRequest(t, s, pub, nil, get("tok")); !strings.HasPrefix(r, "HTTP/1.1 403") {
		t.Fatalf("no device proof: %q", r)
	}
	other, _ := ecdh.X25519().GenerateKey(rand.Reader) // napačen ključ routerja: brez odgovora
	if r := pipeRequest(t, s, other.PublicKey().Bytes(), mac, get("tok")); r != "" {
		t.Fatalf("wrong router key answered: %q", r)
	}

	// Prazen seznam: neznana naprava le z allowAnyDevice.
	s, k = testServer(t, nil)
	if r := pipeRequest(t, s, k.PublicKey().Bytes(), mac, get("tok")); !strings.HasPrefix(r, "HTTP/1.1 403") {
		t.Fatalf("empty list: %q", r)
	}
	s, k = testServer(t, func(c *serveConfig) { c.AllowAnyDevice = true })
	if r := pipeRequest(t, s, k.PublicKey().Bytes(), mac, get("tok")); !strings.HasPrefix(r, "HTTP/1.1 200") {
		t.Fatalf("allowAnyDevice: %q", r)
	}
}

func TestServePlainOnlyFromLoopback(t *testing.T) {
	s, _ := testServer(t, nil)
	l := pipeListener{make(chan net.Conn, 1)}
	go s.serve(l)
	defer close(l.ch)
	a, b := net.Pipe()
	l.ch <- b
	a.SetDeadline(time.Now().Add(5 * time.Second))
	go a.Write([]byte("GET /v1/models HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer tok\r\n\r\n"))
	r, _ := io.ReadAll(a)
	if !strings.HasPrefix(string(r), "HTTP/1.1 426") {
		t.Fatalf("plain from network: %q", r)
	}
}

// startLoopback: router na 127.0.0.1 (navaden HTTP, kot lokalni minigw).
func startLoopback(t *testing.T, s *server) connection {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go s.serve(ln)
	t.Cleanup(func() { ln.Close() })
	return connection{URL: "http://" + ln.Addr().String(), Token: "tok"}
}

func TestServeSyncAgentRoundTrip(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake claude is a shell script")
	}
	tmp := t.TempDir()
	fake := filepath.Join(tmp, "claude")
	argsFile := filepath.Join(tmp, "args")
	os.WriteFile(fake, []byte(`#!/bin/sh
printf '%s\n' "$@" > "`+argsFile+`"
cat > /dev/null
cat src.txt > agent.txt
echo '{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Write","input":{"file_path":"agent.txt"}}]}}'
echo '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"done"}}}'
echo '{"type":"result","subtype":"success","usage":{"output_tokens":3}}'
`), 0o755)
	s, _ := testServer(t, func(c *serveConfig) { c.Claude = fake })
	c := startLoopback(t, s)

	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "src.txt"), []byte("from client\n"), 0o644)
	sc := &shareClient{c: c, session: "sid1", root: dir, sync: true, synced: map[string]syncEntry{}, reported: map[string]bool{}}
	if !sc.register() {
		t.Fatal("register failed: " + sc.refusal)
	}
	sc.start(2)
	defer sc.unregister()

	body := `{"model":"claude-cli-haiku","tools":[{"name":"x"}],"messages":[{"role":"user","content":"go"}]}`
	h := map[string]string{"x-claude-mini-session": "sid1", "x-claude-mini-cwd": dir, "content-type": "application/json"}
	st, data := call(c, "POST", "/v1/messages", h, []byte(body), 30*time.Second)
	if st != 200 || !strings.Contains(string(data), `"done"`) {
		t.Fatalf("messages: %d %s", st, data)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "agent.txt")); string(b) != "from client\n" {
		t.Fatalf("agent change not synced back: %q", b)
	}
	args, _ := os.ReadFile(argsFile)
	if !strings.Contains(string(args), "--setting-sources\n\n") || !strings.Contains(string(args), "--session-id") {
		t.Fatalf("args: %q", args)
	}

	// Streaming: SSE z message_stop; druga zahteva iste zgodovine nadaljuje sejo (--resume).
	body = `{"model":"claude-cli-haiku","stream":true,"tools":[{"name":"x"}],"messages":[{"role":"user","content":"go"},{"role":"assistant","content":"done"},{"role":"user","content":"again"}]}`
	st, data = call(c, "POST", "/v1/messages", h, []byte(body), 30*time.Second)
	if st != 200 || !strings.Contains(string(data), "event: message_stop") {
		t.Fatalf("stream: %d %s", st, data)
	}
	if args, _ = os.ReadFile(argsFile); !strings.Contains(string(args), "--resume") {
		t.Fatalf("not resumed: %q", args)
	}

	// Pobeg iz kopije: PUT zunaj manifesta in pot z »..« → 403; branje → 404.
	for _, p := range []string{"../evil.txt", "src.txt/../../evil.txt", "a/../../evil.txt"} {
		if st, _ := call(c, "PUT", "/minigw/sync/file", map[string]string{"x-share-key": sc.currentKey(), "x-sync-path": p}, []byte("x"), 5*time.Second); st != 403 {
			t.Fatalf("PUT %s: %d", p, st)
		}
		if st, _ := call(c, "GET", "/minigw/sync/file", map[string]string{"x-share-key": sc.currentKey(), "x-sync-path": p}, nil, 5*time.Second); st != 404 {
			t.Fatalf("GET %s: %d", p, st)
		}
	}
	// Manifest z »..« in ».GIT« → router teh poti ne zahteva; PUT nanje 403.
	m, _ := json.Marshal([]syncEntry{{Path: "../evil.txt", SHA256: sha256Hex([]byte("x"))}, {Path: ".GIT/hooks/pre-commit", SHA256: sha256Hex([]byte("x"))}})
	st, data = call(c, "POST", "/minigw/sync/manifest", map[string]string{"x-share-key": sc.currentKey()}, m, 5*time.Second)
	if st != 200 || strings.Contains(string(data), "evil") || strings.Contains(string(data), "GIT") {
		t.Fatalf("manifest: %d %s", st, data)
	}
	if st, _ := call(c, "PUT", "/minigw/sync/file", map[string]string{"x-share-key": sc.currentKey(), "x-sync-path": ".GIT/hooks/pre-commit"}, []byte("x"), 5*time.Second); st != 403 {
		t.Fatalf("PUT .GIT: %d", st)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(s.cfg.scratch("sync")), "evil.txt")); err == nil {
		t.Fatal("file escaped the copy")
	}
}

func TestServeRefusesDirectMode(t *testing.T) {
	s, _ := testServer(t, func(c *serveConfig) { c.Claude = "/nonexistent" })
	c := startLoopback(t, s)
	sc := &shareClient{c: c, session: "sid2", root: t.TempDir(), synced: map[string]syncEntry{}, reported: map[string]bool{}}
	if sc.register() || !strings.Contains(sc.refusal, "--minigw-work=sync only") {
		t.Fatalf("direct mode accepted: %q", sc.refusal)
	}
	body := `{"model":"claude-cli-haiku","tools":[{"name":"x"}],"messages":[{"role":"user","content":"go"}]}`
	st, data := call(c, "POST", "/v1/messages", map[string]string{"x-claude-mini-session": "sid2", "x-claude-mini-share": "1"}, []byte(body), 5*time.Second)
	if st != 400 || !strings.Contains(string(data), "sync only") {
		t.Fatalf("agent without sync: %d %s", st, data)
	}
}
