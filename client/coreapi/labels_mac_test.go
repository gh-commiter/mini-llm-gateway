package coreapi

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"minigw/core"
)

// B3 (the Mac label worker): the settings choice on a host that can run one backend, the revision a worker decided under, and the
// internal usage event of one decision.
func TestLabelBackendsOfTheHostAndTheMacWorkerCalls(t *testing.T) {
	f := newFixture(t)
	mac := New(Options{Store: func() *core.Store { return f.st }, Dir: t.TempDir(), LabelBackends: []string{"apple-fm"}})
	put := func(api *API, body string) (int, string) {
		r := api.Handle(bg, Context{Kind: KindAdmin}, Request{Method: "PUT", Path: "/usage/labels/settings", Body: []byte(body)})
		return r.Status, string(r.Body)
	}
	if st, b := put(mac, `{"backend":"foundry:phi-4-mini"}`); st != 400 || !strings.Contains(b, "choose apple-fm") {
		t.Fatalf("%d %s", st, b)
	}
	none := New(Options{Store: func() *core.Store { return f.st }, Dir: t.TempDir(), LabelBackends: []string{}})
	if st, b := put(none, `{"backend":"apple-fm"}`); st != 400 || !strings.Contains(b, "no on-device model") {
		t.Fatalf("%d %s", st, b)
	}
	if st, b := put(f.api, `{"backend":"foundry:phi-4-mini"}`); st != 200 { // no restriction: the Go gateway keeps its own check
		t.Fatalf("%d %s", st, b)
	}
	if st, b := put(mac, `{"enabled":true,"backend":"apple-fm"}`); st != 200 {
		t.Fatalf("%d %s", st, b)
	}
	in := func(name, body string) (int, string) {
		r := mac.Handle(bg, Context{Kind: KindInternal}, Request{Method: "POST", Path: "/internal/" + name, Body: []byte(body)})
		return r.Status, string(r.Body)
	}
	l, _ := f.st.LabelSettings(bg)
	e := core.UsageEvent{ID: "ev_root0000001", Start: time.Now(), DeviceID: "dev", Provider: "claude-cli", Model: "m"}
	if err := f.st.BeginEvent(bg, e); err != nil {
		t.Fatal(err)
	}
	// a stale revision stores nothing, the current one does
	stale := `{"id":"ev_root0000001","phase":"do","phaseScore":1000,"revision":` + itoa(l.Revision-1) + `}`
	if st, b := in("event-labels", stale); st != 200 || !strings.Contains(b, `"updated":0`) {
		t.Fatalf("%d %s", st, b)
	}
	if st, b := in("event-labels", `{"id":"ev_root0000001","phase":"do","phaseScore":1000,"revision":`+itoa(l.Revision)+`}`); st != 200 || !strings.Contains(b, `"updated":1`) {
		t.Fatalf("%d %s", st, b)
	}
	if st, _ := in("label-agreement", `{"kind":"phase","bucket":"1000","agree":true,"revision":`+itoa(l.Revision-1)+`}`); st != 200 {
		t.Fatal(st)
	}
	if rows, _ := f.st.Agreement(bg, 30); len(rows) != 0 {
		t.Fatalf("a stale agreement was counted: %v", rows)
	}
	// the decision event: internal, no user, the device "gateway"; only apple-fm, a safe model, a real id
	now := time.Now().UTC().Format(time.RFC3339Nano)
	ok := `{"id":"ev_dec00000001","provider":"apple-fm","model":"apple-fm","start":"` + now + `","end":"` + now + `","tokensIn":10,"tokensOut":2,"failed":false}`
	if st, b := in("label-decision", ok); st != 200 {
		t.Fatalf("%d %s", st, b)
	}
	var user *int64
	var device, src, act string
	if err := f.st.DB().QueryRow(`SELECT user_id, device_id, phase_source, activity FROM usage_events WHERE id='ev_dec00000001'`).Scan(&user, &device, &src, &act); err != nil || user != nil || device != "gateway" || src != "internal" || act != "ops" {
		t.Fatal(err, user, device, src, act)
	}
	for _, bad := range []string{strings.Replace(ok, `"apple-fm","model"`, `"codex","model"`, 1), strings.Replace(ok, `"model":"apple-fm"`, `"model":"A B"`, 1), strings.Replace(ok, "ev_dec00000001", "x", 1), strings.Replace(ok, `"failed":false`, `"text":"hello"`, 1)} {
		if st, _ := in("label-decision", bad); st != 400 {
			t.Fatalf("accepted %s", bad)
		}
	}
	// and an internal decision event is never labelled by a model afterwards
	if st, b := in("event-labels", `{"id":"ev_dec00000001","phase":"do","phaseScore":1000}`); st == 200 && !strings.Contains(b, `"updated":0`) {
		t.Fatalf("%d %s", st, b)
	}
}

