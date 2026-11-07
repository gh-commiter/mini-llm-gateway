// minigw — zažene Claude Code prek Mini Gateway (odjemalec za Windows in Linux; isti protokol kot macOS minigw).
// Ena izvršljiva datoteka, le standardna knjižnica Go.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
	"unicode"
)

const version = "0.5.17" // bump-version.sh

type connection struct {
	URL    string  `json:"url"`
	Token  string  `json:"token"`
	Key    *string `json:"key,omitempty"`    // javni ključ routerja (Noise); niz »Copy Token« = <žeton>#<ključ>
	Device *string `json:"device,omitempty"` // zasebni ključ te naprave (Ed25519, base64url); router po njem dovoli Siri
}

func (c connection) isLocal() bool {
	u, _ := url.Parse(c.URL)
	h := ""
	if u != nil {
		h = u.Hostname()
	}
	return h == "127.0.0.1" || h == "localhost" || h == "::1"
}

func say(s string) { fmt.Println(s) }

// clean: besedilo routerja brez kontrolnih znakov (razen \n), da ne krmili terminala.
func clean(s string) string {
	return strings.Map(func(r rune) rune {
		if r != '\n' && (r < 0x20 || r >= 0x7f && r <= 0x9f || unicode.Is(unicode.Cf, r)) { // tudi bidi, ničelna širina
			return -1
		}
		return r
	}, s)
}

func fail(s string, code ...int) {
	fmt.Fprintln(os.Stderr, "minigw: "+s)
	if len(code) > 0 {
		os.Exit(code[0])
	}
	os.Exit(1)
}

func loadConnection() *connection {
	data, err := storeLoad()
	if err != nil {
		return nil
	}
	var c connection
	if json.Unmarshal(data, &c) != nil {
		return nil
	}
	return &c
}

func saveConnection(c connection) {
	data, _ := json.Marshal(c)
	if err := storeSave(data); err != nil {
		fail("cannot save the connection to " + storeName + ": " + err.Error())
	}
}

func requireConnection() connection {
	c := loadConnection()
	if c == nil {
		fail("not connected; run: minigw connect <host[:port]> --token '<token>'")
	}
	return *c
}

// normalize: »192.168.1.20«, »192.168.1.20:8765«, »mini.local«, »http://…« → osnovni URL.
func normalize(s string) string {
	u := s
	if !strings.Contains(u, "://") {
		u = "http://" + u
	}
	u = strings.TrimSuffix(u, "/")
	if p, err := url.Parse(u); err == nil && p.Port() == "" {
		u += ":8765"
	}
	return u
}

func newDeviceKey() string {
	seed := make([]byte, ed25519.SeedSize)
	rand.Read(seed)
	return base64url(seed)
}

// deviceName: ime za odobritev na prehodu, <uporabnik>@<ime računalnika>, da se dva uporabnika istega računalnika
// razlikujeta. Prehod ga očisti (cleanDeviceName); je le oznaka, identiteta je ključ naprave.
func deviceName() string {
	u := os.Getenv("USER")
	if cur, err := user.Current(); err == nil && cur.Username != "" {
		u = cur.Username
	}
	if i := strings.LastIndex(u, "\\"); i >= 0 { // Windows: DOMENA\uporabnik
		u = u[i+1:]
	}
	h := computerName()
	if h == "" {
		h, _ = os.Hostname()
	}
	if u == "" {
		return h
	}
	return u + "@" + h
}

// clientInfo: »Windows · minigw 0.5.18« za čakajočo napravo na prehodu.
func clientInfo() string {
	return map[string]string{"windows": "Windows", "darwin": "macOS", "linux": "Linux"}[runtime.GOOS] + " · minigw " + version
}

var proxy *noiseProxy

