package main

// #4: odgovor s pretokom, ki se začne pred prenosom mape (sync). Takoj glava 200 (SSE) in komentarji »: keep-alive«
// (vsak razčlenjevalnik SSE jih prezre), da Claude Code med dolgim prvim prenosom ne prekine zahteve. Ko ponudnik začne
// pisati, se njegova glava ne pošlje več; napako (status ne-200 in JSON) pretvori v dogodek SSE »error«.

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

var earlyInterval = 10 * time.Second // testi: krajši

type earlyWriter struct {
	w       http.ResponseWriter
	hdr     http.Header
	mu      sync.Mutex
	started bool
	status  int
	stop    chan struct{}
}

func newEarlyWriter(w http.ResponseWriter, rid string) *earlyWriter {
	e := &earlyWriter{w: w, hdr: http.Header{}, status: 200, stop: make(chan struct{})}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("request-id", rid)
	w.WriteHeader(200)
	w.Write([]byte(": syncing the folder\n\n"))
	http.NewResponseController(w).Flush()
	every := earlyInterval
	go func() {
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-e.stop:
				return
			case <-t.C:
				e.mu.Lock()
				if !e.started {
					w.Write([]byte(": keep-alive\n\n"))
					http.NewResponseController(w).Flush()
				}
				e.mu.Unlock()
			}
		}
	}()
	return e
}

func (e *earlyWriter) Header() http.Header { return e.hdr }

func (e *earlyWriter) WriteHeader(code int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.status = code
}

func (e *earlyWriter) Write(b []byte) (int, error) {
	e.mu.Lock()
	if !e.started {
		e.started = true
		close(e.stop)
	}
	status := e.status
	e.mu.Unlock()
	if status == 200 {
		return e.w.Write(b)
	}
	var j struct {
		Error struct{ Type, Message string }
	}
	json.Unmarshal(b, &j)
	if j.Error.Type == "" {
		j.Error.Type = "api_error"
	}
	if j.Error.Message == "" {
		j.Error.Message = "request failed"
	}
	e.w.Write(sse("error", map[string]any{"type": "error", "error": map[string]string{"type": j.Error.Type, "message": j.Error.Message}}))
	return len(b), nil
}

func (e *earlyWriter) Flush() { http.NewResponseController(e.w).Flush() }

func (e *earlyWriter) Unwrap() http.ResponseWriter { return e.w }

// done: ustavi komentarje, če ponudnik ni napisal ničesar.
func (e *earlyWriter) done() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.started {
		e.started = true
		close(e.stop)
	}
}
