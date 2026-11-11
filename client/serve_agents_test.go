package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// #59 (kot codexProjectsUntrusted na Macu): vsaka nadmapa cwd, tudi brez simbolnih povezav, je za zagon »untrusted«.
func TestCodexProjectsUntrusted(t *testing.T) {
	d := filepath.Join(t.TempDir(), "s")
	if err := os.Mkdir(d, 0o700); err != nil {
		t.Fatal(err)
	}
	r, _ := filepath.EvalSymlinks(d)
	a := codexArgs(d, nil, true, "/o", "", true, false, "t1", nil, "", nil)
	i := slices.IndexFunc(a, func(s string) bool { return strings.HasPrefix(s, "projects={") })
	if i < 1 || a[i-1] != "-c" || i > slices.Index(a, "resume") {
		t.Fatalf("codex projects: %v", a)
	}
	for _, k := range []string{d, filepath.Dir(d), r, filepath.VolumeName(d) + string(filepath.Separator)} {
		if runtime.GOOS == "windows" {
			k = strings.ToLower(k)
		}
		if q, _ := json.Marshal(k); !strings.Contains(a[i], string(q)+`={trust_level="untrusted"}`) {
			t.Fatalf("codex projects %s: %s", k, a[i])
		}
	}
}

// #59: Windows: končna pot (GetFinalPathNameByHandle) brez predpone, kot ključ Codex.
func TestTrimFinalPrefix(t *testing.T) {
	for in, want := range map[string]string{`\\?\C:\a\b`: `C:\a\b`, `\\?\UNC\srv\share\x`: `\\srv\share\x`, `C:\a`: `C:\a`} {
		if got := trimFinalPrefix(in); got != want {
			t.Fatalf("trimFinalPrefix(%q) = %q, want %q", in, got, want)
		}
	}
}

// Kot testi Codex.args in Grok.args na Macu.
func TestCodexAndGrokArgs(t *testing.T) {
	a := codexArgs("/w", []string{"/tmp/x"}, true, "/o.txt", "", true, true, "", nil, "", nil)
	j := strings.Join(a, " ")
	if !strings.HasPrefix(j, "exec --json --skip-git-repo-check --color never -C /w -o /o.txt --sandbox workspace-write --add-dir /tmp/x") ||
		!strings.HasSuffix(j, `-c sandbox_mode="workspace-write" -c sandbox_workspace_write.network_access=true -c sandbox_workspace_write.writable_roots=["/tmp/x"] -`) ||
		slices.Contains(a, "--ephemeral") {
		t.Fatalf("codex agent: %v", a)
	}
	if r := codexArgs("/w", nil, false, "/o", "", true, true, "", nil, "", nil); !slices.Contains(r, "read-only") || !slices.Contains(r, "--ephemeral") {
		t.Fatalf("codex chat: %v", r)
	}
	if r := codexArgs("/w", []string{"/tmp/x"}, true, "/o", "gpt-6-luna", false, true, "", nil, "", nil); !slices.Contains(r, "read-only") || slices.Contains(r, "--add-dir") ||
		!slices.Contains(r, "gpt-6-luna") || strings.Contains(strings.Join(r, " "), "network_access") {
		t.Fatalf("codex read-only: %v", r)
	}
	// I5: sandbox tudi za »exec resume« (ta nima --sandbox): -c za »resume <nit>«.
	if r := strings.Join(codexArgs("/w", nil, true, "/o", "", true, false, "t1", nil, "", nil), " "); !strings.Contains(r, `resume t1 -c sandbox_mode="workspace-write" -c sandbox_workspace_write.network_access=false`) ||
		!strings.HasSuffix(r, " -") {
		t.Fatalf("codex resume: %s", r)
	}
	if r := strings.Join(codexArgs("/w", nil, true, "/o", "", false, false, "t1", nil, "", nil), " "); !strings.HasSuffix(r, `resume t1 -c sandbox_mode="read-only" -`) {
		t.Fatalf("codex read-only resume: %s", r)
	}
	// H1: mapa nastavitev prepovedana (Read, Edit, Write); brez pisanja tudi brez spleta.
	deny := "--deny Read(/cfg/**) --deny Edit(/cfg/**) --deny Write(/cfg/**)"
	if g := strings.Join(grokArgs("/w", "/p", "/cfg", true, "grok-4.7", true, ""), " "); g != "--cwd /w --output-format streaming-json --prompt-file /p -m grok-4.7 "+deny+" --sandbox workspace --always-approve" {
		t.Fatalf("grok write: %s", g)
	}
	if g := strings.Join(grokArgs("/w", "/p", "/cfg", true, "", false, "s1"), " "); !strings.Contains(g, deny+" --sandbox read-only --disable-web-search") ||
		strings.Contains(g, "--always-approve") || !strings.HasSuffix(g, "-r s1") {
		t.Fatalf("grok read-only: %s", g)
	}
	if g := grokArgs("/w", "/p", "/cfg", false, "", true, ""); !slices.Contains(g, "read-only") || !slices.Contains(g, "--disable-web-search") {
		t.Fatalf("grok chat: %v", g)
	}
	var c agentCLI
	if json.Unmarshal([]byte(`{"enabled":true,"noNetwork":false}`), &c); c.Network {
		t.Fatal("network must be opt-in")
	}

	for _, tc := range []struct {
		k     agentKind
		in    string
		model string
		ok    bool
	}{{codexKind, "claude-codex", "", true}, {codexKind, "claude-codex-gpt-6-luna", "gpt-6-luna", true}, {codexKind, "claude-codex-", "", false},
		{codexKind, "claude-opus-5-5", "", false}, {grokKind, "claude-grok-grok-4.7", "grok-4.7", true}, {grokKind, "claude-codex", "", false}} {
		if m, ok := tc.k.match(tc.in); m != tc.model || ok != tc.ok {
			t.Errorf("%s match %q: %q %v", tc.k.name, tc.in, m, ok)
		}
	}
	if validAgentModel.MatchString("-rf") || validAgentModel.MatchString("a b") || !validAgentModel.MatchString("openai/gpt-5:latest") {
		t.Fatal("validAgentModel")
	}

	if s := codexStep([]byte(`{"type":"item.started","item":{"type":"command_execution","command":"/bin/zsh -lc 'cat a.txt'"}}`)); s != "$ cat a.txt" {
		t.Fatalf("codex step: %q", s)
	}
	if s := codexStep([]byte(`{"type":"item.completed","item":{"type":"agent_message","text":"Reading docs\nmore"}}`)); s != "Reading docs" {
		t.Fatalf("codex message step: %q", s)
	}
	if codexStep([]byte(`{"type":"turn.started"}`)) != "" || codexThread([]byte(`{"type":"thread.started","thread_id":"t1"}`)) != "t1" {
		t.Fatal("codex thread")
	}
	if e, _ := parseGrokEvent([]byte(`{"type":"tool_call","title":"read_file","rawInput":{"target_file":"a.txt"}}`)); e.step != "read_file a.txt" {
		t.Fatalf("grok step: %+v", e)
	}
	if e, _ := parseGrokEvent([]byte(`{"type":"end","stopReason":"end_turn","sessionId":"s1"}`)); e.session != "s1" {
		t.Fatalf("grok end: %+v", e)
	}

	cx := parseCodexCatalog([]byte(`{"models":[{"slug":"gpt-6.1-sol","display_name":"GPT-6.1-Sol","context_window":400000},{"slug":"hidden","visibility":"hide"}]}`))
	if len(cx) != 1 || cx[0].Name != "GPT-6.1-Sol" || cx[0].Window != 400000 {
		t.Fatalf("codex catalog: %+v", cx)
	}
	gx := parseGrokCatalog("You are logged in.\n\nDefault model: grok-4.7\n\nAvailable models:\n  * grok-4.7 (default)\n  grok-code-fast\n")
	if len(gx) != 2 || gx[0].Slug != "grok-4.7" || gx[1].Slug != "grok-code-fast" {
		t.Fatalf("grok catalog: %+v", gx)
	}
}

