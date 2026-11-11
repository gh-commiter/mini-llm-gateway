package main

// Codex CLI in Grok CLI kot agenta (kot RouterCore/Codex.swift in Grok.swift): claude-codex, claude-codex-<model>,
// claude-grok, claude-grok-<model>. Privzeto izklopljena; agent dela le v sinhronizirani kopiji mape odjemalca.
// Peskovnik postavi CLI sam (Codex: workspace-write / read-only, na Linuxu Landlock in seccomp; Grok: workspace /
// read-only). Oba bereta povsod: Grok dobi pravila --deny za mapo nastavitev. Windows (#68): Grok v škatli MXC (mxc.go);
// Codex in Copilot se v njej ne zaženeta (outsideBox), zato le z allowUnsandboxedBash. Skrbniški API zavrne
// procese agentov (peer check).

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
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
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
	Work       string   `json:"work,omitempty"`       // #45: najvišji način za pisanje (sync, worktree); "" = direct, vsi načini
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
// izvorni program v paketu npm ob ovoju (npmNative: codex.exe, copilot.exe).
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
	if exe := npmNative(k.name, filepath.Dir(p), runtime.GOARCH); exe != "" {
		return exe, nil
	}
	hint := map[string]string{"codex": " (npm install -g @openai/codex)", "copilot": " (npm install -g @github/copilot installs its copilot.exe; reinstall if it is missing)"}[k.name]
	return "", fmt.Errorf("%s is not an .exe and minigw serve does not run batch files: install the native %s.exe%s or set its path", p, k.bin, hint)
}

