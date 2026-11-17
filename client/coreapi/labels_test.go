package coreapi

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"minigw/core"
)

// Labels over HTTP: filters and grouped rows on the member and admin usage routes and on the sessions routes; a label
// never selects another user's events.
func TestLabelRoutes(t *testing.T) {
	f := newFixture(t)
	now := time.Now()
	ev := func(id string, user string, phase, task, taskSrc string) {
		e := core.UsageEvent{ID: id, Start: now, UserID: f.uid[user], ProjectID: f.pid["fakture"], DeviceID: "dev", Provider: "codex", Model: "m", Phase: phase, Activity: "feature", Task: task, TaskSource: taskSrc}
		if err := f.st.BeginEvent(bg, e); err != nil {
			t.Fatal(err)
		}
		e.End, e.Outcome, e.TokensIn, e.TokensOut, e.TokenSource = now.Add(time.Second), core.OutcomeOK, 1, 1, "reported"
		if err := f.st.FinishEvent(bg, e); err != nil {
			t.Fatal(err)
		}
	}
	ev("e1", "ana", "plan", "#1", "given")
	ev("e2", "ana", "do", "feature/x", "branch")
	ev("e3", "fani", "plan", "#2", "given")
	get := func(c Context, path, q string) string {
		t.Helper()
		r := f.api.Handle(bg, c, Request{Method: "GET", Path: path, RawQuery: q})
		if r.Status != 200 {
			t.Fatalf("%s?%s: %d %s", path, q, r.Status, r.Body)
		}
		return string(r.Body)
	}
	project := fmt.Sprint(f.pid["fakture"])
	// A developer asks for plan events of the project: only the own one, never fani's.
	b := get(member("dev-ana"), "/minigw/v1/usage", "project="+project+"&phase=plan&labels=1")
	if strings.Count(b, `"phase":"plan"`) != 1 || strings.Contains(b, `"task":"#2"`) || !strings.Contains(b, `"taskSource":"given"`) {
		t.Fatal(b)
	}
	// Without labels=1 the rows carry none.
	if b = get(member("dev-ana"), "/minigw/v1/usage", "project="+project); strings.Contains(b, `"phase"`) {
		t.Fatal(b)
	}
	// The admin route: all users, grouped by label.
	b = get(Context{Kind: KindAdmin}, "/usage/events", "labels=1")
	if strings.Count(b, `"phase":"plan"`) != 2 || !strings.Contains(b, `"taskSource":"branch"`) {
		t.Fatal(b)
	}
	if b = get(Context{Kind: KindAdmin}, "/usage/events", "task=feature%2Fx"); strings.Count(b, `"events":`) != 1 {
		t.Fatal(b)
	}
	// Sessions: labels of the first request, filter by label, own sessions for a developer.
	b = get(member("dev-ana"), "/minigw/v1/projects/"+project+"/sessions", "phase=plan")
	if !strings.Contains(b, `"task":"#1"`) || strings.Contains(b, `"task":"#2"`) || strings.Contains(b, "feature/x") {
		t.Fatal(b)
	}
	b = get(Context{Kind: KindAdmin}, "/usage/sessions", "project="+project+"&task=feature%2Fx")
	if !strings.Contains(b, `"taskSource":"branch"`) || strings.Count(b, `"requests"`) != 1 {
		t.Fatal(b)
	}
}
