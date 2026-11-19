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
	if !strings.Contains(page, `replaceChildren(...content.filter(k => k !== null`) {
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

func TestSettingsUsage(t *testing.T) {
	if !strings.Contains(settingsPage, `usage: () => usagePane()`) || !strings.Contains(settingsPage, `["usage", t("usage"), ICON.usage]`) {
		t.Fatal("usage page must be reachable from navigation")
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	start := strings.Index(settingsPage, "function usageNumber(")
	end := strings.Index(settingsPage[start:], "// --- panoji ---") + start
	script := `
const assert = require('node:assert/strict');
const t = (k, ...args) => k + (args.length ? ':' + args.join('|') : '');
function el(tag, props, ...children) {
  const n = {tag, children:children.flat(), attrs:{}, events:{},
    append(...c) { this.children.push(...c); },
    replaceChildren(...c) { this.children = c; },
    setAttribute(k,v) { this.attrs[k] = String(v); },
    addEventListener(k,v) { this.events[k] = v; }};
  Object.defineProperty(n, 'innerHTML', {set() { throw Error('HTML assignment'); }});
  return Object.assign(n, props);
}
const document = {createElementNS: (_, tag) => el(tag, {})};
const note = text => el('div', {textContent:text});
const button = (text, onclick) => el('button', {textContent:text, onclick});
const section = text => el('h3', {textContent:text});
const card = () => el('div', {});
const ICON = {usage:[]};
const RealDate = Date;
Date = class extends RealDate { constructor(...a) { super(...(a.length ? a : ['2026-11-16T12:00:00Z'])); } };
` + settingsPage[start:end] + `
assert.equal(usageNumber(46100000), '46.1M');
assert.equal(usageNumber(914000), '914K');
assert.equal(usageNumber(999950), '1M');
assert.equal(usageNumber(0), '0');
const bad = '<img src=x onerror=alert(1)>';
const rows = Array.from({length:8}, (_, i) => ({day:'2026-11-15', provider:'p', model:i ? 'm'+i : bad,
  events:i+1, tokensIn:100, tokensOut:20, reportedCostUsd:i ? '0.1' : null,
  estimatedCostUsd:i === 1 ? '0.2' : null, eventsWithoutCost:i === 0 ? 1 : 0}));
rows.push({...rows[0], day:'2026-11-14', provider:'other', events:2});
rows.push({...rows[2], events:4});
for (const measure of ['tokens','spend','requests']) {
  const d = usageGroups(rows, measure, 7, '2026-11-16');
  assert.equal(d.total.tokens, rows.reduce((n,r) => n+r.tokensIn+r.tokensOut,0));
  assert.equal(d.total.requests, rows.reduce((n,r) => n+r.events,0));
  assert.ok(Math.abs(d.total.spend - rows.reduce((n,r) => n+Number(r.reportedCostUsd)+Number(r.estimatedCostUsd),0)) < 1e-9);
  assert.equal(d.series.length,7);
  assert.equal(d.ranked.length,9);
  assert.ok(d.ranked.every((m,i) => !i || d.ranked[i-1][measure] >= m[measure]));
  assert.deepEqual(d.series.slice(0,6).map(m => m.key),d.ranked.slice(0,6).map(m => m.key));
  assert.equal(d.series[6].key,null);
  assert.ok(Math.abs(d.series.reduce((n,s) => n+s[measure],0)-d.total[measure]) < 1e-9);
  assert.ok(Math.abs(d.bars.flatMap(b => b.segments).reduce((n,s) => n+s[measure],0)-d.total[measure]) < 1e-9);
  assert.equal(d.bars.length,7);
  assert.equal(d.bars[6].segments.reduce((n,s) => n+s.requests,0),0);
}
assert.equal(usageValue(usageSum([rows[0]]),'spend'),'—');
assert.equal(usageValue(usageSum([{reportedCostUsd:'1'}]),'spend'),'$1.00');
assert.equal(usageValue(usageSum([{reportedCostUsd:'0'}]),'spend'),'$0.00');
assert.equal(usageValue(usageSum([{reportedCostUsd:'0',estimatedCostUsd:'0.2'}]),'spend'),'$0.00 + ~$0.20');
assert.equal(usageValue(usageSum([{estimatedCostUsd:'0.2'}]),'spend'),'~$0.20');
assert.equal(usageValue(usageSum([{reportedCostUsd:'1',estimatedCostUsd:'0.2'}]),'spend'),'$1.00 + ~$0.20');
assert.ok(usageNotes({gaps:[]},usageSum(rows),'spend').includes('unknownCost:2'));
const failedTime = usageSum([{failedDurationMs:3480000,unknownDurationEvents:2},{failedDurationMs:60000}]);
assert.equal(usageValue(failedTime,'failed'),'59.0 min');
assert.ok(usageNotes({gaps:[]},failedTime,'failed').includes('unknownDuration:2'));
assert.equal(usageGroups([{day:'2026-11-15',provider:'p',model:'m',events:1,failedDurationMs:120000}],'failed',7,'2026-11-16').total.failed,2);
assert.deepEqual(usageNotes({gaps:[{count:3,from:'a',to:'b'}]},usageSum([]),'tokens'),['usageGap:3|a|b']);
const d = usageGroups([rows[0]],'requests',7,'2026-11-16');
assert.equal(d.series.length,1);
const skewed = usageGroups([rows[0]],'requests',7,'2026-11-01');
assert.equal(skewed.bars.length,8);
assert.equal(skewed.bars.at(-1).day,'2026-11-15');
assert.equal(skewed.bars.flatMap(b => b.segments).reduce((n,s) => n+s.requests,0),skewed.total.requests);
assert.equal(usageGroups([],'tokens',7,'2026-03-02').bars[0].day,'2026-02-24');
const table = usageTable(d,'requests');
assert.equal(table.children[2].children[0].children[0].children[1].textContent,bad);
assert.equal(usageTable(usageGroups([{...rows[0],provider:bad}],'requests',7,'2026-11-16'),'requests')
  .children[2].children[0].children[1].textContent,bad);
const tip = note(''), chart = usageChart(d,'requests',tip);
const segment = chart.children.find(n => n.tag === 'rect');
assert.ok(segment.attrs['aria-label'].includes(bad));
segment.events.focus(); assert.ok(tip.textContent.includes(bad));
document.activeElement = segment;
segment.events.mouseleave(); assert.ok(tip.textContent.includes(bad));
segment.events.blur(); assert.equal(tip.textContent,'');
const smallChart = usageChart(usageGroups([{...rows[0],reportedCostUsd:'0.000003'}],'spend',7,'2026-11-16'),'spend',tip);
assert.equal(smallChart.children.filter(n => n.tag === 'text')[3].textContent,'$0.000003');
const manyChart = usageChart(usageGroups(rows,'requests',7,'2026-11-16'),'requests',tip);
const rects = manyChart.children.filter(n => n.tag === 'rect');
assert.equal(rects.filter(n => n.attrs.tabindex === '0').length,1);
rects[1].focus = () => rects[1].events.focus();
rects[0].events.keydown({key:'ArrowRight',preventDefault() {}});
assert.equal(rects[0].attrs.tabindex,'-1');
assert.equal(rects[1].attrs.tabindex,'0');
let response = {days:[],gaps:[]}, reject = false, calls = [];
const api = async (_, path) => { calls.push(path); if (reject) throw Error('offline'); return response; };
async function check() {
  const parts = usagePane(), body = parts[3], tabs = parts[2], select = parts[1].children[2];
  await new Promise(setImmediate);
  assert.equal(body.children.at(-1).textContent,'usageEmpty');
  assert.equal(body.attrs['aria-live'],undefined);
  assert.equal(tabs.children[0].attrs['aria-selected'],'true');
  const tokenTab = tabs.children[0];
  response = {days:rows,gaps:[]}; select.value = '30'; select.events.change();
  await new Promise(setImmediate);
  assert.equal(calls.at(-1),'/usage/events?days=30');
  tabs.children[1].onclick();
  assert.equal(tabs.children[0],tokenTab);
  assert.equal(tabs.children[1].attrs['aria-selected'],'true');
  assert.equal(tabs.children[1].attrs['aria-controls'],body.id);
  let focused = false;
  tabs.children[2].focus = () => { focused = true; };
  tabs.children[1].events.keydown({key:'ArrowRight',preventDefault() {}});
  assert.equal(focused,true);
  assert.equal(tabs.children[2].attrs['aria-selected'],'true');
  tabs.children[1].onclick();
  assert.ok(body.children.some(n => n.textContent === 'unknownCost:2'));
  reject = true; select.value = '90'; select.events.change();
  await new Promise(setImmediate);
  assert.equal(calls.at(-1),'/usage/events?days=90');
  assert.equal(body.children[0].textContent,'usageError:offline');
  reject = false;
  let finish;
  response = new Promise(resolve => { finish = resolve; });
  select.value = '30'; select.events.change();
  response = {days:[],gaps:[]}; select.value = '7'; select.events.change();
  await new Promise(setImmediate);
  finish({days:rows,gaps:[]});
  await new Promise(setImmediate);
  assert.equal(body.children.at(-1).textContent,'usageEmpty');
}
check().catch(e => { console.error(e); process.exitCode = 1; });
`
	f := filepath.Join(t.TempDir(), "usage.js")
	if err := os.WriteFile(f, []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(node, f).CombinedOutput(); err != nil {
		t.Fatalf("usage: %v\n%s", err, out)
	}
}
