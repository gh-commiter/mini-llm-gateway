package main

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

// GET /activity: tekoča zahteva s korakom, končane najnovejše prve, največ 200, brez vsebine.
func TestActivity(t *testing.T) {
	claude := "/bin/sh"
	if runtime.GOOS == "windows" { // resolveClaude: obstoječ .exe
		claude, _ = os.Executable()
	}
	s, _ := testServer(t, func(c *serveConfig) { c.Claude = claude })
	get := func() (out struct {
		Inflight []activityRun
		Requests []activityReq
	}, raw string) {
		st, v := s.adminDo("GET", "/activity", nil)
		b, _ := json.Marshal(v)
		if st != 200 || json.Unmarshal(b, &out) != nil {
			t.Fatalf("GET /activity: %d %s", st, b)
		}
		return out, string(b)
	}
	if _, raw := get(); raw != `{"inflight":[],"requests":[]}` {
		t.Fatalf("empty: %s", raw)
	}

	r := &req{Request: httptest.NewRequest("POST", "/v1/messages", nil), rid: "req_1", body: []byte(`{"model":"claude-x","messages":[{"role":"user","content":"SECRET"}]}`)}
	stop := s.activity.start(r, "claude-x")
	s.activity.step("req_1", "Read a.go")
	if a, raw := get(); len(a.Inflight) != 1 || a.Inflight[0].Model != "claude-x" || a.Inflight[0].Step != "Read a.go" || strings.Contains(raw, "SECRET") {
		t.Fatalf("inflight: %s", raw)
	}
	stop()
	r.URL.Path = "/v1/messages/count_tokens"
	s.activity.start(r, "claude-x") // count_tokens ni tekoča zahteva (kot na Macu)
	if a, _ := get(); len(a.Inflight) != 0 {
		t.Fatalf("still running: %v", a.Inflight)
	}
	r.URL.Path = "/v1/messages"
	stop = s.activity.start(r, strings.Repeat("m", 201)) // niz odjemalca ne raste v pomnilnik
	if a, _ := get(); a.Inflight[0].Model != "unknown" {
		t.Fatalf("long model: %q", a.Inflight[0].Model)
	}
	stop()

	body := `{"model":"claude-cli-haiku","messages":[{"role":"user","content":"SECRET"}]}`
	if st, _ := call(startLoopback(t, s), "POST", "/v1/messages/count_tokens", nil, []byte(body), 5*time.Second); st != 200 {
		t.Fatalf("count_tokens: %d", st)
	}
	a, raw := get()
	if len(a.Requests) != 1 || strings.Contains(raw, "SECRET") {
		t.Fatalf("requests: %s", raw)
	}
	if x := a.Requests[0]; x.Model != "claude-cli-haiku" || x.Status != 200 || x.Path != "/v1/messages/count_tokens" || x.Bytes != len(body) || x.Method != "POST" {
		t.Fatalf("request: %+v", x)
	}

	// neznan model (404): »unknown«, ne niz odjemalca (M1)
	if st, _ := call(startLoopback(t, s), "POST", "/v1/messages/count_tokens", nil, []byte(`{"model":"claude-codex-SECRET","messages":[]}`), 5*time.Second); st != 404 {
		t.Fatalf("unknown model: %d", st)
	}
	if a, raw = get(); a.Requests[0].Model != "unknown" || a.Requests[0].Status != 404 || strings.Contains(raw, "SECRET") {
		t.Fatalf("404: %s", raw)
	}
	if !codexMessage([]byte(`{"type":"item.completed","item":{"type":"agent_message","text":"answer"}}`)) ||
		codexMessage([]byte(`{"type":"item.started","item":{"type":"command_execution","command":"ls"}}`)) {
		t.Fatal("codexMessage")
	}

	for i := range activityMax + 5 {
		s.activity.done(activityReq{Model: fmt.Sprint(i)})
	}
	if a, _ = get(); len(a.Requests) != activityMax || a.Requests[0].Model != fmt.Sprint(activityMax+4) {
		t.Fatalf("ring: %d, first %s", len(a.Requests), a.Requests[0].Model)
	}
}
