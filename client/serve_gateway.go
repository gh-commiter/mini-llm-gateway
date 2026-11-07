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
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"
)

type serveGateway struct {
	Name     string   `json:"name"`
	BaseURL  string   `json:"baseURL"`            // brez /v1
	Models   []string `json:"models"`             // null = vsi odkriti; seznam = le ti (ID pri upstreamu)
	Insecure bool     `json:"insecure,omitempty"` // http do localhost ali zasebnega IP (gateway add --insecure)
}

func (g serveGateway) enabled(upstream string) bool {
	return g.Models == nil || slices.Contains(g.Models, upstream)
}

// Shramba ključev (store_windows.go, store_other.go); testi jo zamenjajo.
var keyGet, keySet, keyDel = osKeyGet, osKeySet, osKeyDel

var validGatewayName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,31}$`)

// checkGatewayURL: https; http le do localhost ali zasebnega IP in le z --insecure. Brez uporabnika, poizvedbe in /v1.
func checkGatewayURL(raw string, insecure bool) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", errors.New("invalid gateway URL: " + raw)
	}
	switch u.Scheme {
	case "https":
	case "http":
		ip := net.ParseIP(u.Hostname())
		if u.Hostname() != "localhost" && (ip == nil || !(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast())) {
			return "", errors.New("gateway URL must be https (http only to localhost or a private IP, with --insecure)")
		}
		if !insecure {
			return "", errors.New("http sends the API key unencrypted: add --insecure for a gateway on localhost or a private IP")
		}
	default:
		return "", errors.New("gateway URL must be https: " + raw)
	}
	return strings.TrimSuffix(strings.TrimSuffix(u.String(), "/"), "/v1"), nil
}

// publishedID: model discovery v Claude Code obdrži le ID-je s »claude« ali »anthropic« (kot na Macu).
func publishedID(upstream string) string {
	if a := strings.ToLower(upstream); strings.Contains(a, "claude") || strings.Contains(a, "anthropic") {
		return upstream
	}
	return "claude-" + upstream
}

type gwModel struct {
	id     string // pri upstreamu
	window int    // 0 = neznano
}

// gwClients: brez preusmeritev (ključ ne gre drugam), roki za povezavo in glavo odgovora. Za http (--insecure) brez
// HTTP_PROXY: ključ bi šel nešifriran prek posrednika.
var gwClients = func() [2]*http.Client {
	var out [2]*http.Client
	for i := range out {
		t := http.DefaultTransport.(*http.Transport).Clone()
		t.ResponseHeaderTimeout = 10 * time.Minute // odgovor brez toka pride šele na koncu
		if i == 1 {
			t.Proxy = nil
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
	r.Header.Set("authorization", "Bearer "+key)
	resp, err := g.client().Do(r)
	if err != nil {
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
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&obj); err != nil {
		return nil, errors.New("unexpected response from " + path)
	}
	return obj, nil
}

// discoverGateway: modeli in okna (kot Discovery.load); LiteLLM ima okna v /model/info.
func discoverGateway(g serveGateway, key string) ([]gwModel, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
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
			if f, ok := m[k].(float64); ok {
				return int(f)
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
		out = append(out, gwModel{id, w})
	}
	if missing {
		if info, err := gwFetch(ctx, g, key, "/model/info"); err == nil {
			windows := map[string]int{}
			data, _ := info["data"].([]any)
			for _, it := range data {
				m, _ := it.(map[string]any)
				name, _ := m["model_name"].(string)
				mi, _ := m["model_info"].(map[string]any)
				windows[name] = window(mi, "max_input_tokens")
			}
			for i := range out {
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
		c.mu.Unlock()
		return e.models
	}
	if e.loading == nil && time.Since(e.failed) < 45*time.Second {
		c.mu.Unlock()
		return e.models
	}
	if e.loading == nil {
		done := make(chan struct{})
		e.loading = done
		go func() {
			key, _ := keyGet(s.cfgDir, g.Name)
			var ms []gwModel
			err := errors.New("no API key: run minigw serve gateway add " + g.Name)
			if key != "" {
				ms, err = discoverGateway(g, key)
			}
			c.mu.Lock()
			if err == nil {
				e.models, e.ok, e.at, e.failed = ms, true, time.Now(), time.Time{}
			} else {
				e.failed = time.Now()
				s.log.Printf("gateway %s: %v", g.Name, err)
			}
			e.loading = nil
			c.mu.Unlock()
			close(done)
		}()
	}
	done := e.loading
	if e.ok || !wait {
		c.mu.Unlock()
		return e.models
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
		for _, g := range s.cfg.Gateways {
			for _, m := range s.gatewayModels(g, false) {
				if g.enabled(m.id) && !shadowsCLI(m.id) && (m.id == model || publishedID(m.id) == model) {
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
	for _, g := range s.cfg.Gateways {
		wg.Go(func() { s.gatewayModels(g, true) })
	}
	wg.Wait()
}

// shadowsCLI: model prehoda z imenom claude-cli-… ali claude-chat-… bi zakril ponudnika claude -p.
func shadowsCLI(upstream string) bool {
	p := publishedID(upstream)
	return strings.HasPrefix(p, cliAgentPrefix) || strings.HasPrefix(p, cliChatPrefix)
}

func (s *server) gatewayModelList() []map[string]any {
	var data []map[string]any
	seen := map[string]bool{}
	s.loadGateways()
	for _, g := range s.cfg.Gateways {
		for _, m := range s.gatewayModels(g, false) {
			id := publishedID(m.id)
			if !g.enabled(m.id) || seen[id] || shadowsCLI(m.id) { // prvi prehod z imenom zmaga
				continue
			}
			seen[id] = true
			window := ""
			if m.window > 0 {
				window = fmt.Sprintf(" · %dK", m.window/1000)
			}
			data = append(data, map[string]any{"type": "model", "id": id, "display_name": m.id + " · " + g.Name,
				"description": g.Name + " · " + m.id + window, "created_at": "2026-01-01T00:00:00Z"})
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
	select { // največ 16 hkratnih zahtev k prehodom
	case s.gwSlots <- struct{}{}:
		defer func() { <-s.gwSlots }()
	default:
		return writeError(w, r.rid, 429, "Too many gateway requests at once on this router: try again")
	}
	key, _ := keyGet(s.cfgDir, g.Name)
	if key == "" {
		return writeError(w, r.rid, 503, "No API key configured for "+g.Name+": run minigw serve gateway add "+g.Name)
	}
	var body map[string]json.RawMessage
	if json.Unmarshal(r.body, &body) != nil {
		return writeError(w, r.rid, 400, "Body must be a JSON object")
	}
	body["model"], _ = json.Marshal(m.id)
	data, _ := json.Marshal(body)
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel()
	target := g.BaseURL + r.URL.Path
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	up, _ := http.NewRequestWithContext(ctx, "POST", target, bytes.NewReader(data))
	up.Header.Set("content-type", "application/json")
	up.Header.Set("authorization", "Bearer "+key)
	for _, h := range []string{"anthropic-version", "anthropic-beta"} {
		if v := r.h(h); v != "" {
			up.Header.Set(h, v)
		}
	}
	resp, err := g.client().Do(up)
	if err != nil {
		if r.Context().Err() != nil {
			return 499
		}
		return writeError(w, r.rid, 502, g.Name+" unreachable: "+err.Error())
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return upstreamError(w, r.rid, g.Name, resp)
	}
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
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
			return resp.StatusCode
		}
	}
}

// upstreamError: napaka upstreama kot napaka Anthropic. 401/403 → 502: Claude Code bi ju prikazal kot napačno prijavo v router.
func upstreamError(w http.ResponseWriter, rid, name string, resp *http.Response) int {
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
	if msg = strings.TrimSpace(clean(msg)); len(msg) > 300 {
		msg = msg[:300]
	}
	status := resp.StatusCode
	switch {
	case status == 401 || status == 403: // brez besedila upstreama
		msg, status = fmt.Sprintf("HTTP %d, check the API key (minigw serve gateway add %s)", status, name), 502
	case status < 400 || status > 599:
		msg, status = fmt.Sprintf("HTTP %d: %s", status, msg), 502
	}
	return writeError(w, rid, status, name+": "+msg)
}

// --- minigw serve gateway … ---

const gatewayHelp = `minigw serve gateway add <name> <baseURL> [--insecure]   add or update; the API key is read from stdin
minigw serve gateway remove <name>
minigw serve gateway list
minigw serve gateway models <name> [text] [--all]   enabled models; with text or --all also the disabled ones
minigw serve gateway models <name> [--enable m1,m2|all] [--disable m1,m2|all]`

func gatewayCommand(cfgPath string, args []string, insecure bool, enable, disable string, all bool, in io.Reader) {
	if len(args) == 0 {
		fail(gatewayHelp)
	}
	c, err := loadServeConfig(cfgPath)
	if err != nil {
		fail("cannot read " + cfgPath + " (run minigw serve init first): " + err.Error())
	}
	cfgDir, _ := filepath.Abs(filepath.Dir(cfgPath))
	find := func(name string) int {
		return slices.IndexFunc(c.Gateways, func(g serveGateway) bool { return g.Name == name })
	}
	need := func(n int) {
		if len(args) != n+1 {
			fail(gatewayHelp)
		}
	}
	switch args[0] {
	case "add":
		need(2)
		name := args[1]
		if !validGatewayName.MatchString(name) {
			fail("gateway name: letters, digits, . _ - (up to 32)")
		}
		base, err := checkGatewayURL(args[2], insecure)
		if err != nil {
			fail(err.Error())
		}
		key, err := readKey(in, name)
		if err != nil {
			fail(err.Error())
		}
		g := serveGateway{Name: name, BaseURL: base, Insecure: insecure}
		i := find(name)
		if i >= 0 {
			g.Models = c.Gateways[i].Models
		}
		models, err := discoverGateway(g, key)
		if err != nil {
			fail("gateway not added: " + err.Error())
		}
		if i < 0 && len(models) > 20 { // OpenRouter: več sto modelov, izbereš sam (kot na Macu)
			g.Models = []string{}
		}
		where, err := keySet(cfgDir, name, key)
		if err != nil {
			fail("cannot store the API key: " + err.Error())
		}
		if i >= 0 {
			c.Gateways[i] = g
		} else {
			c.Gateways = append(c.Gateways, g)
		}
		saveServeConfig(cfgPath, c)
		say(fmt.Sprintf("gateway %s: %d models, %s enabled; API key stored in %s", name, len(models), enabledCount(g, models), where))
		if g.Models != nil && len(g.Models) == 0 {
			say("enable models with: minigw serve gateway models " + name + " --enable <id>,<id>")
		}
		say("restart minigw serve")
	case "remove":
		need(1)
		i := find(args[1])
		if i < 0 {
			fail("no gateway " + args[1])
		}
		c.Gateways = slices.Delete(c.Gateways, i, i+1)
		saveServeConfig(cfgPath, c)
		keyDel(cfgDir, args[1])
		say("removed gateway " + args[1] + " and its API key (restart minigw serve)")
	case "list":
		need(0)
		for _, g := range c.Gateways {
			n := "all"
			if g.Models != nil {
				n = fmt.Sprint(len(g.Models))
			}
			say(fmt.Sprintf("%s  %s  models enabled: %s", g.Name, g.BaseURL, n))
		}
	case "models":
		filter := ""
		if len(args) == 3 { // models <ime> <besedilo>: vsi modeli, ki ga vsebujejo
			filter, args = strings.ToLower(args[2]), args[:2]
		}
		need(1)
		i := find(args[1])
		if i < 0 {
			fail("no gateway " + args[1])
		}
		g := &c.Gateways[i]
		key, _ := keyGet(cfgDir, g.Name)
		models, err := discoverGateway(*g, key)
		if err != nil {
			fail(err.Error())
		}
		upstream := func(list string) []string {
			var out []string
			for _, v := range strings.Split(list, ",") {
				if v = strings.TrimSpace(v); v == "" {
					continue
				}
				j := slices.IndexFunc(models, func(m gwModel) bool { return m.id == v || publishedID(m.id) == v })
				if j < 0 {
					fail("gateway " + g.Name + " has no model " + v)
				}
				out = append(out, models[j].id)
			}
			return out
		}
		changed := enable != "" || disable != ""
		before := map[string]bool{}
		for _, m := range models {
			before[m.id] = g.enabled(m.id)
		}
		switch {
		case enable == "all":
			g.Models = nil
		case enable != "":
			for _, id := range upstream(enable) {
				if !g.enabled(id) {
					g.Models = append(g.Models, id)
				}
			}
		}
		switch {
		case disable == "all":
			g.Models = []string{}
		case disable != "":
			off := upstream(disable)
			var keep []string
			for _, m := range models {
				if g.enabled(m.id) && !slices.Contains(off, m.id) {
					keep = append(keep, m.id)
				}
			}
			g.Models = append([]string{}, keep...)
		}
		hidden := 0 // OpenRouter: več sto modelov; brez izbire le vklopljeni
		for _, m := range models {
			show := g.enabled(m.id) || all
			switch {
			case changed:
				show = before[m.id] != g.enabled(m.id)
			case filter != "":
				show = strings.Contains(strings.ToLower(publishedID(m.id)), filter)
			}
			if !show {
				if !changed && filter == "" {
					hidden++
				}
				continue
			}
			mark := "[ ]"
			if g.enabled(m.id) {
				mark = "[x]"
			}
			w := ""
			if m.window > 0 {
				w = fmt.Sprintf("  %dK", m.window/1000)
			}
			say(clean(fmt.Sprintf("%s %s  (/model %s)%s", mark, m.id, publishedID(m.id), w)))
		}
		if hidden > 0 {
			say(fmt.Sprintf("%d disabled models not shown: add --all, or a text to search (minigw serve gateway models %s haiku)", hidden, g.Name))
		}
		if changed {
			saveServeConfig(cfgPath, c)
			say(fmt.Sprintf("%s enabled (restart minigw serve)", enabledCount(*g, models)))
		}
	default:
		fail(gatewayHelp)
	}
}

func enabledCount(g serveGateway, models []gwModel) string {
	if g.Models == nil {
		return "all"
	}
	n := 0
	for _, m := range models {
		if g.enabled(m.id) {
			n++
		}
	}
	return fmt.Sprint(n)
}

// readKey: ključ s stdin (cev) ali s terminala brez odmeva; nikoli iz argv. Le natisljivi ASCII (gre v glavo HTTP).
func readKey(in io.Reader, name string) (string, error) {
	var line string
	var err error
	if f, ok := in.(*os.File); ok && isTerminal(f) {
		fmt.Fprintf(os.Stderr, "API key for %s: ", name)
		line, err = readHidden(f)
		fmt.Fprintln(os.Stderr)
	} else {
		var b []byte
		b, err = io.ReadAll(io.LimitReader(in, 8<<10))
		line = string(b)
	}
	key := strings.TrimSpace(line)
	if key == "" {
		return "", errors.New("no API key (pipe it on stdin or type it at the prompt)")
	}
	for _, ch := range key {
		if ch < 0x21 || ch > 0x7e {
			return "", errors.New("the API key must be printable ASCII without spaces")
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
