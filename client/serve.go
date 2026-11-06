package main

// minigw serve: majhen router Mini Gateway za Windows in Linux (kot RouterCore/Server.swift in Router.swift).
// Le šifra MGW2 (AES-GCM, v standardni knjižnici Go), ponudnik claude -p (serve_cli.go) in sinhronizirana kopija
// mape odjemalca (serve_sync.go). Navaden HTTP le z loopback. Brez Siri, Apple FM in prehodov.

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
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
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
	Folder         string        `json:"folder,omitempty"`  // dovoljena mapa za agente brez sinhronizacije
	Scratch        string        `json:"scratch,omitempty"` // kopije map in pogovori; prazno = mapa predpomnilnika
	Log            string        `json:"log,omitempty"`     // prazno = stderr
	Claude         string        `json:"claude,omitempty"`  // pot do claude; prazno = PATH
	Tools          string        `json:"tools"`             // orodja agenta (--tools, --allowedTools)
	PermissionMode string        `json:"permissionMode"`
	Devices        []serveDevice `json:"devices"`
	AllowAnyDevice bool          `json:"allowAnyDevice,omitempty"` // le pri praznem seznamu: vsaka naprava z žetonom
	// Bash v tools na Windows teče brez sandboxa (dostop do vsega, kar sme uporabnik): le z izrecnim dovoljenjem.
	AllowUnsandboxedBash bool `json:"allowUnsandboxedBash,omitempty"`
}

const serveHelp = `minigw serve: a Mini Gateway router on this computer (Windows, Linux) for minigw clients.

  minigw serve init [--port N] [--folder DIR] [--config FILE]   create the config, print the minigw connect line
  minigw serve allow <device id> [name] [--config FILE]         allow a client device
  minigw serve [--port N] [--folder DIR] [--config FILE]        run the router

Clients use: MINIGW_WORK=sync minigw … (folder mounts are macOS only).
Unknown devices are refused; the log prints their ID. Before minigw serve allow, compare the ID with the one on the
client (it is the device key of the computer that just connected): anyone with the token can try to connect.
Connections from this computer itself (loopback) skip the device check and encryption: do not forward the port to
loopback (ssh -L, netsh portproxy, a reverse proxy), or remote clients get the same trust.`

// Privzeta orodja: urejanje datotek v kopiji; Bash (brez sandboxa na Windows) le, če ga nastavitve dodajo.
const defaultTools = "Read,Grep,Glob,Edit,Write,MultiEdit"

func defaultServeConfigPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = "."
	}
	return filepath.Join(dir, "minigw", "serve.json")
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
	if c.Port == 0 {
		c.Port = 8765
	}
	if c.Tools == "" {
		c.Tools = defaultTools
	}
	if c.PermissionMode == "" {
		c.PermissionMode = "dontAsk"
	}
	return c, nil
}

