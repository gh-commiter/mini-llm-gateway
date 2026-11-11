package main

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// pipeListener: povezave net.Pipe (oddaljen naslov »pipe« = ni loopback).
type pipeListener struct{ ch chan net.Conn }

func (l pipeListener) Accept() (net.Conn, error) {
	c, ok := <-l.ch
	if !ok {
		return nil, net.ErrClosed
	}
	return c, nil
}
func (l pipeListener) Close() error   { return nil }
func (l pipeListener) Addr() net.Addr { return &net.TCPAddr{} }

// anyClaude: obstoječa izvršljiva pot za serveConfig.Claude, ki je test ne požene (Windows: le .exe, brez /bin/sh).
func anyClaude(t *testing.T) string {
	if runtime.GOOS == "windows" {
		exe, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		return exe
	}
	return "/bin/sh"
}

func testServer(t *testing.T, mod func(*serveConfig)) (*server, *ecdh.PrivateKey) {
	t.Helper()
	k, _ := ecdh.X25519().GenerateKey(rand.Reader)
	c := serveConfig{Token: "tok", Key: base64url(k.Bytes()), Tools: defaultTools, PermissionMode: "dontAsk", Scratch: t.TempDir()}
	t.Cleanup(releaseScratch) // Windows: ročaj Scratch bi preprečil brisanje TempDir
	if mod != nil {
		mod(&c)
	}
	s, err := newServer(c, filepath.Join(t.TempDir(), "serve.json"))
	if err != nil {
		t.Fatal(err)
	}
	s.log.SetOutput(io.Discard)
	return s, k
}

// pipeRequest: zahteva prek obstoječega pobudnika Noise (kot noiseProxy), odgovor dešifriran.
func pipeRequest(t *testing.T, s *server, routerKey []byte, dev *device, request string) string {
	t.Helper()
	l := pipeListener{make(chan net.Conn, 1)}
	go s.serve(l)
	defer close(l.ch)
	a, b := net.Pipe()
	l.ch <- b
	a.SetDeadline(time.Now().Add(10 * time.Second))
	defer a.Close()
	hs, _ := newInitiator(routerKey)
	first, _ := hs.first()
	if _, err := a.Write(append(append([]byte{}, noiseMagic...), first...)); err != nil {
		return ""
	}
	reply := make([]byte, handshakeLen)
	if _, err := io.ReadFull(a, reply); err != nil {
		return "" // router je zaprl
	}
	sess, err := hs.finish(reply)
	if err != nil {
		t.Fatal(err)
	}
	out := []byte{}
	if dev != nil {
		out = frames(deviceProof(dev.key, dev.name, sess.hash), sess.send)
	}
	go a.Write(append(out, frames([]byte(request), sess.send)...))
	raw, _ := io.ReadAll(a)
	plain, _, err := unframe(raw, sess.receive)
	if err != nil {
		t.Fatal(err)
	}
	return string(plain)
}

func newDevice(name string) *device {
	_, k, _ := ed25519.GenerateKey(rand.Reader)
	return &device{key: k, name: name}
}

func TestServeNoiseHandshakeTokenAndDevices(t *testing.T) {
	mac := newDevice("MacBook")
	macID := base64url(mac.key.Public().(ed25519.PublicKey))
	get := func(tok string) string {
		return "GET /v1/models HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer " + tok + "\r\n\r\n"
	}

	s, k := testServer(t, func(c *serveConfig) { c.Devices = []serveDevice{{ID: macID, Name: "MacBook"}} })
	pub := k.PublicKey().Bytes()
	if r := pipeRequest(t, s, pub, mac, get("tok")); !strings.HasPrefix(r, "HTTP/1.1 200") || !strings.Contains(r, `"data"`) {
		t.Fatalf("allowed device: %q", r)
	}
	if r := pipeRequest(t, s, pub, mac, get("wrong")); !strings.HasPrefix(r, "HTTP/1.1 401") {
		t.Fatalf("wrong token: %q", r)
	}
	if r := pipeRequest(t, s, pub, newDevice("Other"), get("tok")); !strings.HasPrefix(r, "HTTP/1.1 403") || !strings.Contains(r, "Settings → Devices") || !strings.Contains(r, "minigw gw devices allow") {
		t.Fatalf("unknown device: %q", r)
	}
	if r := pipeRequest(t, s, pub, nil, get("tok")); !strings.HasPrefix(r, "HTTP/1.1 403") {
		t.Fatalf("no device proof: %q", r)
	}
	other, _ := ecdh.X25519().GenerateKey(rand.Reader) // napačen ključ routerja: brez odgovora
	if r := pipeRequest(t, s, other.PublicKey().Bytes(), mac, get("tok")); r != "" {
		t.Fatalf("wrong router key answered: %q", r)
	}

	// Prazen seznam: neznana naprava le z allowAnyDevice.
	s, k = testServer(t, nil)
	if r := pipeRequest(t, s, k.PublicKey().Bytes(), mac, get("tok")); !strings.HasPrefix(r, "HTTP/1.1 403") {
		t.Fatalf("empty list: %q", r)
	}
	s, k = testServer(t, func(c *serveConfig) { c.AllowAnyDevice = true })
	if r := pipeRequest(t, s, k.PublicKey().Bytes(), mac, get("tok")); !strings.HasPrefix(r, "HTTP/1.1 200") {
		t.Fatalf("allowAnyDevice: %q", r)
	}
}

