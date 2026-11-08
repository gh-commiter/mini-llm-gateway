package main

// minigw serve: majhen router Mini Gateway za Windows in Linux (kot RouterCore/Server.swift in Router.swift).
// Le šifra MGW2 (AES-GCM, v standardni knjižnici Go), ponudnik claude -p (serve_cli.go) in sinhronizirana kopija
// mape odjemalca (serve_sync.go) in prehodi (serve_gateway.go). Navaden HTTP le z loopback. Brez Siri in Apple FM.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type serveDevice struct {
	ID   string `json:"id"` // javni ključ naprave (Ed25519, base64url), iz dokaza Noise
	Name string `json:"name"`
}

type serveConfig struct {
	Token          string        `json:"token"`
	Key            string        `json:"key"` // zasebni ključ routerja (X25519, base64url); javni del gre v žeton
	Port           int           `json:"port"`
	Address        string        `json:"address,omitempty"` // naslov za poslušanje; prazno = vsi
	Scratch        string        `json:"scratch,omitempty"` // kopije map in pogovori; prazno = mapa predpomnilnika
	Log            string        `json:"log,omitempty"`     // prazno = stderr
	Claude         string        `json:"claude,omitempty"`  // pot do claude; prazno = PATH
	Tools          string        `json:"tools"`             // orodja agenta (--tools, --allowedTools)
	PermissionMode string        `json:"permissionMode"`
	Devices        []serveDevice `json:"devices"`
	AllowAnyDevice bool          `json:"allowAnyDevice,omitempty"` // le pri praznem seznamu: vsaka naprava z žetonom
	AllowLocalApps *bool         `json:"allowLocalApps,omitempty"` // modeli brez žetona za aplikacije tega uporabnika (nil = ne; H1)
	// Bash v tools na Windows teče brez sandboxa (dostop do vsega, kar sme uporabnik): le z izrecnim dovoljenjem.
	AllowUnsandboxedBash bool           `json:"allowUnsandboxedBash,omitempty"`
	Gateways             []serveGateway `json:"gateways,omitempty"` // ključi v shrambi sistema, ne tu (serve_gateway.go)
	// Kot nastavitve na Macu (docs/windows-parity.md).
	AllowedClients   string        `json:"allowedClients,omitempty"`   // IP naslovi, ločeni z vejico ali presledkom; prazno = vsi
	AllowPlainRemote bool          `json:"allowPlainRemote,omitempty"` // navaden HTTP z drugih računalnikov (brez ključa naprave)
	ClaudeDisabled   bool          `json:"claudeDisabled,omitempty"`   // Claude CLI izklopljen (privzeto vklopljen)
	AllowedFolders   []string      `json:"allowedFolders,omitempty"`   // dodatne mape, v katerih agenti smejo delati
	AllowLocalTools  bool          `json:"allowLocalTools,omitempty"`  // vsa orodja Claude Code (razen DisabledTools)
	ClaudeAskOthers  bool          `json:"claudeAskOthers,omitempty"`  // agent Claude CLI sme vprašati druge modele
	DisabledTools    []string      `json:"disabledTools,omitempty"`
	SettingSources   string        `json:"settingSources,omitempty"` // --setting-sources; prazno = brez (hooki iz mape ne tečejo)
	Codex            agentCLI      `json:"codex"`                    // Codex CLI (serve_agents.go), privzeto izklopljen
	Grok             agentCLI      `json:"grok"`                     // Grok CLI, privzeto izklopljen
	Foundry          foundryConfig `json:"foundry"`                  // Foundry Local na napravi (foundry.go), privzeto izklopljen
	Copilot          agentCLI      `json:"copilot"`                  // GitHub Copilot CLI, privzeto izklopljen (le branje)
}

const serveHelp = `minigw serve: Mini Gateway, the LLM gateway, on this computer (Windows, Linux) for minigw clients.

  minigw serve init [--port N] [--config FILE]   create the config, print the minigw connect line
  minigw serve install | uninstall [--config FILE]              start the gateway at sign-in (per user, no admin)
  minigw serve [--port N] [--config FILE]        run the gateway
  minigw gw status|devices|provider|claude|token …              show or change the gateway, live if it runs
  minigw serve allow|deny|devices|provider|status …             the same as minigw gw devices allow|deny, …

Clients use: MINIGW_WORK=sync, worktree (a git repository, results as branches) or auto minigw … (folder mounts,
native mode and minigw run are macOS only).
Unknown devices are refused; the log prints their ID. Before minigw serve allow, compare the ID with the one on the
client (it is the device key of the computer that just connected): anyone with the token can try to connect.
Connections from this computer itself (loopback) skip the device check and encryption: do not forward the port to
loopback (ssh -L, netsh portproxy, a reverse proxy), or remote clients get the same trust.`

// Privzeta orodja: urejanje datotek v kopiji; Bash (brez sandboxa na Windows) le, če ga nastavitve dodajo.
const defaultTools = "Read,Grep,Glob,Edit,Write,MultiEdit"

func defaultServeConfigPath() string {
	dir, err := configBase()
	if err != nil {
		dir = "."
	}
	return filepath.Join(dir, "minigw", "serve.json")
}

// configBase: mapa nastavitev brez spremenljivk okolja (XDG_CONFIG_HOME, HOME, APPDATA): proces, ki jih nastavi, ne
// sme minigw router preusmeriti na lažen serve.json. Windows: znana mapa RoamingAppData; drugod domača mapa iz baze
// uporabnikov. Šele če to ne uspe, os.UserConfigDir.
func configBase() (string, error) {
	if dir, err := knownConfigDir(); err == nil && dir != "" {
		return dir, nil
	}
	return os.UserConfigDir()
}

func loadServeConfig(path string) (serveConfig, error) {
	var c serveConfig
	data, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(data, &c); err != nil {
		return c, fmt.Errorf("%s: %v", path, err)
	}
	// Starejši »folder« (mapa za agente brez sinhronizacije) postane dovoljena mapa; privzete mape ni več (kot na Macu).
	var old struct {
		Folder string `json:"folder"`
	}
	if json.Unmarshal(data, &old) == nil && old.Folder != "" && !slices.Contains(c.AllowedFolders, old.Folder) {
		c.AllowedFolders = append(c.AllowedFolders, old.Folder)
	}
	if c.Port == 0 {
		c.Port = 8765
	}
	if c.Tools == "" {
		c.Tools = defaultTools
	}
	if c.PermissionMode == "" {
		c.PermissionMode = "dontAsk"
	}
	ensureBuiltinGateways(&c)
	return c, nil
}

func saveServeConfig(path string, c serveConfig) {
	if err := writeServeConfig(path, c); err != nil {
		fail(err.Error())
	}
}

