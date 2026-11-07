package main

import (
	"bufio"
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// liveServer: router na 127.0.0.1 s serve.json (vrata v nastavitvah = vrata poslušanja, kot pri pravem zagonu).
func liveServer(t *testing.T, mod func(*serveConfig)) (*server, string, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	k, _ := ecdh.X25519().GenerateKey(rand.Reader)
	c := serveConfig{Token: "tok", Key: base64url(k.Bytes()), Port: ln.Addr().(*net.TCPAddr).Port, Tools: defaultTools,
		PermissionMode: "dontAsk", Scratch: t.TempDir(), Claude: "/bin/sh", Devices: []serveDevice{}}
	if mod != nil {
		mod(&c)
	}
	cfg := filepath.Join(t.TempDir(), "serve.json")
	saveServeConfig(cfg, c)
	s, err := newServer(c, cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.log.SetOutput(io.Discard)
	go s.serve(ln)
	return s, cfg, fmt.Sprintf("http://127.0.0.1:%d", c.Port)
}

// adminNoise: zahteva kot minigw router (Noise s ključem routerja, dokaz MGW-Admin iz skrivnosti secret).
func adminNoise(t *testing.T, s *server, base, secret, method, path, body string) (int, string) {
	t.Helper()
	a := &adminClient{base: base, secret: secret, routerKey: s.key.PublicKey().Bytes()}
	st, b, err := a.http(method, path, []byte(body), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return st, string(b)
}

func adminReq(t *testing.T, base, host, auth, method, path, body string) (int, string) {
	t.Helper()
	r, _ := http.NewRequest(method, base+path, strings.NewReader(body))
	if host != "" {
		r.Host = host
	}
	if auth != "" {
		r.Header.Set("authorization", "Bearer "+auth)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("CORS header on %s %s", method, path)
	}
	return resp.StatusCode, string(b)
}

func TestAdminAPIAuth(t *testing.T) {
	s, cfg, base := liveServer(t, nil)
	host := strings.TrimPrefix(base, "http://")
	st, _ := os.Stat(filepath.Join(filepath.Dir(cfg), "admin.secret"))
	if st == nil || st.Mode().Perm() != 0o600 || len(s.admin) != 43 {
		t.Fatalf("admin.secret: %v %q", st, s.admin)
	}
	// Navaden HTTP: skrivnost (Bearer) ni nikoli sprejeta; Host se preveri pred overitvijo.
	for _, tc := range []struct {
		host, auth string
		want       int
	}{
		{host, s.admin, 401},
		{host, "", 401},
		{host, "tok", 401}, // žeton routerja ni skrivnost API
		{"evil.example:" + strings.Split(host, ":")[1], s.admin, 403}, // DNS rebinding
		{"127.0.0.1:1", s.admin, 403},
	} {
		if got, body := adminReq(t, base, tc.host, tc.auth, "GET", adminPrefix+"/status", ""); got != tc.want {
			t.Errorf("host %q auth %q: %d %s", tc.host, tc.auth, got, body)
		}
	}
	if got, _ := adminReq(t, base, "", "", "OPTIONS", adminPrefix+"/status", ""); got != 401 { // brez CORS preflight
		t.Errorf("OPTIONS: %d", got)
	}
	// Prek Noise z dokazom MGW-Admin: pravi 200, napačna skrivnost 401; dokaz po navadnem HTTP 401 (ni rokovanja).
	if st, b := adminNoise(t, s, base, s.admin, "GET", adminPrefix+"/status", ""); st != 200 || !strings.Contains(b, `"running":true`) {
		t.Fatalf("noise proof: %d %s", st, b)
	}
	if st, _ := adminNoise(t, s, base, s.admin+"x", "GET", adminPrefix+"/status", ""); st != 401 {
		t.Fatalf("noise with a wrong secret: %d", st)
	}
	r0, _ := http.NewRequest("GET", base+adminPrefix+"/status", nil)
	r0.Header.Set("authorization", adminScheme+adminProof(s.admin, make([]byte, 32)))
	if resp, err := http.DefaultClient.Do(r0); err != nil || resp.StatusCode != 401 {
		t.Fatalf("proof over plain HTTP: %v %v", resp, err)
	}
	// Šifrirana povezava z drugega računalnika (pipe ni loopback): zavrnjena, tudi s pravo skrivnostjo.
	r := pipeRequest(t, s, s.key.PublicKey().Bytes(), newDevice("Mac"),
		"GET "+adminPrefix+"/status HTTP/1.1\r\nHost: "+host+"\r\nAuthorization: Bearer "+s.admin+"\r\n\r\n")
	if !strings.HasPrefix(r, "HTTP/1.1 403") {
		t.Fatalf("remote admin: %q", r)
	}
}

func TestAdminAPILiveApply(t *testing.T) {
	keys := fakeKeys(t)
	up := fakeUpstream(t, nil, nil, nil)
	s, cfg, base := liveServer(t, nil)
	host := strings.TrimPrefix(base, "http://")
	admin := func(method, path, body string) (int, string) {
		return adminNoise(t, s, base, s.admin, method, path, body)
	}
	models := func() string {
		_, b := adminReq(t, base, host, "tok", "GET", "/v1/models", "")
		return b
	}

	// Prehod: dodan med delovanjem, modeli takoj v /v1/models; ključ v shrambi, ne v serve.json.
	if st, b := admin("POST", adminPrefix+"/gateways", `{"name":"gw","baseURL":"`+up.URL+`","key":"`+gwKey+`","insecure":true}`); st != 200 || !strings.Contains(b, `"model":"claude-gpt-y"`) {
		t.Fatalf("add gateway: %d %s", st, b)
	}
	if !strings.Contains(models(), "anthropic/claude-x") {
		t.Fatalf("gateway not live: %s", models())
	}
	if st, b := admin("POST", adminPrefix+"/gateways/gw/models", `{"disable":["claude-gpt-y"]}`); st != 200 || !strings.Contains(b, `"changed":[{"id":"gpt-y"`) {
		t.Fatalf("disable: %d %s", st, b)
	}
	if strings.Contains(models(), "claude-gpt-y") {
		t.Fatalf("disabled model still listed: %s", models())
	}
	if data, _ := os.ReadFile(cfg); strings.Contains(string(data), gwKey) || !strings.Contains(string(data), `"gw"`) || keys["gw"] != gwKey {
		t.Fatalf("serve.json: %s", data)
	}

	// Naprava: zavrnjena → med čakajočimi → dovoljena brez ponovnega zagona → odstranjena po imenu.
	mac := newDevice("Mac")
	macID := base64url(mac.key.Public().(ed25519.PublicKey))
	get := "GET /v1/models HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer tok\r\n\r\n"
	pub := s.key.PublicKey().Bytes()
	if r := pipeRequest(t, s, pub, mac, get); !strings.HasPrefix(r, "HTTP/1.1 403") {
		t.Fatalf("unknown device: %q", r)
	}
	if _, b := admin("GET", adminPrefix+"/devices", ""); !strings.Contains(b, `"pending":[{"id":"`+macID+`","name":"Mac",`) {
		t.Fatalf("pending: %s", b)
	}
	if st, b := admin("POST", adminPrefix+"/devices", `{"id":"203.0.113.5"}`); st != 400 || !strings.Contains(b, "not a device ID") {
		t.Fatalf("bad id: %d %s", st, b)
	}
	if st, b := admin("POST", adminPrefix+"/devices", `{"id":"`+macID+`","name":"Mac"}`); st != 200 || !strings.Contains(b, `"added":true`) {
		t.Fatalf("allow: %d %s", st, b)
	}
	if r := pipeRequest(t, s, pub, mac, get); !strings.HasPrefix(r, "HTTP/1.1 200") {
		t.Fatalf("allowed device not live: %q", r)
	}
	if _, b := admin("GET", adminPrefix+"/devices", ""); !strings.Contains(b, `"pending":[]`) {
		t.Fatalf("pending after allow: %s", b)
	}
	if st, _ := admin("DELETE", adminPrefix+"/devices/Mac", ""); st != 200 {
		t.Fatal("deny by name")
	}
	if r := pipeRequest(t, s, pub, mac, get); !strings.HasPrefix(r, "HTTP/1.1 403") {
		t.Fatalf("denied device still allowed: %q", r)
	}

	// Orodja agenta takoj; Bash brez sandboxa na Windows le z dovoljenjem (tu le napačna orodja).
	if st, b := admin("PATCH", adminPrefix+"/agents/claude", `{"tools":"Read, Grep"}`); st != 200 || !strings.Contains(b, `"tools":"Read,Grep"`) {
		t.Fatalf("tools: %d %s", st, b)
	}
	if args := s.agentArgs(t.TempDir()); !slices.Contains(args, "Read,Grep") {
		t.Fatalf("tools not live: %v", args)
	}
	if st, _ := admin("PATCH", adminPrefix+"/agents/claude", `{"tools":"Read;rm -rf"}`); st != 400 {
		t.Fatal("bad tools accepted")
	}
	if st, _ := admin("PATCH", adminPrefix+"/agents/claude", `{"bogus":1}`); st != 400 {
		t.Fatal("unknown field accepted")
	}

	// Vrata: shranjena, ponovni zagon potreben; žeton: stari takoj ne velja.
	if st, b := admin("PATCH", adminPrefix+"/general", `{"port":9999}`); st != 200 || !strings.Contains(b, `"restartRequired":true`) {
		t.Fatalf("port: %d %s", st, b)
	}
	st, b := admin("POST", adminPrefix+"/general/token", "")
	if st != 200 || !strings.Contains(b, "minigw connect") || strings.Contains(b, "'tok#") {
		t.Fatalf("token: %d %s", st, b)
	}
	if st, _ := adminReq(t, base, host, "tok", "GET", "/v1/models", ""); st != 401 {
		t.Fatalf("old token still works: %d", st)
	}
	if c, _ := loadServeConfig(cfg); c.Token == "tok" || c.Port != 9999 || len(c.Devices) != 0 {
		t.Fatalf("serve.json: %+v", c)
	}
}

// minigw router: z tekočim routerjem prek API (velja takoj), brez njega v serve.json.
func TestRouterCLIOnlineAndOffline(t *testing.T) {
	s, cfg, _ := liveServer(t, nil)
	run := func(cfg string, args ...string) string {
		return stdout(t, func() { routerCommand(append(args, "--config", cfg), strings.NewReader("")) })
	}
	if out := run(cfg, "status"); !strings.Contains(out, "gateway: running") {
		t.Fatalf("status: %q", out)
	}
	id := base64url(newDevice("PC").key.Public().(ed25519.PublicKey))
	if out := run(cfg, "devices", "allow", id, "My", "PC"); out != "allowed device "+id+"\n" {
		t.Fatalf("allow: %q", out)
	}
	if c := s.conf(); len(c.Devices) != 1 || c.Devices[0].Name != "My PC" {
		t.Fatalf("not live: %+v", c.Devices)
	}
	if out := run(cfg, "claude", "--tools", "Read"); !strings.Contains(out, "tools: Read\n") || s.conf().Tools != "Read" {
		t.Fatalf("claude: %q", out)
	}
	dir := t.TempDir() // vrstica 2 revizije: ista zastavica kot na Macu
	if out := run(cfg, "claude", "--folders", dir, "--allow-local-tools=true"); !strings.Contains(out, "allowLocalTools: true\n") ||
		!strings.Contains(out, "folders: ") || !s.conf().AllowLocalTools || len(s.conf().AllowedFolders) != 1 {
		t.Fatalf("claude folders: %q %+v", out, s.conf().AllowedFolders)
	}
	if out := run(cfg, "token"); !strings.Contains(out, "minigw connect") {
		t.Fatalf("token: %q", out)
	}

	off := filepath.Join(t.TempDir(), "serve.json")
	saveServeConfig(off, serveConfig{Token: "tok", Key: s.cfg.Key, Port: freePort(t)})
	if out := run(off, "status"); !strings.Contains(out, "gateway: not running") {
		t.Fatalf("offline status: %q", out)
	}
	run(off, "devices", "allow", id)
	if c, _ := loadServeConfig(off); len(c.Devices) != 1 {
		t.Fatalf("offline allow: %+v", c.Devices)
	}
	dash := "-" + id[1:]                     // ID z vodilnim »-« ni zastavica (prej je občasno ustavil test)
	for !validDeviceID(dash) || dash == id { // id se lahko že sam začne z »-« (1 od 64): to bi bila ista naprava
		dash = "-" + base64url(newDevice("X").key.Public().(ed25519.PublicKey))[1:]
	}
	if out := run(off, "devices", "allow", dash); out != "allowed device "+dash+"\n" {
		t.Fatalf("dash id: %q", out)
	}
}

// M1: program na vratih routerja, ki ni ta router, ne dobi skrivnosti ali ključev; minigw router dela brez routerja.
func TestRouterCLINeverSendsSecretToSquatter(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var mu sync.Mutex
	var got []byte
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				c.SetDeadline(time.Now().Add(3 * time.Second))
				c.Write([]byte(strings.Repeat("HTTP/1.1 200 OK\r\n", 20))) // dovolj bajtov za »odgovor« rokovanja
				buf := make([]byte, 64<<10)
				for {
					n, err := c.Read(buf)
					mu.Lock()
					got = append(got, buf[:n]...)
					mu.Unlock()
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	k, _ := ecdh.X25519().GenerateKey(rand.Reader)
	cfg := filepath.Join(t.TempDir(), "serve.json")
	saveServeConfig(cfg, serveConfig{Token: "tok", Key: base64url(k.Bytes()), Port: ln.Addr().(*net.TCPAddr).Port})
	secret := strings.Repeat("S", 43)
	os.WriteFile(filepath.Join(filepath.Dir(cfg), "admin.secret"), []byte(secret+"\n"), 0o600)
	fakeKeys(t)
	id := base64url(newDevice("PC").key.Public().(ed25519.PublicKey))
	stdout(t, func() { routerCommand([]string{"devices", "allow", id, "--config", cfg}, strings.NewReader("")) })
	mu.Lock()
	defer mu.Unlock()
	if len(got) == 0 || strings.Contains(string(got), secret) || strings.Contains(string(got), "Bearer") {
		t.Fatalf("squatter got %d bytes: %q", len(got), got)
	}
	if c, _ := loadServeConfig(cfg); len(c.Devices) != 1 {
		t.Fatalf("not applied to serve.json: %+v", c.Devices)
	}
}

func TestAdminReviewFixes(t *testing.T) {
	s, cfg, base := liveServer(t, nil)
	admin := func(method, path, body string) (int, string) {
		return adminNoise(t, s, base, s.admin, method, path, body)
	}

	// I2: podatki za objektom JSON; L1: naslov, na katerem 127.0.0.1 ne bi delal.
	if st, b := admin("PATCH", adminPrefix+"/agents/claude", `{"tools":"Read"}{"tools":"Bash"}`); st != 400 || !strings.Contains(b, "after the JSON") {
		t.Fatalf("trailing data: %d %s", st, b)
	}
	if st, _ := admin("PATCH", adminPrefix+"/general", `{"address":"192.168.1.5"}`); st != 400 {
		t.Fatal("LAN-only address accepted")
	}
	if st, _ := admin("PATCH", adminPrefix+"/general", `{"address":"0.0.0.0"}`); st != 200 {
		t.Fatal("0.0.0.0 refused")
	}
	// L6: relativna mapa prek API; povezava v mapo nastavitev.
	if st, _ := admin("PATCH", adminPrefix+"/agents/claude", `{"folder":"rel/dir"}`); st != 400 {
		t.Fatal("relative folder accepted")
	}
	link := filepath.Join(t.TempDir(), "link")
	os.Symlink(filepath.Dir(cfg), link)
	if st, b := admin("PATCH", adminPrefix+"/agents/claude", `{"folder":"`+link+`"}`); st != 400 || !strings.Contains(b, "config folder") {
		t.Fatalf("symlink to the config folder: %d %s", st, b)
	}

	// L2: serve.json se zapiše na novo (povezava na poti se ne sledi), 0600.
	outside := filepath.Join(t.TempDir(), "outside.json")
	data, _ := os.ReadFile(cfg)
	os.WriteFile(outside, data, 0o644)
	os.Remove(cfg)
	os.Symlink(outside, cfg)
	if st, _ := admin("PATCH", adminPrefix+"/agents/claude", `{"tools":"Glob,LS"}`); st != 200 {
		t.Fatal("update")
	}
	if b, _ := os.ReadFile(outside); strings.Contains(string(b), "Glob,LS") {
		t.Fatal("symlink at serve.json followed")
	}
	if st, _ := os.Lstat(cfg); st == nil || st.Mode()&os.ModeSymlink != 0 || st.Mode().Perm() != 0o600 {
		t.Fatalf("serve.json: %v", st)
	}

	// L5: na Windows vse razen znanih varnih orodij potrebuje allowUnsandboxedBash.
	if got := unsandboxedTools("Read, Grep,PowerShell,Bash,Mystery,WebFetch"); strings.Join(got, ",") != "PowerShell,Bash,Mystery" {
		t.Fatalf("unsandboxed tools: %v", got)
	}
	// L7: $ v ExecStart; nove vrstice v poteh zavrnjene.
	if q := systemdQuote("/a$HOME"); q != `"/a$$HOME"` {
		t.Fatalf("systemd $: %s", q)
	}
	if _, err := serviceCommand("install", "linux", filepath.Join(t.TempDir(), "x\nExecStartPre=evil", "serve.json")); err == nil {
		t.Fatal("newline in the config path accepted")
	}
}

// S1: lažen strežnik s svojim ključem (npr. preusmerjena mapa nastavitev) dobi le dokaz, vezan na svoje rokovanje:
// ne skrivnosti in nič, kar bi veljalo pri pravem routerju.
func TestAdminProofIsBoundToTheConnection(t *testing.T) {
	s, _, base := liveServer(t, nil)
	fake, _ := ecdh.X25519().GenerateKey(rand.Reader)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		msg := make([]byte, 4+handshakeLen)
		io.ReadFull(c, msg)
		reply, sess, err := respond(msg[4:], fake)
		if err != nil {
			got <- ""
			return
		}
		c.Write(reply)
		var raw, plain []byte
		buf := make([]byte, 64<<10)
		for !strings.Contains(string(plain), "\r\n\r\n") {
			n, err := c.Read(buf)
			if err != nil {
				break
			}
			var p []byte
			p, raw, _ = unframe(append(raw, buf[:n]...), sess.receive)
			plain = append(plain, p...)
		}
		got <- string(plain)
	}()
	a := &adminClient{base: "http://" + ln.Addr().String(), secret: s.admin, routerKey: fake.PublicKey().Bytes()}
	a.http("GET", adminPrefix+"/status", nil, 3*time.Second)
	req := <-got
	auth := regexp.MustCompile(`(?i)authorization: (.*)\r\n`).FindStringSubmatch(req)
	if auth == nil || strings.Contains(req, s.admin) || !strings.HasPrefix(auth[1], adminScheme) {
		t.Fatalf("fake server got: %q", req)
	}
	// Ponovitev dokaza pri pravem routerju (druga povezava, drug h): 401.
	c, err := dialAdmin(context.Background(), strings.TrimPrefix(base, "http://"), s.key.PublicKey().Bytes())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "GET %s/status HTTP/1.1\r\nHost: %s\r\nAuthorization: %s\r\nConnection: close\r\n\r\n", adminPrefix, strings.TrimPrefix(base, "http://"), auth[1])
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil || resp.StatusCode != 401 {
		t.Fatalf("replayed proof: %v %v", resp, err)
	}
}

// Razširitve za stran z nastavitvami: os, pladenj in namestitev v status; najdena pot in različica claude.
func TestAdminSettingsFields(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake claude is a shell script")
	}
	fake := filepath.Join(t.TempDir(), "claude")
	os.WriteFile(fake, []byte("#!/bin/sh\necho '9.8.7 (Claude Code)'\n"), 0o755)
	s, _, base := liveServer(t, func(c *serveConfig) { c.Claude = fake })
	st, b := adminNoise(t, s, base, s.admin, "GET", adminPrefix+"/status", "")
	if st != 200 || !strings.Contains(b, `"os":"`+runtime.GOOS+`"`) || !strings.Contains(b, `"tray":false`) {
		t.Fatalf("status: %d %s", st, b)
	}
	st, b = adminNoise(t, s, base, s.admin, "GET", adminPrefix+"/agents/claude", "")
	if st != 200 || !strings.Contains(b, `"exe":"`+fake+`"`) || !strings.Contains(b, `"version":"9.8.7 (Claude Code)"`) {
		t.Fatalf("claude: %d %s", st, b)
	}
	if st, b := adminNoise(t, s, base, s.admin, "PATCH", adminPrefix+"/general", `{"startAtSignIn":true}`); st != 400 || !strings.Contains(b, "minigw serve install") {
		t.Fatalf("startAtSignIn off Windows: %d %s", st, b)
	}
}

// Parnost z Macom: dovoljeni odjemalci, navaden HTTP od drugod, izklop Claude CLI, dovoljene mape, lokalna orodja.
func TestParityGeneralAndClaude(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake claude is a shell script")
	}
	work := t.TempDir()
	s, _, base := liveServer(t, nil)
	admin := func(method, path, body string) (int, string) {
		return adminNoise(t, s, base, s.admin, method, path, body)
	}

	// Dovoljeni odjemalci in navaden HTTP: pipe ni loopback (naslov »pipe«).
	plain := func() string {
		l := pipeListener{make(chan net.Conn, 1)}
		go s.serve(l)
		defer close(l.ch)
		a, b := net.Pipe()
		l.ch <- b
		a.SetDeadline(time.Now().Add(5 * time.Second))
		go a.Write([]byte("GET /health/live HTTP/1.1\r\nHost: x\r\n\r\n"))
		r, _ := io.ReadAll(a)
		return string(r)
	}
	if r := plain(); !strings.HasPrefix(r, "HTTP/1.1 426") {
		t.Fatalf("plain remote off: %q", r)
	}
	if st, b := admin("PATCH", adminPrefix+"/general", `{"allowPlainRemote":true,"allowedClients":"192.168.1.7, 10.0.0.2"}`); st != 200 || !strings.Contains(b, `"allowPlainRemote":true`) {
		t.Fatalf("general: %d %s", st, b)
	}
	if r := plain(); r != "" { // »pipe« ni na seznamu dovoljenih: povezava zaprta
		t.Fatalf("client not in allowedClients: %q", r)
	}
	if !s.conf().clientAllowed("10.0.0.2") || s.conf().clientAllowed("10.0.0.3") || !(serveConfig{}).clientAllowed("1.2.3.4") {
		t.Fatal("clientAllowed")
	}
	admin("PATCH", adminPrefix+"/general", `{"allowedClients":""}`)
	if r := plain(); !strings.HasPrefix(r, "HTTP/1.1 200") {
		t.Fatalf("plain remote on: %q", r)
	}
	if st, _ := admin("PATCH", adminPrefix+"/general", `{"allowedClients":"not-an-ip"}`); st != 400 {
		t.Fatal("bad IP accepted")
	}
	sc := filepath.Join(nonTempDir(t), "scratch") // M1: začasne mape so zavrnjene
	if st, b := admin("PATCH", adminPrefix+"/general", `{"scratch":"`+sc+`"}`); st != 200 || s.conf().Scratch != sc {
		t.Fatalf("scratch: %d %s", st, b)
	}
	for _, bad := range []string{filepath.Join(t.TempDir(), "x"), "/"} { // začasna mapa, koren diska
		if st, _ := admin("PATCH", adminPrefix+"/general", `{"scratch":"`+bad+`"}`); st != 400 {
			t.Fatalf("scratch %s accepted: %d", bad, st)
		}
	}

	// Claude CLI: izklop skrije modele; mape, lokalna orodja in izjeme v argumentih.
	if st, _ := admin("PATCH", adminPrefix+"/agents/claude", `{"enabled":false}`); st != 200 {
		t.Fatal("disable")
	}
	if _, b := adminReq(t, base, strings.TrimPrefix(base, "http://"), "tok", "GET", "/v1/models", ""); strings.Contains(b, "claude-cli-") {
		t.Fatalf("claude-cli listed while off: %s", b)
	}
	st, b := admin("PATCH", adminPrefix+"/agents/claude", `{"enabled":true,"folders":"`+work+`","allowLocalTools":true,"disabledTools":["WebSearch"],"settingSources":"project"}`)
	if st != 200 || !strings.Contains(b, `"allowLocalTools":true`) || !strings.Contains(b, `"folders":"`) {
		t.Fatalf("claude: %d %s", st, b)
	}
	args := strings.Join(s.agentArgs("/copy"), " ")
	realWork, _ := filepath.EvalSymlinks(work)
	for _, want := range []string{"--tools default", "--permission-mode acceptEdits", "WebSearch", "--add-dir " + realWork, "Read(" + ruleAbs(realWork) + "/**)", `"allowWrite":["/copy","` + realWork + `"]`} {
		if !strings.Contains(args, want) {
			t.Fatalf("agent args miss %q: %s", want, args)
		}
	}
	for _, bad := range []string{`{"folders":"relative/dir"}`, `{"permissionMode":"bypassPermissions"}`, `{"settingSources":"project;rm"}`, `{"disabledTools":["Bad Tool"]}`} {
		if st, _ := admin("PATCH", adminPrefix+"/agents/claude", bad); st != 400 {
			t.Fatalf("accepted %s", bad)
		}
	}
	if st, b := admin("GET", adminPrefix+"/status", ""); st != 200 || !strings.Contains(b, `"build"`) && buildCommit() != "" {
		t.Fatalf("status build: %s", b)
	}
}