// Prek routerja z lažnima CLI: seznam modelov, pogovor in agent le v sinhronizirani mapi (M1), privzeto brez omrežja.
func TestCodexAndGrokThroughRouter(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake CLIs are shell scripts")
	}
	bin := t.TempDir()
	argsFile := filepath.Join(bin, "args")
	codex := filepath.Join(bin, "codex")
	os.WriteFile(codex, []byte(`#!/bin/sh
case "$1 $2" in "-c projects="*) [ "$3 $4" = "debug models" ] && echo '{"models":[{"slug":"gpt-x","display_name":"GPT X","context_window":128000}]}'; exit 0;; esac # #59: -c projects=… pred »debug models«
printf '%s\n' "$@" "home=$CODEX_HOME" > "`+argsFile+`"
out=""; prev=""
for a in "$@"; do [ "$prev" = "-o" ] && out="$a"; prev="$a"; done
cat > /dev/null
echo '{"type":"thread.started","thread_id":"t9"}'
echo "codex says hi" > "$out"
`), 0o755)
	grok := filepath.Join(bin, "grok")
	os.WriteFile(grok, []byte(`#!/bin/sh
if [ "$1" = models ]; then printf 'Available models:\n  * grok-4.7 (default)\n'; exit 0; fi
printf '%s\n' "$@" > "`+argsFile+`"
echo '{"type":"text","data":"grok "}'
echo '{"type":"text","data":"says hi"}'
echo '{"type":"end","sessionId":"s9"}'
`), 0o755)
	s, _, base := liveServer(t, func(c *serveConfig) {
		c.AllowUnsandboxedBash = true // Linux brez bwrap (Docker): sicer Codex in Grok ne tečeta
		t.Setenv("CODEX_HOME", t.TempDir())
		t.Setenv("GROK_HOME", t.TempDir())
		c.Codex = agentCLI{Enabled: true, Executable: codex}
		c.Grok = agentCLI{Enabled: true, Executable: grok, Models: []string{}}
	})
	host := strings.TrimPrefix(base, "http://")
	_, models := adminReq(t, base, host, "tok", "GET", "/v1/models", "")
	if !strings.Contains(models, `"claude-codex"`) || !strings.Contains(models, `"claude-codex-gpt-x"`) || !strings.Contains(models, "gpt-x · 128K") ||
		!strings.Contains(models, `"claude-grok"`) || strings.Contains(models, "claude-grok-grok-4.7") {
		t.Fatalf("models: %s", models)
	}
	// M1: brez sinhronizirane mape ne teče noben (tudi pogovor brez orodij ne).
	if st, b := adminReq(t, base, host, "tok", "POST", "/v1/messages", `{"model":"claude-codex","messages":[{"role":"user","content":"hi"}]}`); st != 400 ||
		!strings.Contains(b, "needs a synced folder") {
		t.Fatalf("codex without a folder: %d %s", st, b)
	}

	c := startLoopback(t, s)
	dir := t.TempDir()
	sc := &shareClient{c: c, session: "sidc", root: dir, sync: true, synced: map[string]syncEntry{}, reported: map[string]bool{}}
	if !sc.register() {
		t.Fatal("register failed: " + sc.refusal)
	}
	sc.start(2)
	defer sc.unregister()
	h := map[string]string{"x-claude-mini-session": "sidc", "x-claude-mini-cwd": dir, "content-type": "application/json"}
	chat := func(model string, stream, tools bool) (int, string) {
		b := `{"model":"` + model + `","stream":` + map[bool]string{true: "true", false: "false"}[stream] +
			map[bool]string{true: `,"tools":[{"name":"x"}]`, false: ""}[tools] + `,"messages":[{"role":"user","content":"hi"}]}`
		st, data := call(c, "POST", "/v1/messages", h, []byte(b), 30*time.Second)
		return st, string(data)
	}
	args := func() string { b, _ := os.ReadFile(argsFile); return string(b) }
	if st, b := chat("claude-codex-gpt-x", false, false); st != 200 || !strings.Contains(b, "codex says hi") || !strings.Contains(args(), "read-only") ||
		!strings.Contains(args(), "home="+s.conf().Scratch+"/state/") {
		t.Fatalf("codex chat: %d %s %q", st, b, args())
	}
	if st, b := chat("claude-codex", false, true); st != 200 || !strings.Contains(args(), "workspace-write") || !strings.Contains(args(), "network_access=false") {
		t.Fatalf("codex agent: %d %s %q", st, b, args())
	}
	h["x-claude-mini-ro"] = "1" // --minigw-ro brez mape le za branje: Codex le bere
	if st, b := chat("claude-codex", false, true); st != 200 || !strings.Contains(args(), "read-only") || strings.Contains(args(), "workspace-write") {
		t.Fatalf("codex read-only header: %d %s %q", st, b, args())
	}
	delete(h, "x-claude-mini-ro")
	if st, b := chat("claude-grok", true, true); st != 200 || !strings.Contains(b, `"text":"grok says hi"`) || !strings.Contains(b, "message_stop") ||
		!strings.Contains(args(), "Read("+filepath.ToSlash(s.cfgDir)+"/**)") {
		t.Fatalf("grok stream: %d %s %q", st, b, args())
	}
	if st, _ := chat("claude-grok-grok-4.7", false, false); st != 404 { // izklopljen model
		t.Fatalf("disabled grok model: %d", st)
	}
	// Skrbniški API: oblika kot na Macu; izklop skrije modele.
	if st, b := adminNoise(t, s, base, s.admin, "GET", adminPrefix+"/agents/codex", ""); st != 200 || !strings.Contains(b, `"network":false`) {
		t.Fatalf("codex network default: %d %s", st, b)
	}
	if st, b := adminNoise(t, s, base, s.admin, "PATCH", adminPrefix+"/agents/codex", `{"write":false,"network":true,"models":["gpt-x"]}`); st != 200 ||
		!strings.Contains(b, `"write":false`) || !strings.Contains(b, `"network":true`) || !strings.Contains(b, `"models":["gpt-x"]`) || !s.conf().Codex.Network {
		t.Fatalf("codex API: %d %s", st, b)
	}
	if st, _ := adminNoise(t, s, base, s.admin, "PATCH", adminPrefix+"/agents/grok", `{"network":true}`); st != 400 {
		t.Fatal("grok network accepted")
	}
	if st, b := adminNoise(t, s, base, s.admin, "GET", adminPrefix+"/agents/codex/models", ""); st != 200 || !strings.Contains(b, `"slug":"gpt-x","name":"GPT X"`) {
		t.Fatalf("codex models: %d %s", st, b)
	}
	adminNoise(t, s, base, s.admin, "PATCH", adminPrefix+"/agents/codex", `{"enabled":false}`)
	if _, m := adminReq(t, base, host, "tok", "GET", "/v1/models", ""); strings.Contains(m, "claude-codex") {
		t.Fatalf("disabled codex listed: %s", m)
	}
}