// route: oddaljen router → vse gre šifrirano prek lokalnega proxyja; lokalno navaden HTTP.
func route(c connection) connection {
	if c.isLocal() {
		return c
	}
	if c.Key == nil {
		fail("no gateway key: copy the token again (Mini Gateway → Copy Token) and run minigw connect")
	}
	key, err := fromBase64url(*c.Key)
	if err != nil || len(key) != 32 {
		fail("invalid gateway key: copy the token again (Mini Gateway → Copy Token)")
	}
	if c.Device == nil { // povezava brez ključa naprave: ustvari ga zdaj
		d := newDeviceKey()
		c.Device = &d
		saveConnection(c)
	}
	var dev *device
	if seed, err := fromBase64url(*c.Device); err == nil && len(seed) == ed25519.SeedSize {
		dev = &device{key: ed25519.NewKeyFromSeed(seed), name: deviceName()}
	}
	p, err := newNoiseProxy(c.URL, key, dev)
	if err == nil {
		err = p.start()
	}
	if err != nil {
		fail("cannot start the local encrypted proxy: " + err.Error())
	}
	proxy = p
	c.URL = p.localURL()
	return c
}

func getJSON(c connection, path string) (int, map[string]any) {
	st, data := call(c, "GET", path, nil, nil, 5*time.Second)
	var m map[string]any
	json.Unmarshal(data, &m)
	return st, m
}

