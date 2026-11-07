package main

// Več prehodov v eni seji (#17): Claude Code govori z lokalnim usmerjevalnikom na 127.0.0.1, ta po priponi modela
// (claude-cli-opus@win) izbere profil, pripono odstrani in zahtevo preda prehodu tega profila (prek njegovega
// šifriranega proxyja). /v1/models združi sezname vseh dosegljivih prehodov. Z enim profilom ga ni (pot kot prej).

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type muxTarget struct {
	name  string
	conn  connection // usmerjena: lokalni šifrirani proxy ali prehod na tem računalniku
	proxy *httputil.ReverseProxy
	// share: registrira mapo pri tem prehodu ob prvi zahtevi zanj (nil = privzeti profil, ki jo registrira main).
	share  func() bool
	once   sync.Once
	shared atomic.Bool
}

type gatewayMux struct {
	targets []*muxTarget // privzeti prvi
	secret  string       // ANTHROPIC_AUTH_TOKEN claude: drug lokalni proces brez njega ne dobi žetonov prehodov
	ln      net.Listener
}

func newGatewayMux(targets []*muxTarget) *gatewayMux {
	m := &gatewayMux{targets: targets, secret: randomToken()}
	for i, t := range targets {
		u, _ := url.Parse(t.conn.URL)
		t, first := t, i == 0
		t.proxy = &httputil.ReverseProxy{
			Rewrite: func(pr *httputil.ProxyRequest) {
				pr.SetURL(u)
				pr.Out.Header.Set("Authorization", "Bearer "+t.conn.Token)
				pr.Out.Header.Del("X-Api-Key")
				if !first { // privzeti dobi glavo iz ANTHROPIC_CUSTOM_HEADERS; drugi le, če je mapa pri njem registrirana
					pr.Out.Header.Del("X-Claude-Mini-Share")
					if t.shared.Load() {
						pr.Out.Header.Set("X-Claude-Mini-Share", "1")
					}
				}
			},
			FlushInterval: -1, // SSE sproti
			ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
				muxError(w, 502, "gateway "+t.name+" is not reachable (minigw status)")
			},
		}
	}
	return m
}

func muxError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": map[string]string{"type": "api_error", "message": msg}})
}

// start: stalen port za ta nabor profilov (Claude Code hrani modele skupaj z ANTHROPIC_BASE_URL), sicer naključen.
func (m *gatewayMux) start() error {
	h := sha256.New()
	for _, t := range m.targets {
		fmt.Fprintf(h, "%s=%s;", t.name, t.conn.URL)
	}
	sum := h.Sum(nil)
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", 30000+(int(sum[0])<<8|int(sum[1]))%10000))
	if err != nil {
		if ln, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
			return err
		}
	}
	m.ln = ln
	go (&http.Server{Handler: m, ReadHeaderTimeout: 30 * time.Second}).Serve(ln)
	return nil
}

func (m *gatewayMux) url() string { return "http://" + m.ln.Addr().String() }

func (m *gatewayMux) names() []string {
	var n []string
	for _, t := range m.targets {
		n = append(n, t.name)
	}
	return n
}

func (m *gatewayMux) authorized(r *http.Request) bool {
	got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if got == "" {
		got = r.Header.Get("X-Api-Key")
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(m.secret)) == 1
}

func (m *gatewayMux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !m.authorized(r) {
		muxError(w, 401, "invalid token for minigw's local proxy")
		return
	}
	if r.Method == "GET" && r.URL.Path == "/v1/models" {
		m.models(w)
		return
	}
	t := m.targets[0]
	if r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/v1/messages") {
		body, err := io.ReadAll(io.LimitReader(r.Body, maxSyncFile+1))
		if err != nil || len(body) > maxSyncFile {
			muxError(w, 413, "request body too large")
			return
		}
		nb, name, model := routeBody(body, m.names())
		for _, x := range m.targets {
			if x.name == name {
				t, body = x, nb
			}
		}
		r.Body, r.ContentLength = io.NopCloser(bytes.NewReader(body)), int64(len(body))
		r.Header.Del("Content-Length")
		if t.share != nil && isAgentModel(model) { // mapa le ob prvi zahtevi za model agenta (ne npr. haiku za naslov)
			t.once.Do(func() { t.shared.Store(t.share()) })
		}
	}
	t.proxy.ServeHTTP(w, r)
}

