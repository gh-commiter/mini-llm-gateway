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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"unicode/utf8"
	"sync"
	"time"
)

// agentCLI: nastavitve Codex ali Grok (JSON v API kot na Macu: enabled, path, write, network, models).
type agentCLI struct {
	Enabled    bool     `json:"enabled,omitempty"`
	Executable string   `json:"executable,omitempty"` // prazno = iz PATH
	Models     []string `json:"models"`               // null = vsi modeli računa (tudi prihodnji); seznam = le ti
	ReadOnly   bool     `json:"readOnly,omitempty"`   // brez pisanja v mapo seje
	Network    bool     `json:"network,omitempty"`    // Codex: omrežje za ukaze v sandboxu; Copilot: spletna orodja (url)
	Write      bool     `json:"write,omitempty"`      // Copilot: pisanje (privzeto le branje)
	Commands   bool     `json:"commands,omitempty"`   // Copilot: ukazi lupine (izrecno dovoljenje)
	AskOthers  bool     `json:"askOthers,omitempty"`  // agent sme vprašati druge modele tega prehoda (serve_agentcalls.go)
}

func (a agentCLI) modelEnabled(m string) bool { return a.Models == nil || slices.Contains(a.Models, m) }

// canWrite: Codex in Grok pišeta, razen z ReadOnly; Copilot le z Write (privzeto le branje, kot na Macu).
func (a agentCLI) canWrite(k agentKind) bool {
	if k.name == "copilot" {
		return a.Write
	}
	return !a.ReadOnly
}

// allowsModel: Codex, Grok: model računa (nil = vsi); Copilot: le našteta imena (Copilot nima seznama modelov).
func (a agentCLI) allowsModel(k agentKind, m string) bool {
	if k.name == "copilot" {
		return slices.Contains(a.Models, m)
	}
	return a.modelEnabled(m)
}

type agentKind struct {
	name, prefix, bin string
	conf              func(serveConfig) agentCLI
}

