package main

import (
	"bytes"
	"encoding/json"
	"io"
	"math/big"
	"minigw/core"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPriceFixtures(t *testing.T) {
	var f struct {
		Rates []struct {
			Name                string
			OpenRouter          bool
			Price               map[string]any
			Input, Output       string
			TokensIn, TokensOut int64
			Nanos               *int64
		}
		Deployments []any
		PublicMap   map[string]any
		Mapped      map[string]core.PriceRates
		Usage       []struct {
			Body     string
			Unpriced bool
		}
	}
	d := json.NewDecoder(bytes.NewReader(srcFile(t, "testdata/pricing.json")))
	d.UseNumber()
	if err := d.Decode(&f); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.Rates {
		t.Run(c.Name, func(t *testing.T) {
			p := priceRates(c.Price, c.OpenRouter)
			if c.Input == "" {
				if p != nil {
					t.Fatal(p)
				}
				return
			}
			if p == nil {
				t.Fatal("missing rates")
			}
			for a, b := range map[string]string{p.Input: c.Input, p.Output: c.Output} {
				x, _ := new(big.Rat).SetString(a)
				y, _ := new(big.Rat).SetString(b)
				if x.Cmp(y) != 0 {
					t.Fatal(a, b)
				}
			}
			got := p.Estimate(c.TokensIn, c.TokensOut)
			if (got == nil) != (c.Nanos == nil) || got != nil && *got != *c.Nanos {
				t.Fatalf("got %v want %v", got, c.Nanos)
			}
		})
	}
	got := deploymentPrices(f.Deployments, f.PublicMap)
	for name, want := range f.Mapped {
		if got[name] == nil || *got[name] != want {
			t.Fatalf("%s: %v", name, got[name])
		}
	}
	if got["ambiguous"] != nil || got["unmapped"] != nil {
		t.Fatal(got)
	}
	for _, c := range f.Usage {
		for split := 0; split <= len(c.Body); split++ {
			var m usageMetrics
			m.feed([]byte(c.Body[:split]))
			m.feed([]byte(c.Body[split:]))
			if m.cacheUsed != c.Unpriced {
				t.Fatalf("usage %s, split %d", c.Body, split)
			}
		}
	}
}

func TestCatalogPricesWithKnownWindows(t *testing.T) {
	keys := fakeKeys(t)
	keys["LiteLLM"] = gwKey
	var infoCalls atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			io.WriteString(w, `{"data":[{"id":"routed","context_length":10000}]}`)
		case "/model/info":
			infoCalls.Add(1)
			io.WriteString(w, `{"data":[{"model_name":"routed","model_info":{"input_cost_per_token":0.000002,"output_cost_per_token":0.000006}}]}`)
		default:
			t.Error("unexpected price request", r.URL)
			http.NotFound(w, r)
		}
	}))
	defer up.Close()
	s, _ := testServer(t, func(c *serveConfig) {
		c.Gateways = []serveGateway{{Name: "LiteLLM", BaseURL: up.URL, Insecure: true, Aliases: "alias = routed, 10k"}}
	})
	g := serveGateway{Name: "LiteLLM", BaseURL: up.URL, Insecure: true, Aliases: "alias = routed, 10k"}
	models := s.gatewayModels(g, true)
	if len(models) != 1 || models[0].price == nil || infoCalls.Load() != 1 {
		t.Fatalf("%+v calls %d", models, infoCalls.Load())
	}
	routes := s.gatewayRoutes(g)
	if len(routes) != 1 || routes[0].price == nil || routes[0].price.UpstreamModel != "routed" {
		t.Fatal(routes)
	}
	frozen := routes[0].price
	// A failed refresh returns the old catalog immediately and keeps its snapshot.
	up.Close()
	s.gateways.mu.Lock()
	s.gateways.m[g.Name].at = time.Now().Add(-time.Hour)
	s.gateways.mu.Unlock()
	models = s.gatewayModels(g, false)
	if models[0].price.ID != frozen.ID {
		t.Fatal("lost last good price")
	}
	s.gateways.mu.Lock()
	done := s.gateways.m[g.Name].loading
	s.gateways.mu.Unlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("refresh not bounded")
	}
	if s.gatewayModels(g, false)[0].price.ID != frozen.ID {
		t.Fatal("failure replaced snapshot")
	}
}

