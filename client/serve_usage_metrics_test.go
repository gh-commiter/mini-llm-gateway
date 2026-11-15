package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestUsageMetricsFixtures(t *testing.T) {
	var cases []struct {
		Name, Body          string
		Input, Output, Cost *int64
	}
	if err := json.Unmarshal(srcFile(t, "testdata/usage-metrics.json"), &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			for split := 0; split <= len(c.Body); split++ {
				var m usageMetrics
				m.feed([]byte(c.Body[:split]))
				m.feed([]byte(c.Body[split:]))
				if !reflect.DeepEqual(m.input, c.Input) || !reflect.DeepEqual(m.output, c.Output) || !reflect.DeepEqual(m.cost, c.Cost) {
					t.Fatalf("split %d: %+v want %+v", split, m, c)
				}
			}
			var m usageMetrics
			for _, b := range []byte(c.Body) {
				m.feed([]byte{b})
			}
			if !reflect.DeepEqual(m.input, c.Input) || !reflect.DeepEqual(m.output, c.Output) || !reflect.DeepEqual(m.cost, c.Cost) {
				t.Fatalf("byte chunks: %+v", m)
			}
		})
	}
}

func TestUpstreamErrorFixtures(t *testing.T) {
	var cases []struct {
		Name, Provider, Body, Message, Key string
		Status                             int
		NoRetry                            bool
	}
	if err := json.Unmarshal(srcFile(t, "testdata/upstream-errors.json"), &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			w := httptest.NewRecorder()
			status := upstreamError(w, "rid", c.Provider, c.Key, &http.Response{StatusCode: c.Status, Body: io.NopCloser(strings.NewReader(c.Body))})
			var body struct{ Error struct{ Message string } }
			json.Unmarshal(w.Body.Bytes(), &body)
			if status != 502 || body.Error.Message != c.Message || (w.Header().Get("x-should-retry") == "false") != c.NoRetry {
				t.Fatalf("%d %s %v", status, w.Body.String(), w.Header())
			}
		})
	}
}

func TestUsageMetricsBounded(t *testing.T) {
	var m usageMetrics
	m.feed([]byte(`{"content":[{"text":"` + strings.Repeat("x", 2<<20) + `"}],"usage":{"input_tokens":1,"cost":0}}`))
	if m.input == nil || *m.input != 1 || m.cost == nil || *m.cost != 0 || len(m.capture) != 0 || len(m.word) != 0 {
		t.Fatalf("%+v", m)
	}
}

func TestUsageMetricsRecoversAfterUnsupportedSSE(t *testing.T) {
	for _, frame := range []string{
		`{"usage":{"padding":"` + strings.Repeat("x", 65536) + `","input_tokens":999,"cost":100}}`,
		strings.Repeat("[", 129),
	} {
		var m usageMetrics
		m.feed([]byte("data: " + frame + "\n\n"))
		if m.input != nil || m.cost != nil {
			t.Fatal("unsupported frame supplied metrics")
		}
		m.feed([]byte("data: {\"usage\":{\"input_tokens\":2,\"cost\":0}}\n\n"))
		if m.input == nil || *m.input != 2 || m.cost == nil || *m.cost != 0 {
			t.Fatal("later usage was lost")
		}
	}
}

func TestProviderEventMetrics(t *testing.T) {
	f := newCoreFx(t, nil)
	r := &req{Request: httptest.NewRequest("POST", "/v1/messages", nil), conn: &serveConn{}, provider: "OpenRouter"}
	f.s.admitEvent(r, "alias")
	r.event.UpstreamModel = "routed"
	f.s.dispatched(r)
	uw := &usageWriter{ResponseWriter: httptest.NewRecorder()}
	uw.Write([]byte(`{"usage":{"input_tokens":0,"output_tokens":0,"cost":0}}`))
	f.s.endEvent(r, 200, uw)
	f.s.api.FlushRecorder()
	var requested, upstream, source string
	var in, out, cost int64
	err := f.s.core.DB().QueryRow(`SELECT model,upstream_model,token_source,tokens_in,tokens_out,cost_usd_nanos FROM usage_events WHERE id=?`, r.event.ID).Scan(&requested, &upstream, &source, &in, &out, &cost)
	if err != nil || requested != "alias" || upstream != "routed" || source != "reported" || in != 0 || out != 0 || cost != 0 {
		t.Fatalf("%s %s %s %d %d %d %v", requested, upstream, source, in, out, cost, err)
	}
}

func TestUsageProviderThroughGateway(t *testing.T) {
	keys := fakeKeys(t)
	keys["OpenRouter"] = gwKey
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			io.WriteString(w, `{"data":[]}`)
			return
		}
		var body struct{ Model string }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Model != "routed" {
			t.Errorf("upstream model %q: %v", body.Model, err)
		}
		io.WriteString(w, `{"usage":{"input_tokens":0,"output_tokens":3,"cost":0.006}}`)
	}))
	defer up.Close()
	f := newCoreFx(t, func(c *serveConfig) {
		c.Gateways = []serveGateway{{Name: "OpenRouter", BaseURL: up.URL, Insecure: true, Aliases: "alias = routed"}}
	})
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
	var provider, model, upstream string
	var cost, in, out int64
	err = f.s.core.DB().QueryRow(`SELECT provider,model,upstream_model,cost_usd_nanos,tokens_in,tokens_out FROM usage_events`).Scan(&provider, &model, &upstream, &cost, &in, &out)
	if err != nil || resp.StatusCode != 200 || provider != "OpenRouter" || model != "claude-alias" || upstream != "routed" || cost != 6000000 || in != 0 || out != 3 {
		t.Fatalf("status %d, %s %s %s %d %d %d: %v", resp.StatusCode, provider, model, upstream, cost, in, out, err)
	}
}
