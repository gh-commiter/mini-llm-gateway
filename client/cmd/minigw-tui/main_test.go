package main

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// fake: testdata (canned answers and the fake minigw script) in a temp dir.
func fake(t *testing.T) (*ui, string) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake minigw is a shell script")
	}
	dir := t.TempDir()
	files, _ := os.ReadDir("testdata")
	for _, f := range files {
		b, err := os.ReadFile(filepath.Join("testdata", f.Name()))
		if err != nil {
			t.Fatal(err)
		}
		os.WriteFile(filepath.Join(dir, f.Name()), b, 0o755)
	}
	return &ui{minigw: filepath.Join(dir, "fake-minigw"), extra: []string{"--config", "/x/serve.json"}, open: map[string]bool{}}, dir
}

func read(t *testing.T, p string) string {
	b, _ := os.ReadFile(p)
	return string(b)
}

func TestCallSendsSecretsOnStdinOnly(t *testing.T) {
	u, dir := fake(t)
	if _, err := u.call("POST", "/providers", map[string]any{"name": "x", "key": "sk-SECRET"}); err != nil {
		t.Fatal(err)
	}
	if calls := read(t, filepath.Join(dir, "calls.log")); calls != "gw api POST /providers --config /x/serve.json\n" {
		t.Fatalf("argv: %q", calls)
	}
	if in := read(t, filepath.Join(dir, "stdin.log")); !strings.Contains(in, `"key":"sk-SECRET"`) {
		t.Fatalf("stdin: %q", in)
	}
}

func TestCallError(t *testing.T) {
	u, _ := fake(t)
	_, err := u.call("GET", "/agents/grok", nil)
	if err == nil || err.Error() != "not found: /agents/grok" {
		t.Fatalf("err: %v", err)
	}
	u.minigw = filepath.Join(t.TempDir(), "missing")
	if _, err := u.call("GET", "/status", nil); err == nil {
		t.Fatal("no error for a missing minigw")
	}
}

func TestSetModel(t *testing.T) {
	all := []string{"a", "b", "c"}
	for _, c := range []struct {
		enabled []string
		name    string
		on      bool
		want    []string
	}{
		{nil, "b", false, []string{"a", "c"}},  // all → all but one
		{[]string{"a", "c"}, "b", true, nil},   // all again → null (also later models)
		{[]string{}, "a", true, []string{"a"}}, // none → one
		{[]string{"a"}, "a", false, []string{}},
	} {
		got := setModel(c.enabled, all, c.name, c.on)
		if (got == nil) != (c.want == nil) || !slices.Equal(got, c.want) {
			t.Fatalf("setModel(%v, %s, %v) = %#v, want %#v", c.enabled, c.name, c.on, got, c.want)
		}
	}
}

// screen: a page as the terminal shows it (80 columns).
func screen(t *testing.T, m model) (model, string) {
	t.Helper()
	m.w, m.h = 80, 60
	next, _ := m.Update(m.reload("", false)())
	m = next.(model)
	if m.loadFailed != nil {
		t.Fatalf("%s: %v", pages[m.tab].title, m.loadFailed)
	}
	return m, m.View()
}

func press(t *testing.T, m model, keys ...tea.KeyMsg) model {
	t.Helper()
	for _, k := range keys {
		next, cmd := m.Update(k)
		m = next.(model)
		if cmd != nil {
			if msg := cmd(); msg != nil {
				next, _ = m.Update(msg)
				m = next.(model)
			}
		}
	}
	return m
}

var enter = tea.KeyMsg{Type: tea.KeyEnter}