// H1, R3: brez sandboxa, ki skrije skrivnosti (Windows; Linux brez bwrap), Codex in Grok le z izrecnim dovoljenjem.
func TestAgentsWithoutSandboxNeedOptIn(t *testing.T) {
	s := &server{cfgDir: t.TempDir()}
	sandboxed := agentSandboxed(s.cfgDir)
	if runtime.GOOS != "linux" && sandboxed { // F4: le Linux z bwrap
		t.Fatalf("%s sandboxed=%v", runtime.GOOS, sandboxed)
	}
	if s.agentsAllowed(serveConfig{}, "grok") != sandboxed || !s.agentsAllowed(serveConfig{AllowUnsandboxedBash: true}, "grok") {
		t.Fatal("opt-in")
	}
	// HPX360 (0.5.24): Copilot se v škatli MXC ne zažene (canonicalize runtime.node): na Windows kot Codex le s stikalom
	if !outsideBox("copilot") || !outsideBox("codex") || outsideBox("grok") || outsideBox("claude") {
		t.Fatal("outsideBox")
	}
	if runtime.GOOS == "windows" && (s.agentsAllowed(serveConfig{}, "copilot") || !s.agentsAllowed(serveConfig{AllowUnsandboxedBash: true}, "copilot")) {
		t.Fatal("copilot on Windows: only with the switch")
	}
}

// L3: prehod ne zakrije imen Codex in Grok.
func TestGatewayDoesNotShadowAgents(t *testing.T) {
	for _, id := range []string{"claude-codex", "claude-codex-gpt-x", "claude-grok", "claude-grok-4", "claude-cli-opus"} {
		if !shadowsCLI(id) {
			t.Errorf("%s not shadowed", id)
		}
	}
	if shadowsCLI("claude-codexy") || shadowsCLI("qwen") {
		t.Error("too broad")
	}
}