func TestServePlainOnlyFromLoopback(t *testing.T) {
	s, _ := testServer(t, nil)
	l := pipeListener{make(chan net.Conn, 1)}
	go s.serve(l)
	defer close(l.ch)
	a, b := net.Pipe()
	l.ch <- b
	a.SetDeadline(time.Now().Add(5 * time.Second))
	go a.Write([]byte("GET /v1/models HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer tok\r\n\r\n"))
	r, _ := io.ReadAll(a)
	if !strings.HasPrefix(string(r), "HTTP/1.1 426") {
		t.Fatalf("plain from network: %q", r)
	}
}

// startLoopback: router na 127.0.0.1 (navaden HTTP, kot lokalni minigw).
func startLoopback(t *testing.T, s *server) connection {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go s.serve(ln)
	t.Cleanup(func() { ln.Close() })
	return connection{URL: "http://" + ln.Addr().String(), Token: "tok"}
}

// N9: vsak claude -p prehoda (tudi samo za branje, pogovor, worktree, vprašani agent) brez »@pot« in ukazov s
// poševnico; ponaredek brez obeh konča brez odgovora.
const cliGuard = `[ "$CLAUDE_CODE_DISABLE_FILE_MENTIONS" = 1 ] || exit 9
case " $* " in *" --disable-slash-commands "*) ;; *) exit 9 ;; esac`