func writeServeConfig(path string, c serveConfig) error {
	data, _ := json.MarshalIndent(c, "", "  ")
	if err := writeFileAtomic(path, append(data, '\n')); err != nil {
		return errors.New("cannot save " + path + ": " + err.Error())
	}
	return nil
}

// writeFileAtomic: nova datoteka 0600 v isti mapi, Sync, preimenovanje: povezava na poti se ne sledi, datoteka ni
// nikoli širša od 0600 in nikoli napol zapisana.
func writeFileAtomic(path string, data []byte) error {
	os.MkdirAll(filepath.Dir(path), 0o700)
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+"-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name()) // po uspešnem preimenovanju ne obstaja več
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// connectLine: vrstica za odjemalce (žeton#javni ključ routerja).
func connectLine(c serveConfig) string {
	t := connectToken(c)
	if t == "" {
		return ""
	}
	return fmt.Sprintf("minigw connect %s:%d --token '%s'", localIP(), c.Port, t)
}

// connectToken: žeton#javni ključ, kar kopira Copy Token (kot na Macu).
func connectToken(c serveConfig) string {
	k, err := c.privateKey()
	if err != nil {
		return ""
	}
	return c.Token + "#" + base64url(k.PublicKey().Bytes())
}

func (c serveConfig) privateKey() (*ecdh.PrivateKey, error) {
	b, err := fromBase64url(c.Key)
	if err != nil {
		return nil, err
	}
	return ecdh.X25519().NewPrivateKey(b)
}

// scratch: podmapa scratch (c.Scratch nastavi newServer).
func (c serveConfig) scratch(sub string) string {
	p := filepath.Join(c.Scratch, sub)
	os.MkdirAll(p, 0o700)
	return p
}

// clientAllowed: IP na seznamu dovoljenih odjemalcev (prazen seznam = vsi), kot allowedClients na Macu.
func (c serveConfig) clientAllowed(ip string) bool {
	list := strings.FieldsFunc(c.AllowedClients, func(r rune) bool { return r == ',' || r == ' ' })
	if len(list) == 0 {
		return true
	}
	p := net.ParseIP(ip)
	for _, a := range list {
		if q := net.ParseIP(a); q != nil && p != nil && q.Equal(p) {
			return true
		}
	}
	return false
}

// deviceAllowed: naprava s seznama; neznana le pri praznem seznamu z allowAnyDevice. Loopback (ta računalnik) vedno.
func (c serveConfig) deviceAllowed(d *serveDevice, loopback bool) bool {
	if loopback {
		return true
	}
	if len(c.Devices) == 0 { // tudi odjemalec brez ključa naprave (kot na Macu)
		return c.AllowAnyDevice
	}
	if d == nil { // oddaljen odjemalec brez ključa naprave pri nepraznem seznamu nikoli
		return false
	}
	for _, a := range c.Devices {
		if d != nil && a.ID == d.ID {
			return true
		}
	}
	return false
}

// listed: naprava je na seznamu dovoljenih.
func (c serveConfig) listed(id string) bool {
	return slices.ContainsFunc(c.Devices, func(x serveDevice) bool { return x.ID == id })
}

func serveCommand(args []string) {
	sub := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	switch sub { // starejši ukazi: ista koda kot minigw router
	case "allow", "deny":
		routerCommand(append([]string{"devices", sub}, args...), os.Stdin)
		return
	case "devices", "provider", "gateway", "status":
		routerCommand(append([]string{sub}, args...), os.Stdin)
		return
	}
	fs := flag.NewFlagSet("minigw serve", flag.ExitOnError)
	cfgPath := fs.String("config", defaultServeConfigPath(), "config file")
	port := fs.Int("port", 0, "port (default 8765)")
	folder := fs.String("folder", "", "deprecated: adds an allowed folder (use minigw gw claude --folders)")
	fs.BoolVar(&allowDowngrade, "allow-downgrade", false, "install: replace a newer installed minigw (#25)")
	fs.Usage = func() { fmt.Fprintln(os.Stderr, serveHelp) }
	fs.Parse(args)

	switch sub {
	case "init":
		c, err := loadServeConfig(*cfgPath)
		if errors.Is(err, os.ErrNotExist) {
			c = newServeConfig()
		} else if err != nil {
			fail(err.Error())
		}
		if *port != 0 {
			c.Port = *port
		}
		deprecatedFolder(&c, *folder)
		abs, _ := filepath.Abs(filepath.Dir(*cfgPath)) // zasebna mapa pred prvim zapisom žetona in ključa
		os.MkdirAll(abs, 0o700)
		if err := protectConfigDir(abs); errors.Is(err, errForeignOwner) {
			fail(abs + " " + err.Error() + ": use a folder of your own")
		} else if err != nil {
			say("WARNING: cannot make " + abs + " private to this user: use the default config folder")
		}
		saveServeConfig(*cfgPath, c)

		if connectLine(c) == "" {
			fail("invalid key in " + *cfgPath)
		}
		say("Config: " + *cfgPath)
		say("On each client run:")
		say("  " + connectLine(c))
		say("Then start the gateway with: minigw serve" + map[bool]string{true: "", false: " --config " + *cfgPath}[*cfgPath == defaultServeConfigPath()])
		say("Unknown devices are refused; the gateway log shows how to allow one (minigw serve allow <id>).")
		return
	case "install", "uninstall":
		msg, err := serviceCommand(sub, runtime.GOOS, *cfgPath)
		if err != nil {
			fail(err.Error())
		}
		say(msg)
		return
	case "":
	case "help":
		say(serveHelp)
		return
	default:
		fail("unknown command minigw serve " + sub + "\n\n" + serveHelp)
	}

	c, err := loadServeConfig(*cfgPath)
	if errors.Is(err, os.ErrNotExist) {
		fail("cannot read " + *cfgPath + ": run minigw serve init first (" + err.Error() + ")")
	} else if err != nil {
		fail("the gateway cannot start with these settings, fix them: " + err.Error())
	}
	if *port != 0 {
		c.Port = *port
	}
	deprecatedFolder(&c, *folder)
	s, err := newServer(c, *cfgPath)
	if err != nil {
		fail(err.Error())
	}
	hardenRouter() // Linux: ni dumpable (agenti ne berejo pomnilnika routerja)
	ln, err := listenRouter(c)
	if err != nil {
		fail(err.Error())
	}
	s.announce(ln)
	fail(s.serve(ln).Error())
}

// listenRouter: na Windows z SO_EXCLUSIVEADDRUSE (listenControl), da drug proces ne prevzame vrat.
func listenRouter(c serveConfig) (net.Listener, error) {
	return (&net.ListenConfig{Control: listenControl}).Listen(context.Background(), "tcp", net.JoinHostPort(c.Address, strconv.Itoa(c.Port)))
}

// newServeConfig: nov žeton in ključ routerja (serve init, prvi zagon pladnja).
func newServeConfig() serveConfig {
	k, _ := ecdh.X25519().GenerateKey(rand.Reader)
	t := make([]byte, 24)
	rand.Read(t)
	return serveConfig{Token: "mgw-" + base64url(t), Key: base64url(k.Bytes()), Port: 8765, Tools: defaultTools,
		PermissionMode: "dontAsk", Devices: []serveDevice{}}
}

// announce: vrstica zagona, opozorila o ključih prehodov in odkrivanje modelov v ozadju.
func (s *server) announce(ln net.Listener) {
	c := s.conf()
	off := ""
	if r := s.claudeInfo().Off; r != "" {
		off = " (Claude CLI and Codex off: " + r + ")"
	}
	agents := ""
	for _, k := range []agentKind{codexKind, grokKind, copilotKind} {
		if p, _ := resolveAgent(k, k.conf(c).Executable); p != "" {
			agents += fmt.Sprintf(" %s=%q", k.name, p)
		}
	}
	if envSource != "" {
		agents += fmt.Sprintf(" env=%q", envSource)
	}
	s.log.Printf("minigw %s serving on %s (MGW2), claude=%q%s folders=%q%s scratch=%q devices=%d providers=%d", version, ln.Addr(),
		strings.Join(s.claudeExe(), " "), agents, strings.Join(c.AllowedFolders, ","), off, c.Scratch, len(c.Devices), len(c.Gateways))
	for _, g := range c.Gateways {
		if key, where := keyGet(s.cfgDir, g.Name); key == "" && !g.Disabled { // izklopljen (npr. vgrajen brez ključa): brez opozorila
			s.log.Printf("WARNING: provider %s has no API key: run minigw gw provider add %s %s", g.Name, g.Name, g.BaseURL)
		} else if strings.HasSuffix(where, ".json") && runtime.GOOS == "linux" {
			s.log.Printf("WARNING: gateway %s: no Secret Service (secret-tool), the API key is in %s (mode 0600)", g.Name, where)
		}
	}
	go s.modelList() // odkrivanje modelov prehodov v ozadju
}

// validDeviceID: ID, kot ga izpiše zavrnitev (base64url 32 bajtov javnega ključa, 43 znakov).
func validDeviceID(id string) bool {
	b, err := fromBase64url(id)
	return err == nil && len(b) == 32 && base64url(b) == id
}

// localIP: prvi naslov IPv4, ki ni loopback (za vrstico minigw connect).
func localIP() string {
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok && !n.IP.IsLoopback() && n.IP.To4() != nil {
			return n.IP.String()
		}
	}
	return "<this-computer>"
}

