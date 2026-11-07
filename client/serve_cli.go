package main

// minigw serve: ponudnik claude -p (kot RouterCore/ClaudeCLI.swift). claude-cli-<m> je agent z orodji v kopiji mape
// odjemalca (ali v --folder); zahteve brez orodij (naslov seje, povzetki) so pogovor brez orodij.
// Izhod stream-json se pretvori v en Anthropic odgovor z enim besedilnim blokom.

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
)

var cliFamilies = []string{"opus", "sonnet", "haiku", "fable"}

const (
	cliAgentPrefix = "claude-cli-"
	cliChatPrefix  = "claude-chat-" // brez orodij; ostane zaradi shranjenih izbir
)

// cliMatch: model za claude --model in ali je način vsiljeno »chat«; vgrajeni modeli Claude Code (claude-haiku-…) tudi.
func cliMatch(model string) (string, bool, bool) {
	fam := func(m string) bool {
		for _, f := range cliFamilies {
			if strings.HasPrefix(m, f) || strings.HasPrefix(m, "claude-"+f) {
				return true
			}
		}
		return false
	}
	for _, p := range []struct {
		prefix string
		chat   bool
	}{{cliChatPrefix, true}, {cliAgentPrefix, false}} {
		if strings.HasPrefix(model, p.prefix) {
			m := strings.TrimPrefix(model, p.prefix)
			return m, p.chat, fam(m)
		}
	}
	for _, f := range cliFamilies {
		if model == f {
			return model, false, true
		}
	}
	return model, false, fam(model) && strings.HasPrefix(model, "claude-")
}

// validModel: ID modela (tudi za --model): male črke, števke, ».-[]« (npr. »claude-opus-4-8[1m]«).
var validModel = regexp.MustCompile(`^[a-z0-9][a-z0-9.\-\[\]]{0,63}$`)

// resolveClaude: claude.exe (ali claude) ali node + cli.js; nikoli .cmd/.bat: Windows jih zažene prek cmd.exe, ki
// argumente razčleni drugače, kot jih Go kodira (BatBadBut, vrivanje ukazov). nil = ni nameščen.
func resolveClaude(configured string) ([]string, error) {
	p := configured
	if p == "" {
		if runtime.GOOS == "windows" {
			p, _ = exec.LookPath("claude.exe")
		}
		if p == "" {
			if p, _ = exec.LookPath("claude"); p == "" {
				return nil, nil
			}
		}
	} else if st, err := os.Stat(p); err != nil || st.IsDir() {
		return nil, fmt.Errorf("claude not found at %s (\"claude\" in the config)", p)
	}
	ext := strings.ToLower(filepath.Ext(p))
	if runtime.GOOS != "windows" || ext == ".exe" {
		return []string{p}, nil
	}
	// Ovojnica npm (claude.cmd): paket je v node_modules ob njej.
	js := filepath.Join(filepath.Dir(p), "node_modules", "@anthropic-ai", "claude-code", "cli.js")
	node, _ := exec.LookPath("node.exe")
	if _, err := os.Stat(js); err != nil || node == "" {
		return nil, fmt.Errorf("%s is not an .exe and minigw serve does not run batch files (unsafe argument quoting): install the native Claude Code (claude.exe) or set \"claude\" in the config to it", p)
	}
	return []string{node, js}, nil
}

func hasBash(tools string) bool {
	for _, t := range strings.Split(tools, ",") {
		if strings.TrimSpace(t) == "Bash" {
			return true
		}
	}
	return false
}

// Orodja z dostopom do datotek: dovoljena le v delovni mapi (./**).
var pathTools = map[string]bool{"Read": true, "Edit": true, "Write": true, "MultiEdit": true, "NotebookEdit": true, "Glob": true, "Grep": true, "LS": true}

// ruleAbs: absolutna pot za pravilo dovoljenj Claude Code (»//pot«; Windows »C:\x« → »//c/x«).
func ruleAbs(p string) string {
	p = filepath.ToSlash(p)
	if len(p) >= 2 && p[1] == ':' {
		p = "/" + strings.ToLower(p[:1]) + p[2:]
	}
	return "/" + p
}

