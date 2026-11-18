package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestReviewDiscoveryDeadline(t *testing.T) {
	s := newCoreFx(t, nil).s
	s.foundry.discover.Lock()
	released := make(chan struct{})
	go func() { time.Sleep(250 * time.Millisecond); s.foundry.discover.Unlock(); close(released) }()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := s.foundryEndpoint(ctx)
	elapsed := time.Since(start)
	<-released
	if err == nil || elapsed > 150*time.Millisecond {
		t.Fatalf("discovery ignored deadline: %v %v", elapsed, err)
	}
}

func TestReviewEnqueueAfterHTTPDispatch(t *testing.T) {
	keys := fakeKeys(t)
	keys["test"] = gwKey
	s := newCoreFx(t, nil).s
	var dispatched atomic.Bool
	s.labels.backend = func(ctx context.Context, system, user string) (string, error) {
		if !dispatched.Load() {
			t.Error("classification began before upstream dispatch")
		}
		return fakeLabelBackend(ctx, system, user)
	}
	enableTestLabels(t, s, "")
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dispatched.Store(true)
		io.WriteString(w, `{"content":[]}`)
	}))
	defer up.Close()
	r := labelReq(s, "dispatch", "", "")
	s.passthrough(httptest.NewRecorder(), r, serveGateway{Name: "test", BaseURL: up.URL, Insecure: true}, gwModel{id: "test"})
	waitLabels(t, func() bool {
		var n int
		_ = s.core.DB().QueryRow(`SELECT count(*) FROM usage_events WHERE device_id='gateway'`).Scan(&n)
		return n == 1
	})
}

func TestReviewHandoffOwnsBoundedInput(t *testing.T) {
	s := newCoreFx(t, nil).s
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	s.labels.backend = func(ctx context.Context, system, user string) (string, error) {
		if calls.Add(1) == 1 {
			close(started)
			select {
			case <-release:
			case <-ctx.Done():
				return "", ctx.Err()
			}
		}
		if strings.Contains(user, "MUTATED") {
			t.Error("handoff retained mutable request body")
		}
		return fakeLabelBackend(ctx, system, user)
	}
	enableTestLabels(t, s, "")
	s.dispatched(labelReq(s, "block", "", ""))
	<-started
	r := labelReq(s, "ORIGINAL", "", "")
	s.dispatched(r)
	replacement := strings.ReplaceAll(string(r.body), "ORIGINAL", "MUTATED!")
	copy(r.body, replacement)
	close(release)
	waitLabels(t, func() bool { return calls.Load() == 12 })
	s.labels.stop()
}

func TestReviewSettingsAtRecorderResume(t *testing.T) {
	for _, shadow := range []bool{false, true} {
		for _, patch := range []string{`{"enabled":false}`, `{"threshold":"1.0"}`} {
			t.Run(patch+map[bool]string{true: "shadow", false: "labels"}[shadow], func(t *testing.T) {
				s := newCoreFx(t, nil).s
				reached, release := make(chan struct{}), make(chan struct{})
				s.labels.flush = func(ctx context.Context) bool {
					close(reached)
					select {
					case <-release:
						return s.api.FlushRecorderContext(ctx)
					case <-ctx.Done():
						return false
					}
				}
				s.labels.backend = fakeLabelBackend
				extra := `,"threshold":"0.67"`
				if shadow {
					extra += `,"shadow":true,"shadowRate":1`
				}
				enableTestLabels(t, s, extra)
				p, a := "", ""
				if shadow {
					p, a = "do", "bugfix"
				}
				r := labelReq(s, "stale", p, a)
				s.dispatched(r)
				<-reached
				status, b := s.adminDo("PUT", "/usage/labels/settings", []byte(patch))
				if status != 200 {
					t.Fatal(status, b)
				}
				close(release)
				s.labels.queue <- labelJob{generation: ^uint64(0)}
				waitLabels(t, func() bool { return s.labels.skipped.Load() == 1 })
				s.labels.stop()
				if shadow {
					rows, err := s.core.Agreement(t0, 30)
					if err != nil || len(rows) != 0 {
						t.Fatal(rows, err)
					}
				} else {
					if p, a := labelsOf(t, s, r.event.ID); p != "" || a != "" {
						t.Fatal(p, a)
					}
				}
			})
		}
	}
}

func reviewFoundryServer(t *testing.T, s *server, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(handler)
	t.Cleanup(up.Close)
	s.foundry.mu.Lock()
	s.foundry.base = up.URL
	s.foundry.checked = time.Now()
	s.foundry.models = []foundryModel{{Name: "foreground"}}
	s.foundry.modelsAt = time.Now()
	s.foundry.mu.Unlock()
	return up
}

