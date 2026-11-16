package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"minigw/core"
)

func TestQuotaHistorySharedFixture(t *testing.T) {
	var fx struct {
		History json.RawMessage
		Text    string
		Wire    core.QuotaProbe
	}
	if err := json.Unmarshal(srcFile(t, "testdata/quota-history.json"), &fx); err != nil {
		t.Fatal(err)
	}
	if got := quotaHistoryText(fx.History); got != fx.Text {
		t.Fatalf("got %q want %q", got, fx.Text)
	}
	if !fx.Wire.Valid() {
		t.Fatal("wire fixture invalid")
	}
	for _, asJSON := range []bool{false, true} {
		var calls []string
		a := &adminClient{fake: func(method, path string, b []byte) (int, []byte) {
			calls = append(calls, method+" "+path)
			return 200, fx.History
		}}
		out, stderr, exit := captured(func() {
			coreCommand(a, []string{"quota"}, coreOpts{days: "7", provider: "codex", history: true, json: asJSON})
		})
		if exit != 0 || stderr != "" || len(calls) != 1 || calls[0] != "GET /quota/history?days=7&provider=codex" {
			t.Fatal(calls, stderr, exit)
		}
		if !asJSON && out != fx.Text {
			t.Fatal(out)
		}
		if asJSON {
			var got, want any
			json.Unmarshal([]byte(out), &got)
			json.Unmarshal(fx.History, &want)
			if fmt.Sprint(got) != fmt.Sprint(want) {
				t.Fatal(out)
			}
		}
	}
	a := &adminClient{fake: func(string, string, []byte) (int, []byte) { return 206, fx.History }}
	_, stderr, exit := captured(func() { coreCommand(a, []string{"quota"}, coreOpts{days: "7", history: true}) })
	if exit != 0 || !strings.Contains(stderr, "capped") {
		t.Fatal(stderr, exit)
	}
	_, stderr, exit = captured(func() { coreCommand(a, []string{"quota"}, coreOpts{days: "7", history: true, claudeProbe: "on"}) })
	if exit == 0 || !strings.Contains(stderr, "cannot be combined") {
		t.Fatal(stderr, exit)
	}
}

func quotaCount(t *testing.T, s *server) int {
	t.Helper()
	s.api.FlushRecorder()
	var n int
	if err := s.core.DB().QueryRow(`SELECT count(*) FROM quota_probes`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestQuotaHistoryRecordsFreshClaudeFailuresAndOptOut(t *testing.T) {
	f := newCoreFx(t, func(c *serveConfig) { c.ClaudeQuotaProbe = true })
	s := f.s
	s.claude = []string{"claude"}
	old := claudeUsageFn
	defer func() { claudeUsageFn = old }()
	var calls atomic.Int32
	claudeUsageFn = func(context.Context, []string, string) ([]byte, error) {
		calls.Add(1)
		return []byte(claudeUsageFixture), nil
	}
	now := time.Now().UTC()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); s.claudeQuota(context.Background(), now) }()
	}
	wg.Wait()
	if calls.Load() != 1 || quotaCount(t, s) != 1 {
		t.Fatal(calls.Load(), quotaCount(t, s))
	}
	s.claudeQuota(context.Background(), now.Add(time.Minute))
	s.quotaReportF(context.Background(), false)
	if quotaCount(t, s) != 1 {
		t.Fatal("cached read recorded")
	}
	claudeUsageFn = func(context.Context, []string, string) ([]byte, error) {
		calls.Add(1)
		return nil, errors.New("SECRET raw failure")
	}
	for i := 0; i < 4; i++ {
		s.claudeQuota(context.Background(), now.Add(time.Hour))
	}
	if calls.Load() != 2 || quotaCount(t, s) != 2 {
		t.Fatal(calls.Load(), quotaCount(t, s))
	}
	var kind string
	f.ok(s.core.DB().QueryRow(`SELECT error_kind FROM quota_probes WHERE status='error'`).Scan(&kind))
	if kind != "probe" {
		t.Fatal(kind)
	}
	f.ok(s.update(func(c *serveConfig) error { c.ClaudeQuotaProbe = false; return nil }))
	s.claudeQuota(context.Background(), now.Add(2*time.Hour))
	if quotaCount(t, s) != 2 {
		t.Fatal("opt out recorded")
	}
	// The history route itself never runs the probe and needs the core.
	status, obj := s.adminDo("GET", "/quota/history?days=7", nil)
	if status != 200 || calls.Load() != 2 || strings.Contains(fmt.Sprint(obj), "SECRET") {
		t.Fatal(status, obj, calls.Load())
	}
	if r := f.raw(nil, "", "GET", adminPrefix+"/quota/history", ""); r.status != 403 {
		t.Fatal(r.status, r.body)
	}
}

