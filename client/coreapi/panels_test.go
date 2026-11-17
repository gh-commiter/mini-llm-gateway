package coreapi

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
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
		"a key in the wrong case": func(s string) string { return strings.Replace(s, `"title":{"en":"Shop"`, `"Title":{"en":"Shop"`, 1) },
		"more than 64 KiB":        func(s string) string { return s[:len(s)-1] + `,"x":"` + strings.Repeat("a", 64<<10) + `"}` },
		"trailing data":           func(s string) string { return s + ` {}` },
		"not an object":           func(string) string { return `[1]` },
		"format as a string":      func(s string) string { return strings.Replace(s, `"format":1`, `"format":"1"`, 1) },
		"more than 12 sections": func(s string) string {
			return strings.Replace(s, `"sections":[`, `"sections":[`+strings.Repeat(`{"kind":"text","id":"t","body":{"en":"a","sl":"b"}},`, 12), 1)
		},
		"the same section id twice": func(s string) string {
			return strings.Replace(s, `"sections":[`, `"sections":[{"kind":"text","id":"intro","body":{"en":"a","sl":"b"}},`, 1)
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
	many := PanelProvider{ID: "builtin", Routes: good.Routes}
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
