package main

import (
	"bytes"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

const remoteClientHeader = "X-Minigw-Client-IP"
const remoteCSRFHeader = "X-Minigw-Remote-Request"
const remoteBodyTimeout = 10 * time.Second

type remoteSettingsConfig struct {
	Enabled            bool     `json:"enabled"`
	Listen             string   `json:"listen"`
	Origin             string   `json:"origin"`
	AllowedSourceCIDRs []string `json:"allowedSourceCIDRs"`
	ProxyIdentity      string   `json:"proxyIdentity"`
	ProxyCA            string   `json:"proxyCA"`
	Certificate        string   `json:"certificate"`
	PrivateKey         string   `json:"privateKey"`
}

func (c *remoteSettingsConfig) UnmarshalJSON(data []byte) error {
	type record remoteSettingsConfig
	var v record
	if err := decodeStrict(data, &v); err != nil {
		return err
	}
	*c = remoteSettingsConfig(v)
	return nil
}

func remoteDNSName(s string) bool {
	if s == "" || len(s) > 253 || strings.ToLower(s) != s {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, ch := range label {
			if !(ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' || ch == '-') {
				return false
			}
		}
	}
	return true
}

func (c remoteSettingsConfig) authority() (string, error) {
	u, err := url.Parse(c.Origin)
	if err != nil || u.Scheme != "https" || u.Opaque != "" || u.User != nil || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(c.Origin, "#") || c.Origin != "https://"+u.Host {
		return "", errors.New("remoteSettings.origin must be a canonical HTTPS origin")
	}
	host := u.Hostname()
	if net.ParseIP(host) == nil && !remoteDNSName(host) {
		return "", errors.New("invalid remoteSettings origin host")
	}
	if strings.HasSuffix(u.Host, ":") {
		return "", errors.New("empty remoteSettings origin port")
	}
	if strings.Contains(u.Host, ":") && u.Host != "["+host+"]" {
		if _, _, err := net.SplitHostPort(u.Host); err != nil {
			return "", errors.New("invalid remoteSettings origin port")
		}
	}
	if u.Port() != "" {
		p, e := strconv.Atoi(u.Port())
		if e != nil || p < 1 || p > 65535 || strconv.Itoa(p) != u.Port() || p == 443 {
			return "", errors.New("invalid or noncanonical remoteSettings origin port")
		}
	}
	return u.Host, nil
}

func (c remoteSettingsConfig) tlsConfig(config string) (*tls.Config, error) {
	if !c.Enabled {
		return nil, nil
	}
	if _, err := c.authority(); err != nil {
		return nil, err
	}
	host, port, err := net.SplitHostPort(c.Listen)
	p, e := strconv.Atoi(port)
	if err != nil || net.ParseIP(host) == nil || e != nil || p < 1 || p > 65535 || strconv.Itoa(p) != port {
		return nil, errors.New("remoteSettings.listen needs an IP address and port")
	}
	if len(c.AllowedSourceCIDRs) == 0 || len(c.AllowedSourceCIDRs) > 256 {
		return nil, errors.New("remoteSettings needs bounded nonempty allowedSourceCIDRs")
	}
	for _, s := range c.AllowedSourceCIDRs {
		prefix, err := netip.ParsePrefix(s)
		if err != nil || prefix.Addr().Is4In6() || prefix != prefix.Masked() {
			return nil, errors.New("invalid remoteSettings source CIDR")
		}
	}
	if !remoteDNSName(c.ProxyIdentity) || net.ParseIP(c.ProxyIdentity) != nil || c.ProxyCA == "" || c.Certificate == "" || c.PrivateKey == "" {
		return nil, errors.New("remoteSettings needs TLS material and a proxy DNS certificate identity")
	}
	if _, _, err := loadRemotePassword(remotePasswordPath(config)); err != nil {
		return nil, fmt.Errorf("remoteSettings password: %w", err)
	}
	pair, err := tls.LoadX509KeyPair(c.Certificate, c.PrivateKey)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, err
	}
	if time.Now().Before(leaf.NotBefore) || !time.Now().Before(leaf.NotAfter) {
		return nil, errors.New("remoteSettings server certificate is not current")
	}
	caPEM, err := os.ReadFile(c.ProxyCA)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("invalid remoteSettings proxy CA")
	}
	for rest := caPEM; len(strings.TrimSpace(string(rest))) > 0; {
		block, tail := pem.Decode(rest)
		if block == nil || block.Type != "CERTIFICATE" {
			return nil, errors.New("invalid remoteSettings proxy CA")
		}
		ca, err := x509.ParseCertificate(block.Bytes)
		if err != nil || !ca.IsCA || time.Now().Before(ca.NotBefore) || !time.Now().Before(ca.NotAfter) {
			return nil, errors.New("invalid or expired remoteSettings proxy CA")
		}
		rest = tail
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{pair}, ClientCAs: pool, ClientAuth: tls.RequireAndVerifyClientCert,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if !remoteProxyVerified(&cs, c.ProxyIdentity) {
				return errors.New("unauthorized settings proxy")
			}
			return nil
		}}, nil
}