// N3: vsaka naprava ima svoje stanje CLI; prijava je povezava na pravo datoteko in se po zamenjavi vrne vanjo.
func TestAgentStatePerDevice(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no per-device state on Windows")
	}
	real, scratch := t.TempDir(), t.TempDir()
	t.Setenv("CODEX_HOME", real)
	os.WriteFile(real+"/auth.json", []byte("OLD"), 0o600)
	a, files, err := agentState(scratch, codexKind, "deviceA")
	b, _, _ := agentState(scratch, codexKind, "deviceB")
	if err != nil || a == b || !strings.HasPrefix(a, scratch+"/state/") || len(files) != 1 || files[0] != real+"/auth.json" {
		t.Fatalf("state: %s %s %v %v", a, b, files, err)
	}
	if l, _ := os.Readlink(a + "/auth.json"); l != real+"/auth.json" {
		t.Fatalf("link: %q", l)
	}
	os.Remove(a + "/auth.json") // CLI je zapisal novo datoteko namesto povezave
	os.WriteFile(a+"/auth.json", []byte("NEW"), 0o600)
	restoreAuth(a, codexKind)
	if got, _ := os.ReadFile(real + "/auth.json"); string(got) != "NEW" {
		t.Fatalf("not restored: %q", got)
	}
	if l, _ := os.Readlink(a + "/auth.json"); l != real+"/auth.json" {
		t.Fatal("link not back")
	}
	env := strings.Join(agentEnv([]string{"SSH_AUTH_SOCK=/s", "GPG_AGENT_INFO=x", "GNOME_KEYRING_CONTROL=y", "DBUS_SESSION_BUS_ADDRESS=z",
		"CODEX_HOME=/h", "ANTHROPIC_API_KEY=k", "PATH=/bin"}), " ")
	if env != "PATH=/bin" {
		t.Fatalf("env: %s", env)
	}
}

// M1: Copilot nikoli ne dobi ~/.copilot (hooki, MCP, zaupane mape) ali stikal, ki naložijo hooke in MCP repozitorija:
// stanje naprave (svež COPILOT_HOME vsak tek) na vseh sistemih, tudi na Windows zunaj škatle; GITHUB_COPILOT_* ne pride do njega.
func TestCopilotNoUserOrRepoHooks(t *testing.T) {
	scratch := t.TempDir()
	st, files, err := agentState(scratch, copilotKind, "dev")
	if err != nil || st == "" || !under(st, scratch) || len(files) != 0 {
		t.Fatalf("copilot state: %q %v %v", st, files, err)
	}
	for _, kv := range agentEnv([]string{"GITHUB_COPILOT_PROMPT_MODE_REPO_HOOKS=true", "github_copilot_prompt_mode_workspace_mcp=true",
		"GITHUB_COPILOT_PROMPT_MODE_EXTENSIONS=true", "COPILOT_ALLOW_ALL=true", "COPILOT_HOME=/real", "KEEP=1"}) {
		if kv != "KEEP=1" {
			t.Errorf("reached Copilot: %s", kv)
		}
	}
}

func TestCodexUsageLimitIs429(t *testing.T) {
	msg := codexFailure([]byte(`{"type":"turn.failed","error":{"message":"You’ve hit your usage limit. Try again at 7:52 PM. /Users/x/secret"}}`))
	if st, m := classifyAgentError(msg); st != 429 || strings.Contains(m, "/Users") || !strings.Contains(m, "usage limit") {
		t.Fatalf("got %d %q", st, m)
	}
	if codexFailure([]byte(`{"type":"error","message":"boom"}`)) != "boom" {
		t.Fatal("error event")
	}
	if st, _ := classifyAgentError("codex exited with 1"); st != 502 {
		t.Fatal("plain failure must stay 502")
	}
}

func TestAgentLimitKeywordsAndCleaning(t *testing.T) {
	for _, m := range []string{"You've hit your 5-hour limit · resets 9pm", "You're out of usage credits", "Your org is out of usage", "monthly spend limit", "You've reached your cap"} {
		if st, _ := classifyAgentError(m); st != 429 {
			t.Fatalf("%q: %d", m, st)
		}
	}
	_, m := classifyAgentError(`failed C:/x/y ./rel file:///etc/passwd https://a.b/c request req_abc123`)
	if m != "failed https://a.b/c request req_abc123" {
		t.Fatalf("%q", m)
	}
	w := httptest.NewRecorder()
	writeAgentError(w, "req_1", 429, "x")
	if w.Header().Get("x-should-retry") != "false" {
		t.Fatal("agent limit needs x-should-retry: false")
	}
	b := httptest.NewRecorder()
	writeError(b, "req_2", 429, "Too many provider requests at once")
	if b.Header().Get("x-should-retry") != "" {
		t.Fatal("busy 429 must stay retryable")
	}
}

// Napaka CLI za odjemalca: le besedilo (brez »Internal error:«, ovoja JSON in »API error (status 402 …):«), kot na Macu.
func TestAgentMessageExtraction(t *testing.T) {
	line := `{"type":"error","message":"Internal error: {\n  \"message\": \"API error (status 402 Payment Required): personal-team-blocked:spending-limit: You have run out of credits or need a Grok subscription. Add credits at https://grok.com/?_s=usage or upgrade at https://grok.com/supergrok.\",\n  \"http_status\": 402\n}","cwd":"/Users/x/w"}`
	e, _ := parseGrokEvent([]byte(line))
	st, m := classifyAgentError(e.err)
	if want := "You have run out of credits or need a Grok subscription. Add credits at https://grok.com/?_s=usage or upgrade at https://grok.com/supergrok."; st != 429 || m != want {
		t.Fatalf("grok: %d %q", st, m)
	}
	codex := codexFailure([]byte(`{"type":"error","message":"{\"error\":{\"type\":\"usage_limit_reached\",\"message\":\"You've hit your usage limit.\"}}"}`))
	if st, m := classifyAgentError(codex); st != 429 || m != "You've hit your usage limit." {
		t.Fatalf("codex: %d %q", st, m)
	}
	if _, m := classifyAgentError(`{"error":"quota exceeded"}`); m != "quota exceeded" {
		t.Fatalf("error string: %q", m)
	}
	if _, m := classifyAgentError("Internal error: {not json"); m != "Internal error: {not json" {
		t.Fatalf("fallback: %q", m)
	}
	// L4: ubežni znaki JSON ne postanejo pravi kontrolni znaki
	if m := agentMessage(`{"error":{"message":"a\u001b]52;c;eA==\u0007b\r\u0085\u202ec\u009b\u007f"}}`); m != "a]52;c;eA==bc" {
		t.Fatalf("control chars: %q", m)
	}
}

