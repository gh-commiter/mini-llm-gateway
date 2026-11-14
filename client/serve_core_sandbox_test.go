package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 0.6.0 task 7a (#106): the core opens on a gateway whose agents could read its folder only when the owner accepted
// that; every run is checked again at launch; the status says so. The platform rows are a pure function (tested for
// all platforms here); the real denies are in the bwrap test (Linux), the MXC request test and the Mac suite.

func TestAgentReadsCoreTable(t *testing.T) {
	for _, tc := range []struct {
		goos, kind       string
		sandboxed, allow bool
		reads            bool
	}{
		// Linux: bwrap hides the folder; without bwrap an agent runs only with the switch, and then reads it
		{"linux", "codex", true, false, false}, {"linux", "codex", true, true, false},
		{"linux", "codex", false, false, false}, {"linux", "codex", false, true, true},
		{"linux", "grok", false, true, true}, {"linux", "copilot", false, true, true}, {"linux", "grok", true, true, false},
		{"linux", "claude", false, true, false}, {"linux", "claude", true, true, false}, // Claude Code's own sandbox, failIfUnavailable
		// Windows: the MXC box denies the config folder; Codex and Copilot run beside it, only with the switch
		{"windows", "claude", true, true, false}, {"windows", "claude", false, true, true}, {"windows", "claude", false, false, false},
		{"windows", "grok", true, true, false}, {"windows", "grok", false, true, true}, {"windows", "grok", false, false, false},
		{"windows", "codex", true, true, true}, {"windows", "codex", true, false, false}, {"windows", "codex", false, true, true},
		{"windows", "copilot", true, true, true}, {"windows", "copilot", false, false, false},
	} {
		if got := agentReadsCoreOn(tc.goos, tc.kind, tc.sandboxed, tc.allow); got != tc.reads {
			t.Errorf("%s %s sandboxed=%v allow=%v: reads=%v, want %v", tc.goos, tc.kind, tc.sandboxed, tc.allow, got, tc.reads)
		}
	}
}

// exposedServer: a server whose Codex runs without a sandbox (the switch is on, no bwrap, no box).
func exposedServer(t *testing.T, mod func(*serveConfig)) *server {
	t.Helper()
	old := agentSandboxedNow
	agentSandboxedNow = func(string) bool { return false }
	t.Cleanup(func() { agentSandboxedNow = old })
	s, _ := testServer(t, func(c *serveConfig) {
		c.AllowUnsandboxedBash = true
		c.Codex.Enabled = true
		if mod != nil {
			mod(c)
		}
	})
	return s
}

func TestCoreRefusesToOpenWhereAgentsReadIt(t *testing.T) {
	s := exposedServer(t, nil)
	var logs strings.Builder
	s.log.SetOutput(&logs)
	s.openCore()
	t.Cleanup(s.closeCore)
	if s.core != nil || s.coreErr == nil || !strings.Contains(s.coreErr.Error(), "acceptAgentsCanReadNotes") || !strings.Contains(s.coreErr.Error(), "Codex") {
		t.Fatalf("core %v err %v", s.core, s.coreErr)
	}
	if _, err := os.Stat(filepath.Join(s.cfgDir, coreDirName)); err == nil {
		t.Fatal("the core folder was created although the core must not open")
	}
	if st, _ := s.adminDo("GET", "/users", nil); st != 503 { // projects off; the models keep working (TestCoreUnavailableStillStarts)
		t.Fatalf("admin: %d", st)
	}
	if line := s.coreStatus(s.conf()); !strings.HasPrefix(line, "projects: off (") || !strings.Contains(line, "acceptAgentsCanReadNotes") {
		t.Fatalf("status: %q", line)
	}
	if !strings.Contains(logs.String(), "WARNING") {
		t.Fatalf("log: %q", logs.String())
	}
	// a Codex that is off does not count
	s2 := exposedServer(t, func(c *serveConfig) { c.Codex.Enabled = false })
	s2.openCore()
	t.Cleanup(s2.closeCore)
	if s2.core == nil {
		t.Fatalf("nothing exposes the core, but it did not open: %v", s2.coreErr)
	}
}

func TestCoreOpensWhenAcceptedAndSaysSo(t *testing.T) {
	s := exposedServer(t, func(c *serveConfig) { c.AcceptAgentsReadNotes = true })
	var logs strings.Builder
	s.log.SetOutput(&logs)
	s.openCore()
	t.Cleanup(s.closeCore)
	if s.core == nil {
		t.Fatalf("accepted, but not opened: %v", s.coreErr)
	}
	if line := s.coreStatus(s.conf()); !strings.Contains(line, "projects: on; Codex can read all notes (accepted: acceptAgentsCanReadNotes)") {
		t.Fatalf("status: %q", line)
	}
	if !strings.Contains(logs.String(), "WARNING: Codex run without a sandbox and can read the core database") || !strings.Contains(logs.String(), "acceptAgentsCanReadNotes") {
		t.Fatalf("start log: %q", logs.String())
	}
	// the run is not refused either
	if why := s.coreRefusal(s.conf(), "codex"); why != "" {
		t.Fatalf("refused although accepted: %s", why)
	}
}

func TestCoreSandboxedAgentsNeedNoAcceptance(t *testing.T) {
	// the switch is off: with a sandbox (or no permission to run without one) nothing reads the folder, on every platform
	s, _ := testServer(t, func(c *serveConfig) { c.Codex.Enabled = true; c.Grok.Enabled = true })
	old := agentSandboxedNow
	agentSandboxedNow = func(string) bool { return true }
	t.Cleanup(func() { agentSandboxedNow = old })
	s.openCore()
	t.Cleanup(s.closeCore)
	if s.core == nil || s.coreStatus(s.conf()) != "projects: on" {
		t.Fatalf("core %v err %v status %q", s.core, s.coreErr, s.coreStatus(s.conf()))
	}
}

// The check is made at every launch, not only at start: the core is open, then the sandbox goes away.
func TestCoreRunRefusedAtLaunch(t *testing.T) {
	s, _ := testServer(t, func(c *serveConfig) { c.Codex.Enabled = true; c.Grok.Enabled = true })
	s.openCore() // nothing exposes it yet (the switch is off): it opens
	t.Cleanup(s.closeCore)
	if s.core == nil {
		t.Fatal(s.coreErr)
	}
	old := agentSandboxedNow
	agentSandboxedNow = func(string) bool { return false }
	t.Cleanup(func() { agentSandboxedNow = old })
	if err := s.update(func(c *serveConfig) error { c.AllowUnsandboxedBash = true; return nil }); err != nil { // the owner turns the switch on while running
		t.Fatal(err)
	}
	c := startLoopback(t, s)
	h := map[string]string{"content-type": "application/json", "x-claude-mini-session": "sid"}
	for _, model := range []string{"claude-codex", "claude-grok"} {
		st, data := call(c, "POST", "/v1/messages", h, []byte(`{"model":"`+model+`","tools":[{"name":"x"}],"messages":[{"role":"user","content":"go"}]}`), 20*time.Second)
		if st != 403 || !strings.Contains(string(data), "cannot run on this gateway while projects are enabled") || !strings.Contains(string(data), "acceptAgentsCanReadNotes") {
			t.Fatalf("%s: %d %s", model, st, data)
		}
	}
	// accepted: the refusal is gone (the request now fails later, for another reason: no synced folder, no program)
	if err := s.update(func(c *serveConfig) error { c.AcceptAgentsReadNotes = true; return nil }); err != nil {
		t.Fatal(err)
	}
	st, data := call(c, "POST", "/v1/messages", h, []byte(`{"model":"claude-codex","tools":[{"name":"x"}],"messages":[{"role":"user","content":"go"}]}`), 20*time.Second)
	if strings.Contains(string(data), "while projects are enabled") {
		t.Fatalf("still refused: %d %s", st, data)
	}
}

func TestAcceptSettingViaAdminAndStatus(t *testing.T) {
	s, _ := testServer(t, nil)
	if st, _ := s.adminDo("PATCH", "/agents/claude", []byte(`{"acceptAgentsCanReadNotes":true}`)); st != 200 {
		t.Fatalf("patch: %d", st)
	}
	if !s.conf().AcceptAgentsReadNotes {
		t.Fatal("setting not stored")
	}
	_, obj := s.adminDo("GET", "/agents/claude", nil)
	b, _ := json.Marshal(obj)
	if !strings.Contains(string(b), `"acceptAgentsCanReadNotes":true`) {
		t.Fatalf("GET: %s", b)
	}
	// persisted in serve.json (the key the owner can also edit)
	raw, _ := os.ReadFile(s.cfgPath)
	if !strings.Contains(string(raw), `"acceptAgentsCanReadNotes": true`) && !strings.Contains(string(raw), `"acceptAgentsCanReadNotes":true`) {
		t.Fatalf("serve.json: %s", raw)
	}
	// `minigw gw status` shows the line: it is part of the status answer
	s.openCore()
	t.Cleanup(s.closeCore)
	_, st := s.adminDo("GET", "/status", nil)
	sb, _ := json.Marshal(st)
	if !strings.Contains(string(sb), `"core":"projects: on`) {
		t.Fatalf("status: %s", sb)
	}
}

// The config folder (so the core folder in it) is in every box request, by its real path; an allowed folder cannot
// contain it, be inside it or reach it through a link.
func TestConfigFolderDeniedAndNotAllowed(t *testing.T) {
	base := realOf(t, t.TempDir())
	cfg := filepath.Join(base, "cfg")
	work := filepath.Join(base, "work")
	for _, d := range []string{filepath.Join(cfg, coreDirName), work} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	paths := []string{base, cfg, filepath.Join(cfg, coreDirName)}
	link := filepath.Join(work, "lnk")
	if err := os.Symlink(filepath.Join(cfg, coreDirName), link); err == nil { // Windows without the privilege: the junction case is in the Windows suite
		paths = append(paths, link)
	}
	for _, f := range paths {
		if _, err := checkFolder(f, cfg, false); err == nil {
			t.Errorf("allowed folder %s reaches the config folder", f)
		}
	}
	if _, err := checkFolder(work, cfg, false); err != nil {
		t.Errorf("work folder refused: %v", err)
	}
	// the deny list of the box request holds the config folder by its real path, and so the core folder in it
	deny := scratchPaths(cfg)
	if len(deny) == 0 || deny[0] != cfg {
		t.Fatalf("deny: %v", deny)
	}
}

// Data of an earlier period stays readable for unsandboxed agents while the core is refused: log and status say so.
func TestStaleCoreDataIsReported(t *testing.T) {
	s := exposedServer(t, nil)
	var logs strings.Builder
	s.log.SetOutput(&logs)
	dir := filepath.Join(s.cfgDir, coreDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// an empty folder is no data
	s.openCore()
	if s.coreStale != "" || strings.Contains(s.coreStatus(s.conf()), "old project data") {
		t.Fatalf("empty folder reported: %q", s.coreStatus(s.conf()))
	}
	if err := os.WriteFile(filepath.Join(dir, "core.db"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	logs.Reset()
	s.coreErr = nil
	s.openCore()
	if s.core != nil || s.coreStale != dir || strings.Count(logs.String(), "old project data is still on disk in "+dir) != 1 {
		t.Fatalf("stale %q log %q", s.coreStale, logs.String())
	}
	if line := s.coreStatus(s.conf()); !strings.Contains(line, "projects: off") || !strings.HasSuffix(line, "old project data is still on disk and readable by agents: "+dir) {
		t.Fatalf("status: %q", line)
	}
	if _, err := os.Stat(filepath.Join(dir, "core.db")); err != nil { // nothing is deleted
		t.Fatal(err)
	}
	// opened (accepted): no stale note
	s2 := exposedServer(t, func(c *serveConfig) { c.AcceptAgentsReadNotes = true })
	s2.openCore()
	t.Cleanup(s2.closeCore)
	if s2.coreStale != "" || strings.Contains(s2.coreStatus(s2.conf()), "old project data") {
		t.Fatalf("accepted: %q", s2.coreStatus(s2.conf()))
	}
}
