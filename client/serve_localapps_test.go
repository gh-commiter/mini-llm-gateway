package main

import (
	"net"
	"net/http"
	"runtime"
	"testing"
)

// Modeli brez žetona za aplikacije tega uporabnika na loopbacku: glava, Host, Origin, uporabnik, stikalo.
func TestLocalAppsWithoutToken(t *testing.T) {
	same := true
	localPeer = func(ours, peer *net.TCPAddr) bool { return same }
	t.Cleanup(func() { localPeer = sameUserPeer })
	s, _ := testServer(t, nil)
	c := startLoopback(t, s)
	get := func(method, path, host string, h map[string]string) int {
		q, _ := http.NewRequest(method, c.URL+path, nil)
		if host != "" {
			q.Host = host
		}
		for k, v := range h {
			q.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(q)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	av := map[string]string{"anthropic-version": "2023-06-01"}
	for _, tc := range []struct {
		name, method, path, host string
		h                        map[string]string
		want                     int
	}{
		{"same user", "GET", "/v1/models", "", av, 200},
		{"x-api-key", "GET", "/v1/models", "", map[string]string{"x-api-key": "anything"}, 200},
		{"localhost host", "GET", "/v1/models", "localhost:1", av, 200},
		{"no custom header", "GET", "/v1/models", "", nil, 401},
		{"browser origin", "GET", "/v1/models", "", map[string]string{"anthropic-version": "1", "origin": "http://127.0.0.1"}, 401},
		{"rebound host", "GET", "/v1/models", "evil.example", av, 401},
		{"not a model route", "POST", "/minigw/share", "", av, 401},
	} {
		if got := get(tc.method, tc.path, tc.host, tc.h); got != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, got, tc.want)
		}
	}
	same = false // drug uporabnik ali agent prehoda
	if got := get("GET", "/v1/models", "", av); got != 401 {
		t.Fatalf("other user: %d", got)
	}
	same = true
	off := false
	s.mu.Lock()
	s.cfg.AllowLocalApps = &off
	s.mu.Unlock()
	if got := get("GET", "/v1/models", "", av); got != 401 {
		t.Fatalf("switch off: %d", got)
	}
	if !(serveConfig{}).localApps() {
		t.Fatal("not on by default")
	}

	// Pravo preverjanje (Linux, Windows): ta proces je istega uporabnika in ni agent.
	if runtime.GOOS == "linux" || runtime.GOOS == "windows" {
		localPeer = sameUserPeer
		s.mu.Lock()
		s.cfg.AllowLocalApps = nil
		s.mu.Unlock()
		if got := get("GET", "/v1/models", "", av); got != 200 {
			t.Fatalf("real same-user check: %d", got)
		}
	}
}