// nonTempDir: mapa za Scratch v testih, zunaj začasnih map (M1 jih zavrne); izbriše se po testu.
func nonTempDir(t *testing.T) string {
	t.Helper()
	base, err := os.UserCacheDir()
	if err != nil {
		t.Skip("no user cache folder")
	}
	d, err := os.MkdirTemp(base, "minigw-test-")
	if err != nil {
		t.Skip(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	return d
}

// M1: Scratch ni koren, domača mapa, začasna mapa ali v dovoljeni mapi (ali okoli nje); v denyRead peskovnika Claude.
func TestScratchProblem(t *testing.T) {
	home, _ := os.UserHomeDir()
	d := nonTempDir(t)
	proj := filepath.Join(d, "proj")
	os.MkdirAll(filepath.Join(proj, "in"), 0o755)
	for _, c := range []struct {
		p    string
		want string
	}{{"/", "root"}, {home, "home"}, {filepath.Dir(home), "home"}, {os.TempDir(), "temporary"}, {filepath.Join(proj, "in"), "allowed"},
		{d, "allowed"}, {filepath.Join(d, "ok"), ""}} {
		if got := scratchProblem(c.p, []string{proj}); (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Fatalf("%s: %q (want %q)", c.p, got, c.want)
		}
	}
	if p := scratchPaths(d); len(p) == 0 || p[0] != d {
		t.Fatalf("paths %v", p)
	}
}

// L2: mapa scratch je le naša (0700) in ni simbolna povezava.
func TestScratchFolderProtected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows: DACL (protectDir)")
	}
	s, _, base := liveServer(t, nil)
	open := filepath.Join(nonTempDir(t), "open")
	os.Mkdir(open, 0o755)
	if st, b := adminNoise(t, s, base, s.admin, "PATCH", adminPrefix+"/general", `{"scratch":"`+open+`"}`); st != 200 {
		t.Fatalf("scratch: %d %s", st, b)
	}
	if fi, _ := os.Stat(open); fi.Mode().Perm() != 0o700 {
		t.Fatalf("mode %v", fi.Mode())
	}
	link := filepath.Join(nonTempDir(t), "link")
	os.Symlink(open, link)
	if st, _ := adminNoise(t, s, base, s.admin, "PATCH", adminPrefix+"/general", `{"scratch":"`+link+`"}`); st != 400 {
		t.Fatalf("symlink accepted: %d", st)
	}
}

// Ime: »gateway« je ta računalnik, »provider« upstream; /providers je kanonično, /gateways in minigw router alias.
func TestProvidersNamesAndAliases(t *testing.T) {
	s, cfg, base := liveServer(t, nil)
	for _, p := range []string{"/providers", "/gateways"} {
		if st, b := adminNoise(t, s, base, s.admin, "GET", adminPrefix+p, ""); st != 200 || !strings.Contains(b, `"providers":[]`) || !strings.Contains(b, `"gateways":[]`) {
			t.Fatalf("%s: %d %s", p, st, b)
		}
	}
	if st, b := adminNoise(t, s, base, s.admin, "GET", adminPrefix+"/status", ""); st != 200 || !strings.Contains(b, `"providers":`) || !strings.Contains(b, `"gateways":`) {
		t.Fatalf("status: %d %s", st, b)
	}
	run := func(args ...string) string {
		return stdout(t, func() { routerCommand(append(args, "--config", cfg), strings.NewReader("")) })
	}
	if out := run("provider", "list"); !strings.HasPrefix(out, "no providers") {
		t.Fatalf("gw provider list: %q", out)
	}
	if run("provider", "list") != run("gateway", "list") {
		t.Fatal("gateway alias differs")
	}
}
