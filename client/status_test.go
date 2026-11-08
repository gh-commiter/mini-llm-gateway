package main

import (
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

func TestFailureReason(t *testing.T) {
	if r := failureReason(401, nil); !strings.HasPrefix(r, "token rejected: copy the token again") {
		t.Error(r)
	}
	body := map[string]any{"error": map[string]any{"message": "approve ID abc"}}
	if r := failureReason(403, body); r != "device not approved: approve ID abc" {
		t.Error(r)
	}
	if r := failureReason(0, nil); r != "gateway not reachable" {
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
