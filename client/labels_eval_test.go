package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"minigw/core"
	"minigw/coreapi"
)

// B4: minigw gw labels eval (serve_labels_eval.go, serve_labels_cli.go), the agreement list, the host's backends and the status line
// of the remote settings password record.

type evalFake struct {
	mu      sync.Mutex
	seen    map[string]int
	calls   atomic.Int32
	maxUser atomic.Int32
}

var evalScripts = map[string][3]string{
	"alpha/phase": {"do", "do", "do"}, "alpha/activity": {"bugfix", "bugfix", "bugfix"},
	"beta/phase": {"do", "do", "plan"}, "beta/activity": {"docs", "docs", "docs"},
	"gamma/phase": {"plan", "plan", "plan"}, "gamma/activity": {"nonsense", "nonsense", "nonsense"},
	"delta/activity": {"feature", "feature", "feature"},
}

func (e *evalFake) backend(ctx context.Context, system, user string) (string, error) {
	e.calls.Add(1)
	if n := int32(len(user)); n > e.maxUser.Load() {
		e.maxUser.Store(n)
	}
	q := "activity"
	if strings.Contains(system, "Which phase") {
		q = "phase"
	}
	for _, k := range []string{"alpha", "beta", "gamma", "delta"} {
		if strings.Contains(user, k) {
			e.mu.Lock()
			defer e.mu.Unlock()
			if e.seen == nil {
				e.seen = map[string]int{}
			}
			n := e.seen[k+"/"+q]
			e.seen[k+"/"+q]++
			return evalScripts[k+"/"+q][n%3], nil
		}
	}
	return "", errors.New(user)
}

func evalServer(t *testing.T) (*server, *evalFake) {
	t.Helper()
	s := newCoreFx(t, nil).s
	fake := &evalFake{}
	s.labels.backend = fake.backend
	// the feature stays OFF: an evaluation needs a backend, not the worker
	if st, b := s.adminDo("PUT", "/usage/labels/settings", []byte(`{"backend":"foundry:test"}`)); st != 200 {
		t.Fatal(st, b)
	}
	return s, fake
}

func evalBody(ex ...[3]string) []byte {
	var list []labelEvalExample
	for _, e := range ex {
		list = append(list, labelEvalExample{Text: e[0], Phase: e[1], Activity: e[2]})
	}
	b, _ := json.Marshal(labelEvalRequest{Examples: list})
	return b
}

func evalRun(t *testing.T, s *server, body []byte) (int, labelEvalResult, string) {
	t.Helper()
	st, obj := s.adminDo("POST", "/usage/labels/eval", body)
	raw, _ := json.Marshal(obj)
	var out struct {
		Eval labelEvalResult `json:"eval"`
	}
	_ = json.Unmarshal(raw, &out)
	return st, out.Eval, string(raw)
}

var evalExamples = [][3]string{
	{"alpha fix the crash", "do", "bugfix"},
	{"beta two of three", "do", "docs"},
	{"gamma wrong and unsure", "check", "test"},
	{"delta only an activity", "", "ops"},
}

