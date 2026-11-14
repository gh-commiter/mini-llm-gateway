package main

// Quota and provider credit for the owner (GET <admin>/quota, minigw gw quota). Every value says where it comes from
// (source), when it was observed, when its window resets and whether it is stale. Sources:
//   - codex-session-log: the last rate_limits record in the session log of a Codex run this gateway started;
//   - claude-usage: `claude -p /usage` run in an empty private folder (see claudeUsage for the guards);
//   - openrouter-api, litellm-api: the providers' documented endpoints with the key the gateway already holds.
// Nothing here is billed per request and nothing here is shown to members.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

type quotaValue struct {
	Name       string     `json:"name"` // codex.5h, claude.week, openrouter.credits, litellm.spend …
	Unit       string     `json:"unit"` // percent or usd
	Value      *float64   `json:"value,omitempty"`
	Source     string     `json:"source"`
	ObservedAt *time.Time `json:"observedAt,omitempty"`
	ResetsAt   *time.Time `json:"resetsAt,omitempty"`
	ResetsText string     `json:"resetsText,omitempty"` // as the source printed it, when it could not be turned into a time
	Stale      bool       `json:"stale"`
	Note       string     `json:"note,omitempty"`
	Reason     string     `json:"reason,omitempty"` // why a value is unavailable ("probe off", "no key for …")
}

const (
	quotaCacheFor = 10 * time.Minute
	quotaStaleAge = 24 * time.Hour // an observation older than this, or one whose window has reset since, is stale
)

// quotaState: mu guards the data only and is never held across a slow call (a probe, an HTTP request, a file read);
// the flight mutexes make the slow refreshes single-flight per source (A3).
type quotaState struct {
	mu        sync.Mutex
	claudeFly sync.Mutex
	remoteFly sync.Map // provider name -> *sync.Mutex
	work      chan codexJob
	startOnce sync.Once
	codex     []quotaValue
	claude    []quotaValue
	claudeAt  time.Time
	claudeOff string // set when `claude /usage` turned out to do more than it should: not run again
	remote    map[string]cachedQuota
}

type codexJob struct{ state, thread string }

type cachedQuota struct {
	at   time.Time
	vals []quotaValue
}

func fptr(f float64) *float64 { return &f }

func stale(obs time.Time, resets *time.Time, now time.Time) bool {
	return now.Sub(obs) > quotaStaleAge || resets != nil && now.After(*resets)
}

// --- Codex ---

// parseCodexRateLimits reads the rate_limits object out of one session-log line (it sits at different depths in
// different Codex versions: found by key). primary and secondary are the 5-hour window and the week; resets_at is
// epoch seconds (or resets_in_seconds from the observation).
func parseCodexRateLimits(line []byte, observed time.Time) []quotaValue {
	var v any
	if json.Unmarshal(line, &v) != nil {
		return nil
	}
	rl := findKey(v, "rate_limits")
	m, _ := rl.(map[string]any)
	var out []quotaValue
	for _, w := range []struct{ key, fallback string }{{"primary", "5h"}, {"secondary", "week"}} {
		x, _ := m[w.key].(map[string]any)
		used, ok := x["used_percent"].(float64)
		if !ok {
			continue
		}
		name := w.fallback
		if mins, _ := x["window_minutes"].(float64); mins > 0 && mins < 1500 {
			name = strconv.Itoa(int(mins/60)) + "h"
		} else if mins >= 1500 {
			name = "week"
			if mins < 10000 {
				name = strconv.Itoa(int(mins/1440)) + "d"
			}
		}
		qv := quotaValue{Name: "codex." + name, Unit: "percent", Value: fptr(used), Source: "codex-session-log", ObservedAt: &observed}
		if at, ok := x["resets_at"].(float64); ok && at > 0 {
			t := time.Unix(int64(at), 0).UTC()
			qv.ResetsAt = &t
		} else if in, ok := x["resets_in_seconds"].(float64); ok {
			t := observed.Add(time.Duration(in) * time.Second).UTC()
			qv.ResetsAt = &t
		}
		out = append(out, qv)
	}
	return out
}

