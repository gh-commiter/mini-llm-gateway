package main

// minigw serve: prehodi z Anthropic /v1/messages (OpenRouter, LiteLLM), kot Router.passthrough in Catalog v
// RouterCore/Router.swift. Modeli se odkrijejo pri upstreamu (/v1/models, okno iz context_length ali LiteLLM
// /model/info); objavljeni ID je kot na Macu (ProviderConfig.publishedID). Ključ ni v serve.json (store_*.go).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"minigw/core"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

type serveGateway struct {
	Name     string   `json:"name"`
	BaseURL  string   `json:"baseURL"`            // brez /v1
	Models   []string `json:"models"`             // null = vsi odkriti; seznam = le ti (ID pri upstreamu)
	Insecure bool     `json:"insecure,omitempty"` // http do localhost ali zasebnega IP (gateway add --insecure)
	Disabled bool     `json:"disabled,omitempty"` // izklopljen v nastavitvah (privzeto vklopljen)
	Aliases  string   `json:"aliases,omitempty"`  // vrstice »alias = upstream ID, okno«; določijo modele namesto tabele (kot na Macu)
	// Tools: the highest tool level the models of this provider may get in the client's Claude Code (none, read, edit; empty = read);
	// ModelTools: the same per model (the /model name), which wins. The owner sets it (minigw gw provider tools), see tools.go.
	Tools      string            `json:"tools,omitempty"`
	ModelTools map[string]string `json:"modelTools,omitempty"`
}

// toolsCap: the highest tool level for the model route (the /model name or its claude- ID).
func (g serveGateway) toolsCap(route string) string {
	for _, k := range []string{route, strings.TrimPrefix(route, "claude-")} {
		if _, ok := toolsRank[g.ModelTools[k]]; ok {
			return g.ModelTools[k]
		}
	}
	if _, ok := toolsRank[g.Tools]; ok {
		return g.Tools
	}
	return "read"
}

// builtinGateways: kot vgrajena ponudnika na Macu: vedno na seznamu, izklopljena, dokler nimata ključa; izklop da,
// odstranitev in preimenovanje ne. Obstoječ ponudnik z enakim imenom (ne glede na velikost črk) je vgrajeni (ključ ostane).
var builtinGateways = []serveGateway{
	{Name: "LiteLLM", BaseURL: "http://localhost:4000", Insecure: true, Disabled: true},
	{Name: "OpenRouter", BaseURL: "https://openrouter.ai/api", Models: []string{}, Disabled: true}, // več sto modelov: izbereš sam
}

func builtinGateway(name string) bool {
	return slices.ContainsFunc(builtinGateways, func(b serveGateway) bool { return strings.EqualFold(b.Name, name) })
}

// ensureBuiltinGateways: manjkajoča vgrajena ponudnika na začetek seznama (kot na Macu).
func ensureBuiltinGateways(c *serveConfig) {
	var add []serveGateway
	for _, b := range builtinGateways {
		if !slices.ContainsFunc(c.Gateways, func(g serveGateway) bool { return strings.EqualFold(g.Name, b.Name) }) {
			b.Models = slices.Clone(b.Models)
			add = append(add, b)
		}
	}
	c.Gateways = append(add, c.Gateways...)
}

type aliasPair struct {
	alias, upstream string
	window          int
}

// aliasPairs: »alias = upstream, 128k« ali le »upstream«; prazne vrstice in »#« preskoči (kot ProviderConfig.aliasPairs).
func aliasPairs(text string) []aliasPair {
	var out []aliasPair
	for _, line := range strings.Split(text, "\n") {
		if line = strings.TrimSpace(line); line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		left, win, _ := strings.Cut(line, ",")
		alias, up, ok := strings.Cut(left, "=")
		alias, up = strings.TrimSpace(alias), strings.TrimSpace(up)
		if !ok || alias == "" || up == "" {
			alias, up = strings.TrimSpace(left), strings.TrimSpace(left)
		}
		out = append(out, aliasPair{alias, up, windowTokens(strings.TrimSpace(win))})
	}
	return out
}

