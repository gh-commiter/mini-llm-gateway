package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const gwKey = "sk-test-secret-123"

// fakeKeys: shramba ključev v pomnilniku namesto Credential Manager / Keychain / secret-tool.
func fakeKeys(t *testing.T) map[string]string {
	m := map[string]string{}
	var mu sync.Mutex
	g, s, d := keyGet, keySet, keyDel
	keyGet = func(_, n string) (string, string) { mu.Lock(); defer mu.Unlock(); return m[n], "test" }
	keySet = func(_, n, k string) (string, error) { mu.Lock(); defer mu.Unlock(); m[n] = k; return "test", nil }
	keyDel = func(_, n string) { mu.Lock(); defer mu.Unlock(); delete(m, n) }
	t.Cleanup(func() { keyGet, keySet, keyDel = g, s, d })
	return m
}

// fakeUpstream: Anthropic-compatible gateway (kot LiteLLM: okna v /model/info). step: tok čaka, da odjemalec prebere dogodek.
func fakeUpstream(t *testing.T, step chan struct{}, got chan map[string]json.RawMessage, evil *httptest.Server) *httptest.Server {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("authorization") != "Bearer "+gwKey {
			http.Error(w, `{"type":"error","error":{"type":"authentication_error","message":"bad key"}}`, 401)
			return
		}
		switch r.URL.Path {
		case "/v1/models":
			io.WriteString(w, `{"data":[{"id":"anthropic/claude-x","context_length":200000},{"id":"gpt-y"},{"id":"other-z"},{"id":"off-w"},{"id":"llama-*"}]}`)
			return
		case "/model/info":
			io.WriteString(w, `{"data":[{"model_name":"gpt-y","model_info":{"max_input_tokens":8000}}]}`)
			return
		case "/v1/messages/count_tokens":
			io.WriteString(w, `{"input_tokens":42}`)
			return
		}
		var body map[string]json.RawMessage
		json.NewDecoder(r.Body).Decode(&body)
		if r.Header.Get("anthropic-version") != "2023-06-01" {
			t.Errorf("anthropic-version not forwarded")
		}
		got <- body
		switch string(body["model"]) {
		case `"gpt-y"`:
			http.Error(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`, 429)
		case `"other-z"`:
			http.Redirect(w, r, evil.URL+"/v1/messages", http.StatusTemporaryRedirect)
		default:
			w.Header().Set("Content-Type", "text/event-stream")
			for i := range 3 {
				io.WriteString(w, "event: content_block_delta\ndata: {\"i\":"+string(rune('0'+i))+"}\n\n")
				w.(http.Flusher).Flush()
				select { // brez sprotnega pošiljanja odjemalec dogodka ne dobi → rok
				case <-step:
				case <-time.After(5 * time.Second):
					t.Errorf("event %d was not delivered before the next one", i)
					return
				}
			}
		}
	}))
	t.Cleanup(up.Close)
	return up
}

func TestServeGatewayPassthrough(t *testing.T) {
	keys := fakeKeys(t)
	keys["gw"] = gwKey
	evilHit := false
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { evilHit = true }))
	defer evil.Close()
	step, got := make(chan struct{}), make(chan map[string]json.RawMessage, 1)
	up := fakeUpstream(t, step, got, evil)

	s, _ := testServer(t, func(c *serveConfig) {
		c.Gateways = []serveGateway{{Name: "gw", BaseURL: up.URL + "/v1/", Insecure: true,
			Models: []string{"anthropic/claude-x", "gpt-y", "other-z"}}}
	})
	var logs bytes.Buffer
	var logMu sync.Mutex
	s.log.SetOutput(writerFunc(func(p []byte) (int, error) { logMu.Lock(); defer logMu.Unlock(); return logs.Write(p) }))
	conn := startLoopback(t, s)
	post := func(path, body string) *http.Response {
		r, _ := http.NewRequest("POST", conn.URL+path, strings.NewReader(body))
		r.Header.Set("authorization", "Bearer tok")
		r.Header.Set("anthropic-version", "2023-06-01")
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	// Odkrivanje: vklopljeni modeli z imeni kot na Macu, okna iz /v1/models in /model/info; brez wildcard in izklopljenih.
	r, _ := http.NewRequest("GET", conn.URL+"/v1/models", nil)
	r.Header.Set("authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	list, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, want := range []string{`"id":"anthropic/claude-x"`, `gw · anthropic/claude-x · 200K`, `"id":"claude-gpt-y"`, `gw · gpt-y · 8K`} {
		if !strings.Contains(string(list), want) {
			t.Fatalf("models: missing %s in %s", want, list)
		}
	}
	if strings.Contains(string(list), "off-w") || strings.Contains(string(list), "llama") {
		t.Fatalf("disabled or wildcard model listed: %s", list)
	}

	// Tok: dogodki po vrsti in sproti; orodja nespremenjena; model zamenjan z ID-jem pri upstreamu.
	tools := `[{"name":"Edit","input_schema":{"type":"object","properties":{"z":{"type":"integer","maximum":12345678901234567890},"a":{"type":"string"}}}}]`
	resp = post("/v1/messages", `{"model":"anthropic/claude-x","stream":true,"max_tokens":5,"tools":`+tools+`,"messages":[{"role":"user","content":"hi"}]}`)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("stream: %d %s", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	body := <-got
	var compact bytes.Buffer
	json.Compact(&compact, []byte(tools))
	if !bytes.Equal(body["tools"], compact.Bytes()) {
		t.Fatalf("tools changed:\n%s\n%s", body["tools"], compact.Bytes())
	}
	if string(body["model"]) != `"anthropic/claude-x"` {
		t.Fatalf("model: %s", body["model"])
	}
	br := bufio.NewReader(resp.Body)
	for i := range 3 {
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				t.Fatalf("event %d: %v", i, err)
			}
			if strings.HasPrefix(line, "data: ") {
				if line != "data: {\"i\":"+string(rune('0'+i))+"}\n" {
					t.Fatalf("event %d out of order: %q", i, line)
				}
				break
			}
		}
		select { // upstream čaka na to, preden pošlje naslednji dogodek
		case step <- struct{}{}:
		case <-time.After(5 * time.Second):
			t.Fatalf("event %d arrived only after the upstream gave up: not flushed", i)
		}
	}
	resp.Body.Close()

	// count_tokens gre k upstreamu; napaka upstreama kot napaka Anthropic; preusmeritev ne odnese ključa.
	resp = post("/v1/messages/count_tokens", `{"model":"anthropic/claude-x","messages":[]}`)
	if b, _ := io.ReadAll(resp.Body); string(b) != `{"input_tokens":42}` {
		t.Fatalf("count_tokens: %s", b)
	}
	resp = post("/v1/messages", `{"model":"claude-gpt-y","messages":[{"role":"user","content":"hi"}]}`)
	<-got
	if b, _ := io.ReadAll(resp.Body); resp.StatusCode != 429 || !strings.Contains(string(b), `"rate_limit_error"`) || !strings.Contains(string(b), "gw: slow down") {
		t.Fatalf("upstream error: %d %s", resp.StatusCode, b)
	}
	resp = post("/v1/messages", `{"model":"claude-other-z","messages":[{"role":"user","content":"hi"}]}`)
	<-got
	if b, _ := io.ReadAll(resp.Body); resp.StatusCode != 502 || evilHit {
		t.Fatalf("redirect followed: %d %s evil=%v", resp.StatusCode, b, evilHit)
	}
	resp = post("/v1/messages", `{"model":"claude-gpt-y","messages":[{"role":"user","content":"`+strings.Repeat("x", 40000)+`"}]}`)
	if b, _ := io.ReadAll(resp.Body); resp.StatusCode != 400 || !strings.Contains(string(b), "prompt is too long") {
		t.Fatalf("window: %d %s", resp.StatusCode, b)
	}
	if resp = post("/v1/messages", `{"model":"claude-off-w","messages":[]}`); resp.StatusCode != 404 {
		t.Fatalf("disabled model routed: %d", resp.StatusCode)
	}

	// Napačen ključ: 502 (ne 401, ki bi ga Claude Code razumel kot napačen žeton routerja).
	keys["gw"] = "sk-wrong"
	resp = post("/v1/messages", `{"model":"anthropic/claude-x","messages":[]}`)
	if b, _ := io.ReadAll(resp.Body); resp.StatusCode != 502 || !strings.Contains(string(b), "check the API key") {
		t.Fatalf("bad key: %d %s", resp.StatusCode, b)
	}
	time.Sleep(50 * time.Millisecond) // zadnja vrstica dnevnika
	logMu.Lock()
	defer logMu.Unlock()
	if strings.Contains(logs.String(), gwKey) || !strings.Contains(logs.String(), "anthropic/claude-x") {
		t.Fatalf("log: %s", logs.String())
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

func TestServeGatewayCLIKeepsKeyOutOfConfig(t *testing.T) {
	keys := fakeKeys(t)
	up := fakeUpstream(t, nil, nil, nil)
	cfg := filepath.Join(t.TempDir(), "serve.json")
	saveServeConfig(cfg, serveConfig{Token: "tok", Key: "k"})

	gatewayCommand(cfg, []string{"add", "gw", up.URL}, true, "", "", strings.NewReader(gwKey+"\n"))
	gatewayCommand(cfg, []string{"models", "gw"}, false, "", "other-z,claude-off-w", nil)
	data, _ := os.ReadFile(cfg)
	if strings.Contains(string(data), gwKey) || keys["gw"] != gwKey {
		t.Fatalf("key in config or not stored: %s", data)
	}
	c, _ := loadServeConfig(cfg)
	if len(c.Gateways) != 1 || c.Gateways[0].BaseURL != up.URL || !c.Gateways[0].Insecure ||
		strings.Join(c.Gateways[0].Models, ",") != "anthropic/claude-x,gpt-y" {
		t.Fatalf("config: %+v", c.Gateways)
	}
	gatewayCommand(cfg, []string{"remove", "gw"}, false, "", "", nil)
	if c, _ = loadServeConfig(cfg); len(c.Gateways) != 0 || keys["gw"] != "" {
		t.Fatalf("remove: %+v %v", c.Gateways, keys)
	}
}

func TestServeGatewayURLRules(t *testing.T) {
	for _, tc := range []struct {
		url      string
		insecure bool
		ok       string
	}{
		{"https://openrouter.ai/api", false, "https://openrouter.ai/api"},
		{"https://gw.example/v1/", false, "https://gw.example"},
		{"http://gw.example", true, ""},      // http na javni naslov nikoli
		{"http://127.0.0.1:4000", false, ""}, // brez --insecure
		{"http://127.0.0.1:4000", true, "http://127.0.0.1:4000"},
		{"http://192.168.1.5:4000", true, "http://192.168.1.5:4000"},
		{"https://user:pw@gw.example", false, ""},
		{"ftp://gw.example", false, ""},
	} {
		got, err := checkGatewayURL(tc.url, tc.insecure)
		if got != tc.ok || (err == nil) != (tc.ok != "") {
			t.Errorf("%s insecure=%v: %q %v", tc.url, tc.insecure, got, err)
		}
	}
	// Ročno vpisan http v serve.json: router se ne zažene.
	k, _ := testServer(t, nil)
	c := k.cfg
	c.Gateways = []serveGateway{{Name: "gw", BaseURL: "http://gw.example"}}
	if _, err := newServer(c, filepath.Join(t.TempDir(), "serve.json")); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("http gateway accepted: %v", err)
	}
}

// Na macOS ključ v gateway-keys.json (0600), nikoli v Keychain.
func TestServeGatewayKeyFileOnMac(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("macOS only")
	}
	dir := t.TempDir()
	if where, err := osKeySet(dir, "gw", gwKey); err != nil || !strings.HasSuffix(where, "gateway-keys.json (mode 0600)") {
		t.Fatalf("set: %q %v", where, err)
	}
	if st, _ := os.Stat(filepath.Join(dir, "gateway-keys.json")); st == nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode: %v", st)
	}
	if k, _ := osKeyGet(dir, "gw"); k != gwKey {
		t.Fatalf("get: %q", k)
	}
	if osKeyDel(dir, "gw"); func() bool {
		b, _ := os.ReadFile(filepath.Join(dir, "gateway-keys.json"))
		return strings.Contains(string(b), gwKey)
	}() {
		t.Fatal("not deleted")
	}
	// Pokvarjena datoteka: napaka, ne tiho prazen seznam (G3).
	path := filepath.Join(dir, "gateway-keys.json")
	os.WriteFile(path, []byte("{broken"), 0o600)
	if _, err := osKeySet(dir, "gw", gwKey); err == nil {
		t.Fatal("corrupt key file overwritten")
	}
	if b, _ := os.ReadFile(path); string(b) != "{broken" {
		t.Fatalf("corrupt key file changed: %q", b)
	}
	// Povezava na poti: zamenjana, cilj nedotaknjen.
	outside := filepath.Join(t.TempDir(), "outside")
	os.WriteFile(outside, []byte("{}"), 0o644)
	os.Remove(path)
	os.Symlink(outside, path)
	if _, err := osKeySet(dir, "gw", gwKey); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(outside); string(b) != "{}" {
		t.Fatalf("symlink followed: %q", b)
	}
	if st, _ := os.Lstat(path); st == nil || st.Mode()&os.ModeSymlink != 0 || st.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v", st)
	}
}

// Prehod, ki ne odgovori: model claude-cli ne čaka nanj, neznan model največ 2 s (prvo nalaganje).
func TestServeGatewayDeadDoesNotDelayCLI(t *testing.T) {
	fakeKeys(t)["dead"] = gwKey
	block := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { <-block }))
	t.Cleanup(up.Close)
	t.Cleanup(func() { close(block) }) // teče pred up.Close
	s, _ := testServer(t, func(c *serveConfig) {
		c.Claude = "/bin/sh"
		c.Gateways = []serveGateway{{Name: "dead", BaseURL: up.URL, Insecure: true}}
	})
	conn := startLoopback(t, s)
	start := time.Now()
	st, _ := call(conn, "POST", "/v1/messages/count_tokens", nil, []byte(`{"model":"claude-cli-haiku","messages":[]}`), 5*time.Second)
	if d := time.Since(start); st != 200 || d > time.Second {
		t.Fatalf("claude-cli waited for the gateway: %d after %v", st, d)
	}
	start = time.Now()
	st, _ = call(conn, "POST", "/v1/messages", nil, []byte(`{"model":"claude-nope","messages":[]}`), 5*time.Second)
	if d := time.Since(start); st != 404 || d < 1500*time.Millisecond || d > 3*time.Second {
		t.Fatalf("unknown model: %d after %v (want 404 after about 2 s)", st, d)
	}
}

// Prehod ne more zakriti claude-cli (G4); pri 16 hkratnih zahtevah 429 (G5).
func TestServeGatewayNoCLIShadowAndLimit(t *testing.T) {
	fakeKeys(t)["gw"] = gwKey
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			io.WriteString(w, `{"data":[{"id":"claude-cli-haiku"},{"id":"cli-sonnet"},{"id":"chat-opus"},{"id":"claude-chat-x"},{"id":"echo"}]}`)
		default:
			io.WriteString(w, `{"input_tokens":42}`)
		}
	}))
	defer up.Close()
	s, _ := testServer(t, func(c *serveConfig) {
		c.Claude = "/bin/sh"
		c.Gateways = []serveGateway{{Name: "gw", BaseURL: up.URL, Insecure: true}}
	})
	conn := startLoopback(t, s)
	_, list := call(conn, "GET", "/v1/models", nil, nil, 5*time.Second)
	if strings.Contains(string(list), "gw · claude-cli") || strings.Contains(string(list), "cli-sonnet · gw") ||
		strings.Contains(string(list), "claude-chat-x · gw") || strings.Contains(string(list), "chat-opus") || !strings.Contains(string(list), "claude-echo") {
		t.Fatalf("models: %s", list)
	}
	body := []byte(`{"model":"claude-cli-haiku","messages":[]}`)
	if _, data := call(conn, "POST", "/v1/messages/count_tokens", nil, body, 5*time.Second); strings.Contains(string(data), "42") {
		t.Fatalf("gateway shadowed claude-cli: %s", data)
	}
	for range cap(s.gwSlots) {
		s.gwSlots <- struct{}{}
	}
	st, data := call(conn, "POST", "/v1/messages/count_tokens", nil, []byte(`{"model":"claude-echo","messages":[]}`), 5*time.Second)
	if st != 429 || !strings.Contains(string(data), "rate_limit_error") {
		t.Fatalf("limit: %d %s", st, data)
	}
}
