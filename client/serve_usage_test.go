package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"minigw/coreapi"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"minigw/core"
)

type evRow struct {
	ID, Parent, Provider, Model, Outcome, Source string
	User, Project                                int64
	In, Out                                      int64 // -1 = NULL
}

// events flushes the recorder and returns the usage rows of the core, oldest first.
func (f *coreFx) events() []evRow {
	f.t.Helper()
	f.s.api.FlushRecorder()
	rows, err := f.s.core.DB().Query(`SELECT id, COALESCE(parent_id,''), provider, model, outcome, token_source, COALESCE(user_id,0), COALESCE(project_id,0),
COALESCE(tokens_in,-1), COALESCE(tokens_out,-1) FROM usage_events ORDER BY start, id`)
	f.ok(err)
	defer rows.Close()
	var out []evRow
	for rows.Next() {
		var r evRow
		f.ok(rows.Scan(&r.ID, &r.Parent, &r.Provider, &r.Model, &r.Outcome, &r.Source, &r.User, &r.Project, &r.In, &r.Out))
		out = append(out, r)
	}
	return out
}

// --- case 19: frozen attribution, nesting, who sees what ---

func TestUsageEventsOfRunsAndWhoSeesThem(t *testing.T) {
	x := newRunFx(t, true, true) // claude asks codex
	f := x.f
	f.project2("side", "ana", core.Developer)
	codexPlan := x.plan + ".codex"
	os.WriteFile(codexPlan, []byte("[]"), 0o644)
	x.setPlan(runStep{Path: "/v1/messages", Method: "POST",
		Body: `{"model":"claude-codex","max_tokens":10,"tools":[{"name":"x"}],"messages":[{"role":"user","content":"hi"}]}`})
	dir := folder(t, t.TempDir(), "w")
	if st, body := x.agent("ana", dir, "sid-u", f.pid["fakture"], "go"); st != 200 {
		t.Fatalf("%d %s", st, body)
	}
	ev := f.events()
	if len(ev) != 2 {
		t.Fatalf("%d events: %+v", len(ev), ev)
	}
	var parent, child evRow
	for _, e := range ev {
		if e.Parent == "" {
			parent = e
		} else {
			child = e
		}
	}
	if parent.Model != "claude-cli-haiku" || parent.Provider != "claude" || parent.User != f.uid["ana"] || parent.Project != f.pid["fakture"] ||
		parent.Outcome != "ok" || parent.Source != "reported" || parent.Out != 3 {
		t.Fatalf("parent: %+v", parent)
	}
	if child.Parent != parent.ID || child.Model != "claude-codex" || child.Provider != "codex" || child.User != f.uid["ana"] || child.Project != f.pid["fakture"] ||
		child.Source != "estimated" || child.In != -1 || child.Out != 3 { // codex answer: 12 bytes / 4
		t.Fatalf("child: %+v", child)
	}
	// totals: every event once; the nested one is inside them and counted as nested
	rep, err := f.s.core.AdminUsage(t0, 7, core.UsageFilter{})
	f.ok(err)
	var events, nested, out int64
	for _, r := range rep.Rows {
		events, nested, out = events+r.Events, nested+r.Nested, out+r.TokensOut
	}
	if events != 2 || nested != 1 || out != 6 {
		t.Fatalf("events %d nested %d out %d", events, nested, out)
	}
	// frozen attribution: the device is rebound to bor; the old event stays ana's, the next one is bor's
	f.ok(f.s.core.SetBinding(t0, devID(f.dev["ana"]), f.uid["bor"]))
	x.setPlan()
	if st, body := x.agent("ana", dir, "sid-u2", 0, "go"); st != 200 {
		t.Fatalf("%d %s", st, body)
	}
	byID := map[string]evRow{}
	for _, e := range f.events() {
		byID[e.ID] = e
	}
	if byID[parent.ID].User != f.uid["ana"] || len(byID) != 3 {
		t.Fatalf("old event remapped: %+v", byID)
	}
	var third evRow
	for id, e := range byID {
		if id != parent.ID && id != child.ID {
			third = e
		}
	}
	if third.User != f.uid["bor"] || third.Project != 0 {
		t.Fatalf("after the rebinding: %+v", third)
	}
	f.ok(f.s.core.SetBinding(t0, devID(f.dev["ana"]), f.uid["ana"]))

	// who sees what, through the member route (case 19, matrix)
	use := func(user, path string) resp { return f.call(user, "GET", "/minigw/v1/usage"+path, "") }
	if r := use("cene", "?project=fakture"); r.status != 404 { // a grant gives no usage: the resource is not visible
		t.Fatalf("cene: %d %s", r.status, r.body)
	}
	if r := use("eva", "?project=fakture"); r.status != 404 { // a reader neither
		t.Fatalf("eva: %d", r.status)
	}
	rr := use("ana", "?project=fakture")
	status(t, rr, 200)
	var ana struct{ Days []struct{ Events int64 } }
	json.Unmarshal([]byte(rr.body), &ana)
	if sum := sumEvents(ana.Days); sum != 2 { // ana's own two events in the project
		t.Fatalf("ana sees %d events: %s", sum, rr.body)
	}
	rf := use("fani", "?project=fakture")
	status(t, rf, 200)
	var fani struct{ Days []struct{ Events int64 } }
	json.Unmarshal([]byte(rf.body), &fani)
	if sum := sumEvents(fani.Days); sum != 2 { // the admin sees all events of the project
		t.Fatalf("fani sees %d events: %s", sum, rf.body)
	}
	if r := use("bor", "?project=fakture"); r.status != 200 || strings.Contains(r.body, `"events":2`) { // bor: developer, none of his own in fakture
		t.Fatalf("bor: %d %s", r.status, r.body)
	}
	status(t, use("ana", ""), 200)
	if r := use("ana", "?days=0"); r.status != 400 {
		t.Fatalf("days=0: %d", r.status)
	}
	if r := use("ana", "?days=x"); r.status != 400 {
		t.Fatalf("days=x: %d", r.status)
	}
	// the owner's route lists everything; today's /usage (folder counters) is a different route and untouched
	st, obj := f.s.adminDo("GET", "/usage/events?days=7", nil)
	b, _ := json.Marshal(obj)
	if st != 200 || !strings.Contains(string(b), `"events":`) {
		t.Fatalf("admin: %d %s", st, b)
	}
	if st, obj := f.s.adminDo("GET", "/usage", nil); st != 200 || fmt.Sprint(obj) == "" {
		t.Fatalf("0.5.x usage: %d", st)
	} else if m, ok := obj.(map[string]any); !ok || m["usage"] == nil && m["days"] != nil {
		t.Fatalf("0.5.x usage changed shape: %v", obj)
	}
}

