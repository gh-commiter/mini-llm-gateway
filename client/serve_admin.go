package main

// Skrbniški API (docs/admin-api.md): /minigw/admin/v1/…, le z loopback, Host 127.0.0.1/localhost in skrivnostjo iz
// admin.secret. adminDo služi HTTP in minigw router brez tekočega routerja (spremeni le serve.json), zato ena koda.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

const adminPrefix = "/minigw/admin/v1"

type apiDevice struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Address  string `json:"address,omitempty"` // le pri čakajočih
	Client   string `json:"client,omitempty"`  // OS in različica minigw (glava X-Minigw-Client), pri dovoljenih od zagona
	Time     string `json:"time,omitempty"`
	LastSeen string `json:"lastSeen,omitempty"` // le pri dovoljenih, od zagona
}

type apiModel struct {
	ID      string `json:"id"`    // pri upstreamu
	Model   string `json:"model"` // ime za /model
	Window  int    `json:"window,omitempty"`
	Enabled bool   `json:"enabled"`
}

type apiGateway struct {
	Name     string   `json:"name"`
	BaseURL  string   `json:"baseURL"`
	Insecure bool     `json:"insecure"`
	Models   []string `json:"models"` // null = vsi odkriti
	HasKey   bool     `json:"hasKey"`
	Ready    bool     `json:"ready"` // seznam modelov naložen (le pri tekočem routerju)
	Enabled  bool     `json:"enabled"`
	Aliases  string   `json:"aliases"`
}

type apiClaude struct {
	Path                 string         `json:"path"` // prazno = iz PATH
	Found                bool           `json:"found"`
	Tools                string         `json:"tools"`
	AllowUnsandboxedBash bool           `json:"allowUnsandboxedBash"`
	Exe                  string         `json:"exe,omitempty"`     // najdena pot (tudi pri samodejnem iskanju)
	ClaudeVersion        string         `json:"version,omitempty"` // claude --version
	Enabled              bool           `json:"enabled"`
	Folders              string         `json:"folders"` // dovoljene mape, ločene z vejico (kot na Macu)
	AllowLocalTools      bool           `json:"allowLocalTools"`
	AskOthers            bool           `json:"askOthers"`
	DisabledTools        []string       `json:"disabledTools"`
	PermissionMode       string         `json:"permissionMode"`
	SettingSources       string         `json:"settingSources"`
	Work                 string         `json:"work"`              // #45: direct, sync ali worktree
	Off                  string         `json:"off,omitempty"`     // zakaj sta Claude CLI in Codex izklopljena ob zagonu (offReason)
	LeftOut              []string       `json:"leftOut,omitempty"` // mape iz serve.json, izpuščene ob zagonu (offFolders)
	Login                map[string]any `json:"login"`             // #68: vrstica Login (stanje žetona setup-token / ključa API, nikoli vrednost)
}

type apiGeneral struct {
	Port             int    `json:"port"`
	Address          string `json:"address"`
	Connect          string `json:"connect,omitempty"` // le GET (žeton prehoda); PATCH in POST ga ne vrneta
	RestartRequired  bool   `json:"restartRequired,omitempty"`
	AllowedClients   string `json:"allowedClients"`
	AllowPlainRemote bool   `json:"allowPlainRemote"`
	AllowAnyDevice   bool   `json:"allowAnyDevice"`
	AllowLocalApps   bool   `json:"allowLocalApps"`
	Scratch          string `json:"scratch"`
	WorktreeNetwork  bool   `json:"worktreeNetwork"`          // #68: »Network for builds« (Claude CLI v načinu worktree), kot na Macu
	ScratchSuggest   string `json:"scratchSuggest,omitempty"` // Scratch ni izbran (v domači mapi, Windows): predlagana mapa za premik
	Warning          string `json:"warning,omitempty"`        // le odgovor premika: stara mapa ni odstranjena
}

type apiStatus struct {
	Running    bool         `json:"running"`
	Version    string       `json:"version"`
	Build      string       `json:"build,omitempty"` // commit gradnje (vcs.revision)
	Port       int          `json:"port"`
	Address    string       `json:"address"`
	Installed  bool         `json:"installed"`
	Devices    int          `json:"devices"`
	Pending    int          `json:"pending"`
	Claude     apiClaude    `json:"claude"`
	Providers  []apiGateway `json:"providers"`
	Gateways   []apiGateway `json:"gateways"`             // starejše ime za providers (do 0.5.14)
	OS         string       `json:"os"`                   // windows, linux, darwin (stran: ukazi brez sandboxa)
	InstallDir string       `json:"installDir,omitempty"` // minigw serve install
	Tray       bool         `json:"tray"`                 // pladenj teče (Windows)
	Agents     int          `json:"agents"`               // tekoči procesi agentov (claude, codex, grok)
	Autostart  string       `json:"autostart"`            // "boot", "sign-in" ali "" (minigw serve install)
	// AgentsUnsandboxed: Codex in Grok tu nimata sandboxa, ki skrije skrivnosti (Windows, Linux brez bwrap).
	AgentsUnsandboxed bool `json:"agentsUnsandboxed"`
	// AgentSandbox (#68, Windows): {"kind":"mxc","version":"1.0.1"} ali {"kind":"","reason":"…"}; drugje ga ni.
	AgentSandbox map[string]string `json:"agentSandbox,omitempty"`
}

