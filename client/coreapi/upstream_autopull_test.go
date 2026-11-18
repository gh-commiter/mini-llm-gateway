package coreapi

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"minigw/core"
)

func autoPullAPI(t *testing.T) (*fixture, *API, func(method, path, body string, want int) []byte, string) {
	f := newFixture(t)
	h := &methodHost{secretHost: secretHost{secrets: map[string]string{}}}
	h.answer = func(method, path string) (int, string) {
		switch {
		case path == "/minigw/v1/projects/shop":
			return 200, `{"id":7,"slug":"shop"}`
		case path == "/minigw/v1/projects/7/kb/manifest":
			return 200, `{"v":1,"sources":[{"id":7,"label":"project:shop"}],"notes":[{"id":4,"source":7,"rev":2,"class":"structure","kind":"finding"}],"truncated":false}`
		case path == "/minigw/v1/projects/7/notes/4":
			return 200, `{"id":4,"project":7,"class":"structure","title":"Deploy order","body":"first the db","updated":"2026-11-17T09:00:00Z","revision":2,"entryKind":"finding"}`
		}
		return 404, `{}`
	}
	api := New(Options{Store: func() *core.Store { return f.st }, Upstream: h})
	call := func(method, path, body string, want int) []byte {
		t.Helper()
		r := api.Handle(bg, Context{Kind: KindAdmin}, Request{Method: method, Path: path, Body: []byte(body)})
		if r.Status != want {
			t.Fatalf("%s %s %s: %d %s", method, path, body, r.Status, r.Body)
		}
		return r.Body
	}
	call("PUT", "/upstreams/hub", `{"address":"127.0.0.1:8766","token":"tok#key"}`, 200)
	link := fmt.Sprintf("/upstreams/hub/links/%d", f.pid["fakture"])
	call("PUT", link, `{"remote":"shop","classes":"structure"}`, 200)
	return f, api, call, link
}

// One step of the timer on the helper's lane: the next due link, pushed or pulled. Only the owner's channel reaches it (a member never
// triggers a pull), it pulls only a link whose reach the owner accepted, and what it answers still carries ran and next for the app's driver.
func TestUpstreamAutoStepPullsAcceptedLinksAndIsTheOwnersAlone(t *testing.T) {
	f, api, call, link := autoPullAPI(t)
	var step struct {
		Ran  bool
		Next int
		Pull *struct{ Added, Mirror int }
		Push *struct{}
	}
	json.Unmarshal(call("POST", "/upstream-auto", "", 200), &step)
	if step.Ran || step.Pull != nil {
		t.Fatalf("a link with reading off was pulled: %+v", step)
	}
	call("PUT", link, `{"read":true,"readOnly":true,"acceptReach":true}`, 200)
	json.Unmarshal(call("POST", "/upstream-auto", "", 200), &step)
	if !step.Ran || step.Pull == nil || step.Pull.Added != 1 || step.Pull.Mirror != 1 {
		t.Fatalf("%+v", step)
	}
	step = struct {
		Ran  bool
		Next int
		Pull *struct{ Added, Mirror int }
		Push *struct{}
	}{}
	json.Unmarshal(call("POST", "/upstream-auto", "", 200), &step)
	if step.Ran || step.Next <= 0 { // pulled just now: the next one is an interval away
		t.Fatalf("%+v", step)
	}
	for _, mp := range []string{"/upstream-auto", "/minigw/v1/upstream-auto", "/minigw/v1/projects/1/upstream-auto"} {
		for _, m := range []string{"POST", "GET"} {
			if r := api.Handle(bg, member("dev-fani"), Request{Method: m, Path: mp}); r.Status != 404 {
				t.Errorf("member %s %s: %d %s", m, mp, r.Status, r.Body)
			}
		}
	}
	if r := api.Handle(bg, Context{Kind: KindAdmin}, Request{Method: "GET", Path: "/upstream-auto"}); r.Status != 404 {
		t.Errorf("GET: %d", r.Status)
	}
	_ = f
}

// The interval is an owner's setting of the link: minutes as a number or as the text a panel's choice sends, 5 to 1440; nothing else.
func TestLinkPullIntervalSettingOnTheRoute(t *testing.T) {
	f, _, call, link := autoPullAPI(t)
	call("PUT", link, `{"read":true,"readOnly":true,"acceptReach":true}`, 200)
	for _, bad := range []string{`{"pullEvery":3}`, `{"pullEvery":"4"}`, `{"pullEvery":1441}`, `{"pullEvery":"often"}`, `{"pullEvery":-1}`, `{"pullEvery":1.5}`, `{"pullEvery":null,"x":1}`} {
		call("PUT", link, strings.Replace(bad, "{", `{"readOnly":true,`, 1), 400)
	}
	call("PUT", link, `{"readOnly":true,"pullEvery":"30"}`, 200)
	var st struct {
		Links []struct {
			PullEvery, PullEveryMin int
			PullNext                *string
			Read                    bool
			ReadClassesText         string
			ReadSourcesText         string
			LastPullAgeText         string
		}
	}
	json.Unmarshal(call("GET", "/upstreams", "", 200), &st)
	if l := st.Links[0]; l.PullEvery != 1800 || l.PullEveryMin != 30 || l.PullNext == nil || !l.Read || l.ReadClassesText != "structure" || l.ReadSourcesText != "-" || l.LastPullAgeText != "" {
		t.Fatalf("%+v", l)
	}
	call("PUT", link, `{"readOnly":true,"pullEvery":15}`, 200)
	json.Unmarshal(call("GET", "/upstreams", "", 200), &st)
	if st.Links[0].PullEvery != 900 {
		t.Fatalf("%+v", st.Links[0])
	}
	// the setting is not a reach: it needs no acceptance, and it does not touch what is read or the mirror
	var n int
	f.st.DB().QueryRow(`SELECT count(*) FROM upstream_mirror`).Scan(&n)
	if n != 0 {
		t.Fatal(n)
	}
}

