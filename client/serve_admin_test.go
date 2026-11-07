package main

import (
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
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
	for _, tc := range []struct {
		host, auth string
		want       int
	}{
		{host, s.admin, 200},
		{"localhost:" + strings.Split(host, ":")[1], s.admin, 200},
		{host, "", 401},
		{host, s.admin + "x", 401},
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
		return adminReq(t, base, host, s.admin, method, path, body)
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
	if st, b := admin("POST", adminPrefix+"/devices", `{"id":"10.192.31.5"}`); st != 400 || !strings.Contains(b, "not a device ID") {
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
	if out := run(cfg, "status"); !strings.Contains(out, "router: running") {
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
	if out := run(cfg, "token"); !strings.Contains(out, "minigw connect") {
		t.Fatalf("token: %q", out)
	}

	off := filepath.Join(t.TempDir(), "serve.json")
	saveServeConfig(off, serveConfig{Token: "tok", Key: s.cfg.Key, Port: freePort(t)})
	if out := run(off, "status"); !strings.Contains(out, "router: not running") {
		t.Fatalf("offline status: %q", out)
	}
	run(off, "devices", "allow", id)
	if c, _ := loadServeConfig(off); len(c.Devices) != 1 {
		t.Fatalf("offline allow: %+v", c.Devices)
	}
}

func TestServeInstallFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	cfg := filepath.Join(home, "my cfg", "serve.json")
	saveServeConfig(cfg, serveConfig{Token: "tok"})
	exe, _ := os.Executable()
	var calls []string
	reg := map[string]string{}
	oldRun, oldSet, oldGet, oldDel := runCmd, runValueSet, runValueGet, runValueDel
	t.Cleanup(func() { runCmd, runValueSet, runValueGet, runValueDel = oldRun, oldSet, oldGet, oldDel })
	runCmd = func(name string, args ...string) error {
		calls = append(calls, name+" "+strings.Join(args, " "))
		return nil
	}
	runValueSet = func(n, v string) error { reg[n] = v; return nil }
	runValueGet = func(n string) (string, error) { return reg[n], nil }
	runValueDel = func(n string) { delete(reg, n) }

	for _, goos := range []string{"linux", "darwin", "windows"} {
		calls = nil
		if _, err := serviceCommand("install", goos, cfg); err != nil {
			t.Fatalf("%s install: %v", goos, err)
		}
		if !serviceInstalled(goos) {
			t.Fatalf("%s: not installed", goos)
		}
		switch goos {
		case "linux":
			data, _ := os.ReadFile(servicePath(goos))
			if !strings.Contains(string(data), `ExecStart="`+exe+`" serve --config "`+cfg+`"`) ||
				strings.Join(calls, ";") != "systemctl --user daemon-reload;systemctl --user enable --now minigw-serve.service" {
				t.Fatalf("unit: %s calls %v", data, calls)
			}
		case "darwin":
			data, _ := os.ReadFile(servicePath(goos))
			if !strings.Contains(string(data), "<string>"+cfg+"</string>") || !strings.Contains(string(data), "<key>RunAtLoad</key><true/>") ||
				!slices.Contains(calls, fmt.Sprintf("launchctl bootstrap gui/%d %s", os.Getuid(), servicePath(goos))) {
				t.Fatalf("plist: %s calls %v", data, calls)
			}
		case "windows":
			if reg[runValueName] != `"`+exe+`" serve --config "`+cfg+`"` {
				t.Fatalf("run value: %q", reg[runValueName])
			}
		}
		if _, err := serviceCommand("uninstall", goos, cfg); err != nil || serviceInstalled(goos) {
			t.Fatalf("%s uninstall: %v", goos, err)
		}
	}
	if q := systemdQuote(`C:\a "b" 100%`); q != `"C:\\a \"b\" 100%%"` {
		t.Fatalf("systemd quote: %s", q)
	}
}