func main() {
	args := os.Args[1:]
	if isTrayExe(os.Args[0]) { // minigw-tray.exe: router z ikono v območju za obvestila
		trayCommand(args)
		return
	}
	first := ""
	if len(args) > 0 {
		first = args[0]
	}
	switch first {
	case "verify-sums": // #8 (skrit): podpis SHA256SUMS za install-minigw.sh; izhod 0 = podpisano s pripetim ključem
		if len(args) != 3 {
			fail("usage: minigw verify-sums SHA256SUMS SHA256SUMS.sig")
		}
		sums, err1 := os.ReadFile(args[1])
		sig, err2 := os.ReadFile(args[2])
		if err1 != nil || err2 != nil {
			fail("cannot read SHA256SUMS or its signature")
		}
		if err := verifyReleaseSums(sums, sig); err != nil {
			fail("SHA256SUMS: " + err.Error())
		}
		say("SHA256SUMS: good signature")
		return
	case agentReaperCmd: // pomočnik za procese agentov (Linux)
		agentReaperMain(args[1:])
		return
	case "serve":
		serveCommand(args[1:])
		return
	case "gw", "router": // router: starejše ime, skrit alias
		routerCommand(args[1:], os.Stdin)
		return
	case "tray":
		trayCommand(args[1:])
		return
	case "share":
		shareCommand(args[1:])
		return
	case "run":
		os.Exit(runCommand(args[1:]))
	case "connect":
		if len(args) < 2 {
			fail("usage: minigw connect <host[:port]> [--token '<token>' | --token -]   (- reads it from stdin)")
		}
		saved := loadConnection()
		token := ""
		if saved != nil {
			token = saved.Token
			if saved.Key != nil {
				token += "#" + *saved.Key
			}
		}
		for i, a := range args {
			if a == "--token" && i+1 < len(args) {
				token = args[i+1]
			}
		}
		if token == "-" { // F3: s stdin (cev) ali s terminala brez odmeva, ne v argv
			t, err := readKey(os.Stdin, "gateway token")
			if err != nil {
				fail(err.Error())
			}
			token = t
		}
		if token == "" {
			fail("missing --token (copy it from Mini Gateway → Settings → General → Copy Token)")
		}
		parts := strings.SplitN(token, "#", 2)
		c := connection{URL: normalize(args[1]), Token: parts[0]}
		if len(parts) > 1 {
			c.Key = &parts[1]
		}
		if saved != nil && saved.Device != nil {
			c.Device = saved.Device
		} else {
			d := newDeviceKey()
			c.Device = &d
		}
		st, m := getJSON(route(c), "/v1/models")
		switch {
		case st == 200:
			saveConnection(c)
			enc := ""
			if !c.isLocal() {
				enc = " (encrypted)"
			}
			say("Connected to " + c.URL + enc)
		case st == 401:
			fail("gateway at " + c.URL + " rejected the token")
		case st == 403: // naprava še ni dovoljena: ključ naprave ostane, da ga router lahko doda
			saveConnection(c)
			e, _ := m["error"].(map[string]any)
			msg, _ := e["message"].(string)
			fail("gateway at " + c.URL + " refused this device: " + clean(msg) + "\nthen run minigw connect again")
		case st == 426:
			fail("gateway at " + c.URL + " needs an encrypted connection: copy the token again (it includes the gateway key)")
		case st == 0 && c.isLocal():
			fail("cannot reach " + c.URL)
		case st == 0:
			fail("cannot reach " + c.URL + ", or the gateway key does not match (copy the token again)")
		default:
			fail(fmt.Sprintf("gateway at %s answered HTTP %d", c.URL, st))
		}
		return

	case "disconnect":
		storeDelete()
		say("Disconnected")
		return

	case "status":
		saved := requireConnection()
		c := route(saved)
		st, ready := getJSON(c, "/health/ready")
		state := fmt.Sprintf("HTTP %d", st)
		if st == 200 {
			state = "reachable"
		} else if st == 0 {
			state = "unreachable"
		}
		if !saved.isLocal() {
			state += ", encrypted"
		}
		say("Gateway:   " + saved.URL + " (" + state + ")")
		if p, ok := ready["providers"].(map[string]any); ok {
			names := make([]string, 0, len(p))
			for k := range p {
				names = append(names, k)
			}
			sort.Strings(names)
			out := []string{}
			for _, k := range names {
				mark := "✗"
				if v, _ := p[k].(bool); v {
					mark = "✓"
				}
				out = append(out, k+" "+mark)
			}
			say("Providers: " + strings.Join(out, ", "))
		}
		ms, models := getJSON(c, "/v1/models")
		count := "–"
		if ms == 200 {
			d, _ := models["data"].([]any)
			count = fmt.Sprint(len(d))
		} else if ms == 401 {
			count = "token rejected"
		}
		say("Models:    " + count)
		if st != 200 {
			os.Exit(1)
		}
		return

	case "models":
		c := route(requireConnection())
		st, models := getJSON(c, "/v1/models")
		data, _ := models["data"].([]any)
		if st != 200 || data == nil {
			if st == 401 {
				fail("token rejected")
			}
			fail(fmt.Sprintf("cannot list models (HTTP %d)", st))
		}
		width := 0
		for _, m := range data {
			if id, _ := m.(map[string]any)["id"].(string); len(id) > width {
				width = len(id)
			}
		}
		for _, m := range data {
			mm, _ := m.(map[string]any)
			id, _ := mm["id"].(string)
			desc, _ := mm["description"].(string)
			say(fmt.Sprintf("%-*s  %s", width, id, desc))
		}
		return

	case "--minigw-help", "-h", "--help", "-v", "--version":
		if first == "-v" || first == "--version" {
			say("minigw " + version)
		} else {
			say("minigw connect <host[:port]> [--token T|-]  save gateway address and token (" + storeName + "; - = stdin)\n" +
				"minigw status | models | disconnect\n" +
				"minigw share add|remove [folder] | list    also share a system or hidden folder with the gateway\n" +
				"minigw share add --work=native|sync|direct [--ro] [folder]   default mode for that folder (flag/env wins)\n" +
				"minigw --minigw-ro [claude args]           share the current folder read-only (or MINIGW_SHARE=ro)\n" +
				"minigw --minigw-work=sync [claude args]    agents work in a synced copy on the gateway (or MINIGW_WORK=sync)\n" +
				"minigw --minigw-work=native [claude args]  folder is the gateway's SMB share: agents work in it there (or MINIGW_WORK=native)\n" +
				"minigw --minigw-work=worktree [claude args]  git repository: agents work in a clone on the gateway, results come back as branches minigw/…\n" +
				"minigw --minigw-work=auto [claude args]    gateway share → native, git repository → worktree, otherwise sync (--minigw-ro → direct)\n" +
				"minigw run [--] <command> [args …]          run a build in this folder on the gateway (SMB share, allowed in its Settings)\n" +
				"minigw serve [init|allow <id>] [--port N] [--folder DIR] [--config FILE]   run a gateway here (Windows, Linux)\n" +
				"minigw gw status|devices|provider|claude|token …  change the gateway here, live (minigw gw help)\n" +
				"minigw [claude options …]                    run Claude Code through the gateway")
		}
		if first == "--minigw-help" {
			return
		}
		os.Exit(runClaude(args, nil)) // le podatki o claude: brez routerja, seje in deljenja mape
	}

	// Claude Code prek routerja.
	// --minigw-ro (ali MINIGW_SHARE=ro): mapa samo za branje; zastavica ne gre naprej v claude.
	readOnly := os.Getenv("MINIGW_SHARE") == "ro"
	// --minigw-work=sync (ali MINIGW_WORK=sync): agenti delajo v kopiji na routerju namesto v priklopu; privzeto direct.
	// Prednost: --minigw-work= / MINIGW_WORK > način mape iz »minigw share add --work=…« > direct.
	work := os.Getenv("MINIGW_WORK")
	for i := 0; i < len(args); i++ {
		if args[i] == "--minigw-ro" || strings.HasPrefix(args[i], "--minigw-work=") {
			if args[i] == "--minigw-ro" {
				readOnly = true
			} else {
				work = strings.TrimPrefix(args[i], "--minigw-work=")
			}
			args = append(args[:i], args[i+1:]...)
			i--
		}
	}
	if work != "" && !workModes[work] { // native: mapa je prek SMB mapa routerja (native.go)
		fail("unknown --minigw-work=" + work + " (use direct, sync, native, worktree or auto)")
	}
	cwd, _ := os.Getwd()
	asked := work
	if f := lookupSharedFolder(sharedFolders(), cwd); asked == "" && f != nil {
		asked = f.Work
	}
	work, readOnly = workMode(work, lookupSharedFolder(sharedFolders(), cwd), readOnly)
	if (asked == "native" || asked == "worktree") && work != asked {
		fmt.Fprintln(os.Stderr, "minigw: "+asked+" mode cannot be read-only; sharing read-only over the network instead")
	}
	saved := requireConnection()
	c := route(saved)
	env := []string{
		"ANTHROPIC_BASE_URL=" + c.URL,
		"ANTHROPIC_AUTH_TOKEN=" + c.Token,
		"ANTHROPIC_API_KEY=",
		"CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1",
		"API_TIMEOUT_MS=1800000",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
	}

	// Seja: router po njej loči seje claude -p in niti Siri. Obstoječa ob --resume <id>, sicer nova.
	sid, own := "", false
	for i, a := range args {
		switch a {
		case "-r", "--resume": // brez ID-ja = izbirnik sej v claude; nikoli skupaj z --session-id
			own = true
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				sid = args[i+1]
			}
		case "--session-id":
			if i+1 < len(args) {
				sid = args[i+1]
			}
			own = true
		case "-c", "--continue":
			own = true
		default:
			if strings.HasPrefix(a, "--resume=") {
				own, sid = true, strings.TrimPrefix(a, "--resume=")
			}
		}
	}
	if sid == "" {
		sid = newUUID() // tudi pri -c/--continue: router po njem deli mapo
		if !own {
			args = append([]string{"--session-id", sid}, args...)
		}
	}

	u, _ := url.Parse(saved.URL)
	routerHost := ""
	if u != nil {
		routerHost = u.Hostname()
	}
	wantShare := shareWanted(cwd, saved.isLocal())
	headers := "x-claude-mini-session: " + sid + "\nx-claude-mini-cwd: " + cwd
	if wantShare {
		headers += "\nx-claude-mini-share: 1" // router po ponovnem zagonu počaka na ponovno registracijo mape
	}
	env = append(env, "ANTHROPIC_CUSTOM_HEADERS="+headers)

	// Načini po vrsti (auto: native → worktree → sync); naslednji, če router prejšnjega ne sprejme.
	top := ""
	if work == "auto" || work == "worktree" {
		top = gitToplevel(cwd)
	}
	_, _, smb := nativeDetect(cwd)
	share := &shareClient{c: c, session: sid, root: cwd, readOnly: readOnly, synced: map[string]syncEntry{}, reported: map[string]bool{}}
	if wantShare && sid != "" {
		tries := workCandidates(work, readOnly, work == "auto" && smb, top != "")
		if work == "worktree" && top == "" {
			fmt.Fprintln(os.Stderr, "minigw: "+cwd+" is not in a git repository; worktree mode is not possible")
		}
		ok := false
		for i, w := range tries {
			share = &shareClient{c: c, session: sid, root: cwd, readOnly: readOnly, sync: w == "sync", native: w == "native",
				worktree: w == "worktree", top: top, synced: map[string]syncEntry{}, reported: map[string]bool{}}
			if ok = share.register(); ok {
				break
			}
			if i+1 < len(tries) {
				fmt.Fprintln(os.Stderr, "minigw: "+w+" mode not available: "+clean(share.refusal)+"; trying "+tries[i+1])
			}
		}
		if ok && share.worktree {
			share.start(4)
			fmt.Fprintf(os.Stderr, "minigw: agent models work in a clone of this git repository on %s; their work comes back as branches minigw/… (worktree)\n", routerHost)
		} else if ok && share.native {
			share.start(4)
			fmt.Fprintf(os.Stderr, "minigw: %s is a folder of %s (SMB): agent models work in it there directly (native)\n", cwd, routerHost)
		} else if ok {
			share.start(4)
			mode := "read-write"
			if readOnly {
				mode = "read-only"
			}
			verb := "sharing"
			if share.sync {
				verb = "syncing"
			}
			fmt.Fprintf(os.Stderr, "minigw: %s %s %s with %s for agent models (--minigw-ro = read-only, MINIGW_SHARE=0 = off)\n", verb, cwd, mode, routerHost)
		} else {
			why := "gateway did not accept the folder"
			if share.refusal != "" {
				why += ": " + clean(share.refusal)
			}
			fmt.Fprintln(os.Stderr, "minigw: "+why+"; Claude CLI models with tools will not see it")
		}
	} else if !saved.isLocal() && os.Getenv("MINIGW_SHARE") != "0" {
		why := "" // samodejno se ne delijo sistemske, skrite in domača mapa
		if os.Getenv("MINIGW_SHARE") == "" {
			why = " (not shared automatically)"
		}
		fmt.Fprintf(os.Stderr, "minigw: %s is not shared with the gateway%s, so agent models (Codex, Claude CLI, Grok) cannot see it; to share it run: minigw share add (or MINIGW_SHARE=1 for this run)\n", cwd, why)
	}
	code := runClaude(args, env)
	share.unregister()
	os.Exit(code)
}