var (
	codexKind   = agentKind{"codex", "claude-codex", "codex", func(c serveConfig) agentCLI { return c.Codex }}
	grokKind    = agentKind{"grok", "claude-grok", "grok", func(c serveConfig) agentCLI { return c.Grok }}
	copilotKind = agentKind{"copilot", "claude-copilot", "copilot", func(c serveConfig) agentCLI { return c.Copilot }}
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
			p = lookPath(k.bin + ".exe")
		}
		if p == "" {
			if p = lookPath(k.bin); p == "" {
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
	cmd := commandContext(ctx, exe, args...)
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

// copilotArgs: kot Copilot.args na Macu. Nikoli --allow-all-tools/-paths/-urls, --allow-all ali --yolo: v načinu -p Copilot
// zavrne vsako orodje brez --allow-tool; zavrnitve so tu še izrecno. Privzeto le branje; write le z dovoljenjem; lupina le
// z dovoljenjem (commands), gh in security tudi tedaj ne; url le z omrežjem. Brez sistemske začasne mape, vgrajenih MCP
// (GitHub s prijavo uporabnika), vprašanj in samodejnih posodobitev; seja po --session-id.
// Navodilo gre na stdin (brez -p; preverjeno s copilot 1.0.107), ne v ukazno vrstico: to vidijo ps in /proc/<pid>/cmdline.
// Brez --add-dir (M3): ta naloži .github/skills in .github/agents mape kot zaupanja vredne; agent vidi le mapo seje.
func copilotArgs(agent bool, model string, write, commands, network bool, session string) []string {
	a := []string{"--output-format", "json", "--no-ask-user", "--disable-builtin-mcps", "--disallow-temp-dir",
		"--no-auto-update", "--no-color", "--disable-client-telemetry",
		"--secret-env-vars=" + strings.Join(copilotTokenVars, ","), "--session-id", session}
	if model != "" {
		a = append(a, "--model", model)
	}
	allow, deny := []string{}, []string{"memory"}
	pick := func(on bool, tool string) {
		if on {
			allow = append(allow, tool)
		} else {
			deny = append(deny, tool)
		}
	}
	pick(agent, "read")
	pick(agent && write, "write")
	commands = commands && write // ukazi pišejo mimo pravila write (tudi ne pri --ro): le s pisanjem
	pick(agent && commands, "shell")
	if agent && commands {
		deny = append(deny, "shell(gh:*)", "shell(security:*)")
	}
	pick(agent && network, "url")
	for _, t := range allow {
		a = append(a, "--allow-tool", t)
	}
	for _, t := range deny {
		a = append(a, "--deny-tool", t)
	}
	return a
}

// copilotTokenVars: žetoni, ki jih copilot sprejme iz okolja; ukazom lupine jih skrije (--secret-env-vars).
var copilotTokenVars = []string{"COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"}

// copilotGH: GitHub CLI (var: testi podtaknejo lažnega).
var copilotGH = func() string { return lookPath("gh") }

// copilotKey: ime žetona za ukaze (fine-grained PAT le z »Copilot Requests«) v shrambi ključev; »:« ime ponudnika ne more imeti.
const copilotKey = "agent:copilot"

// validCopilotToken: le fine-grained PAT (github_pat_…), natisljiv ASCII brez presledkov.
func validCopilotToken(t string) bool {
	return strings.HasPrefix(t, "github_pat_") && len(t) <= 255 && !strings.ContainsFunc(t, func(r rune) bool { return r < 0x21 || r > 0x7e })
}

// copilotToken: žeton iz okolja routerja ali GitHub CLI (»gh auth token«, zunaj bwrap; največ 10 s) za copilot pod bwrap,
// ki skrije ~/.config/gh, shrambo ključev in D-Bus (na Macu pod sandbox-exec copilot 1.0.107: »No authentication
// information found«). Okolje agenta žetonov nima (agentEnv). "" brez žetona, gh ali prijave.
func copilotToken(ctx context.Context) string {
	for _, v := range copilotTokenVars {
		if t := os.Getenv(v); t != "" {
			return t
		}
	}
	gh := copilotGH()
	if gh == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := commandContext(ctx, gh, "auth", "token", "--hostname", "github.com")
	cmd.Env = append(os.Environ(), "GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1")
	out, err := cmd.Output()
	t := strings.TrimSpace(string(out))
	bad := strings.IndexFunc(t, func(r rune) bool {
		return r != '_' && (r < '0' || r > '9') && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z')
	})
	if err != nil || t == "" || len(t) > 255 || bad >= 0 {
		return ""
	}
	return t
}

// copilotSessionFiles: datoteke stanja seje, ki jih copilot potrebuje za nadaljevanje (preverjeno s copilot 1.0.107);
// checkpoints, rewind-file-snapshots, files … ne gredo med zahtevami (R6).
var copilotSessionFiles = []string{"events.jsonl", "workspace.yaml"}

// copySession: le copilotSessionFiles, le navadne datoteke iz prave mape (ne povezave): brez sledenja povezavi
// (openMarker) in ista datoteka kot pri Lstat (R4). Napaka, če ni nobene.
func copySession(src, dst string) error {
	if di, err := os.Lstat(src); err != nil || !di.IsDir() {
		return errors.New("no session state")
	}
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return err
	}
	n := 0
	for _, name := range copilotSessionFiles {
		p := filepath.Join(src, name)
		li, err := os.Lstat(p)
		if err != nil || !li.Mode().IsRegular() {
			continue
		}
		in, err := openMarker(p)
		if err != nil {
			continue
		}
		fi, err := in.Stat()
		if err != nil || !fi.Mode().IsRegular() || !os.SameFile(li, fi) {
			in.Close()
			continue
		}
		out, err := os.OpenFile(filepath.Join(dst, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			_, err = io.Copy(out, in)
			err = errors.Join(err, out.Close())
		}
		in.Close()
		if err != nil {
			return err
		}
		n++
	}
	if n == 0 {
		return errors.New("no session state")
	}
	return nil
}

// copilotPrepared: izvršljive datoteke (pot|velikost|čas), za katere je paket copilot razširjen.
var copilotPrepared sync.Map

// copilotPrepare: paket (~/.cache/copilot/pkg/<platforma>/<različica>) razširi copilot sam ob prvem zagonu nove različice;
// pod bwrap (predpomnilnik le za branje) ne more. Ob novi izvršljivi datoteki ali brez mape pkg enkrat »copilot --version«
// brez bwrap: uporabnikov program, brez navodila agenta in klica modela, svež COPILOT_HOME, brez žetona (R7).
// copilotCacheDir: paket Copilot CLI (koda, ki jo copilot --version razširi in naloži).
func copilotCacheDir() string {
	cache, _ := os.UserCacheDir()
	return filepath.Join(cache, "copilot")
}

// copilotCacheOverlaps (#26): dovoljene mape, ki vsebujejo paket Copilot ali so v njem (kot scratchOverlaps).
func copilotCacheOverlaps(allowed []string) []string {
	return scratchOverlaps(allowed, copilotCacheDir())
}

// copilotPrepare: pod bwrap brez omrežja (#26): podtaknjena koda v paketu ne vidi nastavitev, ključev, Scratch in
// omrežja; pisanje v paket, začasni COPILOT_HOME in začasno mapo zahteve (cwd).
func copilotPrepare(ctx context.Context, exe, home, cwd, cfgDir, scratch string) {
	if runtime.GOOS != "linux" {
		return // Windows: brez peskovnika
	}
	real, err := filepath.EvalSymlinks(exe)
	if err != nil {
		real = exe
	}
	st, err := os.Stat(real)
	if err != nil {
		return
	}
	key := fmt.Sprintf("%s|%d|%d", real, st.Size(), st.ModTime().UnixNano())
	cache, _ := os.UserCacheDir()
	if _, done := copilotPrepared.Load(key); done {
		if _, err := os.Stat(filepath.Join(cache, "copilot", "pkg")); err == nil {
			return
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute) // razširitev ~200 MB
	defer cancel()
	os.MkdirAll(copilotCacheDir(), 0o700)
	os.MkdirAll(home, 0o700)
	name, args := agentWrap(exe, []string{"--version"}, agentMounts{cfgDir: cfgDir, rw: []string{copilotCacheDir(), home, cwd}, hide: scratch, noNet: true})
	cmd := commandContext(ctx, name, args...)
	cmd.Dir = cwd
	cmd.Env = append(agentEnv(os.Environ()), "COPILOT_HOME="+home, "COPILOT_AUTO_UPDATE=false")
	if cmd.Run() == nil {
		copilotPrepared.Store(key, true)
	}
}

// copilotEvent: dogodek »--output-format json« (JSONL): odgovor (assistant.message z vsebino) ali korak (tool.*start).
func copilotEvent(line []byte) (text, step string) {
	var o struct {
		Type string `json:"type"`
		Data struct {
			Content  string `json:"content"`
			ToolName string `json:"toolName"`
			Name     string `json:"name"`
		} `json:"data"`
	}
	if json.Unmarshal(line, &o) != nil {
		return "", ""
	}
	switch {
	case o.Type == "assistant.message":
		return o.Data.Content, ""
	case strings.HasPrefix(o.Type, "tool.") && strings.HasSuffix(o.Type, "start"):
		n := o.Data.ToolName
		if n == "" {
			n = o.Data.Name
		}
		if n == "" {
			n = "tool"
		}
		return "", n
	}
	return "", ""
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
func (s *server) agentFolder(w http.ResponseWriter, r *req, label, agent string) (dir, note string, readOnly bool, st int) {
	if p := runOf(r); p != nil && p.shared { // vprašal ga je drug agent: njegova mapa, brez novega prenosa
		return p.dir, nestedNote(p), p.readOnly, 0
	} else if p != nil { // L1: privzeta mapa prehoda ni deljena; tudi neposredno bi bila zavrnjena
		return "", "", false, writeError(w, r.rid, 400, "The folder "+r.h("x-claude-mini-cwd")+" is not shared with the gateway, so "+label+
			" agents cannot work in it (the asking agent works in the gateway's own folder)")
	}
	session, clientCwd := r.h("x-claude-mini-session"), r.h("x-claude-mini-cwd")
	share := s.hub.waitShare(r.Context(), session, r.owner(), r.h("x-claude-mini-share") == "1")
	switch {
	case share != nil && share.tree != nil:
		if dir = s.hub.push(share, agent); dir == "" {
			return "", "", false, writeError(w, r.rid, 502, "Cloning "+clientCwd+" on the gateway failed (worktree mode; see minigw output)")
		}
		_, branch, _, _, _ := s.hub.worktreeOf(r.h("x-claude-mini-session"), agent)
		return dir, worktreeNote(clientCwd, dir, branch), false, 0
	case share != nil:
		if dir = s.hub.push(share, agent); dir == "" {
			return "", "", false, writeError(w, r.rid, 502, "Syncing "+clientCwd+" to the gateway failed (see minigw output)")
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
		return "", "", false, writeError(w, r.rid, 400, "The folder "+clientCwd+" did not reach this gateway: see the minigw message when it started")
	}
	return "", "", false, writeError(w, r.rid, 400, "The folder "+clientCwd+" is not shared with the gateway, so "+label+" agents cannot work in it: "+
		"in that folder run minigw share add (or MINIGW_SHARE=1 for one run), then start minigw again")
}

// runAgent: Codex ali Grok za eno zahtevo; odgovor kot en besedilni blok (pri pretoku ping med čakanjem).
func (s *server) runAgent(w http.ResponseWriter, r *req, body map[string]any, k agentKind, model string) int {
	c := s.conf()
	ac := k.conf(c)
	if !s.agentsAllowed(c) {
		return writeError(w, r.rid, 403, k.bin+" needs \"Allow commands without a sandbox\" (Claude CLI settings) on this gateway (Windows, "+
			"or Linux without bwrap): it can read and change everything this user can, including the gateway's settings")
	}
	if model != "" && !validAgentModel.MatchString(model) {
		return writeError(w, r.rid, 400, "Invalid model name")
	}
	exe, err := resolveAgent(k, ac.Executable)
	if err != nil || exe == "" {
		return writeError(w, r.rid, 503, k.bin+" not found on the gateway (install it or set its path in the settings)")
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
	if why := chainRefusal(r, k.name); why != "" { // vprašani agent: globina in brez ponovitve vrste
		return writeError(w, r.rid, 403, why)
	}
	if s.hub.isWorktree(r.h("x-claude-mini-session")) && (k.name == "grok" || !ac.canWrite(k)) { // besedila kot na Macu
		msg := "Grok is not supported in worktree mode yet; use sync (MINIGW_WORK=sync)"
		if k.name != "grok" {
			pane := map[string]string{"codex": "Codex", "copilot": "Copilot CLI"}[k.name]
			msg = strings.ToUpper(k.name[:1]) + k.name[1:] + " may not write (settings page → " + pane + "), so worktree mode is not possible; use sync"
		}
		return writeError(w, r.rid, 400, msg)
	}
	dir, note, readOnly, st := s.agentFolder(w, r, k.bin, k.name)
	if st != 0 {
		return st
	}
	if c.askOthers(k.name) { // vrata izvajanja (startRun spodaj)
		// Absolutna pot: PATH izvajanja ne doseže ukazov v sandboxu Codex na Windows (»elevated« = drug uporabnik);
		// »minigw« iz PATH (askOnPath) je le za pravila Claude Code (askRules).
		note += "\n\n" + askNote(minigwExe(), false)
	}
	prompt := note + "\n\n" + cliPrompt(history, last)
	var extra []string
	for _, f := range c.AllowedFolders {
		if f != dir && len(scratchOverlaps([]string{f}, c.Scratch)) == 0 && len(copilotCacheOverlaps([]string{f})) == 0 { // L1, #26
			extra = append(extra, f)
		}
	}
	write := ac.canWrite(k) && !readOnly
	// H1: ukazi Copilot le z namenskim žetonom (ukaz lahko prebere okolje copilota; žeton gh ima vse pravice uporabnika).
	pat := ""
	if k.name == "copilot" {
		pat, _ = keyGet(s.cfgDir, copilotKey)
		if agent && write && ac.Commands && pat == "" {
			return writeError(w, r.rid, 503, "Copilot commands need their own GitHub token (fine-grained, only the \"Copilot Requests\" "+
				"permission): minigw gw copilot token")
		}
	}
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
	if st := s.procSlot(w, r); st != 0 {
		return st
	}
	defer func() { <-s.procs }()
	ctx, cancel := context.WithTimeout(r.Context(), agentTimeout) // L5
	defer cancel()
	return s.replyWhenDone(w, r, body, func() (string, error) {
		var text, thread string
		var err error
		tmp, err := os.MkdirTemp(c.scratch("tmp"), "req-*") // le ta zahteva (pod bwrap vidna namesto cele mape tmp)
		if err != nil {
			return "", err
		}
		defer os.RemoveAll(tmp)
		// N3: vsaka naprava ima svoje stanje CLI (seje, zgodovina); prijava je povezava na pravo datoteko.
		state, files, err := agentState(c.Scratch, k, r.owner())
		if err != nil {
			return "", err
		}
		defer restoreAuth(state, k)
		m := agentMounts{cfgDir: s.cfgDir, state: state, envVar: strings.ToUpper(k.bin) + "_HOME", files: files, rw: append([]string{dir, tmp}, extra...),
			hide: c.Scratch}
		wt, _, maven, objects, inTree := s.hub.worktreeOf(r.h("x-claude-mini-session"), k.name)
		if p := runOf(r); p != nil { // klon klicatelja
			wt, maven, objects, inTree = p.wt, p.maven, p.objects, p.wt != ""
		}
		run, runEnv := s.startRun(r, k.name, dir, readOnly, true, wt, maven, objects, tmp) // »May ask other models«; mapa je deljena (agentFolder); žeton v tmp zahteve
		defer run.stop()
		withTree := func(m agentMounts) agentMounts { // worktree: Maven agentov, objekti zrcala (branje), .git klona le za branje
			m.env, m.started = append(m.env, runEnv...), run.started
			if runEnv != nil {
				m.ro = append(m.ro, minigwExe())
			}
			if inTree {
				m.rw, m.ro = append(m.rw, maven), append(m.ro, objects)
				m.pins = []string{filepath.Join(wt, ".git")} // L1: .git klona je točka priklopa (mv/rm: EBUSY)
				for _, d := range []string{"config", "hooks", "info", "commondir"} {
					m.protect = append(m.protect, filepath.Join(wt, ".git", d))
				}
				m.env = append(m.env, worktreeHarnessEnv(wt, maven)...)
			}
			return m
		}
		if k.name == "copilot" {
			if resume == "" {
				resume = newUUID() // nova seja: svoj ID, da jo naslednja zahteva nadaljuje (--session-id)
			}
			cache, _ := os.UserCacheDir()
			m.ro = []string{filepath.Join(cache, "copilot")} // razširjen paket (Linux, pod bwrap)
			m.rw = []string{dir, tmp}                        // brez drugih dovoljenih map (M3)
			// R9: mapa seje ne sme vsebovati paketa copilot (orodje write bi ga spremenilo za naslednji zagon brez bwrap).
			if pkg := filepath.Join(cache, "copilot"); pkg == dir || strings.HasPrefix(pkg, dir+string(filepath.Separator)) {
				return "", errors.New("copilot: the session's folder contains Copilot's package cache (" + pkg + ")")
			}
			token := pat
			if token == "" {
				token = copilotToken(ctx)
			}
			if token == "" && runtime.GOOS == "linux" {
				return "", errors.New("copilot: no GitHub sign-in on the gateway; sign in with the GitHub CLI (gh auth login) or set a token (minigw gw copilot token)")
			}
			if token != "" {
				m.env = []string{"COPILOT_GITHUB_TOKEN=" + token}
			}
			// C2: svež COPILOT_HOME vsake zahteve (hooki, nastavitve, zaupane mape prejšnjega zagona ne obstajajo); med
			// zahtevami ostane le stanje seje (brez povezav) v stanju naprave, ki ga bwrap ne vidi. Windows: pravi ~/.copilot (I1).
			store := ""
			if state != "" {
				store = filepath.Join(state, "sessions")
				m.state, m.files = filepath.Join(tmp, "home"), nil
				if copySession(filepath.Join(store, resume), filepath.Join(m.state, "session-state", resume)) != nil {
					os.MkdirAll(filepath.Join(m.state, "session-state"), 0o700)
					prompt = note + "\n\n" + cliPrompt(history, last) // stanja seje ni: ves pogovor
				}
			}
			copilotPrepare(ctx, exe, filepath.Join(tmp, "prep"), tmp, s.cfgDir, c.Scratch)
			args := copilotArgs(agent, model, write, ac.Commands, ac.Network, resume)
			var answer string
			err = s.runAgentProcess(ctx, r, exe, args, withTree(m), dir, prompt, func(line []byte) {
				t, st := copilotEvent(line)
				if t != "" {
					answer = t // odgovor je zadnje sporočilo agenta
				}
				if st != "" {
					s.log.Printf("%s step: %q", r.rid, st)
				}
			})
			text, thread = strings.TrimSpace(answer), resume
			if err == nil && agent && store != "" {
				next := filepath.Join(store, resume+".new-"+filepath.Base(tmp))
				if copySession(filepath.Join(m.state, "session-state", resume), next) == nil {
					os.RemoveAll(filepath.Join(store, resume))
					if os.Rename(next, filepath.Join(store, resume)) != nil {
						thread = "" // stanje ni shranjeno: naslednja zahteva začne znova
					}
				} else {
					thread = ""
				}
				os.RemoveAll(next)
			}
		} else if k.name == "codex" {
			out := filepath.Join(tmp, "codex.txt")
			args := codexArgs(dir, extra, agent, out, model, write, ac.Network, resume)
			var failure string
			err = s.runAgentProcess(ctx, r, exe, args, withTree(m), dir, prompt, func(line []byte) {
				if f := codexFailure(line); f != "" {
					failure = f
				}
				if t := codexThread(line); t != "" && thread == "" {
					thread = t
				}
				if st := codexStep(line); st != "" {
					s.log.Printf("%s step: %q", r.rid, st)
				}
			})
			b, _ := os.ReadFile(out)
			text = strings.TrimSpace(string(b))
			if failure != "" && (err != nil || text == "") {
				return "", errors.New(failure)
			}
		} else {
			pf := filepath.Join(tmp, "prompt.txt")
			if err := os.WriteFile(pf, []byte(prompt), 0o600); err != nil {
				return "", err
			}
			args := grokArgs(dir, pf, s.cfgDir, agent, model, write, resume)
			var answer, failure string
			err = s.runAgentProcess(ctx, r, exe, args, withTree(m), dir, "", func(line []byte) {
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
			run.stop()           // I2: vprašanja drugim modelom se končajo pred prevzemom sprememb
			if runOf(r) == nil { // vprašani agent: spremembe prenese klicatelj na koncu
				s.hub.syncBack(r.h("x-claude-mini-session"), k.name) // spremembe agenta k odjemalcu pred odgovorom
			}
			if thread != "" {
				s.sessions.set(transcriptKey(append(slices.Clone(all), turn{"assistant", text}), scope), thread)
			}
		}
		return text, nil
	})
}

// agentEnv: okolje za Codex in Grok: brez spremenljivk, ki bi notranji CLI usmerile nazaj v ta router, in brez
// vtičnic agentov ključev in seje D-Bus (N2: SSH, GPG, GNOME Keyring, Secret Service), brez žetonov GitHub in vseh
// COPILOT_* (COPILOT_ALLOW_ALL, COPILOT_PROVIDER_API_KEY_COMMAND …); CODEX_HOME, GROK_HOME in COPILOT_HOME nastavi
// runAgentProcess, žeton Copilot runAgent.
func agentEnv(env []string) []string {
	var out []string
	for _, e := range env {
		name, _, _ := strings.Cut(e, "=")
		name = strings.ToUpper(name) // Windows: imena spremenljivk brez razlike med velikimi in malimi črkami
		if strings.HasPrefix(name, "ANTHROPIC_") || strings.HasPrefix(name, "CLAUDE_CODE_") || slices.Contains([]string{"CLAUDECODE",
			"DBUS_SESSION_BUS_ADDRESS", "SSH_AUTH_SOCK", "GPG_AGENT_INFO", "GNOME_KEYRING_CONTROL", "CODEX_HOME", "GROK_HOME", "MINIGW_AGENT_URL", "MINIGW_AGENT_AUTH_FILE",
			"GH_TOKEN", "GITHUB_TOKEN"}, name) || strings.HasPrefix(name, "COPILOT_") {
			continue
		}
		out = append(out, e)
	}
	return out
}

// agentAuthFiles: datoteke v pravi mapi CLI, na katere kaže stanje naprave (prijava, nastavitve, zaklep osvežitve).
func agentAuthFiles(k agentKind) (real string, names []string) {
	home, _ := os.UserHomeDir()
	if k.name == "codex" {
		if real = os.Getenv("CODEX_HOME"); real == "" {
			real = filepath.Join(home, ".codex")
		}
		return real, []string{"auth.json", "config.toml"}
	}
	if k.name == "copilot" { // M1: ne prave config.json; prijava je žeton iz okolja (copilotToken ali namenski žeton)
		return filepath.Join(home, ".copilot"), nil
	}
	if real = os.Getenv("GROK_HOME"); real == "" {
		real = filepath.Join(home, ".grok")
	}
	return real, []string{"auth.json", "auth.json.lock", "config.toml"}
}

// agentState: <scratch>/state/<naprava>/<cli> s povezavami na prave datoteke prijave (Codex piše auth.json na
// mestu, skozi povezavo: preverjeno s codex 0.167). Vrne mapo in prave datoteke za bwrap.
func agentState(scratch string, k agentKind, owner string) (string, []string, error) {
	if runtime.GOOS == "windows" { // povezave tam zahtevajo pravice; agenti tam tako ali tako tečejo le z izrecnim dovoljenjem
		return "", nil, nil
	}
	sum := sha256.Sum256([]byte(owner))
	dir := filepath.Join(scratch, "state", hex.EncodeToString(sum[:8]), k.name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", nil, err
	}
	real, names := agentAuthFiles(k)
	var files []string
	for _, n := range names {
		src, dst := filepath.Join(real, n), filepath.Join(dir, n)
		if _, err := os.Stat(src); err != nil {
			continue
		}
		if l, err := os.Readlink(dst); err != nil || l != src {
			if fi, err := os.Lstat(dst); err == nil && fi.Mode().IsRegular() {
				continue // CLI jo je zamenjal: restoreAuth jo prenese nazaj
			}
			os.Remove(dst)
			if err := os.Symlink(src, dst); err != nil {
				return "", nil, err
			}
		}
		files = append(files, src)
	}
	return dir, files, nil
}

// restoreAuth: če je CLI auth.json zamenjal (zapis z rename namesto skozi povezavo), je ta novejša: v pravo mapo in
// spet povezava, da prava prijava ne zastari (osvežitveni žetoni se lahko zamenjajo).
func restoreAuth(dir string, k agentKind) {
	if dir == "" {
		return
	}
	real, _ := agentAuthFiles(k)
	dst, src := filepath.Join(dir, "auth.json"), filepath.Join(real, "auth.json")
	if fi, err := os.Lstat(dst); err != nil || !fi.Mode().IsRegular() {
		return
	}
	if b, err := os.ReadFile(dst); err == nil && writeFileAtomic(src, b) == nil {
		os.Chmod(src, 0o600)
		os.Remove(dst)
		os.Symlink(src, dst)
	}
}

// agentTimeout: najdaljši tek Codex ali Grok za eno zahtevo.
var agentTimeout = 30 * time.Minute

// runAgentProcess: proces z navodili na stdin, vsaka vrstica stdout v onLine; prekinitev odjemalca ali rok ga ustavi.
// Stderr gre le v dnevnik (I1): odjemalec dobi kratko sporočilo.
// maxAgentLine: največja vrstica izhoda agenta (JSONL).
const maxAgentLine = 1 << 20

// procSlot: mesto med največ 6 procesi agentov (s.procs; sprosti ga klicatelj). Vprašani agent (M1) ne čaka: mesto
// drži njegov klicatelj, čakanje bi lahko zaklenilo vse, zato 429, ko jih ni.
func (s *server) procSlot(w http.ResponseWriter, r *req) int {
	if runOf(r) != nil {
		select {
		case s.procs <- struct{}{}:
			return 0
		default:
			return writeError(w, r.rid, 429, "The gateway already runs its maximum of agents; ask again later")
		}
	}
	select {
	case s.procs <- struct{}{}:
		return 0
	case <-r.Context().Done():
		return 499
	}
}

func (s *server) runAgentProcess(ctx context.Context, r *req, exe string, args []string, m agentMounts, dir, stdin string, onLine func([]byte)) error {
	name, args := agentWrap(exe, args, m) // Linux: domača mapa in mapa nastavitev skriti (bwrap), če je na voljo
	cmd := commandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(agentEnv(os.Environ()), m.env...)
	if m.state != "" {
		cmd.Env = append(cmd.Env, m.envVar+"="+m.state)
	}
	cmd.Stdin = strings.NewReader(stdin)
	stderr := &tailBuffer{}
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err == nil {
		err = startAgent(cmd) // Windows: Job agentov; Linux: pomočnik subreaper (peer check)
	}
	if err != nil {
		return err
	}
	if m.started != nil {
		m.started(cmd.Process.Pid)
	}
	s.agentsRunning.Add(1)
	defer s.agentsRunning.Add(-1)
	br := bufio.NewReader(stdout)
	var line []byte
	skip := false // L4: vrstica nad maxAgentLine se zavrže (pomnilnik)
	for {
		chunk, err := br.ReadSlice('\n')
		if !skip {
			if line = append(line, chunk...); len(line) > maxAgentLine {
				line, skip = nil, true
			}
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if len(line) > 0 {
			onLine(line)
		}
		line, skip = nil, false
		if err != nil {
			break
		}
	}
	if err := cmd.Wait(); err != nil {
		tail := strings.TrimSpace(stderr.String())
		if tail != "" {
			s.log.Printf("%s %s stderr: %s", r.rid, filepath.Base(exe), tail[max(0, len(tail)-2000):])
		}
		if agentLimit(tail) { // stderr sam ostane v dnevniku, odjemalec dobi le stalno besedilo
			return fmt.Errorf("%s: usage or rate limit reached; details are in the gateway log, request %s", filepath.Base(exe), r.rid)
		}
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("%s stopped after %v", filepath.Base(exe), agentTimeout)
		}
		return fmt.Errorf("%s failed (%v); details are in the gateway log, request %s", filepath.Base(exe), err, r.rid)
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
		w.Header().Set("x-should-retry", "false")
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
		status, msg := classifyAgentError(err.Error())
		if stream {
			send(sse("error", map[string]any{"type": "error", "error": map[string]string{"type": errType(status), "message": msg}}))
			return status
		}
		return writeError(w, r.rid, status, msg)
	}
	t.usage = map[string]any{"output_tokens": float64(len(text) / 4)}
	delta := t.emit(text)
	if stream {
		send(append(delta, t.finish()...))
		return 200
	}
	return writeJSON(w, r.rid, 200, t.message())
}

// agentsAllowed: brez sandboxa, ki skrije skrivnosti routerja (Windows; Linux brez delujočega bwrap), Codex in Grok
// bereta in pišeta vse, kar uporabnik: le z izrecnim allowUnsandboxedBash, v vsakem načinu (H1, R3).
func (s *server) agentsAllowed(c serveConfig) bool {
	return agentSandboxed(s.cfgDir) || c.AllowUnsandboxedBash
}

// agentModelList: claude-codex in claude-codex-<model> (vklopljeni), enako za Grok; le vklopljen agent z najdenim CLI.
func (s *server) agentModelList(k agentKind) []map[string]any {
	c := s.conf()
	ac := k.conf(c)
	exe, _ := resolveAgent(k, ac.Executable)
	if !ac.Enabled || exe == "" || !s.agentsAllowed(c) {
		return nil
	}
	label := map[string]string{"codex": "Codex CLI", "grok": "Grok CLI", "copilot": "Copilot CLI"}[k.name]
	run := map[string]string{"codex": "codex exec", "grok": "grok -p", "copilot": "copilot -p"}[k.name]
	out := []map[string]any{{"type": "model", "id": k.prefix, "display_name": strings.SplitN(label, " ", 2)[0] + " · " + label,
		"description": run + " · default model · tools in your synced folder", "created_at": "2026-01-01T00:00:00Z"}}
	if k.name == "copilot" { // ni seznama modelov: le našteti
		for _, m := range ac.Models {
			out = append(out, map[string]any{"type": "model", "id": k.prefix + "-" + m, "display_name": m + " · " + label,
				"description": run + " · " + m + " · tools in your synced folder", "created_at": "2026-01-01T00:00:00Z"})
		}
		return out
	}
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

// codexFailure: sporočilo iz {"type":"error","message":…} ali {"type":"turn.failed","error":{"message":…}}.
func codexFailure(line []byte) string {
	var e struct {
		Type    string `json:"type"`
		Message string `json:"message"`
		Error   struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(line, &e) != nil {
		return ""
	}
	switch e.Type {
	case "error":
		return e.Message
	case "turn.failed":
		return e.Error.Message
	}
	return ""
}

var agentLimits = []string{"usage limit", "rate limit", "quota", "too many requests", "payment required", "spending-limit", "out of credits",
	"hit your", "reached your", "out of usage", "spend limit"}

func agentLimit(msg string) bool {
	low := strings.ToLower(msg)
	for _, k := range agentLimits {
		if strings.Contains(low, k) {
			return true
		}
	}
	return false
}

// classifyAgentError: napaka agenta za odjemalca: omejitev porabe, kvote ali kreditov je 429 rate_limit_error (Claude
// Code jo sporoči, ne ponavlja kot začasno 502), sicer 502; sporočilo brez poti in žetonov (besed s / ali \, razen
// http(s) naslovov; ID zahteve req_… ostane), krajše od 300 znakov.
func classifyAgentError(raw string) (int, string) {
	var words []string
	for _, w := range strings.Fields(raw) {
		url := strings.HasPrefix(w, "http://") || strings.HasPrefix(w, "https://")
		if !strings.HasPrefix(w, "req_") && !url && (strings.ContainsAny(w, `/\`) || strings.HasPrefix(w, "~") || utf8.RuneCountInString(w) >= 32) {
			continue
		}
		words = append(words, w)
	}
	msg := strings.Join(words, " ")
	if r := []rune(msg); len(r) > 300 {
		msg = string(r[:300])
	}
	if agentLimit(msg) {
		return 429, msg
	}
	return 502, msg
}