// agentArgs: orodja agenta le v delovni mapi; mapa nastavitev (žeton, ključ routerja) prepovedana. Razen na Windows še
// sandbox Claude Code (kot ClaudeCLI.agentArgs): Bash ne bere domače mape in piše le v kopijo; z Bash je obvezen.
func (s *server) agentArgs(dir string) []string {
	c := s.conf()
	dirs := []string{dir} // delovna mapa in dovoljene mape (kot allowedRoots na Macu)
	for _, f := range c.AllowedFolders {
		if f != dir {
			dirs = append(dirs, f)
		}
	}
	scope := func(t string) []string { // orodje z dostopom do datotek le v teh mapah
		out := []string{t + "(./**)"}
		for _, d := range dirs[1:] {
			out = append(out, t+"("+ruleAbs(d)+"/**)")
		}
		return out
	}
	cfg := ruleAbs(s.cfgDir) + "/**"
	deny := []string{"Read(" + cfg + ")", "Edit(" + cfg + ")", "Write(" + cfg + ")"}
	var args []string
	if c.AllowLocalTools { // vsa orodja Claude Code razen izjem; datoteke le v mapah, Bash v sandboxu (kot na Macu)
		var allowed []string
		for t := range pathTools {
			if !slices.Contains(c.DisabledTools, t) {
				allowed = append(allowed, scope(t)...)
			}
		}
		slices.Sort(allowed)
		args = []string{"--tools", "default", "--allowedTools", strings.Join(allowed, ","), "--permission-mode", "acceptEdits",
			"--disallowedTools", strings.Join(append(deny, c.DisabledTools...), ",")}
	} else {
		var tools, allowed []string
		for _, t := range strings.Split(c.Tools, ",") {
			switch t = strings.TrimSpace(t); {
			case t == "":
				continue
			case pathTools[t]:
				allowed = append(allowed, scope(t)...)
			case t != "Bash" || runtime.GOOS == "windows": // Bash v sandboxu dovoli autoAllowBashIfSandboxed
				allowed = append(allowed, t)
			}
			tools = append(tools, t)
		}
		args = []string{"--tools", strings.Join(tools, ","), "--allowedTools", strings.Join(allowed, ","),
			"--disallowedTools", strings.Join(deny, ","), "--permission-mode", c.PermissionMode}
	}
	for _, d := range dirs[1:] {
		args = append(args, "--add-dir", d)
	}
	if runtime.GOOS != "windows" {
		settings, _ := json.Marshal(map[string]any{"sandbox": map[string]any{
			"enabled": true, "failIfUnavailable": hasBash(c.Tools) || c.AllowLocalTools, "autoAllowBashIfSandboxed": true, "allowUnsandboxedCommands": false,
			"network":    map[string]any{"allowedDomains": []string{"*"}},
			"filesystem": map[string]any{"denyRead": []string{"~", s.cfgDir}, "allowRead": dirs, "allowWrite": dirs},
		}})
		args = append(args, "--settings", string(settings))
	}
	return args
}

// promptFile: besedilo za --system-prompt-file / --append-system-prompt-file (0600, ne v argv).
func (s *server) promptFile(text string) (string, error) {
	f, err := os.CreateTemp(s.conf().scratch("prompts"), "p-*.txt")
	if err != nil {
		return "", err
	}
	_, err = f.WriteString(text)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return f.Name(), err
}

// tailBuffer: zadnjih 4000 bajtov (stderr procesa claude).
type tailBuffer struct {
	mu sync.Mutex
	b  []byte
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.b = append(t.b, p...); len(t.b) > 8000 {
		t.b = append([]byte{}, t.b[len(t.b)-4000:]...)
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.b) > 4000 {
		return string(t.b[len(t.b)-4000:])
	}
	return string(t.b)
}

