package main

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

var copilotForbidden = []string{"--allow-all-tools", "--allow-all-paths", "--allow-all-urls", "--allow-all", "--yolo", "--sandbox", "--trust-folder"}

func copilotRules(a []string) (allow, deny []string) {
	for i := 0; i+1 < len(a); i++ {
		switch a[i] {
		case "--allow-tool":
			allow = append(allow, a[i+1])
		case "--deny-tool":
			deny = append(deny, a[i+1])
		}
	}
	return
}

func TestCopilotArgs(t *testing.T) {
	ro := copilotArgs("hi", nil, true, "", false, false, false, "s1")
	allow, deny := copilotRules(ro)
	if slices.ContainsFunc(ro, func(x string) bool { return slices.Contains(copilotForbidden, x) }) {
		t.Fatalf("forbidden flag: %q", ro)
	}
	if !slices.Equal(allow, []string{"read"}) || !slices.Equal(deny, []string{"memory", "write", "shell", "url"}) {
		t.Fatalf("read-only: allow %q deny %q", allow, deny)
	}
	for _, f := range []string{"--disallow-temp-dir", "--disable-builtin-mcps", "--no-ask-user", "--no-auto-update",
		"--disable-client-telemetry", "--secret-env-vars=COPILOT_GITHUB_TOKEN,GH_TOKEN,GITHUB_TOKEN"} {
		if !slices.Contains(ro, f) {
			t.Errorf("missing %s", f)
		}
	}
	if i := slices.Index(ro, "--session-id"); i < 0 || ro[i+1] != "s1" || ro[slices.Index(ro, "-p")+1] != "hi" {
		t.Fatalf("session/prompt: %q", ro)
	}
	w := copilotArgs("x", []string{"/wt"}, true, "gpt-5.2", true, true, true, "s")
	allow, deny = copilotRules(w)
	if !slices.Equal(allow, []string{"read", "write", "shell", "url"}) || !slices.Contains(deny, "shell(gh:*)") || !slices.Contains(deny, "shell(security:*)") {
		t.Fatalf("write+commands: allow %q deny %q", allow, deny)
	}
	if allow, deny := copilotRules(copilotArgs("x", nil, true, "", false, true, false, "s")); !slices.Equal(allow, []string{"read"}) ||
		!slices.Contains(deny, "shell") {
		t.Fatalf("commands without write (or --ro): allow %q deny %q", allow, deny)
	}
	if w[slices.Index(w, "--model")+1] != "gpt-5.2" || w[slices.Index(w, "--add-dir")+1] != "/wt" {
		t.Fatalf("model/add-dir: %q", w)
	}
	chat := copilotArgs("x", nil, false, "", true, true, true, "s")
	if allow, deny = copilotRules(chat); len(allow) != 0 || !slices.Contains(deny, "read") {
		t.Fatalf("chat: allow %q deny %q", allow, deny)
	}
	big := copilotArgs(strings.Repeat("a", copilotMaxPrompt+5)+"END", nil, false, "", false, false, false, "s")
	if len([]rune(big[1])) != copilotMaxPrompt || !strings.HasSuffix(big[1], "END") {
		t.Fatal("prompt not capped at the end")
	}
	if txt, _ := copilotEvent([]byte(`{"type":"assistant.message","data":{"content":"Hi!"}}`)); txt != "Hi!" {
		t.Fatal("assistant.message")
	}
	if _, st := copilotEvent([]byte(`{"type":"tool.execution_start","data":{"toolName":"view"}}`)); st != "view" {
		t.Fatal("tool step")
	}
	env := agentEnv([]string{"COPILOT_ALLOW_ALL=true", "COPILOT_ALLOW_ALL_TOOLS=1", "COPILOT_ASSISTED_APPROVAL=1", "COPILOT_HOME=/x", "PATH=/bin"})
	if !slices.Equal(env, []string{"PATH=/bin"}) {
		t.Fatalf("env: %q", env)
	}
	c := agentCLI{Models: []string{"gpt-5.2"}}
	if !c.allowsModel(copilotKind, "gpt-5.2") || c.allowsModel(copilotKind, "other") || (agentCLI{}).allowsModel(copilotKind, "x") ||
		(agentCLI{}).canWrite(copilotKind) || !(agentCLI{}).canWrite(codexKind) {
		t.Fatal("models/write defaults")
	}
}

// Žeton GitHub CLI za copilot pod bwrap: lažni gh, nikoli pravi; žeton iz okolja ima prednost, neveljaven izhod ne velja.
func TestCopilotToken(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell script")
	}
	for _, v := range copilotTokenVars {
		t.Setenv(v, "")
	}
	gh := filepath.Join(t.TempDir(), "gh")
	out := "gho_Test123"
	write := func() {
		os.WriteFile(gh, []byte("#!/bin/sh\n[ \"$*\" = 'auth token --hostname github.com' ] && echo '"+out+"'\n"), 0o755)
	}
	write()
	old := copilotGH
	copilotGH = func() string { return gh }
	defer func() { copilotGH = old }()
	if got := copilotToken(t.Context()); got != "gho_Test123" {
		t.Fatalf("token %q", got)
	}
	out = "gho_x; rm -rf"
	write()
	if got := copilotToken(t.Context()); got != "" {
		t.Fatalf("invalid output accepted: %q", got)
	}
	t.Setenv("GH_TOKEN", "x")
	if got := copilotToken(t.Context()); got != "" {
		t.Fatalf("env token present, got %q", got)
	}
}

