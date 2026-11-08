package main

// Foundry Local (Microsoft): model na napravi prek lokalnega strežnika, združljivega z OpenAI (/v1/chat/completions),
// kot Apple Foundation Models na Macu: claude-foundry-<model>, pogovor brez orodij, navodila iz nastavitev.
// Vrata so dinamična: »foundry server status« (starejši CLI: »foundry service status«) izpiše URL, preverimo ga z
// GET /openai/status. Povezujemo se le na 127.0.0.1 (iz izpisa vzamemo samo številko vrat), brez posrednika in
// preusmeritev, z omejenim časom in velikostjo odgovora. Ključev ni.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// foundryConfig: serve.json "foundry" in skrbniški API /foundry.
type foundryConfig struct {
	Enabled      bool     `json:"enabled,omitempty"`
	Executable   string   `json:"executable,omitempty"` // CLI foundry; prazno = iz PATH
	Models       []string `json:"models"`               // null = vsi preneseni modeli; seznam = le ti
	Instructions string   `json:"instructions,omitempty"`
}

const (
	foundryPrefix       = "claude-foundry-"
	foundryInstructions = "You are a helpful assistant running on-device. Answer concisely."
)

var foundryMaxBody int64 = 8 << 20 // odgovor modela največ 8 MB (testi: manj)

var (
	foundryTimeout     = 10 * time.Minute // en odgovor (vključno z nalaganjem modela)
	foundryStatusURL   = regexp.MustCompile(`https?://(?:localhost|127\.0\.0\.1|\[::1\]):(\d{2,5})`)
	foundryCommand     = runFoundryCLI // testi: lažen CLI
	errFoundryNotFound = errors.New("Foundry Local is not installed (Windows: winget install Microsoft.FoundryLocal; macOS: brew tap microsoft/foundrylocal && brew install foundrylocal)")
)

func (f foundryConfig) modelEnabled(m string) bool {
	return f.Models == nil || containsStr(f.Models, m)
}

func (f foundryConfig) instructions() string {
	if strings.TrimSpace(f.Instructions) == "" {
		return foundryInstructions
	}
	return f.Instructions
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// foundryClient: le 127.0.0.1, brez posrednika (HTTP_PROXY) in brez preusmeritev.
var foundryClient = &http.Client{
	Transport:     &http.Transport{Proxy: nil, MaxIdleConnsPerHost: 4, ResponseHeaderTimeout: 5 * time.Minute},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// runFoundryCLI (F1): »start« brez cevi (strežnik v ozadju bi jih držal odprte); pri drugih ukazih WaitDelay zapre
// cevi, ki jih drži otrok, 5 s po koncu ukaza. Izpis največ 64 KB.
func runFoundryCLI(ctx context.Context, exe string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.WaitDelay = 5 * time.Second
	if len(args) > 1 && args[1] == "start" {
		return "", cmd.Run() // Stdout, Stderr nil: /dev/null
	}
	out, err := cmd.CombinedOutput()
	return string(out[:min(len(out), 64<<10)]), err
}

// foundryState: najdena končna točka in seznam modelov (predpomnjeno).
type foundryState struct {
	discover  sync.Mutex // F1: iskanje in zagon (klici CLI) brez mu, da /foundry ne čaka
	retryAt   time.Time  // F6: po neuspelem iskanju ali zagonu 30 s brez novega poskusa
	mu        sync.Mutex
	base      string // http://127.0.0.1:<vrata>
	checked   time.Time
	models    []foundryModel
	modelsAt  time.Time
	loadedAt  map[string]time.Time
	lastError string
}

type foundryModel struct {
	Name   string `json:"name"`
	Vision bool   `json:"vision"`
}

// resolveFoundry: nastavljena pot ali iz PATH; na Windows le .exe (F9, kot resolveAgent: ovoj .cmd bi tekel prek cmd.exe).
func resolveFoundry(path string) string {
	name := path
	if name == "" {
		name = "foundry"
	}
	p, err := exec.LookPath(name)
	if err != nil || (runtime.GOOS == "windows" && !strings.EqualFold(filepath.Ext(p), ".exe")) {
		return ""
	}
	return p
}

// foundryPort: vrata iz izpisa CLI (le številka; gostitelj je vedno 127.0.0.1).
func foundryPort(out string) int {
	if m := foundryStatusURL.FindStringSubmatch(out); m != nil {
		if p, err := strconv.Atoi(m[1]); err == nil && p > 0 && p < 65536 {
			return p
		}
	}
	return 0
}

// foundryCheck: na vratih res odgovarja Foundry Local (GET /openai/status z "Endpoints").
func foundryCheck(ctx context.Context, base string) bool {
	var st struct{ Endpoints []string }
	return foundryGet(ctx, base+"/openai/status", 5*time.Second, &st) == nil && st.Endpoints != nil
}

func foundryGet(ctx context.Context, u string, timeout time.Duration, out any) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	resp, err := foundryClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, foundryMaxBody))
	if err != nil {
		return err
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(b, out)
}

