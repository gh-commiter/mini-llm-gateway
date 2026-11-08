package main

import (
	"net/http"
	"net/http/httptest"
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
