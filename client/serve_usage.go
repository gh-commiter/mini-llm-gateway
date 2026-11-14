package main

// Usage events of the gateway (contracts section 7): one event per model invocation, written without ever blocking or
// slowing the request. A request only enqueues; one writer goroutine stores events in the core and, when the core is
// not usable, appends them to a bounded spool file that is replayed later.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"minigw/core"
)

// Limits of the spool (variables for tests): at most this many events or bytes; the oldest are dropped.
var (
	spoolMaxEvents  = 10000
	spoolMaxBytes   = int64(10 << 20)
	spoolReplayEach = 30 * time.Second
	usageQueue      = 4096
)

const spoolName = "usage.spool"

type usageKind int

const (
	evBegin usageKind = iota
	evFinish
	evDiscard
	evFlush
)

type usageMsg struct {
	kind  usageKind
	ev    core.UsageEvent
	flush chan struct{}
}

type usageRecorder struct {
	s       *server
	ch      chan usageMsg
	quit    chan struct{}
	stopped chan struct{}
	spool   *usageSpool
	lostMu  sync.Mutex
	lost    int64
	lostA   time.Time
	lostB   time.Time
	dropped atomic.Int64
}

// startUsage starts the recorder (always, also when the core could not be opened: events then go to the spool).
func (s *server) startUsage() {
	u := &usageRecorder{s: s, ch: make(chan usageMsg, usageQueue), quit: make(chan struct{}), stopped: make(chan struct{}),
		spool: newSpool(filepath.Join(s.cfgDir, spoolName))}
	s.usage = u
	go u.run()
}

// stopUsage drains the queue and ends the writer (before the core is closed).
func (s *server) stopUsage() {
	if u := s.usage; u != nil {
		close(u.quit)
		<-u.stopped
	}
}

// enqueue never blocks: when the queue is full the event is counted as lost and shows up as a gap.
func (u *usageRecorder) enqueue(m usageMsg) {
	if u == nil {
		return
	}
	select {
	case u.ch <- m:
	default:
		u.lostMu.Lock()
		if u.lost == 0 {
			u.lostA = m.ev.Start
		}
		u.lost++
		u.lostB = time.Now()
		u.lostMu.Unlock()
	}
}

// flush waits until everything enqueued so far is processed (tests, shutdown).
func (u *usageRecorder) flush() {
	if u == nil {
		return
	}
	c := make(chan struct{})
	u.ch <- usageMsg{kind: evFlush, flush: c}
	<-c
}