// foundryEndpoint: tekoči strežnik Foundry Local; če je nameščen in ustavljen, ga zažene.
func (s *server) foundryEndpoint(ctx context.Context) (string, error) {
	st := &s.foundry
	cached := func() (string, error, bool) {
		st.mu.Lock()
		defer st.mu.Unlock()
		if st.base != "" && time.Since(st.checked) < time.Minute {
			return st.base, nil, true
		}
		if time.Now().Before(st.retryAt) { // F6
			return "", errors.New(st.lastError), true
		}
		return "", nil, false
	}
	// N5: poslušalca preveri vsaka zahteva (port lahko medtem prevzame drug proces), ne le novo iskanje.
	ours := func(base string, exe string) bool {
		p, _ := strconv.Atoi(base[strings.LastIndexByte(base, ':')+1:])
		return listenerIsOurs(p) && listenerInDir(p, foundryInstallDir(exe))
	}
	if b, err, ok := cached(); ok {
		if err != nil || ours(b, resolveFoundry(s.conf().Foundry.Executable)) {
			return b, err
		}
		st.mu.Lock()
		if st.base == b {
			st.base = ""
		}
		st.mu.Unlock()
	}
	st.discover.Lock()
	defer st.discover.Unlock()
	if b, err, ok := cached(); ok { // medtem ga je našla druga zahteva
		return b, err
	}
	fail := func(msg string) (string, error) {
		st.mu.Lock()
		st.base, st.lastError, st.retryAt = "", msg, time.Now().Add(30*time.Second)
		st.mu.Unlock()
		return "", errors.New(msg)
	}
	exe := resolveFoundry(s.conf().Foundry.Executable)
	if exe == "" {
		return fail(errFoundryNotFound.Error())
	}
	foreign := false
	find := func() string {
		for _, sub := range []string{"server", "service"} { // »service« pri starejših različicah
			cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			out, _ := foundryCommand(cctx, exe, sub, "status")
			cancel()
			if p := foundryPort(out); p != 0 {
				base := "http://127.0.0.1:" + strconv.Itoa(p)
				if !foundryCheck(ctx, base) {
					continue
				}
				if !ours(base, exe) { // F2, L3: drug uporabnik ali program zunaj namestitve
					foreign = true
					continue
				}
				return base
			}
		}
		return ""
	}
	base := find()
	if base == "" && !foreign {
		for _, sub := range []string{"server", "service"} {
			cctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			_, err := foundryCommand(cctx, exe, sub, "start")
			cancel()
			if err == nil {
				break
			}
		}
		base = find()
	}
	if base == "" {
		if foreign {
			return fail("Foundry Local's port belongs to another user's process: not used")
		}
		return fail("Foundry Local does not answer (foundry server status)")
	}
	st.mu.Lock()
	st.base, st.checked, st.lastError, st.retryAt = base, time.Now(), "", time.Time{}
	st.mu.Unlock()
	return base, nil
}