func sumEvents(d []struct{ Events int64 }) (n int64) {
	for _, x := range d {
		n += x.Events
	}
	return
}

// --- outcomes and token sources: the rules of endEvent ---

func TestEventOutcomesAndTokenSources(t *testing.T) {
	f := newCoreFx(t, nil)
	cases := []struct {
		name       string
		dispatched bool
		status     int
		in, out    int64
		source     string
		errSeen    bool
		cancel     bool
		want       string // outcome; "" = no event
		wantSrc    string
		wantIn     int64
	}{
		{"ok, reported", true, 200, 10, 5, "", false, false, "ok", "reported", 10},
		{"ok, no counts", true, 200, 0, 0, "", false, false, "ok", "unknown", -1},
		{"ok, estimated output", true, 200, 0, 7, "estimated", false, false, "ok", "estimated", -1},
		{"rate limited by the provider", true, 429, 0, 0, "", false, false, "error", "unknown", -1},
		{"upstream failure", true, 502, 0, 0, "", false, false, "error", "unknown", -1},
		{"upstream 400 after dispatch", true, 400, 0, 0, "", false, false, "error", "unknown", -1}, // review A9
		{"upstream 401 after dispatch", true, 401, 0, 0, "", false, false, "error", "unknown", -1},
		{"upstream 404 after dispatch", true, 404, 0, 0, "", false, false, "error", "unknown", -1},
		{"stream error after 200", true, 200, 3, 1, "", true, false, "error", "reported", 3},
		{"client went away", true, 200, 0, 0, "", false, true, "cancelled", "unknown", -1},
		{"499", true, 499, 0, 0, "", false, false, "cancelled", "unknown", -1},
		{"local refusal 400, nothing dispatched", false, 400, 0, 0, "", false, false, "", "", 0},
		{"local 429 (cached limit), nothing dispatched", false, 429, 0, 0, "", false, false, "", "", 0}, // was counted by the status rule
		{"local 503 before the process, nothing dispatched", false, 503, 0, 0, "", false, false, "", "", 0},
		{"local 404 unknown model", false, 404, 0, 0, "", false, false, "", "", 0},
		{"local 200 without a dispatch", false, 200, 0, 0, "", false, false, "", "", 0},
	}
	for _, c := range cases {
		ctx, cancel := context.WithCancel(context.Background())
		hr := httptest.NewRequest("POST", "/v1/messages", nil).WithContext(ctx)
		r := &req{Request: hr, conn: &serveConn{device: &serveDevice{ID: devID(f.dev["ana"])}}, tokenSource: c.source, provider: "codex"}
		f.s.admitEvent(r, "m-"+strings.ReplaceAll(c.name, " ", "-"))
		if r.event == nil || r.event.UserID != f.uid["ana"] || r.event.DeviceID != devID(f.dev["ana"]) {
			t.Fatalf("%s: event %+v", c.name, r.event)
		}
		if c.dispatched {
			f.s.dispatched(r)
		}
		if c.cancel {
			cancel()
		}
		f.s.endEvent(r, c.status, &usageWriter{in: c.in, out: c.out, errSeen: c.errSeen})
		cancel()
	}
	got := map[string]evRow{}
	for _, e := range f.events() {
		got[e.Model] = e
	}
	for _, c := range cases {
		e, ok := got["m-"+strings.ReplaceAll(c.name, " ", "-")]
		if c.want == "" {
			if ok {
				t.Errorf("%s: counted: %+v", c.name, e)
			}
			continue
		}
		if !ok || e.Outcome != c.want || e.Source != c.wantSrc || e.In != c.wantIn {
			t.Errorf("%s: %+v, want %s/%s/%d", c.name, e, c.want, c.wantSrc, c.wantIn)
		}
	}
}