// Napaka pred glavo (tudi pri pretoku) je pravi 429 z x-should-retry: false (Claude Code ne ponovi); po glavi dogodek SSE,
// ponovitev brez pretoka dobi isti 429 brez novega zagona CLI.
func TestReplyWhenDoneRetryHeaders(t *testing.T) {
	defer func(d time.Duration) { agentHeadDelay = d }(agentHeadDelay)
	agentHeadDelay = 50 * time.Millisecond
	s := &server{log: log.New(io.Discard, "", 0)}
	newReq := func() *req {
		hr := httptest.NewRequest("POST", "/v1/messages", nil)
		hr.Header.Set("x-claude-mini-session", "s1")
		return &req{Request: hr, rid: "req_1", conn: &serveConn{peer: "127.0.0.1"}}
	}
	limit := func() (string, error) {
		return "", errors.New(`Internal error: {"message":"API error (status 402 Payment Required): You have run out of credits."}`)
	}
	for _, stream := range []bool{true, false} {
		w := httptest.NewRecorder()
		st := s.replyWhenDone(w, newReq(), map[string]any{"model": "claude-grok", "stream": stream}, limit)
		if st != 429 || w.Code != 429 || w.Header().Get("x-should-retry") != "false" || !strings.Contains(w.Body.String(), `"You have run out of credits."`) {
			t.Fatalf("stream=%v: %d %d %q %s", stream, st, w.Code, w.Header().Get("x-should-retry"), w.Body.String())
		}
	}
	if _, ok := s.recentLimit(newReq(), "claude-grok"); ok {
		t.Fatal("an HTTP 429 is final: nothing to remember")
	}
	// HPX360: agent, ki je padel (502), Claude Code ne zažene znova
	w0 := httptest.NewRecorder()
	if st := s.replyWhenDone(w0, newReq(), map[string]any{"model": "claude-copilot"}, func() (string, error) { return "", errors.New("copilot.exe failed (exit status 1)") }); st != 502 || w0.Header().Get("x-should-retry") != "false" {
		t.Fatalf("failed run: %d %q", st, w0.Header().Get("x-should-retry"))
	}
	w := httptest.NewRecorder()
	slow := func() (string, error) { time.Sleep(150 * time.Millisecond); return limit() }
	s.replyWhenDone(w, newReq(), map[string]any{"model": "claude-grok", "stream": true}, slow)
	if w.Code != 200 || w.Header().Get("x-should-retry") != "false" || !strings.Contains(w.Body.String(), "event: error") ||
		!strings.Contains(w.Body.String(), "rate_limit_error") {
		t.Fatalf("late error: %d %s", w.Code, w.Body.String())
	}
	if m, ok := s.recentLimit(newReq(), "claude-grok"); !ok || m != "You have run out of credits." {
		t.Fatalf("fallback after a stream error: %q %v", m, ok)
	}
	if _, ok := s.recentLimit(newReq(), "claude-grok"); ok {
		t.Fatal("remembered once only")
	}
	w = httptest.NewRecorder()
	if st := s.replyWhenDone(w, newReq(), map[string]any{"model": "claude-grok", "stream": true}, func() (string, error) { return "hi", nil }); st != 200 ||
		!strings.Contains(w.Body.String(), "message_start") || !strings.Contains(w.Body.String(), "hi") {
		t.Fatalf("answer: %d %s", st, w.Body.String())
	}
}

