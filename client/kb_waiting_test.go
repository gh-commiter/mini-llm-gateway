package main

import (
	"encoding/json"
	"fmt"
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
