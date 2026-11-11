package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Vrata izvajanja: le žeton izvajanja in tri poti; zahteva dobi napravo, sejo in mapo klicatelja.
func TestAgentRunListener(t *testing.T) {
	s, _ := testServer(t, func(c *serveConfig) { c.ClaudeAskOthers, c.AllowLocalTools = true, true })
	dev := &serveDevice{Name: "phone"}
	parent := httptest.NewRequest("POST", "/v1/messages", nil)
	parent.Header.Set("X-Claude-Mini-Session", "sid1")
	parent.Header.Set("X-Claude-Mini-Cwd", "/client/dir")
	r := &req{Request: parent, conn: &serveConn{device: dev, peer: "10.0.0.9"}}
	run, env := s.startRun(r, "claude", "/copy", false, true, "", "", "", "")
	run.started(os.Getpid())                                                              // M2: ta proces je »agent« izvajanja
	if runtime.GOOS == "windows" && len(env) == 3 && strings.HasPrefix(env[2], "PATH=") { // askOnPath: minigw.exe na PATH agenta
		env = env[:2]
	}
	if run == nil || len(env) != 2 || !strings.HasPrefix(env[0], "MINIGW_AGENT_URL=http://127.0.0.1:") || env[1] != "MINIGW_AGENT_AUTH_FILE="+run.authFile {
		t.Fatalf("env: %v", env)
	}
	// M3: žeton le v datoteki 0600 (ne v okolju), po koncu izvajanja izbrisan
	if b, err := os.ReadFile(run.authFile); err != nil || string(b) != run.token || strings.Contains(strings.Join(env, " "), run.token) {
		t.Fatalf("auth file: %q %v", b, err)
	}
	if fi, _ := os.Stat(run.authFile); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("auth file mode %v", fi.Mode())
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
	if st := get("/v1/models", run.token); st != 403 { // oddaljena naprava, ki je prehod ne pozna: kot izvirna zahteva
		t.Fatalf("models for an unknown remote device: %d", st)
	}
	s.mu.Lock()
	s.cfg.Devices = []serveDevice{*dev}
	s.mu.Unlock()
	if st := get("/v1/models", run.token); st != 200 {
		t.Fatalf("models: %d", st)
	}
	for range maxNested { // M1: vsa mesta zasedena → 429
		run.slots <- struct{}{}
	}
	if st := get("/v1/models", run.token); st != 429 {
		t.Fatalf("over the nested limit: %d", st)
	}
	for range maxNested {
		<-run.slots
	}
	if runtime.GOOS == "linux" || runtime.GOOS == "windows" { // M2: proces zunaj drevesa izvajanja
		sleep := exec.Command("sleep", "30")
		if runtime.GOOS == "windows" {
			sleep = exec.Command("ping", "-n", "30", "127.0.0.1")
		}
		if err := sleep.Start(); err != nil {
			t.Fatal(err)
		}
		defer sleep.Process.Kill()
		run.started(sleep.Process.Pid)
		if st := get("/v1/models", run.token); st != 403 {
			t.Fatalf("token from outside the run's processes: %d", st)
		}
		run.started(os.Getpid())
	}
	run.stop()
	if st := get("/v1/models", run.token); st != -1 {
		t.Fatalf("listener open after the run: %d", st)
	}
	if _, err := os.Stat(run.ownDir); err == nil {
		t.Fatal("auth file left after the run")
	}

	// Stikalo izklopljeno: ni vrat ne okolja.
	if run, env := s.startRun(r, "codex", "/copy", false, true, "", "", "", ""); run != nil || env != nil {
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
		return `curl -s -o /dev/null -w '%{http_code}' -H "Authorization: Bearer $(cat "$MINIGW_AGENT_AUTH_FILE")" -H 'content-type: application/json' ` +
			`-d '{"model":"` + model + `","max_tokens":10,"tools":[{"name":"x"}],"messages":[{"role":"user","content":"hi"}]}' "$MINIGW_AGENT_URL/v1/messages"`
	}
	claude := filepath.Join(tmp, "claude")
	writeT(t, claude, `#!/bin/sh
`+cliGuard+`
cat > /dev/null
prev=""; for a in "$@"; do [ "$prev" = --append-system-prompt-file ] && cp "$a" note.txt; [ "$prev" = --settings ] && printf '%s' "$a" > settings.txt; prev="$a"; done
echo "$(`+ask("claude-codex")+`)" > asked.txt
echo '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"done"}}}'
echo '{"type":"result","subtype":"success","usage":{"output_tokens":3}}'
`)
	codex := filepath.Join(tmp, "codex")
	writeT(t, codex, `#!/bin/sh
if [ "$3" = debug ]; then echo '{"models":[]}'; exit 0; fi
out=""; prev=""
for a in "$@"; do [ "$prev" = "-o" ] && out="$a"; prev="$a"; done
cat > prompt.txt
echo codex > codex.txt
echo "$(`+ask("claude-cli-haiku")+`)" > loop.txt
echo '{"type":"thread.started","thread_id":"t1"}'
echo "codex answer" > "$out"
`)
	os.Chmod(claude, 0o755)
	os.Chmod(codex, 0o755)
	s, _ := testServer(t, func(c *serveConfig) {
		c.Claude, c.ClaudeAskOthers, c.AllowLocalTools = claude, true, true
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
	exe := minigwExe()
	if n := read("note.txt"); !strings.Contains(n, "You may ask another model of this gateway: `\""+exe+"\" -p --model") {
		t.Fatalf("claude's note: %q", n)
	}
	if st := read("settings.txt"); !strings.Contains(st, `"allowRead":[`) || !strings.Contains(st, exe) {
		t.Fatalf("minigw not readable in claude's sandbox: %s", st)
	}
	if p := read("prompt.txt"); !strings.Contains(p, "Another agent on this gateway (claude) asks you this") || !strings.Contains(p, "You may ask another model") {
		t.Fatalf("codex's prompt: %q", p)
	}
	if got := read("loop.txt"); got != "403" {
		t.Fatalf("codex → claude loop: %q", got)
	}
	// Codex vpraša Claude (vprašani claude -p, mapa klicatelja): tudi ta s cliGuard (N9)
	body = `{"model":"claude-codex","tools":[{"name":"x"}],"messages":[{"role":"user","content":"go"}]}`
	if st, data := call(c, "POST", "/v1/messages", h, []byte(body), 60*time.Second); st != 200 {
		t.Fatalf("codex: %d %s", st, data)
	}
	if got := read("loop.txt"); got != "200" {
		t.Fatalf("codex → claude: %q", got)
	}
}

// minigw v agentu: le povezava iz okolja, brez profilov.
func TestAgentModeConnection(t *testing.T) {
	t.Setenv("MINIGW_AGENT_URL", "http://127.0.0.1:1")
	t.Setenv("MINIGW_AGENT_AUTH_FILE", filepath.Join(t.TempDir(), "missing"))
	if agentConn() != nil {
		t.Fatal("agent mode without a token")
	}
	f := filepath.Join(t.TempDir(), "auth")
	os.WriteFile(f, []byte("rt\n"), 0o600)
	t.Setenv("MINIGW_AGENT_AUTH_FILE", f)
	for _, bad := range []string{"http://evil.example:1", "https://127.0.0.1:1", "http://127.0.0.1", "http://u@127.0.0.1:1", "http://localhost:1"} {
		t.Setenv("MINIGW_AGENT_URL", bad)
		if agentConn() != nil {
			t.Fatalf("agent mode with %s", bad)
		}
	}
	t.Setenv("MINIGW_AGENT_URL", "http://127.0.0.1:1")
	ps := loadProfiles()
	if len(ps) != 1 || ps[0].conn.URL != "http://127.0.0.1:1" || ps[0].conn.Token != "rt" || !ps[0].conn.isLocal() {
		t.Fatalf("profiles in agent mode: %+v", ps)
	}
}

// L1: v agentu ni ukazov, ki spreminjajo povezavo ali namestitev.
func TestAgentModeRefusesCommands(t *testing.T) {
	f := filepath.Join(t.TempDir(), "auth")
	os.WriteFile(f, []byte("rt\n"), 0o600)
	for _, c := range []string{"connect", "reconnect", "disconnect", "profiles", "plugin", "uninstall"} {
		cmd := exec.Command(os.Args[0], c, "--no-such-flag") // brez zavrnitve le napaka uporabe, nič se ne spremeni
		cmd.Env = append(os.Environ(), "MINIGW_TEST_MAIN=1", "MINIGW_AGENT_URL=http://127.0.0.1:1", "MINIGW_AGENT_AUTH_FILE="+f, "HOME="+t.TempDir(), "USERPROFILE="+t.TempDir())
		out, err := cmd.CombinedOutput()
		if err == nil || !strings.Contains(string(out), "minigw runs inside an agent of the gateway (MINIGW_AGENT_URL): "+c+" is not available here") {
			t.Fatalf("%s: %v %s", c, err, out)
		}
	}
}

// minigw -p in agentu: orodje le za modele agentov; posrednik iz okolja tudi za loopback.
func TestAgentAskBodyAndProxy(t *testing.T) {
	if m, w, bad := agentAskArgs([]string{"-p", "--model", "m", "--", "- fix x", "--y"}); m != "m" || bad != "" || strings.Join(w, "|") != "- fix x|--y" {
		t.Fatalf("after --: %q %q %q", m, w, bad)
	}
	if m, w, bad := agentAskArgs([]string{"--minigw-ro", "--minigw-role", "r", "-p", "--model", "m", "--minigw-work=sync", "--minigw-role=x", "q"}); m != "m" || bad != "" || strings.Join(w, "|") != "q" {
		t.Fatalf("minigw flags: %q %q %q", m, w, bad)
	}
	if _, _, bad := agentAskArgs([]string{"-p", "--model", "m", "--verbose", "q"}); bad != "--verbose" {
		t.Fatalf("flag accepted: %q", bad)
	}
	if b := string(agentBody("claude-codex", "q")); !strings.Contains(b, `"tools"`) || !strings.Contains(b, `"stream":true`) {
		t.Fatalf("agent model body: %s", b)
	}
	if b := string(agentBody("or-qwen", "q")); strings.Contains(b, `"tools"`) {
		t.Fatalf("provider model body has tools: %s", b)
	}
	t.Setenv("HTTP_PROXY", "http://u:p@127.0.0.1:9")
	t.Setenv("NO_PROXY", "127.0.0.1,localhost")
	q, _ := http.NewRequest("GET", "http://127.0.0.1:1/v1/models", nil)
	if u, err := agentProxy(q); err != nil || u == nil || u.Host != "127.0.0.1:9" {
		t.Fatalf("proxy for loopback: %v %v", u, err)
	}
	t.Setenv("HTTP_PROXY", "http://proxy.corp.example:3128") // I5: nikoli posrednik zunaj tega računalnika
	if u, _ := agentProxy(q); u != nil {
		t.Fatalf("non-loopback proxy used: %v", u)
	}
	t.Setenv("HTTP_PROXY", "http://localhost:9")
	if u, _ := agentProxy(q); u == nil {
		t.Fatal("loopback proxy (localhost) not used")
	}
	t.Setenv("HTTP_PROXY", "")
	if u, _ := agentProxy(q); u != nil {
		t.Fatalf("proxy without HTTP_PROXY: %v", u)
	}
}

// L1: vprašani agent ne dela v privzeti mapi prehoda, ki je klicatelj ni dobil od odjemalca.
func TestNestedAgentNeedsSharedFolder(t *testing.T) {
	s, _ := testServer(t, nil)
	hr := httptest.NewRequest("POST", "/v1/messages", nil)
	hr = hr.WithContext(context.WithValue(hr.Context(), agentRunKey{}, &agentRun{chain: []string{"claude"}, dir: "/gw/folder"}))
	w := httptest.NewRecorder()
	if _, _, _, st := s.agentFolder(w, &req{Request: hr}, "Codex", "codex"); st != 400 || !strings.Contains(w.Body.String(), "not shared") {
		t.Fatalf("nested agent in the gateway's folder: %d %s", st, w.Body.String())
	}
}

// Varnost L1: »minigw« iz PATH in vnaprej dovoljeno pravilo le, ko agent ne more pisati v mapo minigw.exe.
func TestAskOnPathOnlyOutsideAgentFolders(t *testing.T) {
	root := t.TempDir()
	bin, allowed, scratch := filepath.Join(root, "bin"), filepath.Join(root, "work"), filepath.Join(root, "scratch")
	for _, d := range []string{bin, allowed, scratch} {
		os.MkdirAll(d, 0o700)
	}
	if agentWritable(bin, []string{allowed, "", scratch}) {
		t.Fatal("a separate folder counts as agent-writable")
	}
	for _, f := range []string{root, bin} { // dovoljena mapa, mapa seje ali Scratch, ki vsebuje mapo minigw.exe
		if !agentWritable(bin, []string{allowed, f}) {
			t.Fatalf("%s contains the minigw folder but is not seen", f)
		}
	}
	if n := askNote(`C:\x\minigw.exe`, false); !strings.Contains(n, `"C:\x\minigw.exe" -p`) || !strings.Contains(n, "`& `") {
		t.Fatalf("absolute path note: %s", n)
	}
	if n := askNote(`C:\x\minigw.exe`, true); !strings.Contains(n, "`minigw -p") || !strings.Contains(n, "reached only this way: never run their command-line programs (codex, claude, grok, copilot)") {
		t.Fatalf("PATH note: %s", n)
	}
	// #69: brez stikala agent ne išče programov drugih agentov, odgovori sam
	if !strings.Contains(noAskNote, "not available to you here: never run their command-line programs (codex, claude, grok, copilot) yourself. If you are asked to use one, do the task yourself") {
		t.Fatalf("no-ask note: %s", noAskNote)
	}
	if runtime.GOOS != "windows" && (serveConfig{}).askOnPath(bin) {
		t.Fatal("askOnPath outside Windows")
	}
}
