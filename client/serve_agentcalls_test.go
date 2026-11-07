package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Vrata izvajanja: le žeton izvajanja in tri poti; zahteva dobi napravo, sejo in mapo klicatelja.
func TestAgentRunListener(t *testing.T) {
	s, _ := testServer(t, func(c *serveConfig) { c.ClaudeAskOthers = true })
	dev := &serveDevice{Name: "phone"}
	parent := httptest.NewRequest("POST", "/v1/messages", nil)
	parent.Header.Set("X-Claude-Mini-Session", "sid1")
	parent.Header.Set("X-Claude-Mini-Cwd", "/client/dir")
	r := &req{Request: parent, conn: &serveConn{device: dev}}
	run, env := s.startRun(r, "claude", "/copy", false, "", "", "")
	if run == nil || len(env) != 2 || !strings.HasPrefix(env[0], "MINIGW_AGENT_URL=http://127.0.0.1:") || env[1] != "MINIGW_AGENT_TOKEN="+run.token {
		t.Fatalf("env: %v", env)
	}
	get := func(path, tok string) int {
		q, _ := http.NewRequest("GET", run.url+path, nil)
		if tok != "" {
			q.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := http.DefaultClient.Do(q)
		if err != nil {
			return -1
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if st := get("/v1/models", ""); st != 401 {
		t.Fatalf("no token: %d", st)
	}
	if st := get("/v1/models", "tok"); st != 401 { // žeton prehoda tu ne velja
		t.Fatalf("gateway token: %d", st)
	}
	for _, p := range []string{"/admin/v1/settings", "/minigw/share", "/settings", "/health"} {
		if st := get(p, run.token); st != 404 {
			t.Fatalf("%s: %d", p, st)
		}
	}
	if st := get("/v1/models", run.token); st != 200 {
		t.Fatalf("models: %d", st)
	}
	run.stop()
	if st := get("/v1/models", run.token); st != -1 {
		t.Fatalf("listener open after the run: %d", st)
	}

	// Stikalo izklopljeno: ni vrat ne okolja.
	if run, env := s.startRun(r, "codex", "/copy", false, "", "", ""); run != nil || env != nil {
		t.Fatal("codex without askOthers got a run listener")
	}
}

func TestChainRefusal(t *testing.T) {
	q := func(chain ...string) *req {
		hr := httptest.NewRequest("POST", "/v1/messages", nil)
		if chain != nil {
			hr = hr.WithContext(context.WithValue(hr.Context(), agentRunKey{}, &agentRun{chain: chain}))
		}
		return &req{Request: hr}
	}
	for _, c := range []struct {
		chain []string
		kind  string
		ok    bool
	}{
		{nil, "claude", true},
		{[]string{"claude"}, "codex", true},
		{[]string{"claude"}, "claude", false},
		{[]string{"claude", "codex"}, "grok", false},
		{[]string{"claude", "codex"}, "claude", false},
	} {
		if got := chainRefusal(q(c.chain...), c.kind) == ""; got != c.ok {
			t.Errorf("%v → %s: allowed=%v", c.chain, c.kind, got)
		}
	}
}

// Glava verige od zunaj → 400.
func TestForgedChainHeader(t *testing.T) {
	s, _ := testServer(t, nil)
	c := startLoopback(t, s)
	st, _ := call(c, "GET", "/v1/models", map[string]string{agentChainHeader: "claude"}, nil, 5*time.Second)
	if st != 400 {
		t.Fatalf("forged chain: %d", st)
	}
}

// Lažni claude vpraša lažni codex prek MINIGW_AGENT_URL; codex piše v isto kopijo; codex, ki vpraša claude, je zavrnjen.
func TestAgentAsksAgentInSameFolder(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake agents are shell scripts")
	}
	tmp := t.TempDir()
	ask := func(model string) string { // POST na vrata izvajanja, status v datoteko
		return `curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $MINIGW_AGENT_TOKEN" -H 'content-type: application/json' ` +
			`-d '{"model":"` + model + `","max_tokens":10,"tools":[{"name":"x"}],"messages":[{"role":"user","content":"hi"}]}' "$MINIGW_AGENT_URL/v1/messages"`
	}
	claude := filepath.Join(tmp, "claude")
	writeT(t, claude, `#!/bin/sh
cat > /dev/null
echo "$(`+ask("claude-codex")+`)" > asked.txt
echo '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"done"}}}'
echo '{"type":"result","subtype":"success","usage":{"output_tokens":3}}'
`)
	codex := filepath.Join(tmp, "codex")
	writeT(t, codex, `#!/bin/sh
if [ "$1" = debug ]; then echo '{"models":[]}'; exit 0; fi
out=""; prev=""
for a in "$@"; do [ "$prev" = "-o" ] && out="$a"; prev="$a"; done
cat > /dev/null
echo codex > codex.txt
echo "$(`+ask("claude-cli-haiku")+`)" > loop.txt
echo '{"type":"thread.started","thread_id":"t1"}'
echo "codex answer" > "$out"
`)
	os.Chmod(claude, 0o755)
	os.Chmod(codex, 0o755)
	s, _ := testServer(t, func(c *serveConfig) {
		c.Claude, c.ClaudeAskOthers = claude, true
		c.AllowUnsandboxedBash = runtime.GOOS != "linux" || bwrapReady(t.TempDir()) == ""
		t.Setenv("CODEX_HOME", t.TempDir())
		c.Codex = agentCLI{Enabled: true, Executable: codex, AskOthers: true}
	})
	c := startLoopback(t, s)
	dir := t.TempDir()
	sc := &shareClient{c: c, session: "sid1", root: dir, sync: true, synced: map[string]syncEntry{}, reported: map[string]bool{}}
	if !sc.register() {
		t.Fatal("register failed: " + sc.refusal)
	}
	sc.start(2)
	defer sc.unregister()
	h := map[string]string{"x-claude-mini-session": "sid1", "x-claude-mini-cwd": dir, "content-type": "application/json"}
	body := `{"model":"claude-cli-haiku","tools":[{"name":"x"}],"messages":[{"role":"user","content":"go"}]}`
	if st, data := call(c, "POST", "/v1/messages", h, []byte(body), 60*time.Second); st != 200 {
		t.Fatalf("claude: %d %s", st, data)
	}
	read := func(n string) string { b, _ := os.ReadFile(filepath.Join(dir, n)); return strings.TrimSpace(string(b)) }
	if got := read("asked.txt"); got != "200" {
		t.Fatalf("claude → codex: %q", got)
	}
	if read("codex.txt") != "codex" {
		t.Fatal("codex did not write into the caller's folder (or it was not synced back)")
	}
	if got := read("loop.txt"); got != "403" {
		t.Fatalf("codex → claude loop: %q", got)
	}
}

// minigw v agentu: le povezava iz okolja, brez profilov.
func TestAgentModeConnection(t *testing.T) {
	t.Setenv("MINIGW_AGENT_URL", "http://127.0.0.1:1")
	t.Setenv("MINIGW_AGENT_TOKEN", "")
	if agentConn() != nil {
		t.Fatal("agent mode without a token")
	}
	t.Setenv("MINIGW_AGENT_TOKEN", "rt")
	ps := loadProfiles()
	if len(ps) != 1 || ps[0].conn.URL != "http://127.0.0.1:1" || ps[0].conn.Token != "rt" || !ps[0].conn.isLocal() {
		t.Fatalf("profiles in agent mode: %+v", ps)
	}
}
