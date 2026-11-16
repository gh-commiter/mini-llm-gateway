package coreapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"minigw/core"
)

// Usage events (contracts section 7) are written without ever blocking or slowing a request. A request only enqueues;
// one writer goroutine stores events in the core and, when the core is not usable, appends them to a bounded spool
// file that is replayed later.

func randToken() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// writeFileAtomic: a new 0600 file in the same folder, fsync, rename.
func writeFileAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// Limits of the spool (exported variables so that tests can lower them): at most this many events or bytes; the oldest
// are dropped.
var (
	SpoolMaxEvents  = 10000
	SpoolMaxBytes   = int64(10 << 20)
	SpoolReplayEach = 30 * time.Second
	FoldEvery       = 24 * time.Hour // retention: detail rows past 400 days become daily totals
	usageQueue      = 4096
)

// SpoolName is the spool file in Options.Dir.
const SpoolName = "usage.spool"

type usageKind int

const (
	evBegin usageKind = iota
	evFinish
	evFlush
	evQuota
	evQuotaGap
	evGap // a gap record of the adapter: stored durably before the sender is answered
)

type recMsg struct {
	probe core.QuotaProbe
	kind  usageKind
	ev    core.UsageEvent
	flush chan struct{}
	gap   core.UsageGapRecord
	done  chan error // evGap: nil once the gap is in the core or in the spool
}

type Recorder struct {
	api        *API
	ch         chan recMsg
	quit       chan struct{}
	stopped    chan struct{}
	spool      *spool
	quotaSpool *spool
	quotaLost  core.UsageGapRecord
	lostMu     sync.Mutex
	lost       int64
	lostA      time.Time
	lostB      time.Time
	dropped    atomic.Int64
	once       sync.Once
	bound      sync.Map // device id -> last user seen
}

// StartRecorder starts the usage recorder (always, also when the core is not usable: events then go to the spool).
func (a *API) StartRecorder() {
	u := &Recorder{api: a, ch: make(chan recMsg, usageQueue), quit: make(chan struct{}), stopped: make(chan struct{}),
		spool: newSpool(filepath.Join(a.o.Dir, SpoolName))}
	u.quotaSpool = newSpool(filepath.Join(a.o.Dir, "quota.spool"))
	a.rec = u
	go u.run()
}

// StopRecorder drains the queue and ends the writer (before the core is closed). Safe to call twice.
func (a *API) StopRecorder() {
	if u := a.rec; u != nil {
		u.once.Do(func() { close(u.quit) })
		<-u.stopped
	}
}

// FlushRecorder waits until everything enqueued so far is processed (tests, shutdown).
func (a *API) FlushRecorder() { a.rec.flush() }

// enqueue never blocks: when the queue is full a lost FINISH is counted as a lost event (a lost begin whose finish is
// stored loses nothing: the finish carries the whole event) and shows up as a gap.
func (u *Recorder) enqueue(m recMsg) {
	if u == nil {
		return
	}
	select {
	case u.ch <- m:
	default:
		if m.kind == evQuota {
			u.noteQuotaLoss(1, m.probe.ProbedAt, time.Now())
		}
		if m.kind == evFinish {
			u.noteLoss(1, m.ev.Start, time.Now())
		}
	}
}

// noteLoss keeps lost events in a small in-memory gap (count and time span) until it can be written.
func (u *Recorder) noteLoss(n int64, from, to time.Time) {
	u.lostMu.Lock()
	defer u.lostMu.Unlock()
	if u.lost == 0 || from.Before(u.lostA) {
		u.lostA = from
	}
	if to.After(u.lostB) {
		u.lostB = to
	}
	u.lost += n
}

// storeGap hands a gap record to the writer and waits until it is durable (in the core, or in the spool when the core is
// not usable); an error means it is not, and the sender must keep it and send it again with the same id.
func (u *Recorder) storeGap(g core.UsageGapRecord) error { return u.storeGapKind(false, g) }
func (u *Recorder) storeGapKind(quota bool, g core.UsageGapRecord) error {
	if u == nil {
		return errors.New("no usage recorder")
	}
	kind := evGap
	if quota {
		kind = evQuotaGap
	}
	m := recMsg{kind: kind, gap: g, done: make(chan error, 1)}
	select {
	case u.ch <- m:
	case <-u.stopped:
		return errors.New("the recorder has stopped")
	case <-time.After(5 * time.Second):
		return errors.New("the recorder queue is full")
	}
	select {
	case err := <-m.done:
		return err
	case <-time.After(15 * time.Second):
		return errors.New("the recorder did not answer")
	}
}