func TestLabelsEvalNumbersAndStoresNothing(t *testing.T) {
	s, fake := evalServer(t)
	var before [3]int
	count := func(q string) (n int) { _ = s.core.DB().QueryRow(q).Scan(&n); return }
	for i, q := range []string{`SELECT count(*) FROM label_agreement`, `SELECT count(*) FROM usage_events WHERE phase_source='model' OR activity_source='model'`, `SELECT count(*) FROM usage_events`} {
		before[i] = count(q)
	}
	st, r, raw := evalRun(t, s, evalBody(evalExamples...))
	if st != 200 {
		t.Fatal(st, raw)
	}
	want := labelEvalResult{Backend: "foundry:test", Examples: 4, Run: 4,
		Phase:    labelEvalKind{Scored: 3, Two: labelEvalCell{Decided: 3, Correct: 2}, One: labelEvalCell{Decided: 2, Correct: 1}},
		Activity: labelEvalKind{Scored: 4, Two: labelEvalCell{Decided: 3, Correct: 2}, One: labelEvalCell{Decided: 3, Correct: 2}},
		Mistakes: map[string][]labelEvalConfusion{"phase": {{"check", "plan", 1}}, "activity": {{"ops", "feature", 1}}},
		Wrong:    []labelEvalWrong{{Kind: "phase", Line: 3, Expected: "check", Got: "plan"}, {Kind: "activity", Line: 3, Expected: "test"}, {Kind: "activity", Line: 4, Expected: "ops", Got: "feature"}}}
	gj, _ := json.Marshal(r)
	wj, _ := json.Marshal(want)
	if !bytes.Equal(gj, wj) {
		t.Fatalf("\n got %s\nwant %s", gj, wj)
	}
	// 3 examples x 6 samples + the activity-only one x 3
	if fake.calls.Load() != 21 {
		t.Fatal("samples", fake.calls.Load())
	}
	// stores nothing: no counter, no model label, and exactly one event more, the worker's own internal accounting (once for the run)
	if count(`SELECT count(*) FROM label_agreement`) != before[0] || count(`SELECT count(*) FROM usage_events WHERE phase_source='model' OR activity_source='model'`) != before[1] {
		t.Fatal("eval stored labels or agreement counters")
	}
	if count(`SELECT count(*) FROM usage_events`) != before[2]+1 {
		t.Fatal("expected one internal event")
	}
	if count(`SELECT count(*) FROM usage_events WHERE user_id IS NULL AND device_id='gateway' AND activity='ops' AND task='usage-labels' AND phase_source='internal' AND activity_source='internal' AND task_source='internal' AND outcome='ok'`) != 1 {
		t.Fatal("the internal event is not the worker's own kind")
	}
}

func TestLabelsEvalRefusals(t *testing.T) {
	s, _ := evalServer(t)
	big := make([][3]string, labelEvalMaxExamples+1)
	for i := range big {
		big[i] = [3]string{"alpha", "do", ""}
	}
	for name, c := range map[string]struct {
		body string
		want string
	}{
		"none":          {`{"examples":[]}`, "examples: 1 to 500"},
		"too many":      {string(evalBody(big...)), "examples: 1 to 500"},
		"unknown field": {`{"examples":[{"text":"alpha","phase":"do","extra":1}]}`, "examples: a list"},
		"not json":      {`nope`, "examples: a list"},
		"empty text":    {string(evalBody([3]string{"  ", "do", ""})), "example 1: text is empty"},
		"bad phase":     {string(evalBody([3]string{"alpha", "do", ""}, [3]string{"alpha", "doing", ""})), "example 2: phase is one of plan, do, check, act"},
		"bad activity":  {string(evalBody([3]string{"alpha", "", "gardening"})), "example 1: phase is one of"},
		"no label":      {string(evalBody([3]string{"alpha", "", ""})), "example 1: needs a phase or an activity"},
	} {
		st, obj := s.adminDo("POST", "/usage/labels/eval", []byte(c.body))
		if raw, _ := json.Marshal(obj); st != 400 || !strings.Contains(string(raw), c.want) {
			t.Errorf("%s: %d %s", name, st, raw)
		}
	}
	// a label is a word of the configured list (the owner's own activities count), case does not matter
	if st, _, raw := evalRun(t, s, evalBody([3]string{"alpha", "DO", "Bugfix"})); st != 200 {
		t.Fatal(st, raw)
	}
	// no backend chosen
	s2 := newCoreFx(t, nil).s
	if st, obj := s2.adminDo("POST", "/usage/labels/eval", evalBody(evalExamples[0])); st != 409 || !strings.Contains(fmt.Sprint(obj), "choose a backend") {
		t.Fatal(st, obj)
	}
	// the backend is not one this host runs (a stored apple-fm, no test backend): the worker's own refusal
	s2.labels.backend = nil
	apple := "apple-fm"
	if _, err := s2.core.SetLabelSettings(t0, core.LabelSettingsPatch{Backend: &apple}); err != nil {
		t.Fatal(err)
	}
	if st, obj := s2.adminDo("POST", "/usage/labels/eval", evalBody(evalExamples[0])); st != 409 || !strings.Contains(fmt.Sprint(obj), "label backend unavailable on this gateway") {
		t.Fatal(st, obj)
	}
	// the gateway is not running: the model is not here
	off := &server{offline: true, log: s.log}
	if st, _ := off.adminDo("POST", "/usage/labels/eval", evalBody(evalExamples[0])); st != 503 {
		t.Fatal(st)
	}
}

