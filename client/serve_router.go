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
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const routerHelp = `minigw gw: show or change the minigw serve gateway on this computer. Changes apply at once when it
runs; otherwise they go to serve.json.

  minigw gw status
  minigw gw devices [allow <device id> [--name N] | rename <device id> --name N | deny <device id>]
  minigw gw provider add <name> <baseURL> [--insecure]   the API key is read from stdin or asked for
  minigw gw provider remove <name> | list
  minigw gw provider models <name> [text] [--all] [--enable m1,m2|all] [--disable m1,m2|all]
  minigw gw claude [--path FILE] [--tools T1,T2] [--folders DIR1,DIR2] [--allow-local-tools=true|false]
                  [--allow-unsandboxed-bash=true|false]   (minigw serve only)
  minigw gw autostart on|off                            Windows: start the tray at sign-in (Linux: minigw serve install)
  minigw gw token [rotate]                              print the connect line; rotate makes a new token
  minigw gw copilot token [remove]                      Copilot's token for commands (fine-grained, only Copilot
                                                        Requests), read from stdin or asked for
  minigw gw settings                                    a one-time link (60 s) to the settings page

All take --config FILE (default: the minigw serve config).`

type adminClient struct {
	base, secret string  // tekoči router
	routerKey    []byte  // javni ključ routerja: Noise preveri, da je na vratih res ta router
	off          *server // router ne teče
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
	if err != nil {
		fail("cannot read " + cfgPath + " (run minigw serve init first): " + err.Error())
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
			fmt.Fprintf(os.Stderr, "minigw: port %d answers, but not as the gateway of %s (wrong key): changing that file only; nothing was sent to it\n", c.Port, cfgPath)
		case err != nil: // ne teče
		case status == 200:
			return a
		case status == 401 || status == 403:
			fail(fmt.Sprintf("the gateway on port %d refused the admin request (HTTP %d): is it the gateway of %s?", c.Port, status, cfgPath))
		default:
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
		if status, resp, err = a.http(method, path, data, 60*time.Second); err != nil {
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

func routerCommand(args []string, in io.Reader) {
	fs := flag.NewFlagSet("minigw gw", flag.ExitOnError)
	cfgPath := fs.String("config", defaultServeConfigPath(), "config file")
	insecure := fs.Bool("insecure", false, "provider add: allow http to localhost or a private IP")
	enable := fs.String("enable", "", "provider models: enable these models (comma-separated, or all)")
	disable := fs.String("disable", "", "provider models: disable these models (comma-separated, or all)")
	all := fs.Bool("all", false, "provider models: also list the disabled models")
	devName := fs.String("name", "", "devices allow/rename: the device's name (a label; the ID identifies it)")
	path := fs.String("path", "", "claude: path to claude (empty = PATH)")
	tools := fs.String("tools", "", "claude: the agent's tools")
	bash := fs.String("allow-unsandboxed-bash", "", "claude: true or false")
	folders := fs.String("folders", "", "claude: allowed folders, comma-separated (as on the Mac)")
	localTools := fs.String("allow-local-tools", "", "claude: true or false")
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
		say(map[string]string{"boot": "starts at boot (minigw serve uninstall to stop that)", "sign-in": "starts at sign-in (minigw serve uninstall to stop that)",
			"": "does not start on its own (minigw serve install)"}[st.Autostart])
		say(fmt.Sprintf("agent requests running: %d", st.Agents))
		say(installedInfo(runtime.GOOS))
		say(fmt.Sprintf("devices: %d allowed, %d waiting (minigw gw devices)", st.Devices, st.Pending))
		claude := "not found"
		if st.Claude.Found {
			claude = "found"
		}
		say(clean(fmt.Sprintf("claude: %s, tools %s", claude, st.Claude.Tools)))
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
		case pos[1] == "deny" && len(pos) == 3:
			var r struct {
				Device  apiDevice
				Pending bool
			}
			a.do("DELETE", "/devices/"+esc(pos[2]), nil, &r)
			say(fmt.Sprintf("%s device %s %q", map[bool]string{true: "rejected waiting", false: "removed"}[r.Pending], r.Device.ID, r.Device.Name))
		default:
			fail(routerHelp)
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
		need(1, 1)
		var c apiClaude
		if set["path"] || set["tools"] || set["allow-unsandboxed-bash"] || set["folders"] || set["allow-local-tools"] {
			body := map[string]any{}
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
			a.do("PATCH", "/agents/claude", body, &c)
		} else {
			a.do("GET", "/agents/claude", nil, &c)
		}
		path := c.Path
		if path == "" {
			path = "(from PATH)"
		}
		say(clean(fmt.Sprintf("path: %s, found: %v\ntools: %s\nallowLocalTools: %v\nfolders: %s\nallowUnsandboxedBash: %v",
			path, c.Found, c.Tools, c.AllowLocalTools, c.Folders, c.AllowUnsandboxedBash)))

	case "autostart": // kot na Macu; Linux: API pove, naj se uporabi minigw serve install
		need(2, 2)
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
			a.do("POST", "/general/token", nil, &g)
			say("New token. Each client must run the new line; the old token no longer works:")
		} else {
			a.do("GET", "/general", nil, &g)
			say("On each client run:")
		}
		say("  " + g.Connect)
	case "copilot":
		need(2, 3)
		if pos[1] != "token" || (len(pos) == 3 && pos[2] != "remove") {
			fail(routerHelp)
		}
		token := ""
		if len(pos) == 2 {
			t, err := readKey(in, "Copilot token (github_pat_…, only the Copilot Requests permission)")
			if err != nil {
				fail(err.Error())
			}
			token = t
		}
		a.do("PATCH", "/agents/copilot", map[string]any{"token": token}, nil)
		if token == "" {
			say("removed Copilot's token")
		} else {
			say("Copilot's token stored in the key store")
		}
	case "settings":
		need(1, 1)
		var r struct{ URL string }
		a.do("POST", "/settings/login", nil, &r)
		say("Open within 60 seconds (once), in a browser on this computer:\n  " + r.URL)
	default:
		fail("unknown command minigw gw " + pos[0] + "\n\n" + routerHelp)
	}
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