func TestServeSyncAgentRoundTrip(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake claude is a shell script")
	}
	tmp := t.TempDir()
	fake := filepath.Join(tmp, "claude")
	argsFile := filepath.Join(tmp, "args")
	envFile := filepath.Join(tmp, "env")
	for _, k := range []string{"GH_TOKEN", "GITHUB_TOKEN", "COPILOT_GITHUB_TOKEN", "SSH_AUTH_SOCK"} { // M1: claude dobi agentEnv
		t.Setenv(k, "leak")
	}
	os.WriteFile(fake, []byte(`#!/bin/sh
printf '%s\n' "$@" > "`+argsFile+`"
env > "`+envFile+`"
`+cliGuard+`
cat > /dev/null
cat src.txt > agent.txt
echo '{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Write","input":{"file_path":"agent.txt"}}]}}'
echo '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"done"}}}'
echo '{"type":"result","subtype":"success","usage":{"output_tokens":3}}'
`), 0o755)
	s, _ := testServer(t, func(c *serveConfig) { c.Claude = fake })
	c := startLoopback(t, s)

	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "src.txt"), []byte("from client\n"), 0o644)
	sc := &shareClient{c: c, session: "sid1", root: dir, sync: true, synced: map[string]syncEntry{}, reported: map[string]bool{}}
	if !sc.register() {
		t.Fatal("register failed: " + sc.refusal)
	}
	sc.start(2)
	defer sc.unregister()

	body := `{"model":"claude-cli-haiku","tools":[{"name":"x"}],"messages":[{"role":"user","content":"go"}]}`
	h := map[string]string{"x-claude-mini-session": "sid1", "x-claude-mini-cwd": dir, "content-type": "application/json"}
	st, data := call(c, "POST", "/v1/messages", h, []byte(body), 30*time.Second)
	if st != 200 || !strings.Contains(string(data), `"done"`) {
		t.Fatalf("messages: %d %s", st, data)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "agent.txt")); string(b) != "from client\n" {
		t.Fatalf("agent change not synced back: %q", b)
	}
	args, _ := os.ReadFile(argsFile)
	if !strings.Contains(string(args), "--setting-sources\n\n") || !strings.Contains(string(args), "--session-id") {
		t.Fatalf("args: %q", args)
	}
	if env, _ := os.ReadFile(envFile); len(env) == 0 || strings.Contains(string(env), "=leak") {
		t.Fatalf("claude env: %q", env)
	}

	// Streaming: SSE z message_stop; druga zahteva iste zgodovine nadaljuje sejo (--resume).
	body = `{"model":"claude-cli-haiku","stream":true,"tools":[{"name":"x"}],"messages":[{"role":"user","content":"go"},{"role":"assistant","content":"done"},{"role":"user","content":"again"}]}`
	st, data = call(c, "POST", "/v1/messages", h, []byte(body), 30*time.Second)
	if st != 200 || !strings.Contains(string(data), "event: message_stop") {
		t.Fatalf("stream: %d %s", st, data)
	}
	if args, _ = os.ReadFile(argsFile); !strings.Contains(string(args), "--resume") {
		t.Fatalf("not resumed: %q", args)
	}
	// N9: pogovor brez orodij in mapa samo za branje: claude -p prav tako s cliGuard
	if st, data := call(c, "POST", "/v1/messages", h, []byte(`{"model":"claude-cli-haiku","messages":[{"role":"user","content":"title"}]}`), 30*time.Second); st != 200 {
		t.Fatalf("chat: %d %s", st, data)
	}
	ro := &shareClient{c: c, session: "sid9", root: t.TempDir(), sync: true, readOnly: true, synced: map[string]syncEntry{}, reported: map[string]bool{}}
	if !ro.register() {
		t.Fatal("read-only register failed: " + ro.refusal)
	}
	ro.start(2)
	defer ro.unregister()
	hro := map[string]string{"x-claude-mini-session": "sid9", "x-claude-mini-cwd": ro.root, "x-claude-mini-ro": "1", "content-type": "application/json"}
	if st, data := call(c, "POST", "/v1/messages", hro, []byte(`{"model":"claude-cli-haiku","tools":[{"name":"x"}],"messages":[{"role":"user","content":"go"}]}`), 30*time.Second); st != 200 {
		t.Fatalf("read-only: %d %s", st, data)
	}

	// Pobeg iz kopije: PUT zunaj manifesta in pot z »..« → 403; branje → 404.
	for _, p := range []string{"../evil.txt", "src.txt/../../evil.txt", "a/../../evil.txt"} {
		if st, _ := call(c, "PUT", "/minigw/sync/file", map[string]string{"x-share-key": sc.currentKey(), "x-sync-path": p}, []byte("x"), 5*time.Second); st != 403 {
			t.Fatalf("PUT %s: %d", p, st)
		}
		if st, _ := call(c, "GET", "/minigw/sync/file", map[string]string{"x-share-key": sc.currentKey(), "x-sync-path": p}, nil, 5*time.Second); st != 404 {
			t.Fatalf("GET %s: %d", p, st)
		}
	}
	// Manifest z »..« in ».GIT« → router teh poti ne zahteva; PUT nanje 403.
	m, _ := json.Marshal([]syncEntry{{Path: "../evil.txt", SHA256: sha256Hex([]byte("x"))}, {Path: ".GIT/hooks/pre-commit", SHA256: sha256Hex([]byte("x"))}})
	st, data = call(c, "POST", "/minigw/sync/manifest", map[string]string{"x-share-key": sc.currentKey()}, m, 5*time.Second)
	if st != 200 || strings.Contains(string(data), "evil") || strings.Contains(string(data), "GIT") {
		t.Fatalf("manifest: %d %s", st, data)
	}
	if st, _ := call(c, "PUT", "/minigw/sync/file", map[string]string{"x-share-key": sc.currentKey(), "x-sync-path": ".GIT/hooks/pre-commit"}, []byte("x"), 5*time.Second); st != 403 {
		t.Fatalf("PUT .GIT: %d", st)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(s.cfg.scratch("sync")), "evil.txt")); err == nil {
		t.Fatal("file escaped the copy")
	}
}