type server struct {
	mu      sync.RWMutex // cfg, claude, pending: spremembe prek skrbniškega API med delovanjem
	cfg     serveConfig
	cfgPath string
	cfgDir  string      // mapa nastavitev (žeton, ključ routerja): agentu prepovedana
	claude  []string    // claude.exe ali node + cli.js; prazno = ni nameščen
	pending []apiDevice // nedavno zavrnjene naprave (največ 20)
	state   *gwState    // zadnji stik in priklopi naprav, seje (state.json; serve_history.go)
	admin   string      // skrivnost skrbniškega API (admin.secret)
	offline bool        // minigw router brez tekočega routerja: le serve.json
	// offReason: zakaj sta Claude CLI in Codex ob zagonu izklopljena (H1: dovoljena mapa s Scratch); dnevnik, status, stran.
	offReason  string
	offFolders []string // dovoljene mape iz serve.json, izpuščene ob zagonu (stran jih pokaže, da jih uporabnik odstrani)
	settings   settingsState
	pick      atomic.Pointer[pickFunc] // izbirno okno pladnja (Windows); nil = ni GUI
	picking   atomic.Bool              // eno okno naenkrat
	// agentsRunning: tekoči procesi agentov (claude, codex, grok), za status in posodobitev brez prekinitve
	agentsRunning atomic.Int32
	foundry       foundryState // Foundry Local: vrata, modeli
	key           *ecdh.PrivateKey
	log           *log.Logger
	hub           *shareHub
	sessions      sessionRegistry
	procs         chan struct{} // največ hkratnih procesov claude
	gwSlots       chan struct{} // največ hkratnih zahtev k prehodom
	gateways      gwCatalog
}