// Review A5: one admission context. A nested call carries the root's project and user, a rebinding between admission
// and finish changes nothing, and a run cut off before its finish keeps its project.
func TestEventAdmissionContext(t *testing.T) {
	f := newCoreFx(t, nil)
	mk := func(withRun *agentRun) *req {
		hr := httptest.NewRequest("POST", "/v1/messages", nil)
		if withRun != nil {
			hr = hr.WithContext(context.WithValue(hr.Context(), agentRunKey{}, withRun))
		}
		return &req{Request: hr, conn: &serveConn{device: &serveDevice{ID: devID(f.dev["ana"])}}, provider: "openrouter"}
	}
	// a nested PROVIDER call (it never goes through attachProject)
	root := mk(nil)
	root.project = &projectCtx{user: f.uid["ana"], project: f.pid["fakture"]}
	f.s.admitEvent(root, "claude-cli-haiku")
	f.s.dispatched(root)
	run := &agentRun{evID: root.event.ID, evUser: root.event.UserID, proj: root.project}
	nested := mk(run)
	f.s.admitEvent(nested, "openrouter/some-model")
	f.ok(f.s.core.SetBinding(t0, devID(f.dev["ana"]), f.uid["bor"])) // rebound between admission and finish
	f.s.dispatched(nested)
	f.s.endEvent(nested, 200, &usageWriter{in: 1, out: 1})
	if nested.event.UserID != f.uid["ana"] || nested.event.ProjectID != f.pid["fakture"] || nested.event.ParentID != root.event.ID {
		t.Fatalf("%+v", nested.event)
	}
	rows := map[string]evRow{}
	for _, e := range f.events() {
		rows[e.ID] = e
	}
	if e := rows[nested.event.ID]; e.User != f.uid["ana"] || e.Project != f.pid["fakture"] || e.Parent != root.event.ID || e.Outcome != "ok" {
		t.Fatalf("nested row: %+v", e)
	}
	// the root never finished: the process stops; the next open marks it interrupted and it keeps its project
	f.s.api.StopRecorder()
	dir := f.s.core.Dir()
	f.s.core.Close()
	st, err := core.Open(dir, core.Options{})
	f.ok(err)
	defer st.Close()
	var outcome string
	var project int64
	st.DB().QueryRow(`SELECT outcome, COALESCE(project_id,0) FROM usage_events WHERE id=?`, root.event.ID).Scan(&outcome, &project)
	if outcome != "interrupted" || project != f.pid["fakture"] {
		t.Fatalf("%s project %d", outcome, project)
	}
}

// --- case 20: the core is down, models answer, events are spooled and replayed once; overflow leaves a gap ---

