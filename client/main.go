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
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

const version = "0.5.1" // bump-version.sh

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

func deviceName() string {
	if n := os.Getenv("COMPUTERNAME"); n != "" {
		return n
	}
	n, _ := os.Hostname()
	return n
}

var proxy *noiseProxy

// route: oddaljen router → vse gre šifrirano prek lokalnega proxyja; lokalno navaden HTTP.
func route(c connection) connection {
	if c.isLocal() {
		return c
	}
	if c.Key == nil {
		fail("no router key: copy the token again (Mini Gateway → Copy Token) and run minigw connect")
	}
	key, err := fromBase64url(*c.Key)
	if err != nil || len(key) != 32 {
		fail("invalid router key: copy the token again (Mini Gateway → Copy Token)")
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
	first := ""
	if len(args) > 0 {
		first = args[0]
	}
	switch first {
	case "connect":
		if len(args) < 2 {
			fail("usage: minigw connect <host[:port]> [--token '<token>']")
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
		st, _ := getJSON(route(c), "/v1/models")
		switch {
		case st == 200:
			saveConnection(c)
			enc := ""
			if !c.isLocal() {
				enc = " (encrypted)"
			}
			say("Connected to " + c.URL + enc)
		case st == 401:
			fail("router at " + c.URL + " rejected the token")
		case st == 426:
			fail("router at " + c.URL + " needs an encrypted connection: copy the token again (it includes the router key)")
		case st == 0 && c.isLocal():
			fail("cannot reach " + c.URL)
		case st == 0:
			fail("cannot reach " + c.URL + ", or the router key does not match (copy the token again)")
		default:
			fail(fmt.Sprintf("router at %s answered HTTP %d", c.URL, st))
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
		say("Router:    " + saved.URL + " (" + state + ")")
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
			say("minigw connect <host[:port]> [--token T]   save router address and token (" + storeName + ")\n" +
				"minigw status | models | disconnect\n" +
				"minigw [claude options …]                    run Claude Code through the router")
		}
		if first == "--minigw-help" {
			return
		}
		os.Exit(runClaude(args, nil)) // le podatki o claude: brez routerja, seje in deljenja mape
	}

	// Claude Code prek routerja.
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

	cwd, _ := os.Getwd()
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

	share := &shareClient{c: c, session: sid, root: cwd}
	if wantShare && sid != "" {
		if share.register() {
			share.start(4)
			fmt.Fprintf(os.Stderr, "minigw: sharing %s with %s for Claude CLI models with tools (MINIGW_SHARE=0 to turn off)\n", cwd, routerHost)
		} else {
			fmt.Fprintln(os.Stderr, "minigw: router did not accept the folder; Claude CLI models with tools will not see it")
		}
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
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(home, cwd)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return false
	}
	top := strings.SplitN(filepath.ToSlash(rel), "/", 2)[0]
	if strings.HasPrefix(top, ".") {
		return false
	}
	excluded := map[string]bool{"Library": true, "AppData": true}
	if runtime.GOOS == "windows" {
		return !excluded[top] && !strings.EqualFold(top, "AppData")
	}
	return !excluded[top]
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