// adminSecret: 32 naključnih bajtov (base64url) v admin.secret (0600) v mapi nastavitev, ki je agentom prepovedana.
func adminSecret(cfgDir string) (string, error) {
	p := filepath.Join(cfgDir, "admin.secret")
	for range 20 { // drug proces jo morda prav zdaj ustvarja: počakaj, da je zapisana
		b, err := os.ReadFile(p)
		if err == nil {
			if s := strings.TrimSpace(string(b)); len(s) >= 43 {
				return s, nil
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		r := make([]byte, 32)
		rand.Read(r)
		os.MkdirAll(cfgDir, 0o700)
		f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if errors.Is(err, os.ErrExist) {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		if err != nil {
			return "", err
		}
		_, err = f.WriteString(base64url(r) + "\n")
		if err == nil {
			err = f.Sync()
		}
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return "", err
		}
		return base64url(r), nil
	}
	return "", errors.New(p + " is invalid: delete it, minigw serve creates a new one")
}

// refuseAgentPeer: 403, če zahtevo pošlje proces agenta tega routerja (ali tega ni mogoče ugotoviti).
func (s *server) refuseAgentPeer(w http.ResponseWriter, r *req) int {
	if err := adminPeerCheck(r.conn); err != nil {
		s.log.Printf("%s admin API refused: %v", r.rid, err)
		return writeError(w, r.rid, 403, "Admin API refused: "+err.Error())
	}
	return 0
}

// adminHTTP: odjemalec na tem računalniku (naslov TCP, ne glave), Host za DNS rebinding, skrivnost; brez CORS.
// Loopback sicer preskoči preverjanje naprave; skrbniški API kljub temu zahteva skrivnost.
func (s *server) adminHTTP(w http.ResponseWriter, r *req) int {
	w.Header().Set("Cache-Control", "no-store")
	if st := localOnly(w, r); st != 0 {
		return st
	}
	switch auth := r.h("authorization"); {
	case strings.HasPrefix(auth, adminScheme): // CLI: dokaz skrivnosti, vezan na to povezavo Noise
		if r.conn.sess == nil || s.admin == "" ||
			subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(auth, adminScheme)), []byte(adminProof(s.admin, r.conn.sess.hash))) != 1 {
			return writeError(w, r.rid, 401, "Invalid admin proof: use minigw gw (an encrypted connection with the gateway key)")
		}
	case auth != "": // npr. Bearer: skrivnost ne sme nikoli po žici
		return writeError(w, r.rid, 401, "The admin API takes MGW-Admin over an encrypted connection (minigw gw), never the secret itself")
	case !s.settings.valid(r.h(settingsHeader)): // stran z nastavitvami: seja v glavi (brez piškotkov)
		return writeError(w, r.rid, 401, "Invalid or missing admin secret (admin.secret in the config folder)")
	}
	if st := s.refuseAgentPeer(w, r); st != 0 { // H1, R4: agent tega routerja ne sme spreminjati routerja
		return st
	}

	if !readBody(w, r, 1<<20) {
		return writeError(w, r.rid, 413, "Request body too large or too slow")
	}
	status, obj := s.adminDo(r.Method, strings.TrimPrefix(r.URL.RequestURI(), adminPrefix), r.body)
	return writeJSON(w, r.rid, status, obj)
}

// adminScheme in adminProof: odjemalec ne pošlje skrivnosti, le HMAC-SHA256(skrivnost, "minigw-admin-v1" || h), kjer
// je h zgoščena vrednost rokovanja Noise te povezave. Lažen strežnik (svoj ključ, svoje rokovanje) dobi dokaz, ki ne
// velja nikjer drugje.
const adminScheme = "MGW-Admin "

func adminProof(secret string, h []byte) string {
	return base64url(hmacSHA256([]byte(secret), append([]byte("minigw-admin-v1"), h...)))
}

// localOnly: odjemalec na tem računalniku (naslov TCP, ne glave) in Host 127.0.0.1/localhost s temi vrati (DNS
// rebinding). 0 = v redu, sicer je odgovor že poslan.
func localOnly(w http.ResponseWriter, r *req) int {
	if !r.conn.loopback() {
		return writeError(w, r.rid, 403, "The admin API is only available on the gateway computer")
	}
	_, port, _ := net.SplitHostPort(r.conn.LocalAddr().String())
	if !slices.Contains([]string{"127.0.0.1:" + port, "localhost:" + port, "[::1]:" + port}, strings.ToLower(r.Host)) {
		return writeError(w, r.rid, 403, "Host must be 127.0.0.1:"+port+" or localhost:"+port)
	}
	return 0
}

// offlineServer: router ne teče; isti API spreminja le serve.json in shrambo ključev.
func offlineServer(cfgPath string) (*server, error) {
	c, err := loadServeConfig(cfgPath)
	if err != nil {
		return nil, err
	}
	cfgDir, _ := filepath.Abs(filepath.Dir(cfgPath))
	exe, _ := resolveClaude(c.Claude)
	return &server{cfg: c, cfgPath: cfgPath, cfgDir: cfgDir, claude: exe, offline: true, log: log.New(io.Discard, "", 0),
		state: loadState(filepath.Join(cfgDir, "state.json"))}, nil
}

// deviceSeen: zadnji stik in priklopi dovoljene naprave (state.json); ime, če ga naprava (dodana le z ID-jem ali s
// starejšim minigw) še nima.
func (s *server) deviceSeen(d serveDevice, addr, client string) {
	s.state.seen(d.ID, addr, client, time.Now())
	if d.Name == "" {
		return
	}
	s.mu.RLock()
	missing := slices.ContainsFunc(s.cfg.Devices, func(x serveDevice) bool { return x.ID == d.ID && x.Name == "" })
	s.mu.RUnlock()
	if missing {
		s.update(func(c *serveConfig) error {
			if i := slices.IndexFunc(c.Devices, func(x serveDevice) bool { return x.ID == d.ID && x.Name == "" }); i >= 0 {
				c.Devices[i].Name = d.Name
			}
			return nil
		})
	}
}

// cleanDeviceName: ime naprave (ali podatek odjemalca) za prikaz: brez kontrolnih in oblikovnih znakov, obrezano, največ
// 64 znakov (M1, kot Noise.cleanName na Macu). Ime je le oznaka; identiteta je ključ naprave.
func cleanDeviceName(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf, unicode.Zl, unicode.Zp) { // L2: tudi U+2028, U+2029
			return -1
		}
		return r
	}, strings.ToValidUTF8(s, ""))
	r := []rune(strings.TrimSpace(s))
	return strings.TrimSpace(string(r[:min(len(r), 64)]))
}

var errNoDevice = errors.New("no such device")

// deviceRefusal: isto besedilo kot na Macu.
func deviceRefusal(d serveDevice) string {
	return fmt.Sprintf("Device %q is not allowed on this gateway: allow it in Settings → Devices (compare the ID %s), or run minigw gw devices allow %s", d.Name, d.ID, d.ID)
}

// addPending: največ 20; poln seznam obdrži najstarejše (novi ključi jih ne izrinejo, L1); ista naprava se osveži na mestu.
func (s *server) addPending(d serveDevice, addr, client string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC().Format(time.RFC3339)
	if i := slices.IndexFunc(s.pending, func(p apiDevice) bool { return p.ID == d.ID }); i >= 0 {
		if client == "" {
			client = s.pending[i].Client
		}
		s.pending[i] = apiDevice{ID: d.ID, Name: d.Name, Address: addr, Client: client, Time: now}
		return
	}
	if len(s.pending) < 20 {
		s.pending = append(s.pending, apiDevice{ID: d.ID, Name: d.Name, Address: addr, Client: client, Time: now})
	}
}

var validTools = regexp.MustCompile(`^[A-Za-z0-9_]+(,[A-Za-z0-9_]+)*$`)

func decodeStrict(body []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(body))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return errors.New("bad JSON body: " + err.Error())
	}
	if d.Decode(&struct{}{}) != io.EOF {
		return errors.New("bad JSON body: data after the JSON object")
	}
	return nil
}

