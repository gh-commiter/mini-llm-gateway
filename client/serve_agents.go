package main

// Codex CLI in Grok CLI kot agenta (kot RouterCore/Codex.swift in Grok.swift): claude-codex, claude-codex-<model>,
// claude-grok, claude-grok-<model>. Privzeto izklopljena; agent dela le v sinhronizirani kopiji mape odjemalca.
// Peskovnik postavi CLI sam (Codex: workspace-write / read-only, na Linuxu Landlock in seccomp; Grok: workspace /
// read-only). Oba bereta povsod: Grok dobi pravila --deny za mapo nastavitev; na Windows (brez preverjenega sandboxa)
// tečeta le z allowUnsandboxedBash. Skrbniški API zavrne procese agentov (peer check).

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
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

// agentCLI: nastavitve Codex ali Grok (JSON v API kot na Macu: enabled, path, write, network, models).
type agentCLI struct {
	Enabled    bool     `json:"enabled,omitempty"`
	Executable string   `json:"executable,omitempty"` // prazno = iz PATH
	Models     []string `json:"models"`               // null = vsi modeli računa (tudi prihodnji); seznam = le ti
	ReadOnly   bool     `json:"readOnly,omitempty"`   // brez pisanja v mapo seje
	Network    bool     `json:"network,omitempty"`    // Codex: omrežje za ukaze v sandboxu (privzeto izklopljeno)
}

func (a agentCLI) modelEnabled(m string) bool { return a.Models == nil || slices.Contains(a.Models, m) }

type agentKind struct {
	name, prefix, bin string
	conf              func(serveConfig) agentCLI
}

var (
	codexKind = agentKind{"codex", "claude-codex", "codex", func(c serveConfig) agentCLI { return c.Codex }}
	grokKind  = agentKind{"grok", "claude-grok", "grok", func(c serveConfig) agentCLI { return c.Grok }}
)

// match: »claude-codex« = privzeti model ("", true), »claude-codex-<m>« = m; ok=false, če ime ni tega agenta.
func (k agentKind) match(model string) (string, bool) {
	if model == k.prefix {
		return "", true
	}
	if m, ok := strings.CutPrefix(model, k.prefix+"-"); ok && m != "" {
		return m, true
	}
	return "", false
}

// validAgentModel: ime za -m: le [A-Za-z0-9._:/-], ne na začetku »-« (sicer zastavica CLI), kot Router.validModel.
var validAgentModel = regexp.MustCompile(`^[A-Za-z0-9._:/][A-Za-z0-9._:/-]{0,127}$`)

// resolveAgent: nastavljena pot ali iz PATH; na Windows le .exe (paketni ovoj npm bi tekel prek cmd.exe), ali
// izvorni codex.exe v paketu npm ob njem.
func resolveAgent(k agentKind, configured string) (string, error) {
	p := configured
	if p == "" {
		if runtime.GOOS == "windows" {
			p, _ = exec.LookPath(k.bin + ".exe")
		}
		if p == "" {
			if p, _ = exec.LookPath(k.bin); p == "" {
				return "", nil
			}
		}
	} else if st, err := os.Stat(p); err != nil || st.IsDir() {
		return "", fmt.Errorf("%s not found at %s", k.bin, p)
	}
	if runtime.GOOS != "windows" || strings.EqualFold(filepath.Ext(p), ".exe") {
		return p, nil
	}
	m, _ := filepath.Glob(filepath.Join(filepath.Dir(p), "node_modules", "@openai", "codex", "vendor", "*", "codex", "codex.exe"))
	if k.name == "codex" && len(m) > 0 {
		return m[0], nil
	}
	return "", fmt.Errorf("%s is not an .exe and minigw serve does not run batch files: install the native %s.exe or set its path", p, k.bin)
}

