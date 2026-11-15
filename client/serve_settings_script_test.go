package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Stran z nastavitvami: skripta mora biti veljaven JavaScript (0.5.24 krog 3: manjkajoč oklepaj je ustavil celo stran).
// Preveri node --check, kjer je node; sicer preskoči.
func TestSettingsScriptSyntax(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	page := strings.NewReplacer("{{NONCE}}", "n", "{{PICKER}}", "false").Replace(settingsPage)
	m := regexp.MustCompile(`(?s)<script[^>]*>(.*?)</script>`).FindAllStringSubmatch(page, -1)
	if len(m) == 0 {
		t.Fatal("no script in settings.html")
	}
	// HPX360 0.5.25: General vrne null za neobstoječe skupine; replaceChildren jih brez filtra izpiše kot »nullnull«
	if !strings.Contains(page, `replaceChildren(...(await pages[page](arg)).filter(k => k !== null`) {
		t.Fatal("show(): page parts not filtered for null before replaceChildren")
	}
	for i, s := range m {
		f := filepath.Join(t.TempDir(), "s.js")
		if err := os.WriteFile(f, []byte(s[1]), 0o600); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command(node, "--check", f).CombinedOutput(); err != nil {
			t.Fatalf("script %d: %v\n%s", i, err, out)
		}
	}
}

func TestSettingsRefusedFolders(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	start := strings.Index(settingsPage, "function folderList(")
	end := strings.Index(settingsPage[start:], "\n// agentPane") + start
	// A small DOM double rejects HTML assignment; server paths must stay literal text.
	script := `
const assert = require('node:assert/strict');
const PICKER = false;
const t = key => key;
function el(tag, props, ...children) {
  const n = {tag, children, classList: {toggle() {}}, addEventListener() {},
    replaceChildren(...c) { this.children = c; }};
  Object.defineProperty(n, 'innerHTML', {set() { throw Error('HTML assignment'); }});
  return Object.assign(n, props);
}
const row = (...c) => el('div', {}, ...c);
const note = text => el('div', {textContent: text});
const bad = '/projects/<img src=x onerror=alert(1)>';
` + settingsPage[start:end] + `
for (const refused of [undefined, [], [bad]]) {
  const result = folderList(bad + ',/ok', () => {}, '', false, [], [], refused);
  const labels = result.children[0].children.slice(0, 2).map(r => r.children[0]);
  const warning = !!refused?.length;
  assert.equal(labels[0].textContent, (warning ? '⚠️ ' : '📁 ') + bad + (warning ? ' · folderRefused' : ''));
  assert.equal(labels[0].class.includes('warn'), warning);
  assert.equal(labels[1].textContent, '📁 /ok');
}
`
	if strings.Count(settingsPage, "win, c.leftOut, c.leftOutUnchecked, c.refused)") != 2 {
		t.Fatal("both allowed-folder panes must pass refused paths")
	}
	f := filepath.Join(t.TempDir(), "folders.js")
	if err := os.WriteFile(f, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, f).CombinedOutput(); err != nil {
		t.Fatalf("folder list: %v\n%s", err, out)
	}
}
