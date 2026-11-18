package coreapi

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"minigw/core"
)

// The settings and agreement routes are the owner's (admin channel) and the model-label calls are the gateway's own
// (internal): no member or run context reaches them; an explicit label is never replaced; a label source filter only narrows.
func TestModelLabelRoutes(t *testing.T) {
	f := newFixture(t)
	call := func(c Context, method, path, body string) (int, string) {
		t.Helper()
		path, query, _ := strings.Cut(path, "?")
		r := f.api.Handle(bg, c, Request{Method: method, Path: path, RawQuery: query, Body: []byte(body)})
		return r.Status, string(r.Body)
	}
	admin := Context{Kind: KindAdmin}
	// a member never reaches settings or agreement, on any spelling of the path
	for _, p := range []string{"/usage/labels/settings", "/usage/labels/agreement", "/minigw/v1/usage/labels/settings", "/minigw/v1/usage/labels/agreement"} {
		for _, m := range []string{"GET", "PUT", "POST"} {
			if st, b := call(member("dev-ana"), m, p, `{"enabled":true,"backend":"apple-fm"}`); st == 200 {
				t.Fatalf("a member got %s %s: %s", m, p, b)
			}
		}
	}
	if st, _ := call(Context{Kind: KindInternal}, "GET", "/usage/labels/settings", ""); st == 200 {
		t.Fatal("an internal context reads the admin route")
	}
	// the owner: off by default, a one-row list for the panel
	st, b := call(admin, "GET", "/usage/labels/settings", "")
	if st != 200 || !strings.Contains(b, `"settings":[{"id":"labels","enabled":false,"backend":"","threshold":"1.0","maxPerHour":"120"`) {
		t.Fatalf("%d %s", st, b)
	}
	for _, bad := range []string{`{"enabled":true}`, `{"backend":"grok"}`, `{"nope":1}`, `{"threshold":0.5}`, `{"lastError":"x"}`} {
		if st, b = call(admin, "PUT", "/usage/labels/settings", bad); st != 400 {
			t.Fatalf("%s: %d %s", bad, st, b)
		}
	}
	// what the panel's form sends: strings and booleans
	st, b = call(admin, "PUT", "/usage/labels/settings", `{"enabled":true,"backend":"apple-fm","threshold":"0.67","maxPerHour":"30","shadow":false,"shadowRate":"8","activities":"audit, docs"}`)
	if st != 200 || !strings.Contains(b, `"enabled":true`) || !strings.Contains(b, `"activitiesText":"audit, docs"`) || !strings.Contains(b, `"threshold":"0.67"`) {
		t.Fatalf("%d %s", st, b)
	}
	if st, _ = call(admin, "PUT", "/usage/labels/agreement", `{}`); st != 404 {
		t.Fatal(st)
	}
	// internal calls: the Mac helper's
	in := func(name, body string) (int, string) {
		return call(Context{Kind: KindInternal}, "POST", "/internal/"+name, body)
	}
	if st, b = in("label-settings", `{}`); st != 200 || !strings.Contains(b, `"backend":"apple-fm"`) {
		t.Fatalf("%d %s", st, b)
	}
	now := time.Now()
	begin := func(id string, user, phase string) {
		e := core.UsageEvent{ID: id, Start: now, UserID: f.uid[user], ProjectID: f.pid["fakture"], DeviceID: "dev", Provider: "codex", Model: "m", Phase: phase}
		if err := f.st.BeginEvent(bg, e); err != nil {
			t.Fatal(err)
		}
	}
	begin("e1", "ana", "do") // an explicit phase
	begin("e2", "fani", "")
	if st, _ = in("event-labels", `{"id":"nope","phase":"do","phaseScore":1000}`); st != 404 {
		t.Fatal(st)
	}
	if st, _ = in("event-labels", `{"id":"e1","phase":"plan","phaseScore":1000,"activity":"audit","activityScore":667}`); st != 200 {
		t.Fatal(st)
	}
	if st, _ = in("event-labels", `{"id":"e2","phase":"plan","phaseScore":1000,"activity":"bugfix","activityScore":667}`); st != 200 { // bugfix left the list
		t.Fatal(st)
	}
	for _, bad := range []string{`{"id":"e1","text":"hello"}`, `{"phase":"do"}`, `not json`} {
		if st, _ = in("event-labels", bad); st != 400 {
			t.Fatalf("%s: %d", bad, st)
		}
	}
	var got struct {
		Days []struct{ Phase, PhaseSource, Activity, ActivitySource string }
	}
	_, b = call(admin, "GET", "/usage/events?labels=1", "")
	json.Unmarshal([]byte(b), &got)
	seen := map[string]bool{}
	for _, d := range got.Days {
		seen[d.Phase+"/"+d.PhaseSource+"/"+d.Activity+"/"+d.ActivitySource] = true
	}
	if !seen["do/given/audit/model"] || !seen["plan/model//"] || len(seen) != 2 {
		t.Fatalf("%v", seen)
	}
	// agreement counters
	if st, _ = in("label-agreement", `{"kind":"phase","bucket":"1000","agree":true}`); st != 200 {
		t.Fatal(st)
	}
	if st, _ = in("label-agreement", `{"kind":"phase","bucket":"5","agree":true}`); st != 400 {
		t.Fatal(st)
	}
	if st, b = call(admin, "GET", "/usage/labels/agreement", ""); st != 200 || !strings.Contains(b, `{"kind":"phase","bucket":"1000","agree":1,"n":1,"id":"phase/1000","percent":"100 %","count":"1","days":"`) {
		t.Fatalf("%d %s", st, b)
	}
	if st, _ = call(admin, "GET", "/usage/labels/agreement?days=0", ""); st != 200 {
		t.Fatal(st)
	}
	// the worker's error text
	if st, _ = in("label-error", `{"message":"Apple Intelligence is off"}`); st != 200 {
		t.Fatal(st)
	}
	if _, b = call(admin, "GET", "/usage/labels/settings", ""); !strings.Contains(b, `"lastError":"Apple Intelligence is off"`) {
		t.Fatal(b)
	}
	// a label source filter on the member route only narrows: ana sees her own model event, never fani's
	project := fmt.Sprint(f.pid["fakture"])
	r := f.api.Handle(bg, member("dev-ana"), Request{Method: "GET", Path: "/minigw/v1/usage", RawQuery: "project=" + project + "&label_source=model&labels=1"})
	if r.Status != 200 || strings.Count(string(r.Body), `"events":`) != 1 || strings.Contains(string(r.Body), `"phase":"plan"`) && !strings.Contains(string(r.Body), `"activity":"audit"`) {
		t.Fatalf("%d %s", r.Status, r.Body)
	}
	if st, _ = call(admin, "GET", "/usage/events?label_source=classified", ""); st != 400 {
		t.Fatal(st)
	}
}