// foundryModels: preneseni modeli (GET /openai/models), s podatkom o slikah iz kataloga (GET /foundry/list).
func (s *server) foundryModels(ctx context.Context) ([]foundryModel, error) {
	base, err := s.foundryEndpoint(ctx)
	if err != nil {
		return nil, err
	}
	st := &s.foundry
	st.mu.Lock()
	if st.models != nil && time.Since(st.modelsAt) < 5*time.Minute {
		defer st.mu.Unlock()
		return st.models, nil
	}
	st.mu.Unlock()
	var raw json.RawMessage
	if err := foundryGet(ctx, base+"/openai/models", 30*time.Second, &raw); err != nil {
		return nil, fmt.Errorf("Foundry Local models: %v", err)
	}
	names := foundryNames(raw)
	var cat []map[string]any
	foundryGet(ctx, base+"/foundry/list", 30*time.Second, &cat) // ponytail: katalog le za slike; brez njega samo besedilo
	vision := map[string]bool{}
	for _, m := range cat {
		n, _ := m["name"].(string)
		info, _ := json.Marshal([]any{m["inputModalities"], m["task"], m["modalities"]})
		vision[n] = strings.Contains(strings.ToLower(string(info)), "image")
	}
	var out []foundryModel
	for _, n := range names {
		if foundryValidModel(n) {
			out = append(out, foundryModel{Name: n, Vision: vision[n]})
		}
	}
	st.mu.Lock()
	st.models, st.modelsAt = out, time.Now()
	st.mu.Unlock()
	return out, nil
}

// foundryNames: seznam imen ali objektov z name/id (oblika ni dokumentirana).
func foundryNames(raw json.RawMessage) []string {
	var list []any
	if json.Unmarshal(raw, &list) != nil {
		var obj struct{ Data, Models []any }
		json.Unmarshal(raw, &obj)
		list = append(obj.Data, obj.Models...)
	}
	var out []string
	for _, x := range list {
		switch v := x.(type) {
		case string:
			out = append(out, v)
		case map[string]any:
			for _, k := range []string{"name", "id", "modelId"} {
				if n, ok := v[k].(string); ok && n != "" {
					out = append(out, n)
					break
				}
			}
		}
	}
	return out
}

// foundryModelList: claude-foundry-<model> za vklopljene prenesene modele (le vklopljen ponudnik).
func (s *server) foundryModelList() []map[string]any {
	fc := s.conf().Foundry
	if !fc.Enabled {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	models, err := s.foundryModels(ctx)
	if err != nil {
		s.log.Printf("Foundry Local: %v", err)
		return nil
	}
	var out []map[string]any
	for _, m := range models {
		if fc.modelEnabled(m.Name) {
			out = append(out, map[string]any{"type": "model", "id": foundryPrefix + m.Name, "display_name": m.Name + " · Foundry Local",
				"description": "Foundry Local · on this device · no tools", "created_at": "2026-01-01T00:00:00Z"})
		}
	}
	return out
}

var (
	reminderBlock = regexp.MustCompile(`<system-reminder>[\s\S]*?</system-reminder>`)
	lineNumbers   = regexp.MustCompile(`(?m)^ *\d+\t`)
	readPath      = regexp.MustCompile(`"file_path"\s*:\s*"([^"]+)"`)
)

// stripReminders: kot AppleFM.stripReminders: bloki <system-reminder> ven (sicer bi napolnili majhen kontekst),
// razen vsebine datotek, ki jih je Claude Code priložil z @.
func stripReminders(s string) string {
	return strings.TrimSpace(reminderBlock.ReplaceAllStringFunc(s, func(block string) string {
		if strings.Contains(block, "Called the Read tool with the following input:") {
			if m := readPath.FindStringSubmatch(block); m != nil {
				return "Attached file " + m[1] + ":"
			}
		}
		if _, body, ok := strings.Cut(block, "Result of calling the Read tool:"); ok {
			body = strings.TrimSuffix(body, "</system-reminder>")
			return "```\n" + strings.TrimSpace(lineNumbers.ReplaceAllString(body, "")) + "\n```"
		}
		return ""
	}))
}

// foundryRequest: Anthropic Messages → OpenAI chat completions. Le pogovor: orodja odjemalca (Claude Code jih
// pošlje vedno) se ne pošljejo; tool_choice, ki orodje zahteva, je 400. Slike le zadnjega sporočila, če jih model zna.
func foundryRequest(body map[string]any, model, instructions string, vision, stream bool) (map[string]any, error) {
	if tc, _ := body["tool_choice"].(map[string]any); tc != nil {
		if t, _ := tc["type"].(string); t == "any" || t == "tool" {
			return nil, errors.New("Foundry Local models here take no tools: tool_choice " + t + " cannot be met")
		}
	}
	var ts []turn
	for _, t := range turns(body) {
		if t.text = stripReminders(t.text); t.text != "" {
			ts = append(ts, t)
		}
	}
	if len(ts) == 0 || ts[len(ts)-1].role != "user" {
		return nil, errors.New("Last message must be from the user")
	}
	for len(ts) > 1 && ts[0].role == "assistant" {
		ts = ts[1:]
	}
	msgs := []any{map[string]any{"role": "system", "content": instructions}}
	for i, t := range ts {
		var content any = t.text
		if i == len(ts)-1 {
			if imgs := lastImages(body); len(imgs) > 0 {
				if !vision {
					content = t.text + "\n\n[" + strconv.Itoa(len(imgs)) + " image(s) left out: this model reads text only]"
				} else {
					parts := []any{map[string]any{"type": "text", "text": t.text}}
					for _, u := range imgs {
						parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]string{"url": u}})
					}
					content = parts
				}
			}
		}
		msgs = append(msgs, map[string]any{"role": t.role, "content": content})
	}
	out := map[string]any{"model": model, "messages": msgs, "stream": stream}
	if n, ok := body["max_tokens"].(float64); ok && n > 0 {
		out["max_completion_tokens"] = int(min(n, 32768))
	}
	for _, k := range []string{"temperature", "top_p"} {
		if v, ok := body[k].(float64); ok {
			out[k] = v
		}
	}
	if stop, ok := body["stop_sequences"].([]any); ok && len(stop) > 0 {
		out["stop"] = stop
	}
	if stream {
		out["stream_options"] = map[string]bool{"include_usage": true}
	}
	return out, nil
}