func TestUsageEstimatedThroughGateway(t *testing.T) {
	keys := fakeKeys(t)
	keys["OpenRouter"] = gwKey
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			io.WriteString(w, `{"data":[{"id":"routed","context_length":10000,"pricing":{"prompt":"0.000002","completion":"0.000006"}}]}`)
			return
		}
		io.WriteString(w, `{"usage":{"input_tokens":1000,"output_tokens":500}}`)
	}))
	defer up.Close()
	f := newCoreFx(t, func(c *serveConfig) {
		c.Gateways = []serveGateway{{Name: "OpenRouter", BaseURL: up.URL, Insecure: true, Aliases: "alias = routed"}}
	})
	f.s.loadGateways()
	conn := startLoopback(t, f.s)
	r, _ := http.NewRequest("POST", conn.URL+"/v1/messages", strings.NewReader(`{"model":"claude-alias","messages":[]}`))
	r.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	f.s.api.FlushRecorder()
	var cost int64
	var source, mode, id string
	if err = f.s.core.DB().QueryRow(`SELECT cost_usd_nanos,cost_source,billing_mode,price_snapshot_id FROM usage_events`).Scan(&cost, &source, &mode, &id); err != nil || cost != 5000000 || source != "estimated" || mode != "metered" || id == "" {
		t.Fatal(cost, source, mode, id, err)
	}
}

func TestMalformedPriceRefreshKeepsLastGood(t *testing.T) {
	keys := fakeKeys(t)
	keys["LiteLLM"] = gwKey
	var malformed atomic.Bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/models" {
			io.WriteString(w, `{"data":[{"id":"base","context_length":10000}]}`)
		} else if malformed.Load() {
			io.WriteString(w, `{"error":"temporarily unavailable"}`)
		} else {
			io.WriteString(w, `{"data":[{"model_name":"base","model_info":{"input_cost_per_token":0.000002,"output_cost_per_token":0.000006}}]}`)
		}
	}))
	defer up.Close()
	s, _ := testServer(t, nil)
	g := serveGateway{Name: "LiteLLM", BaseURL: up.URL, Insecure: true}
	first := s.gatewayModels(g, true)
	if len(first) != 1 || first[0].price == nil {
		t.Fatal(first)
	}
	malformed.Store(true)
	s.gateways.mu.Lock()
	s.gateways.m[g.Name].at = time.Now().Add(-time.Hour)
	s.gateways.mu.Unlock()
	s.gatewayModels(g, false)
	s.gateways.mu.Lock()
	done := s.gateways.m[g.Name].loading
	s.gateways.mu.Unlock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("refresh not bounded")
	}
	if got := s.gatewayModels(g, false); len(got) != 1 || got[0].price == nil || got[0].price.ID != first[0].price.ID {
		t.Fatal("malformed refresh discarded last good price", got)
	}
}

func TestInferenceDoesNotWaitForPrices(t *testing.T) {
	keys := fakeKeys(t)
	keys["LiteLLM"] = gwKey
	blocked, release := make(chan struct{}, 10), make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		blocked <- struct{}{}
		<-release
		io.WriteString(w, `{"data":[]}`)
	}))
	defer up.Close()
	defer close(release)
	s, _ := testServer(t, func(c *serveConfig) {
		c.Gateways = []serveGateway{{Name: "LiteLLM", BaseURL: up.URL, Insecure: true, Aliases: "alias = base"}}
	})
	done := make(chan bool, 1)
	go func() {
		_, m, ok := s.gatewayRoute("claude-alias", true)
		done <- ok && m.price == nil && m.id == "base"
	}()
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("cold manual route unavailable")
		}
	case <-time.After(time.Second):
		t.Fatal("inference waited for price discovery")
	}
	// Repeated lookups share the background load.
	s.gatewayRoute("claude-alias", true)
	select {
	case <-blocked:
	case <-time.After(time.Second):
		t.Fatal("no background refresh")
	}
	select {
	case <-blocked:
		t.Fatal("duplicate background refresh")
	default:
	}
}

func TestCacheCountersAndOversizedUsage(t *testing.T) {
	var m usageMetrics
	m.feed([]byte(`{"usage":{"input_tokens":1,"output_tokens":2,"cache_read_input_tokens":10,"cache_creation_input_tokens":20}}`))
	if m.cacheRead == nil || *m.cacheRead != 10 || m.cacheWrite == nil || *m.cacheWrite != 20 || !m.cacheUsed {
		t.Fatal(m)
	}
	m.feed([]byte(`{"usage":{"cache_read_input_tokens":5,"cache_creation_input_tokens":10}}`))
	if *m.cacheRead != 10 || *m.cacheWrite != 20 {
		t.Fatal(m)
	}
	var large usageMetrics
	large.feed([]byte(`{"usage":{"padding":"` + strings.Repeat("x", 65536) + `","input_tokens":1}}`))
	if !large.cacheUsed {
		t.Fatal("oversized usage must disable estimates")
	}
}