// adminDo: metoda, pot pod /minigw/admin/v1 (z ?poizvedbo) in telo → status in JSON.
func (s *server) adminDo(method, uri string, body []byte) (int, any) {
	fail := func(status int, msg string) (int, any) { return status, errorBody(status, msg) }
	u, err := url.Parse(uri)
	if err != nil {
		return fail(400, "bad path")
	}
	var seg []string
	for _, p := range strings.Split(strings.Trim(u.EscapedPath(), "/"), "/") {
		v, err := url.PathUnescape(p)
		if err != nil {
			return fail(400, "bad path")
		}
		seg = append(seg, v)
	}
	if seg[0] == "gateways" { // starejše ime (do 0.5.14), ostane kot alias
		seg[0] = "providers"
	}
	route, arg := method+" "+seg[0], ""
	switch len(seg) {
	case 1:
	case 2:
		route, arg = route+"/{}", seg[1]
	case 3:
		route, arg = route+"/{}/"+seg[2], seg[1]
	default:
		return fail(404, "Not found: "+method+" "+uri)
	}
	switch route {
	case "GET status":
		c := s.conf()
		st := apiStatus{Running: !s.offline, Version: version, Port: c.Port, Address: c.Address, Installed: serviceInstalled(runtime.GOOS),
			Devices: len(c.Devices), Claude: s.claudeInfo(), Gateways: s.apiGateways(c), OS: runtime.GOOS, Tray: trayRunning(), Build: buildCommit(),
			Agents: int(s.agentsRunning.Load()), Autostart: autostart(runtime.GOOS), AgentsUnsandboxed: !agentSandboxed(s.cfgDir),
			AgentSandbox: agentSandboxInfo()}
		if d := installDir(runtime.GOOS); d != "" { // le če je minigw tam res nameščen
			if _, err := os.Stat(filepath.Join(d, map[bool]string{true: "minigw.exe", false: "minigw"}[runtime.GOOS == "windows"])); err == nil {
				st.InstallDir = d
			}
		}
		s.mu.RLock()
		st.Providers = st.Gateways
		st.Pending = len(s.pending)
		s.mu.RUnlock()
		return 200, st

	case "GET devices/{}/history": // priklopi dovoljene naprave, najnovejši prvi
		i := slices.IndexFunc(s.conf().Devices, func(d serveDevice) bool { return d.ID == arg })
		if i < 0 {
			return fail(404, "no allowed device with the ID "+arg+" (minigw gw devices lists the IDs)")
		}
		st, _ := s.state.device(arg)
		h := slices.Clone(st.History)
		slices.Reverse(h)
		if h == nil {
			h = []deviceConn{}
		}
		return 200, map[string]any{"id": arg, "name": s.conf().Devices[i].Name, "history": h}
	case "GET activity":
		return 200, s.activity.list()
	case "GET sessions":
		return 200, map[string]any{"sessions": s.state.sessions(u.Query().Get("device"), u.Query().Get("folder"))}
	case "GET sessions/{}":
		for _, x := range s.state.sessions("", "") {
			if x.ID == arg {
				return 200, x
			}
		}
		return fail(404, "no session "+arg+" (minigw gw sessions lists them)")
	case "GET usage": // #46b: števci po projektu, najnovejši dan prvi
		q := u.Query()
		days := 7
		if v := q.Get("days"); v != "" {
			if n, err := strconv.Atoi(v); err != nil || n < 1 || n > projectDays {
				return fail(400, "days: 1-31")
			} else {
				days = n
			}
		}
		return 200, map[string]any{"usage": s.state.usage(q.Get("device"), q.Get("folder"), days, time.Now())}
	case "DELETE sessions":
		s.state.clearSessions()
		s.state.save()
		return 200, map[string]any{"cleared": true}
	case "GET devices":
		c := s.conf()
		out := map[string]any{"devices": []apiDevice{}, "allowAnyDevice": c.AllowAnyDevice}
		var list []apiDevice
		for _, d := range c.Devices {
			x := apiDevice{ID: d.ID, Name: d.Name}
			if v, ok := s.state.device(d.ID); ok {
				x.LastSeen, x.Client = v.LastSeen.UTC().Format(time.RFC3339), v.Client
			}
			list = append(list, x)
		}
		s.mu.RLock()
		pending := slices.Clone(s.pending)
		s.mu.RUnlock()
		out["devices"], out["pending"] = append([]apiDevice{}, list...), append([]apiDevice{}, pending...)
		return 200, out
	case "POST devices":
		var d apiDevice
		if err := decodeStrict(body, &d); err != nil {
			return fail(400, err.Error())
		}
		if !validDeviceID(d.ID) {
			return fail(400, strconv.Quote(d.ID)+" is not a device ID (43 characters, base64url): copy it from the gateway log or from the client's refusal message")
		}
		added := false
		d.Name = cleanDeviceName(d.Name)
		err := s.update(func(c *serveConfig) error {
			if j := slices.IndexFunc(s.pending, func(p apiDevice) bool { return p.ID == d.ID }); j >= 0 && d.Name == "" {
				d.Name = s.pending[j].Name // »allow <id>« brez imena: ime iz čakajoče naprave (kot na Macu)
			}
			s.pending = slices.DeleteFunc(s.pending, func(p apiDevice) bool { return p.ID == d.ID })
			if slices.ContainsFunc(c.Devices, func(x serveDevice) bool { return x.ID == d.ID }) {
				return nil
			}
			added = true
			c.Devices = append(c.Devices, serveDevice{ID: d.ID, Name: d.Name})
			return nil
		})
		if err != nil {
			return fail(500, err.Error())
		}
		return 200, map[string]any{"device": apiDevice{ID: d.ID, Name: d.Name}, "added": added}
	case "DELETE devices/{}":
		s.mu.Lock() // čakajoča naprava (zavrni): le iz seznama čakajočih
		if j := slices.IndexFunc(s.pending, func(p apiDevice) bool { return p.ID == arg }); j >= 0 &&
			!slices.ContainsFunc(s.cfg.Devices, func(d serveDevice) bool { return d.ID == arg }) {
			d := s.pending[j]
			s.pending = slices.Delete(s.pending, j, j+1)
			s.mu.Unlock()
			return 200, map[string]any{"device": apiDevice{ID: d.ID, Name: d.Name}, "pending": true}
		}
		s.mu.Unlock()
		var gone serveDevice
		err := s.update(func(c *serveConfig) error {
			i := slices.IndexFunc(c.Devices, func(d serveDevice) bool { return d.ID == arg }) // le po ID-ju: ime je oznaka
			if i < 0 {
				return errNoDevice
			}
			gone = c.Devices[i]
			c.Devices = slices.Delete(c.Devices, i, i+1)
			return nil
		})
		if err != nil {
			return fail(404, "no allowed or waiting device with the ID "+arg+" (minigw gw devices lists the IDs)")
		}
		return 200, map[string]any{"device": apiDevice{ID: gone.ID, Name: gone.Name}}
	case "PATCH devices/{}": // preimenovanje dovoljene naprave (ime je le oznaka)
		var b struct {
			Name string `json:"name"`
		}
		if err := decodeStrict(body, &b); err != nil {
			return fail(400, err.Error())
		}
		name := cleanDeviceName(b.Name)
		if name == "" {
			return fail(400, "name: 1-64 printable characters")
		}
		err := s.update(func(c *serveConfig) error {
			i := slices.IndexFunc(c.Devices, func(d serveDevice) bool { return d.ID == arg })
			if i < 0 {
				return errNoDevice
			}
			c.Devices[i].Name = name
			return nil
		})
		if err != nil {
			return fail(404, "no allowed device with the ID "+arg)
		}
		return 200, map[string]any{"device": apiDevice{ID: arg, Name: name}}

	case "GET providers":
		list := s.apiGateways(s.conf())
		return 200, map[string]any{"providers": list, "gateways": list} // gateways: starejše ime
	case "POST providers":
		var b struct {
			Name     string `json:"name"`
			BaseURL  string `json:"baseURL"`
			Key      string `json:"key"`
			Insecure bool   `json:"insecure"`
		}
		if err := decodeStrict(body, &b); err != nil {
			return fail(400, err.Error())
		}
		if !validGatewayName.MatchString(b.Name) {
			return fail(400, "provider name: letters, digits, . _ - (up to 32)")
		}
		base, err := checkGatewayURL(b.BaseURL, b.Insecure)
		if err != nil {
			return fail(400, err.Error())
		}
		if b.Key == "" || strings.ContainsFunc(b.Key, func(r rune) bool { return r < 0x21 || r > 0x7e }) {
			return fail(400, "the API key must be printable ASCII without spaces")
		}
		g := serveGateway{Name: b.Name, BaseURL: base, Insecure: b.Insecure}
		old, exists := findGateway(s.conf(), b.Name)
		if exists {
			g.Models = old.Models
		}
		models, err := discoverGateway(g, b.Key)
		if err != nil {
			return fail(400, "provider not added: "+err.Error())
		}
		if !exists && len(models) > 20 { // OpenRouter: več sto modelov, izbereš sam (kot na Macu)
			g.Models = []string{}
		}
		oldKey, _ := keyGet(s.cfgDir, b.Name)
		where, err := keySet(s.cfgDir, b.Name, b.Key)
		if err != nil {
			return fail(500, "cannot store the API key: "+err.Error())
		}
		if strings.HasPrefix(where, "WARNING") {
			s.log.Printf("provider %s: API key: %s", b.Name, where)
		}
		err = s.update(func(c *serveConfig) error {
			if i := slices.IndexFunc(c.Gateways, func(x serveGateway) bool { return x.Name == g.Name }); i >= 0 {
				c.Gateways[i] = g
			} else {
				c.Gateways = append(c.Gateways, g)
			}
			return nil
		})
		if err != nil { // ključ nazaj, kot je bil
			if oldKey != "" {
				keySet(s.cfgDir, b.Name, oldKey)
			} else {
				keyDel(s.cfgDir, b.Name)
			}
			return fail(500, err.Error())
		}
		s.gateways.forget(g.Name)
		return 200, map[string]any{"provider": s.apiGateway(g), "gateway": s.apiGateway(g), "keyStore": where, "models": apiModels(g, models, "")}
	case "PATCH providers/{}": // ime, vklop, URL (ključ ostane), aliasi; kot polja prehoda v nastavitvah na Macu
		var b struct {
			Name     *string `json:"name"`
			Enabled  *bool   `json:"enabled"`
			BaseURL  *string `json:"baseURL"`
			Insecure *bool   `json:"insecure"`
			Aliases  *string `json:"aliases"`
		}
		if err := decodeStrict(body, &b); err != nil {
			return fail(400, err.Error())
		}
		g, ok := findGateway(s.conf(), arg)
		if !ok {
			return fail(404, "no provider "+arg)
		}
		if b.Insecure != nil {
			g.Insecure = *b.Insecure
		}
		if b.BaseURL != nil || b.Insecure != nil {
			u := g.BaseURL
			if b.BaseURL != nil {
				u = *b.BaseURL
			}
			base, err := checkGatewayURL(u, g.Insecure)
			if err != nil {
				return fail(400, err.Error())
			}
			g.BaseURL = base
		}
		if b.Enabled != nil {
			g.Disabled = !*b.Enabled
		}
		if b.Aliases != nil {
			if len(*b.Aliases) > 64<<10 {
				return fail(400, "aliases: too long")
			}
			g.Aliases = *b.Aliases
		}
		if b.Name != nil && *b.Name != arg {
			if builtinGateway(arg) || builtinGateway(*b.Name) {
				return fail(400, "LiteLLM and OpenRouter are built in: they keep their names")
			}
			if !validGatewayName.MatchString(*b.Name) {
				return fail(400, "provider name: letters, digits, . _ - (up to 32)")
			}
			g.Name = *b.Name
		}
		// L2: zasedeno ime (brez razlike v velikih črkah) se preveri v isti spremembi, pred premikom ključa.
		errTaken := errors.New("a provider named " + g.Name + " already exists")
		var old serveGateway
		if err := s.update(func(c *serveConfig) error {
			i := slices.IndexFunc(c.Gateways, func(x serveGateway) bool { return x.Name == arg })
			if i < 0 {
				return errors.New("no provider " + arg)
			}
			if slices.ContainsFunc(c.Gateways, func(x serveGateway) bool { return x.Name != arg && strings.EqualFold(x.Name, g.Name) }) {
				return errTaken
			}
			old = c.Gateways[i]
			g.Models = slices.Clone(c.Gateways[i].Models)
			c.Gateways[i] = g
			return nil
		}); err != nil {
			if errors.Is(err, errTaken) {
				return fail(400, err.Error())
			}
			return fail(500, err.Error())
		}
		if g.Name != arg { // ključ gre pod novo ime; ob napaki ime nazaj
			if key, _ := keyGet(s.cfgDir, arg); key != "" {
				if _, err := keySet(s.cfgDir, g.Name, key); err != nil {
					s.update(func(c *serveConfig) error {
						if i := slices.IndexFunc(c.Gateways, func(x serveGateway) bool { return x.Name == g.Name }); i >= 0 {
							c.Gateways[i] = old
						}
						return nil
					})
					return fail(500, "cannot move the API key: "+err.Error())
				}
			}
			keyDel(s.cfgDir, arg)
		}
		s.gateways.forget(arg)
		s.gateways.forget(g.Name)
		return 200, map[string]any{"provider": s.apiGateway(g), "gateway": s.apiGateway(g)}
	case "DELETE providers/{}":
		if builtinGateway(arg) {
			return fail(400, arg+" is built in: turn it off instead")
		}
		err := s.update(func(c *serveConfig) error {
			i := slices.IndexFunc(c.Gateways, func(x serveGateway) bool { return x.Name == arg })
			if i < 0 {
				return errors.New("no provider " + arg)
			}
			c.Gateways = slices.Delete(c.Gateways, i, i+1)
			return nil
		})
		if err != nil {
			return fail(404, err.Error())
		}
		keyDel(s.cfgDir, arg)
		s.gateways.forget(arg)
		return 200, map[string]any{"name": arg}
	case "GET providers/{}/models", "POST providers/{}/models":
		g, ok := findGateway(s.conf(), arg)
		if !ok {
			return fail(404, "no provider "+arg)
		}
		key, _ := keyGet(s.cfgDir, g.Name)
		models, err := discoverGateway(g, key)
		if err != nil {
			return fail(502, err.Error())
		}
		if method == "GET" {
			s.gateways.forget(g.Name) // osvežitev (gumb ↻): router znova naloži seznam
			return 200, map[string]any{"models": apiModels(g, models, u.Query().Get("filter"))}
		}
		var b struct {
			All     *bool    `json:"all"` // true: vsi (tudi prihodnji); false: nobeden; nato enable in disable
			Enable  []string `json:"enable"`
			Disable []string `json:"disable"`
		}
		if err := decodeStrict(body, &b); err != nil {
			return fail(400, err.Error())
		}
		upstream := func(v string) (string, bool) {
			j := slices.IndexFunc(models, func(m gwModel) bool { return m.id == v || publishedID(m.id) == v })
			if j < 0 {
				return "", false
			}
			return models[j].id, true
		}
		before := g
		switch {
		case b.All == nil:
		case *b.All:
			g.Models = nil
		default:
			g.Models = []string{}
		}
		for _, v := range b.Enable {
			id, ok := upstream(v)
			if !ok {
				return fail(400, "provider "+g.Name+" has no model "+v)
			}
			if !g.enabled(id) {
				g.Models = append(slices.Clone(g.Models), id)
			}
		}
		if len(b.Disable) > 0 {
			var off []string
			for _, v := range b.Disable {
				id, ok := upstream(v)
				if !ok {
					return fail(400, "provider "+g.Name+" has no model "+v)
				}
				off = append(off, id)
			}
			keep := []string{}
			for _, m := range models {
				if g.enabled(m.id) && !slices.Contains(off, m.id) {
					keep = append(keep, m.id)
				}
			}
			g.Models = keep
		}
		err = s.update(func(c *serveConfig) error {
			i := slices.IndexFunc(c.Gateways, func(x serveGateway) bool { return x.Name == g.Name })
			if i < 0 {
				return errors.New("no provider " + g.Name)
			}
			c.Gateways[i].Models = slices.Clone(g.Models)
			return nil
		})
		if err != nil {
			return fail(500, err.Error())
		}
		var changed []apiModel
		for _, m := range apiModels(g, models, "") {
			if before.enabled(m.ID) != m.Enabled {
				changed = append(changed, m)
			}
		}
		return 200, map[string]any{"models": apiModels(g, models, ""), "changed": append([]apiModel{}, changed...)}

	case "GET agents/{}/tools":
		if arg != "claude" {
			return fail(404, "no agent "+arg)
		}
		return 200, map[string]any{"tools": claudeTools(s.claudeExe(), s.conf().scratch("tmp"))}
	case "GET agents/{}/models": // Codex, Grok: modeli računa za tabelo v nastavitvah
		k, ok := map[string]agentKind{"codex": codexKind, "grok": grokKind}[arg]
		if !ok {
			return fail(404, "no agent "+arg)
		}
		ac := k.conf(s.conf())
		exe, err := resolveAgent(k, ac.Executable)
		if err != nil || exe == "" {
			return 200, map[string]any{"models": []agentModel{}}
		}
		models := agentCatalog(k, exe, s.conf().scratch("tmp"))
		for i := range models {
			models[i].Enabled = ac.modelEnabled(models[i].Slug)
		}
		return 200, map[string]any{"models": append([]agentModel{}, models...)}
	case "GET agents/{}", "PATCH agents/{}":
		if k, ok := map[string]agentKind{"codex": codexKind, "grok": grokKind, "copilot": copilotKind}[arg]; ok {
			return s.agentAPI(method, k, body)
		}
		if arg != "claude" {
			return fail(404, "no agent "+arg)
		}
		if method == "GET" {
			return 200, s.claudeInfo()
		}
		var b struct {
			Path                 *string   `json:"path"`
			Tools                *string   `json:"tools"`
			AllowUnsandboxedBash *bool     `json:"allowUnsandboxedBash"`
			Enabled              *bool     `json:"enabled"`
			Folders              *string   `json:"folders"`
			AllowLocalTools      *bool     `json:"allowLocalTools"`
			AskOthers            *bool     `json:"askOthers"`
			DisabledTools        *[]string `json:"disabledTools"`
			PermissionMode       *string   `json:"permissionMode"`
			SettingSources       *string   `json:"settingSources"`
			Work                 *string   `json:"work"`
			Token                *string   `json:"token"`  // #68: claude setup-token (le za pisanje; "" izbriše)
			APIKey               *string   `json:"apiKey"` // #68: izbira »Use an API key instead« ("" = nazaj na prijavo)
		}
		if err := decodeStrict(body, &b); err != nil {
			return fail(400, err.Error())
		}
		if b.Work != nil && !validWork(*b.Work) {
			return fail(400, badWork)
		}
		if st, e := s.patchLogin("claude", b.Token, b.APIKey); st != 0 {
			return st, e
		}
		if b.Tools != nil {
			if *b.Tools = strings.ReplaceAll(*b.Tools, " ", ""); !validTools.MatchString(*b.Tools) {
				return fail(400, "tools: comma-separated tool names, e.g. "+defaultTools)
			}
		}
		s.mu.RLock()
		left := s.offFolders
		s.mu.RUnlock()
		if b.Folders == nil && len(left) > 0 { // vsaka sprememba brez map: sicer »serve.json differs« (mapa je še v datoteki)
			return fail(400, "first save the allowed folders without "+strings.Join(left, ", ")+" (settings page → Claude CLI → Allowed folders, or minigw gw claude --folders …)")
		}
		var folders []string
		if b.Folders != nil {
			for _, f := range strings.Split(*b.Folders, ",") {
				if f = strings.TrimSpace(f); f != "" {
					folders = append(folders, f)
				}
			}
		}
		var exe []string
		err := s.update(func(c *serveConfig) error {
			if b.Path != nil {
				c.Claude = *b.Path
			}
			if b.Tools != nil {
				c.Tools = *b.Tools
			}
			if b.AllowUnsandboxedBash != nil {
				c.AllowUnsandboxedBash = *b.AllowUnsandboxedBash
			}
			if b.Enabled != nil {
				c.ClaudeDisabled = !*b.Enabled
			}
			if b.Folders != nil {
				c.AllowedFolders = slices.Clone(folders)
			}
			if b.AllowLocalTools != nil {
				c.AllowLocalTools = *b.AllowLocalTools
			}
			if b.AskOthers != nil {
				c.ClaudeAskOthers = *b.AskOthers
			}
			if b.DisabledTools != nil {
				c.DisabledTools = slices.Clone(*b.DisabledTools)
			}
			if b.PermissionMode != nil {
				c.PermissionMode = *b.PermissionMode
			}
			if b.SettingSources != nil {
				c.SettingSources = strings.ReplaceAll(*b.SettingSources, " ", "")
			}
			if b.Work != nil {
				c.ClaudeWork = storedWork(*b.Work)
			}
			e, err := checkAgent(c, s.cfgDir)
			exe = e
			return err
		}, func() {
			s.claude = exe
			if b.Enabled != nil && *b.Enabled { // ponovni vklop uspel: razlog izklopa ob zagonu ne velja več
				s.offReason = ""
			}
			if b.Folders != nil { // shranjen seznam je brez izpuščenih map (tudi v serve.json)
				s.offFolders = nil
			}
		})
		if err != nil {
			return fail(400, err.Error())
		}
		return 200, s.claudeInfo()

	case "GET foundry", "PATCH foundry":
		return s.foundryAPI(method, body)
	case "GET foundry/{}":
		if arg != "models" {
			return fail(404, "Not found: "+method+" "+uri)
		}
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		models, err := s.foundryModels(ctx)
		if err != nil {
			return fail(503, err.Error())
		}
		fc := s.conf().Foundry
		out := []map[string]any{}
		for _, m := range models {
			out = append(out, map[string]any{"name": m.Name, "vision": m.Vision, "enabled": fc.modelEnabled(m.Name)})
		}
		return 200, map[string]any{"models": out}
	case "GET general":
		return 200, s.apiGeneral(false)
	case "PATCH general":
		var b struct {
			Port             *int    `json:"port"`
			Address          *string `json:"address"`
			StartAtSignIn    *bool   `json:"startAtSignIn"`
			AllowedClients   *string `json:"allowedClients"`
			AllowPlainRemote *bool   `json:"allowPlainRemote"`
			AllowAnyDevice   *bool   `json:"allowAnyDevice"`
			AllowLocalApps   *bool   `json:"allowLocalApps"`
			Scratch          *string `json:"scratch"`
			WorktreeNetwork  *bool   `json:"worktreeNetwork"`
		}
		if err := decodeStrict(body, &b); err != nil {
			return fail(400, err.Error())
		}
		if b.Port != nil && (*b.Port < 1 || *b.Port > 65535) {
			return fail(400, "port must be 1-65535")
		}
		if b.Address != nil && !validAddress(*b.Address) {
			return fail(400, "address: use empty (all), 0.0.0.0, :: or 127.0.0.1, so that the admin API stays reachable on 127.0.0.1; limit clients with a firewall")
		}
		if b.AllowedClients != nil {
			for _, ip := range strings.FieldsFunc(*b.AllowedClients, func(r rune) bool { return r == ',' || r == ' ' }) {
				if net.ParseIP(ip) == nil {
					return fail(400, "allowedClients: "+ip+" is not an IP address")
				}
			}
		}
		if b.AllowAnyDevice != nil && *b.AllowAnyDevice && len(s.conf().Devices) > 0 {
			return fail(400, "allowAnyDevice: only while no device is allowed (remove the allowed devices first)")
		}
		scratch := ""
		if b.Scratch != nil { // prazno = privzeta mapa; velja za nove seje (obstoječe kopije ostanejo, kjer so)
			if scratch = *b.Scratch; scratch == "" {
				if scratch = newScratchDefault(); scratch == "" { // Windows: zunaj domače mape (#68)
					scratch = defaultScratch()
				}
			}
			if filepath.IsAbs(scratch) { // L1′: kot premik (C:\minigw-scratch.\x)
				scratch, _ = filepath.Abs(scratch)
			}
			if why := s.scratchCheck(scratch, *b.Scratch != ""); why != "" {
				return fail(400, why)
			}
			if err := protectScratch(scratch); err != nil {
				return fail(400, "scratch folder "+scratch+": "+err.Error()+" (use a folder of your own)")
			}
		}
		if b.StartAtSignIn != nil { // Windows: vrednost Run za ta router (pladenj); drugod storitev namesti minigw serve install
			if runtime.GOOS != "windows" {
				return fail(400, "on this system use minigw serve install or minigw serve uninstall")
			}
			if err := trayStartup(*b.StartAtSignIn, s.cfgPath); err != nil {
				return fail(500, err.Error())
			}
		}
		restart := b.Port != nil || b.Address != nil
		if err := s.update(func(c *serveConfig) error {
			if b.Port != nil {
				c.Port = *b.Port
			}
			if b.Address != nil {
				c.Address = *b.Address
			}
			if b.AllowedClients != nil {
				c.AllowedClients = *b.AllowedClients
			}
			if b.AllowPlainRemote != nil {
				c.AllowPlainRemote = *b.AllowPlainRemote
			}
			if b.AllowAnyDevice != nil {
				c.AllowAnyDevice = *b.AllowAnyDevice
			}
			if b.AllowLocalApps != nil {
				c.AllowLocalApps = b.AllowLocalApps
			}
			if b.Scratch != nil {
				c.Scratch = scratch
			}
			if b.WorktreeNetwork != nil {
				c.WorktreeNetwork = b.WorktreeNetwork
			}
			return nil
		}); err != nil {
			return fail(500, err.Error())
		}
		g := s.apiGeneral(restart && !s.offline)
		g.Connect = "" // brez žetona v odgovoru na spremembo
		return 200, g
	case "POST settings/{}":
		if arg != "login" {
			return fail(404, "Not found: "+method+" "+uri)
		}
		if s.offline {
			return fail(503, "minigw serve is not running")
		}
		return 200, map[string]string{"url": s.settingsLoginURL()}
	case "POST general/{}/move":
		if arg != "scratch" {
			return fail(404, "Not found: "+method+" "+uri)
		}
		return s.moveScratchAPI(body)
	case "POST general/{}":
		if arg != "token" {
			return fail(404, "Not found: "+method+" "+uri)
		}
		t := make([]byte, 24)
		rand.Read(t)
		token := "mgw-" + base64url(t)
		if err := s.update(func(c *serveConfig) error { c.Token = token; return nil }); err != nil {
			return fail(500, err.Error())
		}
		s.settings.clear() // nov žeton konča tudi seje strani z nastavitvami
		g := s.apiGeneral(false)
		g.Connect = "" // novo vrstico da le GET general
		return 200, g
	}
	return fail(404, "Not found: "+method+" "+uri)
}