// shareWanted: mapo deli le z oddaljenim routerjem in le pod domačo mapo (nikoli cele domače mape,
// skritih map ali sistemskih podatkov); MINIGW_SHARE=0/1 preglasi.
func shareWanted(cwd string, local bool) bool {
	switch os.Getenv("MINIGW_SHARE") {
	case "1":
		return true
	case "0":
		return false
	}
	if local {
		return false
	}
	return lookupSharedFolder(sharedFolders(), cwd) != nil || autoShare(cwd)
}

// Sistemske mape (prva raven pod korenom diska, male črke): nikoli samodejno.
var systemTop = map[string]bool{
	"windows": true, "program files": true, "program files (x86)": true, "programdata": true, "users": true,
	"perflogs": true, "recovery": true, "system volume information": true,
	"system": true, "library": true, "usr": true, "bin": true, "sbin": true, "etc": true, "private": true,
	"var": true, "dev": true, "opt": true, "applications": true, "home": true, "root": true, "proc": true,
	"sys": true, "boot": true, "tmp": true, "run": true, "snap": true, "nix": true, "cores": true,
}

// Priklopi diskov: koren priklopa (/Volumes/X, /mnt/X) je kot koren diska.
var mountTop = map[string]bool{"volumes": true, "mnt": true, "media": true}

