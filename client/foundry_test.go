package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestFoundryTranslation(t *testing.T) {
	if p := foundryPort("Model management service is running on http://127.0.0.1:5273/openai/status"); p != 5273 {
		t.Fatalf("port %d", p)
	}
	if foundryPort("running on http://evil.example:5273/") != 0 || foundryPort("nothing") != 0 {
		t.Fatal("non-local URL accepted")
	}
	for _, raw := range []string{`["phi-4-mini-generic-cpu"]`, `[{"name":"phi-4-mini-generic-cpu"}]`, `{"data":[{"id":"phi-4-mini-generic-cpu"}]}`} {
		if n := foundryNames(json.RawMessage(raw)); len(n) != 1 || n[0] != "phi-4-mini-generic-cpu" {
			t.Fatalf("names %s: %v", raw, n)
		}
	}
	if s := stripReminders("hi <system-reminder>CLAUDE.md …</system-reminder> there"); s != "hi  there" {
		t.Fatalf("strip: %q", s)
	}
	img := map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/png", "data": "iVBORw0K"}}
	body := map[string]any{"max_tokens": 100.0, "temperature": 0.2, "stop_sequences": []any{"END"}, "tools": []any{map[string]any{"name": "Read"}},
		"system": "long Claude Code system prompt",
		"messages": []any{
			map[string]any{"role": "assistant", "content": "orphan"},
			map[string]any{"role": "user", "content": "first"},
			map[string]any{"role": "assistant", "content": "ok"},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "look"}, img}},
		}}
	r, err := foundryRequest(body, "phi", "be brief", true, true)
	b, _ := json.Marshal(r)
	j := string(b)
	if err != nil || strings.Contains(j, "tools") || strings.Contains(j, "orphan") || strings.Contains(j, "Claude Code system") ||
		!strings.Contains(j, `"max_completion_tokens":100`) || !strings.Contains(j, `"stop":["END"]`) || !strings.Contains(j, `"include_usage":true`) ||
		!strings.Contains(j, `{"content":"be brief","role":"system"}`) || !strings.Contains(j, `"url":"data:image/png;base64,iVBORw0K"`) {
		t.Fatalf("request: %v %s", err, j)
	}
	if r, _ := foundryRequest(body, "phi", "x", false, false); strings.Contains(mustJSON(r), "image_url") || !strings.Contains(mustJSON(r), "image(s) left out") {
		t.Fatalf("text-only model got an image: %s", mustJSON(r))
	}
	body["tool_choice"] = map[string]any{"type": "any"}
	if _, err := foundryRequest(body, "phi", "x", false, false); err == nil {
		t.Fatal("tool_choice any accepted")
	}
	if foundryStop("length") != "max_tokens" || foundryStop("stop") != "end_turn" {
		t.Fatal("stop reasons")
	}
}

func mustJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

// fakeFoundry: strežnik, združljiv z OpenAI, kot Foundry Local (status, modeli, katalog, nalaganje, chat).
func fakeFoundry(t *testing.T) (*httptest.Server, *[]map[string]any, *sync.Mutex) {
	var mu sync.Mutex
	var got []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/openai/status":
			io.WriteString(w, `{"Endpoints":["http://localhost:1"],"ModelDirPath":"/m","PipeName":"p"}`)
		case "/openai/models":
			io.WriteString(w, `["phi-4-mini-generic-cpu","qwen-vl-generic-gpu"]`)
		case "/foundry/list":
			io.WriteString(w, `[{"name":"qwen-vl-generic-gpu","inputModalities":"text,image"},{"name":"phi-4-mini-generic-cpu","task":"chat-completion"}]`)
		case "/openai/load/phi-4-mini-generic-cpu", "/openai/load/qwen-vl-generic-gpu":
			io.WriteString(w, `{}`)
		case "/v1/chat/completions":
			var b map[string]any
			json.NewDecoder(r.Body).Decode(&b)
			mu.Lock()
			got = append(got, b)
			mu.Unlock()
			if b["stream"] == true {
				w.Header().Set("Content-Type", "text/event-stream")
				for _, c := range []string{`{"choices":[{"delta":{"content":"Hel"}}]}`, `{"choices":[{"delta":{"content":"lo"},"finish_reason":"stop"}]}`,
					`{"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":2}}`} {
					io.WriteString(w, "data: "+c+"\n\n")
				}
				io.WriteString(w, "data: [DONE]\n\n")
				return
			}
			io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"Hi there"},"finish_reason":"length"}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &got, &mu
}