// kick makes the run loop store a pending gap now (every message does that first) instead of at the next tick; never blocks.
func (u *Recorder) kick() {
	select {
	case u.ch <- recMsg{kind: evFlush, flush: make(chan struct{})}:
	default: // the queue is full: the next message or the tick does it
	}
}

// flush waits until everything enqueued so far is processed (tests, shutdown).
func (u *Recorder) flush() {
	if u == nil {
		return
	}
	c := make(chan struct{})
	u.ch <- recMsg{kind: evFlush, flush: c}
	<-c
}

func (u *Recorder) run() {
	defer close(u.stopped)
	tick := time.NewTicker(SpoolReplayEach)
	defer tick.Stop()
	u.replay()
	lastFold := time.Now() // core.Open folded at start
	for {
		select {
		case m := <-u.ch:
			u.handle(m)
		case <-tick.C:
			u.takeLost() // a gap that arrived alone (event-lost after a restart of the app) is stored without waiting for an event
			u.replay()
			if st := u.api.store(); st != nil && time.Since(lastFold) >= FoldEvery { // a long-lived gateway folds daily
				if _, err := st.FoldOldUsage(context.Background()); err == nil {
					lastFold = time.Now()
				}
			}
		case <-u.quit:
			for {
				select {
				case m := <-u.ch:
					u.handle(m)
				default:
					return
				}
			}
		}
	}
}

func (u *Recorder) handle(m recMsg) {
	u.takeLost()
	st := u.api.store()
	ctx := context.Background()
	switch m.kind {
	case evFlush:
		close(m.flush)
	case evQuota:
		if st != nil && u.quotaSpool.empty() && st.ReplayQuota(ctx, []core.QuotaProbe{m.probe}, nil) == nil {
			return
		}
		if err := u.quotaSpool.addProbe(m.probe); err != nil {
			u.noteQuotaLoss(1, m.probe.ProbedAt, time.Now())
		}
		u.replay()
	case evQuotaGap:
		if st != nil && st.ReplayQuota(ctx, nil, []core.UsageGapRecord{m.gap}) == nil {
			m.done <- nil
			return
		}
		m.done <- u.quotaSpool.addGap(m.gap)
	case evGap:
		if st != nil && st.ReplayUsage(ctx, nil, []core.UsageGapRecord{m.gap}) == nil { // INSERT OR IGNORE by id: a repeat is harmless
			m.done <- nil
			return
		}
		m.done <- u.spool.addGap(m.gap) // the replay into the core is by id too
	case evBegin:
		if st != nil {
			st.BeginEvent(ctx, m.ev) // a failure is fine: the finish carries the whole event
		}
	case evFinish:
		if st != nil && u.spool.empty() && st.FinishEvent(ctx, m.ev) == nil {
			return
		}
		if err := u.spool.add(m.ev); err != nil { // the core is down and the disk does not take it: a counted loss, never silent
			u.noteLoss(1, m.ev.Start, m.ev.End)
		}
		u.replay()
	}
}

// takeLost turns the pending losses into a gap record (stored in the core, or in the spool). If neither works the
// loss stays pending in memory and is tried again with the next message or tick.
func (u *Recorder) takeLost() {
	u.lostMu.Lock()
	qg := u.quotaLost
	u.quotaLost = core.UsageGapRecord{}
	u.lostMu.Unlock()
	if qg.Count > 0 {
		qg.ID = "qgap_" + randToken()[:12]
		st := u.api.store()
		if st == nil || st.ReplayQuota(context.Background(), nil, []core.UsageGapRecord{qg}) != nil {
			if u.quotaSpool.addGap(qg) != nil {
				u.noteQuotaLoss(qg.Count, qg.From, qg.To)
			}
		}
	}

	u.lostMu.Lock()
	n, a, b := u.lost, u.lostA, u.lostB
	u.lost = 0
	u.lostMu.Unlock()
	if n == 0 {
		return
	}
	g := core.UsageGapRecord{ID: "gap_" + randToken()[:12], Count: n, From: a, To: b}
	if st := u.api.store(); st != nil && st.ReplayUsage(context.Background(), nil, []core.UsageGapRecord{g}) == nil {
		return
	}
	if u.spool.addGap(g) != nil {
		u.noteLoss(n, a, b)
	}
}

