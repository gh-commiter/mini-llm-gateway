package main

import (
	"crypto/ed25519"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Priklop = prva zahteva po 10 minutah tišine; največ 20; odstranjena naprava izgubi zgodovino, njene seje ostanejo.
func TestDeviceHistoryAndSessions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	g := loadState(path)
	t0 := time.Date(2026, 11, 8, 10, 0, 0, 0, time.UTC)
	g.seen("A", "192.0.2.1", "Linux, minigw 0.5.18", t0)
	g.seen("A", "192.0.2.1", "", t0.Add(time.Minute))
	g.seen("A", "192.0.2.2", "", t0.Add(12*time.Minute))
	if d, _ := g.device("A"); len(d.History) != 2 || d.History[1].Address != "192.0.2.2" || d.History[1].Client != "Linux, minigw 0.5.18" ||
		!d.LastSeen.Equal(t0.Add(12*time.Minute)) {
		t.Fatalf("history: %+v", d)
	}
	for i := range 30 {
		g.seen("A", "192.0.2.3", "", t0.Add(time.Duration(i+2)*time.Hour))
	}
	if d, _ := g.device("A"); len(d.History) != historyPerDevice {
		t.Fatalf("history cap: %d", len(d.History))
	}

	use := func(session, device, model string, status int, at time.Time) {
		g.record(sessionUse{session: session, device: device, deviceName: "PC", folder: "/home/u/p", mode: "sync", model: model,
			status: status, took: 2 * time.Second, in: 10, out: 5}, at)
	}
	use("s1", "A", "claude-codex", 200, t0)
	use("s1", "A", "claude-codex", 500, t0.Add(time.Minute))
	use("s1", "A", "claude-sonnet", 403, t0.Add(2*time.Minute))
	use("", "A", "claude-haiku", 200, t0)
	use("", "A", "claude-haiku", 200, t0.Add(5*time.Minute))  // isto okno
	use("", "A", "claude-haiku", 200, t0.Add(30*time.Minute)) // novo okno
	all := g.sessions("", "")
	if len(all) != 3 {
		t.Fatalf("sessions: %d", len(all))
	}
	var s1 apiSession
	for _, x := range g.sessions("A", "/home/u") {
		if x.ID == "s1" {
			s1 = x
		}
	}
	if m := findUse(s1, "claude-codex", ""); m == nil || m.Requests != 2 || m.Errors != 1 || m.Seconds != 4 || m.InputTokens != 20 || m.OutputTokens != 10 ||
		findUse(s1, "claude-sonnet", "").Refused != 1 || s1.Duration != 120 || s1.Mode != "sync" {
		t.Fatalf("s1: %+v", s1)
	}
	if len(g.sessions("B", "")) != 0 || len(g.sessions("", "/home/u/other")) != 0 {
		t.Fatal("filters")
	}
	g.prune([]serveDevice{{ID: "B"}})
	if _, ok := g.device("A"); ok || !g.sessions("A", "")[0].DeviceRemoved {
		t.Fatal("removed device keeps its history or sessions are not marked")
	}
	for i := range maxSessions + 5 {
		use("x"+string(rune('a'+i%26))+time.Duration(i).String(), "B", "m", 200, t0)
	}
	if n := len(g.sessions("B", "")); n != maxDeviceSess {
		t.Fatalf("per-device cap: %d", n)
	}
	if err := g.save(); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(path); runtime.GOOS != "windows" && st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode())
	}
	if h := loadState(path); len(h.Sessions) != 53 { // 3 naprave A + 50 naprave B
		t.Fatalf("reload: %d", len(h.Sessions))
	}
	g.clearSessions()
	if len(g.sessions("", "")) != 0 {
		t.Fatal("clear")
	}
}

func findUse(x apiSession, model, role string) *modelUse {
	for _, m := range x.Models {
		if m.Model == model && m.Role == role {
			return m
		}
	}
	return nil
}

