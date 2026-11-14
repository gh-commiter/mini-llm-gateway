package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// Q8: the probe has no tools, and /usage stays a slash command.
func TestClaudeProbeArgv(t *testing.T) {
	a := probeArgs([]string{"/x/claude"})
	i := slices.Index(a, "--tools")
	if i < 0 || a[i+1] != "" || a[0] != "-p" || a[1] != "/usage" || slices.Contains(a, "--disable-slash-commands") {
		t.Fatalf("%q", a)
	}
	if j := slices.Index(a, "--setting-sources"); j < 0 || a[j+1] != "" || !slices.Contains(a, "--strict-mcp-config") || !slices.Contains(a, "--no-session-persistence") {
		t.Fatalf("%q", a)
	}
	if b := probeArgs([]string{"node", "/x/cli.js"}); b[0] != "/x/cli.js" || !slices.Contains(b, "--tools") { // a script's own arguments come first
		t.Fatalf("%q", b)
	}
}

// Q6: a failing probe is not run again at once; the settings change that turns it on runs nothing.
func TestFailedProbeIsThrottledAndPatchRunsNothing(t *testing.T) {
	s, _ := testServer(t, nil)
	var calls atomic.Int32
	claudeUsageFn = func(context.Context, []string, string) ([]byte, error) {
		calls.Add(1)
		return nil, fmt.Errorf("no luck")
	}
	defer func() { claudeUsageFn = runClaudeUsage }()
	if st, _ := s.adminDo("PATCH", "/quota", []byte(`{"claudeProbe":true}`)); st != 200 || calls.Load() != 0 {
		t.Fatalf("PATCH: %d, %d probes", st, calls.Load())
	}
	now := time.Now().UTC()
	for i := 0; i < 5; i++ { // five requests within the back-off: one probe
		v := s.claudeQuota(context.Background(), now.Add(time.Duration(i)*5*time.Second))
		if len(v) != 1 || v[0].Reason != "no luck" {
			t.Fatalf("%+v", v)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("%d probes within the back-off", calls.Load())
	}
	s.claudeQuota(context.Background(), now.Add(61*time.Second)) // the back-off (1 minute) is over
	if calls.Load() != 2 {
		t.Fatalf("%d probes after it", calls.Load())
	}
	s.claudeQuota(context.Background(), now.Add(61*time.Second+90*time.Second)) // the second one doubled it to 2 minutes
	if calls.Load() != 2 {
		t.Fatalf("%d: the back-off did not grow", calls.Load())
	}
	s.claudeQuota(context.Background(), now.Add(61*time.Second+130*time.Second))
	if calls.Load() != 3 {
		t.Fatalf("%d", calls.Load())
	}
	// a success clears it
	claudeUsageFn = func(context.Context, []string, string) ([]byte, error) {
		calls.Add(1)
		return []byte(claudeUsageFixture), nil
	}
	later := now.Add(time.Hour)
	if v := s.claudeQuota(context.Background(), later); len(v) != 2 || v[0].Stale {
		t.Fatalf("%+v", v)
	}
	// the report after PATCH: no probe, "not probed yet"
	n := calls.Load()
	s2, _ := testServer(t, func(c *serveConfig) { c.ClaudeQuotaProbe = true })
	if rep := s2.quotaReportF(context.Background(), false); calls.Load() != n || rep[1].Reason != "not probed yet" {
		t.Fatalf("%+v", rep)
	}
}

// Q6 and Q7: provider credit calls are throttled when they fail; a changed address or key is another entry; two providers with one name
// do not share a cache.
func TestProviderCacheIsKeyedByAddressAndKeyAndFailuresAreThrottled(t *testing.T) {
	var hits atomic.Int32
	var fail atomic.Bool
	mk := func(spend string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			if fail.Load() {
				http.Error(w, "no", 500)
				return
			}
			w.Write([]byte(`{"info":{"spend":` + spend + `}}`))
		}))
	}
	a, b := mk("1"), mk("2")
	defer a.Close()
	defer b.Close()
	og := keyGet
	key := "k1"
	keyGet = func(_, n string) (string, string) { return key, "" }
	defer func() { keyGet = og }()
	s, _ := testServer(t, func(c *serveConfig) { c.Gateways = []serveGateway{{Name: "LiteLLM", BaseURL: a.URL, Insecure: true}} })
	spend := func() string {
		for _, v := range s.quotaReport(context.Background()) {
			if v.Name == "litellm.spend" {
				return fmt.Sprint(*v.Value, v.Stale)
			}
		}
		return "none"
	}
	if got := spend(); got != "1 false" {
		t.Fatal(got)
	}
	n := hits.Load()
	spend()
	if hits.Load() != n {
		t.Fatal("not cached")
	}
	// another address: the first account's credit is not shown for it
	setGateways(s, serveGateway{Name: "LiteLLM", BaseURL: b.URL, Insecure: true})
	if got := spend(); got != "2 false" {
		t.Fatalf("another address: %s", got)
	}
	// another key: a new request, not the old cache
	key = "k2"
	n = hits.Load()
	spend()
	if hits.Load() != n+1 {
		t.Fatalf("a changed key reused the cache (%d requests)", hits.Load()-n)
	}
	// entries of the old address and key are dropped
	s.quota.mu.Lock()
	entries := len(s.quota.remote)
	s.quota.mu.Unlock()
	if entries != 1 {
		t.Fatalf("%d cache entries", entries)
	}
	// failures: one request, then the back-off
	fail.Store(true)
	key = "k3"
	n = hits.Load()
	for i := 0; i < 4; i++ {
		for _, v := range s.quotaReport(context.Background()) {
			if v.Name == "litellm" && !strings.Contains(v.Reason, "HTTP 500") {
				t.Fatalf("%+v", v)
			}
		}
	}
	if hits.Load() != n+1 {
		t.Fatalf("%d requests for 4 failing reports", hits.Load()-n)
	}
	// the secret is never in the report
	if rep := fmt.Sprint(s.quotaReport(context.Background())); strings.Contains(rep, "k3") || strings.Contains(rep, "Bearer") {
		t.Fatal("key in the report")
	}
	// two providers with one name and different addresses are two entries
	setGateways(s, serveGateway{Name: "LiteLLM", BaseURL: a.URL, Insecure: true}, serveGateway{Name: "LiteLLM", BaseURL: b.URL, Insecure: true})
	fail.Store(false)
	key = "k4"
	var seen []string
	for _, v := range s.quotaReport(context.Background()) {
		if v.Name == "litellm.spend" {
			seen = append(seen, fmt.Sprint(*v.Value))
		}
	}
	if !slices.Equal(seen, []string{"1", "2"}) {
		t.Fatalf("%v", seen)
	}
}

// setGateways replaces the live provider list (the test server's settings are in memory).
func setGateways(s *server, g ...serveGateway) {
	s.mu.Lock()
	s.cfg.Gateways = g
	s.mu.Unlock()
}
