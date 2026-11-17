package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestContainerInitIdempotentNoAgents(t *testing.T) {
	t.Setenv("MINIGW_CONTAINER", "1")
	cfg := filepath.Join(t.TempDir(), "config", "serve.json")
	created, err := containerInit(cfg, 0)
	if err != nil || !created {
		t.Fatalf("first start: created=%v err=%v", created, err)
	}
	c, _ := loadServeConfig(cfg)
	if c.Scratch != containerScratch || c.Token == "" || len(containerAgentsOn(c)) != 0 {
		t.Fatalf("bad first config: scratch=%q agents=%v", c.Scratch, containerAgentsOn(c))
	}
	// an edited config with agents on is turned off again at the next start; the token stays
	c.ClaudeDisabled, c.Codex.Enabled, c.Foundry.Enabled = false, true, true
	if err := writeServeConfig(cfg, c); err != nil {
		t.Fatal(err)
	}
	created, err = containerInit(cfg, 0)
	if err != nil || created {
		t.Fatalf("second start: created=%v err=%v", created, err)
	}
	c2, _ := loadServeConfig(cfg)
	if c2.Token != c.Token || c2.Key != c.Key || len(containerAgentsOn(c2)) != 0 {
		t.Fatalf("second start changed the identity or left agents on: %v", containerAgentsOn(c2))
	}
}

func TestContainerRefusesAgentsAtStartUpdateAndExecution(t *testing.T) {
	t.Setenv("MINIGW_CONTAINER", "1")
	s, _ := testServer(t, func(c *serveConfig) {
		c.Codex.Enabled, c.Grok.Enabled, c.Copilot.Enabled, c.Foundry.Enabled = true, true, true, true
	})
	if on := containerAgentsOn(s.conf()); len(on) != 0 {
		t.Fatalf("agents on at start: %v", on)
	}
	for name, f := range map[string]func(*serveConfig){
		"claude":  func(c *serveConfig) { c.ClaudeDisabled = false },
		"codex":   func(c *serveConfig) { c.Codex.Enabled = true },
		"foundry": func(c *serveConfig) { c.Foundry.Enabled = true },
	} {
		err := s.update(func(c *serveConfig) error { f(c); return nil })
		if err == nil || !strings.Contains(err.Error(), "providers only") {
			t.Errorf("%s: update not refused: %v", name, err)
		}
	}
	// execution: even a live config that was changed behind the validation reads as off
	s.mu.Lock()
	s.cfg.Codex.Enabled, s.cfg.ClaudeDisabled = true, false
	s.mu.Unlock()
	if on := containerAgentsOn(s.conf()); len(on) != 0 {
		t.Fatalf("execution sees agents on: %v", on)
	}
}

func TestOutsideContainerAgentsUntouched(t *testing.T) {
	t.Setenv("MINIGW_CONTAINER", "")
	s, _ := testServer(t, func(c *serveConfig) { c.Codex.Enabled = true })
	if !s.conf().Codex.Enabled {
		t.Fatal("agents changed outside container mode")
	}
}

func TestHealthcheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/health/live" || r.Header.Get("Authorization") != "" {
			w.WriteHeader(404)
		}
	}))
	port := srv.Listener.Addr().(*net.TCPAddr).Port
	if err := healthcheck(port, time.Second); err != nil {
		t.Fatal(err)
	}
	srv.Close()
	if err := healthcheck(port, time.Second); err == nil {
		t.Fatal("closed port reported healthy")
	}
}

// shutdown waits for a running request, then closes; a request that outlives the deadline is cut.
func TestShutdownDrainsThenCuts(t *testing.T) {
	s, _ := testServer(t, nil)
	done := make(chan struct{}, 1)
	run := func(h http.HandlerFunc, deadline time.Duration) (finished bool) {
		ln, _ := net.Listen("tcp", "127.0.0.1:0")
		srv := &http.Server{Handler: h}
		go srv.Serve(ln)
		go http.Get("http://" + ln.Addr().String())
		time.Sleep(100 * time.Millisecond)
		s.shutdown(srv, deadline)
		select {
		case <-done:
			return true
		default:
			return false
		}
	}
	// short request: finishes inside the deadline, before shutdown returns
	if !run(func(w http.ResponseWriter, r *http.Request) { time.Sleep(300 * time.Millisecond); done <- struct{}{} }, 5*time.Second) {
		t.Fatal("shutdown returned before the running request finished")
	}
	// request that never ends: cut at the deadline
	start := time.Now()
	run(func(w http.ResponseWriter, r *http.Request) { <-r.Context().Done() }, 300*time.Millisecond)
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("shutdown took %v with a 300ms deadline", d)
	}
}

func fakeResolver(t *testing.T, m map[string]string) {
	old := resolveHost
	resolveHost = func(_ context.Context, h string) ([]net.IP, error) {
		if a, ok := m[h]; ok {
			return []net.IP{net.ParseIP(a)}, nil
		}
		return nil, errors.New("no such host")
	}
	t.Cleanup(func() { resolveHost = old })
}

func TestSingleLabelProviderOnlyInContainerMode(t *testing.T) {
	fakeResolver(t, map[string]string{"litellm": "172.22.0.3", "evil": "8.8.8.8"})
	t.Setenv("MINIGW_CONTAINER", "")
	if _, err := checkGatewayURL("http://litellm:4000", true); err == nil {
		t.Fatal("single-label name accepted outside container mode")
	}
	t.Setenv("MINIGW_CONTAINER", "1")
	for u, ok := range map[string]bool{"http://litellm:4000": true, "http://evil:4000": true /* shape; resolution is checked next */, "http://example.com": false,
		"http://8.8.8.8": false, "http://user:pw@litellm:4000": false} {
		if _, err := checkGatewayURL(u, true); (err == nil) != ok {
			t.Errorf("%s: %v", u, err)
		}
	}
	if _, err := checkGatewayURL("http://litellm:4000", false); err == nil {
		t.Error("http without --insecure accepted")
	}
	if err := checkGatewayName("http://litellm:4000"); err != nil {
		t.Errorf("private name refused: %v", err)
	}
	if err := checkGatewayName("http://evil:4000"); err == nil || !strings.Contains(err.Error(), "public") {
		t.Errorf("public name accepted at add: %v", err)
	}
	if err := checkGatewayName("http://nowhere:4000"); err == nil {
		t.Error("unresolvable name accepted at add")
	}
}

// A name that was private when added and later resolves to a public address is not dialled.
func TestSingleLabelProviderRedialGuard(t *testing.T) {
	t.Setenv("MINIGW_CONTAINER", "1")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)
	fakeResolver(t, map[string]string{"litellm": "127.0.0.1"})
	c, err := gwDial(context.Background(), "tcp", "litellm:"+port)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	fakeResolver(t, map[string]string{"litellm": "8.8.8.8"}) // later: public
	if c, err := gwDial(context.Background(), "tcp", "litellm:"+port); err == nil || !strings.Contains(err.Error(), "public") {
		if c != nil {
			c.Close()
		}
		t.Fatalf("dialled a name that resolves to a public address: %v", err)
	}
}