// Each text is capped as the worker caps it (2000 code points, invalid UTF-8 replaced) before the model sees it.
func TestLabelsEvalCapsEachText(t *testing.T) {
	s, fake := evalServer(t)
	text := "alpha " + strings.Repeat("é", 5000)
	if st, _, raw := evalRun(t, s, evalBody([3]string{text, "do", ""})); st != 200 {
		t.Fatal(st, raw)
	}
	// INPUT_DATA is the JSON string of the capped text: 2000 code points of at most 2 bytes here, plus the wrapper
	if n := fake.maxUser.Load(); n > 2000*2+100 || n < 3000 {
		t.Fatal("the model saw", n, "bytes")
	}
	if !utf8.ValidString(text) {
		t.Fatal("test text")
	}
}

// One evaluation at a time: a second one is refused with a clear text while the first one runs, and it runs again afterwards.
func TestLabelsEvalOneAtATime(t *testing.T) {
	s, _ := evalServer(t)
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	s.labels.backend = func(ctx context.Context, system, user string) (string, error) {
		once.Do(func() { close(started) })
		<-release
		return "do", nil
	}
	done := make(chan int, 1)
	go func() { st, _, _ := evalRun(t, s, evalBody([3]string{"alpha", "do", ""})); done <- st }()
	<-started
	st, obj := s.adminDo("POST", "/usage/labels/eval", evalBody(evalExamples[0]))
	if raw, _ := json.Marshal(obj); st != 429 || !strings.Contains(string(raw), "Another label evaluation is running") {
		t.Fatal(st, string(raw))
	}
	close(release)
	if st := <-done; st != 200 {
		t.Fatal(st)
	}
	if st, _, raw := evalRun(t, s, evalBody([3]string{"alpha", "do", ""})); st != 200 {
		t.Fatal("not released", st, raw)
	}
}

