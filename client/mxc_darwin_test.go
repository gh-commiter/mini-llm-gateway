//go:build darwin

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// Claude CLI v škatli od začetka do konca (pot Windows na Macu z lažnim wxc-exec): prijava kopirana noter in osvežena
// nazaj, CLAUDE_CONFIG_DIR, dom in TEMP pod Scratch, PowerShell namesto Bash, zahteva po teku pobrisana.
// Le macOS: pot škatle gre skozi peer_other.go; Windows: seznam za HPX360.
func TestBoxedClaudeSync(t *testing.T) {
	wxc, log := useFakeWxc(t)
	old, oldExe := windowsBox, wxcExe
	windowsBox, wxcExe = func() bool { return true }, func() string { return wxc }
	defer func() { windowsBox, wxcExe = old, oldExe }()
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.MkdirAll(filepath.Join(home, ".claude"), 0o700)
	os.WriteFile(filepath.Join(home, ".claude", ".credentials.json"), []byte("A"), 0o600)
	tmp := t.TempDir()
	fake, argsFile, envFile := filepath.Join(tmp, "claude"), filepath.Join(tmp, "args"), filepath.Join(tmp, "env")
	os.WriteFile(fake, []byte(`#!/bin/sh
printf '%s\n' "$@" > "`+argsFile+`"
env > "`+envFile+`"
`+cliGuard+`
cat > /dev/null
[ "$(cat "$CLAUDE_CONFIG_DIR/.credentials.json")" = A ] || exit 8
printf B > "$CLAUDE_CONFIG_DIR/.credentials.json"
cat src.txt > agent.txt
echo '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"done"}}}'
echo '{"type":"result","subtype":"success","usage":{"output_tokens":3}}'
`), 0o755)
	s, _ := testServer(t, func(c *serveConfig) {
		c.Claude = fake
		c.Tools = defaultTools + ",Bash"
		c.AllowUnsandboxedBash = false
	})
	c := startLoopback(t, s)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "src.txt"), []byte("from client\n"), 0o644)
	sc := &shareClient{c: c, session: "sidbox", root: dir, sync: true, synced: map[string]syncEntry{}, reported: map[string]bool{}}
	if !sc.register() {
		t.Fatal("register failed: " + sc.refusal)
	}
	sc.start(2)
	defer sc.unregister()
	body := `{"model":"claude-cli-haiku","tools":[{"name":"x"}],"messages":[{"role":"user","content":"go"}]}`
	h := map[string]string{"x-claude-mini-session": "sidbox", "x-claude-mini-cwd": dir, "content-type": "application/json"}
	st, data := call(c, "POST", "/v1/messages", h, []byte(body), 30*time.Second)
	if st != 200 || !strings.Contains(string(data), `"done"`) {
		t.Fatalf("messages: %d %s", st, data)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "agent.txt")); string(b) != "from client\n" {
		t.Fatalf("agent change not synced back: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(home, ".claude", ".credentials.json")); string(b) != "B" {
		t.Fatalf("refreshed login not copied back: %q", b)
	}
	env, _ := os.ReadFile(envFile)
	scratch := s.conf().Scratch
	val := func(k string) string { return envGet(strings.Split(string(env), "\n"), k) }
	if !under(val("CLAUDE_CONFIG_DIR"), filepath.Join(scratch, "state")) || !under(val("USERPROFILE"), scratch) || !under(val("TEMP"), scratch) ||
		val("CLAUDE_CODE_USE_POWERSHELL_TOOL") != "1" || val("MINIGW_SANDBOXED") != "1" || val("HTTPS_PROXY") != "" {
		t.Fatalf("env: %s", env)
	}
	args, _ := os.ReadFile(argsFile)
	if a := string(args); !strings.Contains(a, "PowerShell") || !strings.Contains(a, ",Bash") && !strings.Contains(a, "\nBash") {
		t.Fatalf("args: %s", a)
	}
	var req mxcRequest
	b, _ := os.ReadFile(log)
	json.Unmarshal(b, &req)
	if !slices.Contains(req.Filesystem.Denied, s.cfgDir) || !slices.ContainsFunc(req.Filesystem.ReadOnly, func(p string) bool { return under(p, scratch) && strings.Contains(p, "prompts") }) ||
		!slices.ContainsFunc(req.Filesystem.ReadWrite, func(p string) bool { return strings.HasPrefix(filepath.Base(p), "s-") || strings.Contains(p, "sync") }) {
		t.Fatalf("request: %s", b)
	}
	if left, _ := filepath.Glob(filepath.Join(scratch, "tmp", "mxc-*.json")); len(left) > 0 {
		t.Fatalf("request files left: %v", left)
	}
}
