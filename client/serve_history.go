package main

// Zgodovina naprav in sej (state.json ob serve.json, 0600; enako kot DeviceHistory na Macu): zadnji stik in priklopi
// dovoljenih naprav ter seje odjemalcev s števci na model. Nikoli besedila pozivov ali odgovorov, žetonov ali ključev.
// Zapis največ enkrat na minuto in ob koncu. Načrt za omejitve: docs/plans/plan-sessions.md (zasebni del).

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const (
	historyPerDevice = 20               // priklopi na dovoljeno napravo
	maxSessions      = 200              // seje (najstarejše odpadejo)
	maxDeviceSess    = 50               // M2: seje ene naprave (najprej odpadejo njene najstarejše)
	maxSessionModels = 32               // modeli na sejo; ostali štejejo pod »other«
	connectionGap    = 10 * time.Minute // priklop = prva zahteva po tolikšni tišini; tudi meja seje brez ID-ja
	maxStateFile     = 4 << 20          // večje datoteke ne bere (ročno spremenjena ali pokvarjena)
	projectDays      = 31               // #46b: števci po projektu (dnevi UTC)
	maxProjects      = 5000             // vrstice števcev (najprej odpade najstarejši dan)
	maxDeviceProj    = 1250             // vrstice ene naprave (četrtina, kot seje M2: naprava ne izrine drugih)
	maxProjectFolder = 256              // L2: ključ mape v števcih (bajti UTF-8)
)

type deviceConn struct {
	Time    time.Time `json:"time"`
	Address string    `json:"address"`
	Client  string    `json:"client,omitempty"`
}

type deviceState struct {
	LastSeen time.Time    `json:"lastSeen"`
	Client   string       `json:"client,omitempty"`
	History  []deviceConn `json:"history,omitempty"` // najstarejši prvi
}

// modelUse: števci enega modela ali agenta v seji za eno vlogo in klicatelja (osnova za kasnejše omejitve);
// readOnly in tools: zadnje veljavne omejitve agenta (mapa samo za branje; orodja ali stikala agenta).
type modelUse struct {
	Model        string  `json:"model"`
	Role         string  `json:"role,omitempty"`     // vloga klicatelja (--minigw-role)
	Via          string  `json:"via,omitempty"`      // agent ali veriga, ki je klicala (agent-calls); "" = odjemalec
	ReadOnly     bool    `json:"readOnly,omitempty"` // mapa seje le za branje
	Tools        string  `json:"tools,omitempty"`    // Claude CLI: orodja ali »local«; Codex, Grok, Copilot: write, network, commands
	Cap          string  `json:"cap,omitempty"`      // #45: omejitev načina (sync, worktree), zaradi katere je agent le bral
	Requests     int     `json:"requests"`
	Errors       int     `json:"errors"`  // 5xx in prekinjene
	Refused      int     `json:"refused"` // 4xx
	Seconds      float64 `json:"seconds"` // skupni čas zahtev (pri agentih čas izvajanja)
	InputTokens  int64   `json:"inputTokens"`
	OutputTokens int64   `json:"outputTokens"`
}

type sessionRecord struct {
	ID            string      `json:"id"`               // x-claude-mini-session ali »t-…« (naprava in časovno okno)
	Device        string      `json:"device,omitempty"` // "" = ta računalnik brez ključa naprave
	DeviceName    string      `json:"deviceName,omitempty"`
	DeviceRemoved bool        `json:"deviceRemoved,omitempty"`
	Start         time.Time   `json:"start"`
	Last          time.Time   `json:"last"`
	Folder        string      `json:"folder,omitempty"` // mapa odjemalca, normalizirana (normFolder): ključ za nastavitve po projektu
	OS            string      `json:"os,omitempty"`     // OS odjemalca (macOS, Windows, Linux): mapa brez velikih črk pri macOS in Windows
	Role          string      `json:"role,omitempty"`   // zadnja vloga klicatelja
	Mode          string      `json:"mode"`             // chat, direct, sync, native, worktree, refused
	ReadOnly      bool        `json:"readOnly,omitempty"`
	Branch        string      `json:"branch,omitempty"`
	Models        []*modelUse `json:"models"`
}