func TestUsageSpoolReplayExactlyOnceAndGaps(t *testing.T) {
	x := newRunFx(t, false, false)
	f := x.f
	x.setPlan()
	dir := folder(t, t.TempDir(), "w")
	coreDir := filepath.Join(f.s.cfgDir, coreDirName)
	spool := filepath.Join(coreDir, coreapi.SpoolName) // #106 S3: the spool lives inside the protected core folder

	reopen := func() { // a restart of the gateway's core and recorder
		f.s.api.StopRecorder()
		f.s.core.Close()
		st, err := core.Open(coreDir, core.Options{})
		f.ok(err)
		f.s.core = st
		f.s.api.StartRecorder()
	}
	lines := func() int { b, _ := os.ReadFile(spool); return strings.Count(string(b), "\n") }

	f.s.api.UserOf(t0, devID(f.dev["ana"])) // the gateway has seen ana's binding while the core was up
	f.s.core.Close()                        // the core stops; the gateway goes on
	for i := 0; i < 3; i++ {
		if st, body := x.agent("ana", dir, fmt.Sprint("s", i), 0, "go"); st != 200 { // models are never blocked by accounting
			t.Fatalf("with the core down: %d %s", st, body)
		}
	}
	f.s.api.FlushRecorder()
	if lines() != 3 {
		t.Fatalf("spool lines: %d", lines())
	}
	if fi, err := os.Stat(spool); err != nil || (fi.Mode().Perm() != 0o600 && os.PathSeparator != '\\') {
		t.Fatalf("spool file: %v %v", fi, err)
	}
	// the restart replays; the answer is lost between the insert and the clearing of the file: it runs again
	crashed := 0
	coreapi.SpoolAfterReplay = func() bool { crashed++; return true }
	reopen()
	f.s.api.FlushRecorder()
	coreapi.SpoolAfterReplay = nil
	if crashed != 1 || lines() != 3 || len(f.events()) != 3 {
		t.Fatalf("after the interrupted replay: crashed %d, spool %d, rows %d", crashed, lines(), len(f.events()))
	}
	reopen()
	f.s.api.FlushRecorder()
	if lines() != 0 || len(f.events()) != 3 { // exactly once
		t.Fatalf("after the second replay: spool %d, rows %d", lines(), len(f.events()))
	}
	for _, e := range f.events() {
		if e.Outcome != "ok" || e.User != f.uid["ana"] {
			t.Fatalf("replayed event: %+v", e)
		}
	}

	// overflow: a spool of 4 events keeps the newest and records what was dropped
	old := coreapi.SpoolMaxEvents
	coreapi.SpoolMaxEvents = 4
	defer func() { coreapi.SpoolMaxEvents = old }()
	f.s.api.UserOf(t0, devID(f.dev["ana"]))
	f.s.core.Close()
	for i := 0; i < 7; i++ {
		if st, _ := x.agent("ana", dir, fmt.Sprint("o", i), 0, "go"); st != 200 {
			t.Fatal(st)
		}
	}
	f.s.api.FlushRecorder()
	reopen()
	f.s.api.FlushRecorder()
	rep, err := f.s.core.AdminUsage(t0, 7, core.UsageFilter{})
	f.ok(err)
	var kept, lost int64
	for _, r := range rep.Rows {
		kept += r.Events
	}
	for _, g := range rep.Gaps {
		lost += g.Count
	}
	if kept+lost != 10 || lost == 0 || kept < 3+1 { // 3 before, 7 now: every event is either stored or counted as lost
		t.Fatalf("kept %d, lost %d", kept, lost)
	}
	var out strings.Builder
	var r usageReply
	b, _ := json.Marshal(coreapi.UsageJSON(rep))
	json.Unmarshal(b, &r)
	printUsage(&out, r, true)
	if !strings.Contains(out.String(), "events lost between") || strings.Count(out.String(), "events lost between") != len(rep.Gaps) {
		t.Fatalf("usage output: %s", out.String())
	}
	// the member route says so too
	rr := f.call("fani", "GET", "/minigw/v1/usage?project=fakture", "") // a project admin sees gaps (review L5) ...
	if !strings.Contains(rr.body, `"gaps":[{`) {
		t.Fatalf("project admin without the gap: %s", rr.body)
	}
	if rr := f.call("ana", "GET", "/minigw/v1/usage", ""); !strings.Contains(rr.body, `"gaps":[]`) { // ... an ordinary member does not
		t.Fatalf("member sees gaps: %s", rr.body)
	}
}

// --- quota: parsers with fixture logs, no network and no real CLI ---

