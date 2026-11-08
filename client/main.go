// minigw — zažene Claude Code prek Mini Gateway (odjemalec za Windows in Linux; isti protokol kot macOS minigw).
// Ena izvršljiva datoteka, le standardna knjižnica Go.
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
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

// requireProfile: profil tega ukaza (--minigw-profile ali privzeti).
func requireProfile() profile {
	ps := loadProfiles()
	if len(ps) == 0 {
		fail("not connected; run: minigw connect <host[:port]> --token '<token>'")
	}
	return ps[0]
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

// clientInfo: »Windows, minigw 0.5.18« za čakajočo napravo na prehodu. Le ASCII: druge znake v glavi prehod na Macu
// (UTF-8) zavrne.
func clientInfo() string {
	return map[string]string{"windows": "Windows", "darwin": "macOS", "linux": "Linux"}[runtime.GOOS] + ", minigw " + version
}

var proxy *noiseProxy

// route: oddaljen prehod → vse gre šifrirano prek lokalnega proxyja; lokalno navaden HTTP.
func route(p profile) connection {
	c, err := routeErr(p)
	if err != nil {
		fail(err.Error())
	}
	return c
}

func routeErr(p profile) (connection, error) {
	c := p.conn
	if c.isLocal() && c.Key == nil { // L3: s ključem prehoda tudi lokalno šifrirano (drug lokalni uporabnik na vratih ne dobi žetona)
		return c, nil
	}
	if c.Key == nil {
		return c, fmt.Errorf("no gateway key for profile %s: copy the token again (Mini Gateway → Copy Token) and run minigw connect", p.name)
	}
	key, err := fromBase64url(*c.Key)
	if err != nil || len(key) != 32 {
		return c, fmt.Errorf("invalid gateway key for profile %s: copy the token again (Mini Gateway → Copy Token)", p.name)
	}
	if c.Device == nil { // povezava brez ključa naprave: en ključ za ves odjemalec
		if c.Device = anyDevice(); c.Device == nil {
			d := newDeviceKey()
			c.Device = &d
		}
		saveProfile(p.name, c)
	}
	var dev *device
	if seed, err := fromBase64url(*c.Device); err == nil && len(seed) == ed25519.SeedSize {
		dev = &device{key: ed25519.NewKeyFromSeed(seed), name: deviceName()}
	}
	np, err := newNoiseProxy(c.URL, key, dev)
	if err == nil {
		err = np.start()
	}
	if err != nil {
		return c, fmt.Errorf("cannot start the local encrypted proxy: %v", err)
	}
	c.URL = np.localURL()
	return c, nil
}

// profileStatus: naslov, dosegljivost, ponudniki in število modelov enega profila; true = dosegljiv.
func profileStatus(p profile) bool {
	c, err := routeErr(p)
	if err != nil {
		say("Gateway:   " + p.conn.URL + " (" + err.Error() + ")")
		return false
	}
	st, ready := getJSON(c, "/health/ready")
	state := fmt.Sprintf("HTTP %d", st)
	if st == 200 {
		state = "reachable"
	} else if st == 0 {
		state = "unreachable"
	}
	if !p.conn.isLocal() || p.conn.Key != nil {
		state += ", encrypted"
	}
	say("Gateway:   " + p.conn.URL + " (" + state + ")")
	if pr, ok := ready["providers"].(map[string]any); ok {
		names := make([]string, 0, len(pr))
		for k := range pr {
			names = append(names, k)
		}
		sort.Strings(names)
		out := []string{}
		for _, k := range names {
			mark := "✗"
			if v, _ := pr[k].(bool); v {
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
	return st == 200
}

func getJSON(c connection, path string) (int, map[string]any) {
	st, data := call(c, "GET", path, nil, nil, 5*time.Second)
	var m map[string]any
	json.Unmarshal(data, &m)
	return st, m
}

func main() {
	refreshPath() // Windows: uporabnikov PATH tudi, ko pladenj zažene MSI
	args := os.Args[1:]
	if isTrayExe(os.Args[0]) { // minigw-tray.exe: router z ikono v območju za obvestila
		trayCommand(args)
		return
	}
	for i := 0; i < len(args); i++ { // --minigw-profile <ime>: cel ukaz le s tem prehodom (#17)
		if args[i] == "--minigw-profile" && i+1 >= len(args) {
			fail("--minigw-profile needs a profile name")
		}
		if v, ok := strings.CutPrefix(args[i], "--minigw-profile="); ok || args[i] == "--minigw-profile" {
			n := 2
			if ok {
				n = 1
			} else {
				v = args[i+1]
			}
			profileFlag = v
			args = append(args[:i], args[i+n:]...)
			i--
		}
	}
	first := ""
	if len(args) > 0 {
		first = args[0]
	}
	if c := agentConn(); c != nil {
		if first == "connect" || first == "disconnect" || first == "profiles" {
			fail("minigw runs inside an agent of the gateway (MINIGW_AGENT_URL): " + first + " is not available here")
		}
		http.DefaultTransport.(*http.Transport).Proxy = agentProxy
		if strings.HasPrefix(first, "-") && (slices.Contains(args, "-p") || slices.Contains(args, "--print")) {
			agentAsk(c, args)
			return
		}
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
			fail("usage: minigw connect <host[:port]> [--name <profile>] [--token '<token>' | --token -]   (- reads it from stdin)")
		}
		names := profileNames()
		name := defaultName(names) // brez --name: zamenja privzeti profil (kot prej edino povezavo)
		token := ""
		for i, a := range args {
			if a == "--token" && i+1 < len(args) {
				token = args[i+1]
			}
			if a == "--name" && i+1 < len(args) {
				name = args[i+1]
			}
		}
		if !validProfile(name) {
			fail("invalid --name " + name + " (lowercase letters, digits and -, up to 32)")
		}
		saved := loadProfile(name)
		if saved != nil && token == "" {
			token = saved.Token
			if saved.Key != nil {
				token += "#" + *saved.Key
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
		if c.Device = anyDevice(); c.Device == nil { // en ključ naprave za vse profile
			d := newDeviceKey()
			c.Device = &d
		}
		as := ""
		if name != defaultProfile || slices.ContainsFunc(names, func(n string) bool { return n != defaultProfile }) {
			as = " as profile " + name
		}
		st, m := getJSON(route(profile{name, c}), "/v1/models")
		switch {
		case st == 200:
			saveProfile(name, c)
			enc := ""
			if !c.isLocal() || c.Key != nil {
				enc = " (encrypted)"
			}
			say("Connected to " + c.URL + enc + as)
		case st == 401:
			fail("gateway at " + c.URL + " rejected the token")
		case st == 403: // naprava še ni dovoljena: ključ naprave ostane, da ga router lahko doda
			saveProfile(name, c)
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
		name := requireProfile().name
		for i, a := range args {
			if a == "--name" && i+1 < len(args) {
				name = args[i+1]
			}
		}
		if !validProfile(name) || loadProfile(name) == nil { // L1: ime gre v pot ali ime poverilnice
			fail("no profile " + name)
		}
		storeDelete(name)
		if len(profileNames()) == 0 && name == defaultProfile {
			say("Disconnected")
		} else {
			say("Disconnected profile " + name)
		}
		return

	case "profiles":
		profilesCommand(args[1:])
		return

	case "status":
		ps := loadProfiles()
		if len(ps) == 0 {
			requireProfile()
		}
		ok := true
		for i, p := range ps {
			if len(ps) > 1 {
				if i > 0 {
					say("")
				}
				say("Profile:   " + p.name + map[bool]string{true: " (default: its models also appear without @" + p.name + ")", false: ""}[i == 0])
			}
			if !profileStatus(p) && i == 0 {
				ok = false
			}
		}
		if !ok {
			os.Exit(1)
		}
		return

	case "models":
		ps := loadProfiles()
		if len(ps) == 0 {
			requireProfile()
		}
		var data []map[string]any
		if len(ps) == 1 {
			st, models := getJSON(route(ps[0]), "/v1/models")
			raw, _ := models["data"].([]any)
			if st != 200 || raw == nil {
				if st == 401 {
					fail("token rejected")
				}
				fail(fmt.Sprintf("cannot list models (HTTP %d)", st))
			}
			for _, m := range raw {
				mm, _ := m.(map[string]any)
				data = append(data, mm)
			}
		} else {
			lists, names := make([][]map[string]any, len(ps)), make([]string, len(ps))
			for i, p := range ps {
				names[i] = p.name
				if c, err := routeErr(p); err == nil {
					lists[i] = fetchModels(c)
				}
				if lists[i] == nil {
					fmt.Fprintln(os.Stderr, "minigw: gateway "+p.name+" ("+p.conn.URL+") is not reachable; its models are left out")
				}
			}
			if data = mergeModels(lists, names); data == nil {
				fail("no gateway is reachable (minigw status)")
			}
		}
		width := 0
		for _, m := range data {
			if id, _ := m["id"].(string); len(id) > width {
				width = len(id)
			}
		}
		for _, m := range data {
			id, _ := m["id"].(string)
			desc, _ := m["description"].(string)
			say(fmt.Sprintf("%-*s  %s", width, id, desc))
		}
		return

	case "--minigw-help", "-h", "--help", "-v", "--version":
		if first == "-v" || first == "--version" { // le minigw: različico claude da claude --version (gw status jo bere)
			say("minigw " + version)
			return
		}
		say("minigw connect <host[:port]> [--name P] [--token T|-]  save a gateway as profile P (" + storeName + "; - = stdin)\n" +
			"minigw status | models | disconnect [--name P]   all profiles; models of other profiles end in @P\n" +
			"minigw profiles [default P]                list profiles, or pick the default (its models also without @P)\n" +
			"minigw --minigw-profile P …                 use only profile P for this command\n" +
			"minigw share add|remove [folder] | list    share a system or hidden folder itself (contents are always shared)\n" +
			"minigw share add --work=native|native-direct|sync|direct [--ro] [folder]   default mode for that folder (flag/env wins)\n" +
			"minigw --minigw-ro [claude args]           share the current folder read-only (or MINIGW_SHARE=ro)\n" +
			"minigw --minigw-role NAME [claude args]    the caller's role for the gateway's session history (or MINIGW_ROLE)\n" +
			"minigw --minigw-work=sync [claude args]    agents work in a synced copy on the gateway (or MINIGW_WORK=sync)\n" +
			"minigw --minigw-work=native [claude args]  folder is the gateway's SMB share: agents work there; git repository: each on its own branch minigw/… (or MINIGW_WORK=native)\n" +
			"minigw --minigw-work=native-direct [claude args]  as native, but agents change the folder itself, one writing agent at a time\n" +
			"minigw --minigw-work=worktree [claude args]  git repository: agents work in a clone on the gateway, results come back as branches minigw/…\n" +
			"minigw --minigw-work=auto [claude args]    gateway share → native, git repository → worktree, otherwise sync (--minigw-ro → direct)\n" +
			"minigw run [--] <command> [args …]          run a build in this folder on the gateway (SMB share, allowed in its Settings)\n" +
			"minigw serve [init|allow <id>] [--port N] [--config FILE]   run a gateway here (Windows, Linux)\n" +
			"minigw gw status|devices|provider|claude|token …  change the gateway here, live (minigw gw help)\n" +
			"minigw [claude options …]                    run Claude Code through the gateway")
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
	// --minigw-role <ime> (ali MINIGW_ROLE): vloga klicatelja za zgodovino sej (kasneje omejitve); varovalo, ne meja.
	role, args, ok := takeRole(args, os.Getenv("MINIGW_ROLE"))
	if !ok {
		fail("--minigw-role: 1-32 letters, digits, . _ or -")
	}
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
		fail("unknown --minigw-work=" + work + " (use direct, sync, native, native-direct, worktree or auto)")
	}
	cwd, _ := os.Getwd()
	asked := work
	if f := lookupSharedFolder(sharedFolders(), cwd); asked == "" && f != nil {
		asked = f.Work
	}
	work, readOnly = workMode(work, lookupSharedFolder(sharedFolders(), cwd), readOnly)
	if (strings.HasPrefix(asked, "native") || asked == "worktree") && work != asked {
		fmt.Fprintln(os.Stderr, "minigw: "+asked+" mode cannot be read-only; sharing read-only over the network instead")
	}
	if agentConn() == nil {
		args = modelProfile(args, profileNames())
	}
	ps := loadProfiles()
	if len(ps) == 0 {
		requireProfile()
	}
	saved := ps[0].conn // privzeti profil: mapa, glave in modeli brez pripone kot prej
	c := route(ps[0])
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
	if role != "" {
		headers += "\nx-claude-mini-role: " + role
	}
	env = append(env, "ANTHROPIC_CUSTOM_HEADERS="+headers)

	// Načini po vrsti (auto: native → worktree → sync); naslednji, če router prejšnjega ne sprejme.
	top := ""
	if work == "auto" || work == "worktree" {
		top = gitToplevel(cwd)
	}
	_, _, smb := nativeDetect(cwd)
	tries := workCandidates(work, readOnly, work == "auto" && smb, top != "")
	noCommit := top != "" && slices.Contains(tries, "worktree") && !gitHasCommit(top)
	if noCommit { // brez commita ni posnetka: enkrat preveri, nato sync (#10)
		tries = slices.DeleteFunc(tries, func(w string) bool { return w == "worktree" })
	}
	share := &shareClient{c: c, session: sid, root: cwd, readOnly: readOnly, synced: map[string]syncEntry{}, reported: map[string]bool{}}
	if wantShare && sid != "" {
		if work == "worktree" && top == "" {
			fmt.Fprintln(os.Stderr, "minigw: "+cwd+" is not in a git repository; worktree mode is not possible")
		}
		if noCommit {
			fmt.Fprintln(os.Stderr, "minigw: the git repository "+top+" has no commit yet, so worktree mode is not possible; using "+tries[0])
		}
		var ok bool
		share, ok = registerShare(c, sid, cwd, readOnly, top, tries, false, asked == "")
		if ok && share.worktree {
			share.start(4)
			fmt.Fprintf(os.Stderr, "minigw: agent models work in a clone of this git repository on %s; their work comes back as branches minigw/… (worktree)\n", routerHost)
		} else if ok && share.native && share.branches {
			share.start(4)
			fmt.Fprintf(os.Stderr, "minigw: %s is a folder of %s (SMB): agent models work there in clones of this git repository, each on its own branch minigw/… (native; --minigw-work=native-direct = in the folder itself)\n", cwd, routerHost)
		} else if ok && share.native {
			share.start(4)
			lock := ""
			if share.lock {
				lock = ", one writing agent at a time"
			}
			fmt.Fprintf(os.Stderr, "minigw: %s is a folder of %s (SMB): agent models work in it there directly%s (native)\n", cwd, routerHost, lock)
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
	} else if !(saved.isLocal() && runtime.GOOS == "darwin") && os.Getenv("MINIGW_SHARE") != "0" {
		why := "" // samodejno se ne delijo sistemske, skrite in domača mapa
		if os.Getenv("MINIGW_SHARE") == "" {
			why = " (not shared automatically)"
		}
		fmt.Fprintf(os.Stderr, "minigw: %s is not shared with the gateway%s, so agent models (Codex, Claude CLI, Grok) cannot see it; to share it run: minigw share add (or MINIGW_SHARE=1 for this run)\n", cwd, why)
	}
	// Več profilov: lokalni usmerjevalnik pred Claude Code; mapa pri drugem prehodu ob prvi zahtevi zanj (#17).
	var mu sync.Mutex
	var extra []*shareClient
	if len(ps) > 1 {
		targets := []*muxTarget{{name: ps[0].name, conn: c}}
		for _, p := range ps[1:] {
			rc, err := routeErr(p)
			if err != nil {
				fmt.Fprintln(os.Stderr, "minigw: "+err.Error()+"; its models are left out")
				continue
			}
			t := &muxTarget{name: p.name, conn: rc}
			if sid != "" && shareWanted(cwd, p.conn.isLocal()) {
				t.share = func() bool {
					sc, ok := registerShare(rc, sid, cwd, readOnly, top, tries, true, asked == "")
					if !ok {
						fmt.Fprintln(os.Stderr, "minigw: gateway "+p.name+" did not accept the folder: "+clean(sc.refusal))
						return false
					}
					sc.start(4)
					mu.Lock()
					extra = append(extra, sc)
					mu.Unlock()
					return true
				}
			}
			targets = append(targets, t)
		}
		if len(targets) > 1 {
			mux := newGatewayMux(targets)
			if err := mux.start(); err != nil {
				fail("cannot start minigw's local proxy: " + err.Error())
			}
			env[0], env[1] = "ANTHROPIC_BASE_URL="+mux.url(), "ANTHROPIC_AUTH_TOKEN="+mux.secret
			fmt.Fprintf(os.Stderr, "minigw: %d gateways; models of %s also without a suffix, the others as <model>@<profile> (minigw models)\n", len(targets), ps[0].name)
		}
	}
	code := runClaude(args, env)
	share.unregister()
	mu.Lock()
	for _, sc := range extra {
		sc.unregister()
	}
	mu.Unlock()
	os.Exit(code)
}

// modelProfile: --model <model>@<profil> izbere profil za ves zagon (mapa, sync, worktree, native), kot --minigw-profile;
// model gre naprej brez pripone. Sicer bi mapo dobil privzeti prehod, model pa drug (#8).
func modelProfile(args, names []string) []string {
	for i, a := range args {
		v, eq := strings.CutPrefix(a, "--model=")
		if !eq {
			if a != "--model" || i+1 >= len(args) {
				continue
			}
			v = args[i+1]
		}
		at := strings.LastIndex(v, "@")
		if at <= 0 || !slices.Contains(names, v[at+1:]) {
			continue
		}
		if profileFlag != "" && profileFlag != v[at+1:] {
			fail("--model " + v + " and --minigw-profile " + profileFlag + " name different gateways")
		}
		profileFlag = v[at+1:]
		if eq {
			args[i] = "--model=" + v[:at]
		} else {
			args[i+1] = v[:at]
		}
	}
	return args
}

// registerShare: načini po vrsti (auto: native → worktree → sync); naslednji, če prehod prejšnjega ne sprejme.
// implicit: način ni izbran (zastavica, MINIGW_WORK, share add); prehod brez priklopa mape (minigw serve na Windows,
// Linux) direct zavrne → kot auto: worktree v repozitoriju git s commitom, sicer sync. Izrecni direct ostane napaka.
func registerShare(c connection, sid, cwd string, readOnly bool, top string, tries []string, quiet, implicit bool) (*shareClient, bool) {
	sc := &shareClient{c: c, session: sid, root: cwd, readOnly: readOnly, synced: map[string]syncEntry{}, reported: map[string]bool{}}
	for i, w := range tries {
		sc = &shareClient{c: c, session: sid, root: cwd, readOnly: readOnly, sync: w == "sync", native: strings.HasPrefix(w, "native"), inPlace: w == "native-direct",
			worktree: w == "worktree", top: top, synced: map[string]syncEntry{}, reported: map[string]bool{}}
		if sc.register() {
			return sc, true
		}
		if i+1 < len(tries) && !quiet {
			fmt.Fprintln(os.Stderr, "minigw: "+w+" mode not available: "+clean(sc.refusal)+"; trying "+tries[i+1])
		}
	}
	if !implicit || !strings.HasPrefix(sc.refusal, "this gateway supports --minigw-work=") {
		return sc, false
	}
	t, auto := gitToplevel(cwd), []string{"sync"}
	// M1: worktree le, če je koren repozitorija ta mapa; nadrejeni (dotfiles v domači mapi, monorepo) bi poslal tudi
	// datoteke zunaj nje.
	rt, rc := realPath(t), realPath(cwd)
	if !readOnly && t != "" && (rt == rc || runtime.GOOS == "windows" && strings.EqualFold(rt, rc)) && gitHasCommit(t) {
		auto = []string{"worktree", "sync"}
	}
	if !quiet {
		fmt.Fprintln(os.Stderr, "minigw: the gateway has no folder mount; using "+auto[0]+" mode (--minigw-work= to choose)")
	}
	return registerShare(c, sid, cwd, readOnly, t, auto, quiet, false)
}

// shareWanted: projektno mapo (autoShare ali minigw share add) deli samodejno; nikoli cele domače mape, skritih map ali
// sistemskih podatkov; MINIGW_SHARE=0/1 preglasi. Prehod na tem Macu (aplikacija) dela v mapi sami, zato tam ne;
// minigw serve na tem računalniku (Windows, Linux) pa mapo potrebuje kot oddaljen (#9).
func shareWanted(cwd string, local bool) bool {
	switch os.Getenv("MINIGW_SHARE") {
	case "1":
		return true
	case "0":
		return false
	}
	if local && runtime.GOOS == "darwin" {
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
	cmd := consoleCommand("claude", args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = claudeEnv(os.Environ(), env)
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		fmt.Fprintln(os.Stderr, "minigw: cannot run claude: "+err.Error()+" (is Claude Code installed and in PATH?)")
		return 127
	}
	return 0
}

// claudeEnv: okolje za claude brez podedovanih ANTHROPIC_*, CLAUDECODE in CLAUDE_CODE_* (seja in prijava starševskega
// Claude Code ali Claude Desktop, npr. CLAUDE_CODE_OAUTH_TOKEN, ANTHROPIC_BASE_URL: sicer 401 na prehodu), nato naše.
// CLAUDE_CODE_GIT_BASH_PATH ostane (nastavitev uporabnika na Windows, ne prijava).
func claudeEnv(base, own []string) []string {
	var out []string
	for _, e := range base {
		k, _, _ := strings.Cut(strings.ToUpper(e), "=") // Windows: imena brez razlike v velikosti črk
		if (strings.HasPrefix(k, "ANTHROPIC_") || k == "CLAUDECODE" || strings.HasPrefix(k, "CLAUDE_CODE_")) && k != "CLAUDE_CODE_GIT_BASH_PATH" {
			continue
		}
		out = append(out, e)
	}
	return append(out, own...)
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

var workModes = map[string]bool{"direct": true, "sync": true, "native": true, "native-direct": true, "worktree": true, "auto": true}

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
	if (work == "native" || work == "native-direct" || work == "worktree") && ro { // worktree vrne vejo: samo za branje → direct
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
	// add: --work=direct|sync|native|native-direct|worktree|auto in --ro = privzeti način te mape in podmap (zastavica ob zagonu ima prednost)
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
		fail("unknown --work=" + work + " (use direct, sync, native, native-direct, worktree or auto)")
	}
	if strings.HasPrefix(work, "native") && ro {
		fail("--work=" + work + " cannot be read-only (the agent works in the gateway's own folder)")
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
		fail("usage: minigw share add [--work=direct|sync|native|native-direct|worktree|auto] [--ro] [folder] | minigw share remove [folder] | minigw share list")
	}
}

// takeRole: --minigw-role <ime> ali --minigw-role=<ime> (zadnji velja) iz argumentov, sicer env; ok=false za neveljavno ime
// (enako kot GatewayHistory.takeRole na Macu).
func takeRole(args []string, env string) (role string, rest []string, ok bool) {
	role = env
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--minigw-role" && i+1 < len(args):
			role = args[i+1]
			i++
		case strings.HasPrefix(args[i], "--minigw-role="):
			role = strings.TrimPrefix(args[i], "--minigw-role=")
		default:
			rest = append(rest, args[i])
		}
	}
	return role, rest, role == "" || validRole(role)
}
