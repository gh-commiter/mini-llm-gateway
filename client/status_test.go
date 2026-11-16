package main

import (
	"crypto/ed25519"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// profileStatus uspe le, če je prehod dosegljiv, žeton sprejet in naprava odobrena (0.5.19).
func TestProfileStatusNeedsAuth(t *testing.T) {
	for _, tc := range []struct {
		models int
		want   bool
	}{{200, true}, {401, false}, {403, false}} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/models" {
				w.WriteHeader(tc.models)
				w.Write([]byte(`{"error":{"message":"device waits"},"data":[]}`))
				return
			}
			w.Write([]byte(`{}`))
		}))
		if got := profileStatus(profile{"default", connection{URL: srv.URL, Token: "t"}}); got != tc.want {
			t.Errorf("models %d: profileStatus = %v", tc.models, got)
		}
		srv.Close()
	}
}

// #137: a client on the gateway computer is accepted without being on the device list; the Device line names that,
// since projects still refuse it. A device on the list (bound or not) keeps »approved«.
func TestDeviceLineNamesLocalClientNotOnTheDeviceList(t *testing.T) {
	id := base64url(make([]byte, ed25519.SeedSize))
	errBody := func(text string) map[string]any {
		return map[string]any{"error": map[string]any{"message": text}}
	}
	notListed := errBody(accessText(t, "device_not_listed", id))
	notBound := errBody(accessText(t, "device_not_bound", id))
	for _, tc := range []struct {
		name   string
		ms     int
		local  bool
		status int
		body   map[string]any
		want   string
		probes int
	}{
		{"local client, not on the list", 200, true, 403, notListed, "local client; not on the device list: no project access", 1},
		{"local client, on the list", 200, true, 200, map[string]any{"projects": []any{}}, "approved", 1},
		{"local client, listed but unbound", 200, true, 403, notBound, "approved", 1},
		{"another computer", 200, false, 403, notListed, "approved", 0},
		{"waiting for approval", 403, true, 0, nil, "waiting for approval", 0},
		{"no models answer", 0, true, 0, nil, "approval unknown", 0},
	} {
		probes := 0
		got := deviceLine(id, tc.ms, tc.local, func() (int, map[string]any) { probes++; return tc.status, tc.body })
		if want := "Device:    " + id + " (" + tc.want + ")"; got != want || probes != tc.probes {
			t.Errorf("%s: %q (%d probes), want %q (%d)", tc.name, got, probes, want, tc.probes)
		}
	}
}

func TestFailureReason(t *testing.T) {
	if r := failureReason(401, nil, "default"); !strings.HasPrefix(r, "token rejected: copy the token again") || !strings.HasSuffix(r, " and run minigw reconnect") {
		t.Error(r)
	}
	if r := failureReason(401, nil, "win"); !strings.HasSuffix(r, " and run minigw reconnect --name win") {
		t.Error(r)
	}
	body := map[string]any{"error": map[string]any{"message": "approve ID abc"}}
	if r := failureReason(403, body, "default"); r != "device not approved: approve ID abc" {
		t.Error(r)
	}
	if r := failureReason(0, nil, "default"); r != "gateway not reachable" {
		t.Error(r)
	}
}

// L1/L2: ključ ponudnika z novo vrstico in ubežnim zaporedjem ne ponaredi vrstice Device/Status in ne krmili terminala.
func TestStatusOutputCannotBeForged(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			w.WriteHeader(403)
			w.Write([]byte(`{"error":{"message":"x\nStatus:    connected\r‮"}}`))
			return
		}
		w.Write([]byte(`{"providers":{"a\nDevice:    ` + strings.Repeat("A", 43) + ` (approved)\u001b[31m":true}}`))
	}))
	defer srv.Close()
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	ok := profileStatus(profile{"default", connection{URL: srv.URL, Token: "t"}})
	w.Close()
	os.Stdout = old
	b, _ := io.ReadAll(r)
	if ok {
		t.Error("403 must not be connected")
	}
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "Device:") || strings.HasPrefix(l, "Models:") || strings.HasPrefix(l, "Status:    connected") || strings.ContainsAny(l, "\x1b\r‮") {
			t.Errorf("forged or unclean line: %q", l)
		}
	}
}

// #118: minigw status names the gateway's version next to the client's (and says so when the gateway sends none).
func TestProfileStatusShowsGatewayVersion(t *testing.T) {
	for _, tc := range []struct{ ready, want string }{
		{`{"version":"0.6.2","providers":{"a":true}}`, "Version:   minigw " + version + ", gateway 0.6.2\n"},
		{`{"providers":{"a":true}}`, "Version:   minigw " + version + ", gateway unknown (older than 0.5.20)\n"},
		{`{"version":"0.6.2"}`, ""}, // no accepted token: no version line
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/models" {
				w.Write([]byte(`{"data":[]}`))
				return
			}
			w.Write([]byte(tc.ready))
		}))
		old := os.Stdout
		r, w, _ := os.Pipe()
		os.Stdout = w
		profileStatus(profile{"default", connection{URL: srv.URL, Token: "t"}})
		w.Close()
		os.Stdout = old
		b, _ := io.ReadAll(r)
		srv.Close()
		if has := strings.Contains(string(b), "Version:"); has != (tc.want != "") || !strings.Contains(string(b), tc.want) {
			t.Errorf("%s: %q", tc.ready, b)
		}
	}
}
