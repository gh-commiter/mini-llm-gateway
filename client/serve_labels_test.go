package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"minigw/core"
	"minigw/coreapi"
	"minigw/decide"
)

func enableTestLabels(t *testing.T, s *server, extra string) {
	t.Helper()
	status, body := s.adminDo("PUT", "/usage/labels/settings", []byte(`{"enabled":true,"backend":"foundry:test","shadow":false`+extra+`}`))
	if status != 200 {
		t.Fatalf("settings: %d %+v", status, body)
	}
}

func labelReq(s *server, text, phase, activity string) *req {
	body, _ := json.Marshal(map[string]any{"messages": []any{map[string]any{"role": "user", "content": text}}})
	hr := httptest.NewRequest("POST", "/v1/messages", nil)
	hr.Header.Set("x-claude-mini-phase", phase)
	hr.Header.Set("x-claude-mini-activity", activity)
	r := &req{Request: hr, conn: &serveConn{}, provider: "test", body: body}
	s.admitEvent(r, "test")
	return r
}

func waitLabels(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute) // hang guard only; slow disks (Windows CI) need far more than 5 s for 1000 fixtures
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("label worker did not finish")
		}
		time.Sleep(time.Millisecond)
	}
}

func labelsOf(t *testing.T, s *server, id string) (phase, activity string) {
	t.Helper()
	if err := s.core.DB().QueryRow(`SELECT phase,activity FROM usage_events WHERE id=?`, id).Scan(&phase, &activity); err != nil {
		t.Fatal(err)
	}
	return
}

func fakeLabelBackend(c context.Context, system, user string) (string, error) {
	if strings.Contains(system, "Which phase") {
		return "do", nil
	}
	return "bugfix", nil
}

func TestLabelsOffNoWorkerAndOnlyAfterDispatch(t *testing.T) {
	f := newCoreFx(t, nil)
	s := f.s
	var calls atomic.Int32
	s.labels.backend = func(context.Context, string, string) (string, error) { calls.Add(1); return "do", nil }
	r := labelReq(s, "private", "", "")
	s.dispatched(r)
	s.endEvent(r, 200, nil)
	if s.labels.queue != nil || calls.Load() != 0 {
		t.Fatal("worker started while disabled")
	}
	enableTestLabels(t, s, "")
	r = labelReq(s, "not dispatched", "", "")
	s.enqueueLabels(r)
	if s.labels.queue != nil {
		t.Fatal("started before dispatch")
	}
	r.event.ParentID = "root"
	r.evDispatched = true
	s.enqueueLabels(r)
	r.event.ParentID = ""
	r.event.TaskSource = core.LabelInternal
	s.enqueueLabels(r)
	if s.labels.queue != nil {
		t.Fatal("nested/internal event classified")
	}
}

func TestLabelsBlockedBackendDoesNotHoldUserRequestAndStops(t *testing.T) {
	keys := fakeKeys(t)
	keys["test"] = gwKey
	f := newCoreFx(t, nil)
	s := f.s
	started := make(chan struct{})
	stopped := make(chan struct{})
	var backendDone atomic.Bool // set only when the backend finished without being cancelled
	s.labels.backend = func(ctx context.Context, system, user string) (string, error) {
		close(started)
		timer := time.NewTimer(30 * time.Second)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			close(stopped)
			return "", ctx.Err()
		case <-timer.C:
			backendDone.Store(true)
			return "do", nil
		}
	}
	enableTestLabels(t, s, "")
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"id":"msg_test","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer up.Close()
	r := labelReq(s, "Fix the crash", "", "")
	uw := &usageWriter{ResponseWriter: httptest.NewRecorder()}
	status := s.passthrough(uw, r, serveGateway{Name: "test", BaseURL: up.URL, Insecure: true}, gwModel{id: "test"})
	s.endEvent(r, status, uw)
	if status != 200 || backendDone.Load() {
		t.Fatalf("request delayed/failed: %d done=%v", status, backendDone.Load())
	}
	select {
	case <-started:
	case <-time.After(2 * time.Minute):
		t.Fatal("backend not started")
	}
	s.api.FlushRecorder()
	if p, a := labelsOf(t, s, r.event.ID); p != "" || a != "" {
		t.Fatal(p, a)
	}
	s.labels.stop() // returns only when the worker is gone; a hang fails via the go test timeout
	select {
	case <-stopped:
	default:
		t.Fatal("backend not cancelled")
	}
	if len(s.labels.queue) != 0 {
		t.Fatal("queued input retained after shutdown")
	}
	// Stopped means no new worker can be started.
	s.dispatched(labelReq(s, "after shutdown", "", ""))
	if len(s.labels.queue) != 0 {
		t.Fatal("enqueue after shutdown")
	}
}