func TestCodexRateLimitsFromThisRunsLog(t *testing.T) {
	home := t.TempDir()
	day := filepath.Join(home, "sessions", "2026", "11", "14")
	os.MkdirAll(day, 0o755)
	reset5h, resetWeek := time.Now().Add(2*time.Hour).Unix(), time.Now().Add(72*time.Hour).Unix()
	rec := func(p5, pw float64) string {
		return fmt.Sprintf(`{"timestamp":"t","type":"event_msg","payload":{"type":"token_count","info":null,"rate_limits":{"primary":{"used_percent":%v,"window_minutes":300,"resets_at":%d},"secondary":{"used_percent":%v,"window_minutes":10080,"resets_at":%d},"credits":{"has_credits":false}}}}`, p5, reset5h, pw, resetWeek)
	}
	mine := filepath.Join(day, "rollout-2026-11-14T10-00-00-thread-mine.jsonl")
	other := filepath.Join(day, "rollout-2026-11-14T11-00-00-thread-other.jsonl") // newer, but another run's
	os.WriteFile(mine, []byte("not json\n"+rec(10, 20)+"\n"+`{"type":"other"}`+"\n"+rec(12.5, 21)+"\n"+`{"type":"turn"}`+"\n"), 0o644)
	os.WriteFile(other, []byte(rec(99, 99)+"\n"), 0o644)
	s, _ := testServer(t, nil)
	s.recordCodexQuota(home, "thread-mine")
	vals := markStale(s.quota.codex, time.Now())
	if len(vals) != 2 || vals[0].Name != "codex.5h" || *vals[0].Value != 12.5 || vals[1].Name != "codex.week" || *vals[1].Value != 21 {
		t.Fatalf("%+v", vals)
	}
	if vals[0].Source != "codex-session-log" || vals[0].ObservedAt == nil || vals[0].ResetsAt == nil || vals[0].ResetsAt.Unix() != reset5h || vals[0].Stale {
		t.Fatalf("%+v", vals[0])
	}
	// no thread, a thread name that tries a glob, a missing log: nothing recorded
	s.quota.codex = nil
	for _, th := range []string{"", "*", "../x", "thread-none"} {
		s.recordCodexQuota(home, th)
	}
	if s.quota.codex != nil {
		t.Fatal("recorded from nothing")
	}
	// a reading whose window has reset since is stale; a huge log is only read from its end
	old := parseCodexRateLimits([]byte(rec(1, 1)), time.Now().Add(-5*time.Hour))
	old[0].ResetsAt = func() *time.Time { x := time.Now().Add(-time.Hour); return &x }()
	if !markStale(old, time.Now())[0].Stale {
		t.Fatal("a reset window must be stale")
	}
	big := filepath.Join(day, "rollout-2026-11-14T12-00-00-thread-big.jsonl")
	os.WriteFile(big, []byte(strings.Repeat(`{"filler":"`+strings.Repeat("x", 900)+`"}`+"\n", 1000)+rec(33, 44)+"\n"), 0o644)
	if v := lastRateLimits(big, time.Now()); len(v) != 2 || *v[0].Value != 33 {
		t.Fatalf("%+v", v)
	}
}

const claudeUsageFixture = `{"is_error":false,"duration_api_ms":0,"num_turns":0,"total_cost_usd":0,"usage":{"input_tokens":0,"output_tokens":0},"result":"You are currently using your subscription to power your Claude Code usage\n\nCurrent week (all models): 8% used · resets Nov 19 at 12am (Europe/Ljubljana)\nCurrent week (Fable): 12.5% used · resets Nov 19 at 3:30pm (UTC)\n\nWhat's contributing to your usage?\n"}`