func newServer(c serveConfig, cfgPath string) (*server, error) {
	k, err := c.privateKey()
	if err != nil || c.Token == "" {
		return nil, errors.New("config needs token and key: run minigw serve init")
	}
	cfgDir, _ := filepath.Abs(filepath.Dir(cfgPath))
	ensureBuiltinGateways(&c)
	if c.Scratch == "" {
		if c.Scratch = defaultScratch(); c.Scratch == "" {
			return nil, errors.New("no cache folder for copies: set \"scratch\" in the config")
		}
	}
	if c.Scratch, err = filepath.Abs(c.Scratch); err != nil || under(c.Scratch, cfgDir) {
		return nil, errors.New("scratch folder: must not be inside the config folder " + cfgDir)
	}
	if err := protectScratch(c.Scratch); err != nil {
		return nil, fmt.Errorf("scratch folder %s: %v: use a folder of your own", c.Scratch, err)
	}
	for i, g := range c.Gateways { // ročno urejen serve.json: ista pravila kot gateway add
		if !validGatewayName.MatchString(g.Name) {
			return nil, fmt.Errorf("provider name %q in the config: letters, digits, . _ - (up to 32)", g.Name)
		}
		if c.Gateways[i].BaseURL, err = checkGatewayURL(g.BaseURL, g.Insecure); err != nil {
			return nil, fmt.Errorf("provider %s in the config: %v", g.Name, err)
		}
	}
	protectErr := protectConfigDir(cfgDir)
	if errors.Is(protectErr, errForeignOwner) {
		return nil, fmt.Errorf("the config folder %s %v: use a folder of your own", cfgDir, protectErr)
	}
	if !validAddress(c.Address) {
		return nil, errors.New("address " + c.Address + ": use empty (all), 0.0.0.0, :: or 127.0.0.1, so that minigw gw and the admin API reach the gateway on 127.0.0.1; limit clients with a firewall")
	}
	// H1: dovoljena mapa, ki vsebuje Scratch (ali je v njem), bi agentom pokazala zrcala in kopije drugih naprav:
	// ob zagonu (ročno urejen serve.json) take mape ne veljajo, Claude CLI in Codex sta izklopljena, opozorilo v dnevniku.
	var offReason string
	bad := append(scratchOverlaps(c.AllowedFolders, c.Scratch), copilotCacheOverlaps(c.AllowedFolders)...)
	slices.Sort(bad)
	if bad = slices.Compact(bad); len(bad) > 0 { // mapa je lahko v obeh
		offReason = "the allowed folder(s) " + strings.Join(bad, ", ") + " contain or are inside the scratch folder " + c.Scratch +
			" or Copilot's package cache " + copilotCacheDir() + ": they are left out; save the allowed folders without them (settings page → Claude CLI → Allowed folders, or minigw gw claude --folders …), then turn Claude CLI and Codex on"
		c.AllowedFolders = slices.DeleteFunc(slices.Clone(c.AllowedFolders), func(f string) bool { return slices.Contains(bad, f) })
		c.ClaudeDisabled, c.Codex.Enabled = true, false
	}
	exe, err := checkAgent(&c, cfgDir)
	if err != nil {
		return nil, err
	}
	secret, err := adminSecret(cfgDir)
	if err != nil {
		return nil, err
	}
	out := logOut
	if c.Log != "" {
		f, err := os.OpenFile(c.Log, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, err
		}
		out = f
	}
	s := &server{cfg: c, cfgPath: cfgPath, cfgDir: cfgDir, claude: exe, admin: secret, key: k, log: log.New(out, "", log.LstdFlags), hub: newShareHub(),
		sessions: sessionRegistry{m: map[string]string{}}, procs: make(chan struct{}, 6),
		gwSlots: make(chan struct{}, 16), state: loadState(filepath.Join(cfgDir, "state.json"))}
	s.state.prune(c.Devices)
	switch {
	case runtime.GOOS == "windows" && len(unsandboxedTools(c.Tools)) > 0:
		s.log.Printf("WARNING: %s allowed without a sandbox: agents can run any command as this user and read the provider API keys in Credential Manager and the admin secret",
			strings.Join(unsandboxedTools(c.Tools), ", "))
	case runtime.GOOS == "windows" || !hasBash(c.Tools):
	default:
		s.log.Printf("WARNING: Bash is allowed; it runs only in Claude Code's sandbox (on Linux it needs bubblewrap and socat)")
	}
	s.hub.logf = s.log.Printf
	if s.offReason = offReason; offReason != "" {
		s.offFolders = bad
		s.log.Print("WARNING: Claude CLI and Codex are off: " + offReason)
	}
	if protectErr != nil {
		s.log.Printf("WARNING: cannot make %s private to this user (%v): other users of this computer may read the token, the gateway key and the admin secret; use the default config folder", cfgDir, protectErr)
	}
	if len(exe) == 0 {
		s.log.Printf("WARNING: claude not found (install Claude Code or set \"claude\" in the config); no models")
	}
	return s, nil
}

// logOut: dnevnik routerja brez »log« v nastavitvah; pladenj ga nastavi na svojo datoteko pred newServer, da gredo
// tja tudi opozorila ob zagonu (konzole nima).
var logOut io.Writer = os.Stderr

// defaultScratch: kopije map in pogovori v mapi predpomnilnika uporabnika.
func defaultScratch() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "minigw", "scratch")
}

// deprecatedFolder: zastareli --folder (minigw serve, serve init) doda dovoljeno mapo; skripte delujejo naprej.
func deprecatedFolder(c *serveConfig, f string) {
	if f == "" {
		return
	}
	abs, _ := filepath.Abs(f)
	if !slices.Contains(c.AllowedFolders, abs) {
		c.AllowedFolders = append(c.AllowedFolders, abs)
	}
	say("--folder is deprecated: " + abs + " is now an allowed folder (use --folders)")
}

// checkAgent: mapa ne sme biti blizu mape nastavitev, claude mora obstajati, Bash na Windows le z dovoljenjem.
// Za zagon in za spremembe prek skrbniškega API.
func checkAgent(c *serveConfig, cfgDir string) ([]string, error) {
	for i, f := range c.AllowedFolders {
		if f == "" || !filepath.IsAbs(f) {
			return nil, errors.New("allowed folder must be an absolute path: " + f)
		}
		real, err := checkFolder(f, cfgDir)
		if err != nil {
			return nil, err
		}
		c.AllowedFolders[i] = real
	}
	if bad := scratchOverlaps(c.AllowedFolders, c.Scratch); len(bad) > 0 { // H1: PATCH, minigw gw claude --folders, serve.json
		return nil, errors.New("allowed folder " + bad[0] + " contains or is inside the scratch folder " + c.Scratch +
			" (agents could change the gateway's mirrors and other devices' copies): choose another folder")
	}
	if bad := copilotCacheOverlaps(c.AllowedFolders); len(bad) > 0 { // #26
		return nil, errors.New("allowed folder " + bad[0] + " contains or is inside Copilot's package cache " + copilotCacheDir() +
			" (agents could plant code the gateway runs): choose another folder")
	}
	if c.AllowLocalTools && runtime.GOOS == "windows" && !c.AllowUnsandboxedBash {
		return nil, errors.New("local tools include commands (Bash, PowerShell), which have no sandbox on Windows: also set \"allowUnsandboxedBash\": true, or turn local tools off")
	}
	for _, t := range c.DisabledTools {
		if !validTools.MatchString(t) {
			return nil, errors.New("disabled tool: not a tool name: " + t)
		}
	}
	if !validSources.MatchString(c.SettingSources) {
		return nil, errors.New("setting sources: comma-separated user, project, local (or empty)")
	}
	if c.PermissionMode == "" {
		c.PermissionMode = "dontAsk"
	}
	if !slices.Contains([]string{"dontAsk", "acceptEdits", "auto", "plan"}, c.PermissionMode) {
		return nil, errors.New("permission mode: dontAsk, acceptEdits, auto or plan")
	}
	exe, err := resolveClaude(c.Claude)
	if err != nil {
		return nil, err
	}
	if t := unsandboxedTools(c.Tools); len(t) > 0 && runtime.GOOS == "windows" && !c.AllowUnsandboxedBash {
		return nil, errors.New("tools " + strings.Join(t, ", ") + " have no sandbox on Windows: remove them from tools, or set \"allowUnsandboxedBash\": true to let agents run any command as this user (and read the provider API keys in Credential Manager and the admin secret)")
	}
	return exe, nil
}

var validSources = regexp.MustCompile(`^((user|project|local)(,(user|project|local))*)?$`)