func saveServeConfig(path string, c serveConfig) {
	data, _ := json.MarshalIndent(c, "", "  ")
	os.MkdirAll(filepath.Dir(path), 0o700)
	if err := os.WriteFile(path, append(data, '\n'), 0o600); err != nil {
		fail("cannot save " + path + ": " + err.Error())
	}
	os.Chmod(path, 0o600) // WriteFile ohrani način obstoječe datoteke
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

// deviceAllowed: naprava s seznama; neznana le pri praznem seznamu z allowAnyDevice. Loopback (ta računalnik) vedno.
func (c serveConfig) deviceAllowed(d *serveDevice, loopback bool) bool {
	if loopback {
		return true
	}
	if d == nil { // oddaljen odjemalec brez ključa naprave nikoli
		return false
	}
	if len(c.Devices) == 0 {
		return c.AllowAnyDevice
	}
	for _, a := range c.Devices {
		if d != nil && a.ID == d.ID {
			return true
		}
	}
	return false
}

func serveCommand(args []string) {
	sub := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("minigw serve", flag.ExitOnError)
	cfgPath := fs.String("config", defaultServeConfigPath(), "config file")
	port := fs.Int("port", 0, "port (default 8765)")
	folder := fs.String("folder", "", "folder for agents that do not sync (--minigw-work=sync is preferred)")
	fs.Usage = func() { fmt.Fprintln(os.Stderr, serveHelp) }
	var rest []string
	if sub == "allow" { // minigw serve allow <id> [ime] [--config f]
		for len(args) > 0 && !strings.HasPrefix(args[0], "-") {
			rest, args = append(rest, args[0]), args[1:]
		}
	}
	fs.Parse(args)

	switch sub {
	case "init":
		c, err := loadServeConfig(*cfgPath)
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				fail(err.Error())
			}
			k, _ := ecdh.X25519().GenerateKey(rand.Reader)
			t := make([]byte, 24)
			rand.Read(t)
			c = serveConfig{Token: "mgw-" + base64url(t), Key: base64url(k.Bytes()), Port: 8765, Tools: defaultTools,
				PermissionMode: "dontAsk", Devices: []serveDevice{}}
		}
		if *port != 0 {
			c.Port = *port
		}
		if *folder != "" {
			abs, _ := filepath.Abs(*folder)
			c.Folder = abs
		}
		saveServeConfig(*cfgPath, c)
		k, err := c.privateKey()
		if err != nil {
			fail("invalid key in " + *cfgPath)
		}
		say("Config: " + *cfgPath)
		say("On each client run:")
		say(fmt.Sprintf("  minigw connect %s:%d --token '%s#%s'", localIP(), c.Port, c.Token, base64url(k.PublicKey().Bytes())))
		say("Then start the router with: minigw serve" + map[bool]string{true: "", false: " --config " + *cfgPath}[*cfgPath == defaultServeConfigPath()])
		say("Unknown devices are refused; the router log shows how to allow one (minigw serve allow <id>).")
		return
	case "allow":
		if len(rest) == 0 {
			fail("usage: minigw serve allow <device id> [name]")
		}
		c, err := loadServeConfig(*cfgPath)
		if err != nil {
			fail("cannot read " + *cfgPath + " (run minigw serve init first): " + err.Error())
		}
		name := strings.Join(rest[1:], " ")
		for _, d := range c.Devices {
			if d.ID == rest[0] {
				say("already allowed: " + rest[0])
				return
			}
		}
		c.Devices = append(c.Devices, serveDevice{ID: rest[0], Name: name})
		saveServeConfig(*cfgPath, c)
		say("allowed device " + rest[0] + " (restart minigw serve)")
		return
	case "":
	case "help":
		say(serveHelp)
		return
	default:
		fail("unknown command minigw serve " + sub + "\n\n" + serveHelp)
	}

	c, err := loadServeConfig(*cfgPath)
	if err != nil {
		fail("cannot read " + *cfgPath + ": run minigw serve init first (" + err.Error() + ")")
	}
	if *port != 0 {
		c.Port = *port
	}
	if *folder != "" {
		c.Folder = *folder
	}
	if c.Folder != "" {
		abs, _ := filepath.Abs(c.Folder)
		if st, err := os.Stat(abs); err != nil || !st.IsDir() {
			fail("not a folder: " + c.Folder)
		}
		c.Folder = abs
	}
	s, err := newServer(c, *cfgPath)
	if err != nil {
		fail(err.Error())
	}
	ln, err := net.Listen("tcp", fmt.Sprintf(":%d", c.Port))
	if err != nil {
		fail(err.Error())
	}
	s.log.Printf("minigw %s serving on :%d (MGW2), claude=%q folder=%q scratch=%q devices=%d", version, c.Port,
		strings.Join(s.claude, " "), c.Folder, s.cfg.Scratch, len(c.Devices))
	fail(s.serve(ln).Error())
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
	cfg      serveConfig
	cfgDir   string   // mapa nastavitev (žeton, ključ routerja): agentu prepovedana
	claude   []string // claude.exe ali node + cli.js; prazno = ni nameščen
	key      *ecdh.PrivateKey
	log      *log.Logger
	hub      *shareHub
	sessions sessionRegistry
	procs    chan struct{} // največ hkratnih procesov claude
}

