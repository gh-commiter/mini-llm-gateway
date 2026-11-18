package coreapi

import (
	"context"
	"testing"
	"time"
)

func TestLabelRecorderBarrierCancelsWhileQueueOrWriterBlocked(t *testing.T) {
	for _, full := range []bool{false, true} {
		t.Run(map[bool]string{false: "writer", true: "queue"}[full], func(t *testing.T) {
			api := New(Options{})
			api.rec = &Recorder{ch: make(chan recMsg, 1), stopped: make(chan struct{})}
			if full {
				api.rec.ch <- recMsg{}
			} // no reader: the queue stays full
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
			defer cancel()
			if api.FlushRecorderContext(ctx) {
				t.Fatal("barrier completed with a blocked recorder")
			}
		})
	}
	api := New(Options{})
	if !api.FlushRecorderContext(context.Background()) {
		t.Fatal("no recorder should need no barrier")
	}
}