// patchLogin (#68): žeton (Claude, Copilot) in ključ API (Claude, Codex, Grok) v shrambo ključev; 0 = v redu.
func (s *server) patchLogin(kind string, token, apiKey *string) (int, any) {
	if token != nil && !agentHasToken(kind) {
		return 400, errorBody(400, "token: Claude CLI and Copilot only")
	}
	if token != nil && *token != "" {
		if why := agentTokenProblem(kind, *token); why != "" {
			return 400, errorBody(400, why)
		}
	}
	if apiKey != nil && *apiKey != "" {
		if why := apiKeyProblem(kind, *apiKey); why != "" {
			return 400, errorBody(400, why)
		}
	} else if apiKey != nil && agentAPIKeyEnv[kind] == "" {
		return 400, errorBody(400, "apiKey: Claude CLI, Codex and Grok only")
	}
	if token != nil {
		if err := setAgentToken(s.cfgDir, kind, *token); err != nil {
			return 500, errorBody(500, "token not stored: "+err.Error())
		}
	}
	if apiKey != nil {
		if err := setAgentAPIKey(s.cfgDir, kind, *apiKey); err != nil {
			return 500, errorBody(500, "API key not stored: "+err.Error())
		}
	}
	return 0, nil
}

// agentAPI: agents/codex in agents/grok, ista oblika JSON kot na Macu (enabled, path, found, write, network, models).
func (s *server) agentAPI(method string, k agentKind, body []byte) (int, any) {
	info := func() map[string]any {
		ac := k.conf(s.conf())
		exe, _ := resolveAgent(k, ac.Executable)
		o := map[string]any{"enabled": ac.Enabled, "path": ac.Executable, "found": exe != "", "exe": exe, "write": ac.canWrite(k), "models": ac.Models,
			"login":     agentLogin(s.cfgDir, k.name),
			"askOthers": ac.AskOthers, "work": apiWork(ac.Work), "sandboxed": s.agentsAllowed(serveConfig{}, k.name)} // #68: peskovnik brez stikala
		if k.name != "grok" {
			o["network"] = ac.Network
		}
		if k.name == "copilot" {
			o["commands"] = ac.Commands
			t, _ := keyGet(s.cfgDir, copilotKey)
			o["hasToken"] = t != "" // žeton le za pisanje
			if ac.Models == nil {
				o["models"] = []string{} // Copilot: imena modelov (ni seznama računa); prazno = le privzeti
			}
		}
		return o
	}
	if method == "GET" {
		return 200, info()
	}
	var b struct {
		Enabled   *bool           `json:"enabled"`
		Path      *string         `json:"path"`
		Write     *bool           `json:"write"`
		Network   *bool           `json:"network"`
		Commands  *bool           `json:"commands"`  // Copilot: ukazi lupine (izrecno dovoljenje)
		AskOthers *bool           `json:"askOthers"` // sme vprašati druge modele tega prehoda
		Token     *string         `json:"token"`     // Copilot: žeton za ukaze (le za pisanje; "" ga izbriše)
		APIKey    *string         `json:"apiKey"`    // #68: Codex, Grok: ključ API namesto prijave ("" = nazaj na prijavo)
		Models    json.RawMessage `json:"models"`    // null = vsi; seznam = le ti; brez polja = brez spremembe
		Work      *string         `json:"work"`      // #45: direct, sync, worktree
	}
	if err := decodeStrict(body, &b); err != nil {
		return 400, errorBody(400, err.Error())
	}
	if b.Work != nil && !validWork(*b.Work) {
		return 400, errorBody(400, badWork)
	}
	if b.Network != nil && k.name == "grok" {
		return 400, errorBody(400, "network: Codex and Copilot only")
	}
	if (b.Commands != nil || b.Token != nil) && k.name != "copilot" {
		return 400, errorBody(400, "commands, token: Copilot only")
	}
	if st, e := s.patchLogin(k.name, b.Token, b.APIKey); st != 0 { // pred nastavitvami: ključ ni del datoteke nastavitev
		return st, e
	}
	var models []string
	setModels := len(b.Models) > 0
	if setModels && string(b.Models) != "null" {
		if json.Unmarshal(b.Models, &models) != nil || models == nil {
			return 400, errorBody(400, "models must be null or a list of model names")
		}
		for _, m := range models {
			if !validAgentModel.MatchString(m) {
				return 400, errorBody(400, "not a model name: "+m)
			}
		}
	}
	if b.Path != nil && *b.Path != "" {
		if _, err := resolveAgent(k, *b.Path); err != nil {
			return 400, errorBody(400, err.Error())
		}
	}
	err := s.update(func(c *serveConfig) error {
		ac := k.conf(*c)
		if b.Enabled != nil {
			ac.Enabled = *b.Enabled
		}
		if b.Path != nil {
			ac.Executable = *b.Path
		}
		if b.Write != nil {
			ac.ReadOnly, ac.Write = !*b.Write, *b.Write
		}
		if b.Commands != nil {
			ac.Commands = *b.Commands
		}
		if b.AskOthers != nil {
			ac.AskOthers = *b.AskOthers
		}
		if b.Network != nil {
			ac.Network = *b.Network
		}
		if b.Work != nil {
			ac.Work = storedWork(*b.Work)
		}
		if setModels {
			ac.Models = slices.Clone(models)
		}
		switch k.name {
		case "codex":
			c.Codex = ac
		case "copilot":
			c.Copilot = ac
		default:
			c.Grok = ac
		}
		return nil
	}, func() {
		if b.Enabled != nil && *b.Enabled && k.name == "codex" { // kot Claude CLI: vklop uspel, razlog ne velja več
			s.offReason = ""
		}
	})
	if err != nil {
		return 500, errorBody(500, err.Error())
	}
	return 200, info()
}

