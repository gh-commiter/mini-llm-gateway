package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

func TestRouteBodyAndMerge(t *testing.T) {
	names := []string{"mini", "win"}
	b, n := routeBody([]byte(`{"model":"claude-cli-opus@win","max_tokens":5}`), names)
	if n != "win" || !strings.Contains(string(b), `"model":"claude-cli-opus"`) || !strings.Contains(string(b), `"max_tokens":5`) {
		t.Fatalf("route: %s %s", n, b)
	}
	for _, m := range []string{"claude-opus-4@20250514", "x@other", "plain"} { // ni profil: model ostane cel
		if b, n := routeBody([]byte(`{"model":"`+m+`"}`), names); n != "" || string(b) != `{"model":"`+m+`"}` {
			t.Fatalf("%s: %s %s", m, n, b)
		}
	}
	a := []map[string]any{{"id": "b", "display_name": "B"}, {"id": "a"}}
	w := []map[string]any{{"id": "a", "display_name": "A"}}
	var ids []string
	for _, m := range mergeModels([][]map[string]any{a, w}, names) {
		ids = append(ids, m["id"].(string)+"|"+fmtAny(m["display_name"]))
	}
	if !slices.Equal(ids, []string{"b|B", "a|", "b@mini|B @mini", "a@mini|", "a@win|A @win"}) {
		t.Fatalf("merge: %v", ids)
	}
	if m := mergeModels([][]map[string]any{nil, w}, names); len(m) != 1 || m[0]["id"] != "a@win" { // privzeti nedosegljiv
		t.Fatalf("default down: %v", m)
	}
	if mergeModels([][]map[string]any{nil, nil}, names) != nil {
		t.Fatal("none reachable")
	}
}

func fmtAny(v any) string { s, _ := v.(string); return s }

// Usmerjevalnik: skrivnost, združeni modeli, pripona → prehod in njegov žeton, mapa pri drugem ob prvi zahtevi.
func TestGatewayMux(t *testing.T) {
	type seen struct{ auth, model, share string }
	gateway := func(models string, got chan seen) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/models" {
				io.WriteString(w, `{"data":[`+models+`]}`)
				return
			}
			var b struct{ Model string }
			json.NewDecoder(r.Body).Decode(&b)
			got <- seen{r.Header.Get("Authorization"), b.Model, r.Header.Get("X-Claude-Mini-Share")}
			io.WriteString(w, `{"ok":true}`)
		}))
	}
	g1, g2 := make(chan seen, 4), make(chan seen, 4)
	s1, s2 := gateway(`{"id":"claude-siri-ai"}`, g1), gateway(`{"id":"claude-cli-opus"}`, g2)
	defer s1.Close()
	defer s2.Close()
	dead := httptest.NewServer(nil)
	dead.Close()
	var shares atomic.Int32
	win := &muxTarget{name: "win", conn: connection{URL: s2.URL, Token: "tok-win"}, share: func() bool { shares.Add(1); return true }}
	m := newGatewayMux([]*muxTarget{{name: "mini", conn: connection{URL: s1.URL, Token: "tok-mini"}}, win,
		{name: "off", conn: connection{URL: dead.URL, Token: "x"}}})
	if err := m.start(); err != nil {
		t.Fatal(err)
	}
	do := func(method, path, secret, body string, hdr ...string) (int, string) {
		r, _ := http.NewRequest(method, m.url()+path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+secret)
		if len(hdr) == 2 {
			r.Header.Set(hdr[0], hdr[1])
		}
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if st, _ := do("GET", "/v1/models", "wrong", ""); st != 401 {
		t.Fatalf("no secret: %d", st)
	}
	st, b := do("GET", "/v1/models", m.secret, "")
	for _, id := range []string{`"claude-siri-ai"`, `"claude-siri-ai@mini"`, `"claude-cli-opus@win"`} {
		if st != 200 || !strings.Contains(b, id) || strings.Contains(b, "@off") || strings.Contains(b, `"claude-cli-opus"`) {
			t.Fatalf("models: %d %s", st, b)
		}
	}
	for range 2 {
		do("POST", "/v1/messages", m.secret, `{"model":"claude-cli-opus@win"}`, "X-Claude-Mini-Share", "1")
		if g := <-g2; g != (seen{"Bearer tok-win", "claude-cli-opus", "1"}) {
			t.Fatalf("win: %+v", g)
		}
	}
	if shares.Load() != 1 {
		t.Fatalf("share registered %d times", shares.Load())
	}
	do("POST", "/v1/messages", m.secret, `{"model":"claude-siri-ai"}`, "X-Claude-Mini-Share", "1")
	if g := <-g1; g != (seen{"Bearer tok-mini", "claude-siri-ai", "1"}) {
		t.Fatalf("default: %+v", g)
	}
	// Brez registrirane mape (drug prehod) glave x-claude-mini-share ni.
	m2 := newGatewayMux([]*muxTarget{{name: "mini", conn: connection{URL: s1.URL, Token: "tok-mini"}},
		{name: "win", conn: connection{URL: s2.URL, Token: "tok-win"}}})
	m2.start()
	m = m2
	do("POST", "/v1/messages/count_tokens", m.secret, `{"model":"claude-cli-opus@win"}`, "X-Claude-Mini-Share", "1")
	if g := <-g2; g.share != "" || g.model != "claude-cli-opus" {
		t.Fatalf("unshared: %+v", g)
	}
	if st, b := do("POST", "/v1/messages", m.secret, `{"model":"x@win"}`); st != 200 || b != `{"ok":true}` {
		t.Fatalf("reply: %d %s", st, b)
	}
	<-g2
}

func TestGatewayMuxUnreachable(t *testing.T) {
	dead := httptest.NewServer(nil)
	dead.Close()
	m := newGatewayMux([]*muxTarget{{name: "a", conn: connection{URL: dead.URL}}, {name: "b", conn: connection{URL: dead.URL}}})
	m.start()
	r, _ := http.NewRequest("POST", m.url()+"/v1/messages", strings.NewReader(`{"model":"x@b"}`))
	r.Header.Set("x-api-key", m.secret)
	resp, err := http.DefaultClient.Do(r)
	if err != nil || resp.StatusCode != 502 {
		t.Fatalf("dead: %v %v", resp, err)
	}
	r, _ = http.NewRequest("GET", m.url()+"/v1/models", nil)
	r.Header.Set("x-api-key", m.secret)
	if resp, _ := http.DefaultClient.Do(r); resp.StatusCode != 502 {
		t.Fatalf("models none: %d", resp.StatusCode)
	}
}