// projectUse (#46b): števci enega agenta v mapi naprave za en dan UTC; ostanejo, ko seja odpade (osnova za omejitve).
// Le števila, nikoli besedila; žetone javi ponudnik (niso preverjeni).
type projectUse struct {
	Device       string  `json:"device,omitempty"` // "" = ta računalnik brez ključa naprave
	Folder       string  `json:"folder"`           // mapa seje (normFolder): vgnezdeni klici štejejo pod projekt klicatelja
	OS           string  `json:"os,omitempty"`
	Day          string  `json:"day"`   // UTC YYYY-MM-DD
	Agent        string  `json:"agent"` // claude, codex, grok, copilot ali other (ponudniki)
	Requests     int     `json:"requests"`
	Errors       int     `json:"errors"`
	Refused      int     `json:"refused"`
	Seconds      float64 `json:"seconds"`
	InputTokens  int64   `json:"inputTokens"`
	OutputTokens int64   `json:"outputTokens"`
}

type gwState struct {
	mu       sync.Mutex
	path     string
	dirty    bool
	Devices  map[string]*deviceState `json:"devices"`
	Sessions []*sessionRecord        `json:"sessions"`           // po začetku, najstarejša prva
	Projects []*projectUse           `json:"projects,omitempty"` // #46b: po nastanku (dnevi naraščajo), najstarejša prva
}

func loadState(path string) *gwState {
	g := &gwState{path: path}
	// L3: le navadna datoteka (ne povezava, FIFO ali naprava), branje največ do meje
	if st, err := os.Lstat(path); err == nil && st.Mode().IsRegular() {
		if f, err := os.Open(path); err == nil {
			b, _ := io.ReadAll(io.LimitReader(f, maxStateFile+1))
			f.Close()
			if len(b) <= maxStateFile {
				json.Unmarshal(b, g)
			}
		}
	}
	if g.Devices == nil {
		g.Devices = map[string]*deviceState{}
	}
	g.Sessions = slices.DeleteFunc(g.Sessions, func(r *sessionRecord) bool { return r == nil || r.ID == "" })
	for _, r := range g.Sessions {
		r.Models = slices.DeleteFunc(r.Models, func(m *modelUse) bool { return m == nil })
	}
	g.Projects = slices.DeleteFunc(g.Projects, func(p *projectUse) bool { return p == nil })
	return g
}

func (g *gwState) save() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.dirty || g.path == "" {
		return nil
	}
	b, err := json.Marshal(g)
	for err == nil && len(b) > maxStateFile && len(g.Sessions)+len(g.Projects) > 0 { // M1: datoteka nikoli čez mejo (sicer je ob branju ni)
		if len(g.Sessions) > 0 { // najprej seje; števci projektov naj ostanejo
			g.Sessions = slices.Delete(g.Sessions, 0, max(1, len(g.Sessions)/10))
		} else { // L2: najprej najstarejše vrstice naprave z največ vrsticami
			g.Projects = trimBusiest(g.Projects)
		}
		b, err = json.Marshal(g)
	}
	if err != nil {
		return err
	}
	if err := writeFileAtomic(g.path, b); err != nil {
		return err
	}
	g.dirty = false
	return nil
}

// saveEvery: zapis največ enkrat na minuto (spremembe so le v pomnilniku do naslednjega zapisa ali konca).
func (g *gwState) saveEvery(d time.Duration) {
	for range time.Tick(d) {
		g.save()
	}
}

// seen: zahteva dovoljene naprave; priklop, če je od prejšnje minilo vsaj connectionGap (prehod ima eno zahtevo na
// povezavo, zato nova povezava ni priklop).
func (g *gwState) seen(id, addr, client string, now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	d := g.Devices[id]
	if d == nil {
		d = &deviceState{}
		g.Devices[id] = d
	}
	if client != "" {
		d.Client = client
	}
	now = now.UTC().Truncate(time.Second) // RFC 3339 brez delov sekunde, kot na Macu
	if d.LastSeen.IsZero() || now.Sub(d.LastSeen) >= connectionGap {
		d.History = append(d.History, deviceConn{Time: now, Address: clip(addr, 64), Client: clip(d.Client, 128)})
		if n := len(d.History) - historyPerDevice; n > 0 {
			d.History = slices.Delete(d.History, 0, n)
		}
	}
	d.LastSeen = now
	g.dirty = true
}

