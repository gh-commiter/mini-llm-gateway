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
	d := json.NewDecoder(bytes.NewReader(body))
	if t, err := d.Token(); err != nil || t != json.Delim('{') {
		return "The request body must be a JSON object."
	}
	seen := map[string]bool{}
	for d.More() {
		t, err := d.Token()
		key, ok := t.(string)
		if err != nil || !ok {
			return "The request body must be a JSON object."
		}
		var skip json.RawMessage
		if d.Decode(&skip) != nil {
			return "The request body must be a JSON object."
		}
		if seen[key] {
			return "Duplicate key " + strconv.Quote(key) + " in the request body."
		}
		seen[key] = true
		switch key {
		case "name", "enabled", "aliases":
		default:
			return "Changing " + strconv.Quote(key) + " is not allowed remotely: it needs the API key again, so add the provider again with its key, or make the change locally."
		}
	}
	return ""
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
		}
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
		// Audit intent before dispatch, so a broken audit log prevents mutations.
		operation := ""
		if mutation {
			seg := strings.Split(strings.TrimPrefix(path, adminPrefix+"/"), "/")
			operation = r.Method + " " + seg[0]
			if len(seg) > 1 {
				operation += "/{}"
			}
			if len(seg) > 2 {
				operation += "/models"
			}
			s.remoteAuth.mu.Lock()
			err = s.remoteAuth.auditOperation("mutation", "started", ip, rid, operation)
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
			err = s.remoteAuth.auditOperation("mutation", result, ip, rid, operation)
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