func TestClaudeUsageParserAndGuards(t *testing.T) {
	now := time.Date(2026, 11, 14, 10, 0, 0, 0, time.UTC)
	vals, err := parseClaudeUsage([]byte(claudeUsageFixture), now)
	if err != nil || len(vals) != 2 {
		t.Fatalf("%+v %v", vals, err)
	}
	if vals[0].Name != "claude.week.all-models" || *vals[0].Value != 8 || vals[0].Source != "claude-usage" {
		t.Fatalf("%+v", vals[0])
	}
	if vals[0].ResetsAt == nil || vals[0].ResetsAt.Format(time.RFC3339) != "2026-11-18T23:00:00Z" { // 12am Nov 19 in Ljubljana (UTC+1)
		t.Fatalf("resets %v", vals[0].ResetsAt)
	}
	if vals[1].Name != "claude.week.fable" || *vals[1].Value != 12.5 || vals[1].ResetsAt.Format(time.RFC3339) != "2026-11-19T15:30:00Z" {
		t.Fatalf("%+v", vals[1])
	}
	for name, bad := range map[string]string{
		"a turn ran":      strings.Replace(claudeUsageFixture, `"num_turns":0`, `"num_turns":1`, 1),
		"cost":            strings.Replace(claudeUsageFixture, `"total_cost_usd":0`, `"total_cost_usd":0.01`, 1),
		"tokens":          strings.Replace(claudeUsageFixture, `"output_tokens":0`, `"output_tokens":9`, 1),
		"not json":        "Current week: 8% used",
		"another version": strings.Replace(claudeUsageFixture, "Current week", "This week", 2),
	} {
		if _, err := parseClaudeUsage([]byte(bad), now); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// the gateway: cached for 10 minutes, stale on a failed refresh, and switched off for good after inference
	s, _ := testServer(t, func(c *serveConfig) { c.ClaudeQuotaProbe = true }) // opt-in (review A4)
	calls := 0
	reply, rerr := []byte(claudeUsageFixture), error(nil)
	claudeUsageFn = func(context.Context, []string, string) ([]byte, error) { calls++; return reply, rerr }
	defer func() { claudeUsageFn = runClaudeUsage }()
	for i := 0; i < 3; i++ {
		if v := s.claudeQuota(context.Background(), now.Add(time.Duration(i)*time.Minute)); len(v) != 2 || v[0].Stale {
			t.Fatalf("%+v", v)
		}
	}
	if calls != 1 {
		t.Fatalf("%d calls within the cache time", calls)
	}
	rerr = fmt.Errorf("timeout")
	v := s.claudeQuota(context.Background(), now.Add(11*time.Minute))
	if calls != 2 || len(v) != 2 || !v[0].Stale || !strings.Contains(v[0].Note, "timeout") {
		t.Fatalf("after a failed refresh: calls %d %+v", calls, v)
	}
	rerr, reply = nil, []byte(strings.Replace(claudeUsageFixture, `"num_turns":0`, `"num_turns":2`, 1))
	v = s.claudeQuota(context.Background(), now.Add(30*time.Minute))
	if len(v) != 1 || v[0].Source != "unavailable" || !strings.Contains(v[0].Reason, "ran inference") {
		t.Fatalf("%+v", v)
	}
	before := calls
	s.claudeQuota(context.Background(), now.Add(2*time.Hour))
	if calls != before {
		t.Fatal("ran again after inference")
	}
}

func TestProviderCreditAndSpend(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]string{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/credits", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen["openrouter"] = r.Header.Get("Authorization")
		mu.Unlock()
		w.Write([]byte(`{"data":{"total_credits":50,"total_usage":12.25}}`))
	})
	mux.HandleFunc("/key/info", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen["litellm"] = r.Header.Get("Authorization")
		mu.Unlock()
		w.Write([]byte(`{"key":"sk-xxx","info":{"spend":3.5,"max_budget":10,"budget_reset_at":"2026-12-01T00:00:00Z"}}`))
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()
	og := keyGet
	keyGet = func(_, n string) (string, string) { return "SECRET-" + n, "" }
	defer func() { keyGet = og }()
	s, _ := testServer(t, func(c *serveConfig) {
		c.Gateways = []serveGateway{{Name: "OpenRouter", BaseURL: ts.URL + "/api", Insecure: true}, {Name: "LiteLLM", BaseURL: ts.URL, Insecure: true},
			{Name: "Other", BaseURL: ts.URL, Insecure: true}}
	})
	vals := s.quotaReport(context.Background())
	byName := map[string]quotaValue{}
	for _, v := range vals {
		byName[v.Name] = v
	}
	if v := byName["openrouter.credits-remaining"]; v.Value == nil || *v.Value != 37.75 || v.Source != "openrouter-api" || v.Stale {
		t.Fatalf("%+v", v)
	}
	if v := byName["litellm.spend"]; v.Value == nil || *v.Value != 3.5 || v.ResetsAt == nil || v.ResetsAt.Format("2006-01-02") != "2026-12-01" {
		t.Fatalf("%+v", v)
	}
	if byName["litellm.max-budget"].Value == nil {
		t.Fatal("max budget missing")
	}
	if seen["openrouter"] != "Bearer SECRET-OpenRouter" || seen["litellm"] != "Bearer SECRET-LiteLLM" {
		t.Fatalf("keys sent: %v", seen)
	}
	b, _ := json.Marshal(vals) // the key never leaves the gateway
	if strings.Contains(string(b), "SECRET") {
		t.Fatal("a key is in the report")
	}
	if _, ok := byName["codex"]; !ok || byName["codex"].Source != "unavailable" { // no Codex run yet
		t.Fatalf("codex entry: %+v", byName["codex"])
	}
	// the provider goes away: the cached values come back marked stale (the cache is still fresh here: force a refetch)
	ts.Close()
	s.quota.mu.Lock()
	for k, c := range s.quota.remote {
		c.at = c.at.Add(-time.Hour)
		s.quota.remote[k] = c
	}
	s.quota.mu.Unlock()
	for _, v := range s.quotaReport(context.Background()) {
		if strings.HasPrefix(v.Name, "openrouter.") && (!v.Stale || v.Note == "") {
			t.Fatalf("not stale after a failed refresh: %+v", v)
		}
	}
}

func TestQuotaAdminRouteNeedsNoCore(t *testing.T) {
	s, _ := testServer(t, nil) // no core opened
	claudeUsageFn = func(context.Context, []string, string) ([]byte, error) { return nil, fmt.Errorf("no claude") }
	defer func() { claudeUsageFn = runClaudeUsage }()
	st, obj := s.adminDo("GET", "/quota", nil)
	b, _ := json.Marshal(obj)
	if st != 200 || !strings.Contains(string(b), `"quota"`) {
		t.Fatalf("%d %s", st, b)
	}
	if st, _ := s.adminDo("GET", "/usage/events", nil); st != 503 { // usage events need the core
		t.Fatalf("usage/events without a core: %d", st)
	}
}

// --- review A8: the spool reports failures and keeps its bounds ---