func TestServeRefusesDirectMode(t *testing.T) {
	s, _ := testServer(t, func(c *serveConfig) { c.Claude = anyClaude(t) })
	c := startLoopback(t, s)
	sc := &shareClient{c: c, session: "sid2", root: t.TempDir(), synced: map[string]syncEntry{}, reported: map[string]bool{}}
	if sc.register() || !strings.Contains(sc.refusal, "--minigw-work=sync and worktree") {
		t.Fatalf("direct mode accepted: %q", sc.refusal)
	}
	// Način ni izbran: odjemalec po zavrnitvi direct sam izbere sync (mapa ni repozitorij git); izrecni direct ne.
	if sc, ok := registerShare(c, "sid4", t.TempDir(), false, "", []string{"direct"}, true, true); !ok || !sc.sync {
		t.Fatalf("implicit mode not switched to sync: %v %q", ok, sc.refusal)
	} else {
		sc.unregister()
	}
	if _, ok := registerShare(c, "sid5", t.TempDir(), false, "", []string{"direct"}, true, false); ok {
		t.Fatal("explicit direct accepted")
	}
	// --minigw-ro --minigw-work=worktree: workMode da direct, prehod ga zavrne → sync samo za branje (ne 400)
	if w, ro := workMode("worktree", nil, true); !workImplicit("worktree", w) || workImplicit("direct", "direct") {
		t.Fatal("read-only worktree must fall back like auto, explicit direct must not")
	} else if sc, ok := registerShare(c, "sid7", t.TempDir(), ro, "", workCandidates(w, ro, false, false), true, true); !ok || !sc.sync || !sc.readOnly {
		t.Fatalf("read-only worktree not switched to a read-only sync: %v %q", ok, sc.refusal)
	} else {
		sc.unregister()
	}
	// M1: repozitorij v nadrejeni mapi (dotfiles v domači mapi, monorepo) → sync, ne worktree; koren sam → worktree.
	repo := realOf(t, t.TempDir()) // macOS: /var → /private/var, kot ga vrne git
	writeT(t, filepath.Join(repo, "sub", "a.txt"), "a\n")
	gitT(t, repo, "init", "-q", "-b", "main")
	gitT(t, repo, "add", "-A")
	gitT(t, repo, "commit", "-qm", "first")
	for i, c2 := range []struct {
		dir      string
		worktree bool
	}{{filepath.Join(repo, "sub"), false}, {repo, true}} {
		sc, ok := registerShare(c, fmt.Sprintf("sid6-%d", i), c2.dir, false, "", []string{"direct"}, true, true)
		if !ok || sc.worktree != c2.worktree || sc.sync == c2.worktree {
			t.Fatalf("%s: ok=%v worktree=%v sync=%v %q", c2.dir, ok, sc.worktree, sc.sync, sc.refusal)
		}
		sc.unregister()
	}
	body := `{"model":"claude-cli-haiku","tools":[{"name":"x"}],"messages":[{"role":"user","content":"go"}]}`
	st, data := call(c, "POST", "/v1/messages", map[string]string{"x-claude-mini-session": "sid2", "x-claude-mini-share": "1"}, []byte(body), 5*time.Second)
	if st != 400 || !strings.Contains(string(data), "sync and worktree") {
		t.Fatalf("agent without sync: %d %s", st, data)
	}
	// Brez mape (ni glave share): sporočilo o deljenju, ne o sync.
	st, data = call(c, "POST", "/v1/messages", map[string]string{"x-claude-mini-session": "sid3", "x-claude-mini-cwd": "/tmp/x"}, []byte(body), 5*time.Second)
	if st != 400 || !strings.Contains(string(data), "/tmp/x is not shared") || !strings.Contains(string(data), "minigw share add") || strings.Contains(string(data), "sync and worktree") {
		t.Fatalf("agent without share: %d %s", st, data)
	}
}