// The panel cannot turn reading on without the reach confirmation: every action of the Upstream gateways panel whose body can set `read`
// or carries `acceptReach` is on a route that requires a confirmation (the validator refuses one without), and the acceptance a panel
// action records is the CLI's: the same reach, named in the audit row.
func TestPanelReadActionsNeedTheConfirmationAndRecordTheCLIsAcceptance(t *testing.T) {
	p := builtinProvider()
	var doc map[string]any
	for _, raw := range p.Panels {
		if strings.Contains(string(raw), `"builtin.upstreams"`) {
			json.Unmarshal(raw, &doc)
		}
	}
	n := 0
	var walk func(o any)
	walk = func(o any) {
		switch v := o.(type) {
		case map[string]any:
			if body, ok := v["body"].(map[string]any); ok {
				_, reads := body["read"]
				_, accepts := body["acceptReach"]
				_, classes := body["readClasses"]
				_, sources := body["readSources"]
				if reads || accepts || classes || sources {
					n++
					if v["confirm"] == nil {
						t.Errorf("action %v changes what is read without a confirmation", v["id"])
					}
					if v["method"] != "PUT" || v["path"] != "/upstreams/{row.upstream}/links/{row.linkId}" {
						t.Errorf("action %v reaches reading by another route: %v %v", v["id"], v["method"], v["path"])
					}
					if on := body["read"]; on != nil && on.(map[string]any)["const"] == true && body["acceptReach"] == nil {
						t.Errorf("action %v turns reading on without acceptReach", v["id"])
					}
				}
			}
			for _, x := range v {
				walk(x)
			}
		case []any:
			for _, x := range v {
				walk(x)
			}
		}
	}
	walk(doc)
	if n < 3 {
		t.Fatalf("found %d actions that change reading", n)
	}

	// the panel's bodies against the real route: what "Turn Reading On" sends records the same acceptance as the CLI's --accept-reach
	f, _, call, link := autoPullAPI(t)
	call("PUT", link, `{"team":false,"remote":"shop","classes":"","withPrivate":false,"read":true,"readClasses":"","readSources":"","acceptReach":true,"readOnly":true}`, 200) // the CLI
	reach := func() string {
		var detail string
		if err := f.st.DB().QueryRow(`SELECT detail FROM audit_events WHERE action='upstream.link' ORDER BY id DESC LIMIT 1`).Scan(&detail); err != nil {
			t.Fatal(err)
		}
		var d struct {
			ReadAccepted struct{ Reach string } `json:"readAccepted"`
		}
		json.Unmarshal([]byte(detail), &d)
		return d.ReadAccepted.Reach
	}
	cli := reach()
	call("PUT", link, `{"read":false,"readOnly":true,"team":false,"remote":"shop"}`, 200)
	call("PUT", link, `{"team":false,"remote":"shop","readOnly":true,"read":true,"readClasses":"structure","readSources":"","acceptReach":true}`, 200) // the panel
	if got := reach(); got == "" || got != cli {
		t.Fatalf("the panel recorded %q, the CLI %q", got, cli)
	}
	// without the acceptance the route refuses, whoever asks
	call("PUT", link, `{"read":false,"readOnly":true}`, 200)
	call("PUT", link, `{"team":false,"remote":"shop","readOnly":true,"read":true,"readClasses":"structure","readSources":""}`, 400)
	call("PUT", link, `{"team":false,"remote":"shop","readOnly":true,"readClasses":"structure,data","readSources":""}`, 400)
}

// sec-upc4 1 on the route: an omitted readSources keeps the sources, an empty string clears them, reading off forgets them.
func TestReadSourcesOmittedKeepsEmptyClearsOffForgets(t *testing.T) {
	_, _, call, link := autoPullAPI(t)
	text := func() string {
		var st struct {
			Links []struct{ ReadSourcesText string }
		}
		json.Unmarshal(call("GET", "/upstreams", "", 200), &st)
		return st.Links[0].ReadSourcesText
	}
	call("PUT", link, `{"readOnly":true,"read":true,"acceptReach":true,"readSources":"team:acme"}`, 200)
	call("PUT", link, `{"readOnly":true,"read":true,"acceptReach":true}`, 200)
	if got := text(); got != "team:acme" {
		t.Fatal("omitted must keep:", got)
	}
	call("PUT", link, `{"readOnly":true,"read":true,"acceptReach":true,"readSources":""}`, 200)
	if got := text(); got != "-" {
		t.Fatal("empty must clear:", got)
	}
	call("PUT", link, `{"readOnly":true,"read":true,"acceptReach":true,"readSources":"team:acme"}`, 200)
	call("PUT", link, `{"readOnly":true,"read":false}`, 200)
	call("PUT", link, `{"readOnly":true,"read":true,"acceptReach":true}`, 200)
	if got := text(); got != "-" {
		t.Fatal("off must forget:", got)
	}
}