// Review A4: the probe is off unless the owner turns it on; its environment is an allowlist.
func TestClaudeProbeIsOptInWithAnAllowlistedEnvironment(t *testing.T) {
	s, _ := testServer(t, nil)
	called := false
	claudeUsageFn = func(context.Context, []string, string) ([]byte, error) {
		called = true
		return []byte(claudeUsageFixture), nil
	}
	defer func() { claudeUsageFn = runClaudeUsage }()
	v := s.claudeQuota(context.Background(), time.Now())
	if called || len(v) != 1 || v[0].Source != "unavailable" || v[0].Reason != "probe off" {
		t.Fatalf("called %v %+v", called, v)
	}
	if st, obj := s.adminDo("PATCH", "/quota", []byte(`{"claudeProbe":true}`)); st != 200 || fmt.Sprint(obj) == "" {
		t.Fatalf("%d %v", st, obj)
	}
	if v := s.claudeQuota(context.Background(), time.Now()); !called || len(v) != 2 {
		t.Fatalf("called %v %+v", called, v)
	}
	if c, _ := loadServeConfig(s.cfgPath); !c.ClaudeQuotaProbe { // persisted
		t.Fatal("the setting was not saved")
	}
	// the environment: only the allowlist, plus a private temp folder
	env := probeEnv([]string{"PATH=/bin", "HOME=/h", "ANTHROPIC_BASE_URL=http://evil", "ANTHROPIC_API_KEY=k", "CLAUDE_CODE_USE_BEDROCK=1",
		"CLAUDE_CODE_OAUTH_TOKEN=tok", "CLAUDE_CONFIG_DIR=/c", "AWS_SECRET_ACCESS_KEY=s", "HTTPS_PROXY=http://p", "MINIGW_ANYTHING=1", "TZ=UTC"}, "/probe")
	got := strings.Join(env, "\n")
	for _, bad := range []string{"ANTHROPIC", "BEDROCK", "AWS_", "PROXY", "MINIGW"} {
		if strings.Contains(got, bad) {
			t.Errorf("%s reached the probe: %s", bad, got)
		}
	}
	for _, want := range []string{"PATH=/bin", "HOME=/h", "CLAUDE_CODE_OAUTH_TOKEN=tok", "CLAUDE_CONFIG_DIR=/c", "TZ=UTC", "TMPDIR=/probe"} {
		if !strings.Contains(got, want) {
			t.Errorf("%s missing: %s", want, got)
		}
	}
	for k, why := range probeEnvAllow {
		if why == "" {
			t.Errorf("%s has no documented reason", k)
		}
	}
}

// Review A3: a stalled quota refresh never delays a model response or the recording of Codex numbers.
func TestStalledQuotaRefreshDoesNotBlockTheModelPath(t *testing.T) {
	s, _ := testServer(t, func(c *serveConfig) { c.ClaudeQuotaProbe = true })
	release, started := make(chan struct{}), make(chan struct{})
	claudeUsageFn = func(context.Context, []string, string) ([]byte, error) {
		close(started)
		<-release
		return []byte(claudeUsageFixture), nil
	}
	defer func() { claudeUsageFn = runClaudeUsage }()
	done := make(chan struct{})
	go func() { s.claudeQuota(context.Background(), time.Now()); close(done) }()
	<-started // the probe is running and stuck
	home := t.TempDir()
	day := filepath.Join(home, "sessions", "2026", "11", "14")
	os.MkdirAll(day, 0o755)
	os.WriteFile(filepath.Join(day, "rollout-2026-11-14T10-00-00-th.jsonl"), []byte(`{"payload":{"rate_limits":{"primary":{"used_percent":5,"window_minutes":300,"resets_at":4102444800}}}}`+"\n"), 0o644)
	fin := make(chan struct{})
	go func() { s.queueCodexQuota(home, "th"); s.recordCodexQuota(home, "th"); close(fin) }() // what a Codex completion does
	select {
	case <-fin:
	case <-time.After(3 * time.Second):
		t.Fatal("recording Codex numbers waited for the stuck probe")
	}
	// the report's other sources are not held up either: a second caller of the probe waits for the first, but the
	// data lock is free, so codex values are readable at once
	s.quota.mu.Lock()
	n := len(s.quota.codex)
	s.quota.mu.Unlock()
	if n != 1 {
		t.Fatalf("codex values: %d", n)
	}
	close(release)
	<-done
}