// The user's own requests keep priority exactly as for the worker: no sample while a foreground request runs, and a sample that one
// ends is tried again.
func TestLabelsEvalForegroundHasPriority(t *testing.T) {
	s, fake := evalServer(t)
	end := s.foundry.beginForeground()
	done := make(chan labelEvalResult, 1)
	go func() { _, r, _ := evalRun(t, s, evalBody([3]string{"alpha", "do", ""})); done <- r }()
	time.Sleep(60 * time.Millisecond)
	if fake.calls.Load() != 0 {
		t.Fatal("a sample started during foreground work")
	}
	end()
	select {
	case r := <-done:
		if r.Run != 1 || fake.calls.Load() != 3 {
			t.Fatal(r, fake.calls.Load())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("eval did not continue after the foreground request")
	}
	// a sample in flight is cancelled by the arrival of a user's request, and its example is tried again
	started := make(chan struct{})
	var first atomic.Bool
	s.labels.backend = func(ctx context.Context, system, user string) (string, error) {
		if first.CompareAndSwap(false, true) {
			close(started)
			<-ctx.Done()
			return "", ctx.Err()
		}
		return "do", nil
	}
	go func() { _, r, _ := evalRun(t, s, evalBody([3]string{"alpha", "do", ""})); done <- r }()
	<-started
	s.foundry.beginForeground()()
	select {
	case r := <-done:
		if r.Run != 1 || r.NotRun != 0 {
			t.Fatal("the preempted example was not retried", r)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("eval did not retry")
	}
}

// A backend that fails every time, or a decision that takes longer than its budget, gives no result and no text anywhere.
func TestLabelsEvalFailureIsAClearNoAnswer(t *testing.T) {
	s, _ := evalServer(t)
	s.labels.budget = 20 * time.Millisecond
	s.labels.backend = func(ctx context.Context, system, user string) (string, error) {
		<-ctx.Done()
		return "", errors.New(user)
	}
	st, obj := s.adminDo("POST", "/usage/labels/eval", evalBody(evalExamples[0], evalExamples[1]))
	if raw, _ := json.Marshal(obj); st != 502 || !strings.Contains(string(raw), "no answer for any example") {
		t.Fatal(st, string(raw))
	}
	var n int
	_ = s.core.DB().QueryRow(`SELECT count(*) FROM usage_events WHERE device_id='gateway' AND outcome='error'`).Scan(&n)
	if n != 1 {
		t.Fatal("the failed run is one internal event", n)
	}
}

// The marker of the B2 test, now on the eval path: the example text is nowhere on disk, in the logs, in the audit trail or in any API
// answer (the eval answer included); only the terminal output of --show-errors has it.
func TestLabelsEvalMarkerNeverPersistedLoggedOrReturned(t *testing.T) {
	for _, failure := range []string{"error", "panic", "timeout", "garbage", "success"} {
		t.Run(failure, func(t *testing.T) {
			f := newCoreFx(t, nil)
			s := f.s
			marker := "PRIVATE-EVAL-MARKER-514902"
			var logs bytes.Buffer
			s.log.SetOutput(&logs)
			s.labels.budget = 20 * time.Millisecond
			s.labels.backend = func(ctx context.Context, system, user string) (string, error) {
				switch failure {
				case "error":
					return "", errors.New(user)
				case "panic":
					panic(user)
				case "timeout":
					<-ctx.Done()
					return "", fmt.Errorf("timeout: %s", user)
				case "success":
					if strings.Contains(system, "Which phase") {
						return "do", nil
					}
					return "bugfix", nil
				}
				return user, nil
			}
			if st, b := s.adminDo("PUT", "/usage/labels/settings", []byte(`{"backend":"foundry:test"}`)); st != 200 {
				t.Fatal(st, b)
			}
			// a normal user's event too, so that every API has rows to show
			r := labelReq(s, "ordinary", "do", "docs")
			r.event.UserID, r.event.ProjectID, r.event.DeviceID = f.uid["ana"], f.pid["fakture"], devID(f.dev["ana"])
			s.dispatched(r)
			s.endEvent(r, 200, nil)
			s.api.FlushRecorder()
			st, _, raw := evalRun(t, s, evalBody([3]string{marker, "do", "bugfix"}))
			if failure == "success" && st != 200 || failure == "garbage" && st != 200 || failure != "success" && failure != "garbage" && st != 502 {
				t.Fatalf("status %d %s", st, raw)
			}
			if strings.Contains(raw, marker) {
				t.Fatal("the eval answer carries the text")
			}
			for _, path := range []string{"/usage/events", "/usage/sessions", "/usage/labels/settings", "/usage/labels/agreement", "/audit", "/panels", "/panels/strings"} {
				query := ""
				if path == "/usage/sessions" {
					query = fmt.Sprintf("project=%d", f.pid["fakture"])
				}
				response := s.api.Handle(t0, coreapi.Context{Kind: coreapi.KindAdmin}, coreapi.Request{Method: "GET", Path: path, RawQuery: query})
				if response.Status != 200 || bytes.Contains(response.Body, []byte(marker)) {
					t.Fatalf("API %s: %d", path, response.Status)
				}
			}
			if strings.Contains(logs.String(), marker) {
				t.Fatal("log leaked the text")
			}
			if err := filepath.WalkDir(s.cfgDir, func(path string, d os.DirEntry, err error) error {
				if err != nil || d.IsDir() || d.Name() == "core.lock" { // lock file: no content, and Windows refuses to read it while the core runs
					return err
				}
				b, e := os.ReadFile(path)
				if e == nil && bytes.Contains(b, []byte(marker)) {
					t.Error("the text is persisted in", path)
				}
				return e
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// The command end to end against a gateway: the numbers on stdout, the text only with --show-errors, from the client's own copy.
func TestLabelsEvalCommandPrintsNumbersAndTextOnlyOnRequest(t *testing.T) {
	s, _ := evalServer(t)
	a := &adminClient{off: s}
	marker := "SHOWN-ONLY-ON-REQUEST-771"
	file := filepath.Join(t.TempDir(), "examples.jsonl")
	lines := []string{
		`{"text":"alpha fix the crash","phase":"do","activity":"bugfix"}`,
		``,
		`{"text":"gamma ` + marker + `","phase":"check","activity":"test"}`,
	}
	if err := os.WriteFile(file, []byte(strings.Join(lines, "\r\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, errText, exit := captured(func() { coreCommand(a, []string{"labels", "eval"}, coreOpts{file: file}) })
	if exit != 0 || errText != "" || strings.Contains(out, marker) || !strings.Contains(out, "phase: 2 hand-labelled") || !strings.Contains(out, "check -> plan  1") {
		t.Fatalf("exit %d\n%s\n%s", exit, out, errText)
	}
	if !strings.Contains(out, "misses: 2 (--show-errors lists them") {
		t.Fatal(out)
	}
	out, _, exit = captured(func() { coreCommand(a, []string{"labels", "eval"}, coreOpts{file: file, showErrors: true}) })
	if exit != 0 || !strings.Contains(out, "line 3  phase  check -> plan\n    gamma "+marker) || !strings.Contains(out, "line 3  activity  test -> -\n") {
		t.Fatalf("%d %s", exit, out)
	}
}

// The file's own limits, checked before anything is sent; an error names the line, never its text.
func TestLabelsEvalFileLimits(t *testing.T) {
	var sent int
	a := &adminClient{fake: func(method, path string, body []byte) (int, []byte) { sent++; return 599, []byte(`{}`) }}
	write := func(content string) string {
		p := filepath.Join(t.TempDir(), "x.jsonl")
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	marker := "LINE-TEXT-MARKER-3321"
	line := `{"text":"alpha","phase":"do"}` + "\n"
	for name, c := range map[string]struct{ content, want string }{
		"file over 1 MiB":     {strings.Repeat(" ", 1<<20) + line, "the file is larger than 1 MiB"},
		"over 500 lines":      {strings.Repeat(line, 501), "at most 500 examples"},
		"bad line":            {line + `{"text":"` + marker + `","phase":"do",}` + "\n", "line 2: one JSON object"},
		"unknown key":         {`{"text":"` + marker + `","phase":"do","note":1}` + "\n", "line 1: one JSON object"},
		"no text":             {`{"phase":"do"}` + "\n", "line 1: text is required"},
		"no label":            {line + `{"text":"` + marker + `"}` + "\n", "line 2: needs a phase or an activity"},
		"invalid UTF-8":       {line + "{\"text\":\"\xff\",\"phase\":\"do\"}\n", "not valid UTF-8"},
		"empty":               {"\n\n", "no examples"},
		"examples over 1 MiB": {strings.Repeat(`{"text":"`+strings.Repeat("<", 1990)+`","phase":"do"}`+"\n", 500), "larger than the 1 MiB"},
	} {
		out, errText, exit := captured(func() { coreCommand(a, []string{"labels", "eval"}, coreOpts{file: write(c.content)}) })
		if exit != 1 || !strings.Contains(errText, c.want) || strings.Contains(out+errText, marker) {
			t.Errorf("%s: exit %d %q %q", name, exit, out, errText)
		}
	}
	if _, errText, exit := captured(func() { coreCommand(a, []string{"labels", "eval"}, coreOpts{}) }); exit != 1 || !strings.Contains(errText, "--file") {
		t.Error("no file", exit, errText)
	}
	if _, errText, exit := captured(func() {
		coreCommand(a, []string{"labels", "eval"}, coreOpts{file: filepath.Join(t.TempDir(), "missing.jsonl")})
	}); exit != 1 || !strings.Contains(errText, "cannot open the file") {
		t.Error("missing file", exit, errText)
	}
	if sent != 0 {
		t.Fatal("something was sent", sent)
	}
}

// Who can reach the route: the owner on the admin channel of the gateway, nobody else. It is not a coreapi route (so no member, no
// internal call of the Mac helper reaches it) and the remote settings listener does not list it.
func TestLabelsEvalIsOnlyTheOwnersAdminRoute(t *testing.T) {
	f := newCoreFx(t, nil)
	s := f.s
	body := evalBody(evalExamples[0])
	for _, p := range []string{"/usage/labels/eval", "/minigw/v1/usage/labels/eval", "/minigw/v1/projects/1/usage/labels/eval"} {
		for _, cx := range []coreapi.Context{{Kind: coreapi.KindMember, DeviceID: devID(f.dev["ana"]), Approved: true}, {Kind: coreapi.KindInternal}, {Kind: coreapi.KindAdmin}} {
			if r := s.api.Handle(t0, cx, coreapi.Request{Method: "POST", Path: p, Body: body}); r.Status == 200 {
				t.Fatalf("kind %v reached %s", cx.Kind, p)
			}
		}
	}
	if coreapi.IsAdminPath("POST", "/usage/labels/eval") || coreapi.IsMemberPath("/minigw/v1/usage/labels/eval") {
		t.Fatal("a coreapi route")
	}
	for _, m := range []string{"GET", "POST", "PUT", "PATCH", "DELETE"} {
		if remoteAdminAllowed(m, adminPrefix+"/usage/labels/eval") || remoteAdminAllowed(m, adminPrefix+"/usage/labels/agreement") {
			t.Fatal("the remote settings listener lists it", m)
		}
	}
	rt := remoteTransport(t)
	for _, m := range []string{"POST", "GET"} {
		if w := rt.run(rt.request(m, adminPrefix+"/usage/labels/eval", string(body))); w.Code != 404 {
			t.Fatal("remote settings reached the eval route", m, w.Code, w.Body.String())
		}
	}
}

// The settings row names what THIS gateway can run, and the Go gateway refuses the Mac's backend with the clear text.
func TestGoGatewayLabelBackendsAreFoundryOnly(t *testing.T) {
	s := newCoreFx(t, nil).s
	st, obj := s.adminDo("PUT", "/usage/labels/settings", []byte(`{"backend":"apple-fm"}`))
	if st != 400 || !strings.Contains(fmt.Sprint(obj), "backend: not available on this system; choose foundry:<model>") {
		t.Fatal(st, obj)
	}
	if st, obj = s.adminDo("PUT", "/usage/labels/settings", []byte(`{"backend":"foundry:phi-4-mini"}`)); st != 200 {
		t.Fatal(st, obj)
	}
	_, obj = s.adminDo("GET", "/usage/labels/settings", nil)
	raw, _ := json.Marshal(obj)
	if !strings.Contains(string(raw), `"backends":"foundry:\u003cmodel\u003e"`) || !strings.Contains(string(raw), `"backendProblem":""`) {
		t.Fatal(string(raw))
	}
}

// minigw gw status says where the remote settings password record is kept, when there is one, and never what is in it.
func TestStatusSaysWhereTheRemoteSettingsPasswordIsKept(t *testing.T) {
	s, cfg, _ := liveServer(t, nil)
	status := func() (apiStatus, string) {
		_, obj := s.adminDo("GET", "/status", nil)
		b, _ := json.Marshal(obj)
		var st apiStatus
		if err := json.Unmarshal(b, &st); err != nil {
			t.Fatal(err)
		}
		return st, stdout(t, func() { routerCommand([]string{"status", "--config", cfg}, strings.NewReader("")) })
	}
	if st, out := status(); st.RemotePassword != "" || strings.Contains(out, "remote settings password") {
		t.Fatal("a line without a record", st.RemotePassword, out)
	}
	marker := "PASSWORD-RECORD-MARKER-9917"
	path := filepath.Join(s.cfgDir, "remote-settings.password.json")
	if err := os.WriteFile(path, []byte(`{"salt":"`+marker+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	st, out := status()
	if st.RemotePassword != "file "+path || !strings.Contains(out, "remote settings password: file "+path+"\n") || strings.Contains(out, marker) {
		t.Fatalf("%q\n%s", st.RemotePassword, out)
	}
	_, obj := s.adminDo("GET", "/status", nil)
	if b, _ := json.Marshal(obj); strings.Contains(string(b), marker) {
		t.Fatal("the content is in the status answer")
	}
}

var _ = http.StatusOK