func (g *gwState) device(id string) (deviceState, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	d := g.Devices[id]
	if d == nil {
		return deviceState{}, false
	}
	return deviceState{LastSeen: d.LastSeen, Client: d.Client, History: slices.Clone(d.History)}, true
}

// prune: zgodovina le dovoljenih naprav; seje odstranjenih ostanejo z oznako »device removed«.
func (g *gwState) prune(devices []serveDevice) {
	g.mu.Lock()
	defer g.mu.Unlock()
	allowed := func(id string) bool {
		return slices.ContainsFunc(devices, func(d serveDevice) bool { return d.ID == id })
	}
	for id := range g.Devices {
		if !allowed(id) {
			delete(g.Devices, id)
			g.dirty = true
		}
	}
	for _, r := range g.Sessions {
		if r.Device != "" && !r.DeviceRemoved && !allowed(r.Device) {
			r.DeviceRemoved, g.dirty = true, true
		}
	}
}

// sessionUse: ena zahteva za model (brez besedila).
type sessionUse struct {
	session, device, deviceName, folder, os, mode, branch, model, role, via, tools, cap string
	readOnly                                                                            bool
	status                                                                              int
	took                                                                                time.Duration
	in, out                                                                             int64
}

func (g *gwState) record(u sessionUse, now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	now = now.UTC().Truncate(time.Second)
	if len(u.session) > 128 || cleanText(u.session) != u.session { // M2: tak ID se ne bi ujemal s svojim zapisom; L5: zahteva
		u.session = "" // vseeno šteje, v seji »t-…« (sicer bi jo odjemalec tako skril)
	}
	var r *sessionRecord
	for i := len(g.Sessions) - 1; i >= 0 && r == nil; i-- {
		x := g.Sessions[i]
		if x.Device != u.device {
			continue
		}
		if u.session != "" && x.ID == u.session || u.session == "" && strings.HasPrefix(x.ID, "t-") && now.Sub(x.Last) < connectionGap {
			r = x
		}
	}
	if r == nil {
		id := u.session
		if id == "" {
			id = "t-" + strconv.FormatInt(now.UnixNano(), 36)
		}
		r = &sessionRecord{ID: id, Device: u.device, Start: now, Mode: "chat"}
		// M2: naprava ne izrine drugih: najprej odpade njena najstarejša seja, nato najstarejša sploh
		mine := 0
		for _, x := range g.Sessions {
			if x.Device == u.device {
				mine++
			}
		}
		if mine >= maxDeviceSess {
			k := slices.IndexFunc(g.Sessions, func(x *sessionRecord) bool { return x.Device == u.device })
			g.Sessions = slices.Delete(g.Sessions, k, k+1)
		}
		g.Sessions = append(g.Sessions, r)
		if n := len(g.Sessions) - maxSessions; n > 0 {
			g.Sessions = slices.Delete(g.Sessions, 0, n)
		}
	}
	r.Last = now
	if u.deviceName != "" {
		r.DeviceName = u.deviceName
	}
	if r.Folder == "" {
		r.Folder = clip(normFolder(cleanText(u.folder)), 1024)
	}
	if u.os != "" {
		r.OS = clip(u.os, 16)
	}
	r.Role = u.role // zadnja vloga = vloga zadnje zahteve, tudi prazna
	if u.mode != "" && u.mode != "chat" {
		r.Mode = u.mode
	}
	r.ReadOnly = r.ReadOnly || u.readOnly
	if u.branch != "" {
		r.Branch = clip(u.branch, 255)
	}
	key := modelUse{Model: clip(u.model, 64), Role: clip(u.role, 32), Via: clip(u.via, 128)}
	i := slices.IndexFunc(r.Models, func(m *modelUse) bool { return m.Model == key.Model && m.Role == key.Role && m.Via == key.Via })
	if i < 0 && len(r.Models) >= maxSessionModels {
		key = modelUse{Model: "other"}
		i = slices.IndexFunc(r.Models, func(m *modelUse) bool { return m.Model == "other" && m.Role == "" && m.Via == "" })
	}
	if i < 0 {
		r.Models = append(r.Models, &key)
		i = len(r.Models) - 1
	}
	m := r.Models[i]
	m.ReadOnly, m.Tools, m.Cap = u.readOnly, clip(u.tools, 256), clip(u.cap, 16)
	m.Requests++
	switch {
	case u.status >= 500 || u.status == 499 || u.status == 0:
		m.Errors++
	case u.status >= 400:
		m.Refused++
	}
	m.Seconds += u.took.Seconds()
	m.InputTokens += u.in
	m.OutputTokens += u.out
	if r.Folder != "" {
		g.count(r, u, now)
	}
	g.dirty = true
}

