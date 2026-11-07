package main

// Skrbniški API (docs/admin-api.md): /minigw/admin/v1/…, le z loopback, Host 127.0.0.1/localhost in skrivnostjo iz
// admin.secret. adminDo služi HTTP in minigw router brez tekočega routerja (spremeni le serve.json), zato ena koda.

import (
	"bytes"
	"crypto/rand"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"
)

const adminPrefix = "/minigw/admin/v1"

type apiDevice struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Address string `json:"address,omitempty"` // le pri čakajočih
	Time    string `json:"time,omitempty"`
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
}

type apiClaude struct {
	Path                 string `json:"path"` // prazno = iz PATH
	Found                bool   `json:"found"`
	Tools                string `json:"tools"`
	AllowUnsandboxedBash bool   `json:"allowUnsandboxedBash"`
	Folder               string `json:"folder"`
}

type apiGeneral struct {
	Port            int    `json:"port"`
	Address         string `json:"address"`
	Connect         string `json:"connect"`
	RestartRequired bool   `json:"restartRequired,omitempty"`
}

type apiStatus struct {
	Running   bool         `json:"running"`
	Version   string       `json:"version"`
	Port      int          `json:"port"`
	Address   string       `json:"address"`
	Installed bool         `json:"installed"`
	Devices   int          `json:"devices"`
	Pending   int          `json:"pending"`
	Claude    apiClaude    `json:"claude"`
	Gateways  []apiGateway `json:"gateways"`
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
			return writeError(w, r.rid, 401, "Invalid admin proof: use minigw router (an encrypted connection with the router key)")
		}
	case auth != "": // npr. Bearer: skrivnost ne sme nikoli po žici
		return writeError(w, r.rid, 401, "The admin API takes MGW-Admin over an encrypted connection (minigw router), never the secret itself")
	case !s.settings.valid(r.h(settingsHeader)): // stran z nastavitvami: seja v glavi (brez piškotkov)
		return writeError(w, r.rid, 401, "Invalid or missing admin secret (admin.secret in the config folder)")
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
		return writeError(w, r.rid, 403, "The admin API is only available on the router computer")
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
	return &server{cfg: c, cfgPath: cfgPath, cfgDir: cfgDir, claude: exe, offline: true, log: log.New(io.Discard, "", 0)}, nil
}