func remoteProxyVerified(cs *tls.ConnectionState, identity string) bool {
	return cs != nil && len(cs.PeerCertificates) > 0 && len(cs.VerifiedChains) > 0 && len(cs.VerifiedChains[0]) > 0 && cs.VerifiedChains[0][0].Equal(cs.PeerCertificates[0]) && cs.PeerCertificates[0].VerifyHostname(identity) == nil
}

func remoteTrustChanged(a, b remoteSettingsConfig) bool {
	a.Enabled, b.Enabled = false, false
	return !reflect.DeepEqual(a, b)
}

func (s *server) startRemoteSettings() (*http.Server, error) {
	c := s.conf().RemoteSettings
	if !c.Enabled {
		return nil, nil
	}
	if s.remoteAuth == nil { // the lockout state was unusable at startup (see newServer): remote login stays off
		return nil, nil
	}
	tc, err := c.tlsConfig(s.cfgPath)
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", c.Listen)
	if err != nil {
		return nil, err
	}
	srv := &http.Server{Handler: s.remoteSettingsHandler(c), TLSConfig: tc, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: remoteBodyTimeout, WriteTimeout: 30 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
	go func() {
		if err := srv.Serve(tls.NewListener(limitListener(ln, remoteMaxConns), tc)); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.log.Printf("remote settings stopped: %v", err)
		}
	}()
	return srv, nil
}

const remoteMaxConns = 64

type limitedListener struct {
	net.Listener
	slots chan struct{}
}

// limitListener allows n open connections at once; Accept waits for a free slot.
func limitListener(l net.Listener, n int) net.Listener {
	return &limitedListener{l, make(chan struct{}, n)}
}

func (l *limitedListener) Accept() (net.Conn, error) {
	l.slots <- struct{}{}
	c, err := l.Listener.Accept()
	if err != nil {
		<-l.slots
		return nil, err
	}
	return &limitedConn{Conn: c, release: func() { <-l.slots }}, nil
}

type limitedConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *limitedConn) Close() error {
	defer c.once.Do(c.release)
	return c.Conn.Close()
}

// remoteProviderPatchRefusal: a remote PATCH of a provider may only carry name, enabled and aliases, each once with
// exactly this spelling (the shared handler matches keys case-insensitively and lets the last duplicate win). baseURL
// and insecure would send the stored API key to another host; tools and model raise what agents may do.
func remoteProviderPatchRefusal(body []byte) string {
	_, msg := remoteBodyObject(body, func(key string) string {
		switch key {
		case "name", "enabled", "aliases":
			return ""
		}
		return "Changing " + strconv.Quote(key) + " is not allowed remotely: it needs the API key again, so add the provider again with its key, or make the change locally."
	})
	return msg
}

// remoteBodyObject reads a JSON object once, key by key: every key must have exactly the spelling refuse accepts (it returns ""
// for an allowed key, else the refusal text), no key may repeat and nothing may follow the object. Returns the raw values.
func remoteBodyObject(body []byte, refuse func(key string) string) (map[string]json.RawMessage, string) {
	const notObject = "The request body must be a JSON object."
	d := json.NewDecoder(bytes.NewReader(body))
	if t, err := d.Token(); err != nil || t != json.Delim('{') {
		return nil, notObject
	}
	out := map[string]json.RawMessage{}
	for d.More() {
		t, err := d.Token()
		key, ok := t.(string)
		if err != nil || !ok {
			return nil, notObject
		}
		var v json.RawMessage
		if d.Decode(&v) != nil {
			return nil, notObject
		}
		if _, dup := out[key]; dup {
			return nil, "Duplicate key " + strconv.Quote(key) + " in the request body."
		}
		if msg := refuse(key); msg != "" {
			return nil, msg
		}
		out[key] = v
	}
	if t, err := d.Token(); err != nil || t != json.Delim('}') {
		return nil, notObject
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, notObject
	}
	return out, ""
}