func TestServeRejectsBadModelsBodiesAndForeignOwners(t *testing.T) {
	s, _ := testServer(t, func(c *serveConfig) { c.Claude = anyClaude(t) })
	c := startLoopback(t, s)
	for _, m := range []string{`claude-cli-opus\" & calc & \"`, "claude-cli-opus x", "Claude-cli-opus", "claude-cli-" + strings.Repeat("a", 70)} {
		body, _ := json.Marshal(map[string]any{"model": m, "messages": []any{map[string]string{"role": "user", "content": "x"}}})
		if st, _ := call(c, "POST", "/v1/messages", nil, body, 5*time.Second); st != 400 {
			t.Fatalf("model %q: %d", m, st)
		}
	}
	big := make([]byte, 2<<20)
	if st, _ := call(c, "POST", "/minigw/sync/ack", nil, big, 5*time.Second); st != 413 {
		t.Fatalf("2 MB ack: %d", st)
	}
	if st, _ := call(connection{URL: c.URL, Token: "wrong"}, "PUT", "/minigw/sync/file", nil, big, 5*time.Second); st != 401 {
		t.Fatalf("auth before body: %d", st)
	}

	// Mapa lastnika »local« (loopback) ni dosegljiva brez naprave ali z drugo napravo.
	k, _ := s.hub.register("sid", "/x", "", false, filepath.Join(t.TempDir(), "copy"), "local")
	for _, owner := range []string{"", "other-device"} {
		if _, ok := s.hub.poll(t.Context(), k, owner); ok {
			t.Fatalf("poll as %q", owner)
		}
		if _, _, ok := s.hub.syncManifest(k, nil, true, owner); ok {
			t.Fatalf("manifest as %q", owner)
		}
		if s.hub.waitShare(t.Context(), "sid", owner, false) != nil {
			t.Fatalf("waitShare as %q", owner)
		}
	}
	if s.cfg.deviceAllowed(nil, false) {
		t.Fatal("remote client without a device key allowed")
	}

	// Orodja agenta le v delovni mapi; mapa nastavitev prepovedana.
	args := strings.Join(s.agentArgs("/copy", false), " ")
	for _, want := range []string{"Read(./**)", "Edit(./**)", "Write(./**)", "Grep(./**)", "Read(" + ruleAbs(s.cfgDir) + "/**)"} {
		if !strings.Contains(args, want) {
			t.Fatalf("agent args lack %s: %s", want, args)
		}
	}
	// M4: .git metapodatki niso pisljivi (Edit/Write tudi na Windows; sandbox drugje), kot ClaudeCLI.gitDenies.
	for _, g := range gitDenies {
		want := []string{"Edit(//copy/.git/" + g + ")", "Write(//copy/.git/" + g + "/**)"}
		if runtime.GOOS != "windows" {
			want = append(want, `"/copy/.git/`+g+`"`)
		}
		for _, w := range want {
			if !strings.Contains(args, w) {
				t.Fatalf("agent args lack %s: %s", w, args)
			}
		}
	}
	// --minigw-ro: brez orodij za pisanje, sandbox ne piše v mapo (na Windows brez sandboxa tudi brez Bash).
	s.cfg.Tools = defaultTools + ",Monitor,EnterWorktree"
	roArgs := s.agentArgs("/copy", true)
	s.cfg.Tools = defaultTools
	if i := slices.Index(roArgs, "--tools"); i < 0 || roArgs[i+1] != "Read,Grep,Glob" {
		t.Fatalf("read-only tools not from the allowlist: %v", roArgs)
	}
	ro := strings.Join(roArgs, " ")
	s.cfg.AllowLocalTools = true
	local := s.agentArgs("/copy", true)
	s.cfg.AllowLocalTools = false
	if i := slices.Index(local, "--tools"); i < 0 || strings.Contains(local[i+1], "Monitor") || strings.Contains(local[i+1], "Edit") ||
		!strings.Contains(strings.Join(local, " "), "EnterWorktree,ExitWorktree,Monitor") {
		t.Fatalf("read-only local tools: %v", local)
	}
	if strings.Contains(ro, "Edit(./**)") || strings.Contains(ro, "Write(./**)") || !strings.Contains(ro, "Read(./**)") ||
		runtime.GOOS != "windows" && (!strings.Contains(ro, `"allowWrite":[]`) || !strings.Contains(ro, `"/copy"]}`)) {
		t.Fatalf("read-only agent args: %s", ro)
	}
	if ruleAbs(`C:\Users\me`) != "//c/Users/me" && runtime.GOOS == "windows" {
		t.Fatal(ruleAbs(`C:\Users\me`))
	}
}

// Z resničnim claude (MINIGW_REAL_CLAUDE=1, en kratek klic haiku): agent bere v kopiji, ne pa nastavitev ali zunaj nje.
func TestServeRealClaudeStaysInCopy(t *testing.T) {
	exe, _ := resolveClaude("")
	if os.Getenv("MINIGW_REAL_CLAUDE") != "1" || exe == nil {
		t.Skip("set MINIGW_REAL_CLAUDE=1 with Claude Code installed")
	}
	base, _ := filepath.EvalSymlinks(t.TempDir())
	t.Cleanup(releaseScratch) // Windows: ročaj Scratch bi preprečil brisanje TempDir
	cfgPath := filepath.Join(base, "cfg", "serve.json")
	os.MkdirAll(filepath.Dir(cfgPath), 0o700)
	os.WriteFile(cfgPath, []byte(`{"token":"CONFIG-SECRET-7731"}`), 0o600)
	os.WriteFile(filepath.Join(base, "outside.txt"), []byte("OUTSIDE-SECRET-4410"), 0o600)
	k, _ := ecdh.X25519().GenerateKey(rand.Reader)
	s, err := newServer(serveConfig{Token: "tok", Key: base64url(k.Bytes()), Tools: defaultTools, PermissionMode: "dontAsk",
		Scratch: filepath.Join(base, "scratch")}, cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	c := startLoopback(t, s)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "note.txt"), []byte("INSIDE-OK-5521"), 0o644)
	sc := &shareClient{c: c, session: newUUID(), root: dir, sync: true, synced: map[string]syncEntry{}, reported: map[string]bool{}}
	if !sc.register() {
		t.Fatal(sc.refusal)
	}
	sc.start(2)
	defer sc.unregister()
	prompt := "Use the Read tool on each of these files and print each file's exact contents (or the exact error): " +
		"note.txt, " + cfgPath + ", ../../../../cfg/serve.json, " + filepath.Join(base, "outside.txt") + ", ../../../../outside.txt"
	body, _ := json.Marshal(map[string]any{"model": "claude-cli-haiku", "tools": []any{map[string]string{"name": "x"}},
		"messages": []any{map[string]string{"role": "user", "content": prompt}}})
	st, data := call(c, "POST", "/v1/messages", map[string]string{"x-claude-mini-session": sc.session, "x-claude-mini-cwd": dir}, body, 300*time.Second)
	t.Logf("%d %s", st, data)
	if st != 200 || !strings.Contains(string(data), "INSIDE-OK-5521") {
		t.Fatal("agent could not read inside the copy")
	}
	if strings.Contains(string(data), "CONFIG-SECRET") || strings.Contains(string(data), "OUTSIDE-SECRET") {
		t.Fatal("agent read outside the copy")
	}
}

