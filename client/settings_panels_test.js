// Tests of the panel renderer of settings.html (run by serve_settings_panels_test.go with node). argv[2]: the folder of the shared fixtures
// (client/testdata); the code under test is prepended by the Go test (the section between the panel markers of the page).
"use strict";
const assert = require("node:assert/strict");
const fs = require("node:fs");
const dir = process.argv[2];
const fixture = n => fs.readFileSync(dir + "/" + n, "utf8");
const json = n => JSON.parse(fixture(n));
const t = (k, ...a) => k + (a.length ? ":" + a.join("|") : "");
let confirmAnswer = true, confirmLog = [];
const confirm = text => { confirmLog.push(text); return confirmAnswer; };

// ---- a DOM double: only the properties and tags the renderer is meant to use; anything else (innerHTML, href, style, setAttribute …) throws ----
const ALLOWED_PROPS = new Set(["className", "textContent", "value", "type", "checked", "disabled", "placeholder"]);
const ALLOWED_TAGS = new Set(["div", "span", "button", "input", "select", "option", "label", "h3"]);
const NODE_OWN = new Set(["tag", "children", "events", "append", "replaceChildren", "addEventListener", "focus", "setSelectionRange", "setAttribute"]);
function makeEl(tag) {
  const e = {tag, children: [], events: {}, className: "", textContent: "", value: "", disabled: false, checked: false};
  e.append = (...c) => { for (const x of c) e.children.push(x); };
  e.replaceChildren = (...c) => { e.children = c; };
  e.addEventListener = (n, f) => { (e.events[n] ||= []).push(f); };
  e.setAttribute = k => { throw new Error("setAttribute " + k); };
  e.focus = () => {}; e.setSelectionRange = () => {};
  return new Proxy(e, {set(target, k, v) {
    if (typeof k === "string" && !NODE_OWN.has(k) && !ALLOWED_PROPS.has(k)) throw new Error("forbidden property " + k);
    target[k] = v; return true;
  }});
}
const document = {activeElement: null, createElement: tag => { if (!ALLOWED_TAGS.has(tag)) throw new Error("forbidden tag " + tag); return makeEl(tag); }};
const walk = (n, f) => { f(n); for (const c of n.children) walk(c, f); };
const texts = n => { const out = []; walk(n, x => { if (x.textContent) out.push(x.textContent); if (x.tag === "input" && x.value) out.push(x.value); }); return out; };
const fire = (n, ev, ...a) => Promise.all((n.events[ev] || []).map(f => f(...a)));
const sleep = ms => new Promise(r => setTimeout(r, ms));

// ---- helpers ----
function fake(answers) {
  const f = {log: [], delays: {}, answers, async send(req) {
    const full = req.method + " " + req.path + (req.query ? "?" + req.query : "");
    f.log.push(full + (req.body ? " " + req.body : ""));
    const d = f.delays[full]; if (d) await sleep(d);
    const [status, body] = f.answers[full] || f.answers[req.method + " " + req.path] || [404, '{"error":{"message":"nothing here"}}'];
    return {status, body: typeof body === "string" ? body : JSON.stringify(body)};
  }};
  return f;
}
const answerPanels = json("panels.json");
const strings = loc => panelStringsParse(json("panels-strings-" + loc + ".json"));
const registry = json("panels-registry.json").builtin;
const descriptionOf = id => answerPanels.panels.find(p => p.description.id === id).description;
const panelOf = id => { const r = panelParse(descriptionOf(id), "builtin"); assert.equal(r.state, "ready", id + ": " + r.reason); return r.panel; };
const modelOf = (id, f, loc) => new PanelModel(panelOf(id), strings(loc || "C"), PANEL_ROUTES.builtin, f.send);

