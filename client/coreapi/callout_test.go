package coreapi

import (
	"net"
	"testing"
	"time"
)

// A late answer to an earlier callout of the same request is dropped, never taken for the answer of the next one.
func TestALateCalloutAnswerIsNotTakenForTheNextOne(t *testing.T) {
	app, helper := net.Pipe()
	defer app.Close()
	h := &CalloutHost{}
	h.Attach(helper, true)
	h.Begin(7)
	go func() { // the app: answers callout 2 after a stale answer of callout 1
		var f wireCallout
		readFrame(app, &f) // callout 1 (the helper gave up on it; its answer comes late, with the next one)
		readFrame(app, &f) // callout 2
		WriteCalloutResult(app, 7, CalloutResult{Status: 999, Seq: 1})
		WriteCalloutResult(app, 7, CalloutResult{Status: 200, Seq: f.Callout.Seq})
	}()
	// callout 1: the app does not answer in time (the wait is short); that breaks nothing yet only if it ends cleanly, so use a second pipe end
	// by sending callout 1 manually
	if err := writeFrame(helper, wireCallout{V: ProtocolVersion, ID: 7, Callout: &CalloutRequest{Op: CalloutInfo, Upstream: "x", Seq: 1}}); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	h.seq = 1 // as if callout 1 had been made and abandoned
	h.mu.Unlock()
	r, err := h.call(CalloutRequest{Op: CalloutInfo, Upstream: "x"}, 5*time.Second)
	if err != nil || r.Status != 200 {
		t.Fatalf("%+v %v: the stale answer was used", r, err)
	}
}

// A callout that hits its deadline (it may be in the middle of a frame) ends the connection: nothing is continued out of step.
func TestACalloutDeadlineClosesTheConnection(t *testing.T) {
	app, helper := net.Pipe()
	defer app.Close()
	h := &CalloutHost{}
	h.Attach(helper, true)
	h.Begin(1)
	go func() {
		var f wireCallout
		readFrame(app, &f)
		app.Write([]byte{0, 0, 0, 40, '{'}) // the start of a frame that never ends
	}()
	_, err := h.call(CalloutRequest{Op: CalloutInfo, Upstream: "x"}, 300*time.Millisecond)
	if err == nil {
		t.Fatal("a half frame was accepted")
	}
	if h.Available() {
		t.Fatal("the host is still available after a broken callout")
	}
	if _, err := helper.Write([]byte{1}); err == nil {
		t.Fatal("the connection was not closed")
	}
	if _, err := h.call(CalloutRequest{Op: CalloutInfo, Upstream: "x"}, time.Second); err == nil {
		t.Fatal("a callout on a closed connection")
	}
}