func findKey(v any, key string) any {
	switch x := v.(type) {
	case map[string]any:
		if r, ok := x[key]; ok {
			return r
		}
		for _, c := range x {
			if r := findKey(c, key); r != nil {
				return r
			}
		}
	case []any:
		for _, c := range x {
			if r := findKey(c, key); r != nil {
				return r
			}
		}
	}
	return nil
}

// lastRateLimits reads at most the last 256 KiB of a session log and returns the newest rate_limits record.
func lastRateLimits(path string, observed time.Time) []quotaValue {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil
	}
	const tail = 256 << 10
	off := max(0, fi.Size()-tail)
	buf := make([]byte, fi.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil && err != io.EOF {
		return nil
	}
	lines := bytes.Split(buf, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		if bytes.Contains(lines[i], []byte(`"rate_limits"`)) {
			if vals := parseCodexRateLimits(lines[i], observed); len(vals) > 0 {
				return vals
			}
		}
	}
	return nil
}

// codexSessionFile: the log of THIS run's thread: <home>/sessions/YYYY/MM/DD/rollout-<time>-<thread id>.jsonl.
func codexSessionFile(home, thread string) string {
	if thread == "" || strings.ContainsAny(thread, `*?[]/\`) {
		return ""
	}
	m, _ := filepath.Glob(filepath.Join(home, "sessions", "*", "*", "*", "rollout-*-"+thread+".jsonl"))
	var best string
	var bestT time.Time
	for _, p := range m {
		if fi, err := os.Stat(p); err == nil && (best == "" || fi.ModTime().After(bestT)) {
			best, bestT = p, fi.ModTime()
		}
	}
	return best
}

// queueCodexQuota hands the reading of a finished Codex run's session log to a worker: it never waits and never
// blocks the model response (A3). A full queue drops the job (the next run records newer numbers anyway).
func (s *server) queueCodexQuota(state, thread string) {
	q := &s.quota
	q.startOnce.Do(func() {
		q.work = make(chan codexJob, 8)
		go func() {
			for j := range q.work {
				s.recordCodexQuota(j.state, j.thread)
			}
		}()
	})
	select {
	case q.work <- codexJob{state, thread}:
	default:
	}
}

// recordCodexQuota reads the numbers of a gateway-started Codex run's own session log (called by the worker).
func (s *server) recordCodexQuota(state, thread string) {
	home := state
	if home == "" {
		home, _ = agentAuthFiles(codexKind)
	}
	p := codexSessionFile(home, thread)
	if p == "" {
		return
	}
	if vals := lastRateLimits(p, time.Now().UTC()); len(vals) > 0 {
		s.quota.mu.Lock()
		s.quota.codex = vals
		s.quota.mu.Unlock()
	}
}

// --- Claude ---

var claudeUsageLine = regexp.MustCompile(`(?m)^\s*(Current [^:\n]+?):\s*(\d+(?:\.\d+)?)% used(?:\s*[·|-]\s*resets\s+(.+?))?\s*$`)

// parseClaudeUsage reads the result text of `claude -p /usage --output-format json`. It also checks the JSON
// envelope: the command must have done no inference (0 turns, no cost, no tokens), else the result is refused.
func parseClaudeUsage(data []byte, now time.Time) ([]quotaValue, error) {
	var env struct {
		Result       string  `json:"result"`
		NumTurns     int     `json:"num_turns"`
		TotalCostUSD float64 `json:"total_cost_usd"`
		IsError      bool    `json:"is_error"`
		Usage        struct {
			In  int64 `json:"input_tokens"`
			Out int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, errors.New("unexpected output")
	}
	if env.NumTurns != 0 || env.TotalCostUSD != 0 || env.Usage.In != 0 || env.Usage.Out != 0 {
		return nil, errInference
	}
	if env.IsError {
		return nil, errors.New("claude reported an error")
	}
	var out []quotaValue
	for _, m := range claudeUsageLine.FindAllStringSubmatch(env.Result, -1) {
		used, _ := strconv.ParseFloat(m[2], 64)
		name := strings.ToLower(strings.TrimPrefix(m[1], "Current "))
		name = strings.NewReplacer(" (", ".", ")", "", " ", "-").Replace(name)
		qv := quotaValue{Name: "claude." + name, Unit: "percent", Value: fptr(used), Source: "claude-usage", ObservedAt: &now}
		if m[3] != "" {
			if t := parseClaudeReset(m[3], now); t != nil {
				qv.ResetsAt = t
			} else {
				qv.ResetsText = m[3]
			}
		}
		out = append(out, qv)
	}
	if len(out) == 0 {
		return nil, errors.New("no usage lines in the output (another Claude Code version?)")
	}
	return out, nil
}

var errInference = errors.New("claude /usage ran inference; it is switched off until the gateway restarts")

// parseClaudeReset: "Nov 19 at 12am (Europe/Ljubljana)", "Nov 19 at 3:30pm (UTC)".
func parseClaudeReset(s string, now time.Time) *time.Time {
	loc := time.Local
	if i := strings.LastIndex(s, " ("); i >= 0 && strings.HasSuffix(s, ")") {
		if l, err := time.LoadLocation(s[i+2 : len(s)-1]); err == nil {
			loc = l
		} else {
			return nil
		}
		s = s[:i]
	}
	for _, layout := range []string{"Jan 2 at 3pm", "Jan 2 at 3:04pm", "Jan 2, 2006 at 3pm", "Jan 2, 2006 at 3:04pm"} {
		t, err := time.ParseInLocation(layout, s, loc)
		if err != nil {
			continue
		}
		if t.Year() == 0 {
			t = t.AddDate(now.Year(), 0, 0)
			if t.Before(now.AddDate(0, 0, -1)) {
				t = t.AddDate(1, 0, 0)
			}
		}
		u := t.UTC()
		return &u
	}
	return nil
}

// claudeUsageFn runs the command; tests replace it (no real CLI in tests).
var claudeUsageFn = runClaudeUsage

// runClaudeUsage: `claude -p /usage` in an EMPTY private folder, settings sources off, no session saved, a 30 s timeout,
// and an environment without anything that could send it elsewhere (ANTHROPIC_*, CLAUDE_CODE_*, except the token for
// the login). Checked on Claude Code 2.1.322: 0 turns, no cost, no tokens, the folder stays empty. The envelope is
// checked again on every run (parseClaudeUsage); a run that did inference switches the feature off.
func runClaudeUsage(ctx context.Context, exe []string, parent string) ([]byte, error) {
	if err := os.MkdirAll(parent, 0o700); err != nil { // a private folder under the config folder, not Scratch
		return nil, err
	}
	dir, err := os.MkdirTemp(parent, "probe-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	args := append(slicesClone(exe[1:]), "-p", "/usage", "--model", "haiku", "--setting-sources", "", "--strict-mcp-config", "--mcp-config", `{"mcpServers":{}}`,
		"--output-format", "json", "--no-session-persistence")
	cmd := commandContext(ctx, exe[0], args...)
	cmd.Dir = dir
	cmd.Env = probeEnv(os.Environ(), dir)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, errors.New("claude /usage failed: " + err.Error())
	}
	return out.Bytes(), nil
}

// probeEnvAllow: the only variables the quota probe gets (A4). Each one is what the CLI needs to start and find its
// own login; nothing that routes requests elsewhere (ANTHROPIC_*), nothing that selects settings or behaviour
// (CLAUDE_CODE_*, except the login token).
var probeEnvAllow = map[string]string{
	"PATH":                    "find the claude program's own helpers",
	"HOME":                    "the user's folder, where the login (~/.claude) is",
	"USERPROFILE":             "Windows: the same",
	"APPDATA":                 "Windows: the same",
	"LOCALAPPDATA":            "Windows: the same",
	"SystemRoot":              "Windows: needed to start any program",
	"windir":                  "Windows: the same",
	"SystemDrive":             "Windows: the same",
	"XDG_CONFIG_HOME":         "Linux: where ~/.claude may live",
	"XDG_DATA_HOME":           "Linux: the same",
	"XDG_STATE_HOME":          "Linux: the same",
	"CLAUDE_CONFIG_DIR":       "the login folder, when the user moved it",
	"CLAUDE_CODE_OAUTH_TOKEN": "the login by token (claude setup-token)",
	"LANG":                    "text encoding",
	"LC_ALL":                  "text encoding",
	"TZ":                      "the time zone the reset times are printed in",
}

// probeEnv: the allowlisted variables plus a private temporary folder.
func probeEnv(env []string, tmp string) []string {
	var out []string
	for _, e := range env {
		k, _, _ := strings.Cut(e, "=")
		if _, ok := probeEnvAllow[k]; ok {
			out = append(out, e)
		}
	}
	return append(out, "TMPDIR="+tmp, "TEMP="+tmp, "TMP="+tmp)
}

func slicesClone(a []string) []string { return append([]string(nil), a...) }

func (s *server) claudeQuota(ctx context.Context, now time.Time) []quotaValue {
	q := &s.quota
	if !s.conf().ClaudeQuotaProbe { // opt-in (A4)
		return []quotaValue{{Name: "claude", Source: "unavailable", Stale: true, Reason: "probe off"}}
	}
	exe := s.claudeExe()
	if len(exe) == 0 || s.conf().ClaudeDisabled {
		return []quotaValue{{Name: "claude", Source: "unavailable", Stale: true, Reason: "Claude CLI is off or not installed on this gateway"}}
	}
	cached := func() ([]quotaValue, bool, string) { // data under mu, briefly
		q.mu.Lock()
		defer q.mu.Unlock()
		return q.claude, q.claude != nil && now.Sub(q.claudeAt) < quotaCacheFor, q.claudeOff
	}
	if vals, fresh, off := cached(); off != "" {
		return []quotaValue{{Name: "claude", Source: "unavailable", Stale: true, Reason: off}}
	} else if fresh {
		return markStale(vals, now)
	}
	q.claudeFly.Lock() // single-flight: one probe at a time; waiters take the result
	defer q.claudeFly.Unlock()
	if vals, fresh, off := cached(); off != "" {
		return []quotaValue{{Name: "claude", Source: "unavailable", Stale: true, Reason: off}}
	} else if fresh {
		return markStale(vals, now)
	}
	data, err := claudeUsageFn(ctx, exe, filepath.Join(s.cfgDir, "quota-tmp"))
	var vals []quotaValue
	if err == nil {
		vals, err = parseClaudeUsage(data, now)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	switch {
	case errors.Is(err, errInference):
		q.claudeOff = err.Error()
		return []quotaValue{{Name: "claude", Source: "unavailable", Stale: true, Reason: q.claudeOff}}
	case err != nil:
		if q.claude != nil { // the last good values, marked stale
			out := markStale(q.claude, now)
			for i := range out {
				out[i].Stale, out[i].Note = true, "could not refresh: "+err.Error()
			}
			return out
		}
		return []quotaValue{{Name: "claude", Source: "unavailable", Stale: true, Reason: err.Error()}}
	}
	q.claude, q.claudeAt = vals, now
	return vals
}

func markStale(vals []quotaValue, now time.Time) []quotaValue {
	out := append([]quotaValue(nil), vals...)
	for i := range out {
		if out[i].ObservedAt != nil {
			out[i].Stale = stale(*out[i].ObservedAt, out[i].ResetsAt, now)
		}
	}
	return out
}

// --- OpenRouter and LiteLLM ---

var quotaHTTP = &http.Client{Timeout: 10 * time.Second, CheckRedirect: noRedirect}

func providerKind(g serveGateway) string {
	switch {
	case strings.EqualFold(g.Name, "openrouter") || strings.Contains(strings.ToLower(g.BaseURL), "openrouter.ai"):
		return "openrouter"
	case strings.Contains(strings.ToLower(g.Name), "litellm"):
		return "litellm"
	}
	return ""
}

func parseOpenRouterCredits(data []byte, now time.Time) ([]quotaValue, error) {
	var r struct {
		Data struct {
			Total float64 `json:"total_credits"`
			Used  float64 `json:"total_usage"`
		} `json:"data"`
	}
	if err := json.Unmarshal(data, &r); err != nil || r.Data.Total == 0 && r.Data.Used == 0 {
		return nil, errors.New("unexpected answer")
	}
	return []quotaValue{
		{Name: "openrouter.credits-remaining", Unit: "usd", Value: fptr(r.Data.Total - r.Data.Used), Source: "openrouter-api", ObservedAt: &now},
		{Name: "openrouter.credits-total", Unit: "usd", Value: fptr(r.Data.Total), Source: "openrouter-api", ObservedAt: &now},
	}, nil
}

func parseLiteLLMKeyInfo(data []byte, now time.Time) ([]quotaValue, error) {
	var r struct {
		Info struct {
			Spend       *float64 `json:"spend"`
			MaxBudget   *float64 `json:"max_budget"`
			BudgetReset string   `json:"budget_reset_at"`
		} `json:"info"`
	}
	if err := json.Unmarshal(data, &r); err != nil || r.Info.Spend == nil {
		return nil, errors.New("unexpected answer")
	}
	var resets *time.Time
	if t, err := time.Parse(time.RFC3339, r.Info.BudgetReset); err == nil {
		u := t.UTC()
		resets = &u
	}
	out := []quotaValue{{Name: "litellm.spend", Unit: "usd", Value: r.Info.Spend, Source: "litellm-api", ObservedAt: &now, ResetsAt: resets}}
	if r.Info.MaxBudget != nil {
		out = append(out, quotaValue{Name: "litellm.max-budget", Unit: "usd", Value: r.Info.MaxBudget, Source: "litellm-api", ObservedAt: &now, ResetsAt: resets})
	}
	return out, nil
}

func (s *server) remoteQuota(ctx context.Context, g serveGateway, now time.Time) []quotaValue {
	kind := providerKind(g)
	if kind == "" || g.Disabled {
		return nil
	}
	q := &s.quota
	cachedRemote := func() (cachedQuota, bool) { q.mu.Lock(); defer q.mu.Unlock(); c, ok := q.remote[g.Name]; return c, ok }
	if c, ok := cachedRemote(); ok && now.Sub(c.at) < quotaCacheFor {
		return markStale(c.vals, now)
	}
	fm, _ := q.remoteFly.LoadOrStore(g.Name, &sync.Mutex{})
	fm.(*sync.Mutex).Lock() // single-flight per provider; the data lock is not held during the request
	defer fm.(*sync.Mutex).Unlock()
	if c, ok := cachedRemote(); ok && now.Sub(c.at) < quotaCacheFor {
		return markStale(c.vals, now)
	}
	key, _ := keyGet(s.cfgDir, g.Name) // never logged, never returned
	fail := func(note string) []quotaValue {
		if c, ok := cachedRemote(); ok {
			out := markStale(c.vals, now)
			for i := range out {
				out[i].Stale, out[i].Note = true, note
			}
			return out
		}
		return []quotaValue{{Name: kind, Source: "unavailable", Stale: true, Reason: note}}
	}
	if key == "" {
		return fail("no key for " + g.Name)
	}
	u := strings.TrimSuffix(g.BaseURL, "/")
	parse := parseOpenRouterCredits
	if kind == "openrouter" {
		u += "/v1/credits"
	} else {
		u += "/key/info"
		parse = parseLiteLLMKeyInfo
	}
	req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
	if err != nil {
		return fail("bad provider address")
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := quotaHTTP.Do(req)
	if err != nil {
		return fail("the provider did not answer")
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return fail("the provider answered HTTP " + strconv.Itoa(resp.StatusCode))
	}
	vals, err := parse(data, now)
	if err != nil {
		return fail(err.Error())
	}
	q.mu.Lock()
	if q.remote == nil {
		q.remote = map[string]cachedQuota{}
	}
	q.remote[g.Name] = cachedQuota{now, vals}
	q.mu.Unlock()
	return vals
}

// quotaReport: all values, Codex first. Claude and the providers are fetched on demand, at most once per 10 minutes.
func (s *server) quotaReport(ctx context.Context) []quotaValue {
	now := time.Now().UTC()
	s.quota.mu.Lock()
	out := markStale(s.quota.codex, now)
	s.quota.mu.Unlock()
	if len(out) == 0 {
		out = []quotaValue{{Name: "codex", Source: "unavailable", Stale: true, Note: "no Codex run since this gateway started"}}
	}
	out = append(out, s.claudeQuota(ctx, now)...)
	for _, g := range s.conf().Gateways {
		out = append(out, s.remoteQuota(ctx, g, now)...)
	}
	return out
}