// The pages render from the admin API's answers; go test -v shows them.
func TestPages(t *testing.T) {
	u, dir := fake(t)
	want := []string{"Status                                  running on port 8765", "Bob's MacBook", "Models…", "GPT-6.1 Sol", "/home/ana/notes"}
	for i := range pages {
		_, v := screen(t, model{u: u, tab: i})
		t.Logf("\n%s", v)
		if !strings.Contains(v, want[i]) {
			t.Fatalf("page %s lacks %q", pages[i].title, want[i])
		}
		for _, l := range strings.Split(v, "\n") {
			if len([]rune(l)) > 80 {
				t.Fatalf("wider than 80: %q", l)
			}
		}
		if strings.Contains(v, "SECRET") {
			t.Fatalf("token shown on %s", pages[i].title)
		}
	}
	u.models = "OpenRouter"
	_, v := screen(t, model{u: u, tab: 2})
	t.Logf("\n%s", v)
	if !strings.Contains(v, "> [ ← Providers ]") || !strings.Contains(v, "[x]") {
		t.Fatalf("models: %s", v)
	}
	if calls := read(t, filepath.Join(dir, "calls.log")); strings.Contains(calls, "SECRET") {
		t.Fatalf("argv: %s", calls)
	}
}

// The token appears only after "Show the connect line"; a typed key is masked and goes on stdin.
func TestTokenAndKeyEntry(t *testing.T) {
	u, dir := fake(t)
	m, _ := screen(t, model{u: u})
	for m.rows[m.cur].label != "Show the connect line (with the token)" {
		m = press(t, m, tea.KeyMsg{Type: tea.KeyDown})
	}
	m = press(t, m, enter)
	if v := m.View(); !strings.Contains(v, "tok-SECRET-123") {
		t.Fatalf("connect line not shown:\n%s", v)
	}
	m = press(t, m, tea.KeyMsg{Type: tea.KeyTab}, tea.KeyMsg{Type: tea.KeyShiftTab}) // another page hides it
	if strings.Contains(m.View(), "SECRET") {
		t.Fatal("token still shown after changing the page")
	}

	m, _ = screen(t, model{u: u, tab: 2})
	for m.rows[m.cur].label != "API key" {
		m = press(t, m, tea.KeyMsg{Type: tea.KeyDown})
	}
	m = press(t, m, enter, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("sk-TYPED")})
	if v := m.View(); strings.Contains(v, "sk-TYPED") || !strings.Contains(v, "API key: ••••••••") {
		t.Fatalf("key not masked:\n%s", v)
	}
	m = press(t, m, enter)
	if m.bad {
		t.Fatalf("save: %s", m.note)
	}
	if in := read(t, filepath.Join(dir, "stdin.log")); !strings.Contains(in, `"key":"sk-TYPED"`) {
		t.Fatalf("stdin: %q", in)
	}
	if calls := read(t, filepath.Join(dir, "calls.log")); strings.Contains(calls, "sk-TYPED") || !strings.Contains(calls, "gw api POST /providers") {
		t.Fatalf("argv: %q", calls)
	}
}

// A change asks first where the page asks (y/n); n changes nothing.
func TestConfirm(t *testing.T) {
	u, dir := fake(t)
	m, _ := screen(t, model{u: u, tab: 1})
	for m.rows[m.cur].label != "Remove" {
		m = press(t, m, tea.KeyMsg{Type: tea.KeyDown})
	}
	m = press(t, m, enter)
	if !m.asking || !strings.Contains(m.View(), "Remove Bob's MacBook? It can no longer connect (y/n)") {
		t.Fatalf("no question:\n%s", m.View())
	}
	m = press(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("n")})
	if strings.Contains(read(t, filepath.Join(dir, "calls.log")), "DELETE") {
		t.Fatal("deleted after n")
	}
	m = press(t, m, enter, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("y")})
	if !strings.Contains(read(t, filepath.Join(dir, "calls.log")), "gw api DELETE /devices/Qm9iJ3MgTWFjQm9vayBkZXZpY2Uga2V5IDMyIGJ5dGVz") {
		t.Fatalf("calls: %s", read(t, filepath.Join(dir, "calls.log")))
	}
}
