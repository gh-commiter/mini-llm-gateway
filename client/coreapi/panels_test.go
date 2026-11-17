package coreapi

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

// The shared fixtures live in client/testdata (the Swift tests read the same files). A standalone test binary without
// the source tree skips them.
func panelFixture(t *testing.T, name string, v any) {
	t.Helper()
	b, err := os.ReadFile("../testdata/" + name)
	if err != nil {
		t.Skipf("fixture %s is not here: %v", name, err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}
}

func TestBuiltinPanelsValidate(t *testing.T) {
	p := builtinProvider()
	if len(p.Panels) == 0 {
		t.Fatal("no built-in panel")
	}
	for _, raw := range p.Panels {
		if err := ValidatePanel(raw, p); err != nil {
			t.Errorf("%s: %v", raw[:80], err)
		}
	}
}

func TestPanelsAnswerMatchesFixture(t *testing.T) {
	var want any
	panelFixture(t, "panels.json", &want)
	r := New(Options{}).Handle(context.Background(), Context{Kind: KindAdmin}, Request{Method: "GET", Path: "/panels"})
	var got any
	if r.Status != 200 || json.Unmarshal(r.Body, &got) != nil {
		t.Fatalf("GET /panels: %d %s", r.Status, r.Body)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("GET /panels differs from client/testdata/panels.json:\n%s", r.Body)
	}
	var reg map[string][]string
	panelFixture(t, "panels-registry.json", &reg)
	if !reflect.DeepEqual(reg["builtin"], BuiltinRoutes) {
		t.Fatalf("registry fixture %v, BuiltinRoutes %v", reg["builtin"], BuiltinRoutes)
	}
}

type formatFixture struct {
	Provider string
	Routes   []string
	Panel    any
}

func TestFormat1FixtureValidates(t *testing.T) {
	var f formatFixture
	panelFixture(t, "panels-format1.json", &f)
	raw, _ := json.Marshal(f.Panel)
	if err := ValidatePanel(raw, PanelProvider{ID: f.Provider, Routes: f.Routes}); err != nil {
		t.Fatal(err)
	}
	// every route the fixture registers is used, and a provider without them refuses the panel
	if err := ValidatePanel(raw, PanelProvider{ID: f.Provider, Routes: f.Routes[1:]}); err == nil {
		t.Fatal("a missing registered route must refuse the panel")
	}
}

// --- tiny JSON pointer edits for the hostile table ---

func walk(t *testing.T, root any, ptr string) (parent any, key string) {
	t.Helper()
	parts := strings.Split(strings.TrimPrefix(ptr, "/"), "/")
	cur := root
	for _, p := range parts[:len(parts)-1] {
		switch c := cur.(type) {
		case map[string]any:
			cur = c[p]
		case []any:
			cur = c[atoi(t, p)]
		default:
			t.Fatalf("pointer %s", ptr)
		}
	}
	return cur, parts[len(parts)-1]
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			t.Fatalf("index %q", s)
		}
		n = n*10 + int(r-'0')
	}
	return n
}

func TestHostilePanels(t *testing.T) {
	var base formatFixture
	panelFixture(t, "panels-format1.json", &base)
	var table struct {
		Cases []struct {
			Name   string
			Set    map[string]any
			Remove []string
			Grow   map[string]int
			Pad    map[string]int
		}
	}
	panelFixture(t, "panels-hostile.json", &table)
	if len(table.Cases) < 40 {
		t.Fatalf("only %d cases", len(table.Cases))
	}
	for _, c := range table.Cases {
		raw, _ := json.Marshal(base.Panel)
		var doc any
		json.Unmarshal(raw, &doc)
		for ptr, v := range c.Set {
			parent, key := walk(t, doc, ptr)
			switch p := parent.(type) {
			case map[string]any:
				p[key] = v
			case []any:
				p[atoi(t, key)] = v
			}
		}
		for _, ptr := range c.Remove {
			parent, key := walk(t, doc, ptr)
			delete(parent.(map[string]any), key)
		}
		for ptr, n := range c.Grow { // the first element, repeated up to n
			parent, key := walk(t, doc, ptr)
			var list []any
			if m, ok := parent.(map[string]any); ok {
				list = m[key].([]any)
				for len(list) < n {
					list = append(list, list[0])
				}
				m[key] = list
			} else {
				a := parent.([]any)
				list = a[atoi(t, key)].([]any)
				for len(list) < n {
					list = append(list, list[0])
				}
				a[atoi(t, key)] = list
			}
		}
		for ptr, n := range c.Pad {
			parent, key := walk(t, doc, ptr)
			parent.(map[string]any)[key] = strings.Repeat("x", n)
		}
		out, _ := json.Marshal(doc)
		if err := ValidatePanel(out, PanelProvider{ID: base.Provider, Routes: base.Routes}); err == nil {
			t.Errorf("%s: the validator accepted it", c.Name)
		}
	}
}