func findGateway(c serveConfig, name string) (serveGateway, bool) {
	i := slices.IndexFunc(c.Gateways, func(g serveGateway) bool { return g.Name == name })
	if i < 0 {
		return serveGateway{}, false
	}
	return c.Gateways[i], true
}

func (s *server) apiGeneral(restart bool) apiGeneral {
	c := s.conf()
	return apiGeneral{Port: c.Port, Address: c.Address, Connect: connectLine(c), RestartRequired: restart, AllowedClients: c.AllowedClients,
		AllowPlainRemote: c.AllowPlainRemote, AllowAnyDevice: c.AllowAnyDevice, AllowLocalApps: c.localApps(), Scratch: c.Scratch,
		WorktreeNetwork: c.worktreeNet(), ScratchSuggest: scratchSuggest(c.Scratch)}
}

// buildCommit: commit gradnje (go build iz gita), kot »Version 0.5.14 (d62bf92)« na Macu.
func buildCommit() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	rev, dirty := "", false
	for _, kv := range info.Settings {
		switch kv.Key {
		case "vcs.revision":
			rev = kv.Value[:min(7, len(kv.Value))]
		case "vcs.modified":
			dirty = kv.Value == "true"
		}
	}
	if dirty && rev != "" {
		rev += "+"
	}
	return rev
}