// F2: Codex brez pisanja v načinu worktree ni zavrnjen (ni 400): v klonu le bere, navodilo pravi samo za branje, veje ni.
func TestServeWorktreeReadOnlyAgent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake codex is a shell script")
	}
	root := t.TempDir()
	client := filepath.Join(root, "client")
	os.MkdirAll(client, 0o755)
	gitT(t, client, "init", "-q", "-b", "main")
	writeT(t, filepath.Join(client, "a.txt"), "a\n")
	gitT(t, client, "add", "-A")
	gitT(t, client, "commit", "-qm", "first")
	codex := filepath.Join(root, "codex")
	writeT(t, codex, `#!/bin/sh
if [ "$3" = debug ]; then echo '{"models":[]}'; exit 0; fi
out=""; prev=""
for a in "$@"; do [ "$prev" = "-o" ] && out="$a"; prev="$a"; done
p=$(cat)
echo '{"type":"thread.started","thread_id":"t1"}'
case "$p" in *"read-only for you"*) echo ro-note > "$out";; *) echo no-note > "$out";; esac
`)
	os.Chmod(codex, 0o755)
	s, _ := testServer(t, func(c *serveConfig) {
		c.AllowUnsandboxedBash = runtime.GOOS != "linux" || bwrapReady(t.TempDir()) == ""
		t.Setenv("CODEX_HOME", t.TempDir())
		c.Codex = agentCLI{Enabled: true, Executable: codex, ReadOnly: true}
	})
	c := startLoopback(t, s)
	sc := &shareClient{c: c, session: "sidr", root: client, worktree: true, top: filepath.ToSlash(client), synced: map[string]syncEntry{}, reported: map[string]bool{}}
	if !sc.register() {
		t.Fatal("register failed: " + sc.refusal)
	}
	sc.start(2)
	defer sc.unregister()
	h := map[string]string{"x-claude-mini-session": "sidr", "x-claude-mini-cwd": client, "content-type": "application/json"}
	body := `{"model":"claude-codex","tools":[{"name":"x"}],"messages":[{"role":"user","content":"look"}]}`
	h["x-claude-mini-ro"] = "1" // I4: --minigw-ro v worktree: 400 brez WORKTREE-PUSH
	if st, data := call(c, "POST", "/v1/messages", h, []byte(body), 60*time.Second); st != 400 || !strings.Contains(string(data), "cannot be read-only") {
		t.Fatalf("--minigw-ro in worktree: %d %s", st, data)
	}
	if _, _, _, _, ok := s.hub.worktreeOf("sidr", "codex"); ok {
		t.Fatal("--minigw-ro in worktree: cloned before the 400")
	}
	delete(h, "x-claude-mini-ro")
	if st, data := call(c, "POST", "/v1/messages", h, []byte(body), 60*time.Second); st != 200 || !strings.Contains(string(data), "ro-note") {
		t.Fatalf("read-only codex in worktree: %d %s", st, data)
	}
	if _, err := gitUser(client, nil, "rev-parse", "--verify", "-q", "refs/heads/minigw/codex-"+sha256Hex([]byte("sidr"))[:8]+"-0"); err == nil {
		t.Fatal("a read-only agent brought back a branch")
	} // #46a: zgodovina zapiše le branje, ki ga je izvajanje uporabilo (deljenje samo ni samo za branje)
	if x := s.state.sessions("", ""); len(x) != 1 || len(x[0].Models) != 1 || !x[0].Models[0].ReadOnly || x[0].Models[0].Tools != "" {
		t.Fatalf("history: %+v", x)
	}
}

