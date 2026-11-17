package main

// The share long poll must not spin when what answers on the address is not the gateway: an
// unusable answer waits, with a growing delay, and a usable one resets it.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestShareWaitGrowsAndCaps(t *testing.T) {
	var delay time.Duration
	var got []time.Duration
	for i := 0; i < 8; i++ {
		var wait time.Duration
		wait, delay = shareWait(false, delay)
		got = append(got, wait)
	}
	want := []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second, 4 * time.Second,
		8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("delays %v, want %v", got, want)
	}
	if wait, next := shareWait(true, delay); wait != 0 || next != 0 {
		t.Fatalf("a usable answer waited %v, next %v", wait, next)
	}
}

// recorder: records the waits of the work goroutine and ends it after stopAt of them.
type recorder struct {
	mu     sync.Mutex
	waits  []time.Duration
	stopAt int
	done   chan struct{}
}

func (r *recorder) sleep(d time.Duration) {
	r.mu.Lock()
	r.waits = append(r.waits, d)
	stop := len(r.waits) >= r.stopAt
	r.mu.Unlock()
	if stop {
		close(r.done)
		runtime.Goexit() // ends only the goroutine that called the injected sleep
	}
}

func (r *recorder) recorded() []time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Duration(nil), r.waits...)
}

func runWork(t *testing.T, s *shareClient, rec *recorder) {
	t.Helper()
	go s.work()
	select {
	case <-rec.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the long poll did not wait")
	}
}

func TestShareWorkBacksOffOnUnusable200(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("content-type", "application/json")
		w.Write([]byte(`{"ok":true}`)) // not a job
	}))
	defer srv.Close()
	rec := &recorder{stopAt: 3, done: make(chan struct{})}
	s := &shareClient{c: connection{URL: srv.URL}, sleepFn: rec.sleep}
	runWork(t, s, rec)
	if got, want := rec.recorded(), []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second}; !reflect.DeepEqual(got, want) {
		t.Fatalf("delays %v, want %v", got, want)
	}
	if n := requests.Load(); n != 3 {
		t.Fatalf("%d requests, want 3 (one per wait)", n)
	}
}

func TestShareWorkBacksOffOnInstant204(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(204) // faster than any long poll
	}))
	defer srv.Close()
	rec := &recorder{stopAt: 3, done: make(chan struct{})}
	s := &shareClient{c: connection{URL: srv.URL}, sleepFn: rec.sleep}
	runWork(t, s, rec)
	if got, want := rec.recorded(), []time.Duration{500 * time.Millisecond, time.Second, 2 * time.Second}; !reflect.DeepEqual(got, want) {
		t.Fatalf("delays %v, want %v", got, want)
	}
	if n := requests.Load(); n != 3 {
		t.Fatalf("%d requests, want 3 (one per wait)", n)
	}
}

func TestShareWorkHandlesJobAndResetsBackoff(t *testing.T) {
	var mu sync.Mutex
	var calls []string
	var replies []davReply
	next := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		mu.Unlock()
		if r.URL.Path == "/minigw/share/next" {
			mu.Lock()
			first := next == 0
			next++
			mu.Unlock()
			if first {
				w.Header().Set("content-type", "application/json")
				w.Write([]byte(`{"id":"j1","method":"WORKTREE-NOTE"}`))
				return
			}
			w.WriteHeader(204) // instant: the job reset the growth, so this waits 0.5 s
			return
		}
		var reply davReply
		if err := json.NewDecoder(r.Body).Decode(&reply); err == nil {
			mu.Lock()
			replies = append(replies, reply)
			mu.Unlock()
		}
		w.WriteHeader(204)
	}))
	defer srv.Close()
	rec := &recorder{stopAt: 1, done: make(chan struct{})}
	s := &shareClient{c: connection{URL: srv.URL}, root: t.TempDir(), sleepFn: rec.sleep}
	runWork(t, s, rec)
	mu.Lock()
	defer mu.Unlock()
	if len(replies) != 1 || replies[0].ID != "j1" || replies[0].Response.Status != 200 {
		t.Fatalf("replies %+v", replies)
	}
	if got, want := calls[:2], []string{"GET /minigw/share/next", "POST /minigw/share/reply"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("calls %v, want %v", calls, want)
	}
	if got := rec.recorded(); !reflect.DeepEqual(got, []time.Duration{500 * time.Millisecond}) {
		t.Fatalf("delays %v, want 0.5 s (reset by the job)", got)
	}
}