// checkFolder: mapa obstaja (povezave razrešene) in ni v mapi nastavitev ali okoli nje.
func checkFolder(f, cfgDir string) (string, error) {
	abs, _ := filepath.Abs(f)
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", errors.New("folder: " + err.Error())
	}
	if st, err := os.Stat(real); err != nil || !st.IsDir() {
		return "", errors.New("not a folder: " + f)
	}
	realCfg, err := filepath.EvalSymlinks(cfgDir)
	if err != nil {
		realCfg = cfgDir
	}
	for _, a := range []string{abs, real} {
		for _, d := range []string{cfgDir, realCfg} {
			if under(d, a) || under(a, d) { // agent bi dosegel žeton in ključ routerja
				return "", errors.New("folder must not contain or be inside the config folder " + cfgDir)
			}
		}
	}
	return real, nil
}

// windowsSafeTools: orodja, ki na Windows ne poganjajo ukazov (dostop do datotek omeji Claude Code na ./**).
// Vse drugo (Bash, PowerShell, neznana imena) na Windows zahteva allowUnsandboxedBash.
var windowsSafeTools = map[string]bool{"Read": true, "Grep": true, "Glob": true, "LS": true, "Edit": true, "Write": true,
	"MultiEdit": true, "NotebookEdit": true, "NotebookRead": true, "WebFetch": true, "WebSearch": true, "TodoWrite": true}

func unsandboxedTools(tools string) []string {
	var out []string
	for _, t := range strings.Split(tools, ",") {
		if t = strings.TrimSpace(t); t != "" && !windowsSafeTools[t] {
			out = append(out, t)
		}
	}
	return out
}

var errForeignOwner = errors.New("is owned by another user")

// protectConfigDir: na Windows mapa nastavitev zunaj %AppData% dobi DACL samo za lastnika (v %AppData% je že
// zasebna). Drugod je mapa 0700.
func protectConfigDir(cfgDir string) error {
	appData, err := configBase()
	if runtime.GOOS != "windows" || (err == nil && under(cfgDir, appData)) {
		return nil
	}
	return protectDir(cfgDir)
}

// validAddress: naslov, na katerem router posluša tudi na 127.0.0.1 (skrbniški API in minigw router ga potrebujeta).
func validAddress(a string) bool {
	return a == "" || a == "0.0.0.0" || a == "::" || a == "127.0.0.1"
}

// conf: posnetek nastavitev; rezine se ne spreminjajo na mestu (update jih kopira).
func (s *server) conf() serveConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

func (s *server) claudeExe() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.claude
}

// update: sprememba velja takoj in se shrani. f teče dvakrat (na serve.json in na tekoče nastavitve, ki imajo še
// zastavice ukazne vrstice in privzete vrednosti), zato brez stranskih učinkov.
func (s *server) update(f func(*serveConfig) error, commit ...func()) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	clone := func(c serveConfig) serveConfig {
		c.Devices = slices.Clone(c.Devices)
		c.Gateways = slices.Clone(c.Gateways)
		for i := range c.Gateways {
			c.Gateways[i].Models = slices.Clone(c.Gateways[i].Models) // nil ostane nil (vsi modeli)
		}
		return c
	}
	live := clone(s.cfg)
	if err := f(&live); err != nil {
		return err
	}
	file, err := loadServeConfig(s.cfgPath)
	if err != nil {
		return err
	}
	if file = clone(file); f(&file) != nil {
		return errors.New("serve.json differs from the running gateway: restart minigw serve")
	}
	if err := writeServeConfig(s.cfgPath, file); err != nil {
		return err
	}
	s.cfg = live
	if s.state != nil {
		s.state.prune(live.Devices) // odstranjena naprava: brez zgodovine priklopov, seje označene
	}
	for _, c := range commit { // pod s.mu, šele ko sta oba prehoda uspela
		c()
	}
	return nil
}

func (s *server) serve(ln net.Listener) error {
	// Brez ReadTimeout/WriteTimeout: prekinila bi dolge odgovore agenta (telo ima rok v handle).
	srv := &http.Server{Handler: s, ReadHeaderTimeout: 30 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 64 << 10,
		ErrorLog: log.New(io.Discard, "", 0),
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			return context.WithValue(ctx, connKey{}, c.(*serveConn))
		}}
	srv.SetKeepAlivesEnabled(false) // ena zahteva na povezavo, kot router na macOS
	l := &serveListener{Listener: ln, conns: make(chan net.Conn), done: make(chan struct{})}
	go s.acceptLoop(l)
	go s.state.saveEvery(time.Minute)
	go func() { // konec (systemd, Ctrl+C): zgodovina na disk
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, os.Interrupt)
		<-sig
		s.state.save()
		os.Exit(0)
	}()
	return srv.Serve(l)
}

// acceptLoop: napaka sprejema (npr. preveč odprtih datotek) → počakaj in poskusi znova; konec le ob zaprtem poslušanju.
// Največ 256 povezav hkrati v rokovanju in 1024 skupaj; ostale se takoj zaprejo.
func (s *server) acceptLoop(l *serveListener) {
	defer close(l.done)
	sem, total := make(chan struct{}, 256), make(chan struct{}, 1024)
	delay := 5 * time.Millisecond
	for {
		raw, err := l.Listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			time.Sleep(delay)
			delay = min(2*delay, time.Second)
			continue
		}
		delay = 5 * time.Millisecond
		select {
		case total <- struct{}{}:
		default:
			raw.Close()
			continue
		}
		select {
		case sem <- struct{}{}:
		default:
			raw.Close()
			<-total
			continue
		}
		go func() {
			c := s.accept(raw)
			<-sem
			if c == nil {
				<-total
				return
			}
			c.release = func() { <-total }
			select {
			case l.conns <- c:
			case <-l.done:
				c.Close()
			}
		}()
	}
}

// serveListener: http.Server dobi le povezave po rokovanju (in dokazu naprave).
type serveListener struct {
	net.Listener
	conns chan net.Conn
	done  chan struct{} // zaprt, ko se acceptLoop konča
}

func (l *serveListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.conns:
		return c, nil
	case <-l.done:
		return nil, net.ErrClosed
	}
}

type connKey struct{}

// serveConn: navaden HTTP (loopback) ali okvirji Noise; device je iz dokaza naprave (nil: starejši minigw ali navaden HTTP).
type serveConn struct {
	net.Conn
	br      *bufio.Reader
	sess    *noiseSession
	pending []byte // dešifrirano, še ne prebrano
	wmu     sync.Mutex
	peer    string
	device  *serveDevice
	release func() // sprosti mesto v skupni omejitvi povezav
	once    sync.Once
}

func (c *serveConn) Close() error {
	c.once.Do(func() {
		if c.release != nil {
			c.release()
		}
	})
	return c.Conn.Close()
}

