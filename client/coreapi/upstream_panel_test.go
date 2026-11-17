package coreapi

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"minigw/core"
)

// answers by path: what an upstream says to the probe and to the push.
type scriptedHost struct {
	secretHost
	answer func(path string) (int, string)
}

func (h *scriptedHost) Info(string) UpstreamInfo { return UpstreamInfo{Device: "DEV", Where: "test"} }

func (h *scriptedHost) Do(_ context.Context, _ string, _ string, path string, _ map[string]string, _ []byte) (int, []byte, time.Duration, error) {
	st, body := h.answer(path)
	if st < 0 {
		return 0, nil, 0, fmt.Errorf("no route to host")
	}
	return st, []byte(body), 0, nil
}

// What the Upstream gateways panel reads and sends beyond the CLI's answers: link rows with ids, the probe with a state code, a team
// link removed with ?team=true, and the push report with its lines as one text.
func TestUpstreamPanelAnswers(t *testing.T) {
	f := newFixture(t)
	h := &scriptedHost{secretHost: secretHost{secrets: map[string]string{}}}
	h.answer = func(path string) (int, string) { return 404, "{}" }
	api := New(Options{Store: func() *core.Store { return f.st }, Upstream: h})
	admin := Context{Kind: KindAdmin}
	call := func(method, path, query, body string, want int) []byte {
		t.Helper()
		r := api.Handle(bg, admin, Request{Method: method, Path: path, RawQuery: query, Body: []byte(body)})
		if r.Status != want {
			t.Fatalf("%s %s?%s: %d %s, want %d", method, path, query, r.Status, r.Body, want)
		}
		return r.Body
	}
	call("PUT", "/upstreams/hub", "", `{"address":"hub.example","token":"tok#key"}`, 200)
	var created struct{ Team TeamJSON }
	json.Unmarshal(call("POST", "/teams", "", `{"name":"acme"}`, 200), &created)
	fk := f.pid["fakture"]
	call("PUT", fmt.Sprintf("/teams/%d/projects/%d", created.Team.ID, fk), "", "", 200)
	call("PUT", fmt.Sprintf("/upstreams/hub/links/%d", fk), "", `{"team":false,"remote":"","classes":"structure,data","withPrivate":false}`, 200)
	call("PUT", fmt.Sprintf("/upstreams/hub/links/%d", created.Team.ID), "", `{"team":true,"remote":"","classes":"structure","withPrivate":false}`, 200)

	var list struct{ Links []adminLinkJSON }
	json.Unmarshal(call("GET", "/upstreams", "", "", 200), &list)
	if len(list.Links) != 2 {
		t.Fatalf("%+v", list)
	}
	for _, l := range list.Links {
		switch l.Place {
		case "project":
			if l.LinkID != fk || l.ProjectID != fk || l.TeamID != 0 || l.ClassesText != "structure, data" || l.ID != fmt.Sprintf("project-%d-hub", fk) || l.Upstream != "hub" {
				t.Errorf("project link: %+v", l)
			}
		case "team":
			if l.LinkID != created.Team.ID || l.ProjectID != 0 || l.TeamID != created.Team.ID || l.ClassesText != "structure" || !l.Team {
				t.Errorf("team link: %+v", l)
			}
		default:
			t.Errorf("place %q", l.Place)
		}
	}
	// a team's link is removed with ?team=true (a panel sends no body with a DELETE); without it the id is taken for a project's
	call("DELETE", fmt.Sprintf("/upstreams/hub/links/%d", created.Team.ID), "team=true", "", 200)
	json.Unmarshal(call("GET", "/upstreams", "", "", 200), &list)
	if len(list.Links) != 1 || list.Links[0].Place != "project" {
		t.Fatalf("%+v", list.Links)
	}
	call("DELETE", fmt.Sprintf("/upstreams/hub/links/%d", fk), "team=false", "", 200)

	// the probe: one state code for each thing an upstream can say
	probe := func() (string, string) {
		var o struct{ State, Probe, Device string }
		json.Unmarshal(call("GET", "/upstreams/hub/probe", "", "", 200), &o)
		if o.Device != "DEV" {
			t.Errorf("device %q", o.Device)
		}
		return o.State, o.Probe
	}
	for name, c := range map[string]struct {
		answer func(string) (int, string)
		state  string
	}{
		"unreachable": {func(string) (int, string) { return -1, "" }, "unreachable"},
		"token":       {func(string) (int, string) { return 401, "{}" }, "token"},
		"waiting":     {func(string) (int, string) { return 403, "{}" }, "waiting"},
		"notbound": {func(p string) (int, string) {
			if p == "/v1/models" {
				return 200, "{}"
			}
			return 403, `{"error":{"message":"the device is not bound to a user"}}`
		}, "notbound"},
		"connected": {func(string) (int, string) { return 200, "{}" }, "connected"},
		"other":     {func(string) (int, string) { return 418, "{}" }, "other"},
	} {
		h.answer = c.answer
		if st, text := probe(); st != c.state || text == "" {
			t.Errorf("%s: state %q, text %q", name, st, text)
		}
	}
	call("GET", "/upstreams/nope/probe", "", "", 404)
	call("POST", "/upstreams/hub/probe", "", "", 404)

	// the push report: the lines as one text next to the list (the CLI keeps reading the list)
	call("PUT", fmt.Sprintf("/upstreams/hub/links/%d", fk), "", `{"team":false,"remote":"","classes":"structure,data","withPrivate":false}`, 200)
	h.answer = func(string) (int, string) { return 403, `{"error":{"message":"not allowed"}}` }
	body := call("POST", "/upstream-push", "", fmt.Sprintf(`{"project":%d,"upstream":"hub","dryRun":true}`, fk), 200)
	var pr struct {
		Pushes []struct {
			Label     string
			Lines     []string
			LinesText string
			DryRun    bool
		}
	}
	json.Unmarshal(body, &pr)
	if len(pr.Pushes) != 1 || pr.Pushes[0].Label != "fakture" || !pr.Pushes[0].DryRun || pr.Pushes[0].LinesText != strings.Join(pr.Pushes[0].Lines, "\n") || !strings.Contains(string(body), `"linesText"`) {
		t.Fatalf("%s", body)
	}
}