// count (#46b, pod g.mu): zahteva v števce projekta (naprava, mapa seje, dan UTC, agent). Nova vrstica: brez dni,
// starejših od projectDays; naprava največ maxDeviceProj vrstic (najprej njena najstarejša), skupaj maxProjects.
func (g *gwState) count(r *sessionRecord, u sessionUse, now time.Time) {
	day, agent, folder := now.Format(time.DateOnly), agentOfModel(u.model), clip(r.Folder, maxProjectFolder)
	if agent == "" {
		agent = "other"
	}
	var p *projectUse
	for i := len(g.Projects) - 1; i >= 0 && p == nil; i-- {
		if x := g.Projects[i]; x.Day == day && x.Agent == agent && x.Device == r.Device && x.Folder == folder {
			p = x
		}
	}
	if p == nil {
		oldest := now.AddDate(0, 0, 1-projectDays).Format(time.DateOnly)
		g.Projects = slices.DeleteFunc(g.Projects, func(x *projectUse) bool { return x.Day < oldest })
		mine := 0
		for _, x := range g.Projects {
			if x.Device == r.Device {
				mine++
			}
		}
		if mine >= maxDeviceProj {
			k := slices.IndexFunc(g.Projects, func(x *projectUse) bool { return x.Device == r.Device })
			g.Projects = slices.Delete(g.Projects, k, k+1)
		}
		p = &projectUse{Device: r.Device, Folder: folder, OS: r.OS, Day: day, Agent: agent}
		g.Projects = append(g.Projects, p)
		if n := len(g.Projects) - maxProjects; n > 0 {
			g.Projects = slices.Delete(g.Projects, 0, n)
		}
	}
	p.Requests++
	switch {
	case u.status >= 500 || u.status == 499 || u.status == 0:
		p.Errors++
	case u.status >= 400:
		p.Refused++
	}
	p.Seconds += u.took.Seconds()
	p.InputTokens += u.in
	p.OutputTokens += u.out
}

// trimBusiest (L2): brez desetine (vsaj ene) najstarejših vrstic naprave z največ vrsticami (ena naprava ne izrine drugih).
func trimBusiest(rows []*projectUse) []*projectUse {
	n := map[string]int{}
	for _, x := range rows {
		n[x.Device]++
	}
	busiest := rows[0].Device
	for d, c := range n {
		if c > n[busiest] || c == n[busiest] && d < busiest {
			busiest = d
		}
	}
	drop := max(1, n[busiest]/10)
	return slices.DeleteFunc(rows, func(x *projectUse) bool {
		if x.Device == busiest && drop > 0 {
			drop--
			return true
		}
		return false
	})
}

// usage (#46b): števci zadnjih `days` dni (danes je prvi), najnovejši dan prvi, nato mapa, naprava, agent (enako na Macu).
func (g *gwState) usage(device, folder string, days int, now time.Time) []projectUse {
	g.mu.Lock()
	defer g.mu.Unlock()
	from := now.UTC().AddDate(0, 0, 1-days).Format(time.DateOnly)
	f := normFolder(folder)
	out := []projectUse{}
	for _, p := range g.Projects {
		if p.Day >= from && (device == "" || p.Device == device) && (folder == "" || inFolder(p.Folder, f, p.OS)) {
			out = append(out, *p)
		}
	}
	slices.SortFunc(out, func(a, b projectUse) int {
		if c := strings.Compare(b.Day, a.Day); c != 0 {
			return c
		}
		return strings.Compare(a.Folder+"\x00"+a.Device+"\x00"+a.Agent, b.Folder+"\x00"+b.Device+"\x00"+b.Agent)
	})
	return out
}

// clearSessions: seje in števci projektov (eno dejanje lastnika; kasnejše omejitve morda dobijo svoje brisanje).
func (g *gwState) clearSessions() {
	g.mu.Lock()
	g.Sessions, g.Projects, g.dirty = nil, nil, true
	g.mu.Unlock()
}