var claudeToolLists sync.Map // pot → orodja te različice Claude Code

// claudeTools: orodja Claude Code (dogodek init pri »claude -p --tools default«), kot ClaudeCLI.availableTools na Macu;
// proces se ustavi po prvi vrstici (pred klicem modela), največ 15 s; v predpomnilniku po poti.
func claudeTools(exe []string, dir string) []string { // dir (#68): delovna mapa poizvedbe (Scratch\tmp)
	if len(exe) == 0 {
		return []string{}
	}
	key := strings.Join(exe, "\x00")
	if v, ok := claudeToolLists.Load(key); ok {
		return v.([]string)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := commandContext(ctx, exe[0], append(exe[1:], "-p", "x", "--model", "haiku", "--output-format", "stream-json", "--verbose",
		"--tools", "default", "--setting-sources", "", "--strict-mcp-config", "--no-session-persistence")...)
	cmd.Dir = dir
	out, err := cmd.StdoutPipe()
	tools := []string{}
	if err == nil && startAgent(cmd) == nil {
		line, _ := bufio.NewReader(out).ReadBytes('\n')
		cmd.Process.Kill()
		cmd.Wait()
		var init struct{ Tools []string }
		if json.Unmarshal(line, &init) == nil && init.Tools != nil {
			tools = init.Tools
		}
	}
	slices.Sort(tools)
	if len(tools) > 0 {
		claudeToolLists.Store(key, tools)
	}
	return tools
}

func (s *server) claudeInfo() apiClaude {
	c, exe := s.conf(), s.claudeExe()
	info := apiClaude{Path: c.Claude, Found: len(exe) > 0, Tools: c.Tools, AllowUnsandboxedBash: c.AllowUnsandboxedBash,
		Enabled: !c.ClaudeDisabled, Folders: strings.Join(c.AllowedFolders, ","), AllowLocalTools: c.AllowLocalTools, AskOthers: c.ClaudeAskOthers,
		DisabledTools: append([]string{}, c.DisabledTools...), PermissionMode: c.PermissionMode, SettingSources: c.SettingSources,
		Work: apiWork(c.ClaudeWork), Login: agentLogin(s.cfgDir, "claude")}
	if len(exe) > 0 {
		info.Exe, info.ClaudeVersion = exe[len(exe)-1], claudeVersion(exe)
	}
	if c.ClaudeDisabled {
		s.mu.RLock()
		info.Off, info.LeftOut = s.offReason, s.offFolders
		s.mu.RUnlock()
	}
	return info
}

var claudeVersions sync.Map // pot → izpis claude --version (enkrat na pot)

// claudeVersion: »2.1.319 (Claude Code)«; največ 5 s, rezultat v predpomnilniku.
func claudeVersion(exe []string) string {
	key := strings.Join(exe, "\x00")
	if v, ok := claudeVersions.Load(key); ok {
		return v.(string)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, _ := commandContext(ctx, exe[0], append(exe[1:], "--version")...).Output()
	v := strings.TrimSpace(strings.SplitN(string(out), "\n", 2)[0])
	claudeVersions.Store(key, v)
	return v
}

func (s *server) apiGateway(g serveGateway) apiGateway {
	key, _ := keyGet(s.cfgDir, g.Name)
	return apiGateway{Name: g.Name, BaseURL: g.BaseURL, Insecure: g.Insecure, Models: g.Models, HasKey: key != "", Ready: s.gateways.ready(g.Name),
		Enabled: !g.Disabled, Aliases: g.Aliases}
}

func (s *server) apiGateways(c serveConfig) []apiGateway {
	out := []apiGateway{}
	for _, g := range c.Gateways {
		out = append(out, s.apiGateway(g))
	}
	return out
}

// apiModels: odkriti modeli z vklopom; filter = del imena za /model (brez razlike med velikimi in malimi črkami).
func apiModels(g serveGateway, models []gwModel, filter string) []apiModel {
	out := []apiModel{}
	for _, m := range models {
		if filter != "" && !strings.Contains(strings.ToLower(publishedID(m.id)), strings.ToLower(filter)) {
			continue
		}
		out = append(out, apiModel{ID: m.id, Model: publishedID(m.id), Window: m.window, Enabled: g.enabled(m.id)})
	}
	return out
}

func (c *gwCatalog) forget(name string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, name)
}

func (c *gwCatalog) ready(name string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.m[name]
	return e != nil && e.ok
}

// foundryAPI: /foundry (Foundry Local): enabled, path, found, running, instructions, models (null = vsi).
func (s *server) foundryAPI(method string, body []byte) (int, any) {
	info := func() map[string]any {
		fc := s.conf().Foundry
		exe := resolveFoundry(fc.Executable)
		s.foundry.mu.Lock()
		running, lastErr := s.foundry.base != "", s.foundry.lastError
		s.foundry.mu.Unlock()
		return map[string]any{"enabled": fc.Enabled, "path": fc.Executable, "found": exe != "", "exe": exe, "running": running,
			"error": lastErr, "instructions": fc.instructions(), "models": fc.Models}
	}
	if method == "GET" {
		return 200, info()
	}
	var b struct {
		Enabled      *bool           `json:"enabled"`
		Path         *string         `json:"path"`
		Instructions *string         `json:"instructions"`
		Models       json.RawMessage `json:"models"`
	}
	if err := decodeStrict(body, &b); err != nil {
		return 400, errorBody(400, err.Error())
	}
	var models []string
	setModels := len(b.Models) > 0
	if setModels && string(b.Models) != "null" {
		if json.Unmarshal(b.Models, &models) != nil || models == nil {
			return 400, errorBody(400, "models must be null or a list of model names")
		}
		for _, m := range models {
			if !foundryValidModel(m) {
				return 400, errorBody(400, "not a model name: "+m)
			}
		}
	}
	if b.Path != nil && *b.Path != "" && resolveFoundry(*b.Path) == "" {
		return 400, errorBody(400, "path: no foundry program at "+*b.Path)
	}
	if b.Instructions != nil && len(*b.Instructions) > 8000 {
		return 400, errorBody(400, "instructions: up to 8000 characters")
	}
	err := s.update(func(c *serveConfig) error {
		if b.Enabled != nil {
			c.Foundry.Enabled = *b.Enabled
		}
		if b.Path != nil {
			c.Foundry.Executable = *b.Path
		}
		if b.Instructions != nil {
			c.Foundry.Instructions = *b.Instructions
		}
		if setModels {
			c.Foundry.Models = models
		}
		return nil
	})
	if err != nil {
		return 400, errorBody(400, err.Error())
	}
	s.foundry.mu.Lock()
	s.foundry.base, s.foundry.models = "", nil // nova pot ali vklop: znova poišči
	s.foundry.mu.Unlock()
	return 200, info()
}

// #45: omejitev načina agenta v API (direct, sync, worktree; v serve.json direct = "").
const badWork = "work: direct, sync or worktree"

func validWork(v string) bool { return v == "direct" || v == "sync" || v == "worktree" }

func storedWork(v string) string {
	if v == "direct" {
		return ""
	}
	return v
}

func apiWork(v string) string {
	if validWork(v) {
		return v
	}
	return "direct"
}