func (u *usageRecorder) run() {
	defer close(u.stopped)
	tick := time.NewTicker(spoolReplayEach)
	defer tick.Stop()
	u.replay()
	for {
		select {
		case m := <-u.ch:
			u.handle(m)
		case <-tick.C:
			u.replay()
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

func (u *usageRecorder) handle(m usageMsg) {
	u.takeLost()
	st := u.s.core
	ctx := context.Background()
	switch m.kind {
	case evFlush:
		close(m.flush)
	case evBegin:
		if st != nil {
			st.BeginEvent(ctx, m.ev) // a failure is fine: the finish carries the whole event
		}
	case evDiscard:
		if st != nil {
			st.DiscardEvent(ctx, m.ev.ID)
		}
	case evFinish:
		if st != nil && u.spool.empty() && st.FinishEvent(ctx, m.ev) == nil {
			return
		}
		u.spool.add(m.ev) // the core is down, or older events wait in the spool: keep the order
		u.replay()
	}
}

// takeLost turns queue overflow into a gap (stored in the core, or in the spool).
func (u *usageRecorder) takeLost() {
	u.lostMu.Lock()
	n, a, b := u.lost, u.lostA, u.lostB
	u.lost = 0
	u.lostMu.Unlock()
	if n == 0 {
		return
	}
	g := core.UsageGapRecord{ID: "gap_" + randomToken()[:12], Count: n, From: a, To: b}
	if st := u.s.core; st == nil || st.ReplayUsage(context.Background(), nil, []core.UsageGapRecord{g}) != nil {
		u.spool.addGap(g)
	}
}

// replay moves the spool into the core; the file is cleared only after the core accepted everything, and a replay
// that is interrupted in between simply runs again (the core ignores what it already has).
func (u *usageRecorder) replay() {
	st := u.s.core
	if st == nil || u.spool.empty() {
		return
	}
	evs, gaps := u.spool.load()
	if err := st.ReplayUsage(context.Background(), evs, gaps); err != nil {
		return
	}
	if spoolAfterReplay != nil && spoolAfterReplay() { // tests: the process dies here
		return
	}
	u.spool.clear()
}

// spoolAfterReplay: test hook between the insert and the clearing of the file; true = stop (simulated crash).
var spoolAfterReplay func() bool

// --- the spool file: JSON lines, append only, private like the rest of the config folder ---

type spoolLine struct {
	T     string               `json:"t"` // event or gap
	Event *core.UsageEvent     `json:"e,omitempty"`
	Gap   *core.UsageGapRecord `json:"g,omitempty"`
}

type usageSpool struct {
	path   string
	events int
	size   int64
}

func newSpool(path string) *usageSpool {
	sp := &usageSpool{path: path}
	evs, _ := sp.load()
	sp.events = len(evs)
	if fi, err := os.Stat(path); err == nil {
		sp.size = fi.Size()
	}
	return sp
}

func (sp *usageSpool) empty() bool { return sp.events == 0 && sp.size == 0 }

func (sp *usageSpool) readLines() (out []spoolLine) {
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

func (sp *usageSpool) load() (evs []core.UsageEvent, gaps []core.UsageGapRecord) {
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

func (sp *usageSpool) appendLine(l spoolLine) {
	b, _ := json.Marshal(l)
	os.MkdirAll(filepath.Dir(sp.path), 0o700)
	f, err := os.OpenFile(sp.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	n, _ := f.Write(append(b, '\n'))
	f.Close()
	sp.size += int64(n)
}

func (sp *usageSpool) add(e core.UsageEvent) {
	sp.appendLine(spoolLine{T: "event", Event: &e})
	sp.events++
	if sp.events > spoolMaxEvents || sp.size > spoolMaxBytes {
		sp.compact()
	}
}

func (sp *usageSpool) addGap(g core.UsageGapRecord) { sp.appendLine(spoolLine{T: "gap", Gap: &g}) }

// compact drops the oldest tenth of the events (at least one), and writes one gap record for them: count and time span.
func (sp *usageSpool) compact() {
	lines := sp.readLines()
	var evIdx []int
	for i, l := range lines {
		if l.T == "event" {
			evIdx = append(evIdx, i)
		}
	}
	drop := max(1, len(evIdx)/10)
	for len(evIdx)-drop > spoolMaxEvents-1 { // also when the limit itself was lowered
		drop++
	}
	drop = min(drop, len(evIdx))
	if drop == 0 {
		return
	}
	from, to := lines[evIdx[0]].Event.Start, lines[evIdx[drop-1]].Event.Start
	cut := map[int]bool{}
	for _, i := range evIdx[:drop] {
		cut[i] = true
	}
	var b bytes.Buffer
	for i, l := range lines {
		if !cut[i] {
			x, _ := json.Marshal(l)
			b.Write(append(x, '\n'))
		}
	}
	g, _ := json.Marshal(spoolLine{T: "gap", Gap: &core.UsageGapRecord{ID: "gap_" + randomToken()[:12], Count: int64(drop), From: from, To: to}})
	b.Write(append(g, '\n'))
	if writeFileAtomic(sp.path, b.Bytes()) == nil {
		sp.events -= drop
		sp.size = int64(b.Len())
	}
}

func (sp *usageSpool) clear() {
	if writeFileAtomic(sp.path, nil) == nil {
		sp.events, sp.size = 0, 0
	}
}

// --- events of requests ---

// beginEvent: a request that names a model is admitted; attribution is frozen here.
func (s *server) beginEvent(r *req, model string) {
	if s.usage == nil || r.URL.Path != "/v1/messages" {
		return
	}
	e := core.UsageEvent{ID: "ev_" + randomToken()[:16], Start: time.Now(), Model: model, TokensIn: -1, TokensOut: -1, TokenSource: "unknown", Outcome: core.OutcomeOpen}
	if d := r.conn.device; d != nil {
		e.DeviceID = d.ID
		if s.core != nil {
			e.UserID = s.core.BoundUser(r.Context(), d.ID)
		}
	}
	if run := runOf(r); run != nil { // a nested call: the caller's event is the parent, the caller's user stays
		e.ParentID, e.UserID = run.evID, run.evUser
	}
	r.event = &e
	s.usage.enqueue(usageMsg{kind: evBegin, ev: e})
}

// refusal statuses: the request never became a model invocation (not counted).
func refusedStatus(st int) bool { return st == 400 || st == 401 || st == 403 || st == 404 || st == 413 }

// endEvent completes the request's event once the status is known.
func (s *server) endEvent(r *req, status int, uw *usageWriter) {
	e := r.event
	if e == nil {
		return
	}
	if refusedStatus(status) && r.Context().Err() == nil {
		s.usage.enqueue(usageMsg{kind: evDiscard, ev: *e})
		return
	}
	e.End = time.Now()
	e.Provider = r.provider
	if e.Provider == "" {
		if a := agentOfModel(e.Model); a != "" {
			e.Provider = a
		} else {
			e.Provider = "unknown"
		}
	}
	if r.project != nil {
		e.ProjectID = r.project.project
	}
	switch {
	case status == 499 || r.Context().Err() != nil:
		e.Outcome = core.OutcomeCancelled
	case status >= 400 || uw != nil && uw.errSeen: // 429 and 5xx, or an error event inside a stream that began with 200
		e.Outcome = core.OutcomeError
	default:
		e.Outcome = core.OutcomeOK
	}
	if uw != nil && (uw.in > 0 || uw.out > 0) {
		e.TokensIn, e.TokensOut = uw.in, uw.out
		e.TokenSource = "reported"
		if r.tokenSource != "" {
			e.TokenSource = r.tokenSource // estimated: Codex, Grok and Copilot give no counts, the gateway estimates the output
		}
		if e.TokensIn == 0 && e.TokenSource == "estimated" {
			e.TokensIn = -1
		}
	}
	s.usage.enqueue(usageMsg{kind: evFinish, ev: *e})
}

// usageErrSeen reports an SSE error event in a chunk of the stream.
func usageErrSeen(b []byte) bool { return strings.Contains(string(b), "event: error\n") }

// --- routes ---

const usagePath = "/minigw/v1/usage"

type usageRowJSON struct {
	Day           string `json:"day"`
	Model         string `json:"model"`
	User          int64  `json:"user,omitempty"`
	UserName      string `json:"userName,omitempty"`
	Project       int64  `json:"project,omitempty"`
	Events        int64  `json:"events"`
	TokensIn      int64  `json:"tokensIn"`
	TokensOut     int64  `json:"tokensOut"`
	UnknownTokens int64  `json:"eventsWithoutTokens"`
	Nested        int64  `json:"nestedEvents"`
	Failed        int64  `json:"failedEvents"`
}

type usageGapJSON struct {
	Count int64     `json:"count"`
	From  time.Time `json:"from"`
	To    time.Time `json:"to"`
}

func usageJSON(rep core.UsageReport) map[string]any {
	rows, gaps := []usageRowJSON{}, []usageGapJSON{}
	for _, r := range rep.Rows {
		rows = append(rows, usageRowJSON{r.Day, r.Model, r.UserID, r.UserName, r.ProjectID, r.Events, r.TokensIn, r.TokensOut, r.UnknownTokens, r.Nested, r.Failed})
	}
	for _, g := range rep.Gaps {
		gaps = append(gaps, usageGapJSON{g.Count, g.From, g.To})
	}
	return map[string]any{"days": rows, "gaps": gaps}
}

// usageRoute: GET /minigw/v1/usage?project=&days= (project: id or slug; days 1-400, default 7).
func (s *server) usageRoute(w http.ResponseWriter, r *req, p core.Principal) int {
	if r.Method != "GET" {
		return s.coreError(w, r, core.ErrNotFound)
	}
	q := r.URL.Query()
	days := 7
	if v := q.Get("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return s.coreError(w, r, core.ErrInvalid{What: "days"})
		}
		days = n
	}
	var project int64
	if v := q.Get("project"); v != "" {
		pr, err := s.core.GetProject(r.Context(), p, v) // not visible: 404, like the project itself
		if err != nil {
			return s.coreError(w, r, err)
		}
		project = pr.ID
	}
	rep, err := s.core.Usage(r.Context(), p, project, days)
	if err != nil {
		return s.coreError(w, r, err)
	}
	return writeJSON(w, r.rid, 200, usageJSON(rep))
}