// lastImages: slike (base64) zadnjega uporabnikovega sporočila kot data: URL.
func lastImages(body map[string]any) []string {
	msgs, _ := body["messages"].([]any)
	if len(msgs) == 0 {
		return nil
	}
	m, _ := msgs[len(msgs)-1].(map[string]any)
	blocks, _ := m["content"].([]any)
	var out []string
	for _, b := range blocks {
		bm, _ := b.(map[string]any)
		src, _ := bm["source"].(map[string]any)
		if bm["type"] != "image" || src["type"] != "base64" {
			continue
		}
		mt, _ := src["media_type"].(string)
		data, _ := src["data"].(string)
		if _, err := base64.StdEncoding.DecodeString(data); err == nil && strings.HasPrefix(mt, "image/") && !strings.ContainsAny(mt, ";,") {
			out = append(out, "data:"+mt+";base64,"+data)
		}
	}
	return out
}

// foundryCount (F5): število žetonov le, če je končno in v 0…1e9.
func foundryCount(v any) (float64, bool) {
	f, ok := v.(float64)
	return f, ok && !math.IsNaN(f) && !math.IsInf(f, 0) && f >= 0 && f <= 1e9
}

// foundryValidModel (F7): ime gre v pot /openai/load/<ime>: brez »/« in »..«.
func foundryValidModel(m string) bool {
	return validAgentModel.MatchString(m) && !strings.Contains(m, "/") && !strings.Contains(m, "..")
}

// foundryStop: finish_reason → stop_reason.
func foundryStop(reason string) string {
	if reason == "length" {
		return "max_tokens"
	}
	return "end_turn"
}