func newServer(c serveConfig, cfgPath string) (*server, error) {
	k, err := c.privateKey()
	if err != nil || c.Token == "" {
		return nil, errors.New("config needs token and key: run minigw serve init")
	}
	cfgDir, _ := filepath.Abs(filepath.Dir(cfgPath))
	if c.Scratch == "" {
		dir, err := os.UserCacheDir()
		if err != nil {
			return nil, errors.New("no cache folder for copies (" + err.Error() + "): set \"scratch\" in the config")
		}
		c.Scratch = filepath.Join(dir, "minigw", "scratch")
	}
	if c.Scratch, err = filepath.Abs(c.Scratch); err != nil || under(c.Scratch, cfgDir) {
		return nil, errors.New("scratch must not be inside the config folder " + cfgDir)
	}
	exe, err := resolveClaude(c.Claude)
	if err != nil {
		return nil, err
	}
	if hasBash(c.Tools) && runtime.GOOS == "windows" && !c.AllowUnsandboxedBash {
		return nil, errors.New("tools contains Bash, but Windows has no sandbox for it: remove Bash from tools, or set \"allowUnsandboxedBash\": true to let agents run any command as this user")
	}
	out := io.Writer(os.Stderr)
	if c.Log != "" {
		f, err := os.OpenFile(c.Log, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, err
		}
		out = f
	}
	s := &server{cfg: c, cfgDir: cfgDir, claude: exe, key: k, log: log.New(out, "", log.LstdFlags), hub: newShareHub(),
		sessions: sessionRegistry{m: map[string]string{}}, procs: make(chan struct{}, 6)}
	switch {
	case !hasBash(c.Tools):
	case runtime.GOOS == "windows":
		s.log.Printf("WARNING: Bash is allowed without a sandbox: agents can run any command as this user")
	default:
		s.log.Printf("WARNING: Bash is allowed; it runs only in Claude Code's sandbox (on Linux it needs bubblewrap and socat)")
	}
	if len(exe) == 0 {
		s.log.Printf("WARNING: claude not found (install Claude Code or set \"claude\" in the config); no models")
	}
	return s, nil
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
	return srv.Serve(l)
}

// acceptLoop: napaka sprejema (npr. preveč odprtih datotek) → počakaj in poskusi znova; konec le ob zaprtem poslušanju.
// Največ 256 povezav hkrati v rokovanju; ostale se takoj zaprejo.
func (s *server) acceptLoop(l *serveListener) {
	defer close(l.done)
	sem := make(chan struct{}, 256)
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
		case sem <- struct{}{}:
		default:
			raw.Close()
			continue
		}
		go func() {
			c := s.accept(raw)
			<-sem
			if c == nil {
				return
			}
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
		s.log.Printf("%s refused: ChaCha20 (MGW1) connection; this router supports MGW2 (AES-GCM) only: update minigw", strconv.Quote(host))
	case !c.loopback():
		body := `{"type":"error","error":{"type":"permission_error","message":"Unencrypted connections from other computers are off: use minigw connect with the router key"}}`
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
	c.device = &serveDevice{ID: base64url(pub), Name: string(name)}
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
	status, model := s.handle(w, r)
	if hr.URL.Path == "/minigw/share/next" && status == 204 {
		return // long poll vsakih 25 s: brez vrstice
	}
	who := r.conn.peer
	if d := r.conn.device; d != nil {
		who += " " + strconv.Quote(d.Name)
	}
	if model == "" {
		model = "-"
	}
	s.log.Printf("%s %s %s %s model=%s status=%d %dms bytes=%d", r.rid, who, strconv.Quote(hr.Method), strconv.Quote(hr.URL.Path),
		strconv.Quote(model), status, time.Since(start).Milliseconds(), len(r.body))
}

