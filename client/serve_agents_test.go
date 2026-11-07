package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// Kot testi Codex.args in Grok.args na Macu.
func TestCodexAndGrokArgs(t *testing.T) {
	a := codexArgs("/w", []string{"/tmp/x"}, true, "/o.txt", "", true, true, "")
	j := strings.Join(a, " ")
	if !strings.HasPrefix(j, "exec --json --skip-git-repo-check --color never -C /w -o /o.txt --sandbox workspace-write --add-dir /tmp/x") ||
		!strings.HasSuffix(j, `-c sandbox_mode="workspace-write" -c sandbox_workspace_write.network_access=true -c sandbox_workspace_write.writable_roots=["/tmp/x"] -`) ||
		slices.Contains(a, "--ephemeral") {
		t.Fatalf("codex agent: %v", a)
	}
	if r := codexArgs("/w", nil, false, "/o", "", true, true, ""); !slices.Contains(r, "read-only") || !slices.Contains(r, "--ephemeral") {
		t.Fatalf("codex chat: %v", r)
	}
	if r := codexArgs("/w", []string{"/tmp/x"}, true, "/o", "gpt-6-luna", false, true, ""); !slices.Contains(r, "read-only") || slices.Contains(r, "--add-dir") ||
		!slices.Contains(r, "gpt-6-luna") || strings.Contains(strings.Join(r, " "), "network_access") {
		t.Fatalf("codex read-only: %v", r)
	}
	// I5: sandbox tudi za »exec resume« (ta nima --sandbox): -c za »resume <nit>«.
	if r := strings.Join(codexArgs("/w", nil, true, "/o", "", true, false, "t1"), " "); !strings.Contains(r, `resume t1 -c sandbox_mode="workspace-write" -c sandbox_workspace_write.network_access=false`) ||
		!strings.HasSuffix(r, " -") {
		t.Fatalf("codex resume: %s", r)
	}
	if r := strings.Join(codexArgs("/w", nil, true, "/o", "", false, false, "t1"), " "); !strings.HasSuffix(r, `resume t1 -c sandbox_mode="read-only" -`) {
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
if [ "$1" = debug ]; then echo '{"models":[{"slug":"gpt-x","display_name":"GPT X","context_window":128000}]}'; exit 0; fi
printf '%s\n' "$@" > "`+argsFile+`"
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
	if st, b := chat("claude-codex-gpt-x", false, false); st != 200 || !strings.Contains(b, "codex says hi") || !strings.Contains(args(), "read-only") {
		t.Fatalf("codex chat: %d %s %q", st, b, args())
	}
	if st, b := chat("claude-codex", false, true); st != 200 || !strings.Contains(args(), "workspace-write") || !strings.Contains(args(), "network_access=false") {
		t.Fatalf("codex agent: %d %s %q", st, b, args())
	}
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
	if (runtime.GOOS == "windows" && sandboxed) || (runtime.GOOS == "darwin" && !sandboxed) {
		t.Fatalf("%s sandboxed=%v", runtime.GOOS, sandboxed)
	}
	if s.agentsAllowed(serveConfig{}) != sandboxed || !s.agentsAllowed(serveConfig{AllowUnsandboxedBash: true}) {
		t.Fatal("opt-in")
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