func TestCodexCompletionReturnsWhileAQuotaRefreshIsStuck(t *testing.T) { // review A3, through a real Codex run
	x := newRunFx(t, false, true)
	x.f.s.update(func(c *serveConfig) error { c.ClaudeQuotaProbe = true; return nil })
	release, started := make(chan struct{}), make(chan struct{})
	claudeUsageFn = func(context.Context, []string, string) ([]byte, error) {
		close(started)
		<-release
		return []byte(claudeUsageFixture), nil
	}
	defer func() { claudeUsageFn = runClaudeUsage }()
	done := make(chan struct{})
	go func() { x.f.s.claudeQuota(context.Background(), time.Now()); close(done) }()
	<-started
	x.setPlan()
	os.WriteFile(x.plan+".codex", []byte("[]"), 0o644)
	x.model = "claude-codex"
	finished := make(chan int, 1)
	go func() { st, _ := x.agent("ana", folder(t, t.TempDir(), "w"), "s-codex", 0, "go"); finished <- st }()
	select {
	case st := <-finished:
		if st != 200 {
			t.Fatalf("codex: %d", st)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the Codex answer waited for the stuck quota refresh")
	}
	close(release)
	<-done
}

func TestRetentionFoldsAgainOnALongRunningGateway(t *testing.T) { // review (should-fix)
	oe, of := coreapi.SpoolReplayEach, coreapi.FoldEvery
	coreapi.SpoolReplayEach, coreapi.FoldEvery = 20*time.Millisecond, 10*time.Millisecond
	defer func() { coreapi.SpoolReplayEach, coreapi.FoldEvery = oe, of }()
	f := newCoreFx(t, nil) // its recorder starts with the short intervals
	old := time.Now().AddDate(0, 0, -500)
	f.ok(f.s.core.FinishEvent(t0, core.UsageEvent{ID: "ancient", Start: old, End: old, UserID: f.uid["ana"], DeviceID: "d", Provider: "p", Model: "m",
		Outcome: "error", TokensIn: 3, TokensOut: 4, TokenSource: "reported"}))
	deadline := time.Now().Add(5 * time.Second)
	for {
		var n int
		f.s.core.DB().QueryRow(`SELECT count(*) FROM usage_events WHERE id='ancient'`).Scan(&n)
		if n == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the old event was never folded")
		}
		time.Sleep(20 * time.Millisecond)
	}
	rep, err := f.s.core.AdminUsage(t0, 3650, core.UsageFilter{})
	f.ok(err)
	if len(rep.Rows) != 1 || rep.Rows[0].Events != 1 || rep.Rows[0].Failed != 1 || rep.Rows[0].TokensOut != 4 {
		t.Fatalf("%+v", rep.Rows)
	}
}

// Review A5, end to end: a run with a project makes a real nested PROVIDER call through its run port (a fake upstream
// provider); the stored event of that call carries the root's user and project and the parent id.
func TestNestedProviderCallCarriesTheRootsAttribution(t *testing.T) {
	keys := fakeKeys(t)
	keys["gw"] = gwKey
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			io.WriteString(w, `{"data":[{"id":"anthropic/claude-x","context_length":200000}]}`)
		case "/v1/messages":
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"input_tokens\":4,\"output_tokens\":6}}\n\n")
		default:
			http.NotFound(w, r)
		}
	}))
	defer up.Close()
	x := newRunFxCfg(t, true, false, func(c *serveConfig) {
		c.Gateways = []serveGateway{{Name: "gw", BaseURL: up.URL, Insecure: true, Models: []string{"anthropic/claude-x"}}}
	})
	f := x.f
	x.setPlan(runStep{Path: "/v1/messages", Method: "POST",
		Body: `{"model":"anthropic/claude-x","stream":true,"max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`})
	dir := folder(t, t.TempDir(), "w")
	if st, body := x.agent("ana", dir, "sid-np", f.pid["fakture"], "go"); st != 200 {
		t.Fatalf("%d %s", st, body)
	}
	if res := x.results(); len(res) != 1 || res[0].Status != 200 {
		t.Fatalf("the nested provider call: %+v", res)
	}
	f.ok(f.s.core.SetBinding(t0, devID(f.dev["ana"]), f.uid["bor"])) // a rebinding after the fact changes nothing stored
	var root, nested evRow
	for _, e := range f.events() {
		if e.Model == "claude-cli-haiku" {
			root = e
		} else {
			nested = e
		}
	}
	if root.ID == "" || nested.ID == "" {
		t.Fatalf("events: %+v", f.events())
	}
	if nested.Parent != root.ID || nested.User != f.uid["ana"] || nested.Project != f.pid["fakture"] || nested.Provider != "gw" ||
		nested.Model != "anthropic/claude-x" || nested.Outcome != "ok" || nested.Source != "reported" || nested.In != 4 || nested.Out != 6 {
		t.Fatalf("nested: %+v (root %+v)", nested, root)
	}
	if root.User != f.uid["ana"] || root.Project != f.pid["fakture"] {
		t.Fatalf("root: %+v", root)
	}
}