func TestFoundryThroughGateway(t *testing.T) {
	srv, got, mu := fakeFoundry(t)
	port := srv.URL[strings.LastIndex(srv.URL, ":")+1:]
	started := false
	o := foundryCommand
	t.Cleanup(func() { foundryCommand = o })
	foundryCommand = func(_ context.Context, _ string, args ...string) (string, error) {
		switch strings.Join(args, " ") {
		case "server status":
			if started {
				return "Model management service is running on http://127.0.0.1:" + port + "/openai/status\n", nil
			}
			return "Model management service is not running!\n", nil
		case "server start":
			started = true
			return "started\n", nil
		}
		return "", os.ErrNotExist
	}
	exe, _ := os.Executable()
	s, _, base := liveServer(t, func(c *serveConfig) {
		c.Foundry = foundryConfig{Enabled: true, Executable: exe, Instructions: "be brief"}
	})
	host := strings.TrimPrefix(base, "http://")
	_, models := adminReq(t, base, host, "tok", "GET", "/v1/models", "")
	if !started || !strings.Contains(models, `"claude-foundry-phi-4-mini-generic-cpu"`) || !strings.Contains(models, `"claude-foundry-qwen-vl-generic-gpu"`) {
		t.Fatalf("models (started=%v): %s", started, models)
	}
	st, b := adminReq(t, base, host, "tok", "POST", "/v1/messages", `{"model":"claude-foundry-phi-4-mini-generic-cpu","max_tokens":5,"tools":[{"name":"Read"}],"messages":[{"role":"user","content":"hi"}]}`)
	if st != 200 || !strings.Contains(b, `"text":"Hi there"`) || !strings.Contains(b, `"stop_reason":"max_tokens"`) || !strings.Contains(b, `"input_tokens":7`) ||
		!strings.Contains(b, `"output_tokens":3`) {
		t.Fatalf("chat: %d %s", st, b)
	}
	st, b = adminReq(t, base, host, "tok", "POST", "/v1/messages", `{"model":"claude-foundry-phi-4-mini-generic-cpu","stream":true,"messages":[{"role":"user","content":"hi"}]}`)
	if st != 200 || !strings.Contains(b, `"text":"Hel"`) || !strings.Contains(b, `"text":"lo"`) || !strings.Contains(b, `"stop_reason":"end_turn"`) ||
		!strings.Contains(b, `"output_tokens":2`) || !strings.Contains(b, "message_stop") {
		t.Fatalf("stream: %d %s", st, b)
	}
	mu.Lock()
	if len(*got) != 2 || (*got)[0]["model"] != "phi-4-mini-generic-cpu" || (*got)[0]["tools"] != nil || !strings.Contains(mustJSON((*got)[0]["messages"]), "be brief") {
		t.Fatalf("upstream requests: %v", *got)
	}
	mu.Unlock()
	if st, _ := adminReq(t, base, host, "tok", "POST", "/v1/messages", `{"model":"claude-foundry-nope","messages":[{"role":"user","content":"hi"}]}`); st != 404 {
		t.Fatalf("unknown model: %d", st)
	}
	if st, _ := adminReq(t, base, host, "tok", "POST", "/v1/messages", `{"model":"claude-foundry-phi-4-mini-generic-cpu","tool_choice":{"type":"any"},"messages":[{"role":"user","content":"hi"}]}`); st != 400 {
		t.Fatalf("tool_choice any: %d", st)
	}
	// Skrbniški API /foundry: stanje, modeli, izklop modela in navodila.
	if st, b := adminNoise(t, s, base, s.admin, "GET", adminPrefix+"/foundry", ""); st != 200 || !strings.Contains(b, `"running":true`) ||
		!strings.Contains(b, `"found":true`) || !strings.Contains(b, `"instructions":"be brief"`) || !strings.Contains(b, `"models":null`) {
		t.Fatalf("GET foundry: %d %s", st, b)
	}
	if st, b := adminNoise(t, s, base, s.admin, "GET", adminPrefix+"/foundry/models", ""); st != 200 ||
		!strings.Contains(b, `{"enabled":true,"name":"qwen-vl-generic-gpu","vision":true}`) || !strings.Contains(b, `"vision":false`) {
		t.Fatalf("foundry models: %d %s", st, b)
	}
	if st, b := adminNoise(t, s, base, s.admin, "PATCH", adminPrefix+"/foundry", `{"models":["qwen-vl-generic-gpu"],"instructions":"short"}`); st != 200 ||
		!strings.Contains(b, `"models":["qwen-vl-generic-gpu"]`) || s.conf().Foundry.Instructions != "short" {
		t.Fatalf("PATCH foundry: %d %s", st, b)
	}
	if _, m := adminReq(t, base, host, "tok", "GET", "/v1/models", ""); strings.Contains(m, "claude-foundry-phi") || !strings.Contains(m, "claude-foundry-qwen-vl") {
		t.Fatalf("disabled model listed: %s", m)
	}
	if st, _ := adminNoise(t, s, base, s.admin, "PATCH", adminPrefix+"/foundry", `{"models":["-rf"]}`); st != 400 {
		t.Fatal("bad model name accepted")
	}
}

// Rok: počasen strežnik ne zadrži zahteve dlje od foundryTimeout.
func TestFoundryTimeout(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/openai/status" {
			io.WriteString(w, `{"Endpoints":[]}`)
			return
		}
		if r.URL.Path == "/openai/models" {
			io.WriteString(w, `["m"]`)
			return
		}
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer slow.Close()
	port := slow.URL[strings.LastIndex(slow.URL, ":")+1:]
	o, ot := foundryCommand, foundryTimeout
	t.Cleanup(func() { foundryCommand, foundryTimeout = o, ot })
	foundryCommand = func(context.Context, string, ...string) (string, error) {
		return "running on http://127.0.0.1:" + port + "/openai/status", nil
	}
	foundryTimeout = 300 * time.Millisecond
	exe, _ := os.Executable()
	_, _, base := liveServer(t, func(c *serveConfig) { c.Foundry = foundryConfig{Enabled: true, Executable: exe} })
	start := time.Now()
	st, _ := adminReq(t, base, strings.TrimPrefix(base, "http://"), "tok", "POST", "/v1/messages", `{"model":"claude-foundry-m","messages":[{"role":"user","content":"hi"}]}`)
	if st == 200 || time.Since(start) > 3*time.Second {
		t.Fatalf("slow Foundry: %d after %v", st, time.Since(start))
	}
}
