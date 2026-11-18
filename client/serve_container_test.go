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
	"sync/atomic"
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

// fakeResolverIPs: a name that resolves to several addresses (every one is checked).
func fakeResolverIPs(t *testing.T, m map[string][]string) {
	old := resolveHost
	resolveHost = func(_ context.Context, h string) ([]net.IP, error) {
		a, ok := m[h]
		if !ok {
			return nil, errors.New("no such host")
		}
		ips := make([]net.IP, len(a))
		for i := range a {
			ips[i] = net.ParseIP(a[i])
		}
		return ips, nil
	}
	t.Cleanup(func() { resolveHost = old })
}

// fakeDial: gwDial's real connect goes to a 127.0.0.1 listener; the address it was asked for (the checked IP) is recorded.
func fakeDial(t *testing.T, port string) *[]string {
	old := dialChecked
	var got []string
	dialChecked = func(ctx context.Context, network, addr string) (net.Conn, error) {
		got = append(got, addr)
		return (&net.Dialer{}).DialContext(ctx, network, net.JoinHostPort("127.0.0.1", port))
	}
	t.Cleanup(func() { dialChecked = old })
	return &got
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
	dialled := fakeDial(t, port)
	fakeResolver(t, map[string]string{"litellm": "172.22.0.3"})
	c, err := gwDial(context.Background(), "tcp", "litellm:"+port)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if want := "172.22.0.3:" + port; len(*dialled) != 1 || (*dialled)[0] != want {
		t.Fatalf("dialled %v, want the checked address %s", *dialled, want)
	}
	fakeResolver(t, map[string]string{"litellm": "8.8.8.8"}) // later: public
	if c, err := gwDial(context.Background(), "tcp", "litellm:"+port); err == nil || !strings.Contains(err.Error(), "public") {
		if c != nil {
			c.Close()
		}
		t.Fatalf("dialled a name that resolves to a public address: %v", err)
	}
	if len(*dialled) != 1 {
		t.Fatalf("a connect was attempted for a public answer: %v", *dialled)
	}
}

// B1: a service name may resolve only to private addresses. Link-local (a cloud metadata service), loopback,
// unspecified, multicast and their IPv4-mapped forms are refused, at add time and at every dial; every address counts.
func TestSingleLabelProviderRefusesNonPrivateAddresses(t *testing.T) {
	t.Setenv("MINIGW_CONTAINER", "1")
	fakeResolverIPs(t, map[string][]string{
		"meta":    {"169.254.169.254"},
		"meta6":   {"fe80::1"},
		"mapped":  {"::ffff:169.254.169.254"},
		"loop":    {"127.0.0.1"},
		"loop6":   {"::ffff:127.0.0.1"},
		"unset":   {"::"},
		"unset4":  {"::ffff:0:0"},
		"mcast":   {"224.0.0.1"},
		"mixed":   {"172.22.0.3", "169.254.169.254"},
		"private": {"172.22.0.3", "172.22.0.4"},
	})
	dialled := fakeDial(t, "1")
	for _, name := range []string{"meta", "meta6", "mapped", "loop", "loop6", "unset", "unset4", "mcast", "mixed"} {
		if err := checkGatewayName("http://" + name + ":4000"); err == nil {
			t.Errorf("%s: accepted at add time", name)
		}
		if c, err := gwDial(context.Background(), "tcp", name+":4000"); err == nil {
			c.Close()
			t.Errorf("%s: dialled", name)
		}
	}
	if err := checkGatewayName("http://private:4000"); err != nil {
		t.Errorf("private addresses refused at add time: %v", err)
	}
	if len(*dialled) != 0 {
		t.Errorf("a refused name reached the connect step: %v", *dialled)
	}
}

// B2: a provider key must not go to the environment proxy for a single-label service name; the name is dialled
// directly, so gwDial checks the address it resolves to.
func TestServiceNameProviderBypassesEnvProxy(t *testing.T) {
	t.Setenv("MINIGW_CONTAINER", "1")
	var proxied atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer proxy.Close()
	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("HTTPS_PROXY", proxy.URL)
	t.Setenv("NO_PROXY", "")
	fakeResolver(t, map[string]string{"litellm": "172.22.0.3"})

	req, _ := http.NewRequest("GET", "http://litellm:4000/key/info", nil)
	req.Header.Set("authorization", "Bearer k")
	if p, err := quotaHTTP.Transport.(*http.Transport).Proxy(req); err != nil || p != nil {
		t.Fatalf("quota client would use proxy %v for a service name: %v", p, err)
	}
	treq, _ := http.NewRequest("GET", "https://litellm:4000/v1/models", nil)
	if p, err := gwClients[0].Transport.(*http.Transport).Proxy(treq); err != nil || p != nil {
		t.Fatalf("provider client would use proxy %v for a service name: %v", p, err)
	}

	// The dial is refused by gwDial before any connection; the proxy must not have been asked at all.
	fakeResolver(t, map[string]string{"litellm": "169.254.169.254"})
	fakeDial(t, "1")
	if _, err := quotaHTTP.Do(req); err == nil {
		t.Fatal("request to a link-local provider name succeeded")
	}
	if n := proxied.Load(); n != 0 {
		t.Fatalf("the provider request reached the environment proxy %d times", n)
	}
}
