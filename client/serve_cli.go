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

func (s *server) claudeExe() string {
	if s.cfg.Claude != "" {
		if st, err := os.Stat(s.cfg.Claude); err == nil && !st.IsDir() {
			return s.cfg.Claude
		}
		return ""
	}
	p, _ := exec.LookPath("claude")
	return p
}

func (s *server) modelList() map[string]any {
	data := []map[string]any{}
	if s.claudeExe() != "" {
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
	cliModel, chat, ok := cliMatch(model)
	if !ok {
		return writeError(w, r.rid, 404, "Unknown model: "+model), model
	}
	if strings.HasSuffix(r.URL.Path, "/count_tokens") {
		return writeJSON(w, r.rid, 200, map[string]int{"input_tokens": len(r.body) / 4}), model
	}
	tools, _ := body["tools"].([]any)
	if len(tools) > 0 { // sync: ena zahteva agenta naenkrat na kopijo
		copyDir, err := s.hub.lock(r.Context(), r.h("x-claude-mini-session"), r.deviceID(), r.h("x-claude-mini-share") == "1")
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
		"--model", cliModel, "--strict-mcp-config", "--setting-sources", ""} // hooki in nastavitve iz mape odjemalca ne tečejo tukaj
	prompt, session, scope, sessionID := last.text, r.h("x-claude-mini-session"), "", ""
	var dir string
	if !chat {
		clientCwd := r.h("x-claude-mini-cwd")
		share := s.hub.waitShare(r.Context(), session, r.deviceID(), r.h("x-claude-mini-share") == "1")
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
			args = append(args, "--append-system-prompt", note)
		case s.hub.isRefused(session) || s.cfg.Folder == "":
			return writeError(w, r.rid, 400, refusedDirect)
		default:
			dir = s.cfg.Folder
			args = append(args, "--append-system-prompt", "Your working directory is "+dir+" on the router, not the user's folder "+clientCwd+".")
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
		args = append(args, "--tools", s.cfg.Tools, "--allowedTools", s.cfg.Tools, "--permission-mode", s.cfg.PermissionMode)
	} else {
		dir = s.cfg.scratch("chat")
		prompt = cliPrompt(history, last)
		args = append(args, "--tools", "", "--no-session-persistence")
		if sys := systemText(body["system"]); sys != "" {
			args = append(args, "--system-prompt", sys)
		}
	}

	exe := s.claudeExe()
	if exe == "" {
		return writeError(w, r.rid, 503, "claude not found on the router (install Claude Code or set \"claude\" in the config)")
	}
	cmd := exec.CommandContext(r.Context(), exe, args...)
	cmd.Dir = dir
	for _, e := range os.Environ() { // sicer bi notranji claude klical nazaj ta router
		if !strings.HasPrefix(e, "ANTHROPIC_") && !strings.HasPrefix(e, "CLAUDE_CODE_") && !strings.HasPrefix(e, "CLAUDECODE=") {
			cmd.Env = append(cmd.Env, e)
		}
	}
	cmd.Stdin = strings.NewReader(prompt)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
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
				s.log.Printf("%s step: %s", r.rid, t.step)
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
		if msg = strings.TrimSpace(stderr.String()); len(msg) > 4000 {
			msg = msg[len(msg)-4000:]
		}
		if msg == "" {
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
