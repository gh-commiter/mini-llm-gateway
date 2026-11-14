package coreapi

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"minigw/core"
)

// A full queue never blocks a request: the event is counted as lost and shows up as a gap.
func TestUsageQueueOverflowNeverBlocks(t *testing.T) {
	st, err := core.Open(t.TempDir(), core.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	api := New(Options{Store: func() *core.Store { return st }})
	u := &Recorder{api: api, ch: make(chan recMsg, 2), spool: newSpool(filepath.Join(t.TempDir(), SpoolName))} // no writer running
	done := make(chan struct{})
	go func() {
		for i := 0; i < 10; i++ {
			u.enqueue(recMsg{kind: evFinish, ev: core.UsageEvent{ID: fmt.Sprint(i), Start: time.Now()}})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("enqueue blocked")
	}
	if u.lost != 8 {
		t.Fatalf("lost %d", u.lost)
	}
	u.takeLost()
	rep, _ := st.AdminUsage(context.Background(), 7, core.UsageFilter{})
	if len(rep.Gaps) != 1 || rep.Gaps[0].Count != 8 {
		t.Fatalf("%+v", rep.Gaps)
	}
}

func TestSpoolCompactsAndKeepsTheNewest(t *testing.T) {
	old := SpoolMaxEvents
	SpoolMaxEvents = 10
	defer func() { SpoolMaxEvents = old }()
	sp := newSpool(filepath.Join(t.TempDir(), SpoolName))
	base := time.Now()
	for i := 0; i < 25; i++ {
		sp.add(core.UsageEvent{ID: fmt.Sprint("e", i), Start: base.Add(time.Duration(i) * time.Second)})
	}
	evs, gaps := sp.load()
	var lost int64
	for _, g := range gaps {
		lost += g.Count
	}
	if len(evs) > 10 || int64(len(evs))+lost != 25 || evs[len(evs)-1].ID != "e24" {
		t.Fatalf("%d events, %d lost, last %s", len(evs), lost, evs[len(evs)-1].ID)
	}
	for _, g := range gaps {
		if g.From.After(g.To) || g.Count < 1 {
			t.Fatalf("gap %+v", g)
		}
	}
	// the byte limit works the same way
	ob := SpoolMaxBytes
	SpoolMaxBytes = 600
	SpoolMaxEvents = 10000
	defer func() { SpoolMaxBytes = ob }()
	sp2 := newSpool(filepath.Join(t.TempDir(), SpoolName))
	for i := 0; i < 20; i++ {
		sp2.add(core.UsageEvent{ID: fmt.Sprint("b", i), Start: base})
	}
	if fi, _ := os.Stat(sp2.path); fi.Size() > 4*SpoolMaxBytes {
		t.Fatalf("spool grew to %d bytes", fi.Size())
	}
}

func bareRecorder(t *testing.T) (*Recorder, string) {
	p := filepath.Join(t.TempDir(), SpoolName)
	return &Recorder{api: New(Options{}), ch: make(chan recMsg, 4), spool: newSpool(p)}, p // no core: everything goes to the spool
}

func fin(id string) recMsg {
	return recMsg{kind: evFinish, ev: core.UsageEvent{ID: id, Start: time.Now(), End: time.Now(), Outcome: "ok", TokensIn: -1, TokensOut: -1, TokenSource: "unknown"}}
}

func TestSpoolDiskErrorIsACountedLossThatBecomesAGap(t *testing.T) {
	u, path := bareRecorder(t)
	SpoolWrite = func(*os.File, []byte) (int, error) { return 0, fmt.Errorf("disk full") }
	defer func() { SpoolWrite = func(f *os.File, b []byte) (int, error) { return f.Write(b) } }()
	for i := 0; i < 3; i++ {
		u.handle(fin(fmt.Sprint("e", i)))
	}
	if u.lost != 3 || u.spool.events != 0 {
		t.Fatalf("lost %d, spooled %d: a failed write must be a counted loss", u.lost, u.spool.events)
	}
	SpoolWrite = func(f *os.File, b []byte) (int, error) { return f.Write(b) } // the disk works again
	u.handle(fin("after"))
	evs, gaps := u.spool.load()
	if len(evs) != 1 || len(gaps) != 1 || gaps[0].Count != 3 || u.lost != 0 {
		t.Fatalf("events %d, gaps %+v, pending %d (file %s)", len(evs), gaps, u.lost, path)
	}
}

func TestSpoolPartialWriteLeavesNoBrokenLine(t *testing.T) {
	u, path := bareRecorder(t)
	u.handle(fin("one"))
	size := u.spool.size
	SpoolWrite = func(f *os.File, b []byte) (int, error) { return f.Write(b[:len(b)/2]) } // half a line, no error
	u.handle(fin("two"))
	SpoolWrite = func(f *os.File, b []byte) (int, error) { return f.Write(b) }
	if fi, _ := os.Stat(path); fi.Size() != size || u.lost != 1 {
		t.Fatalf("size %d (want %d), lost %d", fi.Size(), size, u.lost)
	}
	u.handle(fin("three"))
	evs, _ := u.spool.load()
	if len(evs) != 2 || evs[0].ID != "one" || evs[1].ID != "three" {
		t.Fatalf("%+v", evs)
	}
}

func TestSpoolFailedCutBackIsReportedAndNextLineStaysClean(t *testing.T) {
	u, _ := bareRecorder(t)
	u.handle(fin("one"))
	SpoolWrite = func(f *os.File, b []byte) (int, error) { return f.Write(b[:len(b)/2]) }
	SpoolTruncate = func(*os.File, int64) error { return fmt.Errorf("access denied") }
	u.handle(fin("two"))
	SpoolWrite = func(f *os.File, b []byte) (int, error) { return f.Write(b) }
	SpoolTruncate = func(f *os.File, n int64) error { return f.Truncate(n) }
	if u.lost != 1 {
		t.Fatalf("lost %d: a failed cut-back must be a counted loss", u.lost)
	}
	u.handle(fin("three"))
	evs, _ := u.spool.load()
	if len(evs) != 2 || evs[0].ID != "one" || evs[1].ID != "three" {
		t.Fatalf("%+v", evs)
	}
}

func TestSpoolGapRecordsAreBoundedToo(t *testing.T) {
	ob, oe := SpoolMaxBytes, SpoolMaxEvents
	SpoolMaxBytes, SpoolMaxEvents = 700, 10
	defer func() { SpoolMaxBytes, SpoolMaxEvents = ob, oe }()
	sp := newSpool(filepath.Join(t.TempDir(), SpoolName))
	at := time.Now()
	var total int64
	for i := 0; i < 200; i++ {
		if err := sp.addGap(core.UsageGapRecord{ID: fmt.Sprint("g", i), Count: 2, From: at, To: at.Add(time.Second)}); err != nil {
			t.Fatal(err)
		}
		total += 2
	}
	fi, _ := os.Stat(sp.path)
	_, gaps := sp.load()
	var sum int64
	for _, g := range gaps {
		sum += g.Count
	}
	if fi.Size() > 2*SpoolMaxBytes || sum != total { // bounded, and nothing lost from the count
		t.Fatalf("size %d (limit %d), gaps %d, counted %d of %d", fi.Size(), SpoolMaxBytes, len(gaps), sum, total)
	}
}

func TestQueueOverflowCountsLostEventsNotMessages(t *testing.T) {
	u := &Recorder{ch: make(chan recMsg, 1)}
	u.enqueue(recMsg{kind: evBegin})
	u.enqueue(recMsg{kind: evBegin, ev: core.UsageEvent{ID: "b1"}}) // lost begins: their finishes carry the events
	u.enqueue(recMsg{kind: evBegin, ev: core.UsageEvent{ID: "b2"}})
	if u.lost != 0 {
		t.Fatalf("lost begins counted as lost events: %d", u.lost)
	}
	u.enqueue(fin("f1")) // a lost finish is a lost event
	u.enqueue(fin("f2"))
	if u.lost != 2 {
		t.Fatalf("lost %d", u.lost)
	}
}