// isAgentModel: modeli agentov, ki delajo v mapi odjemalca (claude-cli-*, claude-codex*, claude-grok*, claude-copilot*).
func isAgentModel(m string) bool {
	for _, p := range []string{"claude-cli-", "claude-codex", "claude-grok", "claude-copilot"} {
		if strings.HasPrefix(m, p) {
			return true
		}
	}
	return false
}

// routeBody: model s pripono profila → telo brez pripone, ime profila in model brez pripone; sicer telo, kot je, "".
func routeBody(body []byte, names []string) ([]byte, string, string) {
	var m map[string]json.RawMessage
	var model string
	if json.Unmarshal(body, &m) != nil || json.Unmarshal(m["model"], &model) != nil {
		return body, "", ""
	}
	bare, name := splitModel(model, names)
	if name == "" {
		return body, "", model
	}
	m["model"], _ = json.Marshal(bare)
	nb, err := json.Marshal(m)
	if err != nil {
		return body, "", model
	}
	return nb, name, bare
}

// models: seznami vseh prehodov hkrati; nedosegljiv izpade (minigw status ga pokaže).
func (m *gatewayMux) models(w http.ResponseWriter) {
	lists := make([][]map[string]any, len(m.targets))
	var wg sync.WaitGroup
	for i, t := range m.targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lists[i] = fetchModels(t.conn)
		}()
	}
	wg.Wait()
	data := mergeModels(lists, m.names())
	if data == nil {
		muxError(w, 502, "no gateway is reachable (minigw status)")
		return
	}
	out := map[string]any{"data": data, "has_more": false}
	if len(data) > 0 {
		out["first_id"], out["last_id"] = data[0]["id"], data[len(data)-1]["id"]
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func fetchModels(c connection) []map[string]any {
	st, data := call(c, "GET", "/v1/models", nil, nil, 10*time.Second)
	var l struct {
		Data []map[string]any `json:"data"`
	}
	if st != 200 || json.Unmarshal(data, &l) != nil {
		return nil
	}
	if l.Data == nil {
		l.Data = []map[string]any{}
	}
	return l.Data
}

// mergeModels: vsak model s pripono @profil; privzeti (prvi) še brez pripone. nil = noben prehod ni odgovoril.
func mergeModels(lists [][]map[string]any, names []string) []map[string]any {
	var out []map[string]any
	seen := map[string]bool{}
	add := func(m map[string]any, id, suffix string) {
		if seen[id] {
			return
		}
		seen[id] = true
		c := map[string]any{}
		for k, v := range m {
			c[k] = v
		}
		c["id"] = id
		if dn, _ := m["display_name"].(string); dn != "" && suffix != "" {
			c["display_name"] = dn + " @" + suffix
		}
		out = append(out, c)
	}
	reached := false
	for i, l := range lists {
		if l == nil {
			continue
		}
		reached = true
		ids := make([]map[string]any, 0, len(l)) // vrstni red prehoda ostane
		for _, m := range l {
			if id, _ := m["id"].(string); id != "" {
				ids = append(ids, m)
			}
		}
		for _, m := range ids {
			if i == 0 {
				add(m, m["id"].(string), "")
			}
		}
		for _, m := range ids {
			add(m, m["id"].(string)+"@"+names[i], names[i])
		}
	}
	if !reached {
		return nil
	}
	if out == nil {
		out = []map[string]any{}
	}
	return out
}