// agentModel: model računa za seznam in tabelo v nastavitvah.
type agentModel struct {
	Slug        string `json:"slug"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Window      int    `json:"window,omitempty"`
	Enabled     bool   `json:"enabled"`
}

var agentCatalogs sync.Map // ime|pot → katalog (5 min)

type cachedCatalog struct {
	models []agentModel
	at     time.Time
}

// agentCatalog: Codex »codex debug models« (JSON), Grok »grok models« (besedilo); največ 20 s, 5 min v predpomnilniku.
func agentCatalog(k agentKind, exe string) []agentModel {
	key := k.name + "|" + exe
	if v, ok := agentCatalogs.Load(key); ok && time.Since(v.(cachedCatalog).at) < 5*time.Minute {
		return v.(cachedCatalog).models
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	args := []string{"models"}
	if k.name == "codex" {
		args = []string{"debug", "models"}
	}
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Dir = os.TempDir()
	out, _ := cmd.Output()
	models := parseCodexCatalog(out)
	if k.name == "grok" {
		models = parseGrokCatalog(string(out))
	}
	if len(models) > 0 {
		agentCatalogs.Store(key, cachedCatalog{models, time.Now()})
	}
	return models
}

func parseCodexCatalog(data []byte) []agentModel {
	var list []map[string]any
	if json.Unmarshal(data, &list) != nil {
		var obj struct {
			Models []map[string]any `json:"models"`
		}
		json.Unmarshal(data, &obj)
		list = obj.Models
	}
	var out []agentModel
	for _, m := range list {
		slug, _ := m["slug"].(string)
		if slug == "" || m["visibility"] == "hide" {
			continue
		}
		name, _ := m["display_name"].(string)
		if name == "" {
			name = slug
		}
		desc, _ := m["description"].(string)
		win, _ := m["context_window"].(float64)
		out = append(out, agentModel{Slug: slug, Name: name, Description: desc, Window: int(win)})
	}
	return out
}

// parseGrokCatalog: vrstice za »Available models:« (»  * grok-4.7 (default)«).
func parseGrokCatalog(text string) []agentModel {
	_, rest, ok := strings.Cut(text, "Available models:")
	if !ok {
		return nil
	}
	var out []agentModel
	for _, line := range strings.Split(rest, "\n") {
		if f := strings.Fields(strings.Trim(strings.TrimSpace(line), "*- ")); len(f) > 0 {
			out = append(out, agentModel{Slug: f[0], Name: f[0]})
		}
	}
	return out
}

// codexArgs: kot Codex.args na Macu. Agent piše le v mapi seje in dovoljenih mapah (sandbox Codex); brez orodij
// (naslovi, povzetki) le branje in brez zapisa seje. resume: nit Codex za nadaljevanje (možnosti exec veljajo zanjo).
func codexArgs(cwd string, extraDirs []string, agent bool, output, model string, write, network bool, resume string) []string {
	a := []string{"exec", "--json", "--skip-git-repo-check", "--color", "never", "-C", cwd, "-o", output}
	if model != "" {
		a = append(a, "-m", model)
	}
	// Sandbox tudi kot -c: »exec resume« nima --sandbox in --add-dir, -c pa sprejme (I5).
	sandbox := []string{"-c", `sandbox_mode="read-only"`}
	if agent && write {
		a = append(a, "--sandbox", "workspace-write")
		for _, d := range extraDirs {
			a = append(a, "--add-dir", d)
		}
		roots, _ := json.Marshal(append([]string{}, extraDirs...)) // niz JSON je veljaven niz TOML
		sandbox = []string{"-c", `sandbox_mode="workspace-write"`, "-c", fmt.Sprintf("sandbox_workspace_write.network_access=%v", network),
			"-c", "sandbox_workspace_write.writable_roots=" + string(roots)}
	} else {
		a = append(a, "--sandbox", "read-only")
	}
	if !agent {
		a = append(a, "--ephemeral")
	}
	if resume != "" {
		a = append(a, "resume", resume)
	}
	return append(append(a, sandbox...), "-") // navodila prek stdin
}

// grokArgs: kot Grok.args na Macu; jedrni sandbox »workspace« piše le v mapo seje (dodatnih map ne dovoli).
// Mapa nastavitev (žeton, ključ routerja, admin.secret, ključi prehodov) je prepovedana z --deny; Grok jih uporabi tudi
// za poti v ukazih lupine. Pravilo ima absolutno pot z enim »/« (preverjeno z grok 1.0.77; »//« ne velja).
func grokArgs(cwd, promptFile, cfgDir string, agent bool, model string, write bool, resume string) []string {
	a := []string{"--cwd", cwd, "--output-format", "streaming-json", "--prompt-file", promptFile}
	if model != "" {
		a = append(a, "-m", model)
	}
	cfg := filepath.ToSlash(cfgDir) + "/**"
	a = append(a, "--deny", "Read("+cfg+")", "--deny", "Edit("+cfg+")", "--deny", "Write("+cfg+")")
	if agent && write {
		a = append(a, "--sandbox", "workspace", "--always-approve")
	} else {
		a = append(a, "--sandbox", "read-only", "--disable-web-search")
	}
	if resume != "" {
		a = append(a, "-r", resume)
	}
	return a
}

// codexStep: korak iz dogodka »codex exec --json« (ukaz ali vmesno sporočilo) za dnevnik; "" = brez koraka.
func codexStep(line []byte) string {
	var o struct {
		Type string `json:"type"`
		Item struct {
			Type, Command, Text string
		} `json:"item"`
	}
	if json.Unmarshal(line, &o) != nil {
		return ""
	}
	switch {
	case o.Type == "item.started" && o.Item.Type == "command_execution":
		cmd := o.Item.Command
		if i := strings.Index(cmd, "-lc '"); i >= 0 && strings.HasSuffix(cmd, "'") {
			cmd = cmd[i+5 : len(cmd)-1]
		}
		return "$ " + cmd[:min(len(cmd), 120)]
	case o.Type == "item.completed" && o.Item.Type == "agent_message":
		first, _, _ := strings.Cut(o.Item.Text, "\n")
		return first[:min(len(first), 120)]
	case o.Type == "item.started" && o.Item.Type != "" && o.Item.Type != "reasoning":
		return strings.ReplaceAll(o.Item.Type, "_", " ")
	}
	return ""
}

// codexThread: ID niti iz {"type":"thread.started","thread_id":…}.
func codexThread(line []byte) string {
	var o struct {
		Type     string `json:"type"`
		ThreadID string `json:"thread_id"`
	}
	if json.Unmarshal(line, &o) == nil && o.Type == "thread.started" {
		return o.ThreadID
	}
	return ""
}

// grokEvent: dogodek »streaming-json«: korak, besedilo, konec (ID seje) ali napaka.
type grokEvent struct{ step, text, session, err string }

func parseGrokEvent(line []byte) (grokEvent, bool) {
	var o map[string]any
	if json.Unmarshal(line, &o) != nil {
		return grokEvent{}, false
	}
	switch o["type"] {
	case "text":
		t, _ := o["data"].(string)
		return grokEvent{text: t}, true
	case "tool_call":
		in, _ := o["rawInput"].(map[string]any)
		arg := ""
		for _, k := range []string{"command", "target_file", "path", "target_directory", "query", "url"} {
			if v, ok := in[k].(string); ok {
				arg = v
				break
			}
		}
		title, _ := o["title"].(string)
		if title == "" {
			title = "tool"
		}
		return grokEvent{step: strings.TrimSpace(title + " " + arg[:min(len(arg), 120)])}, true
	case "end":
		id, _ := o["sessionId"].(string)
		return grokEvent{session: id}, true
	case "error":
		m, _ := o["message"].(string)
		if m == "" {
			m = "grok error"
		}
		return grokEvent{err: m}, true
	}
	return grokEvent{}, false
}

// --- zahteve ---

// agentFolder: sinhronizirana kopija mape odjemalca za agenta (le sync); st != 0 = odgovor že poslan.
func (s *server) agentFolder(w http.ResponseWriter, r *req, label string) (dir, note string, readOnly bool, st int) {
	session, clientCwd := r.h("x-claude-mini-session"), r.h("x-claude-mini-cwd")
	share := s.hub.waitShare(r.Context(), session, r.owner(), r.h("x-claude-mini-share") == "1")
	switch {
	case share != nil:
		if dir = s.hub.push(share); dir == "" {
			return "", "", false, writeError(w, r.rid, 502, "Syncing "+clientCwd+" to the router failed (see minigw output)")
		}
		note = "The user's folder " + clientCwd + " (on their computer) is synced here as " + dir + ", your working directory: a local copy"
		if share.readOnly {
			note += ". Map paths under " + clientCwd + " to it. The folder is shared read-only: do not try to create, change or delete files in it."
		} else {
			note += " whose changed source files are copied back to the user when you finish. Map paths under " + clientCwd + " to it."
		}
		return dir, note, share.readOnly, 0
	case s.hub.isRefused(session):
		return "", "", false, writeError(w, r.rid, 400, refusedDirect)
	case r.h("x-claude-mini-share") == "1":
		return "", "", false, writeError(w, r.rid, 400, "The folder "+clientCwd+" did not reach this router: see the minigw message when it started")
	}
	return "", "", false, writeError(w, r.rid, 400, "The folder "+clientCwd+" is not shared with the router, so "+label+" agents cannot work in it: "+
		"in that folder run minigw share add (or MINIGW_SHARE=1 for one run), then start minigw again")
}

// runAgent: Codex ali Grok za eno zahtevo; odgovor kot en besedilni blok (pri pretoku ping med čakanjem).
func (s *server) runAgent(w http.ResponseWriter, r *req, body map[string]any, k agentKind, model string) int {
	c := s.conf()
	ac := k.conf(c)
	if !agentsAllowed(c) {
		return writeError(w, r.rid, 403, k.bin+" on Windows needs \"Allow commands without a sandbox\" (Claude CLI settings): it can read and change "+
			"everything this user can, including the router's settings")
	}
	if model != "" && !validAgentModel.MatchString(model) {
		return writeError(w, r.rid, 400, "Invalid model name")
	}
	exe, err := resolveAgent(k, ac.Executable)
	if err != nil || exe == "" {
		return writeError(w, r.rid, 503, k.bin+" not found on the router (install it or set its path in the settings)")
	}
	all := turns(body)
	if len(all) == 0 || all[len(all)-1].role != "user" {
		return writeError(w, r.rid, 400, "Last message must be from the user")
	}
	last, history := all[len(all)-1], all[:len(all)-1]
	tools, _ := body["tools"].([]any)
	agent := len(tools) > 0
	// Tudi pogovor brez orodij le v sinhronizirani mapi (M1): CLI ima orodja vedno; brez orodij teče samo za branje.
	if r.h("x-claude-mini-session") == "" {
		return writeError(w, r.rid, 400, k.prefix+" needs a synced folder: run minigw with MINIGW_WORK=sync in a project folder")
	}
	dir, note, readOnly, st := s.agentFolder(w, r, k.bin)
	if st != 0 {
		return st
	}
	prompt := note + "\n\n" + cliPrompt(history, last)
	var extra []string
	for _, f := range c.AllowedFolders {
		if f != dir {
			extra = append(extra, f)
		}
	}
	write := !ac.ReadOnly && !readOnly
	scope := k.name + "|" + r.owner() + "|" + r.h("x-claude-mini-cwd")
	resume := ""
	if agent {
		if resume = s.sessions.get(transcriptKey(history, scope)); resume != "" {
			prompt = last.text
			if note != "" {
				prompt = note + "\n\n" + prompt
			}
		}
	}
	select { // skupaj s claude največ 6 procesov hkrati
	case s.procs <- struct{}{}:
		defer func() { <-s.procs }()
	case <-r.Context().Done():
		return 499
	}
	ctx, cancel := context.WithTimeout(r.Context(), agentTimeout) // L5
	defer cancel()
	return s.replyWhenDone(w, r, body, func() (string, error) {
		var text, thread string
		var err error
		if k.name == "codex" {
			out := filepath.Join(c.scratch("tmp"), "codex-"+r.rid+".txt")
			defer os.Remove(out)
			args := codexArgs(dir, extra, agent, out, model, write, ac.Network, resume)
			err = s.runAgentProcess(ctx, r, exe, args, dir, prompt, func(line []byte) {
				if t := codexThread(line); t != "" && thread == "" {
					thread = t
				}
				if st := codexStep(line); st != "" {
					s.log.Printf("%s step: %q", r.rid, st)
				}
			})
			b, _ := os.ReadFile(out)
			text = strings.TrimSpace(string(b))
		} else {
			pf, perr := s.promptFile(prompt)
			if perr != nil {
				return "", perr
			}
			defer os.Remove(pf)
			args := grokArgs(dir, pf, s.cfgDir, agent, model, write, resume)
			var answer, failure string
			err = s.runAgentProcess(ctx, r, exe, args, dir, "", func(line []byte) {
				e, ok := parseGrokEvent(line)
				switch {
				case !ok:
				case e.step != "":
					s.log.Printf("%s step: %q", r.rid, e.step)
					answer = "" // odgovor je besedilo po zadnjem orodju
				case e.err != "":
					failure = e.err
				case e.session != "":
					thread = e.session
				default:
					answer += e.text
				}
			})
			if failure != "" {
				return "", errors.New(failure)
			}
			text = strings.TrimSpace(answer)
		}
		if err != nil || text == "" {
			if err == nil {
				err = errors.New(k.bin + " gave no answer")
			}
			return "", err
		}
		if agent {
			s.hub.syncBack(r.h("x-claude-mini-session")) // spremembe agenta k odjemalcu pred odgovorom
			if thread != "" {
				s.sessions.set(transcriptKey(append(slices.Clone(all), turn{"assistant", text}), scope), thread)
			}
		}
		return text, nil
	})
}

// agentTimeout: najdaljši tek Codex ali Grok za eno zahtevo.
var agentTimeout = 30 * time.Minute

// runAgentProcess: proces z navodili na stdin, vsaka vrstica stdout v onLine; prekinitev odjemalca ali rok ga ustavi.
// Stderr gre le v dnevnik (I1): odjemalec dobi kratko sporočilo.
func (s *server) runAgentProcess(ctx context.Context, r *req, exe string, args []string, dir, stdin string, onLine func([]byte)) error {
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Dir = dir
	for _, e := range os.Environ() { // notranji agent ne kliče nazaj tega routerja
		if !strings.HasPrefix(e, "ANTHROPIC_") && !strings.HasPrefix(e, "CLAUDE_CODE_") && !strings.HasPrefix(e, "CLAUDECODE=") {
			cmd.Env = append(cmd.Env, e)
		}
	}
	cmd.Stdin = strings.NewReader(stdin)
	stderr := &tailBuffer{}
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err == nil {
		err = cmd.Start()
	}
	if err != nil {
		return err
	}
	s.agentsRunning.Add(1)
	defer s.agentsRunning.Add(-1)
	br := bufio.NewReader(stdout)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			onLine(line)
		}
		if err != nil {
			break
		}
	}
	if err := cmd.Wait(); err != nil {
		if tail := strings.TrimSpace(stderr.String()); tail != "" {
			s.log.Printf("%s %s stderr: %s", r.rid, filepath.Base(exe), tail[max(0, len(tail)-2000):])
		}
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("%s stopped after %v", filepath.Base(exe), agentTimeout)
		}
		return fmt.Errorf("%s failed (%v); details are in the router log, request %s", filepath.Base(exe), err, r.rid)
	}
	return nil
}

// replyWhenDone: ponudnik brez pretoka (Codex, Grok): pri pretoku glave in message_start takoj, med čakanjem ping
// vsakih 10 s, nato cel odgovor (kot Router.replyWhenDone).
func (s *server) replyWhenDone(w http.ResponseWriter, r *req, body map[string]any, work func() (string, error)) int {
	model, _ := body["model"].(string)
	idb := make([]byte, 16)
	rand.Read(idb)
	t := &translator{model: model, id: fmt.Sprintf("msg_%x", idb)}
	stream := body["stream"] == true
	var mu sync.Mutex
	send := func(b []byte) {
		mu.Lock()
		defer mu.Unlock()
		w.Write(b)
		http.NewResponseController(w).Flush()
	}
	done := make(chan struct{})
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("request-id", r.rid)
		w.WriteHeader(200)
		send(t.start())
		go func() {
			tick := time.NewTicker(10 * time.Second)
			defer tick.Stop()
			for {
				select {
				case <-tick.C:
					send(sse("ping", map[string]string{"type": "ping"}))
				case <-done:
					return
				}
			}
		}()
	}
	text, err := work()
	close(done)
	if r.Context().Err() != nil {
		return 499
	}
	if err != nil {
		s.log.Printf("%s %s failed: %v", r.rid, model, err)
		if stream {
			send(sse("error", map[string]any{"type": "error", "error": map[string]string{"type": "api_error", "message": err.Error()}}))
			return 502
		}
		return writeError(w, r.rid, 502, err.Error())
	}
	t.usage = map[string]any{"output_tokens": float64(len(text) / 4)}
	delta := t.emit(text)
	if stream {
		send(append(delta, t.finish()...))
		return 200
	}
	return writeJSON(w, r.rid, 200, t.message())
}

// agentsAllowed: na Windows Codex in Grok nimata preverjenega sandboxa (berejo in pišejo vse, kar uporabnik):
// le z izrecnim allowUnsandboxedBash, v vsakem načinu (H1).
func agentsAllowed(c serveConfig) bool { return runtime.GOOS != "windows" || c.AllowUnsandboxedBash }

// agentModelList: claude-codex in claude-codex-<model> (vklopljeni), enako za Grok; le vklopljen agent z najdenim CLI.
func (s *server) agentModelList(k agentKind) []map[string]any {
	c := s.conf()
	ac := k.conf(c)
	exe, _ := resolveAgent(k, ac.Executable)
	if !ac.Enabled || exe == "" || !agentsAllowed(c) {
		return nil
	}
	label := map[string]string{"codex": "Codex CLI", "grok": "Grok CLI"}[k.name]
	run := map[string]string{"codex": "codex exec", "grok": "grok -p"}[k.name]
	out := []map[string]any{{"type": "model", "id": k.prefix, "display_name": strings.SplitN(label, " ", 2)[0] + " · " + label,
		"description": run + " · default model · tools in your synced folder", "created_at": "2026-01-01T00:00:00Z"}}
	for _, m := range agentCatalog(k, exe) {
		if !ac.modelEnabled(m.Slug) {
			continue
		}
		win := ""
		if m.Window > 0 {
			win = fmt.Sprintf(" · %dK", m.Window/1000)
		}
		out = append(out, map[string]any{"type": "model", "id": k.prefix + "-" + m.Slug, "display_name": m.Name + " · " + label,
			"description": run + " · " + m.Slug + win + " · tools in your synced folder", "created_at": "2026-01-01T00:00:00Z"})
	}
	return out
}