func TestPanelValidatorEdges(t *testing.T) {
	var f formatFixture
	panelFixture(t, "panels-format1.json", &f)
	p := PanelProvider{ID: f.Provider, Routes: f.Routes}
	raw, _ := json.Marshal(f.Panel)
	if err := ValidatePanel(raw, p); err != nil {
		t.Fatal(err)
	}
	for name, mut := range map[string]func(string) string{
		"a key in the wrong case": func(s string) string {
			return strings.Replace(s, `"title":"acme.shop.title"`, `"Title":"acme.shop.title"`, 1)
		},
		"more than 64 KiB":   func(s string) string { return s[:len(s)-1] + `,"x":"` + strings.Repeat("a", 64<<10) + `"}` },
		"trailing data":      func(s string) string { return s + ` {}` },
		"not an object":      func(string) string { return `[1]` },
		"format as a string": func(s string) string { return strings.Replace(s, `"format":1`, `"format":"1"`, 1) },
		"more than 12 sections": func(s string) string {
			return strings.Replace(s, `"sections":[`, `"sections":[`+strings.Repeat(`{"kind":"text","id":"t","body":"acme.shop.section.t.body"},`, 12), 1)
		},
		"the same section id twice": func(s string) string {
			return strings.Replace(s, `"sections":[`, `"sections":[{"kind":"text","id":"intro","body":"acme.shop.section.intro.body"},`, 1)
		},
	} {
		if err := ValidatePanel([]byte(mut(string(raw))), p); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestPanelsLeftOutWhenInvalidOrTooMany(t *testing.T) {
	var logged []string
	a := New(Options{Log: func(f string, args ...any) { logged = append(logged, f) }})
	good := builtinProvider()
	bad := PanelProvider{ID: "builtin", Routes: nil, Panels: good.Panels}
	if n := len(a.panelsAnswer([]PanelProvider{bad})); n != 0 || len(logged) == 0 {
		t.Fatalf("a panel with unregistered routes was served (%d), log %v", n, logged)
	}
	many := PanelProvider{ID: "builtin", Routes: good.Routes, Strings: good.Strings}
	for i := 0; i < 11; i++ {
		many.Panels = append(many.Panels, good.Panels[0])
	}
	if n := len(a.panelsAnswer([]PanelProvider{many})); n != 10 {
		t.Fatalf("%d panels, want the first 10", n)
	}
}

func TestPanelsRouteIsAdminOnly(t *testing.T) {
	a := New(Options{})
	ctx := context.Background()
	if r := a.Handle(ctx, Context{Kind: KindAdmin}, Request{Method: "GET", Path: "/panels"}); r.Status != 200 {
		t.Fatalf("admin GET /panels: %d", r.Status)
	}
	if r := a.Handle(ctx, Context{Kind: KindAdmin}, Request{Method: "POST", Path: "/panels"}); r.Status != 405 {
		t.Fatalf("admin POST /panels: %d", r.Status)
	}
	if IsAdminPath("GET", "/panels/x") || IsMemberPath("/minigw/v1/panels") {
		t.Fatal("only /panels is a panel path, and not a member path")
	}
	cap := Context{Kind: KindRun, Approved: true}
	for _, c := range []struct {
		name string
		cx   Context
		path string
	}{
		{"member", Context{Kind: KindMember, DeviceID: "dev-ana", Approved: true}, "/minigw/v1/panels"},
		{"member with the admin path", Context{Kind: KindMember, DeviceID: "dev-ana", Approved: true}, "/panels"},
		{"run", cap, "/minigw/v1/panels"},
		{"run with the admin path", cap, "/panels"},
		{"internal", Context{Kind: KindInternal}, "/panels"},
	} {
		r := a.Handle(ctx, c.cx, Request{Method: "GET", Path: c.path})
		if r.Status == 200 || strings.Contains(string(r.Body), "builtin.users") {
			t.Errorf("%s: GET %s answered %d %s", c.name, c.path, r.Status, r.Body)
		}
	}
}

func TestStringTablesCoverTheBuiltinPanels(t *testing.T) {
	p := builtinProvider()
	used := map[string]int{}
	for _, raw := range p.Panels {
		keys, err := PanelKeys(raw, p)
		if err != nil {
			t.Fatal(err)
		}
		for k, m := range keys {
			used[k] = max(used[k], m)
		}
	}
	for _, loc := range []string{"C", "en_US", "sl_SI"} {
		tab, ok := p.Strings[loc]
		if !ok {
			t.Fatalf("no table for %s", loc)
		}
		if err := ValidateStringTable(p.ID, tab); err != nil {
			t.Errorf("%s: %v", loc, err)
		}
		for k, v := range tab {
			m, isUsed := used[k]
			if !isUsed {
				t.Errorf("%s: the entry %s is used by no panel", loc, k)
			} else if len([]rune(v)) > m {
				t.Errorf("%s: %s is longer than the %d its position allows", loc, k, m)
			}
		}
	}
	for k := range used { // C holds every key, sl_SI too; en_US only what differs
		for _, loc := range []string{"C", "sl_SI"} {
			if _, ok := p.Strings[loc][k]; !ok {
				t.Errorf("%s has no entry for %s", loc, k)
			}
		}
	}
}

func TestStringsAnswerMatchesFixtures(t *testing.T) {
	for _, loc := range []string{"C", "sl_SI"} {
		var want any
		panelFixture(t, "panels-strings-"+loc+".json", &want)
		r := New(Options{}).Handle(context.Background(), Context{Kind: KindAdmin}, Request{Method: "GET", Path: "/panels/strings", RawQuery: "lang=" + loc})
		var got any
		if r.Status != 200 || json.Unmarshal(r.Body, &got) != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: %d %s", loc, r.Status, r.Body)
		}
	}
}

func TestStringsLocaleChoice(t *testing.T) {
	a := New(Options{})
	ask := func(q string) (int, string) {
		r := a.Handle(context.Background(), Context{Kind: KindAdmin}, Request{Method: "GET", Path: "/panels/strings", RawQuery: q})
		var o struct{ Locale string }
		json.Unmarshal(r.Body, &o)
		return r.Status, o.Locale
	}
	for q, want := range map[string]string{"lang=sl_SI": "sl_SI", "lang=sl": "sl_SI", "lang=en": "en_US", "lang=de": "C", "lang=sl_AT": "sl_SI", "lang=en_US": "en_US", "lang=en_GB": "en_US", "lang=de_DE": "C", "lang=C": "C", "": "C"} {
		if st, loc := ask(q); st != 200 || loc != want {
			t.Errorf("%q: %d %s, want %s", q, st, loc, want)
		}
	}
	for _, q := range []string{"lang=sl_si", "lang=s", "lang=SL", "lang=../x", "lang=c", "lang=sl_SI%0a", "lang=sl-SI"} {
		if st, _ := ask(q); st != 400 {
			t.Errorf("%q: %d, want 400", q, st)
		}
	}
	if r := a.Handle(context.Background(), Context{Kind: KindAdmin}, Request{Method: "POST", Path: "/panels/strings"}); r.Status != 405 {
		t.Fatalf("POST: %d", r.Status)
	}
	if IsAdminPath("GET", "/panels/strings/x") || IsAdminPath("GET", "/panels/other") {
		t.Fatal("only /panels and /panels/strings")
	}
	for _, cx := range []Context{{Kind: KindMember, DeviceID: "d", Approved: true}, {Kind: KindRun, Approved: true}} {
		if r := a.Handle(context.Background(), cx, Request{Method: "GET", Path: "/panels/strings"}); r.Status == 200 {
			t.Errorf("%s served the strings", cx.Kind)
		}
	}
}

func TestMergedStringsFallBackToTheBase(t *testing.T) {
	a := New(Options{})
	ps := []PanelProvider{{ID: "acme", Strings: map[string]map[string]string{
		"C": {"acme.a": "A", "acme.b": "B"}, "sl_SI": {"acme.a": "Ah"}, "sl_AT": {"acme.b": "Bh"}}}}
	got := a.mergedStrings(ps, "sl_SI")
	if got["acme.a"] != "Ah" || got["acme.b"] != "Bh" { // exact locale, then another table of the language, then C
		t.Fatalf("%v", got)
	}
	if got := a.mergedStrings(ps, "C"); got["acme.a"] != "A" {
		t.Fatalf("%v", got)
	}
}

func TestStringTableRefusals(t *testing.T) {
	long := strings.Repeat("x", 601)
	many := map[string]string{}
	for i := 0; i < 2001; i++ {
		many["acme.k"+strconv.Itoa(i)] = "x"
	}
	for name, tab := range map[string]map[string]string{
		"a foreign key":            {"builtin.a": "x"},
		"a bad key":                {"acme.A": "x"},
		"an empty text":            {"acme.a": " "},
		"a long text":              {"acme.a": long},
		"a control char":           {"acme.a": "a\u0007b"},
		"a bidi char":              {"acme.a": "a\u202eb"},
		"a format specifier brace": {"acme.a": "%d {0}"},
		"a stray brace":            {"acme.a": "hi {name"},
		"an upper case var":        {"acme.a": "hi {Name}"},
		"too many entries":         many,
	} {
		if len(tab) > 0 && ValidateStringTable("acme", tab) == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := ValidateStringTable("acme", map[string]string{"acme.a": "Deactivate {name}? 100% <b>plain</b>"}); err != nil {
		t.Fatal(err)
	}
}

func TestFormat1StringsFixture(t *testing.T) {
	var f formatFixture
	panelFixture(t, "panels-format1.json", &f)
	var tabs map[string]map[string]string
	panelFixture(t, "panels-format1-strings.json", &tabs)
	raw, _ := json.Marshal(f.Panel)
	p := PanelProvider{ID: f.Provider, Routes: f.Routes}
	keys, err := PanelKeys(raw, p)
	if err != nil {
		t.Fatal(err)
	}
	for loc, tab := range tabs {
		if err := ValidateStringTable(f.Provider, tab); err != nil {
			t.Fatalf("%s: %v", loc, err)
		}
		if len(tab) != len(keys) {
			t.Errorf("%s: %d entries for %d keys", loc, len(tab), len(keys))
		}
		for k := range keys {
			if _, ok := tab[k]; !ok {
				t.Errorf("%s: no entry for %s", loc, k)
			}
		}
	}
}

func TestProviderStringsAreCheckedWhenServing(t *testing.T) {
	good := builtinProvider()
	if err := checkProviderStrings(good); err != nil {
		t.Fatal(err)
	}
	var logged []string
	a := New(Options{Log: func(f string, args ...any) { logged = append(logged, fmt.Sprintf(f, args...)) }})
	copyTables := func() map[string]map[string]string {
		out := map[string]map[string]string{}
		for l, tab := range good.Strings {
			out[l] = map[string]string{}
			for k, v := range tab {
				out[l][k] = v
			}
		}
		return out
	}
	long := copyTables()
	long["sl_SI"]["builtin.users.title"] = strings.Repeat("x", 121) // a title is a label: 120
	missing := copyTables()
	delete(missing["C"], "builtin.users.about")
	nobase := copyTables()
	delete(nobase, "C")
	for name, tabs := range map[string]map[string]map[string]string{"a text over its position's limit": long, "a key missing in C": missing, "no C table": nobase} {
		p := PanelProvider{ID: "builtin", Routes: good.Routes, Panels: good.Panels, Strings: tabs}
		if checkProviderStrings(p) == nil {
			t.Errorf("%s: accepted", name)
		}
		logged = nil
		if n := len(a.panelsAnswer([]PanelProvider{p})); n != 0 || len(logged) == 0 {
			t.Errorf("%s: %d panels served, log %v", name, n, logged)
		}
		if m := a.mergedStrings([]PanelProvider{p}, "sl_SI"); len(m) != 0 {
			t.Errorf("%s: texts served", name)
		}
	}
}

func TestProviderIDsAreReserved(t *testing.T) {
	var logged []string
	extra := PanelProvider{ID: "builtin", Routes: []string{"GET users"}}
	a := New(Options{PanelProviders: []PanelProvider{extra, {ID: "Bad Id"}, {ID: "acme"}, {ID: "acme"}}, Log: func(f string, args ...any) { logged = append(logged, f) }})
	ps := a.providers()
	if len(ps) != 2 || ps[0].ID != "builtin" || ps[1].ID != "acme" || len(logged) != 3 {
		t.Fatalf("%d providers %v, log %v", len(ps), ps, logged)
	}
	if len(ps[0].Routes) != len(BuiltinRoutes) {
		t.Fatal("a plugin changed the built-in routes")
	}
	// every description is served under the id of the provider that registered it; one that claims another id is refused
	bad := PanelProvider{ID: "acme", Routes: nil, Panels: builtinProvider().Panels}
	if n := len(a.panelsAnswer([]PanelProvider{bad})); n != 0 {
		t.Fatalf("a description that says builtin was served under acme (%d)", n)
	}
	r := New(Options{}).Handle(context.Background(), Context{Kind: KindAdmin}, Request{Method: "GET", Path: "/panels"})
	var o struct{ Panels []struct{ Provider string } }
	json.Unmarshal(r.Body, &o)
	if len(o.Panels) != len(builtinProvider().Panels) || len(o.Panels) < 2 {
		t.Fatalf("%d panels", len(o.Panels))
	}
	for _, p := range o.Panels {
		if p.Provider != "builtin" {
			t.Fatalf("a panel under %q", p.Provider)
		}
	}
}

func TestKeysBelowTheTopLevelAreExactToo(t *testing.T) {
	var f formatFixture
	panelFixture(t, "panels-format1.json", &f)
	p := PanelProvider{ID: f.Provider, Routes: f.Routes}
	raw, _ := json.Marshal(f.Panel)
	for name, mut := range map[string]func(string) string{
		"Method in a row action": func(s string) string { return strings.Replace(s, `"method":"PATCH"`, `"Method":"PATCH"`, 1) },
		"a long s in source":     func(s string) string { return strings.Replace(s, `"source"`, `"\u017fource"`, 1) },
		"a Kelvin sign in a key": func(s string) string { return strings.Replace(s, `"key":"id"`, `"\u212aey":"id"`, 1) },
		"the right key and a cased twin": func(s string) string {
			return strings.Replace(s, `"method":"PATCH"`, `"method":"PATCH","METHOD":"PATCH"`, 1)
		},
		"a cased key in a nested column": func(s string) string { return strings.Replace(s, `"kind":"multiline"`, `"Kind":"multiline"`, 1) },
	} {
		if err := ValidatePanel([]byte(mut(string(raw))), p); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestBuiltinPanelOrderIsExplicit(t *testing.T) {
	ents, _ := builtinPanelFiles.ReadDir("panels")
	if len(ents) != len(builtinPanelOrder) {
		t.Fatalf("%d panel files, %d in the order list", len(ents), len(builtinPanelOrder))
	}
	var ids []string
	for _, raw := range builtinProvider().Panels {
		var d struct{ ID string }
		json.Unmarshal(raw, &d)
		ids = append(ids, d.ID)
	}
	if !reflect.DeepEqual(ids, []string{"builtin.users", "builtin.teams", "builtin.kb"}) {
		t.Fatalf("order %v", ids)
	}
	// the answer keeps it
	r := New(Options{}).Handle(context.Background(), Context{Kind: KindAdmin}, Request{Method: "GET", Path: "/panels"})
	var o struct {
		Panels []struct{ Description struct{ ID string } }
	}
	json.Unmarshal(r.Body, &o)
	if len(o.Panels) != 3 || o.Panels[0].Description.ID != "builtin.users" || o.Panels[1].Description.ID != "builtin.teams" || o.Panels[2].Description.ID != "builtin.kb" {
		t.Fatalf("%s", r.Body)
	}
}
