package main

// minigw router: skrbniški API routerja na tem računalniku. Če router teče, spremembe veljajo takoj; sicer ista koda
// (adminDo) spremeni serve.json in shrambo ključev. minigw serve allow|deny|devices|gateway|status so vzdevki.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

const routerHelp = `minigw gw: show or change the minigw serve gateway on this computer. Changes apply at once when it
runs; otherwise they go to serve.json.

  minigw gw status
  minigw gw devices [allow <device id> [--name N] | rename <device id> --name N | deny <device id>]
  minigw gw devices history <device id>                 the device's last connections
  minigw gw sessions [--device ID] [--folder DIR] | <session id> | clear
  minigw gw usage [--days N] [--device ID] [--folder DIR]   counters per folder, day (UTC) and agent (clear: sessions clear)
  minigw gw provider add <name> <baseURL> [--insecure]   the API key is read from stdin or asked for
  minigw gw provider remove <name> | list
  minigw gw provider models <name> [text] [--all] [--enable m1,m2|all] [--disable m1,m2|all]
  minigw gw claude [--path FILE] [--tools T1,T2] [--folders DIR1,DIR2] [--allow-local-tools=true|false]
                  [--ask-others=true|false]   (may ask other models of this gateway; needs --allow-local-tools)
                  [--allow-unsandboxed-bash=true|false]   (minigw serve only)
  minigw gw claude|codex|grok|copilot [--work=direct|sync|worktree]
                                                        the highest work mode in which the agent may change the
                                                        client's folder (in a lower mode it only reads)
  minigw gw autostart [on|off]                          Windows: start the tray at sign-in (Linux: minigw serve install); no argument: show it
  minigw gw token [rotate]                              print the connect line; rotate makes a new token
  minigw gw scratch [move [DIR]|take [NAME]]            the Scratch folder (mirrors, clones, deps, caches, agent
                                                        state); move: all of it to DIR/<instance> (no DIR: the
                                                        proposed folder), only while no minigw session or agent runs;
                                                        take: take over the folder (or NAME next to it) of a
                                                        gateway that is gone
  minigw gw claude|codex|grok|copilot login            the agent's login (set / not set / logged in), never the value
  minigw gw claude token [remove]                       the token from claude setup-token (needed in the Windows
                                                        sandbox); read from stdin or asked for
  minigw gw claude|codex|grok apikey [remove]           use an API key instead of the login (billed through the
                                                        provider's API); remove: back to the login
  minigw gw settings                                    the settings in this terminal (minigw-tui; also over SSH)
  minigw gw settings link                               a one-time link (60 s) to the settings page
  minigw gw api GET|POST|PATCH|DELETE <path>            one admin API request (docs/admin-api.md): the JSON body
                                                        on stdin, the JSON answer on stdout (used by minigw-tui)

All take --config FILE (default: the minigw serve config).`

type adminClient struct {
	base, secret string  // tekoči router
	routerKey    []byte  // javni ključ routerja: Noise preveri, da je na vratih res ta router
	off          *server // router ne teče
	// answered (H1): vrata so odgovorila (ali ne v času), a ne kot ta router: morda teče; premik in prevzem Scratch ne
	// gresta mimo njega (prazen hub ne vidi njegovih sej).
	answered bool
}

// errNotRouter: na vratih je drug program (rokovanje Noise s ključem routerja ne uspe); skrivnost ni bila poslana.
var errNotRouter = errors.New("not this gateway")

// noiseClientConn: HTTP v okvirjih MGW2 (kot noiseProxy), za skrbniški API na 127.0.0.1.
type noiseClientConn struct {
	net.Conn
	sess       *noiseSession
	raw, plain []byte
}

func (c *noiseClientConn) Write(p []byte) (int, error) {
	if _, err := c.Conn.Write(frames(p, c.sess.send)); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *noiseClientConn) Read(p []byte) (int, error) {
	buf := make([]byte, 64<<10)
	for len(c.plain) == 0 {
		n, err := c.Conn.Read(buf)
		if n > 0 {
			plain, rest, ferr := unframe(append(c.raw, buf[:n]...), c.sess.receive)
			if ferr != nil {
				return 0, ferr
			}
			c.raw, c.plain = rest, append(c.plain, plain...)
		}
		if err != nil && len(c.plain) == 0 {
			return 0, err
		}
	}
	n := copy(p, c.plain)
	c.plain = c.plain[n:]
	return n, nil
}

