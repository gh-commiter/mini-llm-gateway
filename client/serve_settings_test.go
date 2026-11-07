package main

import (
	"crypto/ed25519"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// settingsGet: zahteva brez sledenja preusmeritvam (piškotek in Location preverja test).
func settingsGet(t *testing.T, base, host, path string, cookie *http.Cookie) *http.Response {
	t.Helper()
	r, _ := http.NewRequest("GET", base+path, nil)
	r.Host = host
	if cookie != nil {
		r.AddCookie(cookie)
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestSettingsLoginAndCSRF(t *testing.T) {
	s, _, base := liveServer(t, nil)
	host := strings.TrimPrefix(base, "http://")
	token := func() string { u, _ := url.Parse(s.settingsLoginURL()); return u.Query().Get("t") }

	if r := settingsGet(t, base, host, "/minigw/settings", nil); r.StatusCode != 401 {
		t.Fatalf("page without cookie: %d", r.StatusCode)
	}
	if r := settingsGet(t, base, host, "/minigw/settings/login?t=bogus", nil); r.StatusCode != 401 {
		t.Fatalf("bad token: %d", r.StatusCode)
	}
	expired := token()
	s.settings.mu.Lock()
	s.settings.logins[expired] = time.Now().Add(-time.Second)
	s.settings.mu.Unlock()
	if r := settingsGet(t, base, host, "/minigw/settings/login?t="+expired, nil); r.StatusCode != 401 {
		t.Fatalf("expired token: %d", r.StatusCode)
	}
	tok := token()
	if r := settingsGet(t, "http://"+host, "evil.example:"+strings.Split(host, ":")[1], "/minigw/settings/login?t="+tok, nil); r.StatusCode != 403 {
		t.Fatalf("login with a foreign Host: %d", r.StatusCode)
	}
	r := settingsGet(t, base, host, "/minigw/settings/login?t="+tok, nil)
	var cookie *http.Cookie
	for _, c := range r.Cookies() {
		if c.Name == settingsCookie {
			cookie = c
		}
	}
	if r.StatusCode != 303 || r.Header.Get("Location") != "/minigw/settings" || cookie == nil || !cookie.HttpOnly ||
		cookie.SameSite != http.SameSiteStrictMode || cookie.Path != "/minigw/" {
		t.Fatalf("login: %d %v %+v", r.StatusCode, r.Header, cookie)
	}
	if r := settingsGet(t, base, host, "/minigw/settings/login?t="+tok, nil); r.StatusCode != 401 {
		t.Fatalf("token used twice: %d", r.StatusCode)
	}

	// Stran: CSP z nonce, brez okvirjev; CSRF iz seje v strani.
	r = settingsGet(t, base, host, "/minigw/settings", cookie)
	page, _ := io.ReadAll(r.Body)
	csp := r.Header.Get("Content-Security-Policy")
	nonce := regexp.MustCompile(`'nonce-([^']+)'`).FindStringSubmatch(csp)
	csrf := regexp.MustCompile(`<meta name="csrf" content="([^"]+)">`).FindSubmatch(page)
	if r.StatusCode != 200 || nonce == nil || !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "frame-ancestors 'none'") ||
		r.Header.Get("X-Frame-Options") != "DENY" || !strings.Contains(string(page), `<script nonce="`+nonce[1]+`">`) || csrf == nil ||
		strings.Contains(string(page), "{{") {
		t.Fatalf("page: %d CSP %q", r.StatusCode, csp)
	}
	if r := settingsGet(t, base, "evil.example:"+strings.Split(host, ":")[1], "/minigw/settings", cookie); r.StatusCode != 403 {
		t.Fatalf("page with a foreign Host: %d", r.StatusCode)
	}

	// Skrbniški API s piškotkom: branje brez CSRF, sprememba le s pravim X-CSRF.
	call := func(method, path, csrfHeader, body string) int {
		req, _ := http.NewRequest(method, base+adminPrefix+path, strings.NewReader(body))
		req.AddCookie(cookie)
		if csrfHeader != "" {
			req.Header.Set("X-CSRF", csrfHeader)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if st := call("GET", "/status", "", ""); st != 200 {
		t.Fatalf("GET with cookie: %d", st)
	}
	if st := call("PATCH", "/agents/claude", "", `{"tools":"Read"}`); st != 403 {
		t.Fatalf("PATCH without CSRF: %d", st)
	}
	if st := call("PATCH", "/agents/claude", "wrong", `{"tools":"Read"}`); st != 403 {
		t.Fatalf("PATCH with a wrong CSRF: %d", st)
	}
	if st := call("PATCH", "/agents/claude", string(csrf[1]), `{"tools":"Read"}`); st != 200 || s.conf().Tools != "Read" {
		t.Fatalf("PATCH with CSRF: %d %q", st, s.conf().Tools)
	}
	if st := call("POST", "/general/token", string(csrf[1]), ""); st != 200 {
		t.Fatalf("token rotate with CSRF: %d", st)
	}
	// Bearer za CLI ostane; prijavna povezava tudi prek API.
	if st, b := adminReq(t, base, host, s.admin, "POST", adminPrefix+"/settings/login", ""); st != 200 || !strings.Contains(b, "/minigw/settings/login?t=") {
		t.Fatalf("settings login via API: %d %s", st, b)
	}
}

func TestTrayMenu(t *testing.T) {
	id := base64url(newDevice("PC").key.Public().(ed25519.PublicKey))
	st := apiStatus{Port: 8765, Devices: 1, Gateways: []apiGateway{{Name: "or"}}, Installed: true}
	m := trayMenu(st, []apiDevice{{ID: id, Name: "PC\x1b[31m"}}, false)
	if m[0].Text != "Running on :8765 · 1 device · 1 gateway" || !m[0].Disabled {
		t.Fatalf("status line: %+v", m[0])
	}
	if m[1].Text != "Waiting for approval (1)" || m[1].Sub[0].Text != "PC[31m ("+id[:8]+"…)" ||
		m[1].Sub[0].Sub[0].Action != "allow:"+id || m[1].Sub[0].Sub[1].Action != "deny:"+id {
		t.Fatalf("pending: %+v", m[1])
	}
	var actions []string
	for _, it := range m {
		if it.Action != "" {
			actions = append(actions, it.Action)
		}
		if it.Action == "startup" && !it.Checked {
			t.Fatal("start at sign-in not checked")
		}
	}
	if strings.Join(actions, ",") != "settings,copy,startup,log,quit" {
		t.Fatalf("actions: %v", actions)
	}
	m = trayMenu(apiStatus{Port: 1, Devices: 2}, nil, true)
	if m[0].Text != "Teče na :1 · naprav: 2 · prehodov: 0" || m[1].Sep != true {
		t.Fatalf("sl without pending: %+v", m[:2])
	}
}

func TestTrayDeviceActionsAndInstallValue(t *testing.T) {
	s, _, _ := liveServer(t, nil)
	a, b := newDevice("A"), newDevice("B")
	aID, bID := base64url(a.key.Public().(ed25519.PublicKey)), base64url(b.key.Public().(ed25519.PublicKey))
	s.addPending(serveDevice{ID: aID, Name: "A"}, "10.0.0.2")
	s.addPending(serveDevice{ID: bID, Name: "B"}, "10.0.0.3")
	_, pending := s.trayState()
	if err := s.trayDevice("allow:"+aID, pending); err != nil {
		t.Fatal(err)
	}
	if err := s.trayDevice("deny:"+bID, pending); err != nil {
		t.Fatal(err)
	}
	if _, pending = s.trayState(); len(pending) != 0 || len(s.conf().Devices) != 1 || s.conf().Devices[0].Name != "A" {
		t.Fatalf("after allow/deny: %v %+v", pending, s.conf().Devices)
	}

	dir := t.TempDir()
	exe := filepath.Join(dir, "minigw-windows-amd64.exe")
	if trayBeside(exe) != "" {
		t.Fatal("tray found where there is none")
	}
	tray := filepath.Join(dir, "minigw-tray-windows-amd64.exe")
	os.WriteFile(tray, nil, 0o755)
	if trayBeside(exe) != tray || !isTrayExe(tray) || isTrayExe(exe) {
		t.Fatalf("trayBeside: %q", trayBeside(exe))
	}
	if v := serviceFile("windows", exe, `C:\cfg\serve.json`); v != `"`+tray+`" tray --config "C:\cfg\serve.json"` {
		t.Fatalf("run value: %s", v)
	}
}

func TestRotatingLog(t *testing.T) {
	p := filepath.Join(t.TempDir(), "minigw-serve.log")
	l, err := openRotatingLog(p)
	if err != nil {
		t.Fatal(err)
	}
	chunk := []byte(strings.Repeat("x", 1<<20))
	for range 6 {
		l.Write(chunk)
	}
	old, _ := os.Stat(p + ".1")
	cur, _ := os.Stat(p)
	if old == nil || old.Size() != 5<<20 || cur.Size() != 1<<20 {
		t.Fatalf("rotation: %v %v", old, cur)
	}
}
