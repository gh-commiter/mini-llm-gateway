package coreapi

import (
	"context"
	"encoding/json"
	"fmt"
	"minigw/core"
	"testing"
	"time"
)

func TestSessionsPathRecognition(t *testing.T) {
	for _, tc := range []struct {
		method, path string
		want         bool
	}{
		{"GET", "/minigw/v1/projects/1/sessions", true},
		{"GET", "/minigw/v1/projects/fakture/sessions", true},
		{"GET", "/minigw/v1/projects/sessions", false}, // a project whose slug is sessions
		{"GET", "/minigw/v1/projects/1/sessions/x", false},
		{"POST", "/minigw/v1/projects/1/sessions", false},
		{"GET", "/usage/sessions", true},
		{"GET", "/minigw/v1/usage/sessions", false},
	} {
		if got := IsSessionsPath(tc.method, tc.path); got != tc.want {
			t.Errorf("%s %s: got %v, want %v", tc.method, tc.path, got, tc.want)
		}
	}
}

func TestSessionRoutesAndAdmissionChanges(t *testing.T) {
	f := newFixture(t)
	pid := f.pid["fakture"]
	path := fmt.Sprintf("/minigw/v1/projects/%d/sessions", pid)
	for _, u := range []string{"ana", "eva", "fani"} {
		if err := f.st.FinishEvent(bg, core.UsageEvent{ID: u, ProjectID: pid, UserID: f.uid[u], DeviceID: "dev-" + u, Start: time.Now(), End: time.Now(), Model: "m", Outcome: "ok", TokensIn: -1, TokensOut: 0, TokenSource: "reported"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct {
		c         Context
		path, q   string
		status, n int
	}{
		{member("dev-ana"), path, "", 200, 1}, {member("dev-eva"), path, "", 200, 1}, {member("dev-fani"), path, "", 200, 3},
		{member("dev-cene"), path, "", 404, 0}, {Context{Kind: KindRun}, path, "", 404, 0},
		{Context{Kind: KindAdmin}, "/usage/sessions", "", 400, 0}, {Context{Kind: KindAdmin}, "/usage/sessions", "project=fakture", 200, 3},
		{Context{Kind: KindAdmin}, "/usage/events", "", 200, 0}, {member("dev-ana"), path, "limit=501", 400, 0},
	} {
		r := f.api.Handle(bg, tc.c, Request{Method: "GET", Path: tc.path, RawQuery: tc.q})
		if r.Status != tc.status {
			t.Fatalf("%+v: %d %s", tc, r.Status, r.Body)
		}
		if tc.n > 0 {
			var p core.SessionPage
			if err := json.Unmarshal(r.Body, &p); err != nil || len(p.Sessions) != tc.n {
				t.Fatalf("%s: %v", r.Body, err)
			}
		}
	}
	for _, change := range []string{"membership", "binding", "active"} {
		t.Run(change, func(t *testing.T) {
			f := newFixture(t)
			p := fmt.Sprintf("/minigw/v1/projects/%d/sessions", f.pid["fakture"])
			f.api.o.AfterAdmission = func(context.Context, Context, Request) {
				var err error
				switch change {
				case "membership":
					err = f.st.RemoveMember(bg, f.pid["fakture"], f.uid["ana"])
				case "binding":
					err = f.st.RemoveBinding(bg, "dev-ana")
				case "active":
					err = f.st.SetUserActive(bg, f.uid["ana"], false)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			r := f.api.Handle(bg, member("dev-ana"), get(p))
			if r.Status != 403 && r.Status != 404 {
				t.Fatalf("%d %s", r.Status, r.Body)
			}
		})
	}
}