// windowTokens: »128k«, »1M«, »200000« → število tokenov (0 = neznano).
func windowTokens(s string) int {
	t := strings.ToLower(s)
	mul := 1.0
	switch {
	case strings.HasSuffix(t, "k"):
		mul, t = 1e3, strings.TrimSuffix(t, "k")
	case strings.HasSuffix(t, "m"):
		mul, t = 1e6, strings.TrimSuffix(t, "m")
	}
	f, err := strconv.ParseFloat(t, 64)
	if err != nil {
		return 0
	}
	return int(f * mul)
}

func (g serveGateway) enabled(upstream string) bool {
	return g.Models == nil || slices.Contains(g.Models, upstream)
}

// keyGet, keySet, keyDel: secretstore.go (one interface, backend chosen once); tests replace them.

// keyNames: the names of the stored gateway secrets (only to find the "upstream:<name>" ones when minigw is uninstalled).
var keyNames = osKeyNames

var validGatewayName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,31}$`)

// checkGatewayURL: https; http le do localhost ali zasebnega IP in le z --insecure. Brez uporabnika, poizvedbe in /v1.
func checkGatewayURL(raw string, insecure bool) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("invalid provider URL: " + raw)
	}
	switch u.Scheme {
	case "https":
	case "http":
		// Lokalno: localhost, zasebni ali loopback IP, ime .local (mDNS); isto pravilo kot na Macu. Ime brez pike ne:
		// iskalna domena DNS ga lahko naredi javnega.
		h := strings.ToLower(u.Hostname())
		ip := net.ParseIP(h)
		local := h == "localhost" || strings.HasSuffix(h, ".local") || (ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast())) ||
			(containerMode() && singleLabel(h)) // Compose service name (http://litellm:4000): private at add time and at every dial (gwDial)
		if !local {
			return "", errors.New("provider URL must be https (http only to localhost, a private IP or a .local name, with --insecure)")
		}
		if !insecure {
			return "", errors.New("http sends the API key unencrypted: add --insecure for a provider on localhost, a private IP or a .local name")
		}
	default:
		return "", errors.New("provider URL must be https: " + raw)
	}
	return strings.TrimSuffix(strings.TrimSuffix(u.String(), "/"), "/v1"), nil
}

// singleLabel: a host name without a dot that is not an IP address (a Docker Compose service name).
func singleLabel(h string) bool {
	return h != "" && !strings.ContainsAny(h, ".:") && net.ParseIP(h) == nil && h != "localhost"
}

// privateIP: a single-label service name may resolve only to a private address. Link-local (169.254.0.0/16, fe80::/10,
// such as a cloud metadata service), loopback, unspecified and multicast are refused; IPv4-mapped forms are judged as IPv4.
func privateIP(ip net.IP) bool {
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsLoopback() || ip.IsUnspecified() || ip.IsMulticast() {
		return false
	}
	return ip.IsPrivate()
}

// resolveHost: the resolver of provider names; tests replace it.
var resolveHost = func(ctx context.Context, h string) ([]net.IP, error) {
	a, err := net.DefaultResolver.LookupIPAddr(ctx, h)
	ips := make([]net.IP, len(a))
	for i := range a {
		ips[i] = a[i].IP
	}
	return ips, err
}

// privateHost: a single-label name must resolve, and only to private addresses (container mode); returns the one to use.
func privateHost(ctx context.Context, h string) (net.IP, error) {
	ips, err := resolveHost(ctx, h)
	if err != nil || len(ips) == 0 {
		return nil, fmt.Errorf("the name %s does not resolve (is the service on a shared Docker network?)", h)
	}
	for _, ip := range ips {
		if !privateIP(ip) {
			return nil, fmt.Errorf("the name %s resolves to a public address (%s): refused", h, ip)
		}
	}
	return ips[0], nil
}

// checkGatewayName (add and change of a provider, not the start: the service may start later): in container mode a
// single-label http host must resolve to private addresses now.
func checkGatewayName(base string) error {
	u, err := url.Parse(base)
	if err != nil || !containerMode() || u.Scheme != "http" || !singleLabel(strings.ToLower(u.Hostname())) {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err = privateHost(ctx, strings.ToLower(u.Hostname()))
	return err
}

// gwDial: every connection to a provider. A single-label name (container mode) is resolved again here and followed only
// while it resolves to private addresses: a name that later points to a public address is not dialled.
func gwDial(ctx context.Context, network, addr string) (net.Conn, error) {
	if host, port, err := net.SplitHostPort(addr); err == nil && containerMode() && singleLabel(strings.ToLower(host)) {
		ip, err := privateHost(ctx, strings.ToLower(host))
		if err != nil {
			return nil, err
		}
		return dialChecked(ctx, network, net.JoinHostPort(ip.String(), port))
	}
	return dialChecked(ctx, network, addr)
}

// dialChecked: the one real connect of gwDial (the address is already an IP that passed the check); tests replace it
// so that no test needs a listener on a private address.
var dialChecked = func(ctx context.Context, network, addr string) (net.Conn, error) {
	d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	return d.DialContext(ctx, network, addr)
}

// publishedID: v /model v Claude Code so le ID-ji, ki se začnejo s »claude-« (kot na Macu).
func publishedID(upstream string) string {
	if strings.HasPrefix(strings.ToLower(upstream), "claude-") {
		return upstream
	}
	return "claude-" + upstream
}

type gwModel struct {
	priceRefreshFailed bool
	price              *core.PriceSnapshot
	id                 string // pri upstreamu
	window             int    // 0 = neznano
	name               string // alias (ime za /model); prazno = id
}

func (m gwModel) route() string {
	if m.name != "" {
		return m.name
	}
	return m.id
}

// gatewayRoutes: modeli prehoda za /model in usmerjanje: izklopljen prehod nobenega; z aliasi ti (okno iz alias ali
// odkritih modelov), sicer vklopljeni odkriti modeli.
func (s *server) gatewayRoutes(g serveGateway) []gwModel {
	if g.Disabled {
		return nil
	}
	found := s.gatewayModels(g, false)
	if pairs := aliasPairs(g.Aliases); len(pairs) > 0 {
		win := map[string]int{}
		prices := map[string]*core.PriceSnapshot{}
		for _, m := range found {
			win[m.id] = m.window
			prices[m.id] = m.price
		}
		var out []gwModel
		for _, p := range pairs {
			w := p.window
			if w == 0 {
				w = win[p.upstream]
			}
			out = append(out, gwModel{id: p.upstream, window: w, name: p.alias, price: prices[p.upstream]})
		}
		return out
	}
	var out []gwModel
	for _, m := range found {
		if g.enabled(m.id) {
			out = append(out, m)
		}
	}
	return out
}

// noProxyServiceName: an http provider (private host, .local, container service name: http is allowed only with --insecure) and a
// single-label service name (container mode) are dialled directly, never through the environment proxy: for http the key would go
// unencrypted to the proxy, and the proxy would resolve the name without gwDial's check. https providers keep the environment proxy.
func noProxyServiceName(r *http.Request) (*url.URL, error) {
	if strings.EqualFold(r.URL.Scheme, "http") || containerMode() && singleLabel(strings.ToLower(r.URL.Hostname())) {
		return nil, nil
	}
	return http.ProxyFromEnvironment(r)
}

// gwClients: brez preusmeritev (ključ ne gre drugam), roki za povezavo in glavo odgovora. Za http (--insecure) brez
// HTTP_PROXY: ključ bi šel nešifriran prek posrednika; ime storitve gre vedno neposredno (noProxyServiceName).
var gwClients = func() [2]*http.Client {
	var out [2]*http.Client
	for i := range out {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.DialContext = gwDial
		t.ResponseHeaderTimeout = 10 * time.Minute // odgovor brez toka pride šele na koncu
		if i == 1 {
			t.Proxy = nil
		} else {
			t.Proxy = noProxyServiceName
		}
		out[i] = &http.Client{Transport: t, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	return out
}()

func (g serveGateway) client() *http.Client {
	if g.Insecure {
		return gwClients[1]
	}
	return gwClients[0]
}

func gwFetch(ctx context.Context, g serveGateway, key, path string) (map[string]any, error) {
	r, _ := http.NewRequestWithContext(ctx, "GET", g.BaseURL+path, nil)
	if key != "" {
		r.Header.Set("authorization", "Bearer "+key)
	}
	resp, err := g.client().Do(r)
	if err != nil {
		var ue *url.Error // the URL of the request (and any credentials in it) is not part of what is shown or logged
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, errors.New("cannot connect: " + err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			return nil, fmt.Errorf("HTTP %d from %s: check the API key", resp.StatusCode, path)
		}
		return nil, fmt.Errorf("HTTP %d from %s", resp.StatusCode, path)
	}
	var obj map[string]any
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 16<<20))
	decoder.UseNumber()
	if err := decoder.Decode(&obj); err != nil {
		return nil, errors.New("unexpected response from " + path)
	}
	return obj, nil
}

// discoverGateway: modeli in okna (kot Discovery.load); LiteLLM ima okna v /model/info.
func discoverGateway(g serveGateway, key string) ([]gwModel, error) {
	// rok za cel prenos (OpenRouter: več sto modelov, ~1 MB); zahteve čakajo največ 2 s (gatewayModels)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	list, err := gwFetch(ctx, g, key, "/v1/models")
	if err != nil {
		return nil, err
	}
	items, ok := list["data"].([]any)
	if !ok {
		return nil, errors.New("unexpected response from /v1/models")
	}
	window := func(m map[string]any, keys ...string) int {
		for _, k := range keys {
			if f, ok := m[k].(json.Number); ok {
				n, _ := f.Int64()
				return int(n)
			}
		}
		return 0
	}
	var out []gwModel
	missing := false
	for _, it := range items {
		m, _ := it.(map[string]any)
		id, _ := m["id"].(string)
		if id == "" || strings.Contains(id, "*") { // LiteLLM wildcard ni model
			continue
		}
		w := window(m, "context_length", "max_input_tokens", "context_window")
		missing = missing || w == 0
		pricing, _ := m["pricing"].(map[string]any)
		out = append(out, gwModel{id: id, window: w, price: snapshot(g.Name, id, "/v1/models", priceRates(pricing, true))})
	}
	if missing || strings.EqualFold(g.Name, "LiteLLM") {
		info, err := gwFetch(ctx, g, key, "/model/info")
		if err != nil && strings.EqualFold(g.Name, "LiteLLM") {
			for i := range out {
				out[i].priceRefreshFailed = true
			}
		}
		if err == nil {
			windows := map[string]int{}
			prices := map[string]*core.PriceSnapshot{}
			data, _ := info["data"].([]any)
			if data == nil && strings.EqualFold(g.Name, "LiteLLM") {
				for i := range out {
					out[i].priceRefreshFailed = true
				}
			}
			rates := deploymentPrices(data)
			for _, it := range data {
				m, _ := it.(map[string]any)
				name, _ := m["model_name"].(string)
				mi, _ := m["model_info"].(map[string]any)
				windows[name] = window(mi, "max_input_tokens")
				prices[name] = snapshot(g.Name, name, "/model/info", rates[name])
			}
			for i := range out {
				if p := prices[out[i].id]; p != nil {
					out[i].price = p
				}
				if out[i].window == 0 {
					out[i].window = windows[out[i].id]
				}
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out, nil
}

// gwCatalog: odkriti modeli po prehodih; svež 5 minut, ob napaki ostane zadnji delujoči seznam.
type gwCatalog struct {
	mu sync.Mutex
	m  map[string]*gwEntry
}

type gwEntry struct {
	models  []gwModel
	ok      bool
	at      time.Time
	failed  time.Time // zadnja napaka: nov poskus šele po 45 s
	reason  string    // why the last load failed (no key, cannot connect, HTTP status): shown in the status, never a key or a URL
	loading chan struct{}
}

// gatewayModels: zadnji znani seznam takoj (star seznam se osveži v ozadju). Le pri prvem nalaganju in z wait počaka
// največ 2 s (discovery v Claude Code ima 3 s), da mrtev prehod ne zadrži vsake zahteve.
func (s *server) gatewayModels(g serveGateway, wait bool) []gwModel {
	c := &s.gateways
	c.mu.Lock()
	if c.m == nil {
		c.m = map[string]*gwEntry{}
	}
	e := c.m[g.Name]
	if e == nil {
		e = &gwEntry{}
		c.m[g.Name] = e
	}
	if e.ok && time.Since(e.at) < 5*time.Minute {
		models := e.models
		c.mu.Unlock()
		return models
	}
	if e.loading == nil && time.Since(e.failed) < 45*time.Second {
		models := e.models
		c.mu.Unlock()
		return models
	}
	if e.loading == nil {
		done := make(chan struct{})
		e.loading = done
		go func() {
			key, _ := keyGet(s.cfgDir, g.Name)
			var ms []gwModel
			err := errors.New("no API key: run minigw gw provider add " + g.Name)
			if key != "" {
				ms, err = discoverGateway(g, key)
			}
			c.mu.Lock()
			if err == nil {
				for i := range ms {
					if ms[i].price == nil && ms[i].priceRefreshFailed {
						for _, old := range e.models {
							if old.id == ms[i].id {
								ms[i].price = old.price
								break
							}
						}
					}
				}
				e.models, e.ok, e.at, e.failed = ms, true, time.Now(), time.Time{}
			} else {
				e.failed = time.Now()
				if key == "" { // brez ključa ni bilo klica: ključ, dodan kasneje, velja po ≈2 s (ne po 45 s)
					e.failed = e.failed.Add(-43 * time.Second)
				}
				e.reason = clean(err.Error())
				if r := []rune(e.reason); len(r) > 160 {
					e.reason = string(r[:160]) + "…"
				}
				s.log.Printf("provider %s: models not loaded: %s", g.Name, e.reason)
			}
			e.loading = nil
			c.mu.Unlock()
			close(done)
		}()
	}
	done := e.loading
	if e.ok || !wait {
		models := e.models
		c.mu.Unlock()
		return models
	}
	c.mu.Unlock()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return e.models
}

// gatewayRoute: prvi prehod v vrstnem redu nastavitev z vklopljenim modelom (ID pri upstreamu ali objavljeni ID).
// Najprej zadnji znani seznami; z wait (neznan model) še počaka na prehode, ki se nalagajo prvič.
func (s *server) gatewayRoute(model string, wait bool) (serveGateway, gwModel, bool) {
	for _, w := range []bool{false, true} {
		if w && !wait {
			break
		}
		if w {
			s.loadGateways()
		}
		for _, g := range s.conf().Gateways {
			for _, m := range s.gatewayRoutes(g) {
				if !shadowsCLI(m.route()) && (m.route() == model || publishedID(m.route()) == model) {
					return g, m, true
				}
			}
		}
	}
	return serveGateway{}, gwModel{}, false
}

// loadGateways: počaka na prehode, ki se nalagajo prvič, vse hkrati (skupaj največ 2 s).
func (s *server) loadGateways() {
	var wg sync.WaitGroup
	for _, g := range s.conf().Gateways {
		if !g.Disabled {
			wg.Go(func() { s.gatewayModels(g, true) })
		}
	}
	wg.Wait()
}

// shadowsCLI: model prehoda z imenom claude-cli-… ali claude-chat-… bi zakril ponudnika claude -p.
func shadowsCLI(upstream string) bool {
	p := publishedID(upstream)
	for _, own := range []string{codexKind.prefix, grokKind.prefix, copilotKind.prefix} { // L3: tudi imena Codex, Grok, Copilot
		if p == own || strings.HasPrefix(p, own+"-") {
			return true
		}
	}
	return strings.HasPrefix(p, cliAgentPrefix) || strings.HasPrefix(p, cliChatPrefix)
}

func (s *server) gatewayModelList() []map[string]any {
	var data []map[string]any
	seen := map[string]bool{}
	s.loadGateways()
	for _, g := range s.conf().Gateways {
		for _, m := range s.gatewayRoutes(g) {
			id := publishedID(m.route())
			if seen[id] || shadowsCLI(m.route()) { // prvi prehod z imenom zmaga
				continue
			}
			seen[id] = true
			window := ""
			if m.window > 0 {
				window = fmt.Sprintf(" · %dK", m.window/1000)
			}
			data = append(data, map[string]any{"type": "model", "id": id, "display_name": m.route() + " · " + g.Name,
				"description": g.Name + " · " + m.id + window, "created_at": "2026-01-01T00:00:00Z",
				"minigw_tools": g.toolsCap(m.route()), "minigw_provider": g.Name}) // the client refuses a level above it (tools.go)
		}
	}
	return data
}

// passthrough: zamenja model in overitev; telo (orodja ostanejo nespremenjena) in tok SSE gresta naprej sproti.
func (s *server) passthrough(w http.ResponseWriter, r *req, g serveGateway, m gwModel) int {
	if m.window > 0 && r.URL.Path == "/v1/messages" && len(r.body)/4 > m.window {
		// ponytail: ocena bajti/4 (kot na Macu); upstream count_tokens, če bo premalo natančna
		return writeError(w, r.rid, 400, fmt.Sprintf("prompt is too long: %d tokens > %d maximum", len(r.body)/4, m.window))
	}
	if level := r.h("x-claude-mini-tools"); level != "" { // the level the client's Claude Code runs with: above the owner's cap = refused
		if msg := toolsLevelRefusal(level, g.toolsCap(m.route()), g.Name, m.route()); msg != "" {
			return writeError(w, r.rid, 400, msg)
		}
	}
	select { // največ 16 hkratnih zahtev k prehodom
	case s.gwSlots <- struct{}{}:
		defer func() { <-s.gwSlots }()
	default:
		return writeError(w, r.rid, 429, "Too many provider requests at once on this gateway: try again")
	}
	key, _ := keyGet(s.cfgDir, g.Name)
	if key == "" {
		return writeError(w, r.rid, 503, "No API key configured for "+g.Name+": run minigw gw provider add "+g.Name)
	}
	var body map[string]json.RawMessage
	if json.Unmarshal(r.body, &body) != nil {
		return writeError(w, r.rid, 400, "Body must be a JSON object")
	}
	body["model"], _ = json.Marshal(m.id)
	normalizeForProvider(g.BaseURL, body)
	data, _ := json.Marshal(body)
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel()
	target := g.BaseURL + r.URL.Path
	if r.URL.Query().Get("beta") == "true" { // naprej le ?beta=true, drugih parametrov naprave ne
		target += "?beta=true"
	}
	up, _ := http.NewRequestWithContext(ctx, "POST", target, bytes.NewReader(data))
	up.Header.Set("content-type", "application/json")
	up.Header.Set("authorization", "Bearer "+key)
	for _, h := range []string{"anthropic-version", "anthropic-beta"} {
		if v := r.h(h); v != "" {
			up.Header.Set(h, v)
		}
	}
	if r.event != nil {
		r.event.UpstreamModel = m.id
		r.event.BillingMode = "metered"
		r.event.PriceSnapshot = m.price
		if m.price != nil {
			r.event.PriceSnapshotID = m.price.ID
		}
	}
	s.dispatched(r) // the upstream request goes out (usage event)
	resp, err := g.client().Do(up)
	if err != nil {
		if r.Context().Err() != nil {
			return 499
		}
		r.fail(upstreamFailKind(err, "unreachable"))
		return writeError(w, r.rid, 502, g.Name+" unreachable: "+err.Error())
	}
	defer resp.Body.Close()
	if r.event != nil {
		r.event.UpstreamStatus = resp.StatusCode
	}
	if resp.StatusCode == 404 && strings.HasSuffix(r.URL.Path, "/count_tokens") { // #78: OpenRouter nima count_tokens → ocena kot pri agentih
		return writeJSON(w, r.rid, 200, map[string]int{"input_tokens": len(r.body) / 4})
	}
	if resp.StatusCode/100 != 2 {
		return upstreamError(w, r.rid, g.Name, g.BaseURL, key, resp)
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/json"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("request-id", r.rid)
	w.WriteHeader(resp.StatusCode)
	rc := http.NewResponseController(w)
	buf := make([]byte, 16<<10)
	in := io.LimitReader(resp.Body, maxSyncFile)
	for {
		n, err := in.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return resp.StatusCode // odjemalec je odšel
			}
			rc.Flush() // dogodki SSE gredo naprej sproti
		}
		if err != nil {
			if err != io.EOF { // the upstream stream broke after the head (200): not a success
				if r.Context().Err() != nil {
					return 499
				}
				r.fail(upstreamFailKind(err, "stream"))
			}
			return resp.StatusCode
		}
	}
}

// upstreamFailKind: "timeout" when the upstream request ran into a deadline, else the given kind.
func upstreamFailKind(err error, kind string) string {
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || errors.As(err, &ne) && ne.Timeout() {
		return "timeout"
	}
	return kind
}

// upstreamError: napaka upstreama kot napaka Anthropic. 401/403 → 502: Claude Code bi ju prikazal kot napačno prijavo v router.
func upstreamError(w http.ResponseWriter, rid, name, baseURL, key string, resp *http.Response) int {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var e struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	msg := ""
	if json.Unmarshal(data, &e) == nil {
		msg = e.Error.Message
	}
	if msg == "" {
		msg = string(data)
	}
	if msg = redactSecrets(strings.TrimSpace(clean(msg)), key); len(msg) > 300 {
		msg = capUsageError(msg)
	}
	status := resp.StatusCode
	switch {
	case status == 403 && spendingLimit(baseURL, data):
		w.Header().Set("x-should-retry", "false")
		return writeAgentError(w, rid, 502, "OpenRouter: spending limit reached")
	case status == 403:
		msg, status = "access denied: "+msg, 502
	case status == 401:
		msg, status = fmt.Sprintf("HTTP %d, check the API key (minigw gw provider add %s)", status, name), 502
	case status < 400 || status > 599:
		msg, status = fmt.Sprintf("HTTP %d: %s", status, msg), 502
	}
	// 429 upstreama (omejitev ponudnika, npr. brezplačni model) brez ponavljanja, kot omejitev agenta
	return writeAgentError(w, rid, status, name+": "+msg)
}

// readKey: ključ s stdin (cev) ali s terminala brez odmeva; nikoli iz argv. Le natisljivi ASCII (gre v glavo HTTP).
// what: kaj se bere (»API key for openrouter«, »router token«).
func readKey(in io.Reader, what string) (string, error) {
	var line string
	var err error
	if f, ok := in.(*os.File); ok && isTerminal(f) {
		fmt.Fprintf(os.Stderr, "%s: ", what)
		line, err = readHidden(f)
		fmt.Fprintln(os.Stderr)
	} else {
		var b []byte
		b, err = io.ReadAll(io.LimitReader(in, 8<<10))
		line = string(b)
	}
	key := strings.TrimSpace(line)
	if key == "" {
		return "", errors.New("no " + what + " (pipe it on stdin or type it at the prompt)")
	}
	for _, ch := range key {
		if ch < 0x21 || ch > 0x7e {
			return "", errors.New("the " + what + " must be printable ASCII without spaces")
		}
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return key, nil
}

func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

// Keep the same UTF-8 byte limit as the Mac gateway.
func capUsageError(s string) string {
	for len(s) > 300 {
		_, n := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-n]
	}
	return s
}