// apiSession: zapis seje za API (najnovejša prva) s trajanjem v sekundah.
type apiSession struct {
	sessionRecord
	Duration float64 `json:"duration"`
}

func (g *gwState) sessions(device, folder string) []apiSession {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := []apiSession{}
	f := normFolder(folder)
	for i := len(g.Sessions) - 1; i >= 0; i-- {
		r := g.Sessions[i]
		if device != "" && r.Device != device {
			continue
		}
		if folder != "" && !inFolder(r.Folder, f, r.OS) {
			continue
		}
		c := *r
		c.Models = nil
		for _, v := range r.Models {
			m := *v
			c.Models = append(c.Models, &m)
		}
		slices.SortFunc(c.Models, func(a, b *modelUse) int { // po modelu, vlogi, klicatelju (enako na Macu)
			return strings.Compare(a.Model+"\x00"+a.Role+"\x00"+a.Via, b.Model+"\x00"+b.Role+"\x00"+b.Via)
		})
		out = append(out, apiSession{c, r.Last.Sub(r.Start).Seconds()})
	}
	return out
}

// clip (M1): očiščeno in največ n bajtov UTF-8 (cel znak), enako kot GatewayHistory.clip na Macu.
func clip(s string, n int) string {
	s = cleanText(s)
	for len(s) > n {
		_, size := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-size]
	}
	return s
}

// cleanText: brez kontrolnih znakov C0, C1, DEL, ločil vrstic in smernih kontrol (L4), kot cleanDeviceName, a brez
// meje 64 znakov (poti map).
func cleanText(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r >= 0x7f && r <= 0x9f || r == 0x2028 || r == 0x2029 || r >= 0x202a && r <= 0x202e || r >= 0x2066 && r <= 0x2069 {
			return -1
		}
		return r
	}, strings.ToValidUTF8(s, ""))
}

// sessionMode: način seje iz mape, ki jo je odjemalec delil (chat = brez mape; direct = zavrnjen neposredni dostop).
// `agent`: agent te zahteve (worktree: njegova veja); "" ali brez mesta = prva veja seje.
func (h *shareHub) sessionMode(session, owner, agent string) (mode string, readOnly bool, branch string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.bySession[session]
	if s != nil && s.owner != owner { // M2: tuja mapa (druga naprava z istim ID-jem seje) ne opiše te seje
		s = nil
	}
	switch {
	case s == nil && h.refused[session]: // odjemalec je želel direct, ki ga minigw serve zavrne
		return "refused", false, ""
	case s == nil:
		return "chat", false, ""
	case s.tree != nil:
		agents := make([]string, 0, len(s.tree.slots))
		for a := range s.tree.slots {
			agents = append(agents, a)
		}
		sort.Strings(agents)
		if sl, ok := s.tree.slots[agent]; ok {
			branch = sl.branch
		} else if len(agents) > 0 {
			branch = s.tree.slots[agents[0]].branch
		}
		return "worktree", s.readOnly, branch
	}
	return "sync", s.readOnly, ""
}

// agentOfModel: agent modela (ime mesta worktree), "" za druge modele.
func agentOfModel(m string) string {
	for p, a := range map[string]string{"claude-cli-": "claude", "claude-chat-": "claude", "claude-codex": "codex", "claude-grok": "grok", "claude-copilot": "copilot"} {
		if strings.HasPrefix(m, p) {
			return a
		}
	}
	return ""
}

// usageWriter: šteje žetone iz odgovora (Anthropic JSON ali SSE: največji input_tokens in output_tokens); zadnjih 64
// bajtov ostane za število, ki ga razdeli meja zapisa.
type usageWriter struct {
	http.ResponseWriter
	in, out int64
	tail    []byte
}

var usageRe = regexp.MustCompile(`"(input|output)_tokens"\s*:\s*(\d+)`)

func (u *usageWriter) Write(p []byte) (int, error) {
	buf := append(u.tail, p...)
	for _, m := range usageRe.FindAllSubmatch(buf, -1) {
		n, _ := strconv.ParseInt(string(m[2]), 10, 64)
		if m[1][0] == 'i' {
			u.in = max(u.in, n)
		} else {
			u.out = max(u.out, n)
		}
	}
	if len(buf) > 64 {
		buf = buf[len(buf)-64:]
	}
	u.tail = append([]byte(nil), buf...)
	return u.ResponseWriter.Write(p)
}