// autoShare: projektna mapa kjer koli (tudi D:\Projekti, /Volumes/Disk/projekt), razen korena diska ali priklopa,
// domače mape same, sistemskih map, domačih map drugih ter skritih map in AppData/Library v domači.
func autoShare(cwd string) bool {
	cwd = filepath.Clean(cwd)
	home, _ := os.UserHomeDir()
	if filepath.Dir(cwd) == cwd || (home != "" && strings.EqualFold(cwd, filepath.Clean(home))) {
		return false
	}
	if home != "" {
		if rel, err := filepath.Rel(home, cwd); err == nil && !strings.HasPrefix(rel, "..") {
			top := strings.ToLower(strings.SplitN(filepath.ToSlash(rel), "/", 2)[0])
			return !strings.HasPrefix(top, ".") && top != "library" && top != "appdata"
		}
	}
	parts := strings.Split(strings.Trim(filepath.ToSlash(cwd[len(filepath.VolumeName(cwd)):]), "/"), "/")
	top := strings.ToLower(parts[0])
	if strings.HasPrefix(top, ".") || strings.HasPrefix(top, "$") || systemTop[top] {
		return false
	}
	return !mountTop[top] || len(parts) >= 3
}

// runClaude: claude kot podproces (minigw ostane za proxy in mapo); Ctrl+C dobi claude.
func runClaude(args []string, env []string) int {
	signal.Ignore(os.Interrupt)
	cmd := exec.Command("claude", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(), env...)
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		fmt.Fprintln(os.Stderr, "minigw: cannot run claude: "+err.Error()+" (is Claude Code installed and in PATH?)")
		return 127
	}
	return 0
}

func newUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// Mape, ki jih uporabnik deli z ukazom »minigw share add« (npr. projekti na drugem disku), ena na vrstico.
func sharedFoldersFile() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "minigw", "shared-folders")
}

// sharedFolder: vrstica seznama; za tabulatorjem neobvezno »--work=<način> --ro« (privzeti način mape in podmap).
// Starejše datoteke (le poti) veljajo naprej; ista oblika kot minigw v aplikaciji (SharedFolders.swift).
type sharedFolder struct {
	Path     string
	Work     string // "" = privzeto
	ReadOnly bool
}

var workModes = map[string]bool{"direct": true, "sync": true, "native": true, "worktree": true, "auto": true}

func sharedFolders() []sharedFolder {
	data, _ := os.ReadFile(sharedFoldersFile())
	return parseSharedFolders(string(data))
}

func parseSharedFolders(text string) []sharedFolder {
	var out []sharedFolder
	for _, l := range strings.Split(strings.ReplaceAll(text, "\r", ""), "\n") {
		path, opts, _ := strings.Cut(l, "\t")
		if path = strings.TrimSpace(path); path == "" {
			continue
		}
		f := sharedFolder{Path: path}
		for _, o := range strings.Fields(opts) {
			if w, ok := strings.CutPrefix(o, "--work="); ok && workModes[w] {
				f.Work = w
			}
			f.ReadOnly = f.ReadOnly || o == "--ro"
		}
		out = append(out, f)
	}
	return out
}

func formatSharedFolders(list []sharedFolder) string {
	var b strings.Builder
	for _, f := range list {
		var o []string
		if f.Work != "" {
			o = append(o, "--work="+f.Work)
		}
		if f.ReadOnly {
			o = append(o, "--ro")
		}
		b.WriteString(f.Path)
		if len(o) > 0 {
			b.WriteString("\t" + strings.Join(o, " "))
		}
		b.WriteString("\n")
	}
	return b.String()
}

// lookupSharedFolder: najbolj natančna mapa s seznama, v kateri je cwd.
func lookupSharedFolder(list []sharedFolder, cwd string) *sharedFolder {
	var best *sharedFolder
	for i := range list {
		if under(cwd, list[i].Path) && (best == nil || len(list[i].Path) > len(best.Path)) {
			best = &list[i]
		}
	}
	return best
}

// workMode: --minigw-work= / MINIGW_WORK > način mape iz »minigw share add --work=…« > direct. Shranjen --ro velja vedno;
// native samo za branje ni mogoč → direct samo za branje (tudi lažen router, ki native zavrne, ne odpre pisanja).
func workMode(flag string, saved *sharedFolder, readOnly bool) (string, bool) {
	work := flag
	if work == "" && saved != nil {
		work = saved.Work
	}
	if work == "" {
		work = "direct"
	}
	ro := readOnly || (saved != nil && saved.ReadOnly)
	if (work == "native" || work == "worktree") && ro { // worktree vrne vejo: samo za branje → direct
		work = "direct"
	}
	return work, ro
}

