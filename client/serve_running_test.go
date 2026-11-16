package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

// #131: the steps of a Grok/Codex/Copilot run reach the response stream as they happen (before and after the head), with
// the shared folder's path relative and the gateway's scratch path hidden; plain keepalives still flow in silent phases.
func TestReplyWhenDoneStreamsSteps(t *testing.T) {
	defer func(d, k time.Duration) { agentHeadDelay, keepaliveEvery = d, k }(agentHeadDelay, keepaliveEvery)
	agentHeadDelay, keepaliveEvery = 80*time.Millisecond, 20*time.Millisecond
	s := &server{log: log.New(io.Discard, "", 0)}
	r := &req{Request: httptest.NewRequest("POST", "/v1/messages", nil), rid: "req_1", conn: &serveConn{peer: "127.0.0.1"}}
	defer s.activity.start(r, "claude-grok")()
	w := httptest.NewRecorder()
	dir, scratch := "/scratch/sync/proj", "/scratch"
	s.replyWhenDone(w, r, bodyG(true), func() (string, error) {
		s.activity.step("req_1", relStep("read_file "+dir+"/src/a.go", dir, scratch)) // before the head: held back
		time.Sleep(200 * time.Millisecond)
		s.activity.step("req_1", relStep("read_file "+scratch+"/tmp/x", dir, scratch)) // after the head: at once
		time.Sleep(100 * time.Millisecond)
		return "answer", nil
	})
	b := w.Body.String()
	for _, want := range []string{"⏺ read_file src/a.go\\n", "⏺ read_file …/tmp/x\\n", "event: ping", `"text":"answer"`} {
		if !strings.Contains(b, want) {
			t.Errorf("missing %q in %s", want, b)
		}
	}
	if strings.Contains(b, "/scratch") || strings.Index(b, "message_start") > strings.Index(b, "src/a.go") {
		t.Errorf("scratch path leaked, or a step before the head: %s", b)
	}
}

// #131: GET /minigw/v1/running lists only the asking device's requests; no prompt, nothing for an unknown owner.
func TestRunningIsPerDevice(t *testing.T) {
	s := &server{log: log.New(io.Discard, "", 0)}
	mk := func(rid, dev, cwd string) *req {
		hr := httptest.NewRequest("POST", "/v1/messages", nil)
		hr.Header.Set("x-claude-mini-cwd", cwd)
		return &req{Request: hr, rid: rid, conn: &serveConn{peer: "10.0.0.1", device: &serveDevice{ID: dev}}, body: []byte("SECRET prompt")}
	}
	stopA := s.activity.start(mk("req_a", "devA", "/home/a/proj"), "claude-grok")
	stopB := s.activity.start(mk("req_b", "devB", "/home/b/other"), "claude-codex")
	s.activity.step("req_a", "read_file x.go")
	s.activity.step("req_a", "grep y")
	a := s.activity.running("devA")
	if len(a) != 1 || a[0].Model != "claude-grok" || a[0].Folder != "/home/a/proj" || a[0].Steps != 2 || a[0].LastStep != "grep y" {
		t.Fatalf("devA: %+v", a)
	}
	if b := s.activity.running("devB"); len(b) != 1 || b[0].Model != "claude-codex" || b[0].Steps != 0 {
		t.Fatalf("devB: %+v", b)
	}
	if n := s.activity.running("devC"); len(n) != 0 {
		t.Fatalf("another device sees: %+v", n)
	}
	if n := s.activity.running(""); len(n) != 0 {
		t.Fatalf("no owner sees: %+v", n)
	}
	stopA()
	stopB()
	if n := s.activity.running("devA"); len(n) != 0 {
		t.Fatalf("finished run still listed: %+v", n)
	}
}

// #131: minigw running prints the gateway's answer, and refuses to run inside an agent run.
func TestRunningCommand(t *testing.T) {
	var out strings.Builder
	e := &projectEnv{connect: func() (connection, string) { return connection{}, "" }, out: &out, err: io.Discard,
		do: func(_ connection, m, p string, _ map[string]string, _ []byte) (int, []byte, http.Header) {
			if m != "GET" || p != "/minigw/v1/running" {
				t.Errorf("request %s %s", m, p)
			}
			return 200, []byte(`{"running":[{"model":"claude-grok","folder":"/home/a/proj","elapsedSec":372,"steps":34,"lastStep":"read_file src/a.go","stepAgoSec":12}]}`), nil
		}}
	if err := e.running(nil); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); !strings.Contains(got, "claude-grok") || !strings.Contains(got, "/home/a/proj") || !strings.Contains(got, "6m12s") ||
		!strings.Contains(got, "34 steps") || !strings.Contains(got, "last: read_file src/a.go (12s ago)") {
		t.Fatalf("output: %q", got)
	}
	e.agent = true
	if e.running(nil) == nil {
		t.Fatal("an agent run must not use it")
	}
}

// Shared with the Mac gateway (RunningTests in Tests/RouterCoreTests): client/testdata/running.json and step-marks.json.
func TestRunningSharedFixture(t *testing.T) {
	b, err := os.ReadFile("testdata/running.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx struct {
		Runs []struct {
			ID, Owner, Model, Folder string
			StartSec                 int
			Steps                    []struct {
				At   int
				Text string
			}
		}
		NowSec int
		Expect map[string]any
	}
	if err := json.Unmarshal(b, &fx); err != nil {
		t.Fatal(err)
	}
	base := time.Unix(1_700_000_000, 0)
	a := &activityLog{}
	for _, r := range fx.Runs { // newest first, as start() inserts
		x := activityRun{ID: r.ID, Model: r.Model, owner: r.Owner, folder: r.Folder, started: base.Add(time.Duration(r.StartSec) * time.Second)}
		for _, st := range r.Steps {
			x.Step, x.stepAt, x.Steps = st.Text, base.Add(time.Duration(st.At)*time.Second), x.Steps+1
		}
		a.runs = slices.Insert(a.runs, 0, x)
	}
	for owner, want := range fx.Expect {
		got, _ := json.Marshal(map[string]any{"running": a.runningAt(owner, base.Add(time.Duration(fx.NowSec)*time.Second))})
		var g any
		json.Unmarshal(got, &g)
		if !reflect.DeepEqual(g, want) {
			t.Errorf("owner %q: got %s, want %v", owner, got, want)
		}
	}

	b, err = os.ReadFile("testdata/step-marks.json")
	if err != nil {
		t.Fatal(err)
	}
	var sm struct {
		Cases []struct{ Name, Step, Dir, Scratch, Mark string }
	}
	if err := json.Unmarshal(b, &sm); err != nil || len(sm.Cases) == 0 {
		t.Fatal(err)
	}
	for _, c := range sm.Cases {
		sse := string((&translator{}).stepMark(relStep(c.Step, c.Dir, c.Scratch)))
		var d struct{ Delta struct{ Text string } }
		json.Unmarshal([]byte(strings.TrimPrefix(strings.SplitN(sse, "data: ", 2)[1], "")), &d)
		if d.Delta.Text != c.Mark {
			t.Errorf("%s: got %q, want %q", c.Name, d.Delta.Text, c.Mark)
		}
	}
}
