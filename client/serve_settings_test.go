package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/binary"
	"fmt"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"
)

// settingsCall: zahteva na strani ali API; vrne status, telo in glave (piškotkov ne sme biti nikoli).
func settingsCall(t *testing.T, base, host, method, path, session, body string) (int, string, http.Header) {
	t.Helper()
	r, _ := http.NewRequest(method, base+path, strings.NewReader(body))
	r.Host = host
	if session != "" {
		r.Header.Set(settingsHeader, session)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.Header.Get("Set-Cookie") != "" {
		t.Errorf("%s %s set a cookie", method, path)
	}
	return resp.StatusCode, string(b), resp.Header
}

func TestSettingsSessionWithoutCookies(t *testing.T) {
	s, _, base := liveServer(t, nil)
	host := strings.TrimPrefix(base, "http://")
	token := func() string {
		u := s.settingsLoginURL()
		if !strings.Contains(u, "/minigw/settings#t=") { // žeton v fragmentu, ne v poizvedbi
			t.Fatalf("login URL: %s", u)
		}
		return u[strings.Index(u, "#t=")+3:]
	}
	login := func(tok string) (int, string) {
		st, b, _ := settingsCall(t, base, host, "POST", "/minigw/settings/login", "", `{"t":"`+tok+`"}`)
		return st, b
	}

	// Stran: statična, brez skrivnosti, CSP z nonce, brez okvirjev; tudi brez seje.
	st, page, h := settingsCall(t, base, host, "GET", "/minigw/settings", "", "")
	nonce := regexp.MustCompile(`'nonce-([^']+)'`).FindStringSubmatch(h.Get("Content-Security-Policy"))
	if st != 200 || nonce == nil || !strings.Contains(page, `<script nonce="`+nonce[1]+`">`) || h.Get("X-Frame-Options") != "DENY" ||
		strings.Contains(page, "{{") || strings.Contains(page, s.admin) || strings.Contains(page, "'tok#") || strings.Contains(strings.ToLower(page), "csrf") {
		t.Fatalf("page: %d %v", st, h)
	}

	// Prijava le s POST, enkrat, v 60 s.
	tok := token()
	if st, _, _ := settingsCall(t, base, host, "GET", "/minigw/settings/login?t="+tok, "", ""); st != 405 {
		t.Fatalf("login via GET: %d", st)
	}
	if st, _ := login("bogus"); st != 401 {
		t.Fatalf("bad token: %d", st)
	}
	if st, _, _ := settingsCall(t, "http://"+host, "evil.example:"+strings.Split(host, ":")[1], "POST", "/minigw/settings/login", "", `{"t":"`+tok+`"}`); st != 403 {
		t.Fatalf("login with a foreign Host: %d", st)
	}
	st, b := login(tok)
	session := regexp.MustCompile(`"session":"([^"]+)"`).FindStringSubmatch(b)
	if st != 200 || session == nil {
		t.Fatalf("login: %d %s", st, b)
	}
	if st, _ := login(tok); st != 401 {
		t.Fatalf("token used twice: %d", st)
	}
	expired := token()
	s.settings.mu.Lock()
	s.settings.logins[expired] = time.Now().Add(-time.Second)
	s.settings.mu.Unlock()
	if st, _ := login(expired); st != 401 {
		t.Fatalf("expired token: %d", st)
	}

	// API: brez glave seje ali z napačno 401; s pravo 200 (branje in sprememba); no-store.
	if st, _, _ := settingsCall(t, base, host, "GET", adminPrefix+"/general", "", ""); st != 401 {
		t.Fatalf("API without session: %d", st)
	}
	if st, _, _ := settingsCall(t, base, host, "GET", adminPrefix+"/general", "wrong", ""); st != 401 {
		t.Fatalf("API with a wrong session: %d", st)
	}
	st, _, h = settingsCall(t, base, host, "GET", adminPrefix+"/general", session[1], "")
	if st != 200 || h.Get("Cache-Control") != "no-store" {
		t.Fatalf("API with session: %d %v", st, h)
	}
	if st, _, _ := settingsCall(t, base, host, "PATCH", adminPrefix+"/agents/claude", session[1], `{"tools":"Read"}`); st != 200 || s.conf().Tools != "Read" {
		t.Fatalf("PATCH with session: %d", st)
	}

	// Mirovanje 30 min; odjava; nov žeton routerja konča vse seje.
	s.settings.mu.Lock()
	s.settings.sessions[session[1]] = time.Now().Add(-31 * time.Minute)
	s.settings.mu.Unlock()
	if st, _, _ := settingsCall(t, base, host, "GET", adminPrefix+"/status", session[1], ""); st != 401 {
		t.Fatalf("idle session: %d", st)
	}
	_, b = login(token())
	s2 := regexp.MustCompile(`"session":"([^"]+)"`).FindStringSubmatch(b)[1]
	settingsCall(t, base, host, "POST", "/minigw/settings/logout", s2, "")
	if st, _, _ := settingsCall(t, base, host, "GET", adminPrefix+"/status", s2, ""); st != 401 {
		t.Fatalf("after logout: %d", st)
	}
	_, b = login(token())
	s3 := regexp.MustCompile(`"session":"([^"]+)"`).FindStringSubmatch(b)[1]
	if st, _, _ := settingsCall(t, base, host, "POST", adminPrefix+"/general/token", s3, ""); st != 200 {
		t.Fatalf("rotate: %d", st)
	}
	if st, _, _ := settingsCall(t, base, host, "GET", adminPrefix+"/status", s3, ""); st != 401 {
		t.Fatalf("session after token rotation: %d", st)
	}
	// Bearer za CLI ostane; prijavna povezava tudi prek API.
	if st, b := adminNoise(t, s, base, s.admin, "POST", adminPrefix+"/settings/login", ""); st != 200 || !strings.Contains(b, "/minigw/settings#t=") {
		t.Fatalf("settings login via API: %d %s", st, b)
	}
}

// Izberi… / Prebrskaj…: seja kot pri spremembah, le POST; brez GUI 501, drugo okno 409; zmožnost na strani.
func TestSettingsPick(t *testing.T) {
	s, _, base := liveServer(t, nil)
	host := strings.TrimPrefix(base, "http://")
	u := s.settingsLoginURL()
	_, b, _ := settingsCall(t, base, host, "POST", "/minigw/settings/login", "", `{"t":"`+u[strings.Index(u, "#t=")+3:]+`"}`)
	session := regexp.MustCompile(`"session":"([^"]+)"`).FindStringSubmatch(b)[1]
	pick := func(sess, body string) (int, string) {
		st, b, _ := settingsCall(t, base, host, "POST", "/minigw/settings/pick", sess, body)
		return st, b
	}
	for _, c := range []struct {
		sess, body string
		want       int
	}{{"", `{"kind":"folder"}`, 401}, {"wrong", `{"kind":"folder"}`, 401}, {session, `{"kind":"x"}`, 400},
		{session, `{"kind":"folder","x":1}`, 400}, {session, `{"kind":"folder"}`, 501}} {
		if st, b := pick(c.sess, c.body); st != c.want {
			t.Fatalf("%q %s: %d %s (want %d)", c.sess, c.body, st, b, c.want)
		}
	}
	if st, _, _ := settingsCall(t, base, host, "GET", "/minigw/settings/pick", session, ""); st != 405 {
		t.Fatalf("GET: %d", st)
	}
	if _, page, _ := settingsCall(t, base, host, "GET", "/minigw/settings", "", ""); !strings.Contains(page, "const PICKER = false;") {
		t.Fatal("page must report no picker")
	}

	// Pladenj: okno odprto (čaka), drugo 409; po izbiri pot, ob preklicu "".
	entered, release := make(chan string), make(chan string)
	f := pickFunc(func(kind string) (string, error) { entered <- kind; return <-release, nil })
	s.pick.Store(&f)
	if _, page, _ := settingsCall(t, base, host, "GET", "/minigw/settings", "", ""); !strings.Contains(page, "const PICKER = true;") {
		t.Fatal("page must report the picker")
	}
	done := make(chan string)
	go func() { _, b := pick(session, `{"kind":"exe"}`); done <- b }()
	if k := <-entered; k != "exe" {
		t.Fatalf("kind: %s", k)
	}
	if st, _ := pick(session, `{"kind":"folder"}`); st != 409 {
		t.Fatalf("second dialog: %d", st)
	}
	release <- `C:\Program Files\claude.exe`
	if b := <-done; !strings.Contains(b, `"path":"C:\\Program Files\\claude.exe"`) {
		t.Fatalf("path: %s", b)
	}
	go func() { <-entered; release <- "" }()
	if st, b := pick(session, `{"kind":"folder"}`); st != 200 || !strings.Contains(b, `"path":""`) {
		t.Fatalf("cancel: %d %s", st, b)
	}
}

func TestTrayMenu(t *testing.T) {
	id := base64url(newDevice("PC").key.Public().(ed25519.PublicKey))
	st := apiStatus{Port: 8765, Devices: 1, Gateways: []apiGateway{{Name: "or"}}, Installed: true}
	m := trayMenu(st, []apiDevice{{ID: id, Name: "PC\x1b[31m\u202e\u200b"}}, false) // ESC, bidi, ničelna širina ven
	if m[0].Text != "Running on :8765 · 1 device · 1 provider" || !m[0].Disabled {
		t.Fatalf("status line: %+v", m[0])
	}
	if m[1].Text != "Waiting for approval (1)" || m[1].Sub[0].Text != "PC[31m ("+id+")" ||
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
	if m[0].Text != "Teče na :1 · naprav: 2 · ponudnikov: 0" || m[1].Sep != true {
		t.Fatalf("sl without pending: %+v", m[:2])
	}
}

func TestTrayDeviceActionsAndInstallValue(t *testing.T) {
	s, _, _ := liveServer(t, nil)
	a, b := newDevice("A"), newDevice("B")
	aID, bID := base64url(a.key.Public().(ed25519.PublicKey)), base64url(b.key.Public().(ed25519.PublicKey))
	s.addPending(serveDevice{ID: aID, Name: "A"}, "10.0.0.2", "")
	s.addPending(serveDevice{ID: bID, Name: "B"}, "10.0.0.3", "")
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
	os.WriteFile(filepath.Join(dir, "minigw-tray-evil.exe"), nil, 0o755) // le natančna imena
	if trayBeside(exe) != "" {
		t.Fatal("tray found by a loose name")
	}
	tray := filepath.Join(dir, "minigw-tray-windows-"+runtime.GOARCH+".exe")
	os.WriteFile(tray, nil, 0o755)
	if trayBeside(exe) != tray || !isTrayExe(tray) || isTrayExe(exe) || isTrayExe("minigw-tray-evil.exe") || !isTrayExe("MINIGW-TRAY.EXE") {
		t.Fatalf("trayBeside: %q", trayBeside(exe))
	}
	if _, err := serviceFile("windows", exe, `C:\cfg\"x`); err == nil { // I3
		t.Fatal("quote in a Run value accepted")
	}
	if v, _ := serviceFile("windows", exe, `C:\cfg\serve.json`); v != `"`+tray+`" tray --config "C:\cfg\serve.json"` {
		t.Fatalf("run value: %s", v)
	}
}

func TestRotatingLog(t *testing.T) {
	p := filepath.Join(t.TempDir(), "minigw-serve.log")
	l, err := openRotatingLog(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.f.Close() }) // Windows: odprte datoteke TempDir ne izbriše
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

func TestTrayTipAndBadge(t *testing.T) {
	st := apiStatus{Port: 8765, Devices: 1}
	if s := trayTip(st, 0, false); s != "Mini Gateway · Running on :8765 · 1 device · 0 providers" {
		t.Fatal(s)
	}
	if s := trayTip(st, 2, false); s != "Mini Gateway · Running on :8765 · 1 device · 0 providers · 2 devices waiting for approval" {
		t.Fatal(s)
	}
	if s := trayTip(st, 1, true); !strings.HasSuffix(s, " · na odobritev čaka naprav: 1") {
		t.Fatal(s)
	}
	ico := srcFile(t, "tray.ico")
	for i := 0; i < int(binary.LittleEndian.Uint16(ico[4:])); i++ { // vsak vnos .ico (16, 32, 48)
		e := ico[6+16*i:]
		size, off := binary.LittleEndian.Uint32(e[8:]), binary.LittleEndian.Uint32(e[12:])
		b, err := trayBadge(ico[off : off+size])
		if err != nil {
			t.Fatal(err)
		}
		img, err := png.Decode(bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		w := img.Bounds().Dx()
		if c := color.NRGBAModel.Convert(img.At(w*3/4, w*3/4)).(color.NRGBA); c != (color.NRGBA{0xFF, 0x95, 0, 0xFF}) {
			t.Fatalf("%d: badge center %v", w, c)
		}
		if dir := os.Getenv("TRAY_BADGE_PREVIEW"); dir != "" { // ogled: TRAY_BADGE_PREVIEW=<mapa> go test -run TestTrayTipAndBadge
			os.WriteFile(filepath.Join(dir, fmt.Sprintf("badge%d.png", w)), b, 0o644)
			os.WriteFile(filepath.Join(dir, fmt.Sprintf("plain%d.png", w)), ico[off:off+size], 0o644)
		}
	}
}