func (c *serveConn) loopback() bool { ip := net.ParseIP(c.peer); return ip != nil && ip.IsLoopback() }

func (c *serveConn) readFrame() error {
	var n [2]byte
	if _, err := io.ReadFull(c.br, n[:]); err != nil {
		return err
	}
	ct := make([]byte, int(n[0])<<8|int(n[1]))
	if _, err := io.ReadFull(c.br, ct); err != nil {
		return err
	}
	p, err := c.sess.receive.open(ct, nil)
	if err != nil {
		return err
	}
	c.pending = append(c.pending, p...)
	return nil
}

func (c *serveConn) Read(p []byte) (int, error) {
	if c.sess == nil {
		return c.br.Read(p)
	}
	for len(c.pending) == 0 {
		if err := c.readFrame(); err != nil {
			return 0, err
		}
	}
	n := copy(p, c.pending)
	c.pending = c.pending[n:]
	return n, nil
}

func (c *serveConn) Write(p []byte) (int, error) {
	if c.sess == nil {
		return c.Conn.Write(p)
	}
	c.wmu.Lock() // šifriranje + pošiljanje skupaj: vrstni red okvirjev ostane
	defer c.wmu.Unlock()
	if _, err := c.Conn.Write(frames(p, c.sess.send)); err != nil {
		return 0, err
	}
	return len(p), nil
}

