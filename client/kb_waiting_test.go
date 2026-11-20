package main

import (
	"encoding/json"
	"fmt"
	"minigw/core"
	"net/http"
	"strings"
	"testing"
	"time"
)

// What a reviewer sees of waiting submissions: project show and the session-start line of the opt-in hook.

// The shared fixture client/testdata/kb-waiting.json is asserted here and in Tests/RouterCoreTests/KBHookTests.swift.
type waitingFixture struct {
	Lines []struct {
		N          int
		Slug, Want string
	}
	TeamLines []struct {
		N          int
		Team, Want string
	}
	TeamAnswers []struct {
		Name, Body string
		WantN      int
		Want       []string
	}
	Answers []struct {
		Name   string
		Status int
		Body   string
		Want   int
	}
}

func TestWaitingFixture(t *testing.T) {
	var f waitingFixture
	if err := json.Unmarshal(srcFile(t, "testdata/kb-waiting.json"), &f); err != nil {
		t.Fatal(err)
	}
	for _, l := range f.Lines {
		if g := waitingLine(l.N, l.Slug); g != l.Want {
			t.Errorf("%d %s: %q", l.N, l.Slug, g)
		}
	}
	for _, l := range f.TeamLines {
		if g := teamWaitingLine(l.N, l.Team); g != l.Want {
			t.Errorf("%d %s: %q", l.N, l.Team, g)
		}
	}
	for _, a := range f.TeamAnswers {
		e := &projectEnv{do: func(_ connection, _, p string, _ map[string]string, _ []byte) (int, []byte, http.Header) {
			if !strings.HasPrefix(p, apiProjects+"/7/kb/review?after=") || !strings.HasSuffix(p, "&teams=1") {
				t.Errorf("%s: %s", a.Name, p)
			}
			return 200, []byte(a.Body), nil
		}}
		n, ts := e.reviewWaitingTeams(connection{}, 7, time.Second, true)
		got := []string{}
		for _, x := range ts {
			got = append(got, fmt.Sprintf("%s:%d", x.Name, x.Waiting))
		}
		if n != a.WantN || strings.Join(got, ",") != strings.Join(a.Want, ",") {
			t.Errorf("%s: %d %v", a.Name, n, got)
		}
	}
	for _, a := range f.Answers {
		e := &projectEnv{do: func(_ connection, _, p string, _ map[string]string, _ []byte) (int, []byte, http.Header) {
			if !strings.HasPrefix(p, apiProjects+"/7/kb/review?after=") {
				t.Errorf("%s: %s", a.Name, p)
			}
			return a.Status, []byte(a.Body), nil
		}}
		if n := e.reviewWaiting(connection{}, 7, time.Second); n != a.Want {
			t.Errorf("%s: %d", a.Name, n)
		}
	}
}

func TestWaitingSeenOnlyByReviewers(t *testing.T) {
	c := newClientFx(t)
	dir := t.TempDir()
	pid := c.f.pid["fakture"]
	c.must("ana", dir, "", "project", "link", "fakture")
	show := func(user string) string { return c.must(user, dir, "", "project", "show") }
	hookOn := func(user string, optIn bool) string {
		e, o, _ := c.env(user, dir, fmt.Sprintf(`{"session_id":"s-%s","cwd":%q}`, user, dir))
		conn, _ := e.connect()
		e.gateway = func() string { return "gw-test" }
		e.hookConn = func() (connection, bool) { return conn, true }
		e.gitState = func(string) (string, string) { return "", "" }
		if optIn {
			c.f.t.Helper()
			e.saveHookSettings(hookSettings{Enabled: []hookEnabled{{"gw-test", pid}}})
		}
		if err := e.kb([]string{"hook", "start"}); err != nil {
			t.Fatal(err)
		}
		return o.String()
	}
	const line = "1 submission waits for your review in fakture: minigw kb review fakture"
	if strings.Contains(show("fani"), "wait") {
		t.Fatal("nothing waits yet")
	}
	c.must("ana", dir, "Pitfall: secret title\nbody\n", "kb", "submit", "fakture", "--key", "w1", "--title", "Secret title", "-")
	if o := show("fani"); !strings.Contains(o, line+"\ngateway:") {
		t.Fatalf("reviewer: %q", o)
	}
	if o := show("ana"); strings.Contains(o, "wait") {
		t.Fatalf("a developer who may not review learns of it: %q", o)
	}
	// the hook: only when opted in, and only the count and the project name
	if o := hookOn("fani", false); o != "" {
		t.Fatalf("not opted in: %q", o)
	}
	o := hookOn("fani", true)
	if !strings.HasSuffix(o, line+"\n") || !strings.HasPrefix(o, "Mini Gateway: this folder is linked to project fakture") || strings.Contains(o, "Secret") {
		t.Fatalf("hook: %q", o)
	}
	if o := hookOn("ana", true); strings.Contains(o, "wait") {
		t.Fatalf("hook shows a developer the count: %q", o)
	}
	// accepted or rejected: the line goes
	c.must("fani", dir, "", "kb", "reject", "1", "--reason", "no")
	if o := show("fani"); strings.Contains(o, "wait") {
		t.Fatalf("after the decision: %q", o)
	}
	if o := hookOn("fani", true); strings.Contains(o, "wait") {
		t.Fatalf("hook after the decision: %q", o)
	}
}