// #45: način seje nad omejitvijo agenta (work) → agent le bere, opomba gre odjemalcu; worktree in "" ostaneta.
func TestWorkCap(t *testing.T) {
	s, _, base := liveServer(t, func(c *serveConfig) { c.Codex.Work = "worktree"; c.ClaudeWork = "sync" })
	if _, ok := s.hub.register("s", "/p", "k", false, filepath.Join(t.TempDir(), "c"), "local"); !ok {
		t.Fatal("register")
	}
	if _, ok := s.hub.register("w", "/p", "k2", false, "", "local"); !ok {
		t.Fatal("register")
	}
	s.hub.mu.Lock()
	s.hub.bySession["w"].tree = &shareTree{slots: map[string]*wtSlot{}}
	s.hub.mu.Unlock()
	r := func(session string) *req {
		hr := httptest.NewRequest("POST", "/v1/messages", nil)
		hr.Header.Set("x-claude-mini-session", session)
		return &req{Request: hr, conn: &serveConn{peer: "127.0.0.1"}}
	}
	if got := s.capped(r("s"), "codex"); got != "worktree" {
		t.Fatalf("sync session, codex work=worktree: %q", got)
	}
	select {
	case j := <-s.hub.bySession["s"].jobs:
		if j.Method != "WORKTREE-NOTE" || !strings.Contains(string(j.Body), "lets Codex change files only in worktree mode") ||
			!strings.Contains(string(j.Body), "--minigw-work=worktree") {
			t.Fatalf("note: %s %s", j.Method, j.Body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no note queued")
	}
	if got := s.capped(r("s"), "codex"); got != "worktree" { // L4: opomba le enkrat na sejo in agenta
		t.Fatalf("second turn: %q", got)
	}
	select {
	case j := <-s.hub.bySession["s"].jobs:
		t.Fatalf("second note: %s", j.Body)
	case <-time.After(300 * time.Millisecond):
	}
	for _, c := range []struct{ session, kind string }{{"w", "codex"}, {"s", "claude"}, {"s", "grok"}, {"w", "claude"}, {"chat", "codex"}} {
		if got := s.capped(r(c.session), c.kind); got != "" {
			t.Errorf("%s %s: capped %q", c.session, c.kind, got)
		}
	}
	if workLevels["sync"] <= workLevels[""] || modeLevels["native-direct"] != workLevels["direct"] {
		t.Error("levels")
	}
	for _, p := range []string{"/agents/codex", "/agents/claude", "/agents/copilot", "/agents/grok"} {
		if st, b := adminNoise(t, s, base, s.admin, "PATCH", adminPrefix+p, `{"work":"readonly"}`); st != 400 || !strings.Contains(b, "work: direct, sync or worktree") {
			t.Fatalf("%s bad work: %d %s", p, st, b)
		}
		if st, b := adminNoise(t, s, base, s.admin, "PATCH", adminPrefix+p, `{"work":"sync"}`); st != 200 || !strings.Contains(b, `"work":"sync"`) {
			t.Fatalf("%s work: %d %s", p, st, b)
		}
		if st, b := adminNoise(t, s, base, s.admin, "PATCH", adminPrefix+p, `{"work":"direct"}`); st != 200 || !strings.Contains(b, `"work":"direct"`) {
			t.Fatalf("%s work direct: %d %s", p, st, b)
		}
	}
	if c := s.conf(); c.ClaudeWork != "" || c.Codex.Work != "" { // direct = "" (izpuščeno v serve.json)
		t.Fatalf("direct stored as %q %q", c.ClaudeWork, c.Codex.Work)
	}
}

// #45, #46a: Codex z work=worktree v seji sync le bere (sandbox read-only, navodilo), zgodovina zapiše omejitev.
func TestWorkCapRun(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake codex is a shell script")
	}
	bin := t.TempDir()
	argsFile := filepath.Join(bin, "args")
	codex := filepath.Join(bin, "codex")
	os.WriteFile(codex, []byte(`#!/bin/sh
if [ "$3" = debug ]; then echo '{"models":[]}'; exit 0; fi
printf '%s\n' "$@" > "`+argsFile+`"
out=""; prev=""
cwd=""
for a in "$@"; do [ "$prev" = "-o" ] && out="$a"; [ "$prev" = "-C" ] && cwd="$a"; prev="$a"; done
echo agent > "$cwd/agent.txt" # sandbox brez učinka: L1 preveri, da se ne vrne odjemalcu
p=$(cat)
echo '{"type":"thread.started","thread_id":"t1"}'
case "$p" in *"shared read-only"*) echo ro-note > "$out";; *) echo no-note > "$out";; esac
`), 0o755)
	s, _ := testServer(t, func(c *serveConfig) {
		c.AllowUnsandboxedBash = true
		t.Setenv("CODEX_HOME", t.TempDir())
		c.Codex = agentCLI{Enabled: true, Executable: codex, Work: "worktree"}
	})
	c := startLoopback(t, s)
	dir := t.TempDir()
	sc := &shareClient{c: c, session: "sidw", root: dir, sync: true, synced: map[string]syncEntry{}, reported: map[string]bool{}}
	if !sc.register() {
		t.Fatal("register failed: " + sc.refusal)
	}
	if !slices.Equal(sc.capped, []string{"codex"}) { // #60: začetna vrstica odjemalca
		t.Fatalf("capped in the share reply: %q", sc.capped)
	}
	sc.start(2)
	defer sc.unregister()
	h := map[string]string{"x-claude-mini-session": "sidw", "x-claude-mini-cwd": dir, "content-type": "application/json"}
	body := `{"model":"claude-codex","tools":[{"name":"x"}],"messages":[{"role":"user","content":"change"}]}`
	st, data := call(c, "POST", "/v1/messages", h, []byte(body), 30*time.Second)
	if args, _ := os.ReadFile(argsFile); st != 200 || !strings.Contains(string(data), "ro-note") || !strings.Contains(string(args), "read-only") ||
		strings.Contains(string(args), "workspace-write") {
		t.Fatalf("capped codex: %d %s %q", st, data, args)
	}
	if _, err := os.Stat(filepath.Join(dir, "agent.txt")); err == nil { // L1: omejen tek ne prenese sprememb nazaj
		t.Fatal("a capped run synced changes back")
	}
	x := s.state.sessions("", "")
	if len(x) != 1 || !x[0].ReadOnly || len(x[0].Models) != 1 || x[0].Models[0].Cap != "worktree" || !x[0].Models[0].ReadOnly || x[0].Models[0].Tools != "" {
		t.Fatalf("history: %+v", x)
	}
	if l := sessionLine(x[0]); !strings.Contains(l, "sync, read-only (cap worktree)") {
		t.Fatalf("sessionLine: %q", l)
	}
	if l := useLine(x[0].Models[0]); !strings.Contains(l, `"claude-codex" read-only (cap worktree):`) {
		t.Fatalf("useLine: %q", l)
	}
	s.mu.Lock()
	s.cfg.Codex.Work = "" // direct: piše kot doslej
	s.mu.Unlock()
	if st, _ := call(c, "POST", "/v1/messages", h, []byte(body), 30*time.Second); st != 200 {
		t.Fatal(st)
	}
	if args, _ := os.ReadFile(argsFile); !strings.Contains(string(args), "workspace-write") {
		t.Fatalf("direct cap: %q", args)
	}
	deadline := time.Now().Add(10 * time.Second)
	for _, err := os.Stat(filepath.Join(dir, "agent.txt")); err != nil && time.Now().Before(deadline); _, err = os.Stat(filepath.Join(dir, "agent.txt")) {
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := os.Stat(filepath.Join(dir, "agent.txt")); err != nil {
		t.Fatal("an uncapped run did not sync back (the test would prove nothing)")
	}
}

// #40: Codex v klonu dobi profil dovoljenj: commit v vrhnjem in ugnezdenem klonu gre, config, hooks in info .git ne;
// ukazi brez GIT_DIR vrhnjega klona in z ovojem na PATH. S pravim codex (če je nameščen; macOS Seatbelt) še zares.
func TestCodexTreeProfile(t *testing.T) {
	wt := filepath.Join(t.TempDir(), "w")
	gits := []string{filepath.Join(wt, ".git"), filepath.Join(wt, "lib", ".git")}
	a := strings.Join(codexArgs(wt, []string{"/m"}, true, "/o", "", true, false, "", gits, "/gw/bin:/usr/bin", []string{"/cfg"}), " ")
	for _, want := range []string{`default_permissions="minigw"`, `"` + gits[1] + `"="write"`, `"` + gits[1] + `/config"="read"`, `"` + gits[0] + `/hooks"="read"`,
		`"/m"="write"`, `permissions.minigw.network.enabled=false`, `shell_environment_policy.exclude=["GIT_DIR","GIT_WORK_TREE","GIT_COMMON_DIR"]`,
		`shell_environment_policy.set.PATH="/gw/bin:/usr/bin"`, `"/cfg"="none"`, `shell_environment_policy.set.GIT_CEILING_DIRECTORIES="` + filepath.Dir(wt) + `"`,
		`shell_environment_policy.set.MINIGW_SANDBOXED="1"`} { // #67
		if !strings.Contains(a, want) {
			t.Fatalf("missing %s in %s", want, a)
		}
	}
	// #67: oznake peskovnika in MINIGW_DEPS ne pridejo v okolje harnessa iz okolja prehoda
	for _, kv := range agentEnv([]string{"SANDBOX_RUNTIME=1", "MINIGW_SANDBOXED=1", "CODEX_SANDBOX=seatbelt", "MINIGW_DEPS=/x", "MINIGW_NETWORK_OFF=x", "KEEP=1"}) {
		if kv != "KEEP=1" {
			t.Fatalf("harness env keeps %s", kv)
		}
	}
	if strings.Contains(a, "--sandbox") || strings.Contains(a, "workspace-write") {
		t.Fatalf("workspace-write in a clone: %s", a)
	}
	exe, err := exec.LookPath("codex")
	if err != nil || runtime.GOOS != "darwin" {
		t.Skip("real codex sandbox: macOS with codex only")
	}
	for _, g := range gits {
		os.MkdirAll(filepath.Dir(g), 0o755)
		gitT(t, filepath.Dir(g), "init", "-q", "-b", "main")
	}
	var sb []string
	p := codexTreeProfile([]string{wt}, gits, nil, false, "/usr/bin:/bin")
	for i := 0; i+1 < len(p); i += 2 {
		if !strings.HasPrefix(p[i+1], "shell_environment_policy") {
			sb = append(sb, p[i], p[i+1])
		}
	}
	deps := depsDir(wt) // #67: v $MINIGW_DEPS sme narediti repozitorij (tudi .git/config in hooks)
	os.MkdirAll(deps, 0o700)
	p = codexTreeProfile([]string{wt, deps}, gits, nil, false, "/usr/bin:/bin")
	sb = nil
	for i := 0; i+1 < len(p); i += 2 {
		if !strings.HasPrefix(p[i+1], "shell_environment_policy") {
			sb = append(sb, p[i], p[i+1])
		}
	}
	script := `git init -q "` + deps + `/x" && echo x > "` + deps + `/x/.git/hooks/h" && echo DEPS-OK; ` +
		`git -C lib commit -q --allow-empty -m sub && git commit -q --allow-empty -m top && echo COMMITS; ` +
		`for f in .git/config lib/.git/config lib/.git/hooks/x lib/.git/info/attributes lib/.git/commondir; do (echo x >> $f) 2>/dev/null && echo WROTE-$f; done; echo end`
	c := exec.Command(exe, append(append([]string{"sandbox"}, sb...), "-P", "minigw", "--", "sh", "-c", script)...)
	c.Dir, c.Env = wt, append(os.Environ(), "GIT_CONFIG_COUNT=2", "GIT_CONFIG_KEY_0=user.name", "GIT_CONFIG_VALUE_0=c", "GIT_CONFIG_KEY_1=user.email", "GIT_CONFIG_VALUE_1=c@x")
	out, _ := c.CombinedOutput()
	if s := string(out); !strings.Contains(s, "COMMITS") || !strings.Contains(s, "DEPS-OK") || strings.Contains(s, "WROTE-") || !strings.Contains(s, "end") {
		t.Fatalf("codex sandbox: %s", s)
	}
}

// #60: začetna vrstica odjemalca po omejitvah iz odgovora prehoda (starejši prehod: brez polja → read-write).
func TestCappedMode(t *testing.T) {
	for _, c := range []struct {
		capped []string
		want   string
	}{
		{nil, "read-write"},
		{[]string{"codex", "grok"}, "read-write (read-only for Codex, Grok: capped by the gateway)"},
		{[]string{"claude", "codex", "copilot", "grok"}, "read-only (capped by the gateway)"},
		{[]string{"codex", "codex", "codex", "x\x1b[31m"}, "read-write (read-only for Codex: capped by the gateway)"}, // L1, INFO-2
	} {
		if got := cappedMode(c.capped); got != c.want {
			t.Errorf("%q: %q", c.capped, got)
		}
	}
	for _, c := range []struct {
		capped []string
		want   string
	}{
		{[]string{"evil"}, ""},
		{[]string{"grok", "codex"}, ", read-only for Grok, Codex (capped by the gateway)"},
		{[]string{"grok", "codex", "claude", "copilot"}, ", read-only (capped by the gateway)"},
	} {
		if got := cappedNative(c.capped); got != c.want {
			t.Errorf("native %q: %q", c.capped, got)
		}
	}
	if got := (serveConfig{ClaudeWork: "sync", Copilot: agentCLI{Work: "worktree"}}).cappedKinds("sync"); !slices.Equal(got, []string{"copilot"}) {
		t.Errorf("cappedKinds sync: %q", got)
	}
	if got := (serveConfig{Codex: agentCLI{Work: "worktree"}}).cappedKinds("worktree"); got != nil {
		t.Errorf("cappedKinds worktree: %q", got)
	}
}

// N1: prekinitev konča branje izhoda, tudi ko vnuk še drži stdout (prej je zanka čakala na EOF in mesto teka ostalo zasedeno).
func TestRunAgentProcessCancelGrandchild(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh")
	}
	s := &server{log: log.New(io.Discard, "", 0)}
	ctx, cancel := context.WithCancel(context.Background())
	lines := make(chan string, 1)
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		done <- s.runAgentProcess(ctx, &req{rid: "req_1"}, "sh", []string{"-c", "sleep 20 2>/dev/null & echo up; exec sleep 20"}, agentMounts{}, t.TempDir(), "",
			func(b []byte) { lines <- string(b) })
	}()
	<-lines
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the read loop did not end after cancel")
	}
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("took %v", d)
	}
}