func TestQuotaHistoryRemoteScopeAndFailure(t *testing.T) {
	var hits atomic.Int32
	var status atomic.Int32
	status.Store(200)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(int(status.Load()))
		w.Write([]byte(`{"info":{"spend":3.5}}`))
	}))
	defer upstream.Close()
	f := newCoreFx(t, nil)
	s := f.s
	old := keyGet
	key := "SECRET1"
	keyGet = func(string, string) (string, string) { return key, "" }
	defer func() { keyGet = old }()
	g := serveGateway{Name: "LiteLLM", BaseURL: upstream.URL, Insecure: true}
	now := time.Now().UTC()
	for i := 0; i < 3; i++ {
		s.remoteQuota(context.Background(), g, now, true)
	}
	if quotaCount(t, s) != 1 || hits.Load() != 1 {
		t.Fatal(hits.Load())
	}
	s.remoteQuota(context.Background(), g, now.Add(time.Hour), true)
	key = "SECRET2"
	s.remoteQuota(context.Background(), g, now.Add(time.Hour), true)
	status.Store(403)
	for i := 0; i < 3; i++ {
		s.remoteQuota(context.Background(), g, now.Add(2*time.Hour), true)
	}
	if quotaCount(t, s) != 4 || hits.Load() != 4 {
		t.Fatal(hits.Load(), quotaCount(t, s))
	}
	var scopes int
	f.ok(s.core.DB().QueryRow(`SELECT count(DISTINCT account_scope) FROM quota_probes`).Scan(&scopes))
	if scopes != 2 {
		t.Fatal(scopes)
	}
	var kind string
	f.ok(s.core.DB().QueryRow(`SELECT error_kind FROM quota_probes WHERE status='error'`).Scan(&kind))
	if kind != "http" {
		t.Fatal(kind)
	}
	var leaked int
	f.ok(s.core.DB().QueryRow(`SELECT count(*) FROM quota_probes WHERE account_scope LIKE '%SECRET%'`).Scan(&leaked))
	if leaked != 0 {
		t.Fatal(leaked)
	}
}

func TestQuotaHistoryCodexFailedLookup(t *testing.T) {
	f := newCoreFx(t, nil)
	f.s.recordCodexQuota(t.TempDir(), "missing-thread")
	if quotaCount(t, f.s) != 1 {
		t.Fatal("failed lookup not recorded")
	}
	var n int
	f.ok(f.s.core.DB().QueryRow(`SELECT count(*) FROM quota_values`).Scan(&n))
	if n != 0 {
		t.Fatal(n)
	}
}

func TestQuotaHistoryCodexFinishedRunAndCachedReport(t *testing.T) {
	f := newCoreFx(t, nil)
	home := t.TempDir()
	day := filepath.Join(home, "sessions", "2026", "11", "14")
	f.ok(os.MkdirAll(day, 0700))
	f.ok(os.WriteFile(filepath.Join(day, "rollout-2026-11-14T10-00-00-history-thread.jsonl"), []byte(`{"payload":{"rate_limits":{"primary":{"used_percent":0,"window_minutes":300,"resets_at":4102444800},"secondary":{"used_percent":82,"window_minutes":10080,"resets_at":4102444800}}}}`+"\n"), 0600))
	f.s.recordCodexQuota(home, "history-thread")
	if quotaCount(t, f.s) != 1 {
		t.Fatal("finished run not recorded")
	}
	var windows int
	f.ok(f.s.core.DB().QueryRow(`SELECT count(*) FROM quota_values WHERE resets_at=4102444800000`).Scan(&windows))
	if windows != 2 {
		t.Fatal("missing reset windows", windows)
	}
	f.s.quotaReportF(context.Background(), false)
	if quotaCount(t, f.s) != 1 {
		t.Fatal("cached Codex report recorded again")
	}
}