// A gateway that does not answer in time costs the session start at most the limit and prints no line.
func TestWaitingHookTimeoutPrintsNothing(t *testing.T) {
	w := newHookWorld(t, "")
	w.link(t, "gw1", 7, "acme-shop")
	w.write(t, "kb-hooks.json", hookSettings{1, []hookEnabled{{"gw1", 7}}})
	w.e.gitState = func(string) (string, string) { return "h", "s" }
	w.e.in = strings.NewReader(hookStdin("s1", w.work))
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	w.e.hookConn = func() (connection, bool) { return connection{}, true }
	w.e.do = func(connection, string, string, map[string]string, []byte) (int, []byte, http.Header) {
		<-release
		return 200, []byte(`{"waiting":4}`), nil
	}
	if n := w.e.reviewWaiting(connection{}, 7, 30*time.Millisecond); n != 0 {
		t.Fatal(n)
	}
	// the hook itself uses hookWaitLimit (2 s): shorten it for the test
	old := hookWaitLimit
	hookWaitLimit = 50 * time.Millisecond
	defer func() { hookWaitLimit = old }()
	before := time.Now()
	if err := w.e.kb([]string{"hook", "start"}); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(before); d > time.Second {
		t.Fatal("slow", d)
	}
	if o := w.out.String(); strings.Contains(o, "wait") || !strings.HasPrefix(o, "Mini Gateway: this folder is linked to project acme-shop") {
		t.Fatalf("%q", o)
	}
}

// The waiting submissions of a team's own notes: team show and the session-start line, for those who review the team's notes only.
func TestTeamWaitingSeenOnlyByReviewers(t *testing.T) {
	c := newClientFx(t)
	cs, ctx := c.f.s.core, t0
	team, err := cs.CreateTeam(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{"ana", "fani", "bor"} { // eva is a reader of the project off the roster
		if err = cs.AddTeamMember(ctx, team.ID, c.f.uid[u]); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{"fakture", "eureko"} {
		if err = cs.AddTeamProject(ctx, team.ID, c.f.pid[p]); err != nil {
			t.Fatal(err)
		}
	}
	if err = cs.SetMember(ctx, c.f.pid["eureko"], c.f.uid["bor"], core.Admin); err != nil { // bor reviews the team's notes, not fakture
		t.Fatal(err)
	}
	dir := t.TempDir()
	pid := c.f.pid["fakture"]
	c.must("ana", dir, "", "project", "link", "fakture")
	show := func(user string) string {
		e, o, _ := c.env(user, dir, "")
		e.team([]string{"show", "acme"})
		return o.String()
	}
	hook := func(user string) string {
		e, o, _ := c.env(user, dir, fmt.Sprintf(`{"session_id":"s-%s","cwd":%q}`, user, dir))
		conn, _ := e.connect()
		e.gateway = func() string { return "gw-test" }
		e.hookConn = func() (connection, bool) { return conn, true }
		e.gitState = func(string) (string, string) { return "", "" }
		e.saveHookSettings(hookSettings{Enabled: []hookEnabled{{"gw-test", pid}}})
		if err := e.kb([]string{"hook", "start"}); err != nil {
			t.Fatal(err)
		}
		return o.String()
	}
	const line = "1 submission waits for your review in team acme: minigw kb review --team acme"
	for _, u := range []string{"ana", "bor", "fani", "eva"} {
		if strings.Contains(show(u), "wait") || strings.Contains(hook(u), "wait") {
			t.Fatal("nothing waits yet", u)
		}
	}
	c.must("fani", dir, "Pitfall: secret title\nbody\n", "kb", "submit", "--team", "acme", "--key", "t1", "--title", "Secret title", "-")
	for _, u := range []string{"bor"} {
		if o := show(u); !strings.Contains(o, line+"\n") {
			t.Fatalf("show %s: %q", u, o)
		}
		if o := hook(u); !strings.HasSuffix(o, line+"\n") || strings.Contains(o, "Secret") || strings.Contains(o, "in fakture") {
			t.Fatalf("hook %s: %q", u, o)
		}
	}
	for _, u := range []string{"fani", "ana", "eva"} { // the author, a member who writes nothing, and a reader off the roster: nothing, not even a hint that something waits
		if o := show(u); strings.Contains(o, "wait") {
			t.Fatalf("show %s: %q", u, o)
		}
		if o := hook(u); strings.Contains(o, "wait") || strings.Contains(o, "acme") {
			t.Fatalf("hook %s: %q", u, o)
		}
	}
	// the author sees what became of it
	if o := c.must("fani", dir, "", "kb", "submissions"); !strings.Contains(o, "team acme  pending") {
		t.Fatal(o)
	}
	c.must("bor", dir, "", "kb", "reject", "1", "--reason", "no")
	for _, u := range []string{"fani", "bor"} {
		if strings.Contains(show(u), "wait") || strings.Contains(hook(u), "wait") {
			t.Fatal("after the decision", u)
		}
	}
	if o := c.must("fani", dir, "", "kb", "submissions", "--team", "acme"); !strings.Contains(o, "team acme  rejected") {
		t.Fatal(o)
	}
}