// accept: prvi bajti »MGW2« = Noise, sicer navaden HTTP le z loopback. nil = povezava zavrnjena in zaprta.
func (s *server) accept(raw net.Conn) *serveConn {
	host, _, _ := net.SplitHostPort(raw.RemoteAddr().String())
	c := &serveConn{Conn: raw, br: bufio.NewReaderSize(raw, 64<<10), peer: host}
	if !c.loopback() && !s.conf().clientAllowed(host) { // dovoljeni odjemalci (IP); loopback vedno
		raw.Close()
		return nil
	}
	raw.SetDeadline(time.Now().Add(10 * time.Second)) // zastalo rokovanje
	head, err := c.br.Peek(4)
	switch {
	case err != nil:
	case bytes.Equal(head, noiseMagic):
		if c.handshake(s.key) == nil {
			raw.SetDeadline(time.Time{})
			return c
		}
	case string(head) == "MGW1":
		s.log.Printf("%s refused: ChaCha20 (MGW1) connection; this gateway supports MGW2 (AES-GCM) only: update minigw", strconv.Quote(host))
	case !c.loopback() && s.conf().AllowPlainRemote: // izrecno vklopljeno: brez šifriranja in brez ključa naprave
		raw.SetDeadline(time.Time{})
		return c
	case !c.loopback():
		body := `{"type":"error","error":{"type":"permission_error","message":"Unencrypted connections from other computers are off: use minigw connect with the gateway key"}}`
		fmt.Fprintf(raw, "HTTP/1.1 426 Upgrade Required\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(body), body)
	default:
		raw.SetDeadline(time.Time{})
		return c
	}
	raw.Close()
	return nil
}

func (c *serveConn) handshake(key *ecdh.PrivateKey) error {
	msg := make([]byte, 4+handshakeLen)
	if _, err := io.ReadFull(c.br, msg); err != nil {
		return err
	}
	reply, sess, err := respond(msg[4:], key)
	if err != nil {
		return err // napačen ključ routerja ali poškodovano sporočilo
	}
	if _, err := c.Conn.Write(reply); err != nil {
		return err
	}
	c.sess = sess
	return c.readDevice()
}

// readDevice: »DEV1« + javni ključ (32) + podpis (64) + dolžina imena (1) + ime na začetku podatkov (kot Noise.readDevice).
// Brez »DEV1« (starejši minigw) podatki ostanejo za HTTP; ponarejen podpis → napaka.
func (c *serveConn) readDevice() error {
	need := func(n int) error {
		for len(c.pending) < n {
			if err := c.readFrame(); err != nil {
				return err
			}
		}
		return nil
	}
	if err := need(1); err != nil {
		return err
	}
	for i := 0; i < len(deviceMagic); i++ {
		if err := need(i + 1); err != nil {
			return err
		}
		if c.pending[i] != deviceMagic[i] {
			return nil
		}
	}
	fixed := len(deviceMagic) + 32 + 64 + 1
	if err := need(fixed); err != nil {
		return err
	}
	n := int(c.pending[fixed-1])
	if err := need(fixed + n); err != nil {
		return err
	}
	pub, sig, name := c.pending[4:36], c.pending[36:100], c.pending[fixed:fixed+n]
	msg := append(append([]byte("minigw-device"), c.sess.hash...), name...)
	if !ed25519.Verify(ed25519.PublicKey(pub), msg, sig) {
		return errors.New("invalid device proof")
	}
	c.device = &serveDevice{ID: base64url(pub), Name: cleanDeviceName(string(name))}
	c.pending = c.pending[fixed+n:]
	return nil
}

// --- HTTP ---

type req struct {
	*http.Request
	conn *serveConn
	body []byte
	rid  string
}

func (r *req) h(name string) string { return r.Header.Get(name) }

// owner: lastnik map te zahteve: naprava, »local« za loopback brez naprave, sicer "" (do map ne pride).
func (r *req) owner() string {
	switch {
	case r.conn.device != nil:
		return r.conn.device.ID
	case r.conn.loopback():
		return "local"
	}
	return ""
}

func (s *server) ServeHTTP(w http.ResponseWriter, hr *http.Request) {
	start := time.Now()
	b := make([]byte, 16)
	rand.Read(b)
	r := &req{Request: hr, conn: hr.Context().Value(connKey{}).(*serveConn), rid: fmt.Sprintf("req_%x", b)}
	var uw *usageWriter
	if hr.Method == "POST" && hr.URL.Path == "/v1/messages" {
		uw = &usageWriter{ResponseWriter: w}
		w = uw
	}
	status, model := s.handle(w, r)
	if uw != nil && model != "" { // le zahteva, ki je prišla do modela (overjena, dovoljena naprava)
		s.recordSession(r, model, status, time.Since(start), uw.in, uw.out)
	}
	if hr.URL.Path == "/minigw/share/next" && status == 204 {
		return // long poll vsakih 25 s: brez vrstice
	}
	who := r.conn.peer
	if d := r.conn.device; d != nil {
		who += " " + strconv.Quote(d.Name)
	}
	if p := runOf(r); p != nil { // L3: vprašal ga je agent tega prehoda
		who += " via=" + p.via()
	}
	if model == "" {
		model = "-"
	}
	s.log.Printf("%s %s %s %s model=%s status=%d %dms bytes=%d", r.rid, who, strconv.Quote(hr.Method), strconv.Quote(hr.URL.Path),
		strconv.Quote(model), status, time.Since(start).Milliseconds(), len(r.body))
}

// bigBodies: zahteve s telesom do 64 MB (kot router na macOS); ostale do 1 MB.
var bigBodies = map[string]bool{"PUT /minigw/sync/file": true, "PUT /minigw/worktree/bundle": true, "POST /v1/messages": true, "POST /v1/messages/count_tokens": true}

// handle: overitev in naprava pred branjem telesa; telo z rokom (brez ReadTimeout, ki bi prekinil dolg odgovor).
func (s *server) handle(w http.ResponseWriter, r *req) (int, string) {
	switch r.Method + " " + r.URL.Path {
	case "GET /health/live":
		return writeJSON(w, r.rid, 200, map[string]string{"status": "ok"}), ""
	case "GET /health/ready":
		if !r.conn.loopback() && !s.authorized(r) { // L2: imena ponudnikov le temu računalniku ali z žetonom
			return writeJSON(w, r.rid, 200, map[string]any{"status": "ok"}), ""
		}
		providers := map[string]bool{"claude-cli": len(s.claudeExe()) > 0}
		for _, g := range s.conf().Gateways { // vklopljeni ponudniki: modeli naloženi (kot catalog.status na Macu)
			if !g.Disabled {
				providers[g.Name] = s.gateways.ready(g.Name)
			}
		}
		return writeJSON(w, r.rid, 200, map[string]any{"status": "ok", "providers": providers}), ""
	}
	if r.URL.Path == adminPrefix || strings.HasPrefix(r.URL.Path, adminPrefix+"/") {
		return s.adminHTTP(w, r), ""
	}
	if r.URL.Path == "/minigw/settings" || r.URL.Path == "/minigw/settings/login" || r.URL.Path == "/minigw/settings/logout" ||
		r.URL.Path == "/minigw/settings/pick" {
		return s.settingsHTTP(w, r), ""
	}
	if r.h(agentChainHeader) != "" && runOf(r) == nil { // veriga agentov je le v prehodu; od zunaj je ponarejena
		return writeError(w, r.rid, 400, "The "+agentChainHeader+" header is not accepted"), ""
	}
	if !s.authorized(r) && !s.localApp(r) {
		return writeError(w, r.rid, 401, "Invalid or missing token"), ""
	}
	if !s.conf().deviceAllowed(r.conn.device, r.conn.loopback()) {
		if d := r.conn.device; d != nil {
			s.addPending(*d, r.conn.peer, cleanDeviceName(r.Header.Get("X-Minigw-Client")))
			s.log.Printf("%s refused unknown device %q %s; compare the ID with the client, then to allow it run: minigw gw devices allow %s", r.rid, d.Name, d.ID, d.ID)
			return writeError(w, r.rid, 403, deviceRefusal(*d)), ""
		}
		return writeError(w, r.rid, 403, "This gateway needs a device key: update minigw and run minigw connect again"), ""
	}
	// Zgodovina dovoljene naprave tudi prek loopbacka (profil 127.0.0.1); neznana na loopbacku nima zgodovine.
	if d := r.conn.device; d != nil && (!r.conn.loopback() || s.conf().listed(d.ID)) {
		s.deviceSeen(*d, r.conn.peer, cleanDeviceName(r.Header.Get("X-Minigw-Client")))
	}
	limit := int64(1 << 20)
	if bigBodies[r.Method+" "+r.URL.Path] {
		limit = maxSyncFile
	}
	if !readBody(w, r, limit) {
		return writeError(w, r.rid, 413, "Request body too large or too slow"), ""
	}
	return s.route(w, r)
}

func readBody(w http.ResponseWriter, r *req, limit int64) bool {
	rc := http.NewResponseController(w)
	rc.SetReadDeadline(time.Now().Add(120 * time.Second))
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	rc.SetReadDeadline(time.Time{})
	r.body = body
	return err == nil
}

func writeJSON(w http.ResponseWriter, rid string, status int, obj any) int {
	data, _ := json.Marshal(obj)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("request-id", rid)
	w.WriteHeader(status)
	w.Write(data)
	return status
}

func writeError(w http.ResponseWriter, rid string, status int, msg string) int {
	return writeJSON(w, rid, status, errorBody(status, msg))
}

func errorBody(status int, msg string) map[string]any {
	t := map[int]string{401: "authentication_error", 403: "permission_error", 404: "not_found_error", 413: "request_too_large",
		429: "rate_limit_error", 529: "overloaded_error"}[status]
	if t == "" {
		t = "invalid_request_error"
		if status >= 500 {
			t = "api_error"
		}
	}
	return map[string]any{"type": "error", "error": map[string]string{"type": t, "message": msg}}
}

func writeStatus(w http.ResponseWriter, status int, ctype string, body []byte) int {
	if ctype != "" {
		w.Header().Set("Content-Type", ctype)
	}
	w.WriteHeader(status)
	w.Write(body)
	return status
}

// localApps: »Allow this user's apps without a token« (privzeto izklopljeno).
func (c serveConfig) localApps() bool { return c.AllowLocalApps != nil && *c.AllowLocalApps }

// localPeer: odjemalec na loopbacku je proces tega uporabnika in ni agent prehoda (peer_*.go); testi ga zamenjajo.
var localPeer = sameUserPeer

// localApp: zahteva za modele brez žetona od aplikacije tega uporabnika na tem računalniku. Brskalnik je ne more poslati:
// glava anthropic-version ali x-api-key zahteva CORS preflight (prehod ne pošlje glav CORS), Host je loopback (ne DNS
// rebinding), brez Origin (stran z istim izvorom, npr. nastavitve). Drugi uporabniki in oddaljeni odjemalci: žeton.
func (s *server) localApp(r *req) bool {
	switch r.Method + " " + r.URL.Path {
	case "GET /v1/models", "POST /v1/messages", "POST /v1/messages/count_tokens":
	default:
		return false
	}
	host := r.Host
	if h, _, err := net.SplitHostPort(r.Host); err == nil {
		host = h
	}
	host = strings.Trim(host, "[]")
	if !s.conf().localApps() || !r.conn.loopback() || r.h("origin") != "" || (r.h("anthropic-version") == "" && r.h("x-api-key") == "") ||
		(host != "localhost" && !(net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback())) {
		return false
	}
	ours, ok1 := r.conn.LocalAddr().(*net.TCPAddr)
	peer, ok2 := r.conn.RemoteAddr().(*net.TCPAddr)
	return ok1 && ok2 && localPeer(ours, peer)
}

func (s *server) authorized(r *req) bool {
	given := strings.TrimPrefix(r.h("authorization"), "Bearer ")
	if r.h("authorization") == "" {
		given = r.h("x-api-key")
	}
	return given != "" && subtle.ConstantTimeCompare([]byte(given), []byte(s.conf().Token)) == 1
}

// route: po overitvi, napravi in branju telesa.
func (s *server) route(w http.ResponseWriter, r *req) (int, string) {
	path := r.URL.Path
	dev := r.owner()
	key := r.h("x-share-key")
	switch r.Method + " " + path {
	case "GET /v1/models":
		return writeJSON(w, r.rid, 200, s.modelList()), ""
	case "POST /v1/messages", "POST /v1/messages/count_tokens":
		return s.messages(w, r)

	case "POST /minigw/share":
		session, cwd := r.h("x-claude-mini-session"), r.h("x-claude-mini-cwd")
		if session == "" || cwd == "" {
			return writeError(w, r.rid, 400, "Missing session or cwd"), ""
		}
		work := r.h("x-claude-mini-work")
		if work != "sync" && work != "worktree" { // brez priklopa WebDAV in mape SMB na Windows in Linux
			s.hub.refuse(session)
			return writeError(w, r.rid, 400, refusedDirect), ""
		}
		owner := dev
		if owner == "" {
			return writeError(w, r.rid, 400, "Sync needs an encrypted connection with a device key (run minigw connect again)"), ""
		}
		if work == "worktree" { // golo zrcalo v scratch/repos/<naprava>/<repo>.git, kloni v scratch/worktrees (serve_worktree.go)
			if r.h("x-share-mode") == "ro" {
				return writeError(w, r.rid, 400, "Worktree mode needs a writable git repository that contains the folder"), ""
			}
			if _, err := gitExe(); err != nil {
				return writeError(w, r.rid, 400, "Worktree mode: git is not installed on the gateway"), ""
			}
			t, err := worktreeTree(s.conf().Scratch, owner, r.h("x-worktree-top"), cwd)
			if err != nil {
				return writeError(w, r.rid, 400, err.Error()), ""
			}
			if tooManyCopies(t.worktrees) {
				return writeError(w, r.rid, 429, fmt.Sprintf("This device already has %d repositories on the gateway; unused ones expire after 30 days", maxCopies)), ""
			}
			k, ok := s.hub.registerTree(session, cwd, key, t, dev)
			if !ok {
				return writeError(w, r.rid, 409, "Session already has a folder"), ""
			}
			return writeJSON(w, r.rid, 200, map[string]string{"key": k}), ""
		}
		ro := r.h("x-share-mode") == "ro"
		suffix := ""
		if ro {
			suffix = "-ro" // samo za branje: ločena kopija
		}
		copyDir := filepath.Join(s.conf().scratch("sync"), owner, sha256Hex([]byte(cwd))[:16]+suffix)
		if tooManyCopies(copyDir) {
			return writeError(w, r.rid, 429, fmt.Sprintf("This device already has %d synced folders on the gateway; unused ones expire after 14 days", maxCopies)), ""
		}
		k, ok := s.hub.register(session, cwd, key, ro, copyDir, dev)
		if !ok {
			return writeError(w, r.rid, 409, "Session already has a folder"), ""
		}
		return writeJSON(w, r.rid, 200, map[string]string{"key": k}), ""
	case "DELETE /minigw/share":
		s.hub.unregister(key, dev)
		return writeStatus(w, 204, "", nil), ""
	case "GET /minigw/share/next":
		job, ok := s.hub.poll(r.Context(), key, dev)
		if !ok {
			return writeError(w, r.rid, 404, "No such share"), ""
		}
		if job == nil {
			return writeStatus(w, 204, "", nil), ""
		}
		return writeJSON(w, r.rid, 200, job), ""
	case "POST /minigw/share/reply":
		var rep davReply
		if json.Unmarshal(r.body, &rep) != nil {
			return writeError(w, r.rid, 400, "Bad reply"), ""
		}
		if !s.hub.reply(key, dev, rep) {
			return writeStatus(w, 404, "", nil), ""
		}
		return writeStatus(w, 204, "", nil), ""
	case "POST /minigw/sync/manifest":
		var files []syncEntry
		if json.Unmarshal(r.body, &files) != nil {
			return writeError(w, r.rid, 400, "Bad manifest"), ""
		}
		if why := syncOverQuota(files); why != "" { // #1, #9: odjemalec ne napolni diska
			return writeError(w, r.rid, 413, why), ""
		}
		s.hub.expireCopies(s.conf().scratch("sync"), time.Now())
		need, kept, ok := s.hub.syncManifest(key, files, r.h("x-sync-exec") != "none", dev)
		if !ok {
			return writeError(w, r.rid, 404, "No such share"), ""
		}
		return writeJSON(w, r.rid, 200, map[string][]string{"need": need, "kept": kept}), ""
	case "PUT /minigw/worktree/bundle":
		part, err := strconv.Atoi(r.h("x-part"))
		if err != nil {
			part = -1
		}
		return writeStatus(w, s.hub.worktreePut(key, part, r.body, dev), "", nil), ""
	case "GET /minigw/worktree/result":
		data, st, why := s.hub.worktreeResult(key, dev)
		if st != 200 {
			return writeError(w, r.rid, st, why), ""
		}
		return writeStatus(w, 200, "application/octet-stream", data), ""
	case "PUT /minigw/sync/file":
		p, _ := url.PathUnescape(r.h("x-sync-path"))
		return writeStatus(w, s.hub.syncPut(key, p, r.body, dev), "", nil), ""
	case "GET /minigw/sync/changes":
		ch, ok := s.hub.syncChanges(key, dev)
		if !ok {
			return writeError(w, r.rid, 404, "No such share"), ""
		}
		return writeJSON(w, r.rid, 200, ch), ""
	case "GET /minigw/sync/file":
		p, _ := url.PathUnescape(r.h("x-sync-path"))
		data, ok := s.hub.syncGet(key, p, dev)
		if !ok {
			return writeError(w, r.rid, 404, "No such file"), ""
		}
		return writeStatus(w, 200, "application/octet-stream", data), ""
	case "POST /minigw/sync/ack":
		var paths []string
		if json.Unmarshal(r.body, &paths) != nil || !s.hub.syncAck(key, paths, dev) {
			return writeError(w, r.rid, 404, "No such share"), ""
		}
		return writeStatus(w, 204, "", nil), ""
	}
	return writeError(w, r.rid, 404, "Not found: "+r.Method+" "+path), ""
}

const refusedDirect = "this gateway supports --minigw-work=sync and worktree (no folder mount or native mode on Windows or Linux): run minigw with MINIGW_WORK=sync, worktree or auto"