// npmNative (#77): izvorni program v paketu npm ob ovoju (copilot.cmd/.ps1 v dir). Copilot: npm-loader.js prek node
// le zažene copilot.exe iz platformnega paketa (@github/copilot-win32-<arch>, pri -g vgnezden v paketu), zato teče
// ta neposredno, brez node in ovoja (Copilot na Windows teče zunaj škatle, outsideBox).
func npmNative(name, dir, goarch string) string {
	pats := map[string][]string{"codex": {"@openai/codex/vendor/*/codex/codex.exe"}}[name]
	if name == "copilot" { // najprej arhitektura minigw, nato druga (x64 minigw na ARM64 Windows, Copilot arm64)
		archs := []string{"x64", "arm64"}
		if goarch == "arm64" {
			slices.Reverse(archs)
		}
		for _, a := range archs {
			pats = append(pats, "@github/copilot/node_modules/@github/copilot-win32-"+a+"/copilot.exe", "@github/copilot-win32-"+a+"/copilot.exe")
		}
	}
	for _, pat := range pats {
		m, _ := filepath.Glob(filepath.Join(dir, "node_modules", filepath.FromSlash(pat)))
		for _, f := range m {
			if st, err := os.Lstat(f); err == nil && st.Mode().IsRegular() {
				return f
			}
		}
	}
	return ""
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
// dir (#68): delovna mapa poizvedbe (Scratch\tmp), ne sistemska začasna mapa.
func agentCatalog(k agentKind, exe, dir string) []agentModel {
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
	if k.name == "codex" { // #59: brez projektnih nastavitev (.codex/config.toml) v začasni mapi ali nad njo
		args = append(codexUntrusted(dir), args...)
	}
	cmd := commandContext(ctx, exe, args...)
	cmd.Dir = dir
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
func codexArgs(cwd string, extraDirs []string, agent bool, output, model string, write, network bool, resume string, gits []string, path string, deny []string) []string {
	a := []string{"exec", "--json", "--skip-git-repo-check", "--color", "never", "-C", cwd, "-o", output}
	if model != "" {
		a = append(a, "-m", model)
	}
	// Sandbox tudi kot -c: »exec resume« nima --sandbox in --add-dir, -c pa sprejme (I5).
	sandbox := []string{"-c", `sandbox_mode="read-only"`}
	if agent && write && len(gits) > 0 { // #40: klon, profil dovoljenj namesto workspace-write
		sandbox = codexTreeProfile(append([]string{cwd}, extraDirs...), gits, deny, network, path)
	} else if agent && write {
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
	a = append(a, codexUntrusted(cwd)...)
	if resume != "" {
		a = append(a, "resume", resume)
	}
	return append(append(a, sandbox...), "-") // navodila prek stdin
}

// codexUntrusted (#59, kot Codex.untrustedProjects na Macu): projektne nastavitve (.codex/config.toml v mapi seje ali
// nad njo: notify, mcp_servers, hooki) Codex naloži le za zaupanja vredne mape v ~/.codex/config.toml in bi tekle zunaj
// peskovnika. Vsaka nadmapa cwd (tudi pot brez simbolnih povezav, ključ Codex) je za ta zagon »untrusted«: -c se združi
// nad uporabnikovo tabelo projects in natančen ključ mape prevlada. Windows: Codex ključe primerja z malimi črkami.
func codexUntrusted(cwd string) []string {
	keys := map[string]bool{}
	starts := []string{filepath.Clean(cwd)}
	if r, err := filepath.EvalSymlinks(cwd); err == nil {
		starts = append(starts, r)
	}
	if f := codexFinalPath(cwd); f != "" { // Windows: stičišča (codexpath_windows.go)
		starts = append(starts, f)
	}
	for _, d := range starts {
		for {
			if runtime.GOOS == "windows" {
				keys[strings.ToLower(d)] = true
			} else {
				keys[d] = true
			}
			up := filepath.Dir(d)
			if up == d {
				break
			}
			d = up
		}
	}
	var b strings.Builder
	for i, k := range slices.Sorted(maps.Keys(keys)) {
		q, _ := json.Marshal(k) // niz JSON je veljaven niz TOML
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(string(q) + `={trust_level="untrusted"}`)
	}
	return []string{"-c", "projects={" + b.String() + "}"}
}

// trimFinalPrefix: pot GetFinalPathNameByHandle brez »\\?\« (»\\?\UNC\« → »\\«), kot ključ Codex.
func trimFinalPrefix(s string) string {
	if r, ok := strings.CutPrefix(s, `\\?\UNC\`); ok {
		return `\\` + r
	}
	return strings.TrimPrefix(s, `\\?\`)
}

// codexTreeProfile (#40, kot Codex.treeProfile na Macu): v klonu profil dovoljenj Codex namesto workspace-write, ki
// .git vsake pisljive mape zaklene v celoti (commit ni mogoč): .git vsakega klona pisljiv razen nastavitev, hookov,
// info, commondir in preusmeritev (git harnessa jih bere zunaj peskovnika). Ukazi agenta: PATH z ovojem in brez
// GIT_DIR, GIT_WORK_TREE, GIT_COMMON_DIR (harness: /dev/null), iskanje repozitorija ne nad klonom (L1). deny: mape
// brez dostopa (L3: mapa nastavitev prehoda, kot --deny pri Grok).
func codexTreeProfile(writable, gits, deny []string, network bool, path string) []string {
	fs := map[string]string{"/": "read"} // #67: nič v /tmp (vse pod Scratch; TMPDIR ukazov je .build/tmp klona)
	for _, d := range deny {
		fs[d] = "none"
	}
	for _, d := range writable {
		fs[d] = "write"
	}
	for _, g := range gits {
		fs[g] = "write"
		for _, d := range gitDenies {
			fs[filepath.Join(g, d)] = "read"
		}
	}
	keys := slices.Sorted(maps.Keys(fs))
	var b strings.Builder
	for i, k := range keys {
		q, _ := json.Marshal(k)
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(string(q) + `="` + fs[k] + `"`)
	}
	p, _ := json.Marshal(path)
	c, _ := json.Marshal(filepath.Dir(filepath.Dir(gits[0]))) // nad vrhnjim klonom
	return []string{"-c", `default_permissions="minigw"`, "-c", "permissions.minigw.filesystem={" + b.String() + "}",
		"-c", fmt.Sprintf("permissions.minigw.network.enabled=%v", network),
		"-c", `shell_environment_policy.exclude=["GIT_DIR","GIT_WORK_TREE","GIT_COMMON_DIR"]`, "-c", "shell_environment_policy.set.PATH=" + string(p),
		"-c", "shell_environment_policy.set.GIT_CEILING_DIRECTORIES=" + string(c),
		"-c", `shell_environment_policy.set.MINIGW_SANDBOXED="1"`} // #67: le ukazi (v peskovniku), ne harness
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
func copilotPrepare(ctx context.Context, exe, home, cwd, cfgDir, scratch string, tmps []string) {
	if runtime.GOOS != "linux" {
		return // Windows: Copilot ni v škatli (outsideBox), paket razširi sam
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
	env := append(agentEnv(os.Environ()), "COPILOT_HOME="+home, "COPILOT_AUTO_UPDATE=false")
	name, args, done, err := agentWrap(exe, []string{"--version"}, agentMounts{cfgDir: cfgDir, rw: []string{copilotCacheDir(), home, cwd}, hide: scratch, tmps: tmps, noNet: true,
		cmdEnv: env, cwd: cwd})
	if err != nil {
		return
	}
	defer done()
	cmd := commandContext(ctx, name, args...)
	cmd.Dir = cwd
	cmd.Env = env
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

// agentUnconfined: agent na tem prehodu ne teče v svojem imenskem prostoru PID (Linux brez bwrap); spremenljivka za teste.
var agentUnconfined = func(cfgDir string) bool { return runtime.GOOS == "linux" && !agentSandboxed(cfgDir) }

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
		if r.h("x-claude-mini-ro") == "1" { // I4: pred WORKTREE-PUSH (deljena mapa worktree ni nikoli samo za branje)
			return "", "", false, writeError(w, r.rid, 400, readOnlyWorktree)
		}
		var refused string
		if dir, refused = s.hub.push(share, agent); refused != "" {
			return "", "", false, writeError(w, r.rid, 400, refused) // N1: končna, Claude Code je ne ponovi
		} else if dir == "" {
			return "", "", false, writeError(w, r.rid, 502, "Cloning "+clientCwd+" on the gateway failed (worktree mode; see minigw output and the gateway log)")
		}
		wt, branch, _, _, _ := s.hub.worktreeOf(r.h("x-claude-mini-session"), agent)
		gits, _ := s.hub.worktreeGits(wt)
		// #67: Copilot ima oznako peskovnika le pod bwrap (Linux); Codex prek profila, Grok v klonu le bere
		sandboxed := agent != "copilot" || runtime.GOOS == "linux" && !agentUnconfined(s.cfgDir)
		if runtime.GOOS == "windows" { // #68: ovoj git (minigw.exe kot git.exe) le v škatli MXC; Codex ni v njej
			sandboxed = windowsBox() && !outsideBox(agent)
		}
		return dir, worktreeNote(clientCwd, dir, branch, gits, sandboxed), false, 0
	case share != nil:
		if dir, _ = s.hub.push(share, agent); dir == "" {
			return "", "", false, writeError(w, r.rid, 502, "Syncing "+clientCwd+" to the gateway failed (see minigw output)")
		}
		return dir, syncNote(clientCwd, dir, share.readOnly), share.readOnly, 0
	case s.hub.isRefused(session):
		return "", "", false, writeError(w, r.rid, 400, refusedDirect)
	case r.h("x-claude-mini-share") == "1":
		return "", "", false, writeError(w, r.rid, 400, "The folder "+clientCwd+" did not reach this gateway: see the minigw message when it started")
	}
	return "", "", false, writeError(w, r.rid, 400, "The folder "+clientCwd+" is not shared with the gateway, so "+label+" agents cannot work in it: "+
		"in that folder run minigw share add (or MINIGW_SHARE=1 for one run), then start minigw again")
}

// syncNote: navodilo agentu za sinhronizirano kopijo mape odjemalca (kot ClaudeCLI.mountNote na Macu).
func syncNote(clientCwd, dir string, readOnly bool) string {
	note := "The user's folder " + clientCwd + " (on their computer) is synced here as " + dir + ", your working directory: a local copy"
	if readOnly {
		return note + ". Map paths under " + clientCwd + " to it. The folder is shared read-only: do not try to create, change or delete files in it."
	}
	return note + " whose changed source files are copied back to the user when you finish. Map paths under " + clientCwd + " to it."
}

// workLevels (#45): zaupanje omejitve agenta (work) in načina seje; višje = manj zaupanja. chat in refused nimata mape.
var (
	workLevels = map[string]int{"": 0, "direct": 0, "sync": 1, "worktree": 2}
	modeLevels = map[string]int{"direct": 0, "native-direct": 0, "sync": 1, "native": 2, "worktree": 2}
)

// workCap: omejitev načina agenta `kind` (claude, codex, grok, copilot); "" = direct.
func (c serveConfig) workCap(kind string) string {
	switch kind {
	case "claude":
		return c.ClaudeWork
	case "codex":
		return c.Codex.Work
	case "grok":
		return c.Grok.Work
	case "copilot":
		return c.Copilot.Work
	}
	return ""
}

// cappedKinds (#60): agenti, ki v načinu seje mode zaradi omejitve (work) le berejo; odgovor na registracijo mape jih
// pove odjemalcu za začetno vrstico (starejši odjemalci polje prezrejo).
func (c serveConfig) cappedKinds(mode string) []string {
	lvl, ok := modeLevels[mode]
	if !ok {
		return nil
	}
	var kinds []string
	for _, k := range []string{"claude", "codex", "copilot", "grok"} {
		if lvl < workLevels[c.workCap(k)] {
			kinds = append(kinds, k)
		}
	}
	return kinds
}

// agentNames: imena agentov v sporočilih uporabniku.
var agentNames = map[string]string{"claude": "Claude CLI", "codex": "Codex", "grok": "Grok", "copilot": "Copilot"}

// capped (#45): način seje (iz deljenja na prehodu, ne iz glave) je nad omejitvijo agenta → vrne omejitev in pošlje
// opombo odjemalcu; agent tedaj le bere. Vgnezdeni klic: seja klicatelja, omejitev vprašanega agenta. "" = brez omejitve.
func (s *server) capped(r *req, kind string) string {
	cap := s.conf().workCap(kind)
	session := r.h("x-claude-mini-session")
	mode, _, _ := s.hub.sessionMode(session, r.owner(), kind)
	if lvl, ok := modeLevels[mode]; !ok || lvl >= workLevels[cap] {
		return ""
	}
	if !s.hub.firstCapNote(session, kind) {
		return cap
	}
	modes := map[string]string{"sync": "sync or worktree mode", "worktree": "worktree mode"}[cap]
	hint := "run minigw --minigw-work=" + cap
	if cap == "worktree" { // worktree mode needs a git repository
		hint = "in a git repository, " + hint
	}
	s.hub.note(session, "Mini Gateway lets "+agentNames[kind]+" change files only in "+modes+", so it only read the folder ("+hint+")")
	return cap
}

// cappedNote: navodilo agentu, ki zaradi omejitve le bere (sync: kopija samo za branje; vgnezdeni: mapa klicatelja).
func cappedNote(r *req, dir, note string) string {
	if runOf(r) != nil {
		return note + " The folder is read-only for you: do not try to create, change or delete files in it."
	}
	return syncNote(r.h("x-claude-mini-cwd"), dir, true)
}

// appliedLimits (#46a): omejitve, ki jih je izvajanje res uporabilo (zgodovina jih ne izračuna znova).
type appliedLimits struct {
	readOnly bool
	cap      string
}

// runAgent: Codex ali Grok za eno zahtevo; odgovor kot en besedilni blok (pri pretoku ping med čakanjem).
func (s *server) runAgent(w http.ResponseWriter, r *req, body map[string]any, k agentKind, model string) int {
	c := s.conf()
	ac := k.conf(c)
	if k.name == "copilot" && copilotUnsupported {
		return writeError(w, r.rid, 400, copilotUnsupportedMsg)
	}
	if !s.agentsAllowed(c, k.name) {
		hint := ""
		if k.name == "codex" && runtime.GOOS == "windows" { // #68: tudi s peskovnikom »elevated« (bere nastavitve prehoda)
			hint = "; Codex's own Windows sandbox can read the gateway's settings, so it does not replace this switch"
		} else if k.name == "copilot" && runtime.GOOS == "windows" {
			hint = "; Copilot cannot start in the Windows sandbox"
		}
		return writeError(w, r.rid, 403, k.bin+" needs \"Allow commands without a sandbox\" (Claude CLI settings) on this gateway (Windows, "+
			"or Linux without bwrap): it can read and change everything this user can, including the gateway's settings"+hint)
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
	// worktree: Grok s pisanjem zavrne (besedilo kot na Macu); agent brez pisanja le bere v klonu (veje ne vrne).
	treeRun := s.hub.isWorktree(r.h("x-claude-mini-session"))
	if treeRun && k.name == "grok" && ac.canWrite(k) {
		return writeError(w, r.rid, 400, "Grok is not supported in worktree mode yet; rerun with minigw --minigw-work=sync …")
	}
	dir, note, readOnly, st := s.agentFolder(w, r, k.bin, k.name)
	if st != 0 {
		return st
	}
	cap := ""
	if agent { // #45: način seje nad omejitvijo agenta → le branje
		if cap = s.capped(r, k.name); cap != "" {
			note = cappedNote(r, dir, note)
			readOnly = true
		}
	}
	// --minigw-ro tudi brez mape le za branje
	readOnly = readOnly || r.h("x-claude-mini-ro") == "1"
	if readOnly && treeRun && runOf(r) == nil { // L2: kot na Macu (zahteva samo za branje; odjemalca je ne pošiljata)
		return writeError(w, r.rid, 400, readOnlyWorktree)
	}
	if treeRun && !ac.canWrite(k) { // agent brez pisanja: le branje v klonu, veje ne vrne
		name := map[string]string{"codex": "Codex", "copilot": "Copilot", "grok": "Grok"}[k.name]
		s.hub.note(r.h("x-claude-mini-session"), name+" may not write, so it only reads in the clone; no branch comes back")
		note, readOnly = worktreeReadOnlyNote(r.h("x-claude-mini-cwd"), dir), true
	}
	// #46a: zgodovina zapiše, kar je veljalo
	r.applied = &appliedLimits{readOnly: readOnly, cap: cap}
	s.boxNote(r, k.name)     // #68
	if c.askOthers(k.name) { // vrata izvajanja (startRun spodaj)
		// Absolutna pot: PATH izvajanja ne doseže ukazov v sandboxu Codex na Windows (»elevated« = drug uporabnik);
		// »minigw« iz PATH (askOnPath) je le za pravila Claude Code (askRules).
		note += "\n\n" + askNote(minigwExe(), false)
	} else {
		note += "\n\n" + noAskNote
	}
	prompt := note + "\n\n" + cliPrompt(history, last)
	var extra []string
	for _, f := range c.AllowedFolders {
		if f != dir && !scratchHiddenOverlaps(f, c.Scratch) && len(copilotCacheOverlaps([]string{f})) == 0 { // L1, #26
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
	// M2: preveč točk priklopa (pinPaths): končna zavrnitev (400) pred replyWhenDone
	if wt, _, _, _, inTree := s.hub.worktreeOf(r.h("x-claude-mini-session"), k.name); inTree || runOf(r) != nil {
		if p := runOf(r); p != nil { // klon klicatelja
			wt = p.wt
		}
		if wt != "" {
			if !s.hub.worktreeIntact(wt) { // H2 (kot runCLI): git harnessa ne z .git, ki ga je zamenjal agent (tudi klicatelja)
				return writeError(w, r.rid, 400, "The agent's clone was changed (its .git was replaced): start a new conversation")
			}
			gits, _ := s.hub.worktreeGits(wt)
			if tooDeep(wt, gits) {
				return writeError(w, r.rid, 400, tooManyPins)
			}
			if why := foreignGit(wt, dir, gits); why != "" { // #61: pred vsakim zagonom harnessa (tudi v klonu klicatelja)
				return writeError(w, r.rid, 400, why)
			}
		}
	}
	// N1: brez bwrap (svoj imenski prostor PID) agent lahko preživi do finalize; klon z ugnezdenimi: končna zavrnitev (400)
	// pred replyWhenDone, kot pri Claude CLI (L6).
	if agentUnconfined(s.cfgDir) {
		wt, _, _, _, inTree := s.hub.worktreeOf(r.h("x-claude-mini-session"), k.name)
		if p := runOf(r); p != nil { // klon klicatelja
			wt, inTree = p.wt, p.wt != ""
		}
		if inTree {
			if why := s.hub.markUnconfined(wt, k.name); why != "" {
				return writeError(w, r.rid, 400, why)
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
			hide: c.Scratch, tmps: s.tempRoots(), temp: tmp, unboxed: outsideBox(k.name), // #68: Codex in Copilot nista v škatli
			env: agentKeyEnv(s.cfgDir, k.name)} // #68: ključ API le, če ga je uporabnik izbral; dom v škatli svež vsak tek
		if runtime.GOOS == "windows" && outsideBox(k.name) { // #68: začasne datoteke Codex in Copilot pod Scratch (tek)
			m.env = append(m.env, "TEMP="+tmp, "TMP="+tmp)
			if k.name == "copilot" { // razširjen paket (~265 MB, enkrat na različico) in predpomnilnik SDK pod Scratch
				pkg := c.scratch(filepath.Join("cache", "copilot"))
				if under(pkg, dir) || under(dir, pkg) { // L3 (kot R9): orodje write ne sme spremeniti paketa za naslednji zagon
					return "", errors.New("copilot: the session's folder overlaps Copilot's package cache (" + pkg + ")")
				}
				m.env = append(m.env, "COPILOT_CACHE_HOME="+pkg)
			}
		}
		if runtime.GOOS == "windows" && state != "" && k.name == "grok" && agentAPIKey(s.cfgDir, "grok") != "" {
			os.Remove(filepath.Join(state, "auth.json")) // izbran ključ API: prijava ne gre v škatlo
		}
		wt, _, maven, objects, inTree := s.hub.worktreeOf(r.h("x-claude-mini-session"), k.name)
		if p := runOf(r); p != nil { // klon klicatelja
			wt, maven, objects, inTree = p.wt, p.maven, p.objects, p.wt != ""
		}
		var treeEnv []string // L-N9a: kot Claude CLI (harness Codex, Grok in Copilot sam poganja git v klonu)
		gw := c.scratch("gitexec")
		gits, objs := s.hub.worktreeGits(wt)
		if len(objs) == 0 {
			objs = []string{objects}
		}
		var deps []string // #67: $MINIGW_DEPS in skupni predpomnilniki, le s pisanjem (sicer agent nima MINIGW_DEPS)
		if inTree {
			if treeEnv = worktreeClaudeEnv(wt, maven, filepath.Join(wt, ".build", "tmp"), gw, gits, k.name); treeEnv == nil {
				return "", errors.New("cannot prepare git for the agent's clone (git --exec-path); use sync")
			}
			if write { // #67 H1: mape za pisanje pod Scratch preverjene, preden nastane peskovnik
				var fixed []string
				if deps, fixed, err = grantDirs(wt); err != nil {
					s.log.Printf("%s worktree folders: %v", r.rid, err)
					return "", errors.New("the agent's folders on the gateway are not safe to use (see the gateway log): start a new conversation")
				}
				for _, f := range fixed {
					s.log.Printf("%s worktree: %s was not a folder of the gateway; made again", r.rid, f)
				}
				defer s.recheckGrants(r.rid, wt) // #73: po teku znova
				treeEnv = append(treeEnv, networkOffEnv(k.name, ac.Network)...)
			} else {
				treeEnv = slices.DeleteFunc(treeEnv, func(kv string) bool { return strings.HasPrefix(kv, "MINIGW_DEPS=") })
			}
		}
		run, runEnv := s.startRun(r, k.name, dir, readOnly, true, wt, maven, objects, tmp) // »May ask other models«; mapa je deljena (agentFolder); žeton v tmp zahteve
		defer run.stop()
		withTree := func(m agentMounts) agentMounts { // worktree: Maven agentov, objekti zrcala (branje), .git klona le za branje
			m.env, m.started, m.loopback = append(m.env, runEnv...), run.started, run.asks() // #68: loopback le za vrata izvajanja
			if runEnv != nil {
				m.ro = append(m.ro, minigwExe())
			}
			if inTree {
				if write { // #68: koren klona (tudi .git za commit), ko je mapa podmapa
					m.rw = append(m.rw, wt)
				}
				m.rw, m.ro = append(append(m.rw, maven), deps...), append(append(m.ro, objs...), gw)
				m.pins = append(pinPaths(wt, gits), deps...) // L1, M2: .git vsakega klona in mape do ugnezdenih so točke priklopa (mv/rm: EBUSY); #67 H1: tudi mape pod Scratch
				for _, g := range gits {
					for _, d := range []string{"config", "hooks", "info", "commondir"} {
						m.protect = append(m.protect, filepath.Join(g, d))
					}
				}
				m.env = append(m.env, treeEnv...)
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
			if token == "" && runtime.GOOS != "darwin" { // M1: svež COPILOT_HOME nima prijave copilot login
				return "", errors.New("copilot: no GitHub sign-in on the gateway; sign in with the GitHub CLI (gh auth login) or set a token (minigw gw copilot token)")
			}
			if token != "" {
				m.env = append(m.env, "COPILOT_GITHUB_TOKEN="+token)
			}
			if deps != nil && runtime.GOOS == "linux" && !agentUnconfined(s.cfgDir) { // #67: ves Copilot je v bwrap (ovoj git: init/clone v $MINIGW_DEPS)
				m.env = append(m.env, "MINIGW_SANDBOXED=1")
			}
			// C2: svež COPILOT_HOME vsake zahteve (hooki, nastavitve, zaupane mape prejšnjega zagona ne obstajajo); med
			// zahtevami ostane le stanje seje (brez povezav) v stanju naprave, ki ga bwrap ne vidi. M1: tudi na Windows zunaj škatle (nikoli ~\.copilot: hooki, MCP, zaupane mape;
			// brez zaupane mape copilot v načinu -p hookov in MCP repozitorija ne naloži), prijava le z žetonom.
			store := ""
			if state != "" {
				store = filepath.Join(state, "sessions")
				m.state, m.files = filepath.Join(tmp, "home"), nil
				if copySession(filepath.Join(store, resume), filepath.Join(m.state, "session-state", resume)) != nil {
					os.MkdirAll(filepath.Join(m.state, "session-state"), 0o700)
					prompt = note + "\n\n" + cliPrompt(history, last) // stanja seje ni: ves pogovor
				}
			}
			copilotPrepare(ctx, exe, filepath.Join(tmp, "prep"), tmp, s.cfgDir, c.Scratch, s.tempRoots())
			args := copilotArgs(agent, model, write, ac.Commands, ac.Network, resume)
			var answer string
			err = s.runAgentProcess(ctx, r, exe, args, withTree(m), dir, prompt, func(line []byte) {
				t, st := copilotEvent(line)
				if t != "" {
					answer = t // odgovor je zadnje sporočilo agenta
				}
				if st != "" {
					s.log.Printf("%s step: %q", r.rid, st)
					s.activity.step(r.rid, st)
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
			var cgits, cdeny []string
			cpath := ""
			if inTree && runtime.GOOS != "windows" {
				cgits, cpath, cdeny = gits, agentPath(gw), []string{s.cfgDir}
			}
			args := codexArgs(dir, append(extra, deps...), agent, out, model, write, ac.Network, resume, cgits, cpath, cdeny)
			if runtime.GOOS == "windows" { // #68: nikoli namestitev z UAC iz teka prehoda
				if why := codexSandboxProblem(); why != "" {
					return "", errors.New(why)
				}
				if codexElevated() { // dodatna zaščita (~/.ssh, profil), ne odklene Codex
					args = append([]string{args[0], "-c", `windows.sandbox="elevated"`}, args[1:]...)
				}
			}
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
					if !codexMessage(line) { // besedilo agenta le v dnevnik
						s.activity.step(r.rid, st)
					}
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
					s.activity.step(r.rid, e.step)
					answer = "" // odgovor je besedilo po zadnjem orodju
				case e.err != "":
					failure = e.err
					cancel() // napaka konča tek: ne čakaj, da grok konča sam
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
			run.stop() // I2: vprašanja drugim modelom se končajo pred prevzemom sprememb
			// vprašani agent: spremembe prenese klicatelj na koncu; L1: omejen (#45) ne prenese ničesar
			if runOf(r) == nil && (r.applied == nil || r.applied.cap == "") {
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
		name = strings.ToUpper(name)             // Windows: imena spremenljivk brez razlike med velikimi in malimi črkami
		if strings.HasSuffix(name, "_API_KEY") { // #68: noben podedovan ključ API (le izrecno nastavljen, agentKeyEnv)
			continue
		}
		if strings.HasPrefix(name, "ANTHROPIC_") || strings.HasPrefix(name, "CLAUDE_CODE_") || slices.Contains([]string{"CLAUDECODE",
			"DBUS_SESSION_BUS_ADDRESS", "SSH_AUTH_SOCK", "GPG_AGENT_INFO", "GNOME_KEYRING_CONTROL", "CODEX_HOME", "GROK_HOME", "MINIGW_AGENT_URL", "MINIGW_AGENT_AUTH_FILE",
			"GH_TOKEN", "GITHUB_TOKEN", "SANDBOX_RUNTIME", "MINIGW_SANDBOXED", "CODEX_SANDBOX", "CODEX_SANDBOX_NETWORK_DISABLED", // #67: oznake peskovnika
			"MINIGW_DEPS", "MINIGW_NETWORK_OFF",
			// #68: ključi API iz okolja prehoda nikoli do agenta (obračun API); le izrecno nastavljen (agentKeyEnv)
			"OPENAI_API_KEY", "CODEX_API_KEY", "XAI_API_KEY", "GROK_CODE_XAI_API_KEY", "GEMINI_API_KEY", "GOOGLE_API_KEY"}, name) || strings.HasPrefix(name, "COPILOT_") ||
			strings.HasPrefix(name, "GITHUB_COPILOT_") { // M1: GITHUB_COPILOT_PROMPT_MODE_REPO_HOOKS (… _WORKSPACE_MCP, _EXTENSIONS) naloži hooke repozitorija
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
	// brez škatle (in Codex): kot prej, prave mape CLI. M1: Copilot vedno svoje stanje (svež COPILOT_HOME vsak tek, brez
	// hookov, MCP in zaupanih map iz ~\.copilot; brez zaupanja copilot hookov in MCP repozitorija ne naloži)
	if runtime.GOOS == "windows" && (!windowsBox() && k.name != "copilot" || k.name == "codex") {
		return "", nil, nil
	}
	sum := sha256.Sum256([]byte(owner))
	dir := filepath.Join(scratch, "state", hex.EncodeToString(sum[:8]), k.name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", nil, err
	}
	real, names := agentAuthFiles(k)
	if runtime.GOOS == "windows" { // #68: povezave zahtevajo pravice, trde ne prečkajo nosilcev: le kopija noter, nikoli nazaj
		// pred vsakim tekom vse drugo iz stanja ven (nastavitve, vtičniki, zaupane mape, ki jih je pustil prejšnji tek);
		// ostanejo le prijava, nastavitve in seje (-r nadaljuje sejo)
		ents, _ := os.ReadDir(dir)
		for _, e := range ents {
			if n := e.Name(); !slices.Contains(names, n) && n != "sessions" {
				os.RemoveAll(filepath.Join(dir, n))
			}
		}
		for _, n := range names {
			if strings.HasSuffix(n, ".lock") {
				continue
			}
			if err := authCopyIn(filepath.Join(real, n), filepath.Join(dir, n)); err != nil {
				return "", nil, err
			}
		}
		return dir, nil, nil
	}
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
	if runtime.GOOS == "windows" { // #68: iz škatle nikoli nič nazaj
		return
	}
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
	env := append(agentEnv(os.Environ()), m.env...)
	if m.state != "" {
		env = append(env, m.envVar+"="+m.state)
	}
	m.cmdEnv, m.cwd = env, dir
	name, args, done, err := agentWrap(exe, args, m) // Linux: domača mapa in mapa nastavitev skriti (bwrap), če je na voljo; Windows: škatla MXC
	if err != nil {
		return err
	}
	defer done()
	cmd := commandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdin = strings.NewReader(stdin)
	stderr := &tailBuffer{}
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err == nil {
		err = startAgent(cmd) // Windows: Job agentov (tudi proces v škatli, otrok wxc-exec); Linux: pomočnik subreaper (peer check)
	}
	if err != nil {
		return err
	}
	if m.started != nil {
		m.started(cmd.Process.Pid)
	}
	s.agentsRunning.Add(1)
	defer s.agentsRunning.Add(-1)
	defer context.AfterFunc(ctx, func() { stdout.Close() })() // N1: branje konča tudi, ko vnuk še drži stdout
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
	werr := cmd.Wait()
	if err := stopAgentTree(cmd); err != nil { // Windows: drevo teka ubito, preden klicatelj prevzame spremembe; sicer jih ne prevzame
		s.log.Printf("%s %s: %v", r.rid, filepath.Base(exe), err)
		return fmt.Errorf("%s: its processes did not stop after the run, so the gateway did not take its changes; details are in the gateway log, request %s", filepath.Base(exe), r.rid)
	}
	if err := werr; err != nil {
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

// agentHeadDelay: pri pretoku glava 200 šele po tem času (nato ping vsakih 10 s). Napaka prej (omejitev agenta) je pravi
// status HTTP z x-should-retry: false; po glavi je le dogodek SSE »error«, na katerega Claude Code ponovi zahtevo brez
// pretoka. Testi: krajši.
var agentHeadDelay = 30 * time.Second

// replyWhenDone: ponudnik brez pretoka (Codex, Grok): pri pretoku glava, message_start in ping šele po agentHeadDelay,
// nato cel odgovor (kot Router.replyWhenDone).
func (s *server) replyWhenDone(w http.ResponseWriter, r *req, body map[string]any, work func() (string, error)) int {
	model, _ := body["model"].(string)
	idb := make([]byte, 16)
	rand.Read(idb)
	t := &translator{model: model, id: fmt.Sprintf("msg_%x", idb)}
	stream := body["stream"] == true
	var mu sync.Mutex
	headSent := false
	send := func(b []byte) {
		mu.Lock()
		defer mu.Unlock()
		if !headSent {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("x-should-retry", "false")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("request-id", r.rid)
			w.WriteHeader(200)
			w.Write(t.start())
			headSent = true
		}
		w.Write(b)
		http.NewResponseController(w).Flush()
	}
	done, stopped := make(chan struct{}), make(chan struct{})
	if stream {
		go func() {
			defer close(stopped)
			wait := agentHeadDelay
			for {
				select {
				case <-time.After(wait):
					send(sse("ping", map[string]string{"type": "ping"}))
					wait = 10 * time.Second
				case <-done:
					return
				}
			}
		}()
	} else {
		close(stopped)
	}
	text, err := work()
	close(done)
	<-stopped
	if r.Context().Err() != nil {
		return 499
	}
	if err != nil {
		s.log.Printf("%s %s failed: %v", r.rid, model, err)
		status, msg := classifyAgentError(err.Error())
		if _, early := w.(*earlyWriter); stream && (headSent || early) { // napaka bo dogodek SSE
			s.noteLimit(r, model, status, msg)
		}
		if headSent {
			send(sse("error", map[string]any{"type": "error", "error": map[string]string{"type": errType(status), "message": msg}}))
			return status
		}
		// HPX360: agent je tekel in padel; ponovitev Claude Code bi ga zagnala znova (po glavi je napaka že brez ponovitve)
		w.Header().Set("x-should-retry", "false")
		return writeAgentError(w, r.rid, status, msg)
	}
	t.usage = map[string]any{"output_tokens": float64(len(text) / 4)}
	if stream {
		send(append(t.emit(text), t.finish()...))
		return 200
	}
	t.emit(text)
	return writeJSON(w, r.rid, 200, t.message())
}

// agentsAllowed: brez sandboxa, ki skrije skrivnosti routerja (Windows; Linux brez delujočega bwrap), Codex in Grok
// bereta in pišeta vse, kar uporabnik: le z izrecnim allowUnsandboxedBash, v vsakem načinu (H1, R3).
// #68: Codex in Copilot na Windows ne tečeta v škatli MXC (outsideBox). Codexov peskovnik »elevated« bere
// večino profila, tudi %APPDATA%\minigw (admin.secret, serve.json; HPX360), zato Codex tam le z izrecnim dovoljenjem.
// copilotUnsupported (0.5.24, lastnik): Copilot na prehodu Windows ni podprt. V škatli MXC se ne zažene (outsideBox),
// zunaj nje pa bi potreboval ~\.copilot (prijava copilot login je v shrambi poverilnic s ključem, ki ga ne poznamo, ob
// hookih, MCP in zaupanih mapah, M1). Ni na seznamu modelov, vklop in zahteve so zavrnjeni. Spremenljivka za teste.
var copilotUnsupported = runtime.GOOS == "windows"

const copilotUnsupportedMsg = "Copilot is not supported on a Windows gateway in this version"

func (s *server) agentsAllowed(c serveConfig, kind string) bool {
	if outsideBox(kind) && runtime.GOOS == "windows" {
		return c.AllowUnsandboxedBash
	}
	return agentSandboxed(s.cfgDir) || c.AllowUnsandboxedBash
}

// codexElevated (#68): Codex ima nastavljen svoj peskovnik Windows »elevated« (enkratni »codex sandbox setup
// --elevated« kot skrbnik): [windows] sandbox = "elevated" v config.toml in mapa .sandbox-secrets (gesla uporabnikov
// peskovnika) v CODEX_HOME. Ta peskovnik ukazom ne da brati ~/.ssh in pisati v profil (HPX360, 2026-11-10).
func codexElevated() bool {
	real, _ := agentAuthFiles(codexKind)
	b, err := os.ReadFile(filepath.Join(real, "config.toml"))
	if err != nil || !tomlElevated(string(b)) {
		return false
	}
	st, err := os.Stat(filepath.Join(real, ".sandbox-secrets"))
	return err == nil && st.IsDir()
}

// codexSandboxProblem (#68): config.toml zahteva »elevated«, peskovnik pa ni nastavljen (ni .sandbox-secrets): Codex bi
// iz teka prehoda sprožil UAC (nihče ga ne potrdi: »error 1223«). "" = v redu.
func codexSandboxProblem() string {
	real, _ := agentAuthFiles(codexKind)
	b, err := os.ReadFile(filepath.Join(real, "config.toml"))
	if err != nil || !tomlElevated(string(b)) || codexElevated() {
		return ""
	}
	return "Codex's elevated Windows sandbox is not set up on the gateway: run codex sandbox setup --elevated once at the gateway PC (as administrator)"
}

// tomlElevated: [windows] sandbox = "elevated" ali windows.sandbox = "elevated" (preprosto branje TOML, brez tabel v vrstici).
func tomlElevated(toml string) bool {
	section := ""
	for _, l := range strings.Split(toml, "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		if strings.HasPrefix(l, "[") {
			section = strings.TrimSpace(strings.Trim(l, "[]"))
			continue
		}
		k, v, ok := strings.Cut(l, "=")
		if !ok {
			continue
		}
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if i := strings.Index(v, "#"); i >= 0 {
			v = strings.TrimSpace(v[:i])
		}
		if (section == "windows" && k == "sandbox" || section == "" && k == "windows.sandbox") && strings.Trim(v, `"'`) == "elevated" {
			return true
		}
	}
	return false
}

// agentModelList: claude-codex in claude-codex-<model> (vklopljeni), enako za Grok; le vklopljen agent z najdenim CLI.
func (s *server) agentModelList(k agentKind) []map[string]any {
	c := s.conf()
	ac := k.conf(c)
	exe, _ := resolveAgent(k, ac.Executable)
	if !ac.Enabled || exe == "" || !s.agentsAllowed(c, k.name) || k.name == "copilot" && copilotUnsupported {
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
	for _, m := range agentCatalog(k, exe, c.scratch("tmp")) {
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

// noteLimit: omejitev agenta, sporočena kot dogodek SSE (glava 200 je bila že poslana): Claude Code na tako napako takoj
// ponovi zahtevo brez pretoka; recentLimit ji 60 s odgovori z istim 429 brez novega zagona CLI.
func (s *server) noteLimit(r *req, model string, status int, msg string) {
	if status != 429 {
		return
	}
	now := time.Now()
	s.limits.Range(func(k, v any) bool { // stari vnosi
		if now.Sub(v.(limitHit).at) > time.Minute {
			s.limits.Delete(k)
		}
		return true
	})
	s.limits.Store(limitKey(r, model), limitHit{msg, now})
}

// recentLimit: nedavna omejitev za ta model, napravo in sejo (enkrat; le zahteva brez pretoka).
func (s *server) recentLimit(r *req, model string) (string, bool) {
	v, ok := s.limits.LoadAndDelete(limitKey(r, model))
	if !ok || time.Since(v.(limitHit).at) > time.Minute {
		return "", false
	}
	return v.(limitHit).msg, true
}

type limitHit struct {
	msg string
	at  time.Time
}

func limitKey(r *req, model string) string {
	return model + "|" + r.owner() + "|" + r.h("x-claude-mini-session")
}

var apiErrorPrefix = regexp.MustCompile(`^API error \(status \d+[^)]*\):\s*`)

// agentMessage: besedilo napake CLI brez ovoja: JSON {"message":…}, {"error":{"message":…}} ali {"error":"…"} (tudi za
// predpono, npr. »Internal error: {…}«, in ovoj v ovoju) ter brez »API error (status 402 …):«; sicer vrstica, kot je.
// Brez kontrolnih in oblikovnih znakov (clean): JSON \u001b bi sicer postal pravi ESC v terminalu odjemalca (L4).
func agentMessage(raw string) string {
	msg := strings.TrimSpace(raw)
	for range 3 {
		i, j := strings.Index(msg, "{"), strings.LastIndex(msg, "}")
		if i < 0 || j < i {
			break
		}
		var o map[string]any
		if json.Unmarshal([]byte(msg[i:j+1]), &o) != nil {
			break
		}
		m, _ := o["message"].(string)
		if e, ok := o["error"].(map[string]any); ok && m == "" {
			m, _ = e["message"].(string)
		} else if e, ok := o["error"].(string); ok && m == "" {
			m = e
		}
		if strings.TrimSpace(m) == "" {
			break
		}
		msg = strings.TrimSpace(m)
	}
	return apiErrorPrefix.ReplaceAllString(clean(msg), "")
}

// classifyAgentError: napaka agenta za odjemalca: omejitev porabe, kvote ali kreditov je 429 rate_limit_error (Claude
// Code jo sporoči, ne ponavlja kot začasno 502), sicer 502; sporočilo brez poti in žetonov (besed s / ali \, razen
// http(s) naslovov; ID zahteve req_… ostane), krajše od 300 znakov.
func classifyAgentError(raw string) (int, string) {
	var words []string
	for _, w := range strings.Fields(agentMessage(raw)) {
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
	if agentLimit(raw) { // tudi ovoj (»Payment Required«)
		return 429, msg
	}
	return 502, msg
}

// writeAgentError: napaka agenta; 429 zaradi omejitve agenta ima x-should-retry: false (Claude Code ga ponavlja
// drugače), prehodni 429 (zasedeno) ga nima.
func writeAgentError(w http.ResponseWriter, rid string, status int, msg string) int {
	if status == 429 {
		w.Header().Set("x-should-retry", "false")
	}
	return writeError(w, rid, status, msg)
}
