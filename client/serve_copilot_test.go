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
	ro := copilotArgs(true, "", false, false, false, "s1")
	allow, deny := copilotRules(ro)
	if slices.ContainsFunc(ro, func(x string) bool { return slices.Contains(copilotForbidden, x) || x == "--add-dir" }) { // M3
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
	if i := slices.Index(ro, "--session-id"); i < 0 || ro[i+1] != "s1" || slices.Contains(ro, "-p") { // navodilo na stdin
		t.Fatalf("session/prompt: %q", ro)
	}
	w := copilotArgs(true, "gpt-5.2", true, true, true, "s")
	allow, deny = copilotRules(w)
	if !slices.Equal(allow, []string{"read", "write", "shell", "url"}) || !slices.Contains(deny, "shell(gh:*)") || !slices.Contains(deny, "shell(security:*)") {
		t.Fatalf("write+commands: allow %q deny %q", allow, deny)
	}
	if allow, deny := copilotRules(copilotArgs(true, "", false, true, false, "s")); !slices.Equal(allow, []string{"read"}) ||
		!slices.Contains(deny, "shell") {
		t.Fatalf("commands without write (or --ro): allow %q deny %q", allow, deny)
	}
	if w[slices.Index(w, "--model")+1] != "gpt-5.2" {
		t.Fatalf("model: %q", w)
	}
	chat := copilotArgs(false, "", true, true, true, "s")
	if allow, deny = copilotRules(chat); len(allow) != 0 || !slices.Contains(deny, "read") {
		t.Fatalf("chat: allow %q deny %q", allow, deny)
	}
	if txt, _ := copilotEvent([]byte(`{"type":"assistant.message","data":{"content":"Hi!"}}`)); txt != "Hi!" {
		t.Fatal("assistant.message")
	}
	if _, st := copilotEvent([]byte(`{"type":"tool.execution_start","data":{"toolName":"view"}}`)); st != "view" {
		t.Fatal("tool step")
	}
	env := agentEnv([]string{"COPILOT_ALLOW_ALL=true", "COPILOT_ALLOW_ALL_TOOLS=1", "COPILOT_ASSISTED_APPROVAL=1", "COPILOT_HOME=/x", "PATH=/bin",
		"COPILOT_PROVIDER_API_KEY_COMMAND=touch /tmp/x", "COPILOT_CLI_DIST_DIR=/evil", "GH_TOKEN=u", "GITHUB_TOKEN=u", "COPILOT_GITHUB_TOKEN=u"})
	if !slices.Equal(env, []string{"PATH=/bin"}) {
		t.Fatalf("env: %q", env)
	}
	c := agentCLI{Models: []string{"gpt-5.2"}}
	if !c.allowsModel(copilotKind, "gpt-5.2") || c.allowsModel(copilotKind, "other") || (agentCLI{}).allowsModel(copilotKind, "x") ||
		(agentCLI{}).canWrite(copilotKind) || !(agentCLI{}).canWrite(codexKind) {
		t.Fatal("models/write defaults")
	}
	if !validCopilotToken("github_pat_11A_x") || validCopilotToken("gho_x") || validCopilotToken("github_pat_a b") {
		t.Fatal("token validation")
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
	t.Setenv("GH_TOKEN", "envtok") // žeton iz okolja routerja ima prednost (agentEnv ga agentu sicer ne da)
	if got := copilotToken(t.Context()); got != "envtok" {
		t.Fatalf("env token: got %q", got)
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
if [ "$1" = --version ]; then echo "token=${COPILOT_GITHUB_TOKEN:-none}" >> "`+argsFile+`.version"; exit 0; fi
id=""; prev=""; for a in "$@"; do [ "$prev" = --session-id ] && id="$a"; prev="$a"; done
printf '%s\n' "$@" "home=$COPILOT_HOME" "token=$COPILOT_GITHUB_TOKEN" > "`+argsFile+`"
cat > "`+argsFile+`.stdin"
st="$COPILOT_HOME/session-state/$id"; mkdir -p "$st"
n=$(cat "$st/events.jsonl" 2>/dev/null || echo 0); echo $((n + 1)) > "$st/events.jsonl"
hooks=no; [ -e "$COPILOT_HOME/hooks.json" ] && hooks=yes; [ -e "$st/checkpoints" ] && hooks=checkpoints
link=no; [ -L "$st/link" ] && link=yes
echo '{}' > "$COPILOT_HOME/hooks.json"; ln -s /etc "$st/link" 2>/dev/null; mkdir -p "$st/checkpoints"
echo '{"type":"session.start","data":{"sessionId":"x"}}'
echo '{"type":"assistant.message","data":{"content":"first"}}'
echo '{"type":"assistant.message","data":{"content":"copilot says hi turn='$((n + 1))' hooks='$hooks' link='$link'"}}'
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
	ask := func(model string, history ...string) (int, string) {
		msgs := `{"role":"user","content":"PROMPT_MARKER"}`
		if len(history) == 1 {
			msgs += `,{"role":"assistant","content":"` + history[0] + `"},{"role":"user","content":"next"}`
		}
		st, data := call(c, "POST", "/v1/messages", h, []byte(`{"model":"`+model+`","tools":[{"name":"x"}],"messages":[`+msgs+`]}`), 30*time.Second)
		return st, string(data)
	}
	args := func() []string {
		b, _ := os.ReadFile(argsFile)
		return strings.Split(strings.TrimSpace(string(b)), "\n")
	}
	st, b := ask("claude-copilot-gpt-5.2")
	if st != 200 || !strings.Contains(b, "copilot says hi turn=1 hooks=no") || strings.Contains(b, "first") {
		t.Fatalf("copilot: %d %s", st, b)
	}
	a := args()
	home := strings.TrimPrefix(a[len(a)-2], "home=")
	allow, deny := copilotRules(a)
	if !slices.Equal(allow, []string{"read"}) || !slices.Contains(deny, "write") || !slices.Contains(deny, "shell") ||
		slices.ContainsFunc(a, func(x string) bool { return slices.Contains(copilotForbidden, x) }) ||
		!strings.HasPrefix(home, s.conf().Scratch+"/tmp/req-") {
		t.Fatalf("copilot args: %q", a)
	}
	if _, err := os.Stat(home); err == nil {
		t.Fatal("per-request COPILOT_HOME not removed")
	}
	// Navodilo na stdin, ne v argumentih (ps, /proc/<pid>/cmdline).
	if in, _ := os.ReadFile(argsFile + ".stdin"); !strings.Contains(string(in), "PROMPT_MARKER") ||
		slices.ContainsFunc(a, func(x string) bool { return strings.Contains(x, "PROMPT_MARKER") }) {
		t.Fatalf("prompt not on stdin: %q / %q", in, a)
	}
	if a[len(a)-1] != "token=gho_Test123" { // žeton GitHub CLI (agentEnv sam ne da nobenega)
		t.Fatalf("token handoff: %q", a[len(a)-1])
	}
	// R7: na Linuxu enkrat »--version« brez bwrap in brez žetona (razširitev paketa).
	if v, _ := os.ReadFile(argsFile + ".version"); runtime.GOOS == "linux" && !strings.HasPrefix(string(v), "token=none") {
		t.Fatalf("prepare: %q", v)
	}
	// C2, R6: naslednji zavoj: le events.jsonl in workspace.yaml ostaneta (brez povezave, checkpoints), hooks.json ne.
	if st, b := ask("claude-copilot-gpt-5.2", "copilot says hi turn=1 hooks=no link=no"); st != 200 || !strings.Contains(b, "turn=2 hooks=no link=no") {
		t.Fatalf("second turn: %d %s", st, b)
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
	// H1: ukazi brez namenskega žetona: zavrnjeno; z njim gre ta žeton (ne gh).
	if st, b := ask("claude-copilot"); st != 503 || !strings.Contains(b, "Copilot Requests") {
		t.Fatalf("commands without token: %d %s", st, b)
	}
	if st, _ := adminNoise(t, s, base, s.admin, "PATCH", adminPrefix+"/agents/copilot", `{"token":"gho_classic"}`); st != 400 {
		t.Fatal("classic token accepted")
	}
	if st, b := adminNoise(t, s, base, s.admin, "PATCH", adminPrefix+"/agents/copilot", `{"token":"github_pat_cmd1"}`); st != 200 ||
		!strings.Contains(b, `"hasToken":true`) || strings.Contains(b, "github_pat_cmd1") {
		t.Fatalf("token: %d %s", st, b)
	}
	if st, b := ask("claude-copilot"); st != 200 || args()[len(args())-1] != "token=github_pat_cmd1" {
		t.Fatalf("commands with token: %d %s %q", st, b, args())
	}
	defer adminNoise(t, s, base, s.admin, "PATCH", adminPrefix+"/agents/copilot", `{"token":""}`)
	if allow, deny = copilotRules(args()); !slices.Contains(allow, "write") || !slices.Contains(allow, "shell") || !slices.Contains(deny, "shell(gh:*)") {
		t.Fatalf("after PATCH: allow %q deny %q", allow, deny)
	}
}

// R4, R6: le events.jsonl in workspace.yaml, le navadne datoteke iz prave mape; povezava (datoteka ali mapa) ne.
func TestCopySession(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges")
	}
	d := t.TempDir()
	src, secret := filepath.Join(d, "s"), filepath.Join(d, "secret")
	os.MkdirAll(filepath.Join(src, "checkpoints"), 0o700)
	os.WriteFile(secret, []byte("s"), 0o600)
	os.WriteFile(filepath.Join(src, "events.jsonl"), []byte("e"), 0o600)
	os.WriteFile(filepath.Join(src, "checkpoints", "x"), []byte("c"), 0o600)
	os.Symlink(secret, filepath.Join(src, "workspace.yaml"))
	if err := copySession(src, filepath.Join(d, "o")); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadDir(filepath.Join(d, "o"))
	if len(got) != 1 || got[0].Name() != "events.jsonl" {
		t.Fatalf("copied %v", got)
	}
	os.Symlink(src, filepath.Join(d, "link"))
	if copySession(filepath.Join(d, "link"), filepath.Join(d, "o2")) == nil {
		t.Fatal("followed a linked session folder")
	}
}

// #26: dovoljena mapa, ki vsebuje paket Copilot ali je v njem, je zavrnjena (kot Scratch).
func TestCopilotCacheFoldersRefused(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	if runtime.GOOS == "windows" {
		t.Setenv("LocalAppData", filepath.Join(home, "AppData", "Local"))
	}
	cache := copilotCacheDir()
	os.MkdirAll(filepath.Join(cache, "pkg"), 0o700)
	other := filepath.Join(home, "projects")
	os.MkdirAll(other, 0o700)
	if len(copilotCacheOverlaps([]string{filepath.Dir(cache)})) != 1 || len(copilotCacheOverlaps([]string{filepath.Join(cache, "pkg")})) != 1 ||
		len(copilotCacheOverlaps([]string{other})) != 0 {
		t.Fatal("overlaps")
	}
	cfgDir := filepath.Join(home, "cfg")
	os.MkdirAll(cfgDir, 0o700)
	c := serveConfig{AllowedFolders: []string{filepath.Dir(cache)}, Scratch: filepath.Join(home, "scratch")}
	if _, err := checkAgent(&c, cfgDir); err == nil || !strings.Contains(err.Error(), "Copilot's package cache") {
		t.Fatalf("checkAgent: %v", err)
	}
	// N2: tudi privzeta mapa (Folder) ne v paketu Copilot ali Scratch
	c = serveConfig{Folder: filepath.Join(cache, "pkg"), Scratch: filepath.Join(home, "scratch")}
	if _, err := checkAgent(&c, cfgDir); err == nil || !strings.HasPrefix(err.Error(), "folder ") || !strings.Contains(err.Error(), "Copilot's package cache") {
		t.Fatalf("checkAgent folder: %v", err)
	}
	os.MkdirAll(filepath.Join(home, "scratch", "x"), 0o700)
	c = serveConfig{Folder: filepath.Join(home, "scratch", "x"), Scratch: filepath.Join(home, "scratch")}
	if _, err := checkAgent(&c, cfgDir); err == nil || !strings.Contains(err.Error(), "scratch folder") {
		t.Fatalf("checkAgent scratch folder: %v", err)
	}
}
