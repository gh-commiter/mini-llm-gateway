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
		// crude: a property reached by a computed name (["inner"+"HTML"]), a write to .style., Object.assign on a node
		for _, re := range []string{`[\w)\]]\[[^\]\n]*["'][^\]\n]*\+[^\]\n]*\]`, `\.style\.`, `Object\.assign\(`, `\[\s*["'](inner|outer)`} {
			if m := regexp.MustCompile(re).FindString(code); m != "" && name == "renderer" {
				t.Errorf("%s: %s", name, m)
			}
		}
		// no assignment of an attribute-like property (href, src, style, title, an on* handler): data never reaches one
		if m := regexp.MustCompile(`\.(href|src|style|title|cssText|on[a-z]+)\s*=[^=]`).FindString(code); m != "" {
			t.Errorf("%s assigns %s", name, m)
		}
	}
	if !strings.Contains(loader, `redirect: "error"`) || !strings.Contains(loader, "status: 0") || strings.Contains(loader, "core_unavailable") {
		t.Error("panelFetch: redirect must be refused, and a failed call is its own state (status 0), not a made-up 503")
	}
	if strings.Contains(renderer, "fetch(") || strings.Contains(renderer, "XMLHttpRequest") || strings.Contains(renderer, "WebSocket") {
		t.Error("the renderer makes a network call itself: only the transport it is given may")
	}
	// the page stays self-contained: nothing loaded from outside, no inline handler or style attribute in the document
	if strings.Count(settingsPage, "<link") != 1 || !strings.Contains(settingsPage, `<link rel="icon" href="data:,">`) {
		t.Error("the page must have exactly one <link>: the empty data icon that stops the browser's /favicon.ico request (401 in the console)")
	}
	for _, bad := range []string{"src=\"http", "href=\"http", "@import", "url(http", "onclick=", "onerror=", "<iframe", "<object", "<embed"} {
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
	if st != 200 || json.Unmarshal([]byte(body), &panels) != nil || len(panels.Panels) != 4 || panels.Panels[0].Description.ID != "builtin.users" || panels.Panels[3].Description.ID != "builtin.upstreams" || h.Get("Cache-Control") != "no-store" {
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

// The page may demand Trusted Types for scripts only because it has no string-to-HTML sink at all (old panes included). The
// inventory: every sink Trusted Types guards is searched for, and none is found; setAttribute is called with a literal name
// everywhere except the two helpers that draw the usage chart from literal attribute objects.
func TestSettingsPageHasNoStringToHTMLSink(t *testing.T) {
	for _, sink := range []string{"innerHTML", "outerHTML", "insertAdjacentHTML", "document.write", "srcdoc", "eval(", "new Function", "createContextualFragment", "DOMParser",
		"createElement(\"script\")", "importScripts", "setTimeout(\"", "setInterval(\"", "setTimeout('", "setInterval('", "setAttribute(\"on", "setAttribute('on", "unsafe-inline", "unsafe-eval"} {
		// the one comment that explains the el() helper names the word; code lines are what counts
		for _, line := range strings.Split(settingsPage, "\n") {
			code := line
			if i := strings.Index(code, "//"); i >= 0 && !strings.Contains(code[:i], "\"") && !strings.Contains(code[:i], "'") {
				code = code[:i]
			}
			if strings.Contains(code, sink) {
				t.Errorf("sink %q in: %s", sink, strings.TrimSpace(line))
			}
		}
	}
	dynamic := 0
	for _, m := range regexp.MustCompile(`setAttribute\(([^,)]+),`).FindAllStringSubmatch(settingsPage, -1) {
		if !strings.HasPrefix(m[1], "\"") {
			dynamic++
		}
	}
	if dynamic != 2 { // usageSVG and the devices glyph: both loop over attribute objects written as literals in this file
		t.Errorf("%d setAttribute calls with a computed name, want the 2 known helpers", dynamic)
	}
}

func TestSettingsPageDemandsTrustedTypes(t *testing.T) {
	_, _, base := liveServer(t, nil)
	host := strings.TrimPrefix(base, "http://")
	_, _, h := settingsCall(t, base, host, "GET", "/minigw/settings", "", "")
	csp := h.Get("Content-Security-Policy")
	for _, want := range []string{"default-src 'none'", "require-trusted-types-for 'script'", "frame-ancestors 'none'"} {
		if !strings.Contains(csp, want) {
			t.Errorf("CSP lacks %s: %s", want, csp)
		}
	}
	if strings.Contains(csp, "unsafe") {
		t.Errorf("CSP: %s", csp)
	}
}

// The browser asks for /favicon.ico on its own. Before, that fell through to the model routes and answered 401 (a line in the console of
// every load); now the settings server answers 204 with no body, without a session, under the same Host and loopback checks as the page.
func TestFaviconAnswers204(t *testing.T) {
	_, _, base := liveServer(t, nil)
	host := strings.TrimPrefix(base, "http://")
	st, body, h := settingsCall(t, base, host, "GET", "/favicon.ico", "", "")
	if st != 204 || body != "" || h.Get("Cache-Control") != "max-age=86400" {
		t.Fatalf("GET /favicon.ico: %d %q %v", st, body, h)
	}
	if st, _, _ := settingsCall(t, base, "evil.example:"+strings.Split(host, ":")[1], "GET", "/favicon.ico", "", ""); st != 403 {
		t.Fatalf("a foreign Host: %d", st)
	}
	if st, _, _ := settingsCall(t, base, host, "POST", "/favicon.ico", "", ""); st != 405 {
		t.Fatalf("POST /favicon.ico: %d", st)
	}
}
