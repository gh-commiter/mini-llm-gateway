package main

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Failed time: the execution time runs from dispatch (not admission), a stream that breaks after HTTP 200 and an
// agent that failed behind a 200 are errors with a bounded kind, and the upstream's own status is kept.
func TestEventDurationAndFailureKinds(t *testing.T) {
	keys := fakeKeys(t)
	keys["gw"] = gwKey
	serve := func(h http.HandlerFunc) string {
		s := httptest.NewServer(h)
		t.Cleanup(s.Close)
		return s.URL
	}
	okURL := serve(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "event: message_stop\ndata: {}\n\n")
	})
	forbidden := serve(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		io.WriteString(w, `{"error":{"message":"spending limit"}}`)
	})
	broken := serve(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, "event: message_start\ndata: {}\n\n")
		w.(http.Flusher).Flush()
		conn, _, _ := w.(http.Hijacker).Hijack() // the connection dies in the middle of the chunked body
		conn.Close()
	})
	f := newCoreFx(t, nil)
	run := func(name, base, agentFail string) {
		ctx, stop := context.WithCancel(context.Background())
		defer stop()
		hr := httptest.NewRequest("POST", "/v1/messages", nil).WithContext(ctx)
		r := &req{Request: hr, conn: &serveConn{device: &serveDevice{ID: devID(f.dev["ana"])}}, rid: "r", body: []byte(`{"model":"m"}`), provider: "gw"}
		f.s.admitEvent(r, name)
		r.event.Start = time.Now().Add(-time.Hour) // admitted an hour ago: a queue is not execution time
		uw := &usageWriter{ResponseWriter: httptest.NewRecorder()}
		status := 200
		if agentFail == "" {
			status = f.s.passthrough(uw, r, serveGateway{Name: "gw", BaseURL: base, Insecure: true}, gwModel{id: "m"})
		} else { // an agent that exited non-zero or hit the timeout but answered 200
			f.s.dispatched(r)
			r.fail(agentFail)
		}
		f.s.endEvent(r, status, uw)
	}
	run("ok", okURL, "")
	run("403", forbidden, "")
	run("broken", broken, "")
	run("agent exit", "", "exit")
	run("agent timeout", "", "timeout")
	f.s.api.FlushRecorder()
	type row struct {
		outcome string
		dur     sql.NullInt64
		status  sql.NullInt64
		kind    sql.NullString
	}
	got := map[string]row{}
	rows, err := f.s.core.DB().Query(`SELECT model, outcome, duration_ms, upstream_status, error_kind FROM usage_events`)
	f.ok(err)
	for rows.Next() {
		var m string
		var x row
		f.ok(rows.Scan(&m, &x.outcome, &x.dur, &x.status, &x.kind))
		got[m] = x
	}
	want := map[string]struct {
		outcome, kind string
		status        int64
	}{"ok": {"ok", "", 200}, "403": {"error", "http", 403}, "broken": {"error", "stream", 200}, "agent exit": {"error", "exit", 0}, "agent timeout": {"error", "timeout", 0}}
	for name, w := range want {
		x := got[name]
		if x.outcome != w.outcome || x.kind.String != w.kind || x.status.Int64 != w.status || !x.dur.Valid || x.dur.Int64 >= 600000 {
			t.Errorf("%s: %+v, want %+v (the duration must not include the hour before dispatch)", name, x, w)
		}
	}
}

func TestAgentFailKindDeadlineFirst(t *testing.T) {
	msg := "claude failed (signal: killed); details are in the gateway log, request r"
	if got := agentFailKind(context.Background(), msg); got != "exit" {
		t.Fatal(got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()
	if got := agentFailKind(ctx, msg); got != "timeout" {
		t.Fatal(got)
	}
}