// replay moves the spool into the core; the file is cleared only after the core accepted everything, and a replay
// that is interrupted in between simply runs again (the core ignores what it already has).
func (u *Recorder) replay() {
	st := u.api.store()
	if st != nil && u.quotaSpool != nil && !u.quotaSpool.empty() && u.quotaSpool.repairProbes() == nil {
		probes, gaps := u.quotaSpool.loadProbes()
		if st.ReplayQuota(context.Background(), probes, gaps) == nil {
			if SpoolAfterReplay == nil || !SpoolAfterReplay() {
				u.quotaSpool.clear()
			}
		}
	}
	if st == nil || u.spool.empty() {
		return
	}
	evs, gaps := u.spool.load()
	if err := st.ReplayUsage(context.Background(), evs, gaps); err != nil {
		return
	}
	if SpoolAfterReplay != nil && SpoolAfterReplay() { // tests: the process dies here
		return
	}
	u.spool.clear()
}

// SpoolAfterReplay: test hook between the insert and the clearing of the file; true = stop (simulated crash).
var SpoolAfterReplay func() bool

// --- the spool file: JSON lines, append only, private like the rest of the config folder ---

type spoolLine struct {
	Probe *core.QuotaProbe     `json:"p,omitempty"`
	T     string               `json:"t"` // event or gap
	Event *core.UsageEvent     `json:"e,omitempty"`
	Gap   *core.UsageGapRecord `json:"g,omitempty"`
}

type spool struct {
	path   string
	events int
	size   int64
	torn   bool // a failed cut-back left half a line: the next line starts after a newline
}

func newSpool(path string) *spool {
	sp := &spool{path: path}
	evs, _ := sp.load()
	probes, _ := sp.loadProbes()
	sp.events = len(evs) + len(probes)
	if fi, err := os.Stat(path); err == nil {
		sp.size = fi.Size()
	}
	return sp
}

func (sp *spool) empty() bool { return sp.events == 0 && sp.size == 0 }

func (sp *spool) readLines() (out []spoolLine) {
	b, err := os.ReadFile(sp.path)
	if err != nil {
		return nil
	}
	for _, l := range bytes.Split(b, []byte("\n")) {
		var x spoolLine
		if len(l) > 0 && json.Unmarshal(l, &x) == nil {
			out = append(out, x)
		}
	}
	return out
}

func (sp *spool) load() (evs []core.UsageEvent, gaps []core.UsageGapRecord) {
	for _, l := range sp.readLines() {
		switch {
		case l.T == "event" && l.Event != nil:
			evs = append(evs, *l.Event)
		case l.T == "gap" && l.Gap != nil:
			gaps = append(gaps, *l.Gap)
		}
	}
	return
}

// appendLine appends one line; it reports every failure. A short write is cut back so that the next line starts clean.
func (sp *spool) appendLine(l spoolLine) error {
	b, _ := json.Marshal(l)
	b = append(b, '\n')
	if sp.torn {
		b = append([]byte{'\n'}, b...)
	}
	if err := os.MkdirAll(filepath.Dir(sp.path), 0o700); err != nil {
		return err
	}
	// No O_APPEND: on Windows Go then opens with FILE_APPEND_DATA only, and Truncate (the cut-back below) is denied,
	// which left half a line in the file. The recorder goroutine is the only writer, so seeking to the end is the same.
	f, err := os.OpenFile(sp.path, os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekEnd); err != nil {
		f.Close()
		return err
	}
	n, werr := SpoolWrite(f, b)
	if werr != nil || n < len(b) {
		if werr == nil {
			werr = io.ErrShortWrite
		}
		if terr := SpoolTruncate(f, sp.size); terr != nil { // the half line stays: count it in the size, end it with a newline next time
			sp.size += int64(n)
			sp.torn = true
			werr = fmt.Errorf("%w; cut-back failed: %v", werr, terr)
		}
		f.Close()
		return werr
	}
	if err := f.Close(); err != nil {
		return err
	}
	sp.size += int64(n)
	sp.torn = false
	return nil
}

// SpoolTruncate: tests make the cut-back fail.
var SpoolTruncate = func(f *os.File, size int64) error { return f.Truncate(size) }

// SpoolWrite: tests make the disk fail.
var SpoolWrite = func(f *os.File, b []byte) (int, error) { return f.Write(b) }

func (sp *spool) add(e core.UsageEvent) error {
	if err := sp.appendLine(spoolLine{T: "event", Event: &e}); err != nil {
		return err
	}
	sp.events++
	sp.bound()
	return nil
}

func (sp *spool) addGap(g core.UsageGapRecord) error {
	if err := sp.appendLine(spoolLine{T: "gap", Gap: &g}); err != nil {
		return err
	}
	sp.bound()
	return nil
}

// bound enforces both limits after every append, gap records included.
func (sp *spool) bound() {
	if sp.events > SpoolMaxEvents || sp.size > SpoolMaxBytes {
		sp.compact()
	}
}

