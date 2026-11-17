package main

import (
	_ "embed"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

//go:embed settings_panels_test.js
var panelsScriptTest string

const (
	panelsFrom = "// --- paneli iz opisov: logika ---"
	panelsTo   = "// --- /paneli iz opisov: prikaz ---"
)

// panelCode: the renderer of the panels in settings.html (logic and view), and the loader that fetches them.
func panelCode(t *testing.T) (renderer, loader string) {
	t.Helper()
	a := strings.Index(settingsPage, panelsFrom)
	b := strings.Index(settingsPage, panelsTo)
	if a < 0 || b < a {
		t.Fatal("the panel renderer markers are not in settings.html")
	}
	renderer = settingsPage[a : b+len(panelsTo)]
	c := strings.Index(settingsPage, "const PANELS = {")
	d := strings.Index(settingsPage[c:], "const gwIcon") + c
	if c < 0 || d < c {
		t.Fatal("the panel loader is not in settings.html")
	}
	return renderer, settingsPage[c:d]
}

// The renderer puts nothing from a description, a string table or an answer into the page except as text: no HTML sink, no
// attribute built from data, no code from a string, and no network call outside the loader.
func TestSettingsPanelRendererHasNoForbiddenSinks(t *testing.T) {
	renderer, loader := panelCode(t)
	for name, code := range map[string]string{"renderer": renderer, "loader": loader} {
		for _, bad := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "createContextualFragment", "setAttribute", "srcdoc", "eval(", "new Function", "importScripts",
			"window.open", "location.href", "location =", "location.assign", "<a ", "<img", "<script", "<iframe", "javascript:", "`"} {
			if strings.Contains(code, bad) {
				t.Errorf("%s uses %s", name, bad)
			}
		}
		// no assignment of an attribute-like property (href, src, style, title, an on* handler): data never reaches one
		if m := regexp.MustCompile(`\.(href|src|style|title|cssText|on[a-z]+)\s*=[^=]`).FindString(code); m != "" {
			t.Errorf("%s assigns %s", name, m)
		}
	}
	if strings.Contains(renderer, "fetch(") || strings.Contains(renderer, "XMLHttpRequest") || strings.Contains(renderer, "WebSocket") {
		t.Error("the renderer makes a network call itself: only the transport it is given may")
	}
	// the page stays self-contained: nothing loaded from outside, no inline handler or style attribute in the document
	for _, bad := range []string{"<link", "src=\"http", "href=\"http", "@import", "url(http", "onclick=", "onerror=", "<iframe", "<object", "<embed"} {
		if strings.Contains(settingsPage, bad) {
			t.Errorf("settings.html contains %s", bad)
		}
	}
}

// The renderer script against the shared fixtures (client/testdata/panels*.json) that the Go validator and the Swift model also read.
func TestSettingsPanelRenderer(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	if _, err := os.Stat("testdata/panels-requests.json"); err != nil {
		t.Skip("fixtures are not here: " + err.Error())
	}
	renderer, _ := panelCode(t)
	file := filepath.Join(t.TempDir(), "panels.js")
	if err := os.WriteFile(file, []byte(renderer+"\n"+panelsScriptTest), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, file, "testdata").CombinedOutput(); err != nil {
		t.Fatalf("panel renderer: %v\n%s", err, out)
	}
}

// With a settings session the page reaches GET /panels and GET /panels/strings like any admin route; without one, 401.
func TestSettingsSessionReachesThePanels(t *testing.T) {
	s, _, base := liveServer(t, nil)
	host := strings.TrimPrefix(base, "http://")
	u := s.settingsLoginURL()
	tok := u[strings.Index(u, "#t=")+3:]
	_, b, _ := settingsCall(t, base, host, "POST", "/minigw/settings/login", "", `{"t":"`+tok+`"}`)
	session := regexp.MustCompile(`"session":"([^"]+)"`).FindStringSubmatch(b)
	if session == nil {
		t.Fatalf("login: %s", b)
	}
	for _, p := range []string{"/panels", "/panels/strings?lang=sl_SI"} {
		if st, _, _ := settingsCall(t, base, host, "GET", adminPrefix+p, "", ""); st != 401 {
			t.Errorf("%s without a session: %d", p, st)
		}
	}
	st, body, h := settingsCall(t, base, host, "GET", adminPrefix+"/panels", session[1], "")
	var panels struct {
		Format int
		Panels []struct {
			Provider    string
			Description struct{ ID string }
		}
	}
	if st != 200 || json.Unmarshal([]byte(body), &panels) != nil || len(panels.Panels) != 3 || panels.Panels[0].Description.ID != "builtin.users" || h.Get("Cache-Control") != "no-store" {
		t.Fatalf("GET /panels: %d %s", st, body)
	}
	st, body, _ = settingsCall(t, base, host, "GET", adminPrefix+"/panels/strings?lang=sl_SI", session[1], "")
	if st != 200 || !strings.Contains(body, `"locale":"sl_SI"`) || !strings.Contains(body, "Uporabniki in naprave") {
		t.Fatalf("GET /panels/strings: %d %s", st, body)
	}
	if st, _, _ := settingsCall(t, base, host, "GET", adminPrefix+"/panels/strings?lang=../x", session[1], ""); st != 400 {
		t.Errorf("a bad lang: %d", st)
	}
	// a foreign Host is refused before the session is looked at
	if st, _, _ := settingsCall(t, "http://"+host, "evil.example:"+strings.Split(host, ":")[1], "GET", adminPrefix+"/panels", session[1], ""); st != 403 {
		t.Errorf("GET /panels with a foreign Host: %d", st)
	}
}