func (u *usageWriter) Flush() {
	if f, ok := u.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (u *usageWriter) Unwrap() http.ResponseWriter { return u.ResponseWriter }

// recordSession: zahteva za model v zgodovino seje (naprava, mapa in način iz glav in delitve; brez besedila).
func (s *server) recordSession(r *req, model string, status int, took time.Duration, in, out int64) {
	session := r.h("x-claude-mini-session")
	if status == 404 { // M1: neznan ali zavrnjen model: stalno ime, ne niz odjemalca
		model = "unknown"
	}
	u := sessionUse{session: session, folder: r.h("x-claude-mini-cwd"), model: model, status: status, took: took, in: in, out: out}
	u.role = reqRole(r)
	agent := agentOfModel(model)
	if p := runOf(r); p != nil { // vgnezdeni klic: veriga iz izvajanja (nikoli iz glave), v seji klicatelja
		u.via = p.via()
		agent = p.chain[0] // vprašani agent dela v klonu (in na veji) prvega agenta verige
	}
	// Ime iz seznama dovoljenih (oznaka na prehodu), sicer iz rokovanja. Naprava, ki ni na seznamu, na loopbacku je ta
	// računalnik (sprejet brez odobritve), ne »odstranjena naprava«.
	if d := r.conn.device; d != nil && (!r.conn.loopback() || s.conf().listed(d.ID)) {
		u.device, u.deviceName = d.ID, d.Name
		if i := slices.IndexFunc(s.conf().Devices, func(x serveDevice) bool { return x.ID == d.ID }); i >= 0 && s.conf().Devices[i].Name != "" {
			u.deviceName = s.conf().Devices[i].Name
		}
	}
	if client := cleanDeviceName(r.h("X-Minigw-Client")); client != "" {
		u.os, _, _ = strings.Cut(client, ",") // »Windows, minigw 0.5.18«
	}
	if session != "" {
		u.mode, u.readOnly, u.branch = s.hub.sessionMode(session, r.owner(), agent)
	}
	if a := r.applied; a != nil { // #46a: kar je izvajanje res uporabilo (tudi le branje brez pisanja v worktree, omejitev #45)
		u.readOnly, u.cap = a.readOnly, a.cap
	} else {
		u.readOnly = u.readOnly || r.h("x-claude-mini-ro") == "1" // tudi brez deljenja; vgnezdeni klic: glava iz izvajanja
	}
	u.tools = s.conf().agentRestrictions(model, u.readOnly) // veljavne omejitve (brez pisanja pri --minigw-ro)
	s.state.record(u, time.Now())
}

// sessionLine: vrstica seje za minigw gw sessions (enako v minigw za macOS).
func sessionLine(x apiSession) string {
	name := x.DeviceName
	if x.Device == "" {
		name = "this computer"
	}
	if x.DeviceRemoved {
		name += " (device removed)"
	}
	mode := x.Mode
	if x.ReadOnly {
		mode += ", read-only"
		if i := slices.IndexFunc(x.Models, func(m *modelUse) bool { return m.Cap != "" }); i >= 0 {
			mode += " (cap " + x.Models[i].Cap + ")"
		}
	}
	var models []string
	for _, m := range x.Models {
		if q := strconv.Quote(m.Model); !slices.Contains(models, q) { // L4: imena v narekovajih
			models = append(models, q)
		}
	}
	slices.Sort(models)
	return fmt.Sprintf("%s  %q  %q  %s  %q  %s", x.Last.UTC().Format(time.RFC3339), x.ID, name, mode, x.Folder, strings.Join(models, ", "))
}

// validRole: ime vloge (--minigw-role, glava x-claude-mini-role): 1-32 znakov iz črk, števk, . _ -.
func validRole(s string) bool { return roleRe.MatchString(s) }

var roleRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,32}$`)

// normFolder: mapa odjemalca za zgodovino in poznejše nastavitve po projektu (enako kot GatewayHistory.folder na
// Macu): brez ».«, »..« in končnega ločila; poti Windows (C:\…, \\strežnik\…) z »/«; velike črke ostanejo.
func normFolder(f string) string {
	if f == "" {
		return ""
	}
	if winPath.MatchString(f) {
		f = strings.ReplaceAll(f, `\`, "/")
	}
	unc := strings.HasPrefix(f, "//")
	if f = path.Clean(f); unc {
		f = "/" + f
	}
	return f
}

var winPath = regexp.MustCompile(`^([A-Za-z]:([\\/]|$)|\\\\)`)

// inFolder: mapa seje je f ali v njej; brez razlike v velikih črkah, če je odjemalec macOS ali Windows.
func inFolder(p, f, os string) bool {
	if os == "macOS" || os == "Windows" {
		p, f = strings.ToLower(p), strings.ToLower(f)
	}
	return p == f || strings.HasPrefix(p, strings.TrimSuffix(f, "/")+"/")
}

// agentRestrictions: veljavne omejitve agenta za zgodovino: Claude CLI orodja ali »local«; Codex, Grok, Copilot
// stikala write, network, commands ("" = le branje); drugi modeli "" (enako kot GatewayHistory.restrictions na Macu).
// readOnly (--minigw-ro): brez pisanja (Claude CLI brez orodij za pisanje oz. »local,read-only«; ukazi Copilot le s pisanjem).
func (c serveConfig) agentRestrictions(model string, readOnly bool) string {
	var on []string
	add := func(ok bool, name string) {
		if ok {
			on = append(on, name)
		}
	}
	switch {
	case model == strings.TrimSuffix(cliAgentPrefix, "-") || strings.HasPrefix(model, cliAgentPrefix):
		switch {
		case c.AllowLocalTools && readOnly:
			return "local,read-only"
		case c.AllowLocalTools:
			return "local"
		case readOnly:
			var t []string
			for _, x := range strings.Split(c.Tools, ",") {
				if x = strings.TrimSpace(x); x != "" && roAllowed(strings.SplitN(x, "(", 2)[0]) {
					t = append(t, x)
				}
			}
			return strings.Join(t, ",")
		}
		return c.Tools
	case isKind(codexKind, model):
		write := c.Codex.canWrite(codexKind) && !readOnly
		add(write, "write")
		add(write && c.Codex.Network, "network") // sandbox read-only nima omrežja
	case isKind(grokKind, model):
		add(c.Grok.canWrite(grokKind) && !readOnly, "write")
	case isKind(copilotKind, model):
		add(c.Copilot.canWrite(copilotKind) && !readOnly, "write")
		add(c.Copilot.Network, "network")
		add(c.Copilot.Commands && !readOnly, "commands")
	default:
		return ""
	}
	return strings.Join(on, ",")
}

func isKind(k agentKind, model string) bool { _, ok := k.match(model); return ok }

// useLine: vrstica modela seje za minigw gw sessions <id> (enako v minigw za macOS).
func useLine(m *modelUse) string {
	label := strconv.Quote(m.Model)
	if m.Role != "" {
		label += " role " + m.Role
	}
	if m.Via != "" {
		label += " via " + strconv.Quote(m.Via)
	}
	if m.ReadOnly {
		label += " read-only"
		if m.Cap != "" {
			label += " (cap " + m.Cap + ")"
		}
	}
	if m.Tools != "" {
		label += " tools " + m.Tools
	}
	return fmt.Sprintf("%s: %d requests, %d errors, %d refused, %.0fs, %d input and %d output tokens", label, m.Requests, m.Errors, m.Refused,
		m.Seconds, m.InputTokens, m.OutputTokens)
}

// reqRole: vloga iz glave x-claude-mini-role, preverjena (brez obrezovanja); neveljavna ali predolga = "". Pri vgnezdenem klicu jo glava nosi
// iz izvajanja (runHandler), zato je vprašani agent ne more spremeniti.
func reqRole(r *req) string {
	if role := r.h("x-claude-mini-role"); validRole(role) {
		return role
	}
	return ""
}

// usageLine: vrstica števcev projekta za minigw gw usage (enako v minigw za macOS).
func usageLine(p projectUse) string {
	name := p.Device
	if name == "" {
		name = "this computer"
	}
	return fmt.Sprintf("%s  %q  %s  %s: %d requests, %d errors, %d refused, %.1f agent minutes, %d input and %d output tokens", p.Day, p.Folder,
		name, p.Agent, p.Requests, p.Errors, p.Refused, p.Seconds/60, p.InputTokens, p.OutputTokens)
}