func TestLabelsQueueFloodRateAndSingleFlight(t *testing.T) {
	f := newCoreFx(t, nil)
	s := f.s
	started := make(chan struct{})
	release := make(chan struct{})
	var calls, active, maxActive atomic.Int32
	s.labels.backend = func(ctx context.Context, system, user string) (string, error) {
		n := active.Add(1)
		defer active.Add(-1)
		if n > maxActive.Load() {
			maxActive.Store(n)
		}
		if calls.Add(1) == 1 {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		return fakeLabelBackend(ctx, system, user)
	}
	enableTestLabels(t, s, `,"maxPerHour":1`)
	s.dispatched(labelReq(s, "first", "", ""))
	<-started
	for i := 0; i < 1000; i++ {
		s.dispatched(labelReq(s, fmt.Sprintf("flood %d", i), "", ""))
	}
	if len(s.labels.queue) != 8 || cap(s.labels.queue) != 8 || s.labels.skipped.Load() != 992 {
		t.Fatal(len(s.labels.queue), s.labels.skipped.Load())
	}
	close(release)
	waitLabels(t, func() bool { return s.labels.skipped.Load() == 1000 })
	s.labels.stop()
	if calls.Load() != 6 || maxActive.Load() != 1 {
		t.Fatal(calls.Load(), maxActive.Load())
	}
}

func TestLabelsStoreDedupeExplicitAndLateChildren(t *testing.T) {
	f := newCoreFx(t, nil)
	s := f.s
	var calls atomic.Int32
	s.labels.backend = func(c context.Context, system, user string) (string, error) {
		calls.Add(1)
		return fakeLabelBackend(c, system, user)
	}
	enableTestLabels(t, s, "")
	root := labelReq(s, "Fix same crash", "plan", "")
	s.dispatched(root)
	waitLabels(t, func() bool {
		var activity string
		_ = s.core.DB().QueryRow(`SELECT activity FROM usage_events WHERE id=?`, root.event.ID).Scan(&activity)
		return activity == "bugfix"
	})
	if p, a := labelsOf(t, s, root.event.ID); p != "plan" || a != "bugfix" {
		t.Fatal(p, a)
	}
	kid := core.UsageEvent{ID: "late-kid", ParentID: root.event.ID, Start: time.Now(), DeviceID: "d", Model: "test", Provider: "test"}
	if err := s.core.BeginEvent(t0, kid); err != nil {
		t.Fatal(err)
	}
	s.endEvent(root, 200, nil)
	s.api.FlushRecorder()
	if _, a := labelsOf(t, s, kid.ID); a != "bugfix" {
		t.Fatal("late child missed label", a)
	}
	repeat := labelReq(s, "Fix same crash", "", "")
	s.dispatched(repeat)
	waitLabels(t, func() bool {
		var activity string
		_ = s.core.DB().QueryRow(`SELECT activity FROM usage_events WHERE id=?`, repeat.event.ID).Scan(&activity)
		return activity == "bugfix"
	})
	s.labels.stop()
	if calls.Load() != 6 {
		t.Fatal("cache missed", calls.Load())
	}
	var n int
	if err := s.core.DB().QueryRow(`SELECT count(*) FROM usage_events WHERE device_id='gateway' AND user_id IS NULL AND parent_id IS NULL AND activity='ops' AND task='usage-labels' AND phase_source='internal' AND activity_source='internal' AND task_source='internal' AND token_source='estimated' AND billing_mode='local'`).Scan(&n); err != nil || n != 1 {
		t.Fatal(n, err)
	}
}

func TestLabelsShadowOneInFourOnlyCounters(t *testing.T) {
	f := newCoreFx(t, nil)
	s := f.s
	var calls atomic.Int32
	s.labels.backend = func(c context.Context, system, user string) (string, error) {
		calls.Add(1)
		return fakeLabelBackend(c, system, user)
	}
	enableTestLabels(t, s, `,"shadow":true,"shadowRate":4`)
	var roots []*req
	for i := 0; i < 4; i++ {
		r := labelReq(s, "Fix a crash", "do", "docs")
		s.dispatched(r)
		roots = append(roots, r)
	}
	waitLabels(t, func() bool { rows, _ := s.core.Agreement(t0, 30); return len(rows) == 2 })
	s.labels.stop()
	rows, err := s.core.Agreement(t0, 30)
	if err != nil || len(rows) != 2 || rows[0] != (core.AgreementRow{Kind: "activity", Bucket: "1000", N: 1, ID: "activity/1000", Percent: "0 %", Count: "1", Days: rows[0].Days}) || rows[1] != (core.AgreementRow{Kind: "phase", Bucket: "1000", Agree: 1, N: 1, ID: "phase/1000", Percent: "100 %", Count: "1", Days: rows[1].Days}) || calls.Load() != 6 {
		t.Fatal(rows, err, calls.Load())
	}
	for _, r := range roots {
		if p, a := labelsOf(t, s, r.event.ID); p != "do" || a != "docs" {
			t.Fatal(p, a)
		}
	}
}

func TestLabelsPromptMarkerNeverPersistedLoggedOrReturned(t *testing.T) {
	for _, failure := range []string{"error", "panic", "timeout", "garbage", "success"} {
		t.Run(failure, func(t *testing.T) {
			f := newCoreFx(t, nil)
			s := f.s
			marker := "PRIVATE-PROMPT-MARKER-837215"
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
					return fakeLabelBackend(ctx, system, user)
				default:
					return user, nil
				}
			}
			enableTestLabels(t, s, "")
			r := labelReq(s, marker, "", "")
			r.event.UserID = f.uid["ana"]
			r.event.ProjectID = f.pid["fakture"]
			r.event.DeviceID = devID(f.dev["ana"])
			s.dispatched(r)
			s.endEvent(r, 200, nil)
			if failure == "success" {
				waitLabels(t, func() bool {
					var a string
					_ = s.core.DB().QueryRow(`SELECT activity FROM usage_events WHERE id=?`, r.event.ID).Scan(&a)
					return a == "bugfix"
				})
			} else if failure == "garbage" {
				waitLabels(t, func() bool {
					var n int
					_ = s.core.DB().QueryRow(`SELECT count(*) FROM usage_events WHERE device_id='gateway'`).Scan(&n)
					return n == 1
				})
			} else {
				waitLabels(t, func() bool { return s.labels.failed.Load() == 1 })
			}
			s.labels.stop()
			s.api.FlushRecorder()
			p, a := labelsOf(t, s, r.event.ID)
			if failure == "success" {
				if p != "do" || a != "bugfix" {
					t.Fatal(p, a)
				}
			} else if p != "" || a != "" {
				t.Fatal(p, a)
			}
			for _, path := range []string{"/usage/events", "/usage/sessions", "/usage/labels/settings", "/usage/labels/agreement", "/audit", "/panels", "/panels/strings"} {
				query := ""
				if path == "/usage/sessions" {
					query = fmt.Sprintf("project=%d", f.pid["fakture"])
				}
				response := s.api.Handle(t0, coreapi.Context{Kind: coreapi.KindAdmin}, coreapi.Request{Method: "GET", Path: path, RawQuery: query})
				if response.Status != 200 {
					t.Fatalf("API check did not reach %s: %d %s", path, response.Status, response.Body)
				}
				if bytes.Contains(response.Body, []byte(marker)) {
					t.Fatal("API leaked prompt", path)
				}
			}
			for _, path := range []string{"/minigw/v1/usage", fmt.Sprintf("/minigw/v1/projects/%d/sessions", f.pid["fakture"])} {
				response := s.api.Handle(t0, coreapi.Context{Kind: coreapi.KindMember, DeviceID: devID(f.dev["ana"]), Approved: true}, coreapi.Request{Method: "GET", Path: path})
				if response.Status != 200 || bytes.Contains(response.Body, []byte(marker)) {
					t.Fatalf("member API %s: %d %s", path, response.Status, response.Body)
				}
			}
			var settings core.LabelSettings
			if err := s.api.Call(t0, "label-settings", nil, &settings); err != nil {
				t.Fatal(err)
			}
			b, _ := json.Marshal(settings)
			if bytes.Contains(b, []byte(marker)) {
				t.Fatal("internal API leaked")
			}
			if strings.Contains(logs.String(), marker) {
				t.Fatal("log leaked prompt")
			}
			// Includes SQLite, WAL, audit rows, state, spool and all files in the temporary config folder.
			if err := filepath.WalkDir(s.cfgDir, func(path string, d os.DirEntry, err error) error {
				if err != nil {
					return err
				}
				if d.IsDir() {
					return nil
				}
				if d.Name() == "core.lock" { // lock file: no content, and Windows refuses to read it while the core runs
					return nil
				}
				b, e := os.ReadFile(path)
				if e == nil && bytes.Contains(b, []byte(marker)) {
					t.Error("prompt persisted in", path)
				}
				return e
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestLabelInputSharedRemindersFirstUserAndCaps(t *testing.T) {
	b, err := os.ReadFile("testdata/decide-reminders.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []struct{ Name, Input, Expected string }
	if err = json.Unmarshal(b, &fixtures); err != nil {
		t.Fatal(err)
	}
	for _, f := range fixtures {
		t.Run(f.Name, func(t *testing.T) {
			if got := stripReminders(f.Input); got != f.Expected {
				t.Fatalf("%q != %q", got, f.Expected)
			}
		})
	}
	body := []byte(`{"messages":[{"role":"assistant","content":"ignore"},{"role":"user","content":[{"type":"text","text":"<system-reminder>secret</system-reminder>first"},{"type":"tool_result","text":"tool secret"}]},{"role":"user","content":"second"}]}`)
	text, key := labelInput(body, "device", "fix-145")
	if text != "task: fix-145\nfirst" {
		t.Fatal(text)
	}
	_, other := labelInput(body, "other", "fix-145")
	if key == other {
		t.Fatal("device not in key")
	}
	body, _ = json.Marshal(map[string]any{"messages": []any{map[string]any{"role": "user", "content": strings.Repeat("🧪", 2100)}}})
	text, _ = labelInput(body, "", "")
	if utf8.RuneCountInString(text) != 2000 || !utf8.ValidString(text) {
		t.Fatal("input cap")
	}
	text, _ = labelInput([]byte("{\"messages\":[{\"role\":\"user\",\"content\":\"\xff\"}]}"), "", "")
	if !utf8.ValidString(text) {
		t.Fatal("invalid UTF-8")
	}
}

func TestFoundryLabelsRefuseRemoteAndUseDirectLocalCompletion(t *testing.T) {
	for _, base := range []string{"http://example.com:80", "http://192.0.2.1:80", "http://localhost:80", "https://127.0.0.1:80", "http://x@127.0.0.1:80", "http://127.0.0.1:80/?x=1", "http://[::ffff:192.0.2.1]:80"} {
		if _, err := foundryLabelCompletion(t0, base, "test", "private", "private"); err == nil {
			t.Fatal("accepted endpoint", base)
		}
	}
	var completions, loads atomic.Int32
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/openai/load/test" {
			loads.Add(1)
			return
		}
		if r.URL.Path != "/v1/chat/completions" {
			t.Error("recursive/wrong endpoint", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		completions.Add(1)
		var b struct {
			Temperature float64                          `json:"temperature"`
			Max         int                              `json:"max_completion_tokens"`
			Stream      bool                             `json:"stream"`
			Messages    []struct{ Role, Content string } `json:"messages"`
		}
		if json.NewDecoder(r.Body).Decode(&b) != nil || b.Temperature != 0.7 || b.Max != 16 || b.Stream || len(b.Messages) != 2 || b.Messages[0].Content != "fixed" || b.Messages[1].Content != "data" {
			t.Error("bad constrained request")
		}
		io.WriteString(w, `{"choices":[{"message":{"content":"do"}}]}`)
	}))
	defer local.Close()
	got, err := foundryLabelCompletion(t0, local.URL, "test", "fixed", "data")
	if err != nil || got != "do" || completions.Load() != 1 || loads.Load() != 1 {
		t.Fatal(got, err, completions.Load(), loads.Load())
	}
}

func TestLabelsCacheTTLBoundAndRollingRate(t *testing.T) {
	cache := labelCache{}
	now := time.Now()
	for i := 0; i < 501; i++ {
		cache.put(sha256.Sum256([]byte(fmt.Sprint(i))), labelCacheEntry{at: now.Add(time.Duration(i) * time.Second)})
	}
	if len(cache) != 500 {
		t.Fatal(len(cache))
	}
	cache.put(sha256.Sum256([]byte("500")), labelCacheEntry{at: now.Add(500 * time.Second)})
	if len(cache) != 500 {
		t.Fatal("replacement evicted an entry")
	}
	if _, ok := cache[sha256.Sum256([]byte("0"))]; ok {
		t.Fatal("oldest not evicted")
	}
	cache.expire(now.Add(6 * time.Hour))
	if len(cache) != 500 {
		t.Fatal("expired too early")
	}
	cache.expire(now.Add(6*time.Hour + time.Second))
	if len(cache) != 499 {
		t.Fatal("TTL boundary", len(cache))
	}
	cache.expire(now.Add(7 * time.Hour))
	if len(cache) != 0 {
		t.Fatal("cache not expired")
	}
	var rate labelRate
	if !rate.take(now, 2) || !rate.take(now.Add(time.Minute), 2) || rate.take(now.Add(59*time.Minute), 2) || !rate.take(now.Add(time.Hour), 2) || rate.take(now.Add(time.Hour), 2) {
		t.Fatal("rolling hour rate")
	}
}

func TestLabelsDisableCancelsInFlightAndDropsQueued(t *testing.T) {
	f := newCoreFx(t, nil)
	s := f.s
	started := make(chan struct{})
	cancelled := make(chan struct{})
	var calls atomic.Int32
	s.labels.backend = func(ctx context.Context, system, user string) (string, error) {
		calls.Add(1)
		deadline, ok := ctx.Deadline()
		if !ok || time.Until(deadline) > 20*time.Second {
			t.Error("decision budget missing")
		}
		close(started)
		<-ctx.Done()
		close(cancelled)
		return "", ctx.Err()
	}
	enableTestLabels(t, s, "")
	r := labelReq(s, "blocked", "", "")
	s.dispatched(r)
	<-started
	s.dispatched(labelReq(s, "queued", "", ""))
	status, body := s.adminDo("PUT", "/usage/labels/settings", []byte(`{"enabled":false}`))
	if status != 200 {
		t.Fatal(status, body)
	}
	select {
	case <-cancelled:
	case <-time.After(2 * time.Minute):
		t.Fatal("disable did not cancel")
	}
	waitLabels(t, func() bool { return s.labels.skipped.Load() == 2 })
	s.labels.stop()
	if calls.Load() != 1 {
		t.Fatal("queued call ran after disable")
	}
	if settings, err := s.core.LabelSettings(t0); err != nil || settings.LastError != "" || s.labels.failed.Load() != 0 {
		t.Fatal("owner cancellation reported as a backend failure", settings, err)
	}
	if p, a := labelsOf(t, s, r.event.ID); p != "" || a != "" {
		t.Fatal(p, a)
	}
}

func TestLabelsUnavailableBackendIdles(t *testing.T) {
	f := newCoreFx(t, nil)
	s := f.s
	// Apple FM is deliberately unsupported by the Go worker on every platform.
	// A fake enables exercising worker lifecycle on the Mac test host; remove it before dispatch on other hosts.
	s.labels.backend = fakeLabelBackend
	enableTestLabels(t, s, "")
	// The Go gateway refuses apple-fm at the settings route; a stored one (an older version or a restore) is written directly.
	if status, body := s.adminDo("PUT", "/usage/labels/settings", []byte(`{"backend":"apple-fm"}`)); status != 400 || !strings.Contains(fmt.Sprint(body), "choose foundry:<model>") {
		t.Fatal(status, body)
	}
	apple := "apple-fm"
	stored, err := s.core.SetLabelSettings(t0, core.LabelSettingsPatch{Backend: &apple})
	if err != nil {
		t.Fatal(err)
	}
	s.labels.configure(stored)
	// Check production platform gating directly through the worker, without a real model.
	s.labels.backend = nil
	s.labels.mu.Lock()
	s.labels.queue = make(chan labelJob, 8)
	s.labels.done = make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	s.labels.cancel = cancel
	s.labels.queue <- labelJob{generation: s.labels.generation}
	s.labels.mu.Unlock()
	go s.runLabels(ctx)
	waitLabels(t, func() bool { return s.labels.skipped.Load() == 1 })
	s.labels.stop()
	l, err := s.core.LabelSettings(t0)
	if err != nil || l.LastError != "label backend unavailable on this gateway" {
		t.Fatal(l, err)
	}
	var n int
	_ = s.core.DB().QueryRow(`SELECT count(*) FROM usage_events WHERE device_id='gateway'`).Scan(&n)
	if n != 0 {
		t.Fatal("unavailable backend invoked")
	}
}

var _ decide.Backend = fakeLabelBackend

func TestLabelsBodyBudgetAndClientCannotClaimInternal(t *testing.T) {
	f := newCoreFx(t, nil)
	s := f.s
	started := make(chan struct{})
	s.labels.backend = func(ctx context.Context, system, user string) (string, error) {
		close(started)
		<-ctx.Done()
		return "", ctx.Err()
	}
	enableTestLabels(t, s, "")
	r := labelReq(s, "first", "", "")
	s.dispatched(r)
	<-started
	tooLarge := labelReq(s, "large", "", "")
	tooLarge.body = make([]byte, labelQueueBytes+1)
	s.dispatched(tooLarge)
	if len(s.labels.queue) != 0 || s.labels.skipped.Load() != 1 {
		t.Fatal("byte budget exceeded")
	}
	spoof := labelReq(s, "spoof", "", "")
	spoof.Header.Set("x-claude-mini-task", "task")
	spoof.Header.Set("x-claude-mini-task-source", "internal")
	s.admitEvent(spoof, "test")
	if spoof.event.TaskSource != core.LabelGiven {
		t.Fatal("client claimed internal source")
	}
	s.dispatched(spoof)
	if len(s.labels.queue) != 1 {
		t.Fatal("client bypassed classification")
	}
	for i := 0; i < 3; i++ {
		raw := labelReq(s, "not parsed at admission", "", "")
		raw.body = make([]byte, 3<<20) // malformed JSON: queueing must not parse it
		s.dispatched(raw)
	}
	if len(s.labels.queue) != 3 || s.labels.queueBytes != (6<<20)+len(spoof.body) || s.labels.skipped.Load() != 2 {
		t.Fatal("total queued byte budget", len(s.labels.queue), s.labels.queueBytes, s.labels.skipped.Load())
	}
	s.labels.stop()
	if s.labels.queueBytes != 0 {
		t.Fatal("queued bodies retained")
	}
}

func TestLabelsRestartAfterCoreReopen(t *testing.T) {
	f := newCoreFx(t, nil)
	s := f.s
	s.labels.backend = fakeLabelBackend
	enableTestLabels(t, s, "")
	classify := func(text string) {
		r := labelReq(s, text, "", "")
		s.dispatched(r)
		waitLabels(t, func() bool {
			var a string
			_ = s.core.DB().QueryRow(`SELECT activity FROM usage_events WHERE id=?`, r.event.ID).Scan(&a)
			return a == "bugfix"
		})
	}
	classify("before reopen")
	s.closeCore()
	s.openCore()
	if s.core == nil {
		t.Fatal(s.coreErr)
	}
	classify("after reopen")
}