async function main() {
  // 1. the registry constant equals the one of Go and Swift
  assert.deepEqual(PANEL_ROUTES.builtin, registry);

  // 2. the Go answer loads, in the registry's order, with the texts of the chosen locale
  const sends = (locale, strs) => async req => (req.path === "/panels/strings" ? {status: 200, body: fixture("panels-strings-" + locale + ".json")} : {status: 200, body: strs || fixture("panels.json")});
  const cat = await panelCatalogLoad(sends("sl_SI"), "sl_SI");
  assert.deepEqual(cat.entries.map(e => e.id), ["builtin.users", "builtin.teams", "builtin.kb"]);
  assert.ok(cat.entries.every(e => e.parse.state === "ready" && e.provider === "builtin"));
  assert.equal(panelText(cat.strings, cat.entries[0].title), "Uporabniki in naprave");
  assert.equal(panelText(cat.strings, cat.entries[2].title), "Baza znanja");
  assert.equal(panelText((await panelCatalogLoad(sends("C"), "C")).strings, "builtin.users.title"), "Users and devices");
  assert.equal(panelText({table: {}}, "acme.missing"), "acme.missing");  // a missing key is shown as the key
  assert.equal(panelText({table: {}}, undefined), "");
  assert.equal(panelText({table: {}}, "__proto__"), "__proto__");
  // locale from the browser language: never a made-up region
  for (const [l, want] of [["sl-SI", "sl_SI"], ["sl", "sl"], ["en-US", "en_US"], ["zh-Hans-CN", "zh"], ["", "C"], ["../x", "C"], [undefined, "C"], ["de_AT", "de_AT"]]) assert.equal(panelLocale(l), want, String(l));
  // errors of the list call, a higher format, a registry entry under another provider
  const failing = (status, body) => async req => (req.path === "/panels/strings" ? {status: 200, body: fixture("panels-strings-C.json")} : {status, body});
  assert.deepEqual((await panelCatalogLoad(failing(404, '{"error":{"message":"Not found"}}'), "C")).error, {kind: "notFound"});
  assert.deepEqual((await panelCatalogLoad(failing(503, '{"error":{"type":"core_unavailable"}}'), "C")).error, {kind: "unavailable"});
  assert.deepEqual((await panelCatalogLoad(failing(500, '{"error":{"message":"sqlite: disk I/O error"}}'), "C")).error, {kind: "serverFault", status: 500});
  assert.deepEqual((await panelCatalogLoad(failing(200, '{"format":2,"panels":[]}'), "C")).error, {kind: "needsNewer"});
  assert.equal((await panelCatalogLoad(failing(200, "[]"), "C")).error.kind, "refused");
  const under = await panelCatalogLoad(failing(200, JSON.stringify({format: 1, panels: [{provider: "acme", description: descriptionOf("builtin.users")}, {description: {}}]})), "C");
  assert.equal(under.entries.length, 1);
  assert.equal(under.entries[0].parse.state, "refused");
  assert.ok(under.entries[0].parse.reason.includes("registry says acme"));
  const newer = {...descriptionOf("builtin.users"), format: 2, id: "builtin.future", sections: [{kind: "hologram"}]};
  const mixed = await panelCatalogLoad(failing(200, JSON.stringify({format: 1, panels: [{provider: "builtin", description: newer}, {provider: "builtin", description: descriptionOf("builtin.users")}]})), "C");
  assert.deepEqual(mixed.entries.map(e => e.id), ["builtin.future", "builtin.users"]);
  assert.deepEqual(mixed.entries[0].parse, {state: "needsNewer", format: 2});
  const failedStrings = await panelCatalogLoad(async req => (req.path === "/panels" ? {status: 200, body: '{"format":1,"panels":[]}'} : {status: 404, body: "{}"}), "sl_SI");
  assert.ok(failedStrings.stringsError && panelText(failedStrings.strings, "x.y") === "x.y");

  // 3. the format fixture, and the hostile table exactly as the fixture says for the renderer side
  const f1 = json("panels-format1.json");
  assert.equal(panelParse(f1.panel, "acme", f1.routes).state, "ready");
  assert.notEqual(panelParse(f1.panel, "acme", f1.routes.slice(1)).state, "ready");
  const edit = (doc, ptr, f, remove) => {
    const go = (node, path) => {
      const head = path[0];
      if (Array.isArray(node)) { const i = Number(head); if (path.length === 1) { if (remove) node.splice(i, 1); else node[i] = f(node[i]); } else go(node[i], path.slice(1)); }
      else if (path.length === 1) { if (remove) delete node[head]; else node[head] = f(node[head]); }
      else go(node[head], path.slice(1));
    };
    go(doc, ptr.slice(1).split("/"));
  };
  const hostile = json("panels-hostile.json").cases;
  assert.ok(hostile.length >= 60);
  for (const c of hostile) {
    const doc = JSON.parse(JSON.stringify(f1.panel));
    for (const [ptr, v] of Object.entries(c.set || {})) edit(doc, ptr, () => v);
    for (const ptr of c.remove || []) edit(doc, ptr, null, true);
    for (const [ptr, n] of Object.entries(c.grow || {})) edit(doc, ptr, a => { while (a.length < n) a.push(JSON.parse(JSON.stringify(a[0]))); return a; });
    for (const [ptr, n] of Object.entries(c.pad || {})) edit(doc, ptr, () => "x".repeat(n));
    const r = panelParse(doc, "acme", f1.routes);
    if (c.swift === "refuse") assert.notEqual(r.state, "ready", "accepted: " + c.name);
    else if (c.swift === "accept") assert.equal(r.state, "ready", "refused (this side ignores unknown keys): " + c.name + " " + r.reason);
    else {  // placeholder: a known format with an unknown element
      assert.equal(r.state, "ready", "refused: " + c.name + " " + r.reason);
      const orders = r.panel.sections[1], m = new PanelModel(r.panel, {table: {}}, f1.routes, async () => ({status: 404, body: "{}"}));
      assert.ok(!pSupportedSection(r.panel.sections[0]) || m.unsupportedCount(orders) > 0, "no visible placeholder: " + c.name);
    }
  }

  // 4. the shared table of exact requests, confirmations and refusals (the Swift model reads the same file)
  const cases = json("panels-requests.json").cases;
  assert.ok(cases.length >= 60);
  for (const c of cases) {
    const model = modelOf(c.panel, fake({}));
    const parts = c.section.split("/");
    let sec = model.panel.sections.find(s => s.id === parts[0]);
    if (parts.length > 1) sec = sec.detail.sections.find(s => s.id === parts[1]);
    const a = (c.kind === "actions" ? sec.actions : sec.rowActions).find(x => x.id === c.action);
    assert.ok(a, JSON.stringify(c));
    const row = c.row === undefined ? null : {id: "", values: c.row}, parent = c.parent === undefined ? null : {id: "", values: c.parent};
    const r = model.request(a, row, parent, c.values || {});
    if (c.want === "refused") assert.ok(r.error, "sent: " + JSON.stringify(c));
    else assert.deepEqual(r.request, c.want, JSON.stringify(c));
    const conf = model.confirmation(a, row);
    assert.equal(conf === null ? "none" : conf.destructive ? "destructive" : "plain", c.confirm, JSON.stringify(c));
  }

  // 5. templates, transport and the model's behaviour (ports of the Swift tests)
  assert.deepEqual(pTemplateParse("/users/{row.id}"), [{literal: "users"}, {ref: ["row", "id"]}]);
  for (const bad of ["users", "/users/", "/users//x", "/users/x{row.id}", "/users/{row.id}{row.id}", "/users/{id}", "/users/{row.}", "/users/..", "/users/%2e%2e", "/users/a b", "/users?x", "/users/{other.id}"]) assert.equal(pTemplateParse(bad), null, bad);
  assert.equal(pRouteOf("PATCH", "/users/{row.id}"), "PATCH users/{}");
  assert.equal(pResolveQuery("item={field.item}&mode=draft", {field: {item: "a b"}}), "item=a%20b&mode=draft");
  assert.equal(pResolveQuery("item={field.item}", {field: {item: "a&b=c"}}), "item=a%26b%3Dc");
  assert.equal(pEncodeQuery("x/../y?z&w=1 é"), "x%2F..%2Fy%3Fz%26w%3D1%20%C3%A9");
  assert.equal(pEncodeQuery("a\u0007b"), null);
  assert.equal(pEncode("!'()*"), "%21%27%28%29%2A");
  const routes = PANEL_ROUTES.builtin;
  assert.ok(pRouteAllows("PUT", "/bindings/dev%201", routes) && !pRouteAllows("PUT", "/bindings", routes) && !pRouteAllows("PUT", "/bindings/a/b", routes));
  assert.ok(!pRouteAllows("PUT", "/bindings/%2E", routes) && !pRouteAllows("PUT", "/bindings/a%5Cb", routes) && !pRouteAllows("PUT", "/bindings/a%0Ab", routes) && !pRouteAllows("PUT", "/bindings/%zz", routes));
  const fx = fake({});
  const send = panelTransport(fx.send, routes);
  for (const [m, p] of [["GET", "/users"], ["GET", "/panels"], ["GET", "/panels/strings"], ["PATCH", "/users/7"], ["DELETE", "/bindings/dev-1"], ["POST", "/kb/submissions/3/accept"], ["DELETE", "/teams/4/assignments"], ["GET", "/grants"]]) {
    const before = fx.log.length; await send({method: m, path: p, query: ""}); assert.equal(fx.log.length, before + 1, m + " " + p);
  }
  const before = fx.log.length;
  for (const [m, p] of [["POST", "/core/restore"], ["POST", "/core/backup"], ["POST", "/import"], ["POST", "/export"], ["DELETE", "/teams/1"], ["PUT", "/projects/1"], ["DELETE", "/users/7"], ["PATCH", "/users/7/x"],
    ["POST", "/kb/submissions/1/purge"], ["POST", "/panels"], ["GET", "/panels/x"], ["PATCH", "/users/.."], ["PATCH", "/users/%2e%2e"], ["PATCH", "/users/a%2Fb"], ["PATCH", "/users/"], ["GET", "/kb/search/rebuild"],
    ["GET", "/usage/events"], ["GET", "users"], ["DELETE", "/devices/abc"], ["GET", "/audit"], ["GET", "/general"], ["POST", "/general/token"], ["PATCH", "/agents/claude"], ["GET", "/providers"]]) {
    assert.equal((await send({method: m, path: p, query: ""})).status, 404, m + " " + p);
  }
  assert.equal(fx.log.length, before, "a refused request reached the network");

  // the users panel: loads refs then lists; labels; hide and search
  const userAnswers = {"GET /users": [200, {users: [{id: 1, name: "acme", active: true, owner: true}, {id: 2, name: "shop", active: true}, {id: 3, name: "old", active: false}]}],
    "GET /devices": [200, {devices: [{id: "dev-1", name: "Mac"}, {id: "dev-2", name: "PC"}]}], "GET /bindings": [200, {bindings: [{device: "dev-1", user: 1}]}]};
  const f2 = fake(userAnswers), um = modelOf("builtin.users", f2);
  await um.load();
  assert.deepEqual(f2.log, ["GET /devices", "GET /users", "GET /users", "GET /bindings"]);
  const [usersSec, bindSec] = um.panel.sections;
  const bound = um.visibleRows(bindSec, "bindings")[0];
  assert.deepEqual(bindSec.columns.map(c => um.cell(c, bound)), ["Mac", "acme"]);
  assert.deepEqual(um.visibleRows(usersSec, "users").map(r => r.id), ["1", "2"]);
  um.setShowHidden(true, "users"); assert.deepEqual(um.visibleRows(usersSec, "users").map(r => r.id), ["1", "2", "3"]);
  um.setSearch("SHO", "users"); assert.deepEqual(um.visibleRows(usersSec, "users").map(r => r.id), ["2"]);
  assert.deepEqual(um.rowActions(usersSec, {id: "1", values: {active: true}}).map(a => a.id), ["rename", "deactivate"]);
  assert.deepEqual(um.rowActions(usersSec, {id: "3", values: {active: false}}).map(a => a.id), ["rename", "activate"]);
  // sl texts
  const sl = modelOf("builtin.users", fake(userAnswers), "sl_SI");
  await sl.load();
  assert.equal(sl.cell(sl.panel.sections[0].columns[1], sl.visibleRows(sl.panel.sections[0], "users")[0]), "Aktiven");
  assert.equal(sl.confirmation(sl.panel.sections[0].rowActions[1], {id: "1", values: {name: "acme"}}).title, "Deaktiviram uporabnika acme?");
  // the name limit counts bytes like the core
  const add = usersSec.actions[0];
  assert.deepEqual(um.problems(add, {name: "č".repeat(64)}), {});
  assert.deepEqual(um.problems(add, {name: "č".repeat(65)}), {name: {tooLong: 128}});
  assert.deepEqual(um.problems(add, {name: "  "}), {name: {required: true}});
  // confirmation first, then send and reload; failures
  um.setSearch("", "users");
  const off = usersSec.rowActions[1], shop = um.visibleRows(usersSec, "users")[1];
  assert.equal(await um.perform(off, "users", shop, null, {}, false), false);
  assert.deepEqual(um.actionError.get(pListKey("users", "")), {kind: "refused", why: "the action needs a confirmation"});
  f2.answers["PATCH /users/2"] = [200, {ok: true}]; f2.log.length = 0;
  assert.equal(await um.perform(off, "users", shop, null, {}, true), true);
  assert.deepEqual(f2.log, ['PATCH /users/2 {"active":false}', "GET /devices", "GET /users", "GET /users", "GET /bindings"]);
  f2.answers["POST /users"] = [400, {error: {message: "name already used"}}];
  const before2 = um.data("users").rows;
  assert.equal(await um.perform(add, "users", null, null, {name: "acme"}, true), false);
  assert.deepEqual(um.actionError.get(pListKey("users", "")), {kind: "server", message: "name already used"});
  assert.equal(um.data("users").rows, before2);
  f2.answers["POST /users"] = [503, '{"error":{"type":"core_unavailable"}}'];
  await um.perform(add, "users", null, null, {name: "acme"}, true); assert.deepEqual(um.actionError.get(pListKey("users", "")), {kind: "unavailable"});
  f2.answers["POST /users"] = [500, '{"error":{"message":"internal error: sqlite: disk I/O error"}}'];
  await um.perform(add, "users", null, null, {name: "acme"}, true); assert.deepEqual(um.actionError.get(pListKey("users", "")), {kind: "serverFault", status: 500});
  f2.answers["POST /users"] = [500, "garbage"];
  await um.perform(add, "users", null, null, {name: "acme"}, true); assert.deepEqual(um.actionError.get(pListKey("users", "")), {kind: "serverFault", status: 500});

  // two quick triggers send once
  const f3 = fake(userAnswers), m3 = modelOf("builtin.users", f3);
  await m3.load();
  const on = m3.panel.sections[0].rowActions[2], row3 = m3.visibleRows(m3.panel.sections[0], "users")[1];
  f3.answers["PATCH /users/2"] = [200, {ok: true}]; f3.delays["PATCH /users/2"] = 80; f3.log.length = 0;
  const first = m3.perform(on, "users", row3, null, {}, true);
  await sleep(20);
  assert.equal(await m3.perform(on, "users", row3, null, {}, true), false);
  assert.equal(await first, true);
  assert.equal(f3.log.filter(l => l.startsWith("PATCH")).length, 1);
  assert.equal(m3.sending.size, 0);

  // untrusted text stays the same string; a confirmation fills a value once
  const nasty = ["<script>alert(1)</script>", "<img src=x onerror=alert(1)>", "[x](https://evil.example)", "**b** _i_ `c`", "\\*raw\\*", "{name} {row.id}", "%@ %d", "a‮b", "javascript:alert(1)", "\"quoted\" 'single'"];
  const f4 = fake({...userAnswers, "GET /users": [200, {users: nasty.map((n, i) => ({id: i + 1, name: n, active: true}))}]}), m4 = modelOf("builtin.users", f4);
  await m4.load();
  const shown = m4.visibleRows(m4.panel.sections[0], "users");
  assert.deepEqual(shown.map(r => m4.cell(m4.panel.sections[0].columns[0], r)), nasty);
  assert.equal(m4.confirmation(m4.panel.sections[0].rowActions[1], shown[5]).title, "Deactivate {name} {row.id}?");

  // refs over the limit are an error, not a truncation
  const f5 = fake({...userAnswers, "GET /users": [200, {users: Array.from({length: 1001}, (_, i) => ({id: i + 1, name: "u" + i}))}]}), m5 = modelOf("builtin.users", f5);
  await m5.load();
  assert.deepEqual(m5.refError, {kind: "refused", why: "more than 1000 entries in /users"});
  assert.deepEqual(m5.refs.users, []);

  // a provider's route that is not registered is never sent
  const f6 = fake({"GET /users": [200, {users: []}]}), m6 = new PanelModel(panelOf("builtin.users"), {table: {}}, ["GET users"], f6.send);
  await m6.load();
  assert.deepEqual(f6.log, ["GET /users", "GET /users"]);
  assert.ok(m6.request(m6.panel.sections[0].actions[0], null, null, {name: "x"}).error);

  // the format fixture: paging, detail pages, scope picker, record with `when`, stale answers, load more once, cursor, duplicate keys
  const tabs = json("panels-format1-strings.json");
  const fp = panelParse(f1.panel, "acme", f1.routes).panel, orders = fp.sections[1];
  const shopAnswers = {"GET /plugins/acme/items": [200, {items: [{sku: "s1", title: "Shirt"}, {sku: "s2", title: "Hat"}]}],
    "GET /plugins/acme/orders?item=s1": [200, {orders: [{id: 5, name: "first", paid: true, state: "new", item: "s1", created: "2026-11-17T10:00:00Z", note: "a\nb"}], next: 6}],
    "GET /plugins/acme/orders?item=s1&after=6": [200, {orders: [{id: 6, name: "second", state: "sent"}], next: 0}],
    "GET /plugins/acme/orders?item=s2": [200, {orders: [{id: 7, name: "of s2"}]}],
    "GET /plugins/acme/orders/5/lines": [200, {lines: [{id: 9, title: "one"}]}], "GET /plugins/acme/orders/5/summary": [200, {title: "s", note: "long"}]};
  const f7 = fake(shopAnswers), m7 = new PanelModel(fp, {locale: "C", table: tabs.C}, f1.routes, f7.send);
  await m7.load();
  assert.equal(m7.data("orders").next, "6");
  await m7.loadMore(orders, "orders");
  assert.equal(f7.log.at(-1), "GET /plugins/acme/orders?item=s1&after=6");
  assert.deepEqual(m7.data("orders").rows.map(r => r.id), ["5", "6"]);
  assert.equal(m7.data("orders").next, null);
  const row = m7.data("orders").rows[0];
  assert.deepEqual(orders.columns.map(c => m7.cell(c, row)).slice(0, 5), ["first", "a\nb", "Paid", "New", "Shirt"]);
  assert.notEqual(m7.cell(orders.columns[5], row), "2026-11-17T10:00:00Z");
  f7.log.length = 0;
  await m7.openDetail(orders, row);
  assert.deepEqual(f7.log.slice(-2), ["GET /plugins/acme/orders/5/lines", "GET /plugins/acme/orders/5/summary"]);
  const plain = {id: "7", values: {id: 7, paid: false}};
  assert.ok(!m7.recordVisible(orders.detail.sections[2], plain) && m7.recordVisible(orders.detail.sections[2], row));
  const lines = orders.detail.sections[1], line = m7.data("orders/lines", row).rows[0];
  assert.deepEqual(m7.request(lines.actions[0], null, row, {title: "two"}).request, {method: "POST", path: "/plugins/acme/orders/5/lines", query: "", body: '{"title":"two"}'});
  assert.deepEqual(m7.request(lines.rowActions[0], line, row, {}).request, {method: "DELETE", path: "/plugins/acme/orders/5/lines/9", query: "", body: ""});
  // stale answers of the picker are dropped
  f7.delays["GET /plugins/acme/orders?item=s1"] = 150; f7.delays["GET /plugins/acme/orders?item=s2"] = 10;
  const one = m7.select("s1", orders, "orders"); await sleep(20); const two = m7.select("s2", orders, "orders");
  await Promise.all([one, two]);
  assert.deepEqual(m7.data("orders").rows.map(r => r.id), ["7"]);
  assert.equal(m7.selected.get(pListKey("orders", "")), "s2");
  assert.equal(m7.data("orders").loading, false);
  // two quick Load more clicks send the cursor once
  const f8 = fake({...shopAnswers, "GET /plugins/acme/orders?item=s1": [200, {orders: [{id: 1}], next: "c2"}], "GET /plugins/acme/orders?item=s1&after=c2": [200, {orders: [{id: 10}]}]}), m8 = new PanelModel(fp, {table: {}}, f1.routes, f8.send);
  await m8.load();
  f8.delays["GET /plugins/acme/orders?item=s1&after=c2"] = 60; f8.log.length = 0;
  const a1 = m8.loadMore(orders, "orders"); await sleep(15); const a2 = m8.loadMore(orders, "orders"); await Promise.all([a1, a2]);
  assert.deepEqual(f8.log, ["GET /plugins/acme/orders?item=s1&after=c2"]);
  assert.deepEqual(m8.data("orders").rows.map(r => r.id), ["1", "10"]);
  // a cursor is a query value; one that cannot be sent is an error, the rows stay
  for (const [cursor, want] of [["x/../y?z&w=1 é", "item=s1&after=x%2F..%2Fy%3Fz%26w%3D1%20%C3%A9"], ["..", "item=s1&after=.."]]) {
    const f9 = fake({...shopAnswers, "GET /plugins/acme/orders?item=s1": [200, {orders: [{id: 1}], next: cursor}]}), m9 = new PanelModel(fp, {table: {}}, f1.routes, f9.send);
    await m9.load(); await m9.loadMore(orders, "orders");
    assert.equal(f9.log.at(-1), "GET /plugins/acme/orders?" + want);
  }
  const f10 = fake({...shopAnswers, "GET /plugins/acme/orders?item=s1": [200, {orders: [{id: 1}], next: "a\u0007b"}]}), m10 = new PanelModel(fp, {table: {}}, f1.routes, f10.send);
  await m10.load(); await m10.loadMore(orders, "orders");
  assert.deepEqual(m10.data("orders").error, {kind: "refused", why: "the paging cursor cannot be sent"});
  assert.equal(m10.data("orders").rows.length, 1);
  // rows without a unique key are not shown
  for (const orders2 of [[{id: 1}, {id: 1}], [{name: "a"}, {id: 2}]]) {
    const f11 = fake({...shopAnswers, "GET /plugins/acme/orders?item=s1": [200, {orders: orders2}]}), m11 = new PanelModel(fp, {table: {}}, f1.routes, f11.send);
    await m11.load();
    assert.equal(m11.data("orders").error.kind, "cannotShow"); assert.deepEqual(m11.data("orders").rows, []);
  }
  const f12 = fake({...shopAnswers, "GET /plugins/acme/orders?item=s1": [200, {orders: [{id: "#1"}, {id: 2}]}]}), m12 = new PanelModel(fp, {table: {}}, f1.routes, f12.send);
  await m12.load(); assert.equal(m12.data("orders").error, null); assert.deepEqual(m12.data("orders").rows.map(r => r.id), ["#1", "2"]);
  // a long text is cut with a marker
  const body = {kind: "multiline", field: "body", label: "x"}, big = {id: "", values: {body: "x".repeat(PL.multiline + 5)}};
  assert.equal(m7.cell(body, big).length, PL.multiline); assert.ok(m7.cellIsTruncated(body, big)); assert.ok(!m7.cellIsTruncated(body, {id: "", values: {body: "short"}}));

  // 6. the view: hostile texts reach the DOM as text only
  const bad = ["<img src=x onerror=alert(1)>", "<script>alert(1)</script>", "javascript:alert(1)", "\"quoted\" 'single' `tick`", "a‮b", "[x](https://evil.example)", "</div><b>x</b>", "&lt;b&gt;"];
  const desc = {format: 1, id: "acme.shop", provider: "acme", title: "acme.shop.t", about: "acme.shop.a", symbol: "tray", refs: {items: {path: "/plugins/acme/items", items: "items", key: "sku", label: "title"}},
    sections: [{kind: "text", id: "intro", body: "acme.shop.intro"},
      {kind: "list", id: "orders", title: "acme.shop.orders", source: {path: "/plugins/acme/orders", items: "orders"}, key: "id",
        scope: {ref: "items", param: "item", label: "acme.shop.scope"},
        columns: [{field: "name", kind: "text", label: "acme.shop.name"}, {field: "note", kind: "multiline", label: "acme.shop.note"}, {field: "state", kind: "enum", label: "acme.shop.state", values: {new: "acme.shop.new"}}],
        filter: ["name"], empty: "acme.shop.empty",
        actions: [{id: "add", label: "acme.shop.add", method: "POST", path: "/plugins/acme/orders", fields: [{id: "name", type: "text", label: "acme.shop.fname", required: true},
          {id: "kind", type: "choice", label: "acme.shop.kind", options: [{value: "web", label: "acme.shop.web"}]}, {id: "item", type: "ref", ref: "items", label: "acme.shop.item"}], body: {name: {field: "name"}}}],
        rowActions: [{id: "delete", label: "acme.shop.delete", method: "DELETE", path: "/plugins/acme/orders/{row.id}", confirm: {title: "acme.shop.ct", message: "acme.shop.cm", button: "acme.shop.cb"}}],
        detail: {title: "name", sections: [{kind: "record", id: "rec", title: "acme.shop.rec", source: {path: "/plugins/acme/orders/{parent.id}"}, columns: [{field: "note", kind: "multiline", label: "acme.shop.note"}, {field: "name", kind: "text", label: "acme.shop.name"}]}]}}]};
  const keys = Object.fromEntries(["t", "a", "intro", "orders", "scope", "name", "note", "state", "new", "empty", "add", "fname", "kind", "web", "item", "delete", "ct", "cm", "cb", "rec"].map((k, i) => ["acme.shop." + k, bad[i % bad.length] + " " + k]));
  const routesAcme = ["GET plugins/acme/items", "GET plugins/acme/orders", "POST plugins/acme/orders", "DELETE plugins/acme/orders/{}", "GET plugins/acme/orders/{}"];
  const parsed = panelParse(desc, "acme", routesAcme);
  assert.equal(parsed.state, "ready", parsed.reason);
  const f13 = fake({"GET /plugins/acme/items": [200, {items: [{sku: "<s1>", title: bad[2]}]}],
    "GET /plugins/acme/orders?item=%3Cs1%3E": [200, {orders: bad.map((b, i) => ({id: i + 1, name: b, note: b + "\n" + b, state: i ? "other " + b : "new"}))}],
    "GET /plugins/acme/orders/1": [200, {name: bad[0], note: bad[1]}], "DELETE /plugins/acme/orders/2": [500, {error: {message: "sqlite: disk I/O error " + bad[0]}}]});
  const vm = new PanelModel(parsed.panel, {locale: "C", table: keys}, routesAcme, f13.send), host = makeEl("div");
  const view = panelView(vm, host);
  await vm.load();
  let all = texts(host);
  for (const b of bad) assert.ok(all.some(x => x.includes(b)), "not shown as text: " + b);
  const tags = new Set(); walk(host, n => tags.add(n.tag));
  assert.ok([...tags].every(x => ALLOWED_TAGS.has(x)), [...tags].join());
  // the form, the confirmation (the browser's own plain-text dialog) and a server message that is hostile
  const addBtn = []; walk(host, n => { if (n.tag === "button" && n.textContent === keys["acme.shop.add"]) addBtn.push(n); });
  assert.equal(addBtn.length, 1);
  await fire(addBtn[0], "click");
  all = texts(host);
  assert.ok(all.some(x => x.includes(keys["acme.shop.fname"])) && all.some(x => x.includes(keys["acme.shop.web"])) && all.some(x => x.includes(bad[2])));
  const inputs = []; walk(host, n => { if (n.tag === "input" && n.type === "text" && n.placeholder === undefined) inputs.push(n); });
  assert.ok(inputs.length >= 1);
  const rowSelects = []; walk(host, n => { if (n.tag === "select" && n.children.some(o => o.textContent === keys["acme.shop.delete"])) rowSelects.push(n); });
  assert.equal(rowSelects.length, 8);  // one action menu per row
  // a 5xx message with internals is not shown; a 4xx message is, as text
  f13.answers["DELETE /plugins/acme/orders/3"] = [400, {error: {message: bad[1]}}];
  for (const [i, hidden] of [[1, true], [2, false]]) {  // row 2 answers 500 (internals), row 3 answers 400 with a hostile message
    walk(host, n => { if (n.tag === "button" && n.textContent === t("cancel")) fire(n, "click"); });
    const sels = []; walk(host, n => { if (n.tag === "select" && n.children.some(o => o.textContent === keys["acme.shop.delete"])) sels.push(n); });
    const sel = sels[i]; sel.value = "0";
    confirmLog.length = 0;
    await fire(sel, "change");
    await sleep(30);
    assert.ok(confirmLog.length === 1 && confirmLog[0].includes(keys["acme.shop.ct"]) && confirmLog[0].includes(keys["acme.shop.cm"]));
    const t2 = texts(host);
    if (hidden) { assert.ok(t2.some(x => x === "pFault (HTTP 500)")); assert.ok(!t2.some(x => x.includes(bad[0] + " ") && x.startsWith("pFault"))); }
    else assert.ok(t2.some(x => x === bad[1]));
  }
  // declining the dialog sends nothing
  confirmAnswer = false; f13.log.length = 0;
  const sels2 = []; walk(host, n => { if (n.tag === "select" && n.children.some(o => o.textContent === keys["acme.shop.delete"])) sels2.push(n); });
  sels2[3].value = "0"; await fire(sels2[3], "change"); await sleep(20);
  assert.ok(!f13.log.some(l => l.startsWith("DELETE")));
  confirmAnswer = true;
  // opening the detail page shows the record as text
  const opens = []; walk(host, n => { if (n.tag === "button" && n.textContent === t("pOpen")) opens.push(n); });
  assert.equal(opens.length, 8);
  await fire(opens[0], "click"); await sleep(30);
  const t3 = texts(host);
  assert.ok(t3.some(x => x.includes(bad[1])) && t3.some(x => x.includes(bad[0])));
  const tags2 = new Set(); walk(host, n => tags2.add(n.tag)); assert.ok([...tags2].every(x => ALLOWED_TAGS.has(x)));
}
main().then(() => console.log("ok"), e => { console.error(e); process.exit(1); });