// bigBodies: zahteve s telesom do 64 MB (kot router na macOS); ostale do 1 MB.
var bigBodies = map[string]bool{"PUT /minigw/sync/file": true, "POST /v1/messages": true, "POST /v1/messages/count_tokens": true}

// handle: overitev in naprava pred branjem telesa; telo z rokom (brez ReadTimeout, ki bi prekinil dolg odgovor).
func (s *server) handle(w http.ResponseWriter, r *req) (int, string) {
	switch r.Method + " " + r.URL.Path {
	case "GET /health/live":
		return writeJSON(w, r.rid, 200, map[string]string{"status": "ok"}), ""
	case "GET /health/ready":
		return writeJSON(w, r.rid, 200, map[string]any{"status": "ok", "providers": map[string]bool{"claude-cli": len(s.claude) > 0}}), ""
	}
	if !s.authorized(r) {
		return writeError(w, r.rid, 401, "Invalid or missing token"), ""
	}
	if !s.cfg.deviceAllowed(r.conn.device, r.conn.loopback()) {
		if d := r.conn.device; d != nil {
			s.log.Printf("%s refused unknown device %q %s; compare the ID with the client, then to allow it run: minigw serve allow %s %q", r.rid, d.Name, d.ID, d.ID, d.Name)
			return writeError(w, r.rid, 403, fmt.Sprintf("Device %q is not allowed on this router: on the router run minigw serve allow %s", d.Name, d.ID)), ""
		}
		return writeError(w, r.rid, 403, "This router needs a device key: update minigw and run minigw connect again"), ""
	}
	limit := int64(1 << 20)
	if bigBodies[r.Method+" "+r.URL.Path] {
		limit = maxSyncFile
	}
	rc := http.NewResponseController(w)
	rc.SetReadDeadline(time.Now().Add(120 * time.Second))
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	rc.SetReadDeadline(time.Time{})
	if err != nil {
		return writeError(w, r.rid, 413, "Request body too large or too slow"), ""
	}
	r.body = body
	return s.route(w, r)
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
	t := map[int]string{401: "authentication_error", 403: "permission_error", 404: "not_found_error", 413: "request_too_large"}[status]
	if t == "" {
		t = "invalid_request_error"
		if status >= 500 {
			t = "api_error"
		}
	}
	return writeJSON(w, rid, status, map[string]any{"type": "error", "error": map[string]string{"type": t, "message": msg}})
}

func writeStatus(w http.ResponseWriter, status int, ctype string, body []byte) int {
	if ctype != "" {
		w.Header().Set("Content-Type", ctype)
	}
	w.WriteHeader(status)
	w.Write(body)
	return status
}

func (s *server) authorized(r *req) bool {
	given := strings.TrimPrefix(r.h("authorization"), "Bearer ")
	if r.h("authorization") == "" {
		given = r.h("x-api-key")
	}
	return given != "" && subtle.ConstantTimeCompare([]byte(given), []byte(s.cfg.Token)) == 1
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
		if r.h("x-claude-mini-work") != "sync" { // brez priklopa WebDAV na Windows in Linux
			s.hub.refuse(session)
			return writeError(w, r.rid, 400, refusedDirect), ""
		}
		owner := dev
		if owner == "" {
			return writeError(w, r.rid, 400, "Sync needs an encrypted connection with a device key (run minigw connect again)"), ""
		}
		ro := r.h("x-share-mode") == "ro"
		suffix := ""
		if ro {
			suffix = "-ro" // samo za branje: ločena kopija
		}
		copyDir := filepath.Join(s.cfg.scratch("sync"), owner, sha256Hex([]byte(cwd))[:16]+suffix)
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
		need, kept, ok := s.hub.syncManifest(key, files, r.h("x-sync-exec") != "none", dev)
		if !ok {
			return writeError(w, r.rid, 404, "No such share"), ""
		}
		return writeJSON(w, r.rid, 200, map[string][]string{"need": need, "kept": kept}), ""
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

const refusedDirect = "this router supports --minigw-work=sync only (no folder mount on Windows or Linux): run minigw with MINIGW_WORK=sync"