// remoteKBDecisionRefusal: the body of a remote review decision. Accept carries exactly {"reviewHash": 64 lowercase hex} (the
// version the owner read), reject exactly {"reason": text up to 1024 bytes}. The core's decoder matches keys case-insensitively
// and lets the last duplicate win, so the keys are checked here, before anything is dispatched.
func remoteKBDecisionRefusal(accept bool, body []byte) string {
	want := "reason"
	if accept {
		want = "reviewHash"
	}
	vals, msg := remoteBodyObject(body, func(key string) string {
		if key == want {
			return ""
		}
		return "The request body may only carry " + strconv.Quote(want) + ", spelled exactly so."
	})
	if msg != "" {
		return msg
	}
	raw, ok := vals[want]
	var v string
	if !ok || json.Unmarshal(raw, &v) != nil {
		return "The request body must carry " + strconv.Quote(want) + " as a string."
	}
	if accept {
		if len(v) != 64 || strings.Trim(v, "0123456789abcdef") != "" {
			return "reviewHash must be the 64 lowercase hexadecimal characters of the version you read."
		}
	} else {
		if len(v) > 1024 {
			return "The reason is longer than 1024 bytes."
		}
		for _, r := range v { // the core refuses these too; refused here they never use up a decision
			if unicode.IsControl(r) {
				return "The reason may not contain line breaks, tabs or other control characters."
			}
		}
	}
	return ""
}

// remoteKBAcceptRefusal: the answer of a fresh review read (the one GET uses) decides whether a remote accept is even offered:
// only a text that reads "ok" and replaces no note. A replacement overwrites the note it replaces and keeps the old text
// nowhere, and this listener cannot show that text; hidden characters and long texts are reviewed on the server. The core still
// compares the review hash inside its write transaction, and the hash covers the replaced id, so nothing can change between this read and the write.
func (s *server) remoteKBAcceptRefusal(id string) string {
	status, obj := s.adminDo("GET", "/kb/submissions/"+id, nil)
	m, ok := obj.(map[string]any)
	if status != 200 || !ok {
		return "" // not found, no database: the core answers the same way
	}
	if r, _ := m["replaces"].(float64); r != 0 {
		return fmt.Sprintf("This submission replaces note %d: accepting it overwrites that note, and its current text cannot be shown here. Review it on the server, or reject it.", int64(r))
	}
	switch m["reviewCheck"] {
	case "ok":
		return ""
	case "hidden":
		return "This submission contains hidden characters and cannot be accepted: reject it and ask for the text without them."
	}
	return "This submission is too long to review here: review it on the server (minigw gw kb review, kb accept), or reject it."
}

// remoteKBDecisionPath: POST kb/submissions/{id}/accept|reject (seg is the path below the admin prefix, split).
func remoteKBDecisionPath(seg []string) bool {
	return len(seg) == 4 && seg[0] == "kb" && seg[1] == "submissions" && (seg[3] == "accept" || seg[3] == "reject")
}