// Mapa, vloga, omejitve in ID seje; enaki primeri kot folderRoleAndRestrictions na Macu.
func TestFolderRoleAndRestrictions(t *testing.T) {
	for in, want := range map[string]string{`C:\Users\u\p\`: "C:/Users/u/p", "/Users/u/p/./a/..": "/Users/u/p", "/": "/", "rel//x/": "rel/x", "": "",
		`\\srv\share\x\`: "//srv/share/x", `/home/u/a\b/`: `/home/u/a\b`, "c:": "c:"} {
		if got := normFolder(in); got != want {
			t.Errorf("normFolder(%q) = %q, want %q", in, got, want)
		}
	}
	for r, ok := range map[string]bool{"dev": true, "sec.review-1_x": true, "": false, "a b": false, "x\nx-evil: 1": false, "dév": false, strings.Repeat("a", 33): false} {
		if validRole(r) != ok {
			t.Errorf("validRole(%q)", r)
		}
	}
	g := loadState(filepath.Join(t.TempDir(), "state.json"))
	for _, role := range []string{"security", "dev", "dev", ""} {
		g.record(sessionUse{session: "s", folder: `C:\Proj\`, os: "Windows", model: "claude-codex", role: role, status: 200, tools: "write", readOnly: true}, time.Now())
	}
	s := g.sessions("", `c:/proj`) // Windows: brez razlike v velikih črkah
	if len(s) != 1 || s[0].Folder != "C:/Proj" || s[0].Role != "" || findUse(s[0], "claude-codex", "dev").Requests != 2 ||
		findUse(s[0], "claude-codex", "security").Requests != 1 || findUse(s[0], "claude-codex", "").Requests != 1 ||
		!findUse(s[0], "claude-codex", "dev").ReadOnly || findUse(s[0], "claude-codex", "dev").Tools != "write" {
		t.Fatalf("%+v", s)
	}
	for h, want := range map[string]string{"dev": "dev", strings.Repeat("a", 33): "", "a b": ""} {
		if got := reqRole(&req{Request: &http.Request{Header: http.Header{"X-Claude-Mini-Role": {h}}}}); got != want {
			t.Errorf("reqRole(%q) = %q, want %q", h, got, want)
		}
	}
	g.record(sessionUse{session: "l", folder: "/home/U", os: "Linux", model: "m"}, time.Now())
	if len(g.sessions("", "/home/u")) != 0 || len(g.sessions("", "/home/U")) != 1 {
		t.Fatal("Linux folders are case-sensitive")
	}
	// M2: ID nad 128 bajtov ali s kontrolnimi znaki ni zapisan; L5: zahteva vseeno šteje, v seji »t-…«
	g.record(sessionUse{session: strings.Repeat("x", 129), model: "m"}, time.Now())
	g.record(sessionUse{session: "a\x1b[31m", model: "m"}, time.Now())
	if l := g.sessions("", ""); len(l) != 3 || !strings.HasPrefix(l[0].ID, "t-") || l[0].Models[0].Requests != 2 {
		t.Fatalf("bad session IDs: %+v", l)
	}
	c := serveConfig{Tools: "Read,Grep"}
	c.Copilot.Network = true
	for model, want := range map[string]string{"claude-cli": "Read,Grep", "claude-cli-opus": "Read,Grep", "claude-codex": "write", "claude-codex-gpt-5": "write",
		"claude-grok": "write", "claude-copilot": "network", "claude-sonnet": ""} {
		if got := c.agentRestrictions(model, false); got != want {
			t.Errorf("agentRestrictions(%s) = %q, want %q", model, got, want)
		}
	}
	c.Tools, c.Copilot.Write, c.Copilot.Commands = "Read,Edit,Grep", true, true
	for model, want := range map[string]string{"claude-cli": "Read,Grep", "claude-codex": "", "claude-grok": "", "claude-copilot": "network"} { // --minigw-ro
		if got := c.agentRestrictions(model, true); got != want {
			t.Errorf("agentRestrictions(%s, ro) = %q, want %q", model, got, want)
		}
	}
	c.AllowLocalTools = true
	if c.agentRestrictions("claude-cli", false) != "local" || c.agentRestrictions("claude-cli", true) != "local,read-only" {
		t.Error("local tools")
	}
	c.Codex.Network = true // omrežje Codex le s pisanjem
	if c.agentRestrictions("claude-codex", false) != "write,network" || c.agentRestrictions("claude-codex", true) != "" {
		t.Error("codex network without write")
	}
}

// --minigw-role: oblike, prednost pred MINIGW_ROLE, neveljavno ime; enaki primeri kot takeRole na Macu.
func TestTakeRole(t *testing.T) {
	for _, c := range []struct {
		args      []string
		env, role string
		rest      string
		ok        bool
	}{
		{[]string{"-p", "x"}, "", "", "-p x", true},
		{[]string{"--minigw-role", "dev", "-p", "x"}, "", "dev", "-p x", true},
		{[]string{"-p", "--minigw-role=security"}, "dev", "security", "-p", true},
		{[]string{"-p"}, "dev", "dev", "-p", true},
		{[]string{"--minigw-role", "a b"}, "", "a b", "", false},
		{[]string{"-p", "--minigw-role"}, "", "", "-p", false}, // brez vrednosti: napaka
	} {
		role, rest, ok := takeRole(c.args, c.env)
		if role != c.role || strings.Join(rest, " ") != c.rest || ok != c.ok {
			t.Errorf("takeRole(%q, %q) = %q, %q, %v", c.args, c.env, role, rest, ok)
		}
	}
}

// Ugnezdeni klic (agent-calls): v seji klicatelja pod isto napravo, z »via« = veriga izvajanja in vlogo klicatelja
// (glavi vprašanega agenta ne veljata).
func TestNestedCallLandsInCallersSession(t *testing.T) {
	dev := serveDevice{ID: base64url(newDevice("PC").key.Public().(ed25519.PublicKey)), Name: "PC"}
	s, _ := testServer(t, func(c *serveConfig) {
		c.ClaudeAskOthers, c.AllowLocalTools = true, true
		c.Devices = []serveDevice{dev}
	})
	parent := httptest.NewRequest("POST", "/v1/messages", nil)
	parent.Header.Set("X-Claude-Mini-Session", "sid1")
	parent.Header.Set("X-Claude-Mini-Cwd", "/client/dir")
	parent.Header.Set("X-Claude-Mini-Role", "dev")
	run, _ := s.startRun(&req{Request: parent, conn: &serveConn{device: &dev, peer: "10.0.0.9"}}, "claude", "/copy", true, true, "", "", "", "")
	if run == nil {
		t.Fatal("no run")
	}
	defer run.stop()
	run.started(os.Getpid())
	q, _ := http.NewRequest("POST", run.url+"/v1/messages", strings.NewReader(`{"model":"claude-nope","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`))
	q.Header.Set("Authorization", "Bearer "+run.token)
	q.Header.Set("X-Claude-Mini-Session", "forged")
	q.Header.Set("X-Claude-Mini-Role", "security")
	q.Header.Set("X-Claude-Mini-Ro", "0") // ne odstrani --minigw-ro klicatelja (spodaj)
	q.Header.Set("X-Minigw-Agent-Chain", "codex")
	resp, err := http.DefaultClient.Do(q)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	l := s.state.sessions(dev.ID, "")
	if len(l) != 1 || l[0].ID != "sid1" || l[0].Role != "dev" || l[0].Folder != "/client/dir" || len(l[0].Models) != 1 ||
		l[0].Models[0].Via != "claude" || l[0].Models[0].Role != "dev" || l[0].Models[0].Model != "unknown" || !l[0].Models[0].ReadOnly {
		t.Fatalf("status %d, sessions %+v", resp.StatusCode, l)
	}
}

// Žetoni iz odgovora (SSE), tudi ko meja zapisa razdeli število.
func TestUsageWriter(t *testing.T) {
	u := &usageWriter{ResponseWriter: httptest.NewRecorder()}
	for _, p := range []string{`data: {"type":"message_start","message":{"usage":{"input_tokens":12`, `34,"cache_read_input_tokens":999,"output_tokens":1}}}`,
		"\n\ndata: {\"usage\":{\"output_tok", `ens": 56}}`} {
		u.Write([]byte(p))
	}
	if u.in != 1234 || u.out != 56 {
		t.Fatalf("in %d out %d", u.in, u.out)
	}
}

// API in CLI: zgodovina dovoljene naprave, seje, brisanje.
func TestSessionsAPIAndCLI(t *testing.T) {
	id := base64url(newDevice("PC").key.Public().(ed25519.PublicKey))
	s, cfg, base := liveServer(t, func(c *serveConfig) { c.Devices = []serveDevice{{ID: id, Name: "PC"}} })
	s.deviceSeen(serveDevice{ID: id, Name: "PC"}, "192.0.2.7", "Windows, minigw 0.5.18")
	s.state.record(sessionUse{session: "abc", device: id, deviceName: "PC", folder: `C:\p`, mode: "worktree", branch: "minigw/claude-1",
		model: "claude-sonnet", status: 200, took: time.Second, in: 3, out: 4}, time.Now())
	if st, b := adminNoise(t, s, base, s.admin, "GET", adminPrefix+"/devices/"+id+"/history", ""); st != 200 ||
		!strings.Contains(b, `"address":"192.0.2.7"`) || !strings.Contains(b, `"client":"Windows, minigw 0.5.18"`) {
		t.Fatalf("history: %d %s", st, b)
	}
	if st, _ := adminNoise(t, s, base, s.admin, "GET", adminPrefix+"/devices/nope/history", ""); st != 404 {
		t.Fatal("history of an unknown device")
	}
	if st, b := adminNoise(t, s, base, s.admin, "GET", adminPrefix+"/sessions/abc", ""); st != 200 || !strings.Contains(b, `"branch":"minigw/claude-1"`) ||
		!strings.Contains(b, `"inputTokens":3`) {
		t.Fatalf("session: %d %s", st, b)
	}
	run := func(args ...string) string {
		return stdout(t, func() { routerCommand(append(args, "--config", cfg), strings.NewReader("")) })
	}
	if out := run("devices", "history", id); !strings.Contains(out, `"192.0.2.7"  "Windows, minigw 0.5.18"`) {
		t.Fatalf("cli history: %q", out)
	}
	if out := run("sessions", "--folder", `C:\p`); !strings.Contains(out, `"abc"  "PC"  worktree  "C:/p"  "claude-sonnet"`) {
		t.Fatalf("cli sessions: %q", out)
	}
	if out := run("sessions", "abc"); !strings.Contains(out, `"claude-sonnet": 1 requests, 0 errors, 0 refused, 1s, 3 input and 4 output tokens`) ||
		!strings.Contains(out, `branch "minigw/claude-1"`) {
		t.Fatalf("cli session: %q", out)
	}
	if out := run("sessions", "clear"); !strings.Contains(out, "cleared") || len(s.state.sessions("", "")) != 0 {
		t.Fatalf("clear: %q", out)
	}
	if b, _ := os.ReadFile(filepath.Join(filepath.Dir(cfg), "state.json")); strings.Contains(string(b), s.conf().Token) {
		t.Fatal("token in state.json")
	}
}

// M1: najslabši primer (200 sej × 32 modelov z najdaljšimi imeni, ki jih JSON razširi) ostane pod mejo in se prebere;
// M2: naprava ima največ 50 sej in ne izrine drugih; L1, L3, L4.
func TestStateLimits(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	g := loadState(path)
	now := time.Now()
	g.record(sessionUse{session: "keep", device: "B", model: "m"}, now)
	for i := range 120 { // ena naprava z veliko sejami
		g.record(sessionUse{session: fmt.Sprintf("x%03d", i), device: "D0", model: "m"}, now)
	}
	if len(g.sessions("D0", "")) != maxDeviceSess || len(g.sessions("B", "")) != 1 {
		t.Fatal("one device pushed out another's session")
	}
	for i := range 200 { // najslabši primer: 4 naprave × 50 sej
		for j := range maxSessionModels + 2 {
			g.record(sessionUse{session: fmt.Sprintf("s%03d", i), device: fmt.Sprintf("D%d", i%4), folder: strings.Repeat("<", 2000),
				model: fmt.Sprintf("%02d", j) + strings.Repeat("<", 100), role: strings.Repeat("r", 40), via: strings.Repeat("<", 300),
				branch: strings.Repeat("<", 400), tools: strings.Repeat("<", 400)}, now)
		}
	}
	if n := len(g.sessions("", "")); n != maxSessions {
		t.Fatalf("sessions: %d", n)
	}
	x := g.sessions("D1", "")[0]
	if len(x.Folder) > 1024 || len(x.Models[0].Model) > 64 || len(x.Models[0].Role) > 32 || len(x.Branch) > 255 {
		t.Fatalf("not clipped by bytes: %d %d %d", len(x.Folder), len(x.Models[0].Model), len(x.Models[0].Role))
	}
	g.dirty = true
	if err := g.save(); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(path); st.Size() > maxStateFile {
		t.Fatalf("state.json %d bytes", st.Size())
	}
	if len(g.sessions("", "")) >= maxSessions {
		t.Fatal("worst case did not exceed the cap (no sessions dropped)")
	}
	if h := loadState(path); len(h.Sessions) == 0 {
		t.Fatal("state.json not read back")
	}
	if clip("a\u0085b\u202ec\u2066d\u009be", 64) != "abcde" || clip("žžž", 5) != "žž" {
		t.Fatalf("clip: %q %q", clip("a\u0085b\u202ec\u2066d\u009be", 64), clip("žžž", 5))
	}
	// L1: prazen vnos modela; L3: povezava namesto datoteke se ne bere
	os.WriteFile(path, []byte(`{"sessions":[{"id":"x","models":[null,{"model":"m","requests":1}]}]}`), 0o600)
	if h := loadState(path); len(h.sessions("", "")) != 1 || len(h.sessions("", "")[0].Models) != 1 {
		t.Fatal("nil model entry")
	}
	if runtime.GOOS != "windows" {
		os.Remove(path)
		os.Symlink("/dev/zero", path)
		if h := loadState(path); len(h.Sessions) != 0 {
			t.Fatal("read through a symlink")
		}
	}
}

// M2: mapa druge naprave z istim ID-jem seje ne opiše te seje.
func TestSessionModeOwner(t *testing.T) {
	h := newShareHub()
	if _, ok := h.register("s", "/p", "k", true, filepath.Join(t.TempDir(), "c"), "A"); !ok {
		t.Fatal("register")
	}
	if m, ro, _ := h.sessionMode("s", "A", ""); m != "sync" || !ro {
		t.Fatalf("owner: %s %v", m, ro)
	}
	if m, _, _ := h.sessionMode("s", "B", ""); m != "chat" {
		t.Fatalf("other device: %s", m)
	}
}

// Veja zahteve je veja njenega agenta (ne prve v seji); vprašani agent šteje k prvemu agentu verige.
func TestSessionModeBranchPerAgent(t *testing.T) {
	h := newShareHub()
	if _, ok := h.register("w", "/p", "k2", false, "", "A"); !ok {
		t.Fatal("register")
	}
	h.mu.Lock()
	h.bySession["w"].tree = &shareTree{slots: map[string]*wtSlot{"claude": {branch: "minigw/claude-x-0"}, "codex": {branch: "minigw/codex-x-0"}}}
	h.mu.Unlock()
	for agent, want := range map[string]string{"codex": "minigw/codex-x-0", "claude": "minigw/claude-x-0", "": "minigw/claude-x-0"} {
		if m, _, b := h.sessionMode("w", "A", agent); m != "worktree" || b != want {
			t.Errorf("%q: %s %s", agent, m, b)
		}
	}
	if agentOfModel("claude-codex-gpt-6.1-sol") != "codex" || agentOfModel("claude-cli-opus") != "claude" || agentOfModel("or-qwen") != "" {
		t.Error("agentOfModel")
	}
}

// Poročilo Windows #14, #15: naprava na loopbacku (profil 127.0.0.1) brez odobritve je »ta računalnik«; dovoljena
// naprava ima zgodovino priklopov tudi prek loopbacka.
func TestLoopbackDeviceSessionsAndHistory(t *testing.T) {
	listed := serveDevice{ID: base64url(newDevice("PC").key.Public().(ed25519.PublicKey)), Name: "PC"}
	other := serveDevice{ID: base64url(newDevice("Here").key.Public().(ed25519.PublicKey)), Name: "Here"}
	s, _ := testServer(t, func(c *serveConfig) { c.Devices = []serveDevice{listed} })
	for _, d := range []serveDevice{listed, other} {
		hr := httptest.NewRequest("POST", "/v1/messages", nil)
		hr.Header.Set("X-Claude-Mini-Session", "s-"+d.Name)
		s.recordSession(&req{Request: hr, conn: &serveConn{device: &d, peer: "127.0.0.1"}}, "m", 200, 0, 0, 0)
	}
	if l := s.state.sessions("", ""); len(l) != 2 || l[0].Device != "" || l[1].Device != listed.ID {
		t.Fatalf("sessions: %+v", l)
	}
	s.state.prune(s.conf().Devices)
	if l := s.state.sessions("", ""); l[0].DeviceRemoved || l[1].DeviceRemoved {
		t.Fatalf("removed: %+v", l)
	}
}

// #46b: števci po projektu (naprava, mapa seje, dan UTC, agent): seštevanje, nov dan, 31 dni, meja naprave in skupna,
// API /usage (najnovejši dan prvi), brisanje skupaj s sejami, CLI.
func TestProjectCounters(t *testing.T) {
	g := loadState("")
	day := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	use := func(session, device, folder, model string, status int, at time.Time) {
		g.record(sessionUse{session: session, device: device, folder: folder, os: "macOS", model: model, status: status, took: 30 * time.Second, in: 10, out: 2}, at)
	}
	use("a", "D", "/p", "claude-cli-opus", 200, day)
	use("a", "D", "/p/sub", "claude-cli-opus", 500, day)               // mapa seje ostane /p
	use("a", "D", "/p", "claude-codex", 403, day)                      // drug agent
	use("b", "D", "/p", "or-qwen", 200, day)                           // ponudnik: other
	use("a", "D", "/p", "claude-cli-opus", 200, day.Add(13*time.Hour)) // nov dan UTC
	use("c", "D", "", "claude-cli-opus", 200, day)                     // brez mape: ne šteje
	rows := g.usage("", "", 7, day.Add(13*time.Hour))
	if len(rows) != 4 || rows[0].Day != "2026-11-11" || rows[1].Day != "2026-11-10" {
		t.Fatalf("rows: %+v", rows)
	}
	c := rows[1] // 2026-11-10, /p, D, claude
	if c.Agent != "claude" || c.Requests != 2 || c.Errors != 1 || c.Seconds != 60 || c.InputTokens != 20 || c.OutputTokens != 4 || c.OS != "macOS" {
		t.Fatalf("claude row: %+v", c)
	}
	if rows[2].Agent != "codex" || rows[2].Refused != 1 || rows[3].Agent != "other" {
		t.Fatalf("agents: %+v", rows)
	}
	if n := len(g.usage("", "/P", 1, day.Add(13*time.Hour))); n != 1 { // macOS: brez razlike v velikih črkah; le danes
		t.Fatalf("folder/days filter: %d", n)
	}
	if n := len(g.usage("X", "", 7, day)); n != 0 {
		t.Fatal("device filter")
	}
	use("a", "D", "/p", "claude-cli-opus", 200, day.AddDate(0, 0, 31)) // nova vrstica: starejši od 31 dni odpadejo
	if n := len(g.Projects); n != 2 {
		t.Fatalf("retention: %d rows", n)
	}
	for i := range maxDeviceProj + 10 { // ena naprava ne izrine drugih
		use(fmt.Sprintf("x%d", i), "BIG", fmt.Sprintf("/f%d", i), "m", 200, day.AddDate(0, 0, 31))
	}
	if mine := len(g.usage("BIG", "", 31, day.AddDate(0, 0, 31))); mine != maxDeviceProj || len(g.usage("D", "", 31, day.AddDate(0, 0, 31))) != 2 {
		t.Fatalf("device cap: %d", mine)
	}
	for i := range maxProjects + 10 { // skupna meja: najstarejše odpadejo
		use(fmt.Sprintf("y%d", i), fmt.Sprintf("E%d", i%8), fmt.Sprintf("/g%d", i), "m", 200, day.AddDate(0, 0, 31))
	}
	if n := len(g.Projects); n != maxProjects {
		t.Fatalf("total cap: %d", n)
	}
	g.clearSessions()
	if len(g.Projects) != 0 || len(g.Sessions) != 0 {
		t.Fatal("clear kept the counters")
	}
	if usageLine(rows[1]) != `2026-11-10  "/p"  D  claude: 2 requests, 1 errors, 0 refused, 1.0 agent minutes, 20 input and 4 output tokens` {
		t.Fatalf("usageLine: %q", usageLine(rows[1]))
	}

	s, cfg, base := liveServer(t, nil)
	s.state.record(sessionUse{session: "u", folder: "/proj", model: "claude-codex", status: 200, took: time.Minute, in: 5, out: 6}, time.Now())
	if st, b := adminNoise(t, s, base, s.admin, "GET", adminPrefix+"/usage?days=1&folder=/proj", ""); st != 200 ||
		!strings.Contains(b, `"folder":"/proj"`) || !strings.Contains(b, `"agent":"codex"`) || !strings.Contains(b, `"requests":1`) || strings.Contains(b, `"device"`) {
		t.Fatalf("usage API: %d %s", st, b)
	}
	if st, b := adminNoise(t, s, base, s.admin, "GET", adminPrefix+"/usage?days=32", ""); st != 400 || !strings.Contains(b, "days: 1-31") {
		t.Fatalf("days=32: %d %s", st, b)
	}
	out := stdout(t, func() { routerCommand([]string{"usage", "--days", "2", "--config", cfg}, strings.NewReader("")) })
	if !strings.Contains(out, `"/proj"  this computer  codex: 1 requests`) {
		t.Fatalf("cli usage: %q", out)
	}
	if st, _ := adminNoise(t, s, base, s.admin, "DELETE", adminPrefix+"/sessions", ""); st != 200 || len(s.state.usage("", "", 31, time.Now())) != 0 {
		t.Fatal("DELETE /sessions kept the counters")
	}
}

// M1: najslabši primer števcev (vsa mesta z najdaljšimi mapami) ostane pod mejo datoteke in se prebere.
func TestProjectCountersFileLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	g := loadState(path)
	now := time.Now()
	for i := range maxProjects {
		g.record(sessionUse{session: fmt.Sprintf("s%d", i), device: fmt.Sprintf("D%d", i%8), folder: fmt.Sprintf("/%d", i) + strings.Repeat("<", 2000), model: "m"}, now)
	}
	if len(g.Projects) != maxProjects {
		t.Fatalf("rows: %d", len(g.Projects))
	}
	if err := g.save(); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(path); st.Size() > maxStateFile || len(g.Projects) == maxProjects {
		t.Fatalf("state.json %d bytes, %d rows", st.Size(), len(g.Projects))
	}
	if h := loadState(path); len(h.Projects) == 0 || len(h.Projects[0].Folder) > maxProjectFolder { // L2: ključ mape ≤ 256 B
		t.Fatal("state.json not read back")
	}
}

// L2: pri meji datoteke najprej odpadejo vrstice naprave z največ vrsticami (najstarejše).
func TestTrimBusiest(t *testing.T) {
	rows := []*projectUse{{Device: "A", Day: "1"}, {Device: "B", Day: "1"}, {Device: "B", Day: "2"}, {Device: "A", Day: "2"}, {Device: "B", Day: "3"}}
	got := trimBusiest(rows)
	if len(got) != 4 || got[1].Device != "B" || got[1].Day != "2" || got[0].Device != "A" {
		t.Fatalf("%+v", got)
	}
}

// Parity: isti state.json (testdata, tudi v HistoryTests.swift) prebereta oba prehoda.
func TestStateParitySample(t *testing.T) {
	g := loadState(filepath.Join("testdata", "state-0.5.21.json"))
	if len(g.Sessions) != 1 || len(g.Projects) != 2 || g.Sessions[0].Models[0].Cap != "worktree" || !g.Sessions[0].Models[0].ReadOnly {
		t.Fatalf("sample: %+v %+v", g.Sessions, g.Projects)
	}
	now := time.Date(2026, 11, 10, 18, 0, 0, 0, time.UTC)
	if u := g.usage("", "c:/PROJ", 7, now); len(u) != 1 || u[0].Agent != "codex" || u[0].Seconds != 42.5 || u[0].Refused != 1 {
		t.Fatalf("usage: %+v", u)
	}
	if u := g.usage("", "", 2, now); len(u) != 2 || u[0].Day != "2026-11-10" || u[1].Device != "" {
		t.Fatalf("usage: %+v", u)
	}
}