// Starejši »folder« (privzeta mapa agentov) postane dovoljena mapa in se ne zapiše več.
func TestServeOldFolderMigrates(t *testing.T) {
	p := filepath.Join(t.TempDir(), "serve.json")
	os.WriteFile(p, []byte(`{"port":8765,"folder":"/srv/old","allowedFolders":["/srv/a"]}`), 0o600)
	c, err := loadServeConfig(p)
	if err != nil || !slices.Equal(c.AllowedFolders, []string{"/srv/a", "/srv/old"}) {
		t.Fatalf("%v %v", c.AllowedFolders, err)
	}
	saveServeConfig(p, c)
	if b, _ := os.ReadFile(p); strings.Contains(string(b), `"folder"`) {
		t.Fatalf("folder written back: %s", b)
	}
	if c, _ = loadServeConfig(p); len(c.AllowedFolders) != 2 {
		t.Fatalf("migrated twice: %v", c.AllowedFolders)
	}
	// zastareli --folder: doda dovoljeno mapo (enkrat)
	dir := t.TempDir()
	deprecatedFolder(&c, dir)
	deprecatedFolder(&c, dir)
	if len(c.AllowedFolders) != 3 || c.AllowedFolders[2] != dir {
		t.Fatalf("--folder: %v", c.AllowedFolders)
	}
}

func TestServeFolderNotNearConfig(t *testing.T) {
	k, _ := ecdh.X25519().GenerateKey(rand.Reader)
	base := t.TempDir()
	cfgPath := filepath.Join(base, "cfg", "serve.json")
	for _, d := range []string{"cfg/x", "work"} {
		os.MkdirAll(filepath.Join(base, d), 0o700)
	}
	t.Cleanup(releaseScratch)
	scratch := filepath.Join(t.TempDir(), "scratch") // ne v base: dovoljena mapa okoli Scratch se ob zagonu le izpusti
	for folder, bad := range map[string]bool{base: true, filepath.Join(base, "cfg"): true, filepath.Join(base, "cfg", "x"): true, filepath.Join(base, "work"): false} {
		_, err := newServer(serveConfig{Token: "tok", Key: base64url(k.Bytes()), Claude: anyClaude(t), Scratch: scratch, AllowedFolders: []string{folder}}, cfgPath)
		if (err != nil) != bad {
			t.Fatalf("folder %s: %v", folder, err)
		}
	}
}

func TestServeAllowDenyDevices(t *testing.T) {
	id := base64url(newDevice("Mac").key.Public().(ed25519.PublicKey))
	for _, bad := range []string{"203.0.113.5", "", id[:42], id + "A", strings.Replace(id, id[:1], "+", 1), base64url(make([]byte, 31))} {
		if validDeviceID(bad) {
			t.Errorf("accepted %q", bad)
		}
	}
	if !validDeviceID(id) {
		t.Fatalf("refused %q", id)
	}
	cfg := filepath.Join(t.TempDir(), "serve.json")
	saveServeConfig(cfg, serveConfig{Token: "tok", Key: "k", Port: freePort(t)})
	other := base64url(newDevice("PC").key.Public().(ed25519.PublicKey))
	serveCommand([]string{"allow", id, "Mac", "Book", "--config", cfg})
	serveCommand([]string{"allow", other, "PC", "--config", cfg})
	serveCommand([]string{"deny", id, "--config", cfg}) // le po ID-ju (ime je oznaka)
	c, _ := loadServeConfig(cfg)
	if len(c.Devices) != 1 || c.Devices[0].ID != other {
		t.Fatalf("after deny: %+v", c.Devices)
	}
	serveCommand([]string{"deny", other, "--config", cfg}) // po ID
	serveCommand([]string{"devices", "--config", cfg})
	if c, _ = loadServeConfig(cfg); len(c.Devices) != 0 {
		t.Fatalf("after deny by ID: %+v", c.Devices)
	}
}