func itoa(n uint64) string { return strconv.FormatUint(n, 10) }

// A backend stored before this version (or by a restore) is checked when the worker is turned on, and marked in the row.
func TestMacRefusesToEnableAStoredUnsupportedBackendAndMarksIt(t *testing.T) {
	f := newFixture(t)
	if _, err := f.st.SetLabelSettings(bg, core.LabelSettingsPatch{Backend: func() *string { s := "foundry:phi-4-mini"; return &s }()}); err != nil {
		t.Fatal(err)
	}
	mac := New(Options{Store: func() *core.Store { return f.st }, Dir: t.TempDir(), LabelBackends: []string{"apple-fm"}})
	do := func(method, body string) (int, string) {
		r := mac.Handle(bg, Context{Kind: KindAdmin}, Request{Method: method, Path: "/usage/labels/settings", Body: []byte(body)})
		return r.Status, string(r.Body)
	}
	if st, b := do("PUT", `{"enabled":true}`); st != 400 || !strings.Contains(b, "choose apple-fm") {
		t.Fatalf("%d %s", st, b)
	}
	st, b := do("GET", "")
	if st != 200 || !strings.Contains(b, `"backends":"apple-fm"`) || !strings.Contains(b, `"backendProblem":"backend: not available on this system; choose apple-fm"`) {
		t.Fatalf("%d %s", st, b)
	}
	if st, b := do("PUT", `{"shadow":false}`); st != 200 { // not turning it on: other changes are fine
		t.Fatalf("%d %s", st, b)
	}
	if st, b := do("PUT", `{"enabled":true,"backend":"apple-fm"}`); st != 200 || !strings.Contains(b, `"backendProblem":""`) {
		t.Fatalf("%d %s", st, b)
	}
	if st, b := func() (int, string) {
		r := f.api.Handle(bg, Context{Kind: KindAdmin}, Request{Method: "GET", Path: "/usage/labels/settings"})
		return r.Status, string(r.Body)
	}(); st != 200 || !strings.Contains(b, `"backends":"apple-fm, foundry:\u003cmodel\u003e"`) {
		t.Fatalf("%d %s", st, b)
	}
}

func TestAPreemptedDecisionIsRecordedAsCancelled(t *testing.T) {
	f := newFixture(t)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	r := f.api.Handle(bg, Context{Kind: KindInternal}, Request{Method: "POST", Path: "/internal/label-decision",
		Body: []byte(`{"id":"ev_pre00000001","provider":"apple-fm","model":"apple-fm","start":"` + now + `","end":"` + now + `","tokensIn":10,"tokensOut":1,"failed":false,"preempted":true}`)})
	if r.Status != 200 {
		t.Fatal(r.Status, string(r.Body))
	}
	var outcome, kind string
	if err := f.st.DB().QueryRow(`SELECT outcome, COALESCE(error_kind,'') FROM usage_events WHERE id='ev_pre00000001'`).Scan(&outcome, &kind); err != nil || outcome != "cancelled" || kind != "" {
		t.Fatal(err, outcome, kind)
	}
}