// dialAdmin: TCP na 127.0.0.1 in rokovanje Noise NK s ključem routerja iz serve.json. Kdor zasede vrata brez
// zasebnega ključa routerja, rokovanja ne konča in nikoli ne dobi skrivnosti ali ključev prehodov.
func dialAdmin(ctx context.Context, addr string, routerKey []byte) (*noiseClientConn, error) {
	c, err := (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	hs, err := newInitiator(routerKey)
	first, ferr := hs.first()
	if err != nil || ferr != nil {
		c.Close()
		return nil, errNotRouter
	}
	c.SetDeadline(time.Now().Add(10 * time.Second))
	reply := make([]byte, handshakeLen)
	if _, err := c.Write(append(append([]byte{}, noiseMagic...), first...)); err == nil {
		_, err = io.ReadFull(c, reply)
	}
	sess, herr := hs.finish(reply)
	if err != nil || herr != nil {
		c.Close()
		return nil, fmt.Errorf("%w: %v", errNotRouter, errors.Join(err, herr))
	}
	c.SetDeadline(time.Time{})
	return &noiseClientConn{Conn: c, sess: sess}, nil
}

// http: ena zahteva na novi povezavi Noise; glava MGW-Admin je dokaz, vezan na rokovanje te povezave (skrivnost
// nikoli ne gre po žici).
func (a *adminClient) http(method, path string, body []byte, timeout time.Duration) (int, []byte, error) {
	r, err := http.NewRequest(method, a.base+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	c, err := dialAdmin(ctx, r.URL.Host, a.routerKey)
	if err != nil {
		return 0, nil, err
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(timeout))
	r.Header.Set("authorization", adminScheme+adminProof(a.secret, c.sess.hash))
	r.Header.Set("content-type", "application/json")
	r.Close = true
	if err := r.Write(c); err != nil {
		return 0, nil, err
	}
	resp, err := http.ReadResponse(bufio.NewReader(c), r)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	return resp.StatusCode, data, err
}

// openAdmin: tekoči router (admin.secret in odgovor na status) ali brez routerja (serve.json).
func openAdmin(cfgPath string) *adminClient {
	c, err := loadServeConfig(cfgPath)
	if errors.Is(err, os.ErrNotExist) {
		fail("cannot read " + cfgPath + " (run minigw serve init first): " + err.Error())
	} else if err != nil { // neveljaven JSON: router se s to datoteko ne zažene
		fail("the gateway cannot start with these settings, fix them: " + err.Error())
	}
	a := &adminClient{}
	cfgDir, _ := filepath.Abs(filepath.Dir(cfgPath))
	k, kerr := c.privateKey()
	if b, err := os.ReadFile(filepath.Join(cfgDir, "admin.secret")); err == nil && kerr == nil {
		a.base, a.secret = "http://127.0.0.1:"+strconv.Itoa(c.Port)+adminPrefix, strings.TrimSpace(string(b))
		a.routerKey = k.PublicKey().Bytes()
		status, _, err := a.http("GET", "/status", nil, 3*time.Second)
		switch {
		case errors.Is(err, errNotRouter):
			a.answered = true
			fmt.Fprintf(os.Stderr, "minigw: port %d answers, but not as the gateway of %s (wrong key): changing that file only; nothing was sent to it\n", c.Port, cfgPath)
		case err != nil: // ne teče
			a.answered = !connRefused(err)
		case status == 200:
			return a
		case status == 401 || status == 403:
			fail(fmt.Sprintf("the gateway on port %d refused the admin request (HTTP %d): is it the gateway of %s?", c.Port, status, cfgPath))
		default:
			a.answered = true
			fmt.Fprintf(os.Stderr, "minigw: port %d answers without the admin API (another program, or an older minigw serve): changing %s; restart that gateway\n", c.Port, cfgPath)
		}
	}
	if a.off, err = offlineServer(cfgPath); err != nil {
		fail(err.Error())
	}
	a.base = ""
	return a
}

// do: zahteva k API; napaka API konča ukaz z njenim sporočilom.
func (a *adminClient) do(method, path string, body, out any) {
	var data []byte
	if body != nil {
		data, _ = json.Marshal(body)
	}
	var status int
	var resp []byte
	if a.off == nil {
		var err error
		timeout := 60 * time.Second
		if path == "/general/scratch/move" { // kopija na drug disk traja
			timeout = 4 * time.Hour
		}
		if status, resp, err = a.http(method, path, data, timeout); err != nil {
			fail("gateway admin API: " + err.Error())
		}
	} else {
		st, obj := a.off.adminDo(method, path, data)
		status, resp = st, must(json.Marshal(obj))
	}
	if status >= 300 {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		json.Unmarshal(resp, &e)
		fail(clean(e.Error.Message))
	}
	if out != nil {
		json.Unmarshal(resp, out)
	}
}

func must(b []byte, _ error) []byte { return b }

// autostartLine: zagon ob prijavi za gw status in gw autostart. Windows: vrednost Run, tudi pri namestitvi MSI, kjer
// minigw serve uninstall zavrne.
func autostartLine(autostart string) string {
	stop, start := "minigw serve uninstall", "minigw serve install"
	if runtime.GOOS == "windows" {
		stop, start = "minigw gw autostart off", "minigw gw autostart on"
	}
	return map[string]string{"boot": "starts at boot (" + stop + " to stop that)", "sign-in": "starts at sign-in (" + stop + " to stop that)",
		"": "does not start on its own (" + start + ")"}[autostart]
}

func routerCommand(args []string, in io.Reader) {
	fs := flag.NewFlagSet("minigw gw", flag.ExitOnError)
	cfgPath := fs.String("config", defaultServeConfigPath(), "config file")
	insecure := fs.Bool("insecure", false, "provider add: allow http to localhost or a private IP")
	enable := fs.String("enable", "", "provider models: enable these models (comma-separated, or all)")
	disable := fs.String("disable", "", "provider models: disable these models (comma-separated, or all)")
	all := fs.Bool("all", false, "provider models: also list the disabled models")
	device := fs.String("device", "", "sessions: only this device")
	folder := fs.String("folder", "", "sessions: only this folder and its subfolders")
	days := fs.String("days", "7", "usage: the last N days (1-31; the gateway checks it)")
	devName := fs.String("name", "", "devices allow/rename: the device's name (a label; the ID identifies it)")
	path := fs.String("path", "", "claude: path to claude (empty = PATH)")
	tools := fs.String("tools", "", "claude: the agent's tools")
	bash := fs.String("allow-unsandboxed-bash", "", "claude: true or false")
	folders := fs.String("folders", "", "claude: allowed folders, comma-separated (as on the Mac)")
	localTools := fs.String("allow-local-tools", "", "claude: true or false")
	askOthers := fs.String("ask-others", "", "claude: true or false")
	work := fs.String("work", "", "claude, codex, grok, copilot: direct, sync or worktree")
	fs.Usage = func() { fmt.Fprintln(os.Stderr, routerHelp) }
	var pos []string // ukazi in zastavice v poljubnem vrstnem redu
	for {
		// ID naprave (base64url) se lahko začne z »-«: ni zastavica.
		for len(args) > 0 && validDeviceID(args[0]) {
			pos, args = append(pos, args[0]), args[1:]
		}
		fs.Parse(args)
		if args = fs.Args(); len(args) == 0 {
			break
		}
		pos, args = append(pos, args[0]), args[1:]
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if len(pos) == 0 || pos[0] == "help" {
		say(routerHelp)
		return
	}
	if set["work"] && !validWork(*work) {
		fail("--" + badWork)
	}
	if set["work"] && !slices.Contains([]string{"claude", "codex", "grok"}, pos[0]) && (pos[0] != "copilot" || len(pos) != 1) {
		fail("--work only applies to minigw gw claude|codex|grok|copilot")
	}
	if len(pos) == 1 && pos[0] == "settings" { // terminal UI: kliče nazaj ta minigw (gw api), skrivnosti ne vidi
		var extra []string
		if set["config"] {
			extra = []string{"--config", *cfgPath}
		}
		settingsTUI(extra)
	}
	a := openAdmin(*cfgPath)
	need := func(min, max int) {
		if len(pos) < min || len(pos) > max {
			fail(routerHelp)
		}
	}
	esc := url.PathEscape
	switch pos[0] {
	case "status":
		need(1, 1)
		var st apiStatus
		a.do("GET", "/status", nil, &st)
		state := "not running (changes go to " + *cfgPath + ")"
		if st.Running {
			state = "running, changes apply at once"
		}
		say("gateway: " + state)
		addr := st.Address
		if addr == "" {
			addr = "all addresses"
		}
		say(fmt.Sprintf("version %s, port %d on %s", st.Version, st.Port, addr))
		say(autostartLine(st.Autostart))
		say(fmt.Sprintf("agent requests running: %d", st.Agents))
		if sb := st.AgentSandbox; sb != nil { // #68: Windows
			if sb["kind"] == "mxc" {
				say("agent sandbox: Windows sandbox (MXC " + sb["version"] + ")")
			} else {
				say(clean("agent sandbox: not available (" + sb["reason"] + ")"))
			}
		}
		say(installedInfo(runtime.GOOS))
		say(fmt.Sprintf("devices: %d allowed, %d waiting (minigw gw devices)", st.Devices, st.Pending))
		claude := "not found"
		if st.Claude.Found {
			claude = "found"
		}
		say(clean(fmt.Sprintf("claude: %s, tools %s", claude, st.Claude.Tools)))
		if st.Claude.Off != "" {
			say(clean("Claude CLI and Codex are off: " + st.Claude.Off))
		}
		if st.Copilot != "" { // #81
			say(clean("Copilot is off: " + st.Copilot))
		}
		for _, g := range st.Gateways {
			n := "all"
			if g.Models != nil {
				n = strconv.Itoa(len(g.Models))
			}
			ready := ""
			if st.Running {
				ready = map[bool]string{true: ", ready", false: ", models not loaded (see the gateway log)"}[g.Ready]
			}
			say(clean(fmt.Sprintf("provider %s: %s, models enabled: %s%s", g.Name, g.BaseURL, n, ready)))
		}

	case "devices":
		switch {
		case len(pos) == 1:
			var d struct {
				Devices, Pending []apiDevice
				AllowAnyDevice   bool
			}
			a.do("GET", "/devices", nil, &d)
			for _, x := range d.Devices {
				client := ""
				if x.Client != "" {
					client = fmt.Sprintf(" (%q)", x.Client)
				}
				say(fmt.Sprintf("%s  %q%s", x.ID, x.Name, client))
			}
			if len(d.Devices) == 0 {
				say(map[bool]string{true: "no devices listed: any device with the token is allowed (allowAnyDevice)", false: "no devices allowed"}[d.AllowAnyDevice])
			}
			for _, x := range d.Pending {
				client := ""
				if x.Client != "" {
					client = fmt.Sprintf(" (%q)", x.Client)
				}
				say(fmt.Sprintf("waiting: %s  %q from %q%s at %s; compare the ID with the client (minigw status), then: minigw gw devices allow %s [--name N]",
					x.ID, x.Name, x.Address, client, x.Time, x.ID))
			}
		case pos[1] == "allow" && len(pos) >= 3:
			name := *devName
			if name == "" { // starejša oblika: allow <id> <ime>
				name = strings.Join(pos[3:], " ")
			}
			var r struct {
				Device apiDevice
				Added  bool
			}
			a.do("POST", "/devices", apiDevice{ID: pos[2], Name: name}, &r)
			say(fmt.Sprintf("%s%s %q", map[bool]string{true: "allowed device ", false: "already allowed: "}[r.Added], r.Device.ID, r.Device.Name))
		case pos[1] == "rename" && len(pos) == 3 && *devName != "":
			var r struct{ Device apiDevice }
			a.do("PATCH", "/devices/"+esc(pos[2]), map[string]string{"name": *devName}, &r)
			say(fmt.Sprintf("renamed device %s to %q", r.Device.ID, r.Device.Name))
		case pos[1] == "history" && len(pos) == 3:
			var r struct {
				ID, Name string
				History  []deviceConn
			}
			a.do("GET", "/devices/"+esc(pos[2])+"/history", nil, &r)
			say(fmt.Sprintf("%s  %q", r.ID, r.Name))
			for _, h := range r.History {
				say(fmt.Sprintf("%s  %q  %q", h.Time.UTC().Format(time.RFC3339), h.Address, h.Client))
			}
			if len(r.History) == 0 {
				say("no connections recorded")
			}
		case pos[1] == "deny" && len(pos) == 3:
			delete(set, "config")
			if len(set) > 0 { // --name ipd.: brisanje ni namen (preimenuj: rename)
				fail("minigw gw devices deny takes no options (to rename a device: minigw gw devices rename <id> --name N)")
			}
			var r struct {
				Device  apiDevice
				Pending bool
			}
			a.do("DELETE", "/devices/"+esc(pos[2]), nil, &r)
			say(fmt.Sprintf("%s device %s %q", map[bool]string{true: "rejected waiting", false: "removed"}[r.Pending], r.Device.ID, r.Device.Name))
		default:
			fail(routerHelp)
		}

	case "sessions":
		switch {
		case len(pos) == 1:
			var r struct{ Sessions []apiSession }
			a.do("GET", "/sessions?device="+url.QueryEscape(*device)+"&folder="+url.QueryEscape(*folder), nil, &r)
			for _, x := range r.Sessions {
				say(sessionLine(x))
			}
			if len(r.Sessions) == 0 {
				say("no sessions recorded")
			}
		case len(pos) == 2 && pos[1] == "clear":
			a.do("DELETE", "/sessions", nil, nil)
			say("session history cleared")
		case len(pos) == 2:
			var x apiSession
			a.do("GET", "/sessions/"+esc(pos[1]), nil, &x)
			say(sessionLine(x))
			say(fmt.Sprintf("start %s, last %s, duration %.0fs", x.Start.UTC().Format(time.RFC3339), x.Last.UTC().Format(time.RFC3339), x.Duration))
			if x.Branch != "" {
				say("branch " + strconv.Quote(x.Branch))
			}
			if x.Role != "" {
				say("role " + x.Role)
			}
			for _, m := range x.Models {
				say(useLine(m))
			}
		default:
			fail(routerHelp)
		}

	case "usage":
		if len(pos) != 1 {
			fail(routerHelp)
		}
		var r struct{ Usage []projectUse }
		a.do("GET", "/usage?days="+url.QueryEscape(*days)+"&device="+url.QueryEscape(*device)+"&folder="+url.QueryEscape(*folder), nil, &r)
		for _, p := range r.Usage {
			say(usageLine(p))
		}
		if len(r.Usage) == 0 {
			say("no usage recorded")
		}

	case "provider", "gateway": // gateway: starejše ime, skrit alias
		if len(pos) < 2 {
			fail(routerHelp)
		}
		switch pos[1] {
		case "add":
			need(4, 4)
			key, err := readKey(in, "API key for "+pos[2])
			if err != nil {
				fail(err.Error())
			}
			var r struct {
				Gateway  apiGateway
				KeyStore string
				Models   []apiModel
			}
			a.do("POST", "/gateways", map[string]any{"name": pos[2], "baseURL": pos[3], "key": key, "insecure": *insecure}, &r)
			say(fmt.Sprintf("provider %s: %d models, %s enabled; API key stored in %s", pos[2], len(r.Models), enabledText(r.Models), r.KeyStore))
			if len(r.Models) > 0 && enabledText(r.Models) == "0" {
				say("enable models with: minigw gw provider models " + pos[2] + " --enable <id>,<id> (find them with: minigw gw provider models " + pos[2] + " <text>)")
			}
		case "remove":
			need(3, 3)
			a.do("DELETE", "/gateways/"+esc(pos[2]), nil, nil)
			say("removed provider " + pos[2] + " and its API key")
		case "list":
			need(2, 2)
			var r struct{ Gateways []apiGateway }
			a.do("GET", "/gateways", nil, &r)
			for _, g := range r.Gateways {
				n := "all"
				if g.Models != nil {
					n = strconv.Itoa(len(g.Models))
				}
				say(clean(fmt.Sprintf("%s  %s  models enabled: %s", g.Name, g.BaseURL, n)))
			}
			if len(r.Gateways) == 0 {
				say("no providers (add one with: minigw gw provider add <name> <baseURL>)")
			}
		case "models":
			need(3, 4)
			var r struct{ Models, Changed []apiModel }
			if *enable != "" || *disable != "" {
				body := map[string]any{}
				for _, x := range []struct {
					v, key string
					all    bool
				}{{*enable, "enable", true}, {*disable, "disable", false}} {
					if x.v == "all" {
						body["all"] = x.all
					} else if x.v != "" {
						body[x.key] = strings.Split(strings.ReplaceAll(x.v, " ", ""), ",")
					}
				}
				a.do("POST", "/gateways/"+esc(pos[2])+"/models", body, &r)
				for _, m := range r.Changed {
					say(modelLine(m))
				}
				say(fmt.Sprintf("%s of %d enabled", enabledText(r.Models), len(r.Models)))
				return
			}
			filter := ""
			if len(pos) == 4 {
				filter = pos[3]
			}
			a.do("GET", "/gateways/"+esc(pos[2])+"/models?filter="+url.QueryEscape(filter), nil, &r)
			hidden := 0 // OpenRouter: več sto modelov; brez izbire le vklopljeni
			for _, m := range r.Models {
				if m.Enabled || *all || filter != "" {
					say(modelLine(m))
				} else {
					hidden++
				}
			}
			if hidden > 0 {
				say(fmt.Sprintf("%d disabled models not shown: add --all, or a text to search (minigw gw provider models %s haiku)", hidden, pos[2]))
			}
		default:
			fail(routerHelp)
		}

	case "claude":
		if len(pos) > 1 {
			agentLoginCmd(a, in, pos)
			return
		}
		need(1, 1)
		var c apiClaude
		if set["path"] || set["tools"] || set["allow-unsandboxed-bash"] || set["folders"] || set["allow-local-tools"] || set["ask-others"] || set["work"] {
			body := map[string]any{}
			if set["work"] {
				body["work"] = *work
			}
			if set["path"] {
				body["path"] = *path
			}
			if set["tools"] {
				body["tools"] = *tools
			}
			if set["allow-unsandboxed-bash"] {
				b, err := strconv.ParseBool(*bash)
				if err != nil {
					fail("--allow-unsandboxed-bash: true or false")
				}
				body["allowUnsandboxedBash"] = b
			}
			if set["folders"] {
				body["folders"] = *folders
			}
			if set["allow-local-tools"] {
				b, err := strconv.ParseBool(*localTools)
				if err != nil {
					fail("--allow-local-tools: true or false")
				}
				body["allowLocalTools"] = b
			}
			if set["ask-others"] {
				b, err := strconv.ParseBool(*askOthers)
				if err != nil {
					fail("--ask-others: true or false")
				}
				body["askOthers"] = b
			}
			a.do("PATCH", "/agents/claude", body, &c)
		} else {
			a.do("GET", "/agents/claude", nil, &c)
		}
		path := c.Path
		if path == "" {
			path = "(from PATH)"
		}
		say(clean(fmt.Sprintf("path: %s, found: %v\ntools: %s\nallowLocalTools: %v\naskOthers: %v\nfolders: %s\nallowUnsandboxedBash: %v\nwork: %s",
			path, c.Found, c.Tools, c.AllowLocalTools, c.AskOthers, c.Folders, c.AllowUnsandboxedBash, c.Work)))
		if c.Off != "" {
			say(clean("Claude CLI and Codex are off: " + c.Off))
		}

	case "autostart": // kot na Macu; Linux: API pove, naj se uporabi minigw serve install
		need(1, 2)
		if len(pos) == 1 { // brez on/off: trenutno stanje
			var st apiStatus
			a.do("GET", "/status", nil, &st)
			say(autostartLine(st.Autostart))
			return
		}
		if pos[1] != "on" && pos[1] != "off" {
			fail(routerHelp)
		}
		a.do("PATCH", "/general", map[string]any{"startAtSignIn": pos[1] == "on"}, nil)
		say(map[bool]string{true: "Mini Gateway starts at sign-in", false: "Mini Gateway no longer starts at sign-in"}[pos[1] == "on"])

	case "token":
		need(1, 2)
		var g apiGeneral
		if len(pos) == 2 {
			if pos[1] != "rotate" {
				fail(routerHelp)
			}
			a.do("POST", "/general/token", nil, nil)
			a.do("GET", "/general", nil, &g) // POST žetona ne vrne
			say("New token. Each client must run the new line, or run minigw reconnect [--name P] with the new token; the old token no longer works:")
		} else {
			a.do("GET", "/general", nil, &g)
			say("On each client run:")
		}
		say("  " + g.Connect)
	case "scratch":
		need(1, 3)
		var g apiGeneral
		if len(pos) > 1 && a.off != nil && a.answered {
			fail("the gateway of " + *cfgPath + " may be running (its port answers, but not as that gateway): stop it, or fix the port and admin.secret, then try again")
		}
		if len(pos) == 1 {
			a.do("GET", "/general", nil, &g)
			say("Scratch folder: " + g.Scratch)
			switch g.ScratchState {
			case scratchInUse:
				say("It is " + inUseText() + ": agents are off; choose another folder (minigw gw scratch move DIR)")
			case scratchStale:
				say("It belongs to another gateway that is not running (stale owner): agents are off; take it over (minigw gw scratch take) or choose another folder (minigw gw scratch move DIR)")
			}
			if len(g.ScratchOthers) > 0 {
				say("Folders of other gateways next to it: " + scratchOthersLine(g.ScratchOthers) + "; take a stale one over: minigw gw scratch take NAME")
			}
			if g.ScratchSuggest != "" {
				say("It is inside your home folder (agents in the Windows sandbox can list the names there); move it: minigw gw scratch move " + g.ScratchSuggest)
			}
			return
		}
		if pos[1] == "take" { // N4: headless Linux (stran z nastavitvami, TUI, Mac: Take over)
			var body any
			if len(pos) == 3 {
				body = map[string]string{"name": pos[2]}
			} else if a.do("GET", "/general", nil, &g); g.ScratchState == "" { // nastavitev je naša: sosednja mapa
				var stale []string
				for _, o := range g.ScratchOthers {
					if o.State == scratchStale {
						stale = append(stale, o.Name)
					}
				}
				if len(stale) != 1 {
					fail("name the folder: minigw gw scratch take NAME (" + scratchOthersLine(g.ScratchOthers) + ")")
				}
				body = map[string]string{"name": stale[0]}
			}
			a.do("POST", "/general/scratch/take", body, &g)
			say("Scratch folder taken over: " + g.Scratch)
			if g.Warning != "" {
				say(g.Warning)
			}
			return
		}
		if pos[1] != "move" {
			fail(routerHelp)
		}
		to := ""
		if len(pos) == 3 {
			abs, err := filepath.Abs(pos[2])
			if err != nil {
				fail(err.Error())
			}
			to = abs
		}
		say("Moving the Scratch folder (new sessions wait until it is done) …")
		a.do("POST", "/general/scratch/move", map[string]string{"to": to}, &g)
		say("Scratch folder: " + g.Scratch)
		if g.Warning != "" {
			say("WARNING: " + g.Warning)
		}
	case "codex", "grok", "copilot": // #60: omejitev načina (work); brez --work le izpis; #68: login, token, apikey
		if len(pos) > 1 {
			agentLoginCmd(a, in, pos)
			return
		}
		need(1, 1)
		agentWork(a, pos[0], set["work"], *work)
	case "api": // minigw-tui: ena zahteva skrbniškega API; telo JSON s stdin (ključi nikoli v argv), odgovor JSON na stdout
		need(3, 3)
		if !apiRequestOK(pos[1], pos[2]) {
			fail(routerHelp)
		}
		var body any
		if pos[1] == "POST" || pos[1] == "PATCH" {
			data, err := io.ReadAll(io.LimitReader(in, 1<<20))
			if err != nil || (len(bytes.TrimSpace(data)) > 0 && !json.Valid(data)) {
				fail("minigw gw api: the body on stdin is not JSON")
			}
			if len(bytes.TrimSpace(data)) > 0 {
				body = json.RawMessage(data)
			}
		}
		var out json.RawMessage
		a.do(pos[1], pos[2], body, &out)
		os.Stdout.Write(append(out, '\n'))
	case "settings":
		if len(pos) != 2 || pos[1] != "link" {
			fail(routerHelp)
		}
		var r struct{ URL string }
		a.do("POST", "/settings/login", nil, &r)
		say("Open within 60 seconds (once), in a browser on this computer:\n  " + r.URL)
	default:
		fail("unknown command minigw gw " + pos[0] + "\n\n" + routerHelp)
	}
}

// apiRequestOK: gw api le z metodo API in potjo pod skrbniškim API (brez »..«, presledkov, kontrolnih znakov in »#«).
func apiRequestOK(method, path string) bool {
	return slices.Contains([]string{"GET", "POST", "PATCH", "DELETE"}, method) && strings.HasPrefix(path, "/") &&
		!strings.Contains(path, "..") && !strings.ContainsFunc(path, func(r rune) bool { return r <= 0x20 || r == 0x7f || r == '#' })
}

// settingsTUI: minigw-tui ob tem minigw (izdaja, MSI, install.sh) z istim terminalom; izhodna koda ostane.
func settingsTUI(extra []string) {
	self, err := os.Executable()
	if err == nil {
		self, err = filepath.EvalSymlinks(self)
	}
	if err != nil {
		fail("cannot find this minigw: " + err.Error())
	}
	tui, err := settingsTUIPath(runtime.GOOS, self)
	if err != nil {
		fail(err.Error())
	}
	cmd := consoleCommand(tui, append([]string{"--minigw", self}, extra...)...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err = cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		os.Exit(ee.ExitCode())
	} else if err != nil {
		fail("cannot start " + tui + ": " + err.Error())
	}
	os.Exit(0)
}

// settingsTUIPath (#51): minigw-tui ob minigw; na Linuxu in macOS le v varnih mapah do / in last uporabnika ali roota (kot povezava vtičnika).
func settingsTUIPath(goos, self string) (string, error) {
	tui := filepath.Join(filepath.Dir(self), "minigw-tui")
	if goos == "windows" {
		tui += ".exe"
	}
	if _, err := os.Stat(tui); err != nil {
		return "", errors.New("the settings terminal UI is not installed (" + tui + "): it comes with Mini Gateway 0.5.20 or newer (https://github.com/gh-commiter/mini-llm-gateway/releases/latest); or open the settings page with: minigw gw settings link")
	}
	if goos != "windows" {
		if !safePath(tui) {
			return "", errors.New(tui + " or a folder above it is writable by others or not owned by you or root: not starting it; reinstall Mini Gateway in a safe folder")
		}
	}
	return tui, nil
}

func modelLine(m apiModel) string {
	mark := "[ ]"
	if m.Enabled {
		mark = "[x]"
	}
	w := ""
	if m.Window > 0 {
		w = fmt.Sprintf("  %dK", m.Window/1000)
	}
	return clean(fmt.Sprintf("%s %s  (/model %s)%s", mark, m.ID, m.Model, w))
}

func enabledText(models []apiModel) string {
	n := 0
	for _, m := range models {
		if m.Enabled {
			n++
		}
	}
	return strconv.Itoa(n)
}

// agentWork (#60): minigw gw codex|grok|copilot [--work=…]: omejitev načina agenta prek skrbniškega API.
func agentWork(a *adminClient, kind string, set bool, work string) {
	var c struct {
		Work        string
		Unsupported bool
		Reason      string
	}
	if a.do("GET", "/agents/"+kind, nil, &c); c.Unsupported { // 0.5.24: Copilot na prehodu Windows
		say(clean(c.Reason))
		return
	}
	if set {
		a.do("PATCH", "/agents/"+kind, map[string]any{"work": work}, &c)
	}
	say(clean(agentNames[kind] + " work: " + c.Work))
}

// agentLoginCmd (#68): minigw gw <agent> login | token [remove] | apikey [remove]; ključ s stdin ali vprašan, nikoli v argv.
func agentLoginCmd(a *adminClient, in io.Reader, pos []string) {
	kind := pos[0]
	if len(pos) > 3 || len(pos) == 3 && pos[2] != "remove" || !slices.Contains([]string{"login", "token", "apikey"}, pos[1]) ||
		pos[1] == "token" && !agentHasToken(kind) || pos[1] == "apikey" && agentAPIKeyEnv[kind] == "" || pos[1] == "login" && len(pos) != 2 {
		fail(routerHelp)
	}
	var info struct {
		Login       map[string]any `json:"login"`
		Unsupported bool           `json:"unsupported"`
		Reason      string         `json:"reason"`
	}
	if pos[1] == "login" {
		if a.do("GET", "/agents/"+kind, nil, &info); info.Unsupported { // 0.5.24: Copilot na prehodu Windows
			say(clean(info.Reason))
			return
		}
		say(clean(fmt.Sprintf("%s login: %v", agentNames[kind], info.Login["state"])))
		say(clean(fmt.Sprint(info.Login["hint"])))
		if n, ok := info.Login["apiKeyNote"]; ok {
			say(clean(fmt.Sprint(n)) + " (minigw gw " + kind + " apikey)")
		}
		return
	}
	what := "Claude token (from claude setup-token, sk-ant-…)"
	field := "token"
	if pos[1] == "apikey" {
		what, field = agentNames[kind]+" API key (billed through the provider's API)", "apiKey"
	}
	v := ""
	if len(pos) == 2 {
		t, err := readKey(in, what)
		if err != nil {
			fail(err.Error())
		}
		v = t
	}
	a.do("PATCH", "/agents/"+kind, map[string]any{field: v}, nil)
	if v == "" {
		say("removed " + agentNames[kind] + "'s " + map[string]string{"token": "token", "apiKey": "API key (the login is used again)"}[field])
	} else {
		say(agentNames[kind] + "'s " + map[string]string{"token": "token", "apiKey": "API key"}[field] + " stored in the key store")
	}
}

// scratchOthersLine: »gw-1 (stale owner), gw-2 (used by another gateway)« ali »none«.
func scratchOthersLine(o []scratchSibling) string {
	var l []string
	for _, x := range o {
		l = append(l, x.Name+map[string]string{scratchStale: " (stale owner)", scratchInUse: " (used by another gateway)"}[x.State])
	}
	if len(l) == 0 {
		return "none"
	}
	return strings.Join(l, ", ")
}