func TestCopilotThroughRouter(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake CLIs are shell scripts")
	}
	bin := t.TempDir()
	argsFile := filepath.Join(bin, "args")
	copilot := filepath.Join(bin, "copilot")
	os.WriteFile(copilot, []byte(`#!/bin/sh
printf '%s\n' "$@" "home=$COPILOT_HOME" "token=${COPILOT_GITHUB_TOKEN:+set}" > "`+argsFile+`"
echo '{"type":"session.start","data":{"sessionId":"x"}}'
echo '{"type":"assistant.message","data":{"content":"first"}}'
echo '{"type":"assistant.message","data":{"content":"copilot says hi"}}'
`), 0o755)
	for _, v := range copilotTokenVars {
		t.Setenv(v, "")
	}
	gh := filepath.Join(bin, "gh") // Linux: žeton pošlje router (lažni gh)
	os.WriteFile(gh, []byte("#!/bin/sh\necho gho_Test123\n"), 0o755)
	old := copilotGH
	copilotGH = func() string { return gh }
	defer func() { copilotGH = old }()
	s, _, base := liveServer(t, func(c *serveConfig) {
		c.AllowUnsandboxedBash = true // Linux brez bwrap (Docker)
		t.Setenv("COPILOT_HOME", t.TempDir())
		c.Copilot = agentCLI{Enabled: true, Executable: copilot, Models: []string{"gpt-5.2"}}
	})
	host := strings.TrimPrefix(base, "http://")
	if _, models := adminReq(t, base, host, "tok", "GET", "/v1/models", ""); !strings.Contains(models, `"claude-copilot"`) ||
		!strings.Contains(models, `"claude-copilot-gpt-5.2"`) {
		t.Fatalf("models: %s", models)
	}
	c := startLoopback(t, s)
	dir := t.TempDir()
	sc := &shareClient{c: c, session: "sidp", root: dir, sync: true, synced: map[string]syncEntry{}, reported: map[string]bool{}}
	if !sc.register() {
		t.Fatal("register failed: " + sc.refusal)
	}
	sc.start(2)
	defer sc.unregister()
	h := map[string]string{"x-claude-mini-session": "sidp", "x-claude-mini-cwd": dir, "content-type": "application/json"}
	ask := func(model string) (int, string) {
		st, data := call(c, "POST", "/v1/messages", h, []byte(`{"model":"`+model+`","tools":[{"name":"x"}],"messages":[{"role":"user","content":"hi"}]}`), 30*time.Second)
		return st, string(data)
	}
	args := func() []string {
		b, _ := os.ReadFile(argsFile)
		return strings.Split(strings.TrimSpace(string(b)), "\n")
	}
	st, b := ask("claude-copilot-gpt-5.2")
	if st != 200 || !strings.Contains(b, "copilot says hi") || strings.Contains(b, "first") {
		t.Fatalf("copilot: %d %s", st, b)
	}
	a := args()
	allow, deny := copilotRules(a)
	if !slices.Equal(allow, []string{"read"}) || !slices.Contains(deny, "write") || !slices.Contains(deny, "shell") ||
		slices.ContainsFunc(a, func(x string) bool { return slices.Contains(copilotForbidden, x) }) ||
		!strings.HasPrefix(a[len(a)-2], "home="+s.conf().Scratch+"/state/") {
		t.Fatalf("copilot args: %q", a)
	}
	if want := map[bool]string{true: "token=set", false: "token="}[runtime.GOOS == "linux"]; a[len(a)-1] != want {
		t.Fatalf("token handoff: %q, want %q", a[len(a)-1], want)
	}
	if st, _ := ask("claude-copilot-unknown"); st != 404 {
		t.Fatalf("unlisted model: %d", st)
	}
	if st, b := adminNoise(t, s, base, s.admin, "GET", adminPrefix+"/agents/copilot", ""); st != 200 || !strings.Contains(b, `"write":false`) ||
		!strings.Contains(b, `"commands":false`) || !strings.Contains(b, `"network":false`) {
		t.Fatalf("copilot API defaults: %d %s", st, b)
	}
	if st, b := adminNoise(t, s, base, s.admin, "PATCH", adminPrefix+"/agents/copilot", `{"write":true,"commands":true}`); st != 200 ||
		!strings.Contains(b, `"write":true`) || !strings.Contains(b, `"commands":true`) {
		t.Fatalf("copilot API: %d %s", st, b)
	}
	if st, _ := adminNoise(t, s, base, s.admin, "PATCH", adminPrefix+"/agents/codex", `{"commands":true}`); st != 400 {
		t.Fatal("codex commands accepted")
	}
	ask("claude-copilot")
	if allow, deny = copilotRules(args()); !slices.Contains(allow, "write") || !slices.Contains(allow, "shell") || !slices.Contains(deny, "shell(gh:*)") {
		t.Fatalf("after PATCH: allow %q deny %q", allow, deny)
	}
}