// runFoundry: en odgovor modela Foundry Local (pretok SSE ali cel odgovor).
func (s *server) runFoundry(w http.ResponseWriter, r *req, body map[string]any, model string) int {
	fc := s.conf().Foundry
	ctx, cancel := context.WithTimeout(r.Context(), foundryTimeout)
	defer cancel()
	models, err := s.foundryModels(ctx)
	if err != nil {
		return writeError(w, r.rid, 503, err.Error())
	}
	var m *foundryModel
	for i := range models {
		if models[i].Name == model {
			m = &models[i]
		}
	}
	if m == nil || !fc.modelEnabled(model) {
		return writeError(w, r.rid, 404, "Unknown model: "+foundryPrefix+model)
	}
	stream := body["stream"] == true
	oreq, err := foundryRequest(body, model, fc.instructions(), m.Vision, stream)
	if err != nil {
		return writeError(w, r.rid, 400, err.Error())
	}
	base, err := s.foundryEndpoint(ctx)
	if err != nil {
		return writeError(w, r.rid, 503, err.Error())
	}
	s.foundry.mu.Lock()
	if s.foundry.loadedAt == nil {
		s.foundry.loadedAt = map[string]time.Time{}
	}
	loaded := time.Since(s.foundry.loadedAt[model]) < time.Minute
	s.foundry.mu.Unlock()
	if !loaded { // naloži model (prvič lahko traja)
		if err := foundryGet(ctx, base+"/openai/load/"+url.PathEscape(model), foundryTimeout, nil); err != nil {
			return writeError(w, r.rid, 502, "Foundry Local could not load "+model+": "+err.Error())
		}
		s.foundry.mu.Lock()
		s.foundry.loadedAt[model] = time.Now()
		s.foundry.mu.Unlock()
	}
	b, _ := json.Marshal(oreq)
	hreq, _ := http.NewRequestWithContext(ctx, "POST", base+"/v1/chat/completions", bytes.NewReader(b))
	hreq.Header.Set("Content-Type", "application/json")
	resp, err := foundryClient.Do(hreq)
	if err != nil {
		s.foundry.mu.Lock()
		s.foundry.base = "" // znova poišči (strežnik je morda na drugih vratih)
		s.foundry.mu.Unlock()
		return writeError(w, r.rid, 502, "Foundry Local: "+err.Error())
	}
	defer resp.Body.Close()
	limited := &io.LimitedReader{R: resp.Body, N: foundryMaxBody}
	if resp.StatusCode != 200 {
		msg, _ := io.ReadAll(io.LimitReader(limited, 2000))
		s.log.Printf("%s Foundry Local HTTP %d: %q", r.rid, resp.StatusCode, msg) // F8
		return writeError(w, r.rid, 502, fmt.Sprintf("Foundry Local answered HTTP %d (see the gateway log)", resp.StatusCode))
	}
	idb := make([]byte, 16)
	rand.Read(idb)
	t := &translator{model: foundryPrefix + model, id: fmt.Sprintf("msg_%x", idb), usage: map[string]any{}}
	usage := func(u map[string]any) {
		for from, to := range map[string]string{"prompt_tokens": "input_tokens", "completion_tokens": "output_tokens"} {
			if v, ok := foundryCount(u[from]); ok {
				t.usage[to] = v
			}
		}
	}
	if !stream {
		var o struct {
			Choices []struct {
				Message      struct{ Content string } `json:"message"`
				FinishReason string                   `json:"finish_reason"`
			} `json:"choices"`
			Usage map[string]any `json:"usage"`
		}
		if err := json.NewDecoder(limited).Decode(&o); err != nil || len(o.Choices) == 0 {
			return writeError(w, r.rid, 502, "Foundry Local gave no answer")
		}
		t.emit(o.Choices[0].Message.Content)
		t.stop = foundryStop(o.Choices[0].FinishReason)
		usage(o.Usage)
		return writeJSON(w, r.rid, 200, t.message())
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("request-id", r.rid)
	w.WriteHeader(200)
	send := func(b []byte) { w.Write(b); http.NewResponseController(w).Flush() }
	send(t.start())
	sc := bufio.NewScanner(limited)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data:")
		if data = strings.TrimSpace(data); !ok || data == "" {
			continue
		}
		if data == "[DONE]" {
			break
		}
		var c struct {
			Choices []struct {
				Delta        struct{ Content string } `json:"delta"`
				FinishReason *string                  `json:"finish_reason"`
			} `json:"choices"`
			Usage map[string]any `json:"usage"`
		}
		if json.Unmarshal([]byte(data), &c) != nil {
			continue
		}
		usage(c.Usage)
		for _, ch := range c.Choices {
			if ch.Delta.Content != "" {
				send(t.emit(ch.Delta.Content))
			}
			if ch.FinishReason != nil {
				t.stop = foundryStop(*ch.FinishReason)
			}
		}
	}
	if err := sc.Err(); err != nil || ctx.Err() != nil || limited.N <= 0 {
		if r.Context().Err() != nil {
			return 499
		}
		msg := "Foundry Local: the answer stopped"
		if ctx.Err() != nil {
			msg = fmt.Sprintf("Foundry Local: no answer within %v", foundryTimeout)
		} else if limited.N <= 0 { // F3: ne tiho »end_turn« pri prirezanem odgovoru
			msg = fmt.Sprintf("Foundry Local: the answer is larger than %d MB", foundryMaxBody>>20)
		}
		send(sse("error", map[string]any{"type": "error", "error": map[string]string{"type": "api_error", "message": msg}}))
		return 502
	}
	if t.tokens("output_tokens") == 0 {
		t.usage["output_tokens"] = float64(len(t.text) / 4)
	}
	send(t.finish())
	return 200
}