// remoteNumericID: 1 to 18 ASCII digits (no sign, no space), the same as [0-9]{1,18} in the Caddyfile.
func remoteNumericID(s string) bool {
	if len(s) < 1 || len(s) > 18 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// Keep core, agents, host actions and loopback link creation outside this listener.
func remoteAdminAllowed(method, path string) bool {
	if !strings.HasPrefix(path, adminPrefix+"/") {
		return false
	}
	seg := strings.Split(strings.TrimPrefix(path, adminPrefix+"/"), "/")
	for _, v := range seg {
		if v == "" || v == "." || v == ".." || strings.ContainsAny(v, "%\\?#") {
			return false
		}
	}
	if len(seg) == 1 {
		switch seg[0] {
		case "providers", "devices":
			return method == "GET" || method == "POST"
		case "users", "bindings", "teams", "projects": // read-only lists; changes stay with the CLI
			return method == "GET"
		}
	}
	// KB review: list and read pending submissions, accept or reject the version read. Accept is the one entry below to drop
	// (and "accept" in the Caddyfile @post, and the page's button) to make the remote review reject-only.
	if len(seg) >= 2 && seg[0] == "kb" && seg[1] == "submissions" {
		switch {
		case len(seg) == 2:
			return method == "GET"
		case len(seg) == 3:
			return method == "GET" && remoteNumericID(seg[2])
		case len(seg) == 4 && remoteNumericID(seg[2]):
			return method == "POST" && (seg[3] == "accept" || seg[3] == "reject")
		}
		return false
	}
	if len(seg) == 2 && seg[0] == "teams" { // read-only team detail: one numeric id, GET only
		return method == "GET" && remoteNumericID(seg[1])
	}
	if len(seg) == 3 && method == "GET" && remoteNumericID(seg[1]) && (seg[0] == "teams" && seg[2] == "assignments" || seg[0] == "projects" && seg[2] == "members") {
		return true // who is in a team, what it assigns, who has access to a project and why: read-only
	}
	if len(seg) == 2 {
		switch seg[0] {
		case "providers":
			return method == "PATCH" || method == "DELETE"
		case "devices":
			return method == "PATCH" || method == "DELETE"
		}
	}
	if len(seg) == 3 {
		if seg[0] == "providers" && seg[2] == "models" {
			return method == "GET" || method == "POST"
		}
		if seg[0] == "devices" && seg[2] == "history" {
			return method == "GET"
		}
	}
	return false
}

func remoteSingleHeader(r *http.Request, key string) (string, bool) {
	v := r.Header.Values(key)
	return r.Header.Get(key), len(v) == 1 && v[0] != "" && !strings.Contains(v[0], ",")
}

func (s *server) remoteSettingsHandler(c remoteSettingsConfig) http.Handler {
	authority, _ := c.authority()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Cache-Control", "no-store")
		h.Set("X-Frame-Options", "DENY")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		var raw [16]byte
		if _, err := rand.Read(raw[:]); err != nil {
			http.Error(w, "Unavailable", 503)
			return
		}
		rid := "req_" + hex.EncodeToString(raw[:])
		deny := func(code int) { writeError(w, rid, code, "Remote settings request refused") }
		if !s.conf().RemoteSettings.Enabled || s.remoteAuth == nil {
			deny(403)
			return
		}
		if !remoteProxyVerified(r.TLS, c.ProxyIdentity) {
			deny(403)
			return
		}
		ip, ok := remoteSingleHeader(r, remoteClientHeader)
		addr, err := netip.ParseAddr(ip)
		if !ok || err != nil || addr.Zone() != "" {
			deny(403)
			return
		}
		addr = addr.Unmap()
		ip = addr.String()
		allowed := false
		for _, cidr := range c.AllowedSourceCIDRs {
			prefix, e := netip.ParsePrefix(cidr)
			if e == nil && prefix.Contains(addr) {
				allowed = true
			}
		}
		if !allowed {
			deny(403)
			return
		}
		if r.Host != authority || r.URL.IsAbs() || r.URL.RawPath != "" || strings.ContainsAny(r.URL.Path, "%\\?#") {
			deny(403)
			return
		}
		for key := range r.Header {
			k := strings.ToLower(key)
			if k == "x-http-method-override" || k == "x-http-method" || k == "x-method-override" { // Go ignores them; refused so a later proxy or handler cannot turn a GET into a change
				deny(403)
				return
			}
			if k == "authorization" || k == "proxy-authorization" || strings.HasPrefix(k, "x-claude-") || strings.HasPrefix(k, "x-minigw-") && k != strings.ToLower(remoteSessionHeader) && k != strings.ToLower(remoteClientHeader) && k != strings.ToLower(remoteCSRFHeader) {
				deny(401)
				return
			}
		}
		if r.Method == "OPTIONS" {
			deny(405)
			return
		}
		if vals := r.Header.Values("Origin"); len(vals) > 0 && (len(vals) != 1 || vals[0] != c.Origin) {
			deny(403)
			return
		}
		if vals := r.Header.Values("Sec-Fetch-Site"); len(vals) > 0 && (len(vals) != 1 || vals[0] != "same-origin" && vals[0] != "none") {
			deny(403)
			return
		}
		mutation := r.Method != "GET"
		if mutation {
			origin, one := remoteSingleHeader(r, "Origin")
			csrf, custom := remoteSingleHeader(r, remoteCSRFHeader)
			media, _, e := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if !one || origin != c.Origin || !custom || csrf != "1" || e != nil || media != "application/json" || len(r.Header.Values("Content-Type")) != 1 {
				deny(403)
				return
			}
			if vals := r.Header.Values("Sec-Fetch-Mode"); len(vals) > 0 && (len(vals) != 1 || vals[0] != "cors" && vals[0] != "same-origin") {
				deny(403)
				return
			}
			if vals := r.Header.Values("Sec-Fetch-Dest"); len(vals) > 0 && (len(vals) != 1 || vals[0] != "empty") {
				deny(403)
				return
			}
		}
		path := r.URL.Path
		login := r.Method == "POST" && path == "/minigw/settings/login"
		logout := r.Method == "POST" && path == "/minigw/settings/logout"
		page := r.Method == "GET" && path == "/minigw/settings"
		if r.URL.RawQuery != "" || !login && !logout && !page && !remoteAdminAllowed(r.Method, path) {
			deny(404)
			return
		}
		if page {
			s.remoteSettingsPage(w)
			return
		}
		token, one := remoteSingleHeader(r, remoteSessionHeader)
		if !login && (!one || !s.remoteAuth.valid(token)) {
			deny(401)
			return
		}
		limit := int64(1 << 20)
		if login || logout {
			limit = 4 << 10
		}
		pathSeg := strings.Split(strings.TrimPrefix(path, adminPrefix+"/"), "/")
		kbDecision := mutation && remoteKBDecisionPath(pathSeg)
		if kbDecision {
			limit = 8 << 10 // a reason of 1024 bytes needs at most 6 KiB escaped
		}
		rc := http.NewResponseController(w)
		if err := rc.SetReadDeadline(time.Now().Add(remoteBodyTimeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			deny(413)
			return
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
		rc.SetReadDeadline(time.Time{})
		if err != nil {
			deny(413)
			return
		}
		if login {
			var b struct {
				Password string `json:"password"`
			}
			if decodeStrict(body, &b) != nil {
				deny(400)
				return
			}
			s.remoteAuth.loginResponse(w, ip, rid, b.Password)
			return
		}
		if logout {
			if s.remoteAuth.logout(token, ip, rid) != nil {
				deny(500)
				return
			}
			writeJSON(w, rid, 200, map[string]bool{"ok": true})
			return
		}
		if mutation && r.Method == "PATCH" && strings.HasPrefix(path, adminPrefix+"/providers/") {
			if msg := remoteProviderPatchRefusal(body); msg != "" {
				writeError(w, rid, 403, msg)
				return
			}
		}
		operation, subject := "", ""
		if kbDecision {
			// Every refusal a decision can get without the core being asked comes first and never counts against the limiter:
			// a malformed body, and an accept that a fresh review read says cannot be accepted here.
			operation, subject = "POST kb/submissions/{}/"+pathSeg[3], pathSeg[2]
			code, msg := 403, remoteKBDecisionRefusal(pathSeg[3] == "accept", body)
			if msg == "" && pathSeg[3] == "accept" {
				msg = s.remoteKBAcceptRefusal(pathSeg[2])
			}
			if msg != "" {
				s.remoteAuth.mu.Lock()
				err = s.remoteAuth.auditOperation("mutation", "denied", ip, rid, operation, subject)
				s.remoteAuth.mu.Unlock()
				if err != nil {
					deny(500)
					return
				}
				writeError(w, rid, code, msg)
				return
			}
		}
		// Audit intent before dispatch, so a broken audit log prevents mutations.
		if mutation {
			seg := pathSeg
			if !kbDecision { // a decision's operation and subject are set above
				operation = r.Method + " " + seg[0]
				if len(seg) > 1 {
					operation += "/{}"
				}
				if len(seg) > 2 {
					operation += "/models"
				}
			}
			if kbDecision {
				if wait := s.remoteAuth.allowDecision(); wait > 0 {
					s.remoteAuth.mu.Lock()
					err = s.remoteAuth.auditOperation("mutation", "limited", ip, rid, operation, subject)
					s.remoteAuth.mu.Unlock()
					if err != nil {
						deny(500)
						return
					}
					w.Header().Set("Retry-After", strconv.Itoa(int(wait/time.Second)+1))
					writeError(w, rid, 429, "Too many review decisions in a short time: wait a few minutes.")
					return
				}
			}
			s.remoteAuth.mu.Lock()
			err = s.remoteAuth.auditOperation("mutation", "started", ip, rid, operation, subject)
			s.remoteAuth.mu.Unlock()
			if err != nil {
				deny(500)
				return
			}
		}
		status, obj := s.adminDo(r.Method, strings.TrimPrefix(path, adminPrefix), body)
		if mutation {
			result := "ok"
			if status >= 400 {
				result = "denied"
			}
			s.remoteAuth.mu.Lock()
			err = s.remoteAuth.auditOperation("mutation", result, ip, rid, operation, subject)
			s.remoteAuth.mu.Unlock()
			if err != nil {
				writeError(w, rid, 500, "The operation may have completed; its audit result could not be saved. Check the settings before retrying.")
				return
			}
		}
		writeJSON(w, rid, status, obj)
	})
}

func (s *server) remoteSettingsPage(w http.ResponseWriter) {
	writeSettingsPage(w, false, true)
}
