package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A Foundry Local that is not installed is a normal state: the models route answers 200 with installed:false, not a 503 the page logs as an error.
func TestFoundryModelsRouteNotInstalledIsNotAnError(t *testing.T) {
	s, _, base := liveServer(t, func(c *serveConfig) {
		c.Foundry = foundryConfig{Enabled: true, Executable: filepath.Join(t.TempDir(), "no-such-foundry")}
	})
	st, b := adminNoise(t, s, base, s.admin, "GET", adminPrefix+"/foundry/models", "")
	if st != 200 || !strings.Contains(b, `"installed":false`) || !strings.Contains(b, `"models":[]`) {
		t.Fatalf("not installed: %d %s", st, b)
	}
}

// The Foundry pane of the settings page says so in words, and a not-installed answer (or an older gateway's 503) leaves it showing that.
func TestFoundryPaneSaysItIsNotInstalled(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	start := strings.Index(settingsPage, "  async foundry() {")
	end := strings.Index(settingsPage, "  async general() {")
	if start < 0 || end < start {
		t.Fatal("foundry pane markers")
	}
	script := `
const assert = require('node:assert/strict');
const t = k => k;
const el = (tag, props, ...kids) => ({tag, props: props || {}, kids: kids.flat()});
const note = x => el('div', {textContent: x});
const row = (...k) => el('row', {}, ...k);
const group = (...k) => el('group', {}, ...k);
const labelSub = x => el('span', {textContent: x});
const toggle = () => el('toggle'), button = () => el('button'), buttons = () => el('buttons'), card = () => el('card'), section = () => el('h3');
const autosave = () => {}, save = () => {}, show = () => {}, nav = () => {}, ICON = {foundry: []};
const texts = n => [n.props && n.props.textContent, ...(n.kids || []).flatMap(texts)].filter(Boolean);
const pages = {` + settingsPage[start:end] + `};
(async () => {
  for (const models of [{installed: false, models: []}, new Error('503')]) {
    const calls = [];
    const api = async (m, p) => {
      calls.push(m + ' ' + p);
      if (p === '/foundry') return {enabled: true, path: '', found: false, running: false, instructions: '', models: null};
      if (models instanceof Error) throw models;
      return models;
    };
    globalThis.api = api;
    const out = await pages.foundry();
    assert.ok(calls.includes('GET /foundry/models'));
    assert.ok(out.flatMap(texts).includes('foundryNotInstalled'), JSON.stringify(out.flatMap(texts)));
  }
})().catch(e => { console.error(e); process.exit(1); });
`
	f := filepath.Join(t.TempDir(), "foundry.js")
	if err := os.WriteFile(f, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, f).CombinedOutput(); err != nil {
		t.Fatalf("foundry pane: %v\n%s", err, out)
	}
}