// compact drops the oldest events until the spool is at 90% of both limits, and writes one gap record for them
// (count and time span).
func (sp *spool) compact() {
	lines := sp.readLines()
	enc := make([][]byte, len(lines))
	var evIdx []int
	var total int64
	for i, l := range lines {
		enc[i], _ = json.Marshal(l)
		total += int64(len(enc[i]) + 1)
		if l.T == "event" || l.T == "probe" {
			evIdx = append(evIdx, i)
		}
	}
	keepEvents, keepBytes := max(1, SpoolMaxEvents*9/10), SpoolMaxBytes*9/10
	drop, left := 0, len(evIdx)
	for drop < len(evIdx) && (left > keepEvents || total > keepBytes || drop == 0 && sp.events > SpoolMaxEvents) {
		total -= int64(len(enc[evIdx[drop]]) + 1)
		drop++
		left--
	}
	gapLines := 0
	for _, l := range lines {
		if l.T == "gap" {
			gapLines++
		}
	}
	if drop == 0 && gapLines < 2 {
		return
	}
	merged := core.UsageGapRecord{ID: "gap_" + randToken()[:12], Count: int64(drop)}
	cut := map[int]bool{}
	for _, i := range evIdx[:drop] {
		cut[i] = true
		st := time.Time{}
		if lines[i].Event != nil {
			st = lines[i].Event.Start
		} else if lines[i].Probe != nil {
			st = lines[i].Probe.ProbedAt
		}
		if merged.From.IsZero() || st.Before(merged.From) {
			merged.From = st
		}
		if st.After(merged.To) {
			merged.To = st
		}
	}
	for i, l := range lines { // earlier gaps of this spool fold into the new one: one record, never a growing list
		if l.T == "gap" && l.Gap != nil {
			cut[i] = true
			merged.Count += l.Gap.Count
			if merged.From.IsZero() || l.Gap.From.Before(merged.From) {
				merged.From = l.Gap.From
			}
			if l.Gap.To.After(merged.To) {
				merged.To = l.Gap.To
			}
		}
	}
	var b bytes.Buffer
	for i := range lines {
		if !cut[i] {
			b.Write(append(enc[i], '\n'))
		}
	}
	g, _ := json.Marshal(spoolLine{T: "gap", Gap: &merged})
	b.Write(append(g, '\n'))
	if writeFileAtomic(sp.path, b.Bytes()) == nil {
		sp.events -= drop
		sp.size = int64(b.Len())
	}
}

func (sp *spool) clear() {
	if writeFileAtomic(sp.path, nil) == nil {
		sp.events, sp.size = 0, 0
	}
}

// Quota uses the same bounded spool mechanics, in a separate file and gap table.
// Replace a parseable but invalid probe with a durable gap before replaying it.
func (sp *spool) repairProbes() error {
	lines := sp.readLines()
	changed := false
	for i, l := range lines {
		if l.T != "probe" || l.Probe != nil && l.Probe.Valid() {
			continue
		}
		at := time.Now().UTC()
		if l.Probe != nil && !l.Probe.ProbedAt.IsZero() {
			at = l.Probe.ProbedAt
		}
		lines[i] = spoolLine{T: "gap", Gap: &core.UsageGapRecord{ID: "qgap_" + randToken()[:12], Count: 1, From: at, To: at}}
		changed = true
	}
	if !changed {
		return nil
	}
	var b bytes.Buffer
	for _, l := range lines {
		data, err := json.Marshal(l)
		if err != nil {
			return err
		}
		b.Write(append(data, '\n'))
	}
	if err := writeFileAtomic(sp.path, b.Bytes()); err != nil {
		return err
	}
	sp.events = 0
	for _, l := range lines {
		if l.T == "probe" {
			sp.events++
		}
	}
	sp.size, sp.torn = int64(b.Len()), false
	return nil
}

func (sp *spool) loadProbes() (probes []core.QuotaProbe, gaps []core.UsageGapRecord) {
	for _, l := range sp.readLines() {
		if l.T == "probe" && l.Probe != nil {
			probes = append(probes, *l.Probe)
		}
		if l.T == "gap" && l.Gap != nil {
			gaps = append(gaps, *l.Gap)
		}
	}
	return
}
func (sp *spool) addProbe(p core.QuotaProbe) error {
	if err := sp.appendLine(spoolLine{T: "probe", Probe: &p}); err != nil {
		return err
	}
	sp.events++
	sp.bound()
	return nil
}
func (u *Recorder) noteQuotaLoss(n int64, from, to time.Time) {
	u.lostMu.Lock()
	defer u.lostMu.Unlock()
	g := &u.quotaLost
	if g.Count == 0 || from.Before(g.From) {
		g.From = from
	}
	if to.After(g.To) {
		g.To = to
	}
	g.Count += n
}