func (s *server) addPending(d serveDevice, addr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pending = slices.DeleteFunc(s.pending, func(p apiDevice) bool { return p.ID == d.ID })
	s.pending = append(s.pending, apiDevice{ID: d.ID, Name: d.Name, Address: addr, Time: time.Now().UTC().Format(time.RFC3339)})
	if n := len(s.pending); n > 20 {
		s.pending = slices.Clone(s.pending[n-20:])
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
			Devices: len(c.Devices), Claude: s.claudeInfo(), Gateways: s.apiGateways(c)}
		s.mu.RLock()
		st.Pending = len(s.pending)
		s.mu.RUnlock()
		return 200, st

	case "GET devices":
		c := s.conf()
		out := map[string]any{"devices": []apiDevice{}, "allowAnyDevice": c.AllowAnyDevice}
		var list []apiDevice
		for _, d := range c.Devices {
			list = append(list, apiDevice{ID: d.ID, Name: d.Name})
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
			return fail(400, strconv.Quote(d.ID)+" is not a device ID (43 characters, base64url): copy it from the router log or from the client's refusal message")
		}
		added := false
		err := s.update(func(c *serveConfig) error {
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
			i := slices.IndexFunc(c.Devices, func(d serveDevice) bool { return d.ID == arg })
			if i < 0 { // ime le, če je enolično
				for j, d := range c.Devices {
					if d.Name == arg {
						if i >= 0 {
							return errors.New("several devices are named " + strconv.Quote(arg) + ": use the device ID")
						}
						i = j
					}
				}
			}
			if i < 0 {
				return errors.New("no allowed device " + strconv.Quote(arg))
			}
			gone = c.Devices[i]
			c.Devices = slices.Delete(c.Devices, i, i+1)
			return nil
		})
		if err != nil {
			return fail(400, err.Error())
		}
		return 200, map[string]any{"device": apiDevice{ID: gone.ID, Name: gone.Name}}

	case "GET gateways":
		return 200, map[string]any{"gateways": s.apiGateways(s.conf())}
	case "POST gateways":
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
			return fail(400, "gateway name: letters, digits, . _ - (up to 32)")
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
			return fail(400, "gateway not added: "+err.Error())
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
			s.log.Printf("gateway %s: API key: %s", b.Name, where)
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
		return 200, map[string]any{"gateway": s.apiGateway(g), "keyStore": where, "models": apiModels(g, models, "")}
	case "DELETE gateways/{}":
		err := s.update(func(c *serveConfig) error {
			i := slices.IndexFunc(c.Gateways, func(x serveGateway) bool { return x.Name == arg })
			if i < 0 {
				return errors.New("no gateway " + arg)
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
	case "GET gateways/{}/models", "POST gateways/{}/models":
		g, ok := findGateway(s.conf(), arg)
		if !ok {
			return fail(404, "no gateway "+arg)
		}
		key, _ := keyGet(s.cfgDir, g.Name)
		models, err := discoverGateway(g, key)
		if err != nil {
			return fail(502, err.Error())
		}
		if method == "GET" {
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
				return fail(400, "gateway "+g.Name+" has no model "+v)
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
					return fail(400, "gateway "+g.Name+" has no model "+v)
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
				return errors.New("no gateway " + g.Name)
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

	case "GET agents/{}", "PATCH agents/{}":
		if arg != "claude" {
			return fail(404, "no agent "+arg+" (this router has claude only)")
		}
		if method == "GET" {
			return 200, s.claudeInfo()
		}
		var b struct {
			Path                 *string `json:"path"`
			Tools                *string `json:"tools"`
			AllowUnsandboxedBash *bool   `json:"allowUnsandboxedBash"`
			Folder               *string `json:"folder"`
		}
		if err := decodeStrict(body, &b); err != nil {
			return fail(400, err.Error())
		}
		if b.Tools != nil {
			if *b.Tools = strings.ReplaceAll(*b.Tools, " ", ""); !validTools.MatchString(*b.Tools) {
				return fail(400, "tools: comma-separated tool names, e.g. "+defaultTools)
			}
		}
		if b.Folder != nil && *b.Folder != "" {
			if !filepath.IsAbs(*b.Folder) {
				return fail(400, "folder must be an absolute path")
			}
			if st, err := os.Stat(*b.Folder); err != nil || !st.IsDir() {
				return fail(400, "not a folder: "+*b.Folder)
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
			if b.Folder != nil {
				c.Folder = *b.Folder
			}
			e, err := checkAgent(c, s.cfgDir)
			exe = e
			return err
		}, func() { s.claude = exe })
		if err != nil {
			return fail(400, err.Error())
		}
		return 200, s.claudeInfo()

	case "GET general":
		c := s.conf()
		return 200, apiGeneral{Port: c.Port, Address: c.Address, Connect: connectLine(c)}
	case "PATCH general":
		var b struct {
			Port    *int    `json:"port"`
			Address *string `json:"address"`
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
		if err := s.update(func(c *serveConfig) error {
			if b.Port != nil {
				c.Port = *b.Port
			}
			if b.Address != nil {
				c.Address = *b.Address
			}
			return nil
		}); err != nil {
			return fail(500, err.Error())
		}
		c := s.conf()
		return 200, apiGeneral{Port: c.Port, Address: c.Address, Connect: connectLine(c), RestartRequired: !s.offline}
	case "POST settings/{}":
		if arg != "login" {
			return fail(404, "Not found: "+method+" "+uri)
		}
		if s.offline {
			return fail(503, "minigw serve is not running")
		}
		return 200, map[string]string{"url": s.settingsLoginURL()}
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
		c := s.conf()
		return 200, apiGeneral{Port: c.Port, Address: c.Address, Connect: connectLine(c)}
	}
	return fail(404, "Not found: "+method+" "+uri)
}

func findGateway(c serveConfig, name string) (serveGateway, bool) {
	i := slices.IndexFunc(c.Gateways, func(g serveGateway) bool { return g.Name == name })
	if i < 0 {
		return serveGateway{}, false
	}
	return c.Gateways[i], true
}

func (s *server) claudeInfo() apiClaude {
	c := s.conf()
	return apiClaude{Path: c.Claude, Found: len(s.claudeExe()) > 0, Tools: c.Tools, AllowUnsandboxedBash: c.AllowUnsandboxedBash, Folder: c.Folder}
}

func (s *server) apiGateway(g serveGateway) apiGateway {
	key, _ := keyGet(s.cfgDir, g.Name)
	return apiGateway{Name: g.Name, BaseURL: g.BaseURL, Insecure: g.Insecure, Models: g.Models, HasKey: key != "", Ready: s.gateways.ready(g.Name)}
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
