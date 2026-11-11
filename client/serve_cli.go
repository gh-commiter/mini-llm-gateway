package main

// minigw serve: ponudnik claude -p (kot RouterCore/ClaudeCLI.swift). claude-cli-<m> je agent z orodji v kopiji mape
// odjemalca; zahteve brez orodij (naslov seje, povzetki) so pogovor brez orodij.
// Izhod stream-json se pretvori v en Anthropic odgovor z enim besedilnim blokom.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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
			p = lookPath("claude.exe")
		}
		if p == "" {
			if p = lookPath("claude"); p == "" {
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
	node := lookPath("node.exe")
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
// readOnly (--minigw-ro): le orodja s seznama dovoljenih (roAllowed), sandbox ne piše v mape; na Windows (brez sandboxa) brez Bash.
func (s *server) agentArgs(dir string, readOnly bool) []string {
	c := s.conf()
	dirs := []string{dir} // delovna mapa in dovoljene mape (kot allowedRoots na Macu)
	for _, f := range c.AllowedFolders {
		if f != dir && !scratchHiddenOverlaps(f, c.Scratch) && len(copilotCacheOverlaps([]string{f})) == 0 && isDir(f) { // L1, #26
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
	var deny []string // #83: mapa nastavitev dobesedno in kanonično (Claude Code primerja tudi cilj povezave/stičišča)
	for _, d := range scratchPaths(s.cfgDir) {
		cfg := ruleAbs(d) + "/**"
		deny = append(deny, "Read("+cfg+")", "Edit("+cfg+")", "Write("+cfg+")")
	}
	var gitDeny []string // M4 (kot ClaudeCLI.gitDenies): .git, ki ga bere git lastnika (nastavitve, hooki, preusmeritve)
	if readOnly {
		deny = append(deny, roDenied()...)
	} else {
		for _, d := range dirs {
			for _, g := range gitDenies {
				gitDeny = append(gitDeny, filepath.Join(d, ".git", g))
				for _, t := range []string{"Edit", "Write"} { // tudi na Windows, kjer sandboxa ni
					deny = append(deny, t+"("+ruleAbs(d)+"/.git/"+g+")", t+"("+ruleAbs(d)+"/.git/"+g+"/**)")
				}
			}
		}
	}
	var args []string
	box := windowsBox() // #68: ves Claude v škatli MXC; ukazi z orodjem PowerShell (Git Bash je MSYS2, v škatli umre, W4)
	if box && !readOnly {
		deny = append(deny, "Bash")
	}
	if c.AllowLocalTools { // vsa orodja Claude Code razen izjem; datoteke le v mapah, Bash v sandboxu (kot na Macu)
		var allowed []string
		for t := range pathTools {
			if !slices.Contains(c.DisabledTools, t) && (!readOnly || roAllowed(t)) {
				allowed = append(allowed, scope(t)...)
			}
		}
		if box && !readOnly && !slices.Contains(c.DisabledTools, "Bash") && !slices.Contains(c.DisabledTools, "PowerShell") {
			allowed = append(allowed, "PowerShell") // kot autoAllowBashIfSandboxed: škatla je peskovnik
		}
		slices.Sort(allowed)
		tools := "default"
		if readOnly { // le seznam dovoljenih: nova orodja, ki pišejo, so izključena
			var ro []string
			for _, t := range []string{"Read", "Grep", "Glob", "LS", "WebSearch", "WebFetch", "Bash"} {
				if roAllowed(t) && !slices.Contains(c.DisabledTools, t) {
					ro = append(ro, t)
				}
			}
			tools = strings.Join(ro, ",")
		}
		args = []string{"--tools", tools, "--allowedTools", strings.Join(allowed, ","), "--permission-mode", "acceptEdits",
			"--disallowedTools", strings.Join(append(deny, c.DisabledTools...), ",")}
	} else {
		var tools, allowed []string
		for _, t := range strings.Split(c.Tools, ",") {
			switch t = strings.TrimSpace(t); {
			case t == "", readOnly && !roAllowed(strings.SplitN(t, "(", 2)[0]):
				continue
			case pathTools[t]:
				allowed = append(allowed, scope(t)...)
			case box && t == "Bash": // #68: »Bash« iz nastavitev je v škatli PowerShell
				t = "PowerShell"
				allowed = append(allowed, t)
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
		// #67: koreni tempRoot ne v denyRead: v njih so le povezave runTempLink v $MINIGW_DEPS/tmp pod Scratch (ta je v
		// denyRead razen lastnega klona), skrit koren pa bi v peskovniku skril tudi povezavo teka (TMPDIR ukazov)
		settings, _ := json.Marshal(map[string]any{"sandbox": map[string]any{
			"enabled": true, "failIfUnavailable": hasBash(c.Tools) || c.AllowLocalTools, "autoAllowBashIfSandboxed": true, "allowUnsandboxedCommands": false,
			"network": map[string]any{"allowedDomains": []string{"*"}},
			// M1: Scratch (kopije drugih naprav, zrcala) tudi zunaj domače mape; mapa seje v njem ostane berljiva (allowRead).
			// #26 (N2): paket Copilot nikoli za pisanje (naloži ga priprava paketa), tudi če je v dovoljeni mapi.
			// --minigw-ro: pisanje v te mape prepovedano (denyWrite velja tudi za delovno mapo, ki jo Claude Code dovoli sam).
			"filesystem": map[string]any{"denyRead": slices.Concat([]string{"~"}, scratchPaths(s.cfgDir), scratchHidden(c.Scratch)), "allowRead": dirs, "allowWrite": onlyIf(!readOnly, dirs),
				"denyWrite": append(append(scratchPaths(copilotCacheDir()), onlyIf(readOnly, dirs)...), gitDeny...)},
		}})
		args = append(args, "--settings", string(settings))
	}
	return args
}

var gitDenies = []string{"config", "config.worktree", "hooks", "info", "commondir", "worktrees"}

const readOnlyWorktree = "Worktree mode cannot be read-only (--minigw-ro): use direct or sync"

// roAllowed: orodje, ki ga ima Claude CLI pri --minigw-ro (seznam dovoljenih kot ClaudeCLIConfig.readOnlyAllowed na Macu);
// Bash le zunaj Windows, kjer ga sandbox brez pisanja v mape omeji.
func roAllowed(t string) bool {
	return slices.Contains([]string{"Read", "Grep", "Glob", "LS", "WebSearch", "WebFetch"}, t) || t == "Bash" && runtime.GOOS != "windows"
}

// roDenied: pri --minigw-ro še izrecno prepovedana orodja, ki pišejo ali poganjajo ukaze mimo sandboxa Bash
// (EnterWorktree: git worktree add s hooki); Bash in PowerShell na Windows, kjer ni sandboxa.
func roDenied() []string {
	t := []string{"Edit", "Write", "MultiEdit", "NotebookEdit", "EnterWorktree", "ExitWorktree", "Monitor"}
	if runtime.GOOS == "windows" {
		t = append(t, "Bash", "PowerShell")
	}
	return t
}

// onlyIf: dirs, ko je ok, sicer prazen seznam (JSON [] namesto null).
func onlyIf(ok bool, dirs []string) []string {
	if ok {
		return dirs
	}
	return []string{}
}

// scratchPaths: Scratch (ali mapa nastavitev) dobesedno in kanonično (za denyRead peskovnika Claude, prepovedi orodij).
func scratchPaths(scratch string) []string {
	if scratch == "" {
		return nil
	}
	out := []string{scratch}
	if r, err := finalPath(scratch); err == nil && r != scratch { // #83: na Windows tudi stičišča
		out = append(out, r)
	}
	return out
}

// scratchOverlaps (H1): dovoljene mape, ki vsebujejo Scratch ali so v njem (kanonično). #83: mapa, ki je ni mogoče
// razrešiti (ali Scratch), šteje kot prekrivanje.
func scratchOverlaps(allowed []string, scratch string) []string {
	if scratch == "" {
		return nil
	}
	sc, err := realPath(scratch)
	var bad []string
	for _, a := range allowed {
		if ra, err2 := realPath(a); err != nil || err2 != nil || under(sc, ra) || under(ra, sc) {
			bad = append(bad, a)
		}
	}
	return bad
}

// isDir (#83): dovoljena mapa, ki je zdaj ni (pogon odklopljen), ne gre agentom (kot fileExists na Macu).
func isDir(p string) bool { st, err := os.Stat(p); return err == nil && st.IsDir() }

// realPath (#83): kanonična pot (finalPath: na Windows tudi stičišča, preslikan pogon → UNC); za pot, ki je še ni, prek
// najbližje obstoječe nadrejene mape. Navzgor le, ko pot res ne obstaja; vsaka druga napaka (dostop zavrnjen, visoča
// povezava, ime 8.3 brez dolge oblike) je napaka: varovala zavrnejo, ne primerjajo sestavljene poti. Pot na pogonu, ki
// ga ni (odklopljen disk z dovoljeno mapo), ostane, kot je.
func realPath(p string) (string, error) {
	p, rest := filepath.Clean(p), ""
	for {
		r, err := finalPath(p)
		if err == nil {
			return filepath.Join(r, rest), nil
		}
		if _, lerr := os.Lstat(p); !errors.Is(lerr, fs.ErrNotExist) {
			return "", fmt.Errorf("cannot resolve the real path of %s: %v", p, err)
		}
		parent := filepath.Dir(p)
		if parent == p { // nič ne obstaja (pogon ni priklopljen): ni povezave, ki bi ji sledili
			return filepath.Join(p, rest), nil
		}
		rest, p = filepath.Join(filepath.Base(p), rest), parent
	}
}

// sameReal: a in b sta po realPath ista pot (samePath); napaka = ne.
func sameReal(a, b string) bool {
	ra, err1 := realPath(a)
	rb, err2 := realPath(b)
	return err1 == nil && err2 == nil && samePath(ra, rb)
}

// scratchProblem (M1): Scratch ni koren diska, domača mapa ali mapa nad njo, začasna mapa ali mapa v dovoljeni mapi
// agentov (ali obratno): tam bi agenti brali kopije drugih naprav. "" = v redu. #83: nerazrešljiva pot = zavrnjeno.
func scratchProblem(scratch string, allowed []string) string {
	var bad error
	real := func(p string) string {
		r, err := realPath(p)
		if err != nil && bad == nil {
			bad = err
		}
		return r
	}
	sc := real(scratch)
	if bad != nil {
		return bad.Error()
	}
	if filepath.Dir(sc) == sc {
		return "it is a disk root"
	}
	if home, _ := os.UserHomeDir(); home != "" && under(real(home), sc) {
		return "it is your home folder or contains it"
	}
	for _, t := range []string{os.TempDir(), "/tmp", "/var/tmp", "/private/tmp", "/private/var/folders"} {
		if t != "" && under(sc, real(t)) {
			return "it is a temporary folder"
		}
	}
	for _, a := range allowed {
		if ra := real(a); under(sc, ra) || under(ra, sc) {
			return "it is inside or contains the allowed folder " + a
		}
	}
	if bad != nil {
		return bad.Error()
	}
	return ""
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
		for _, m := range cliFamilies {
			data = append(data, map[string]any{"type": "model", "id": cliAgentPrefix + m,
				"display_name": strings.ToUpper(m[:1]) + m[1:] + " · Claude CLI",
				"description":  "claude -p · " + m + " · tools in your synced folder", "created_at": "2026-01-01T00:00:00Z"})
		}
	}
	data = append(append(append(append(data, s.agentModelList(codexKind)...), s.agentModelList(grokKind)...), s.agentModelList(copilotKind)...), s.foundryModelList()...)
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
	stop                               string // stop_reason; prazno = end_turn
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

func (t *translator) stopReason() string {
	if t.stop == "" {
		return "end_turn"
	}
	return t.stop
}

func (t *translator) tokens(k string) int { f, _ := t.usage[k].(float64); return int(f) }

func (t *translator) finish() []byte {
	return append(append(sse("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0}),
		sse("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": t.stopReason(), "stop_sequence": nil},
			"usage": map[string]int{"input_tokens": t.tokens("input_tokens"), "output_tokens": t.tokens("output_tokens")}})...), // vhod šele iz rezultata CLI (kot API; zgodovina sej)
		sse("message_stop", map[string]string{"type": "message_stop"})...)
}

func (t *translator) message() map[string]any {
	return map[string]any{"id": t.id, "type": "message", "role": "assistant", "model": t.model,
		"content": []any{map[string]string{"type": "text", "text": t.text}}, "stop_reason": t.stopReason(), "stop_sequence": nil,
		"usage": map[string]int{"input_tokens": t.tokens("input_tokens"), "output_tokens": t.tokens("output_tokens")}}
}

// earlyStream (#4): agent z orodji v mapi odjemalca in pretokom: glava SSE takoj (prenos mape lahko traja).
func earlyStream(w http.ResponseWriter, r *req, body map[string]any) *earlyWriter {
	tools, _ := body["tools"].([]any)
	if body["stream"] != true || len(tools) == 0 || r.h("x-claude-mini-session") == "" || strings.HasSuffix(r.URL.Path, "/count_tokens") {
		return nil
	}
	return newEarlyWriter(w, r.rid)
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
	defer s.activity.start(r, model)()
	if body["stream"] != true && !strings.HasSuffix(r.URL.Path, "/count_tokens") { // ponovitev po omejitvi v toku (noteLimit)
		if msg, status, ok := s.recentLimit(r, model); ok {
			w.Header().Set("x-should-retry", "false")
			return writeAgentError(w, r.rid, status, msg), model
		}
	}
	_, _, isCLI := cliMatch(model)                     // model claude-cli ne čaka na odkrivanje prehodov
	if g, m, ok := s.gatewayRoute(model, !isCLI); ok { // prehodi pred claude-cli (kot Router.swift); ID-ji z »/« (OpenRouter)
		return s.passthrough(w, r, g, m), model
	}
	for _, k := range []agentKind{codexKind, grokKind, copilotKind} { // kot na Macu: claude-codex…, claude-grok…, claude-copilot…
		m, ok := k.match(model)
		if !ok {
			continue
		}
		if k.name == "copilot" && copilotUnsupported { // razlog namesto 404, ki ga Claude Code pokaže kot »model may not exist«
			w.Header().Set("x-should-retry", "false")
			return writeError(w, r.rid, 400, copilotUnsupportedMsg), model
		}
		if ac := k.conf(s.conf()); !ac.Enabled || (m != "" && !ac.allowsModel(k, m)) {
			return writeError(w, r.rid, 404, "Unknown model: "+model), model
		}
		if strings.HasSuffix(r.URL.Path, "/count_tokens") {
			return writeJSON(w, r.rid, 200, map[string]int{"input_tokens": len(r.body) / 4}), model
		}
		if ew := earlyStream(w, r, body); ew != nil { // #4: tudi čakanje na drugega agenta v isti mapi
			defer ew.done()
			w = ew
		}
		if tools, _ := body["tools"].([]any); len(tools) > 0 && runOf(r) == nil { // sync: ena zahteva agenta naenkrat na kopijo (vprašani agent: mapa je že klicateljeva)
			copyDir, err := s.hub.lock(r.Context(), r.h("x-claude-mini-session"), r.owner(), r.h("x-claude-mini-share") == "1")
			if err != nil {
				return 499, model
			}
			if copyDir != "" {
				defer s.hub.unlock(copyDir)
			}
		}
		return s.runAgent(w, r, body, k, m), model
	}
	if m, ok := strings.CutPrefix(model, foundryPrefix); ok { // Foundry Local (na napravi, kot Apple FM na Macu)
		if !foundryValidModel(m) || !s.conf().Foundry.Enabled {
			return writeError(w, r.rid, 404, "Unknown model: "+model), model
		}
		if strings.HasSuffix(r.URL.Path, "/count_tokens") {
			return writeJSON(w, r.rid, 200, map[string]int{"input_tokens": len(r.body) / 4}), model
		}
		return s.runFoundry(w, r, body, m), model
	}
	cliModel, chat, ok := cliMatch(model)
	if !validModel.MatchString(model) || !validModel.MatchString(cliModel) {
		return writeError(w, r.rid, 400, "Invalid model name"), "?"
	}
	if !ok {
		return writeError(w, r.rid, 404, "Unknown model: "+model), model
	}
	if s.conf().ClaudeDisabled {
		return writeError(w, r.rid, 404, "Claude CLI is turned off on this gateway (settings → Claude CLI)"), model
	}
	if strings.HasSuffix(r.URL.Path, "/count_tokens") {
		return writeJSON(w, r.rid, 200, map[string]int{"input_tokens": len(r.body) / 4}), model
	}
	tools, _ := body["tools"].([]any)
	if !chat { // #4: glava SSE pred čakanjem na mapo in prenosom
		if ew := earlyStream(w, r, body); ew != nil {
			defer ew.done()
			w = ew
		}
	}
	if len(tools) > 0 && runOf(r) == nil { // sync: ena zahteva agenta naenkrat na kopijo (vprašani agent: mapa je že klicateljeva)
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
	if why := chainRefusal(r, "claude"); why != "" { // vprašani agent: globina in brez ponovitve vrste (tudi pogovor)
		return writeError(w, r.rid, 403, why)
	}
	all := turns(body)
	if len(all) == 0 || all[len(all)-1].role != "user" {
		return writeError(w, r.rid, 400, "Last message must be from the user")
	}
	last, history := all[len(all)-1], all[:len(all)-1]
	// N9: besedilo naprave je vnos uporabnika claude -p: brez ukazov s poševnico (/config piše nastavitve lastnika
	// prehoda) in brez »@pot« (CLAUDE_CODE_DISABLE_FILE_MENTIONS spodaj: priložil bi datoteko prehoda mimo peskovnika)
	args := []string{"-p", "--disable-slash-commands", "--output-format", "stream-json", "--include-partial-messages", "--verbose",
		"--model", cliModel, "--strict-mcp-config", "--setting-sources", s.conf().SettingSources} // privzeto prazno: hooki iz mape odjemalca ne tečejo
	prompt, session, scope, sessionID := last.text, r.h("x-claude-mini-session"), "", ""
	var dir, extra string                    // extra: --append-system-prompt-file ali --system-prompt-file
	var worktreeEnv, runEnv, grants []string // grants: #67 H1 (grantDirs)
	var run *agentRun                        // »May ask other models« (serve_agentcalls.go)
	var wt, maven, objects string
	parent := runOf(r) // vprašal ga je drug agent tega prehoda (serve_agentcalls.go)
	extraFlag := "--append-system-prompt-file"
	box := windowsBox() // #68: ves claude v škatli MXC (Windows)
	var boxRW, boxRO []string
	boxTemp, netOff := "", false
	if !chat {
		if why := s.conf().unsandboxedRefusal(s.cfgDir); why != "" { // #68: prej ob nalaganju nastavitev
			return writeError(w, r.rid, 403, why)
		}
		clientCwd := r.h("x-claude-mini-cwd")
		var share *serveShare
		if parent == nil {
			share = s.hub.waitShare(r.Context(), session, r.owner(), r.h("x-claude-mini-share") == "1")
		}
		switch {
		case parent != nil: // mapa klicatelja, brez novega prenosa
			dir, extra = parent.dir, nestedNote(parent)
		case share != nil && share.tree != nil:
			if (worktreeNeedsUnsandboxed || runtime.GOOS == "windows" && !box) && !s.conf().AllowUnsandboxedBash { // L3; box iz začetka zahteve (ponovna sonda med waitShare)
				return writeError(w, r.rid, 400, "Claude CLI in worktree mode on Windows needs \"Allow commands without a sandbox\" "+
					"(settings page → Claude CLI): Claude Code's own git runs there without a sandbox; use sync")
			}
			if r.h("x-claude-mini-ro") == "1" { // I4, L2: pred WORKTREE-PUSH
				return writeError(w, r.rid, 400, readOnlyWorktree)
			}
			var refused string
			if dir, refused = s.hub.push(share, "claude"); refused != "" {
				return writeError(w, r.rid, 400, refused) // N1: končna, Claude Code je ne ponovi
			} else if dir == "" {
				return writeError(w, r.rid, 502, "Cloning "+clientCwd+" on the gateway failed (worktree mode; see minigw output and the gateway log)")
			}
			wt, branch, _, _, _ := s.hub.worktreeOf(session, "claude")
			gits, _ := s.hub.worktreeGits(wt)
			extra = worktreeNote(clientCwd, dir, branch, gits, true) // peskovnik Claude Code: SANDBOX_RUNTIME (Windows: depsNote)
		case share != nil:
			if dir, _ = s.hub.push(share, "claude"); dir == "" {
				return writeError(w, r.rid, 502, "Syncing "+clientCwd+" to the gateway failed (see minigw output)")
			}
			extra = syncNote(clientCwd, dir, share.readOnly)
		case s.hub.isRefused(session): // odjemalec je poslal mapo brez sync
			return writeError(w, r.rid, 400, refusedDirect)
		case r.h("x-claude-mini-share") == "1": // mapa je bila napovedana, a ni prišla
			return writeError(w, r.rid, 400, "The folder "+clientCwd+" did not reach this gateway: see the minigw message when it started")
		default: // odjemalec mape ni delil (npr. /tmp); privzete mape na prehodu ni (kot na Macu)
			return writeError(w, r.rid, 400, "The folder "+clientCwd+" is not shared with the gateway, so Claude CLI agents cannot work in it: "+
				"in that folder run minigw share add (or MINIGW_SHARE=1 for one run), then start minigw again")
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
		cap := s.capped(r, "claude") // #45: način seje nad omejitvijo → le branje
		if cap != "" {
			extra = cappedNote(r, dir, extra)
		}
		ro := (share != nil && share.readOnly) || (parent != nil && parent.readOnly) || r.h("x-claude-mini-ro") == "1" || cap != ""
		// #46a: zgodovina zapiše, kar je veljalo
		r.applied = &appliedLimits{readOnly: ro, cap: cap}
		s.boxNote(r, "claude") // #68
		args = append(args, s.agentArgs(dir, ro)...)
		if ro { // #68: mapa in dovoljene mape v škatli
			boxRO = append(boxRO, dir)
		} else {
			boxRW = append(boxRW, dir)
		}
		for _, f := range s.conf().AllowedFolders {
			if f != dir && !scratchHiddenOverlaps(f, s.conf().Scratch) && len(copilotCacheOverlaps([]string{f})) == 0 && isDir(f) { // kot agentArgs
				if ro {
					boxRO = append(boxRO, f)
				} else {
					boxRW = append(boxRW, f)
				}
			}
		}
		ok := false
		if wt, _, maven, objects, ok = s.hub.worktreeOf(session, "claude"); parent != nil {
			wt, maven, objects, ok = parent.wt, parent.maven, parent.objects, parent.wt != ""
		}
		if ok && ro { // L2: kot na Macu
			return writeError(w, r.rid, 400, readOnlyWorktree)
		}
		if ok && !s.hub.worktreeIntact(wt) { // H2: git samega claude (zunaj peskovnika) ne z .git, ki ga je zamenjal agent
			return writeError(w, r.rid, 400, "The agent's clone was changed (its .git was replaced): start a new conversation")
		}
		if ok {
			gits, objs := s.hub.worktreeGits(wt)             // ugnezdeni kloni enako kot vrhnji
			if why := foreignGit(wt, dir, gits); why != "" { // #61: tudi v klonu klicatelja
				return writeError(w, r.rid, 400, why)
			}
			if len(objs) == 0 {
				objs = []string{objects}
			}
			// #67 H1: mape za pisanje pod Scratch preverjene, preden nastane peskovnik
			var fixed []string
			var err error
			if grants, fixed, err = grantDirs(wt); err != nil {
				s.log.Printf("%s worktree folders: %v", r.rid, err) // L3: pot le v dnevnik
				return writeError(w, r.rid, 503, "The agent's folders on the gateway are not safe to use (see the gateway log): start a new conversation")
			}
			for _, f := range fixed {
				s.log.Printf("%s worktree: %s was not a folder of the gateway; made again", r.rid, f)
			}
			defer s.recheckGrants(r.rid, wt) // #73: po teku znova
			// kratek TMPDIR: vtičnice peskovnika Claude Code (maxRunTemp); #67: povezava, podatki pod Scratch ($MINIGW_DEPS/tmp);
			// Windows (brez peskovnika): kar $MINIGW_DEPS/tmp
			tmpRoot, tmp, done := "", filepath.Join(depsDir(wt), "tmp"), func() {}
			if runtime.GOOS != "windows" {
				tmpRoot = s.tempRoot()
				if tmp, done, err = runTempLink(tmpRoot, tmp); err != nil {
					s.log.Printf("%s run temp: %v", r.rid, err) // L3: pot le v dnevnik
					return writeError(w, r.rid, 500, "Cannot create a temporary folder for the agent (see the gateway log)")
				}
			}
			defer done()
			args = worktreeClaudeArgs(args, wt, grants, tmpRoot, objs, gits)
			gw := s.conf().scratch("gitexec")
			if tooDeep(wt, gits) {
				return writeError(w, r.rid, 400, tooManyPins)
			}
			if worktreeEnv = worktreeClaudeEnv(wt, maven, tmp, gw, gits, "claude"); worktreeEnv == nil {
				return writeError(w, r.rid, 503, "Cannot prepare git for the agent's clone (git --exec-path): use sync")
			}
			worktreeEnv = append(worktreeEnv, "CLAUDE_CODE_TMPDIR="+tmp) // #67: kratka povezava (zadnja vrednost velja)
			args = sandboxAllowRead(args, gw)
			// #68 (odločitev 7): »Network for builds« kot na Macu: brez omrežja ukazi brez omrežja, ovoj git zavrne oddaljen clone
			if netOff = !s.conf().worktreeNet(); netOff {
				args = sandboxNetwork(args, []string{})
				worktreeEnv = append(worktreeEnv, networkOffEnv("claude", false)...)
			}
			boxRW = append(append(boxRW, wt), grants...)
			boxRO = append(append(boxRO, objs...), gw)
			boxTemp = tmp
		}
		shared := share != nil || (parent != nil && parent.shared)
		run, runEnv = s.startRun(r, "claude", dir, ro, shared, wt, maven, objects, "") // »May ask other models«
		defer run.stop()
		if run.asks() {
			boxRO = append(boxRO, filepath.Dir(run.authFile))
			exe := minigwExe()
			onPath := s.conf().askOnPath(dir)
			extra += "\n\n" + askNote(exe, onPath)
			args = sandboxAllowRead(args, exe, run.authFile) // M3: le ta datoteka iz Scratch
			if i := slices.Index(args, "--allowedTools"); onPath && i >= 0 {
				args[i+1] += "," + askRules
			}
		} else {
			extra += "\n\n" + noAskNote
		}
	} else {
		dir = s.conf().scratch("chat")
		boxRO = append(boxRO, dir)
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
		boxRO = append(boxRO, f)
	}

	exe := s.claudeExe()
	if len(exe) == 0 {
		return writeError(w, r.rid, 503, "claude not found on the gateway (install Claude Code or set \"claude\" in the config)")
	}
	if st := s.procSlot(w, r); st != 0 {
		return st
	}
	defer func() { <-s.procs }()
	name, cargs := exe[0], append(exe[1:], args...)
	unpinned := false // N3b: brez pin (ni bwrap ali gnezdeni ne deluje) se klon preverja med tekom (watch spodaj)
	// brez ANTHROPIC_* (klic nazaj v router), žetonov GitHub/Copilot in agentov ključev (M1)
	cmdEnv := append(append(append(agentEnv(os.Environ()), worktreeEnv...), runEnv...), "CLAUDE_CODE_DISABLE_FILE_MENTIONS=1")
	if p := envGet(worktreeEnv, "PATH"); p != "" && envGet(runEnv, "PATH") != "" { // askOnPath in ovoj git (#68): oba na PATH
		cmdEnv = envSet(cmdEnv, "PATH", filepath.Dir(minigwExe())+string(os.PathListSeparator)+p)
	}
	cmdEnv = append(cmdEnv, agentKeyEnv(s.cfgDir, "claude")...) // #68: ključ API le, če ga je uporabnik izbral
	// #68: ves claude v škatli MXC s prijavo »claude setup-token« (CLAUDE_CODE_OAUTH_TOKEN; ~\.claude se ne kopira) in
	// svežim CLAUDE_CONFIG_DIR ter domom vsak tek; med teki ostane le prepis seje (--resume) v stanju naprave.
	if box {
		key, token := agentAPIKey(s.cfgDir, "claude"), agentToken(s.cfgDir, "claude")
		if key == "" && token == "" {
			return writeError(w, r.rid, 403, "Claude CLI in the Windows sandbox needs a long-lived sign-in token: run claude setup-token, then minigw gw claude token")
		}
		if key == "" {
			cmdEnv = append(cmdEnv, "CLAUDE_CODE_OAUTH_TOKEN="+token)
		}
		runDir, err := os.MkdirTemp(s.conf().scratch("tmp"), "run-*")
		if err != nil {
			return writeError(w, r.rid, 500, "Cannot prepare Claude CLI's folder on the gateway: "+err.Error())
		}
		defer os.RemoveAll(runDir)
		conf := filepath.Join(runDir, "claude")
		store := ""
		if !chat {
			store = claudeSessionStore(s.conf().Scratch, r.owner(), sessionID)
			copyRegularTree(store, filepath.Join(conf, "projects"))
			defer func() { copyRegularTree(filepath.Join(conf, "projects"), store) }()
		}
		os.MkdirAll(conf, 0o700)
		cmdEnv = append(cmdEnv, "CLAUDE_CONFIG_DIR="+conf, "CLAUDE_CODE_USE_POWERSHELL_TOOL=1")
		if len(exe) > 1 { // node + cli.js
			boxRO = append(boxRO, filepath.Dir(exe[1]))
		}
		var apiIPs []net.IP
		if netOff { // #68: brez omrežja le naslovi API (razrešeni zdaj, pravila MXC so številska), vrata 443
			if apiIPs, err = mxcResolve(claudeAPIHost); err != nil || len(apiIPs) == 0 {
				s.log.Printf("%s claude: cannot resolve %s: %v", r.rid, claudeAPIHost, err)
				return writeError(w, r.rid, 503, "Cannot resolve "+claudeAPIHost+" for Claude CLI without network for builds; try again, or turn on "+claudeNetworkSetting)
			}
			s.log.Printf("%s claude: network for builds off, only %s %v:443", r.rid, claudeAPIHost, apiIPs)
		}
		var done func()
		var err2 error
		name, cargs, done, err2 = agentWrap(name, cargs, agentMounts{cfgDir: s.cfgDir, rw: append(boxRW, conf), ro: boxRO, hide: s.conf().Scratch,
			cmdEnv: cmdEnv, cwd: dir, home: filepath.Join(runDir, "home"), temp: boxTemp, noNet: netOff, allowIPs: apiIPs, loopback: run.asks()})
		if err2 != nil {
			s.log.Printf("%s claude: %v", r.rid, err2)
			return writeError(w, r.rid, 503, "Cannot start Claude CLI in the Windows sandbox: "+err2.Error())
		}
		defer done()
		unpinned = worktreeEnv != nil // odločitev: .git klona se preverja vsako sekundo tudi v škatli
	} else if worktreeEnv != nil { // N3: .git klona je med tekom točka priklopa (kot pins pri Codex), zamenjati ga ni mogoče
		gits, _ := s.hub.worktreeGits(wt)
		name, cargs = pinWrap(s.cfgDir, name, cargs, append(pinPaths(wt, gits), grants...), s.log.Printf) // M2: tudi mape do ugnezdenih klonov; #67 H1: mape pod Scratch (zamenjava: EBUSY)
		if unpinned = name == exe[0]; unpinned && runtime.GOOS == "linux" {
			if why := s.hub.markUnconfined(wt, "Claude CLI"); why != "" { // N1: brez imenskega prostora PID agent lahko preživi do finalize
				return writeError(w, r.rid, 400, why)
			}
		}
	}
	ctx, kill := context.WithCancel(r.Context())
	defer kill()
	var swapped atomic.Bool
	stopMoves := func() {}
	if unpinned { // N3c: brez pin vsak premik .git (tudi zamenjava, ki jo agent vrne pred naslednjo preverbo) ustavi tek
		gits, _ := s.hub.worktreeGits(wt)
		// vsak .git (vrhnji in ugnezdeni) in mape do ugnezdenih (M2)
		stop, ok := watchGitMoves(pinPaths(wt, gits), func() {
			if swapped.CompareAndSwap(false, true) {
				kill()
				killInside(wt) // tudi git, ki ga je claude že zagnal v klonu
			}
		})
		if !ok && runtime.GOOS == "linux" {
			return writeError(w, r.rid, 503, "Cannot watch the agent's clone (inotify) and nested bwrap does not work here, "+
				"so Claude CLI cannot run in worktree mode on this gateway now: use sync")
		}
		if ok {
			stopMoves = stop
			defer stop()
		}
		if !s.hub.worktreeIntact(wt) { // zamenjava med preverbo zgoraj in začetkom nadzora
			return writeError(w, r.rid, 400, "The agent's clone was changed (its .git was replaced): start a new conversation")
		}
	}
	cmd := commandContext(ctx, name, cargs...)
	cmd.Dir = dir
	cmd.Env = cmdEnv
	cmd.Stdin = strings.NewReader(prompt)
	stderr := &tailBuffer{}
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err == nil {
		err = startAgent(cmd) // Windows: v Job agentov (peer check; tudi claude v škatli, otrok wxc-exec)
	}
	if err != nil {
		return writeError(w, r.rid, 500, "Cannot start claude: "+err.Error())
	}
	run.started(cmd.Process.Pid)
	s.agentsRunning.Add(1)
	defer s.agentsRunning.Add(-1)

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
		w.Header().Set("x-should-retry", "false")
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
	// N3b: zamenjan .git klona (mv .git; mkdir .git s filter.*) bi med istim tekom izvedel lastni git Claude Code brez
	// peskovnika: ob vsakem dogodku in vsako sekundo preverba, ob neujemanju se claude ustavi (na Linux še inotify zgoraj).
	watch := func() {
		if unpinned && !swapped.Load() && !s.hub.worktreeIntact(wt) && swapped.CompareAndSwap(false, true) {
			kill()
			killInside(wt)
		}
	}
	if unpinned {
		go func() {
			tick := time.NewTicker(time.Second)
			defer tick.Stop()
			for {
				select {
				case <-tick.C:
					watch()
				case <-stopPing:
					return
				}
			}
		}()
	}
	defer context.AfterFunc(ctx, func() { stdout.Close() })() // N1: branje konča tudi, ko vnuk še drži stdout
	br := bufio.NewReader(stdout)
	for {
		line, err := br.ReadBytes('\n')
		watch()
		if len(line) > 0 {
			before := t.step
			send(t.feed(line))
			if t.step != before {
				s.log.Printf("%s step: %q", r.rid, t.step)
				s.activity.step(r.rid, t.step)
			}
		}
		if err != nil {
			break
		}
	}
	werr := cmd.Wait()
	treeErr := stopAgentTree(cmd) // Windows: drevo teka ubito pred prevzemom sprememb; sicer jih ne prevzame
	if treeErr != nil {
		s.log.Printf("%s claude: %v", r.rid, treeErr)
	}
	stopMoves()                                          // syncBack in konec seje .git klona lahko odstranita
	run.stop()                                           // I2: vprašanja drugim modelom se končajo pred prevzemom sprememb
	if worktreeEnv != nil && !s.hub.worktreeIntact(wt) { // N3: naslednji zagon ga zavrne (H2)
		s.log.Printf("%s claude: the agent's clone changed during the run (its .git was replaced)", r.rid)
	}
	if r.Context().Err() != nil {
		return 499 // Claude Code je prekinil
	}
	// sync: spremembe agenta k odjemalcu pred koncem odgovora (Claude Code jih nato že vidi); med tem še ping
	pullErr := "" // #82: delo agenta ni prišlo k odjemalcu → napaka, ne uspeh
	if !chat && treeErr == nil {
		// vprašani agent: spremembe prenese klicatelj na koncu; L1: omejen (#45) ne prenese ničesar
		if parent == nil && (r.applied == nil || r.applied.cap == "") {
			pullErr = s.hub.syncBack(session, "claude")
		}
	}
	msg := t.resultError
	if swapped.Load() {
		msg = "the agent replaced its clone's .git during the run (worktree mode), so the gateway stopped it"
	}
	if treeErr != nil {
		msg = "its processes did not stop after the run, so the gateway did not take its changes; details are in the gateway log, request " + r.rid
	}
	if msg == "" && werr != nil && t.text == "" { // stderr le v dnevnik (I1)
		if tail := strings.TrimSpace(stderr.String()); tail != "" {
			s.log.Printf("%s claude stderr: %s", r.rid, tail)
		}
		msg = "claude failed (" + werr.Error() + "); details are in the gateway log, request " + r.rid
	}
	if msg == "" {
		msg = pullErr
	}
	if msg != "" {
		s.log.Printf("%s claude failed: %s", r.rid, msg)
		status, clean := classifyAgentError(msg)
		if pullErr != "" { // #82: končna; Claude Code agenta ne zažene znova (spremembe bi se uveljavile dvakrat)
			w.Header().Set("x-should-retry", "false")
		}
		if !stream {
			return writeAgentError(w, r.rid, status, "claude: "+clean)
		}
		if t.text == "" || treeErr != nil || pullErr != "" {
			if pullErr != "" {
				s.noteFinal(r, model, status, "claude: "+clean)
			} else {
				s.noteLimit(r, model, status, "claude: "+clean)
			}
			send(sse("error", map[string]any{"type": "error", "error": map[string]string{"type": errType(status), "message": "claude: " + clean}}))
			return status
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