func (s *server) modelList() map[string]any {
	data := append([]map[string]any{}, s.gatewayModelList()...)
	if len(s.claudeExe()) > 0 && !s.conf().ClaudeDisabled {
		for _, m := range cliFamilies[:3] {
			data = append(data, map[string]any{"type": "model", "id": cliAgentPrefix + m,
				"display_name": strings.ToUpper(m[:1]) + m[1:] + " · Claude CLI",
				"description":  "claude -p · " + m + " · tools in your synced folder", "created_at": "2026-01-01T00:00:00Z"})
		}
	}
	var first, last any
	if len(data) > 0 {
		first, last = data[0]["id"], data[len(data)-1]["id"]
	}
	return map[string]any{"data": data, "has_more": false, "first_id": first, "last_id": last}
}

type turn struct{ role, text string }

func contentText(c any) string {
	if s, ok := c.(string); ok {
		return s
	}
	blocks, _ := c.([]any)
	var out []string
	for _, b := range blocks {
		m, _ := b.(map[string]any)
		switch m["type"] {
		case "text":
			t, _ := m["text"].(string)
			out = append(out, t)
		case "tool_use":
			n, _ := m["name"].(string)
			out = append(out, "[tool_use "+n+"]")
		case "tool_result":
			out = append(out, "[tool_result] "+contentText(m["content"]))
		}
	}
	return strings.Join(out, "\n")
}

// turns: samo user/assistant (Claude Code vriva tudi system, ki se spreminja).
func turns(body map[string]any) []turn {
	msgs, _ := body["messages"].([]any)
	var out []turn
	for _, x := range msgs {
		m, _ := x.(map[string]any)
		if r, _ := m["role"].(string); r == "user" || r == "assistant" {
			out = append(out, turn{r, strings.TrimSpace(contentText(m["content"]))})
		}
	}
	return out
}