// M2: velikost iz manifesta je zavezujoča (kvota); največ maxCopies kopij na napravo.
func TestSyncPutSizeAndCopyLimit(t *testing.T) {
	h := newShareHub()
	copyDir := t.TempDir()
	key, _ := h.register("s1", "/c", "", false, copyDir, "dev")
	data := []byte("hello")
	if _, _, ok := h.syncManifest(key, []syncEntry{{Path: "a.txt", Size: 0, SHA256: sha256Hex(data)}}, true, "dev"); !ok {
		t.Fatal("manifest")
	}
	if st := h.syncPut(key, "a.txt", data, "dev"); st != 409 {
		t.Fatalf("size mismatch: %d", st)
	}
	root := t.TempDir()
	for i := range maxCopies {
		if tooManyCopies(filepath.Join(root, strconv.Itoa(i))) {
			t.Fatalf("refused copy %d", i)
		}
		os.Mkdir(filepath.Join(root, strconv.Itoa(i)), 0o700)
		os.WriteFile(filepath.Join(root, strconv.Itoa(i)+".used"), nil, 0o600)
	}
	if !tooManyCopies(filepath.Join(root, "new")) || tooManyCopies(filepath.Join(root, "0")) {
		t.Fatal("copy limit")
	}
}

// #21: pisanje prenesene datoteke ne drži skupnega zaklepa (počasen prenos ne ustavi drugih map).
func TestSyncPutWritesWithoutTheHubLock(t *testing.T) {
	h := newShareHub()
	copyDir := t.TempDir()
	key, ok := h.register("s1", "/c", "", false, copyDir, "dev")
	if !ok {
		t.Fatal("register")
	}
	data := []byte("hello")
	if _, _, ok := h.syncManifest(key, []syncEntry{{Path: "a.txt", Size: 5, SHA256: sha256Hex(data)}}, true, "dev"); !ok {
		t.Fatal("manifest")
	}
	entered, release := make(chan struct{}), make(chan struct{})
	o := syncPutWrite
	t.Cleanup(func() { syncPutWrite = o })
	syncPutWrite = func(full string, d []byte, m int64, x, k bool) error {
		close(entered)
		<-release
		return o(full, d, m, x, k)
	}
	done := make(chan int)
	go func() { done <- h.syncPut(key, "a.txt", data, "dev") }()
	<-entered
	if !h.mu.TryLock() {
		t.Fatal("hub lock held while writing")
	}
	h.mu.Unlock()
	close(release)
	if st := <-done; st != 204 {
		t.Fatalf("put: %d", st)
	}
	if b, _ := os.ReadFile(filepath.Join(copyDir, "a.txt")); string(b) != "hello" {
		t.Fatalf("copy: %q", b)
	}
}

// #1, #9: kopije, neuporabljene 14 dni, se izbrišejo (ne dejavne, ne runs); manifest nad mejo je zavrnjen.
func TestSyncCopiesExpireAndQuota(t *testing.T) {
	root := t.TempDir()
	old := time.Now().Add(-15 * 24 * time.Hour)
	for _, p := range []string{"dev/old", "dev/fresh", "dev/active", "runs/clone"} {
		os.MkdirAll(filepath.Join(root, p), 0o700)
	}
	for _, p := range []string{"dev/old", "dev/active", "runs/clone"} {
		os.Chtimes(filepath.Join(root, p), old, old)
	}
	touchUsed(filepath.Join(root, "dev", "fresh"))
	os.WriteFile(filepath.Join(root, "dev", "old.used"), nil, 0o600)
	os.Chtimes(filepath.Join(root, "dev", "old.used"), old, old)
	removeOldCopies(root, map[string]bool{filepath.Join(root, "dev", "active"): true}, time.Now())
	if _, err := os.Stat(filepath.Join(root, "dev", "old")); err == nil {
		t.Fatal("old copy kept")
	}
	for _, p := range []string{"dev/fresh", "dev/active", "runs/clone"} {
		if _, err := os.Stat(filepath.Join(root, p)); err != nil {
			t.Fatalf("%s removed", p)
		}
	}
	if syncOverQuota([]syncEntry{{Path: "a", Size: 10}}) != "" || !strings.Contains(syncOverQuota([]syncEntry{{Path: "b", Size: maxSyncBytes + 1}}), "too large") {
		t.Fatal("quota")
	}
}

// #4: glava SSE in komentarji pred prenosom mape; napaka po tem kot dogodek SSE.
func TestEarlyStreamWriter(t *testing.T) {
	o := earlyInterval
	t.Cleanup(func() { earlyInterval = o })
	earlyInterval = 20 * time.Millisecond
	rec := httptest.NewRecorder()
	e := newEarlyWriter(rec, "r1")
	time.Sleep(70 * time.Millisecond)
	writeError(e, "r1", 502, "Syncing /p to the gateway failed")
	e.done()
	b := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(b, ": syncing the folder") || !strings.Contains(b, ": keep-alive") ||
		!strings.Contains(b, "event: error") || !strings.Contains(b, "Syncing /p to the gateway failed") {
		t.Fatalf("early stream: %d %q", rec.Code, b)
	}
	rec2 := httptest.NewRecorder()
	e2 := newEarlyWriter(rec2, "r2")
	e2.Header().Set("Content-Type", "text/event-stream")
	e2.WriteHeader(200)
	e2.Write([]byte("event: message_start\ndata: {}\n\n"))
	if !strings.Contains(rec2.Body.String(), "event: message_start") || strings.Contains(rec2.Body.String(), "event: error") {
		t.Fatalf("pass-through: %q", rec2.Body.String())
	}
}

