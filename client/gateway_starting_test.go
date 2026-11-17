package main

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/json"
	"net"
	"path/filepath"
	"strings"
	"testing"

	"minigw/core"
)

// While a gateway process holds the project database (it opens and migrates it before it listens), no gateway answers yet. A command then
// says "starting", not "not running", in `gw status` and in the error of an admin command; once nothing holds the lock it says "not running".
func TestStartingIsNotTheSameAsNotRunning(t *testing.T) {
	k, _ := ecdh.X25519().GenerateKey(rand.Reader)
	cfgDir := t.TempDir()
	cfg := filepath.Join(cfgDir, "serve.json")
	ln, err := net.Listen("tcp", "127.0.0.1:0") // a port nothing listens on: the command finds no gateway (never the real one on the default port)
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	saveServeConfig(cfg, serveConfig{Token: "tok", Key: base64url(k.Bytes()), Port: port, Address: "127.0.0.1", Tools: defaultTools, PermissionMode: "dontAsk", Scratch: t.TempDir()})
	status := func() (string, apiStatus, int, string) {
		s, err := offlineServer(cfg)
		if err != nil {
			t.Fatal(err)
		}
		out, _, exit := captured(func() { routerCommand([]string{"--config", cfg, "status"}, strings.NewReader("")) })
		var st apiStatus
		code, obj := s.adminDo("GET", "/status", nil)
		b, _ := json.Marshal(obj)
		json.Unmarshal(b, &st)
		_ = code
		ucode, uobj := s.adminDo("GET", "/users", nil)
		ub, _ := json.Marshal(uobj)
		return out, st, ucode + exit*1000, string(ub)
	}
	// nothing holds the database
	out, st, ucode, ub := status()
	if st.Starting || !strings.Contains(out, "gateway: not running") || ucode != 503 || !strings.Contains(ub, "not running") || strings.Contains(ub, "starting") {
		t.Fatalf("stopped: %v %q %d %s", st.Starting, out, ucode, ub)
	}
	// a gateway process (here: this test) has the database open
	held, err := core.Open(filepath.Join(cfgDir, coreDirName), core.Options{})
	if err != nil {
		t.Fatal(err)
	}
	out, st, ucode, ub = status()
	if !st.Starting || !strings.Contains(out, "gateway: starting (the project database is being opened or migrated") || ucode != 503 || !strings.Contains(ub, "starting: its project database is being opened or migrated") {
		t.Fatalf("starting: %v %q %d %s", st.Starting, out, ucode, ub)
	}
	held.Close()
	out, st, _, _ = status()
	if st.Starting || !strings.Contains(out, "gateway: not running") {
		t.Fatalf("closed again: %v %q", st.Starting, out)
	}
}