// transcriptKey: ista zgodovina v isti zunanji seji → ista notranja seja (kot ClaudeCLI.key).
func transcriptKey(ts []turn, scope string) string {
	h := sha256.New()
	h.Write([]byte(scope))
	for _, t := range ts {
		h.Write([]byte("\x01" + t.role + "\x02" + t.text))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func cliPrompt(history []turn, last turn) string {
	if len(history) == 0 {
		return last.text
	}
	var past []string
	for _, t := range history {
		who := "User"
		if t.role == "assistant" {
			who = "Assistant"
		}
		past = append(past, who+": "+t.text)
	}
	return "Earlier conversation (continued from another session):\n\n" + strings.Join(past, "\n\n") + "\n\n---\n\n" + last.text
}

func systemText(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	blocks, _ := v.([]any)
	var out []string
	for _, b := range blocks {
		if t, ok := b.(map[string]any)["text"].(string); ok {
			out = append(out, t)
		}
	}
	return strings.Join(out, "\n\n")
}

// sessionRegistry: zunanja seja (ključ transkripta) → notranja seja claude; ob ponovnem zagonu se izgubi.
type sessionRegistry struct {
	mu sync.Mutex
	m  map[string]string
}

func (r *sessionRegistry) get(k string) string { r.mu.Lock(); defer r.mu.Unlock(); return r.m[k] }

func (r *sessionRegistry) set(k, v string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.m) > 2000 { // ponytail: grobo čiščenje, LRU ko bo treba
		r.m = map[string]string{}
	}
	r.m[k] = v
}

// translator: stream-json → dogodki SSE enega besedilnega bloka (kot StreamTranslator).
type translator struct {
	model, id, text, resultError, step string
	usage                              map[string]any
}

func sse(event string, obj any) []byte {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.Encode(obj)
	return []byte("event: " + event + "\ndata: " + strings.TrimSuffix(b.String(), "\n") + "\n\n")
}

func (t *translator) start() []byte {
	return append(sse("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": t.id, "type": "message", "role": "assistant", "model": t.model, "content": []any{},
		"stop_reason": nil, "stop_sequence": nil, "usage": map[string]int{"input_tokens": 0, "output_tokens": 0}}}),
		sse("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]string{"type": "text", "text": ""}})...)
}

func (t *translator) emit(s string) []byte {
	t.text += s
	return sse("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]string{"type": "text_delta", "text": s}})
}

func (t *translator) feed(line []byte) []byte {
	var obj map[string]any
	if json.Unmarshal(line, &obj) != nil {
		return nil
	}
	main := obj["parent_tool_use_id"] == nil // dogodki notranjih podagentov ne gredo naprej
	switch obj["type"] {
	case "stream_event":
		ev, _ := obj["event"].(map[string]any)
		if !main || ev == nil {
			return nil
		}
		switch ev["type"] {
		case "message_start":
			if t.text != "" && !strings.HasSuffix(t.text, "\n") {
				return t.emit("\n\n")
			}
		case "content_block_start":
			if b, _ := ev["content_block"].(map[string]any); b["type"] == "tool_use" {
				name, _ := b["name"].(string)
				if name == "" {
					name = "tool"
				}
				nl := "\n"
				if t.text == "" || strings.HasSuffix(t.text, "\n") {
					nl = ""
				}
				return t.emit(nl + "⏺ " + name + "\n")
			}
		case "content_block_delta":
			if d, _ := ev["delta"].(map[string]any); d["type"] == "text_delta" {
				s, _ := d["text"].(string)
				return t.emit(s)
			}
		}
	case "assistant": // celotno sporočilo z vhodi orodij (za dnevnik korakov)
		msg, _ := obj["message"].(map[string]any)
		blocks, _ := msg["content"].([]any)
		for _, x := range blocks {
			b, _ := x.(map[string]any)
			if !main || b["type"] != "tool_use" {
				continue
			}
			in, _ := b["input"].(map[string]any)
			arg := ""
			for _, k := range []string{"command", "file_path", "pattern", "path", "url", "query", "description"} {
				if v, ok := in[k].(string); ok {
					arg = v
					break
				}
			}
			if len(arg) > 120 {
				arg = arg[:120]
			}
			name, _ := b["name"].(string)
			t.step = strings.TrimSpace(name + " " + arg)
		}
	case "result":
		t.usage, _ = obj["usage"].(map[string]any)
		if obj["is_error"] == true || obj["subtype"] != "success" {
			t.resultError, _ = obj["result"].(string)
			if t.resultError == "" {
				t.resultError, _ = obj["subtype"].(string)
			}
			if t.resultError == "" {
				t.resultError = "claude error"
			}
		}
	}
	return nil
}

func (t *translator) tokens(k string) int { f, _ := t.usage[k].(float64); return int(f) }

func (t *translator) finish() []byte {
	return append(append(sse("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0}),
		sse("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
			"usage": map[string]int{"output_tokens": t.tokens("output_tokens")}})...),
		sse("message_stop", map[string]string{"type": "message_stop"})...)
}

func (t *translator) message() map[string]any {
	return map[string]any{"id": t.id, "type": "message", "role": "assistant", "model": t.model,
		"content": []any{map[string]string{"type": "text", "text": t.text}}, "stop_reason": "end_turn", "stop_sequence": nil,
		"usage": map[string]int{"input_tokens": t.tokens("input_tokens"), "output_tokens": t.tokens("output_tokens")}}
}

// messages: /v1/messages in /v1/messages/count_tokens za claude-cli-*.
func (s *server) messages(w http.ResponseWriter, r *req) (int, string) {
	var body map[string]any
	model := ""
	if json.Unmarshal(r.body, &body) == nil {
		model, _ = body["model"].(string)
	}
	if model == "" {
		return writeError(w, r.rid, 400, "Body must be JSON with a model field"), ""
	}
	_, _, isCLI := cliMatch(model)                     // model claude-cli ne čaka na odkrivanje prehodov
	if g, m, ok := s.gatewayRoute(model, !isCLI); ok { // prehodi pred claude-cli (kot Router.swift); ID-ji z »/« (OpenRouter)
		return s.passthrough(w, r, g, m), model
	}
	cliModel, chat, ok := cliMatch(model)
	if !validModel.MatchString(model) || !validModel.MatchString(cliModel) {
		return writeError(w, r.rid, 400, "Invalid model name"), "?"
	}
	if !ok {
		return writeError(w, r.rid, 404, "Unknown model: "+model), model
	}
	if s.conf().ClaudeDisabled {
		return writeError(w, r.rid, 404, "Claude CLI is turned off on this router (settings → Claude CLI)"), model
	}
	if strings.HasSuffix(r.URL.Path, "/count_tokens") {
		return writeJSON(w, r.rid, 200, map[string]int{"input_tokens": len(r.body) / 4}), model
	}
	tools, _ := body["tools"].([]any)
	if len(tools) > 0 { // sync: ena zahteva agenta naenkrat na kopijo
		copyDir, err := s.hub.lock(r.Context(), r.h("x-claude-mini-session"), r.owner(), r.h("x-claude-mini-share") == "1")
		if err != nil {
			return 499, model // Claude Code je med čakanjem prekinil
		}
		if copyDir != "" {
			defer s.hub.unlock(copyDir)
		}
	}
	return s.runCLI(w, r, body, cliModel, chat || len(tools) == 0), model
}

func (s *server) runCLI(w http.ResponseWriter, r *req, body map[string]any, cliModel string, chat bool) int {
	all := turns(body)
	if len(all) == 0 || all[len(all)-1].role != "user" {
		return writeError(w, r.rid, 400, "Last message must be from the user")
	}
	last, history := all[len(all)-1], all[:len(all)-1]
	args := []string{"-p", "--output-format", "stream-json", "--include-partial-messages", "--verbose",
		"--model", cliModel, "--strict-mcp-config", "--setting-sources", s.conf().SettingSources} // privzeto prazno: hooki iz mape odjemalca ne tečejo
	prompt, session, scope, sessionID := last.text, r.h("x-claude-mini-session"), "", ""
	var dir, extra string // extra: --append-system-prompt-file ali --system-prompt-file
	extraFlag := "--append-system-prompt-file"
	if !chat {
		clientCwd, folder := r.h("x-claude-mini-cwd"), s.conf().Folder
		share := s.hub.waitShare(r.Context(), session, r.owner(), r.h("x-claude-mini-share") == "1")
		switch {
		case share != nil:
			if dir = s.hub.push(share); dir == "" {
				return writeError(w, r.rid, 502, "Syncing "+clientCwd+" to the router failed (see minigw output)")
			}
			note := "The user's folder " + clientCwd + " (on their computer) is synced here as " + dir + ", your working directory: a local copy"
			if share.readOnly {
				note += ". Map paths under " + clientCwd + " to it. The folder is shared read-only: do not try to create, change or delete files in it."
			} else {
				note += " whose changed source files are copied back to the user when you finish. Map paths under " + clientCwd + " to it."
			}
			extra = note
		case s.hub.isRefused(session): // odjemalec je poslal mapo brez sync
			return writeError(w, r.rid, 400, refusedDirect)
		case folder == "" && r.h("x-claude-mini-share") == "1": // mapa je bila napovedana, a ni prišla
			return writeError(w, r.rid, 400, "The folder "+clientCwd+" did not reach this router: see the minigw message when it started")
		case folder == "": // odjemalec mape ni delil (npr. /tmp, ki se ne deli samodejno)
			return writeError(w, r.rid, 400, "The folder "+clientCwd+" is not shared with the router, so Claude CLI agents cannot work in it: "+
				"in that folder run minigw share add (or MINIGW_SHARE=1 for one run), then start minigw again")
		default:
			dir = folder
			extra = "Your working directory is " + dir + " on the router, not the user's folder " + clientCwd + "."
		}
		scope = session + "|" + dir
		if id := s.sessions.get(transcriptKey(history, scope)); id != "" {
			args = append(args, "--resume", id)
			sessionID = id
		} else {
			sessionID = newUUID()
			args = append(args, "--session-id", sessionID)
			prompt = cliPrompt(history, last)
		}
		args = append(args, s.agentArgs(dir)...)
	} else {
		dir = s.conf().scratch("chat")
		prompt = cliPrompt(history, last)
		args = append(args, "--tools", "", "--no-session-persistence")
		extra, extraFlag = systemText(body["system"]), "--system-prompt-file"
	}
	if extra != "" { // prosto besedilo (glave odjemalca, sistemski poziv) nikoli v argv
		f, err := s.promptFile(extra)
		if err != nil {
			return writeError(w, r.rid, 500, "Cannot write the prompt file: "+err.Error())
		}
		defer os.Remove(f)
		args = append(args, extraFlag, f)
	}

	exe := s.claudeExe()
	if len(exe) == 0 {
		return writeError(w, r.rid, 503, "claude not found on the router (install Claude Code or set \"claude\" in the config)")
	}
	select { // največ 6 procesov claude hkrati
	case s.procs <- struct{}{}:
		defer func() { <-s.procs }()
	case <-r.Context().Done():
		return 499
	}
	cmd := exec.CommandContext(r.Context(), exe[0], append(exe[1:], args...)...)
	cmd.Dir = dir
	for _, e := range os.Environ() { // sicer bi notranji claude klical nazaj ta router
		if !strings.HasPrefix(e, "ANTHROPIC_") && !strings.HasPrefix(e, "CLAUDE_CODE_") && !strings.HasPrefix(e, "CLAUDECODE=") {
			cmd.Env = append(cmd.Env, e)
		}
	}
	cmd.Stdin = strings.NewReader(prompt)
	stderr := &tailBuffer{}
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		return writeError(w, r.rid, 500, "Cannot start claude: "+err.Error())
	}

	idb := make([]byte, 16)
	rand.Read(idb)
	model, _ := body["model"].(string)
	t := &translator{model: model, id: fmt.Sprintf("msg_%x", idb)}
	stream := body["stream"] == true
	var mu sync.Mutex // ping in dogodki iz dveh gorutin
	send := func(b []byte) {
		if !stream || len(b) == 0 {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		w.Write(b)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}
	stopPing := make(chan struct{})
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("request-id", r.rid)
		w.WriteHeader(200)
		send(t.start())
		go func() { // notranji agent lahko dolgo dela brez dogodkov
			tick := time.NewTicker(15 * time.Second)
			defer tick.Stop()
			for {
				select {
				case <-tick.C:
					send(sse("ping", map[string]string{"type": "ping"}))
				case <-stopPing:
					return
				}
			}
		}()
	}
	defer close(stopPing)
	br := bufio.NewReader(stdout)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			before := t.step
			send(t.feed(line))
			if t.step != before {
				s.log.Printf("%s step: %q", r.rid, t.step)
			}
		}
		if err != nil {
			break
		}
	}
	werr := cmd.Wait()
	if r.Context().Err() != nil {
		return 499 // Claude Code je prekinil
	}
	// sync: spremembe agenta k odjemalcu pred koncem odgovora (Claude Code jih nato že vidi); med tem še ping
	if !chat {
		s.hub.syncBack(session)
	}
	msg := t.resultError
	if msg == "" && werr != nil && t.text == "" {
		if msg = strings.TrimSpace(stderr.String()); msg == "" {
			msg = "claude failed: " + werr.Error()
		}
	}
	if msg != "" {
		s.log.Printf("%s claude failed: %s", r.rid, msg)
		if !stream {
			return writeError(w, r.rid, 502, "claude: "+msg)
		}
		if t.text == "" {
			send(sse("error", map[string]any{"type": "error", "error": map[string]string{"type": "api_error", "message": "claude: " + msg}}))
			return 502
		}
	}
	if !chat {
		s.sessions.set(transcriptKey(append(all, turn{"assistant", strings.TrimSpace(t.text)}), scope), sessionID)
	}
	if stream {
		send(t.finish())
		return 200
	}
	return writeJSON(w, r.rid, 200, t.message())
}