// H1, M1, L1: odjemalec brez ključa le pri praznem seznamu z allowAnyDevice; ime očiščeno; poln seznam čakajočih obdrži
// najstarejše.
func TestDeviceRulesLikeMac(t *testing.T) {
	listed := serveConfig{Devices: []serveDevice{{ID: "a"}}, AllowAnyDevice: true}
	if listed.deviceAllowed(nil, false) || !(serveConfig{AllowAnyDevice: true}).deviceAllowed(nil, false) || (serveConfig{}).deviceAllowed(nil, false) {
		t.Fatal("keyless remote client")
	}
	if got := cleanDeviceName("  PC\n192.168.1.20 · x\u202e\u200b\t "); got != "PC192.168.1.20 · x" {
		t.Fatalf("clean: %q", got)
	}
	if got := cleanDeviceName("PC\u202810.0.0.1\u2029x"); got != "PC10.0.0.1x" { // L2
		t.Fatalf("Zl/Zp: %q", got)
	}
	if got := cleanDeviceName(strings.Repeat("ž", 100)); len([]rune(got)) != 64 {
		t.Fatalf("cap: %d", len([]rune(got)))
	}
	s := &server{}
	for i := 0; i < 25; i++ {
		s.addPending(serveDevice{ID: fmt.Sprint("id", i)}, "a", "")
	}
	s.addPending(serveDevice{ID: "id0"}, "b", "Linux, minigw 0.5.18")
	if len(s.pending) != 20 || s.pending[0].ID != "id0" || s.pending[0].Address != "b" || s.pending[0].Client == "" || s.pending[19].ID != "id19" {
		t.Fatalf("pending: %+v", s.pending)
	}
}

// OS in različica tudi pri dovoljeni napravi (v pomnilniku ob zadnjem stiku; prazna glava ohrani prejšnjo).
func TestAllowedDeviceClientInfo(t *testing.T) {
	id := base64url(newDevice("PC").key.Public().(ed25519.PublicKey))
	s, _, base := liveServer(t, func(c *serveConfig) { c.Devices = []serveDevice{{ID: id, Name: "PC"}} })
	s.deviceSeen(serveDevice{ID: id, Name: "PC"}, "192.0.2.7", "Windows, minigw 0.5.18")
	s.deviceSeen(serveDevice{ID: id, Name: "PC"}, "192.0.2.7", "")
	if st, b := adminNoise(t, s, base, s.admin, "GET", adminPrefix+"/devices", ""); st != 200 || !strings.Contains(b, `"client":"Windows, minigw 0.5.18"`) || !strings.Contains(b, `"lastSeen":"`) {
		t.Fatalf("devices: %d %s", st, b)
	}
}

// Podatek odjemalca le v ASCII: URLSession na Macu pošlje druge znake v glavi kot Latin-1 in prehod na Macu (UTF-8)
// zahtevo zavrne (»Bad request header«).
func TestClientInfoIsASCII(t *testing.T) {
	if s := clientInfo(); strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r > 0x7e }) || !strings.Contains(s, ", minigw "+version) {
		t.Fatalf("clientInfo: %q", s)
	}
}

// Varnost L2: /health/ready našteje ponudnike le temu računalniku ali klicatelju z žetonom; #42: lokalno le z loopback Host
// (stran prek DNS rebindinga pošlje svoje ime).
func TestHealthReadyProvidersOnlyLocalOrAuthorized(t *testing.T) {
	s, _ := testServer(t, nil)
	for _, c := range []struct {
		peer, host, token string
		want              bool
	}{{"192.0.2.9", "gw.example", "", false}, {"192.0.2.9", "gw.example", s.conf().Token, true}, {"127.0.0.1", "127.0.0.1:4100", "", true},
		{"127.0.0.1", "localhost:4100", "", true}, {"::1", "[::1]:4100", "", true}, {"127.0.0.1", "evil.example:4100", "", false},
		{"127.0.0.1", "evil.example", s.conf().Token, true}} {
		hr := httptest.NewRequest("GET", "/health/ready", nil)
		hr.Host = c.host
		if c.token != "" {
			hr.Header.Set("Authorization", "Bearer "+c.token)
		}
		w := httptest.NewRecorder()
		s.handle(w, &req{Request: hr, conn: &serveConn{peer: c.peer}})
		if got := strings.Contains(w.Body.String(), `"providers"`); got != c.want || w.Code != 200 {
			t.Fatalf("%+v: %d %s", c, w.Code, w.Body)
		}
	}
}