func TestReviewForegroundPreemptsSampleAndRetryBound(t *testing.T) {
	exe, _ := os.Executable()
	s := newCoreFx(t, func(c *serveConfig) { c.Foundry = foundryConfig{Enabled: true, Executable: exe} }).s
	started := make(chan struct{}, 2)
	cancelled := make(chan struct{}, 2)
	release := make(chan struct{})
	var samples, foregroundLoads atomic.Int32
	up := reviewFoundryServer(t, s, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/openai/load/") {
			if r.URL.Path == "/openai/load/foreground" {
				foregroundLoads.Add(1)
			}
			return
		}
		var b struct{ Model string }
		_ = json.NewDecoder(r.Body).Decode(&b)
		if b.Model == "test" {
			samples.Add(1)
			started <- struct{}{}
			// Runtime intentionally ignores cancellation and stays blocked.
			<-release
			io.WriteString(w, `{"choices":[{"message":{"content":"do"}}]}`)
			return
		}
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	})
	defer close(release)
	s.foundry.loadedAt = map[string]time.Time{"foreground": time.Now()}
	s.labels.backend = func(ctx context.Context, system, user string) (string, error) {
		reply, err := foundryLabelCompletion(ctx, up.URL, "test", system, user)
		if ctx.Err() != nil {
			cancelled <- struct{}{}
		}
		return reply, err
	}
	enableTestLabels(t, s, "")
	root := labelReq(s, "background", "", "")
	s.dispatched(root)
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("sample not started")
		}
		r := labelReq(s, "foreground", "do", "docs")
		r.provider = "foundry"
		done := make(chan int, 1)
		go func() {
			done <- s.runFoundry(httptest.NewRecorder(), r, map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}}, "foreground")
		}()
		select {
		case status := <-done:
			if status != 200 {
				t.Fatal(status)
			}
		case <-time.After(time.Second):
			t.Fatal("foreground delayed by blocked sample")
		}
		select {
		case <-cancelled:
		case <-time.After(time.Second):
			t.Fatal("sample was not cancelled")
		}
	}
	waitLabels(t, func() bool { return s.labels.skipped.Load() == 1 })
	s.labels.stop()
	if samples.Load() != 2 {
		t.Fatal("retry limit", samples.Load())
	}
	if foregroundLoads.Load() != 2 {
		t.Fatal("foreground reused a potentially evicted model", foregroundLoads.Load())
	}
	s.api.FlushRecorder()
	if p, a := labelsOf(t, s, root.event.ID); p != "" || a != "" {
		t.Fatal(p, a)
	}
}

func TestReviewNoSamplesWhileForegroundRuns(t *testing.T) {
	exe, _ := os.Executable()
	s := newCoreFx(t, func(c *serveConfig) { c.Foundry = foundryConfig{Enabled: true, Executable: exe} }).s
	entered, release := make(chan struct{}), make(chan struct{})
	reviewFoundryServer(t, s, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/openai/load/") {
			return
		}
		close(entered)
		<-release
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}]}`)
	})
	var calls atomic.Int32
	s.labels.backend = func(ctx context.Context, system, user string) (string, error) {
		calls.Add(1)
		return fakeLabelBackend(ctx, system, user)
	}
	enableTestLabels(t, s, "")
	r := labelReq(s, "foreground", "do", "docs")
	r.provider = "foundry"
	done := make(chan int, 1)
	go func() {
		done <- s.runFoundry(httptest.NewRecorder(), r, map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}}, "foreground")
	}()
	<-entered
	s.dispatched(labelReq(s, "queued", "", ""))
	time.Sleep(50 * time.Millisecond)
	if calls.Load() != 0 {
		t.Error("sample started during foreground work")
	}
	close(release)
	if status := <-done; status != 200 {
		t.Fatal(status)
	}
	waitLabels(t, func() bool { return calls.Load() == 6 })
	s.labels.stop()
}

func TestReviewDiscoveryShutdown(t *testing.T) {
	s := newCoreFx(t, nil).s
	s.foundry.discover.Lock()
	defer s.foundry.discover.Unlock()
	entered := make(chan struct{})
	s.labels.backend = func(ctx context.Context, _, _ string) (string, error) { close(entered); return s.foundryEndpoint(ctx) }
	enableTestLabels(t, s, "")
	s.dispatched(labelReq(s, "discovery", "", ""))
	<-entered
	done := make(chan struct{})
	go func() { s.labels.stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("shutdown waited on discovery")
	}
}

func TestReviewSnapshotBound(t *testing.T) {
	s := newCoreFx(t, nil).s
	s.labels.backend = fakeLabelBackend
	enableTestLabels(t, s, "")
	// Keep the queue under test control.
	s.labels.mu.Lock()
	s.labels.queue = make(chan labelJob, 8)
	s.labels.mu.Unlock()
	r := labelReq(s, strings.Repeat("🧪", 10000), "", "")
	s.dispatched(r)
	job := <-s.labels.queue
	if len(job.input) == 0 || len(job.input) > 8000 {
		t.Fatal("unbounded retained input")
	}
}
