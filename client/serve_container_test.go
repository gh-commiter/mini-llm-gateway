package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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

// the automatic upstream push stops when the drain starts, not only when the core closes: no new push while requests finish
func TestShutdownStopsTheAutoPushBeforeTheDrain(t *testing.T) {
	s, _ := testServer(t, nil)
	stop, done := make(chan struct{}), make(chan struct{})
	s.autoStop, s.autoDone = stop, done
	go func() { <-stop; close(done) }()
	stoppedDuringDrain := make(chan bool, 1)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond) // shutdown has started by now
		select {
		case <-stop:
			stoppedDuringDrain <- true
		default:
			stoppedDuringDrain <- false
		}
	})}
	go srv.Serve(ln)
	go http.Get("http://" + ln.Addr().String())
	time.Sleep(100 * time.Millisecond)
	s.shutdown(srv, 5*time.Second)
	if !<-stoppedDuringDrain {
		t.Fatal("the automatic push was still running while requests drained")
	}
}