// validSharePath: ena vrstica seznama; tabulator loči možnosti.
func validSharePath(p string) bool { return !strings.ContainsAny(p, "\t\r\n") }

// under: path je folder ali v njem (na Windows brez razlike v velikih črkah).
func under(path, folder string) bool {
	rel, err := filepath.Rel(folder, path)
	if err != nil || strings.HasPrefix(rel, "..") || filepath.IsAbs(rel) {
		return false
	}
	return true
}

// shareAllowed: nikoli koren diska ali domača mapa sama (to bi izpostavilo vse).
func shareAllowed(dir string) bool {
	home, _ := os.UserHomeDir()
	return filepath.Dir(dir) != dir && !strings.EqualFold(filepath.Clean(dir), filepath.Clean(home))
}

func shareCommand(args []string) {
	sub := "list"
	if len(args) > 0 {
		sub = args[0]
	}
	// add: --work=direct|sync|native in --ro = privzeti način te mape in podmap (zastavica ob zagonu ima prednost)
	work, ro, rest := "", false, []string{}
	for _, a := range args[min(1, len(args)):] {
		if w, ok := strings.CutPrefix(a, "--work="); ok {
			work = w
		} else if a == "--ro" {
			ro = true
		} else {
			rest = append(rest, a)
		}
	}
	if work != "" && !workModes[work] {
		fail("unknown --work=" + work + " (use direct, sync, native, worktree or auto)")
	}
	if work == "native" && ro {
		fail("--work=native cannot be read-only (the agent works in the gateway's own folder)")
	}
	dir, _ := os.Getwd()
	if len(rest) > 0 {
		dir = rest[0]
	}
	dir, _ = filepath.Abs(dir)
	if !validSharePath(dir) {
		fail("folder names with tabs or line breaks cannot be shared")
	}
	list := sharedFolders()
	save := func(l []sharedFolder) {
		f := sharedFoldersFile()
		os.MkdirAll(filepath.Dir(f), 0o700)
		if err := os.WriteFile(f, []byte(formatSharedFolders(l)), 0o600); err != nil {
			fail("cannot save " + f + ": " + err.Error())
		}
	}
	describe := func(f sharedFolder) string {
		var o []string
		if f.Work != "" {
			o = append(o, f.Work)
		}
		if f.ReadOnly {
			o = append(o, "read-only")
		}
		if len(o) == 0 {
			return f.Path
		}
		return f.Path + "  (" + strings.Join(o, ", ") + ")"
	}
	var keep []sharedFolder
	for _, f := range list {
		if !strings.EqualFold(f.Path, dir) {
			keep = append(keep, f)
		}
	}
	switch sub {
	case "add":
		if st, err := os.Stat(dir); err != nil || !st.IsDir() {
			fail("not a folder: " + dir)
		}
		if !shareAllowed(dir) {
			fail("refusing to share " + dir + " (a drive root or your home folder would expose everything); share a project folder")
		}
		e := sharedFolder{Path: dir, Work: work, ReadOnly: ro}
		for _, f := range list {
			if strings.EqualFold(f.Path, dir) && f.Work == work && f.ReadOnly == ro {
				fmt.Println("already shared:", describe(e))
				return
			}
		}
		save(append(keep, e))
		fmt.Println("shared with the gateway from now on:", describe(e), "(and its subfolders)")
	case "remove":
		save(keep)
		fmt.Println("no longer shared:", dir)
	case "list":
		if len(list) == 0 {
			fmt.Println("no extra shared folders (folders under your home folder are shared by default)")
		}
		for _, f := range list {
			fmt.Println(describe(f))
		}
	default:
		fail("usage: minigw share add [--work=direct|sync|native] [--ro] [folder] | minigw share remove [folder] | minigw share list")
	}
}
