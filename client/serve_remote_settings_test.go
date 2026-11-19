package main

import (
	"bufio"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

type remoteTransportFixture struct {
	s        *server
	c        remoteSettingsConfig
	config   string
	proxy    tls.Certificate
	other    tls.Certificate
	foreign  tls.Certificate
	expired  tls.Certificate
	pool     *x509.CertPool
	verified *tls.ConnectionState
	token    string
}

func remoteTransport(t *testing.T) *remoteTransportFixture {
	t.Helper()
	a, config, _ := remoteFixture(t)
	f := &remoteTransportFixture{config: config}
	pub, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, pub, key)
	if err != nil {
		t.Fatal(err)
	}
	root, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	f.pool = x509.NewCertPool()
	f.pool.AddCert(root)
	dir := filepath.Dir(config)
	caPath := filepath.Join(dir, "proxy-ca.pem")
	if err := os.WriteFile(caPath, caPEM, 0600); err != nil {
		t.Fatal(err)
	}
	serial := int64(2)
	cert := func(name string, usages []x509.ExtKeyUsage, expired, foreign bool) (tls.Certificate, []byte, []byte) {
		t.Helper()
		serial++
		pub, k, e := ed25519.GenerateKey(rand.Reader)
		if e != nil {
			t.Fatal(e)
		}
		leaf := &x509.Certificate{SerialNumber: big.NewInt(serial), DNSNames: []string{name}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: usages}
		if expired {
			leaf.NotAfter = time.Now().Add(-time.Minute)
		}
		parent, signer := ca, key
		if foreign {
			parent = leaf
			signer = k
		}
		der, e := x509.CreateCertificate(rand.Reader, leaf, parent, pub, signer)
		if e != nil {
			t.Fatal(e)
		}
		b := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
		kd, e := x509.MarshalPKCS8PrivateKey(k)
		if e != nil {
			t.Fatal(e)
		}
		kb := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kd})
		pair, e := tls.X509KeyPair(b, kb)
		if e != nil {
			t.Fatal(e)
		}
		pair.Leaf, e = x509.ParseCertificate(der)
		if e != nil {
			t.Fatal(e)
		}
		return pair, b, kb
	}
	_, b, kb := cert("settings.internal", []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, false, false)
	cp, kp := filepath.Join(dir, "server.pem"), filepath.Join(dir, "server.key")
	if err := os.WriteFile(cp, b, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(kp, kb, 0600); err != nil {
		t.Fatal(err)
	}
	f.proxy, _, _ = cert("proxy.internal", []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, false, false)
	f.other, _, _ = cert("other.internal", []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, false, false)
	f.foreign, _, _ = cert("proxy.internal", []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, false, true)
	f.expired, _, _ = cert("proxy.internal", []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, true, false)
	f.c = remoteSettingsConfig{Enabled: true, Listen: "127.0.0.1:8766", Origin: "https://settings.example", AllowedSourceCIDRs: []string{"192.0.2.0/24"}, ProxyIdentity: "proxy.internal", ProxyCA: caPath, Certificate: cp, PrivateKey: kp}
	f.s = &server{cfg: serveConfig{RemoteSettings: f.c}, cfgPath: config, remoteAuth: a}
	f.verified = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{f.proxy.Leaf}, VerifiedChains: [][]*x509.Certificate{{f.proxy.Leaf, root}}}
	f.token = remoteLogin(t, a)
	return f
}

func (f *remoteTransportFixture) request(method, path, body string) *http.Request {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Host = "settings.example"
	r.TLS = f.verified
	r.Header.Set(remoteClientHeader, "192.0.2.1")
	r.Header.Set(remoteSessionHeader, f.token)
	r.Header.Set("Origin", f.c.Origin)
	r.Header.Set(remoteCSRFHeader, "1")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	return r
}

func (f *remoteTransportFixture) run(r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	f.s.remoteSettingsHandler(f.c).ServeHTTP(w, r)
	return w
}

func TestRemoteSettingsBoundary(t *testing.T) {
	f := remoteTransport(t)
	for _, tc := range []struct {
		name   string
		change func(*http.Request)
		status int
	}{
		{"valid", func(r *http.Request) {}, 200},
		{"no TLS", func(r *http.Request) { r.TLS = nil }, 403},
		{"unverified spoof", func(r *http.Request) { r.TLS = &tls.ConnectionState{PeerCertificates: f.verified.PeerCertificates} }, 403},
		{"wrong proxy identity", func(r *http.Request) {
			r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{f.other.Leaf}, VerifiedChains: [][]*x509.Certificate{{f.other.Leaf}}}
		}, 403},
		{"source outside policy", func(r *http.Request) { r.Header.Set(remoteClientHeader, "203.0.113.1") }, 403},
		{"missing source", func(r *http.Request) { r.Header.Del(remoteClientHeader); r.Header.Set("X-Forwarded-For", "192.0.2.1") }, 403},
		{"multiple source lines", func(r *http.Request) { r.Header.Add(remoteClientHeader, "192.0.2.2") }, 403},
		{"multiple source values", func(r *http.Request) { r.Header.Set(remoteClientHeader, "192.0.2.1, 192.0.2.2") }, 403},
		{"bad source", func(r *http.Request) { r.Header.Set(remoteClientHeader, "not-ip") }, 403},
		{"zone source", func(r *http.Request) { r.Header.Set(remoteClientHeader, "fe80::1%eth0") }, 403},
		{"source not TCP peer", func(r *http.Request) {
			r.RemoteAddr = "203.0.113.1:12"
			r.Header.Set("Forwarded", "for=203.0.113.1")
			r.Header.Set("X-Forwarded-For", "203.0.113.1")
		}, 200},
		{"wrong host", func(r *http.Request) { r.Host = "evil.example" }, 403},
		{"uppercase host", func(r *http.Request) { r.Host = "Settings.example" }, 403},
		{"trailing dot host", func(r *http.Request) { r.Host = "settings.example." }, 403},
		{"absolute target", func(r *http.Request) { r.URL.Scheme = "https"; r.URL.Host = "settings.example" }, 403},
		{"mapped source", func(r *http.Request) { r.Header.Set(remoteClientHeader, "::ffff:192.0.2.1") }, 200},
		{"wrong port", func(r *http.Request) { r.Host = "settings.example:444" }, 403},
		{"wrong origin", func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }, 403},
		{"duplicate origin", func(r *http.Request) { r.Header.Add("Origin", f.c.Origin) }, 403},
		{"cross site", func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }, 403},
		{"same site", func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "same-site") }, 403},
		{"missing session", func(r *http.Request) { r.Header.Del(remoteSessionHeader) }, 401},
		{"duplicate session", func(r *http.Request) { r.Header.Add(remoteSessionHeader, f.token) }, 401},
		{"wrong session", func(r *http.Request) { r.Header.Set(remoteSessionHeader, strings.Repeat("a", 64)) }, 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := f.request("GET", adminPrefix+"/providers", "")
			tc.change(r)
			w := f.run(r)
			if w.Code != tc.status {
				t.Fatalf("%d: %s", w.Code, w.Body.String())
			}
			if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Access-Control-Allow-Origin") != "" {
				t.Fatal(w.Header())
			}
		})
	}
}

func TestRemoteSettingsAlternateCredentials(t *testing.T) {
	f := remoteTransport(t)
	local, _ := f.s.settings.login(f.s.settings.newLogin())
	for _, tc := range []struct{ key, value string }{
		{"Authorization", "Bearer gateway-token"}, {"Authorization", adminScheme + "proof"}, {"Authorization", "Basic abc"}, {"Authorization", ""},
		{settingsHeader, local}, {"X-Minigw-Device", "paired"}, {agentChainHeader, "agent"}, {projectHeader, "1"}, {"X-Claude-Mini-Role", "agent"}, {"Proxy-Authorization", "Bearer token"},
	} {
		t.Run(tc.key+tc.value, func(t *testing.T) {
			r := f.request("GET", adminPrefix+"/providers", "")
			r.Header[http.CanonicalHeaderKey(tc.key)] = []string{tc.value}
			if w := f.run(r); w.Code != 401 {
				t.Fatal(w.Code, w.Body.String())
			}
		})
	}
	// A local session is never accepted as a remote session either.
	r := f.request("GET", adminPrefix+"/providers", "")
	r.Header.Set(remoteSessionHeader, local)
	if w := f.run(r); w.Code != 401 {
		t.Fatal(w.Code)
	}
	if f.s.settings.valid(f.token) {
		t.Fatal("remote session accepted locally")
	}
}

func TestRemoteSettingsCSRF(t *testing.T) {
	f := remoteTransport(t)
	for _, path := range []string{"/minigw/settings/login", "/minigw/settings/logout", adminPrefix + "/providers"} {
		for _, tc := range []struct {
			name   string
			change func(*http.Request)
			status int
		}{
			{"no origin", func(r *http.Request) { r.Header.Del("Origin") }, 403},
			{"null origin", func(r *http.Request) { r.Header.Set("Origin", "null") }, 403},
			{"no custom header", func(r *http.Request) { r.Header.Del(remoteCSRFHeader) }, 403},
			{"wrong custom header", func(r *http.Request) { r.Header.Set(remoteCSRFHeader, "0") }, 403},
			{"form", func(r *http.Request) { r.Header.Set("Content-Type", "application/x-www-form-urlencoded") }, 403},
			{"text", func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, 403},
			{"cross site", func(r *http.Request) { r.Header.Set("Sec-Fetch-Site", "cross-site") }, 403},
			{"navigate", func(r *http.Request) { r.Header.Set("Sec-Fetch-Mode", "navigate") }, 403},
			{"document", func(r *http.Request) { r.Header.Set("Sec-Fetch-Dest", "document") }, 403},
			{"preflight", func(r *http.Request) { r.Method = "OPTIONS"; r.Header.Set("Access-Control-Request-Method", "POST") }, 405},
		} {
			t.Run(path+tc.name, func(t *testing.T) {
				r := f.request("POST", path, `{}`)
				tc.change(r)
				if w := f.run(r); w.Code != tc.status {
					t.Fatal(w.Code, w.Body.String())
				}
			})
		}
	}
}

func TestRemoteSettingsPathPolicy(t *testing.T) {
	f := remoteTransport(t)
	for _, path := range []string{"/v1/messages", "/v1/models", "/minigw/v1/projects", "/minigw/v1/member", "/minigw/sync/file", "/MGW2", adminPrefix + "/sync", adminPrefix + "/panels", adminPrefix + "/unknown", adminPrefix + "/general", adminPrefix + "/general/token", adminPrefix + "/status", adminPrefix + "/settings/login", "/minigw/settings/pick", adminPrefix + "/agents/claude", adminPrefix + "/general/scratch/move", adminPrefix + "/providers/../general", adminPrefix + "//providers", adminPrefix + "/providers?token=secret"} {
		for _, method := range []string{"GET", "POST", "PATCH", "DELETE", "PUT", "HEAD"} {
			r := f.request(method, path, `{}`)
			w := f.run(r)
			if w.Code != 404 && w.Code != 403 {
				t.Fatalf("%s %s: %d %s", method, path, w.Code, w.Body.String())
			}
		}
	}
	// The four read-only lists: GET only; every other method, deeper path, encoding, query string and override header stays refused.
	for _, name := range []string{"users", "bindings", "teams", "projects"} {
		for _, method := range []string{"POST", "PATCH", "DELETE", "PUT", "HEAD", "OPTIONS"} {
			if w := f.run(f.request(method, adminPrefix+"/"+name, `{}`)); w.Code != 404 && w.Code != 403 && w.Code != 405 {
				t.Fatalf("%s %s: %d", method, name, w.Code)
			}
		}
		for _, h := range []string{"X-HTTP-Method-Override", "X-HTTP-Method", "X-Method-Override"} {
			for _, method := range []string{"GET", "POST"} {
				r := f.request(method, adminPrefix+"/"+name, `{}`)
				r.Header.Set(h, "DELETE")
				if w := f.run(r); w.Code != 403 {
					t.Fatalf("override %s %s %s: %d", h, method, name, w.Code)
				}
			}
		}
		for _, path := range []string{"/" + name + "/", "//" + name, "/" + name + "?x=1", "/" + name[:1] + "%" + "65" + name[1:], "/" + name + "%2F1", "/%2e%2e/" + name, "/" + name + "\\1"} {
			for _, method := range []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD"} {
				if w := f.run(f.request(method, adminPrefix+path, `{}`)); w.Code != 404 && w.Code != 403 {
					t.Fatalf("%s %s: %d", method, path, w.Code)
				}
			}
		}
	}
	for _, path := range []string{"/users/1", "/bindings/1", "/projects/1", "/projects/1/depends", "/grants", "/audit", "/kb", "/panels", "/upstreams", "/export"} {
		for _, method := range []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD"} {
			if w := f.run(f.request(method, adminPrefix+path, `{}`)); w.Code != 404 && w.Code != 403 {
				t.Fatalf("%s %s: %d", method, path, w.Code)
			}
		}
	}
	// The three id routes: GET only, exactly one numeric id (1-18 digits), the same in the Caddyfile.
	for _, path := range []string{"/teams/1", "/teams/0", "/teams/123456789012345678", "/teams/1/assignments", "/projects/1/members", "/projects/123456789012345678/members"} {
		if !remoteAdminAllowed("GET", adminPrefix+path) {
			t.Fatal("GET refused", path)
		}
		for _, method := range []string{"POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"} {
			if remoteAdminAllowed(method, adminPrefix+path) {
				t.Fatal(method, "allowed", path)
			}
			if w := f.run(f.request(method, adminPrefix+path, `{}`)); w.Code != 404 && w.Code != 403 && w.Code != 405 {
				t.Fatalf("%s %s: %d", method, path, w.Code)
			}
		}
	}
	for _, path := range []string{"/teams/1234567890123456789", "/teams/1a", "/teams/-1", "/teams/+1", "/teams/%31", "/teams/1/", "/teams/1/members", "/teams/1/members/2", "/teams/1/assignments/2", "/teams/1/depends", "/teams/1/projects", "/projects/1", "/projects/1a/members", "/projects/-1/members", "/projects/+1/members", "/projects/%31/members", "/projects/1234567890123456789/members", "/projects/1/members/", "/projects/1/members/2", "/projects/1/grants", "/projects/1/depends", "/projects/1/remotes", "/projects/1/assignments", "/projects/members", "/teams/%201", "/teams/١"} {
		for _, method := range []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD"} {
			if remoteAdminAllowed(method, adminPrefix+path) {
				t.Fatal(method, "allowed", path)
			}
			if w := f.run(f.request(method, adminPrefix+path, `{}`)); w.Code != 404 && w.Code != 403 {
				t.Fatalf("%s %s: %d", method, path, w.Code)
			}
		}
	}
	for _, tc := range []struct{ method, path string }{{"GET", "users"}, {"GET", "bindings"}, {"GET", "teams"}, {"GET", "projects"}, {"GET", "providers"}, {"POST", "providers"}, {"PATCH", "providers/example"}, {"DELETE", "providers/example"}, {"GET", "providers/example/models"}, {"POST", "providers/example/models"}, {"GET", "devices"}, {"POST", "devices"}, {"PATCH", "devices/id"}, {"DELETE", "devices/id"}, {"GET", "devices/id/history"}} {
		if !remoteAdminAllowed(tc.method, adminPrefix+"/"+tc.path) {
			t.Fatal(tc)
		}
	}
	r := f.request("PATCH", adminPrefix+"/general", `{"startAtSignIn":true}`)
	if w := f.run(r); w.Code != 404 {
		t.Fatal("host install reached", w.Code)
	}
	r = f.request("GET", adminPrefix+"/pro%76iders", "")
	if w := f.run(r); w.Code != 403 {
		t.Fatal("encoded path reached", w.Code)
	}
	for _, path := range []string{adminPrefix + "/providers/name%3Fquery", adminPrefix + "/providers/name%23fragment"} {
		if w := f.run(f.request("DELETE", path, `{}`)); w.Code != 403 {
			t.Fatal("encoded separator reached", w.Code, path)
		}
	}
}

func TestRemoteSettingsLoginLogoutAndBodyLimit(t *testing.T) {
	f := remoteTransport(t)
	r := f.request("POST", "/minigw/settings/login", `{"password":"wrong"}`)
	if w := f.run(r); w.Code != 401 {
		t.Fatal(w.Code, w.Body.String())
	}
	r = f.request("POST", "/minigw/settings/login", `{"password":"`+remoteTestPassword+`"}`)
	w := f.run(r)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var j struct {
		Session string `json:"session"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &j); err != nil {
		t.Fatal(err)
	}
	r = f.request("POST", "/minigw/settings/logout", `{}`)
	r.Header.Set(remoteSessionHeader, j.Session)
	if w := f.run(r); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if f.s.remoteAuth.valid(j.Session) {
		t.Fatal("logout did not revoke")
	}
	for _, tc := range []struct {
		path string
		size int
	}{{"/minigw/settings/login", 4097}, {adminPrefix + "/providers", (1 << 20) + 1}} {
		r = f.request("POST", tc.path, strings.Repeat(" ", tc.size))
		if w := f.run(r); w.Code != 413 {
			t.Fatal(tc.path, w.Code)
		}
	}
	r = f.request("POST", "/minigw/settings/login", `{"password":"bad","extra":true}`)
	if w := f.run(r); w.Code != 400 {
		t.Fatal(w.Code)
	}
}

func TestRemoteSettingsConfigValidation(t *testing.T) {
	f := remoteTransport(t)
	var cfg serveConfig
	if err := json.Unmarshal([]byte(`{"remoteSettings":{"enabled":true,"trustAll":true}}`), &cfg); err == nil {
		t.Fatal("unknown trust option accepted")
	}
	if _, err := f.c.tlsConfig(f.config); err != nil {
		t.Fatal(err)
	}
	if tc, err := (remoteSettingsConfig{}).tlsConfig("missing"); tc != nil || err != nil {
		t.Fatal("default not disabled")
	}
	for _, origin := range []string{"http://settings.example", "https://*.example", "https://settings.example/", "https://settings.example/path", "https://user@settings.example", "https://settings.example?x=y", "https://settings.example#x", "https://settings.example:443", "https://settings.example:0", "https://settings.example:65536", "https://Settings.example", "https://settings.example:", "https://settings.example#"} {
		t.Run(origin, func(t *testing.T) {
			c := f.c
			c.Origin = origin
			if _, err := c.tlsConfig(f.config); err == nil {
				t.Fatal("accepted", origin)
			}
		})
	}
	for i, mutate := range []func(*remoteSettingsConfig){
		func(c *remoteSettingsConfig) {
			c.AllowedSourceCIDRs = make([]string, 257)
			for i := range c.AllowedSourceCIDRs {
				c.AllowedSourceCIDRs[i] = "192.0.2.0/24"
			}
		},
		func(c *remoteSettingsConfig) { c.Listen = ":8766" }, func(c *remoteSettingsConfig) { c.Listen = "127.0.0.1:0" }, func(c *remoteSettingsConfig) { c.Listen = "localhost:8766" },
		func(c *remoteSettingsConfig) { c.AllowedSourceCIDRs = nil }, func(c *remoteSettingsConfig) { c.AllowedSourceCIDRs = []string{"invalid"} }, func(c *remoteSettingsConfig) { c.AllowedSourceCIDRs = []string{"192.0.2.1/24"} },
		func(c *remoteSettingsConfig) { c.ProxyIdentity = "" }, func(c *remoteSettingsConfig) { c.ProxyIdentity = "*.internal" }, func(c *remoteSettingsConfig) { c.ProxyCA = "" }, func(c *remoteSettingsConfig) { c.Certificate = "" }, func(c *remoteSettingsConfig) { c.PrivateKey = "" }, func(c *remoteSettingsConfig) { c.ProxyCA = c.Certificate },
	} {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			c := f.c
			mutate(&c)
			if _, err := c.tlsConfig(f.config); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	if err := os.WriteFile(remotePasswordPath(f.config), []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := f.c.tlsConfig(f.config); err == nil {
		t.Fatal("accepted missing hash")
	}
}

func TestRemoteSettingsMutualTLS(t *testing.T) {
	f := remoteTransport(t)
	tc, err := f.c.tlsConfig(f.config)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(f.s.remoteSettingsHandler(f.c))
	srv.TLS = tc
	srv.StartTLS()
	defer srv.Close()
	for _, test := range []struct {
		name  string
		certs []tls.Certificate
		ok    bool
	}{{"proxy", []tls.Certificate{f.proxy}, true}, {"none", nil, false}, {"wrong identity", []tls.Certificate{f.other}, false}, {"foreign CA", []tls.Certificate{f.foreign}, false}, {"expired", []tls.Certificate{f.expired}, false}} {
		t.Run(test.name, func(t *testing.T) {
			tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: f.pool, ServerName: "settings.internal", Certificates: test.certs, MinVersion: tls.VersionTLS13}}
			defer tr.CloseIdleConnections()
			client := &http.Client{Transport: tr, Timeout: 3 * time.Second}
			r := f.request("GET", adminPrefix+"/providers", "")
			r.URL, _ = urlForRemoteTest(srv.URL + adminPrefix + "/providers")
			r.RequestURI = ""
			r.TLS = nil
			resp, err := client.Do(r)
			if err != nil {
				if test.ok {
					t.Fatal(err)
				}
				return
			}
			defer resp.Body.Close()
			if !test.ok || resp.StatusCode != 200 {
				t.Fatal("unexpected TLS result", resp.StatusCode)
			}
		})
	}
	// Plaintext HTTP is never dispatched by the HTTPS listener.
	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "https://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	fmt.Fprint(conn, "GET /minigw/settings HTTP/1.1\r\nHost: settings.example\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err == nil {
		defer resp.Body.Close()
		if resp.StatusCode < 400 {
			t.Fatal("plaintext accepted")
		}
	}
}

// Keep URL construction outside the handler's origin-form requests.
func urlForRemoteTest(s string) (*url.URL, error) { return url.Parse(s) }

type remoteDeadlineWriter struct {
	*httptest.ResponseRecorder
	deadline time.Time
	body     *remoteDeadlineBody
}

func (w *remoteDeadlineWriter) SetReadDeadline(d time.Time) error {
	w.deadline = d
	if !d.IsZero() {
		w.body.protected = true
	}
	return nil
}

type remoteDeadlineBody struct{ protected bool }

func (b *remoteDeadlineBody) Read(p []byte) (int, error) {
	if b.protected {
		return 0, os.ErrDeadlineExceeded
	}
	return 0, io.EOF
}
func (b *remoteDeadlineBody) Close() error { return nil }

func TestRemoteSettingsSlowBodyDeadline(t *testing.T) {
	f := remoteTransport(t)
	r := f.request("POST", "/minigw/settings/login", "")
	b := &remoteDeadlineBody{}
	r.Body = b
	w := &remoteDeadlineWriter{ResponseRecorder: httptest.NewRecorder(), body: b}
	f.s.remoteSettingsHandler(f.c).ServeHTTP(w, r)
	if w.Code != 413 || !b.protected || !w.deadline.IsZero() {
		t.Fatal(w.Code, b.protected, w.deadline)
	}
}

func TestRemoteSettingsConfigLifecycle(t *testing.T) {
	f := remoteTransport(t)
	s, _ := testServer(t, nil)
	s.cfg.RemoteSettings = f.c
	s.remoteAuth = f.s.remoteAuth
	if err := writeServeConfig(s.cfgPath, s.cfg); err != nil {
		t.Fatal(err)
	}
	if err := s.update(func(c *serveConfig) error { c.RemoteSettings.Listen = "127.0.0.1:8767"; return nil }); err == nil {
		t.Fatal("live listener change accepted")
	}
	if err := s.update(func(c *serveConfig) error { c.RemoteSettings.ProxyIdentity = "other.internal"; return nil }); err == nil {
		t.Fatal("live trust change accepted")
	}
	if err := s.update(func(c *serveConfig) error { c.RemoteSettings.AllowedSourceCIDRs[0] = "198.51.100.0/24"; return nil }); err == nil {
		t.Fatal("live CIDR change accepted")
	}
	if s.conf().RemoteSettings.AllowedSourceCIDRs[0] != "192.0.2.0/24" {
		t.Fatal("rejected CIDR update changed live policy")
	}
	// Editing trust on disk must not be silently accepted by an unrelated admin update.
	disk := s.cfg
	disk.RemoteSettings.Origin = "https://other.example"
	if err := writeServeConfig(s.cfgPath, disk); err != nil {
		t.Fatal(err)
	}
	if err := s.update(func(c *serveConfig) error { c.Token = "changed"; return nil }); err == nil {
		t.Fatal("disk trust mismatch accepted")
	}
	if err := writeServeConfig(s.cfgPath, s.cfg); err != nil {
		t.Fatal(err)
	}
	if err := s.update(func(c *serveConfig) error { c.RemoteSettings.Enabled = false; return nil }); err != nil {
		t.Fatal(err)
	}
	if s.remoteAuth.valid(f.token) {
		t.Fatal("disable kept session")
	}
	if srv, err := s.startRemoteSettings(); err != nil || srv != nil {
		t.Fatal("disabled listener started", err)
	}
	if err := s.update(func(c *serveConfig) error { c.RemoteSettings.Enabled = true; return nil }); err == nil {
		t.Fatal("live enable accepted")
	}
	f.s = s
	if w := f.run(f.request("GET", adminPrefix+"/providers", "")); w.Code != 403 {
		t.Fatal("disabled handler admitted request", w.Code)
	}
}

func TestRemoteSettingsDispatchAndAudit(t *testing.T) {
	f := remoteTransport(t)
	s, _ := testServer(t, nil)
	s.cfg.RemoteSettings = f.c
	s.remoteAuth = f.s.remoteAuth
	if err := writeServeConfig(s.cfgPath, s.cfg); err != nil {
		t.Fatal(err)
	}
	f.s = s
	// A real device mutation runs through the shared dispatch.
	id := base64url(make([]byte, 32))
	body := `{"id":"` + id + `","name":"remote device"}`
	w := f.run(f.request("POST", adminPrefix+"/devices", body))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if len(s.conf().Devices) != 1 {
		t.Fatal("not dispatched")
	}
	audit, err := os.ReadFile(filepath.Join(s.remoteAuth.dir, "remote-settings.audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(audit), `"Action":"mutation"`) || !strings.Contains(string(audit), `"IP":"192.0.2.1"`) || !strings.Contains(string(audit), `"Operation":"POST devices"`) || !strings.Contains(string(audit), `"Result":"started"`) {
		t.Fatal(string(audit))
	}
	for _, secret := range []string{body, f.token, remoteTestPassword, "remote device"} {
		if strings.Contains(string(audit), secret) {
			t.Fatal("secret in audit")
		}
	}
	// Failure to record intent prevents dispatch.
	if err := os.Rename(filepath.Join(s.remoteAuth.dir, "remote-settings.audit.jsonl"), filepath.Join(s.remoteAuth.dir, "saved-audit")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(s.remoteAuth.dir, "remote-settings.audit.jsonl"), 0700); err != nil {
		t.Fatal(err)
	}
	if w := f.run(f.request("DELETE", adminPrefix+"/devices/"+id, `{}`)); w.Code != 500 {
		t.Fatal(w.Code)
	}
	if len(s.conf().Devices) != 1 {
		t.Fatal("mutation ran without audit")
	}
}

func TestRemoteSettingsHTTPLockoutRecovery(t *testing.T) {
	f := remoteTransport(t)
	now := f.s.remoteAuth.now()
	f.s.remoteAuth.now = func() time.Time { return now }
	request := func(password string) *httptest.ResponseRecorder {
		r := f.request("POST", "/minigw/settings/login", `{"password":"`+password+`"}`)
		r.Header.Set(remoteClientHeader, "192.0.2.2")
		return f.run(r)
	}
	for i := 0; i < 5; i++ {
		w := request("wrong")
		want := 401
		if i == 4 {
			want = 429
		}
		if w.Code != want {
			t.Fatal(i, w.Code, w.Body.String())
		}
	}
	if w := request(remoteTestPassword); w.Code != 429 || w.Header().Get("Retry-After") != "900" {
		t.Fatal(w.Code, w.Header())
	}
	// Restart drops sessions but keeps source lockouts.
	var err error
	f.s.remoteAuth, err = newRemoteAuth(f.config, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if w := request(remoteTestPassword); w.Code != 429 {
		t.Fatal(w.Code)
	}
	now = now.Add(15 * time.Minute)
	if w := request(remoteTestPassword); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
}

func TestRemoteSettingsSlowBodyWire(t *testing.T) {
	f := remoteTransport(t)
	tc, err := f.c.tlsConfig(f.config)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(f.s.remoteSettingsHandler(f.c))
	srv.TLS = tc
	srv.StartTLS()
	defer srv.Close()
	conn, err := tls.Dial("tcp", strings.TrimPrefix(srv.URL, "https://"), &tls.Config{RootCAs: f.pool, ServerName: "settings.internal", Certificates: []tls.Certificate{f.proxy}, MinVersion: tls.VersionTLS13})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(remoteBodyTimeout + 5*time.Second))
	fmt.Fprintf(conn, "POST /minigw/settings/login HTTP/1.1\r\nHost: settings.example\r\n%s: 192.0.2.1\r\nOrigin: %s\r\n%s: 1\r\nContent-Type: application/json\r\nContent-Length: 100\r\nConnection: close\r\n\r\n", remoteClientHeader, f.c.Origin, remoteCSRFHeader)
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 413 {
		t.Fatal(resp.StatusCode)
	}
}

func TestRemoteSettingsStartup(t *testing.T) {
	f := remoteTransport(t)
	local, _ := testServer(t, nil)
	c := local.conf()
	c.RemoteSettings = f.c
	if _, err := newServer(c, local.cfgPath); err == nil {
		t.Fatal("startup accepted missing password")
	}
	password, err := os.ReadFile(remotePasswordPath(f.config))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(remotePasswordPath(local.cfgPath), password, 0600); err != nil {
		t.Fatal(err)
	}
	bad := c
	bad.RemoteSettings.Origin = "https://*.example"
	if _, err := newServer(bad, local.cfgPath); err == nil {
		t.Fatal("startup bypassed origin validation")
	}
	s, err := newServer(c, local.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if s.remoteAuth == nil {
		t.Fatal("startup did not create remote store")
	}
	// Reserve an ephemeral loopback port; the production validator refuses port zero.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s.cfg.RemoteSettings.Listen = ln.Addr().String()
	ln.Close()
	s.log = log.New(io.Discard, "", 0)
	srv, err := s.startRemoteSettings()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: f.pool, ServerName: "settings.internal", Certificates: []tls.Certificate{f.proxy}, MinVersion: tls.VersionTLS13}}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 3 * time.Second}
	r := f.request("GET", "/minigw/settings", "")
	r.URL, _ = url.Parse("https://" + s.cfg.RemoteSettings.Listen + "/minigw/settings")
	r.RequestURI = ""
	r.TLS = nil
	resp, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
	if srv.ReadTimeout != remoteBodyTimeout || srv.ReadHeaderTimeout == 0 || srv.WriteTimeout == 0 || srv.IdleTimeout == 0 || srv.MaxHeaderBytes > 16<<10 {
		t.Fatal("listener deadlines or header bound missing")
	}
}

func TestRemoteSettingsIgnoresCookie(t *testing.T) {
	f := remoteTransport(t)
	r := f.request("GET", adminPrefix+"/providers", "")
	r.Header.Set("Cookie", "sso=parent-domain")
	if w := f.run(r); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	r = f.request("GET", adminPrefix+"/providers", "")
	r.Header.Del(remoteSessionHeader)
	r.Header.Set("Cookie", "session="+f.token)
	if w := f.run(r); w.Code != 401 {
		t.Fatal("cookie accepted as session", w.Code)
	}
}

func TestRemoteProviderPatchCannotRedirectKey(t *testing.T) {
	f := remoteTransport(t)
	s, _ := testServer(t, func(c *serveConfig) {
		c.Gateways = []serveGateway{{Name: "lite", BaseURL: "https://good.example/v1"}}
	})
	s.cfg.RemoteSettings = f.c
	s.remoteAuth = f.s.remoteAuth
	if err := writeServeConfig(s.cfgPath, s.cfg); err != nil {
		t.Fatal(err)
	}
	f.s = s
	orig, _ := findGateway(s.conf(), "lite")
	for _, body := range []string{
		`{"baseURL":"https://evil.example/v1"}`, `{"BaseURL":"https://evil.example/v1"}`, `{"BASEURL":"https://evil.example/v1"}`,
		`{"baseURL":"https://evil.example/v1","baseURL":null}`, `{"enabled":true,"enabled":false}`,
		`{"insecure":true}`, `{"Insecure":true}`, `{"tools":"edit"}`, `{"model":"x","tools":"edit"}`, `{"key":"k","baseURL":"https://evil.example/v1"}`,
		`{"aliases":"a","nested":{"baseURL":"https://evil.example/v1"}}`,
		`[]`, `"baseURL"`, `null`, `{"baseURL"`, ``,
	} {
		w := f.run(f.request("PATCH", adminPrefix+"/providers/lite", body))
		if w.Code < 400 || w.Code >= 500 || !strings.Contains(w.Body.String(), "remote") && !strings.Contains(w.Body.String(), "JSON object") && !strings.Contains(w.Body.String(), "Duplicate") {
			t.Fatalf("%s: %d %s", body, w.Code, w.Body.String())
		}
	}
	if g, _ := findGateway(s.conf(), "lite"); g.BaseURL != orig.BaseURL || g.Tools != "" || g.Insecure || g.Disabled {
		t.Fatal("provider changed", g)
	}
	// Name, enabled and aliases still work remotely.
	if w := f.run(f.request("PATCH", adminPrefix+"/providers/lite", `{"enabled":false,"aliases":"fast=x"}`)); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	// Re-adding an existing provider needs the key.
	if w := f.run(f.request("POST", adminPrefix+"/providers", `{"name":"lite","baseURL":"https://evil.example/v1"}`)); w.Code != 400 {
		t.Fatal(w.Code, w.Body.String())
	}
	if g, _ := findGateway(s.conf(), "lite"); g.BaseURL != orig.BaseURL {
		t.Fatal("provider URL changed without the key")
	}
}

func TestRemoteLimitListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	l := limitListener(ln, 2)
	var dialed []net.Conn
	for i := 0; i < 3; i++ {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		dialed = append(dialed, c)
	}
	var open []net.Conn
	for i := 0; i < 2; i++ {
		c, err := l.Accept()
		if err != nil {
			t.Fatal(err)
		}
		open = append(open, c)
	}
	got := make(chan net.Conn, 1)
	go func() { c, _ := l.Accept(); got <- c }()
	select {
	case <-got:
		t.Fatal("third connection accepted above the limit")
	case <-time.After(200 * time.Millisecond):
	}
	open[0].Close()
	open[0].Close() // a double close frees one slot only
	select {
	case c := <-got:
		c.Close()
	case <-time.After(3 * time.Second):
		t.Fatal("slot not released")
	}
}

func TestRemoteBrokenLockoutsDisableOnlyRemote(t *testing.T) {
	f := remoteTransport(t)
	local, _ := testServer(t, nil)
	c := local.conf()
	c.RemoteSettings = f.c
	password, _ := os.ReadFile(remotePasswordPath(f.config))
	if err := os.WriteFile(remotePasswordPath(local.cfgPath), password, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(local.cfgPath), "remote-settings.lockouts.json"), []byte(`{"sources":{"not-ip":{}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := newServer(c, local.cfgPath)
	if err != nil || s.remoteAuth != nil {
		t.Fatal("gateway must start with remote login off", err)
	}
	s.log = log.New(io.Discard, "", 0)
	if srv, err := s.startRemoteSettings(); srv != nil || err != nil {
		t.Fatal("remote listener started without lockout state")
	}
}

// The four read-only lists end to end with a real core store: 200 with the local shapes, and nothing secret in any answer.
func TestRemoteReadOnlyListsEndToEnd(t *testing.T) {
	f := remoteTransport(t)
	cf := newCoreFx(t, func(c *serveConfig) {
		c.Token = "gw-token-Zq7xK2"
		c.Gateways = []serveGateway{{Name: "prov", BaseURL: "https://upstream.example"}}
	})
	cf.s.cfg.RemoteSettings, cf.s.remoteAuth = f.c, f.s.remoteAuth
	f.s = cf.s
	password, err := os.ReadFile(remotePasswordPath(f.config))
	if err != nil {
		t.Fatal(err)
	}
	team, err := cf.s.core.CreateTeam(t0, "team-alpha")
	if err != nil {
		t.Fatal(err)
	}
	prj, err := cf.s.core.CreateProject(t0, "remoteprj", "Remote project")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []error{cf.s.core.AddTeamMember(t0, team.ID, cf.uid["bor"]), cf.s.core.AddTeamProject(t0, team.ID, prj.ID), cf.s.core.SetTeamAssignment(t0, team.ID, prj.ID, cf.uid["bor"], "reader"), cf.s.core.SetMember(t0, prj.ID, cf.uid["ana"], "developer")} {
		if e != nil {
			t.Fatal(e)
		}
	}
	secrets := []string{"gw-token-Zq7xK2", cf.s.cfg.Key, remoteTestPassword, string(password), f.token}
	for name, key := range map[string]string{"users": "users", "bindings": "bindings", "teams": "teams", "projects": "projects"} {
		w := f.run(f.request("GET", adminPrefix+"/"+name, ""))
		if w.Code != 200 {
			t.Fatal(name, w.Code, w.Body.String())
		}
		var j map[string]json.RawMessage
		if json.Unmarshal(w.Body.Bytes(), &j) != nil || j[key] == nil {
			t.Fatal(name, w.Body.String())
		}
		for _, v := range secrets {
			if v != "" && strings.Contains(w.Body.String(), v) {
				t.Fatal(name, "leaks a secret")
			}
		}
		for _, d := range cf.dev {
			if strings.Contains(w.Body.String(), base64url(d.key.Seed())) {
				t.Fatal(name, "leaks a device key")
			}
		}
	}
	for _, tc := range []struct {
		change func(*http.Request)
		status int
	}{{func(r *http.Request) { r.Header.Del(remoteSessionHeader) }, 401}, {func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }, 403}} {
		r := f.request("GET", adminPrefix+"/users", "")
		tc.change(r)
		if w := f.run(r); w.Code != tc.status {
			t.Fatal(w.Code)
		}
	}
	// who has access to a project and why, who is in a team and what it assigns: read-only
	members := adminPrefix + "/projects/" + fmt.Sprint(prj.ID) + "/members"
	detail := adminPrefix + "/teams/" + fmt.Sprint(team.ID)
	for path, want := range map[string][]string{
		members:                 {`"name":"ana"`, `"directRole":"developer"`, `"name":"bor"`, `"fromTeams":"team-alpha (reader)"`, `"kind":"direct"`, `"kind":"team"`},
		detail:                  {`"name":"team-alpha"`, `"members":[{"name":"bor","user":` + fmt.Sprint(cf.uid["bor"]) + `}]`, `"slug":"remoteprj"`},
		detail + "/assignments": {`"projectName":"Remote project"`, `"userName":"bor"`, `"role":"reader"`},
	} {
		w := f.run(f.request("GET", path, ""))
		if w.Code != 200 {
			t.Fatal(path, w.Code, w.Body.String())
		}
		for _, v := range append(append([]string{}, secrets...), want...) {
			if has := strings.Contains(w.Body.String(), v); has == (v == secrets[0] || v == secrets[1] || v == secrets[2] || v == secrets[3] || v == secrets[4]) {
				t.Fatal(path, "unexpected presence of", v, w.Body.String())
			}
		}
		r := f.request("GET", path, "")
		r.Header.Del(remoteSessionHeader)
		if w := f.run(r); w.Code != 401 {
			t.Fatal(path, "without a session", w.Code)
		}
		for _, method := range []string{"POST", "PUT", "DELETE"} {
			if w := f.run(f.request(method, path, `{}`)); w.Code != 404 && w.Code != 403 {
				t.Fatal(method, path, w.Code)
			}
		}
	}
	if w := f.run(f.request("GET", adminPrefix+"/teams/99999", "")); w.Code != 404 {
		t.Fatal("a deleted team id:", w.Code)
	}
	if w := f.run(f.request("GET", adminPrefix+"/bindings", "")); !strings.Contains(w.Body.String(), `"device":"`+devID(cf.dev["ana"])+`","user":`+fmt.Sprint(cf.uid["ana"])) {
		t.Fatal("the binding is not listed", w.Body.String())
	}
	before := f.run(f.request("GET", adminPrefix+"/users", "")).Body.String()
	f.run(f.request("POST", adminPrefix+"/users", `{"name":"mallory"}`))
	if after := f.run(f.request("GET", adminPrefix+"/users", "")).Body.String(); after != before {
		t.Fatal("a refused POST changed the users")
	}
}

// remoteCaddyMatchers: the matchers of the Caddyfile (name -> methods and path regexp), the proxy's own allow-list.
func remoteCaddyMatchers(t *testing.T) map[string]struct {
	methods []string
	re      *regexp.Regexp
} {
	t.Helper()
	if _, err := os.Stat("../deploy/docker/Caddyfile"); os.IsNotExist(err) {
		t.Skip("needs the repository tree (../deploy/docker/Caddyfile)")
	}
	out := map[string]struct {
		methods []string
		re      *regexp.Regexp
	}{}
	cur := ""
	var m struct {
		methods []string
		re      *regexp.Regexp
	}
	dirtyDefined, dirtyHandle, dirtyResponds, firstGateway := false, -1, false, -1
	n := 0
	inDirty := false
	for i, line := range strings.Split(string(srcFile(t, "../deploy/docker/Caddyfile")), "\n") {
		f := strings.Fields(line)
		// The parity model drops every percent-encoded path because @dirty does: require that refusal, and that it comes first.
		if len(f) > 1 && f[0] == "@dirty" && f[1] == "expression" {
			for _, want := range []string{`(^|/)[.][.]?(/|$)`, `|//|`, `|%|`, `\\\\`, `.contains("%")`} {
				if !strings.Contains(line, want) {
					t.Fatal("the @dirty matcher no longer refuses", want)
				}
			}
			dirtyDefined = true
		}
		if len(f) == 3 && f[0] == "handle" && f[1] == "@dirty" && f[2] == "{" {
			dirtyHandle, inDirty = i, true
		} else if inDirty && len(f) == 1 && f[0] == "}" {
			inDirty = false
		} else if inDirty && strings.Join(f, " ") == `respond "Not found" 404` {
			dirtyResponds = true
		}
		if len(f) == 2 && f[0] == "import" && f[1] == "gateway" && firstGateway < 0 {
			firstGateway = i
		}
		// Only @get, @post, @change may reach the gateway; @dirty and the final handle answer 404 themselves.
		if len(f) > 0 && strings.HasPrefix(f[0], "@") && f[0] != "@get" && f[0] != "@post" && f[0] != "@change" && f[0] != "@dirty" {
			t.Fatal("Caddyfile matcher the parity test does not know:", f[0])
		}
		if len(f) > 0 && f[0] == "handle" && !(len(f) == 2 && f[1] == "{" || len(f) == 3 && f[2] == "{" && (f[1] == "@get" || f[1] == "@post" || f[1] == "@change" || f[1] == "@dirty")) {
			t.Fatal("Caddyfile handle the parity test does not know:", line)
		}
		if len(f) == 2 && f[0] == "import" && f[1] == "gateway" {
			n++
		}
		switch {
		case len(f) == 2 && f[1] == "{" && (f[0] == "@get" || f[0] == "@post" || f[0] == "@change"):
			cur = f[0]
			m.methods, m.re = nil, nil
		case cur != "" && len(f) > 1 && f[0] == "method":
			m.methods = f[1:]
		case cur != "" && len(f) == 2 && f[0] == "path_regexp":
			m.re = regexp.MustCompile(f[1])
		case cur != "" && len(f) == 1 && f[0] == "}":
			if m.re == nil || m.methods == nil {
				t.Fatal("matcher", cur, "not parsed")
			}
			out[cur] = m
			cur = ""
		}
	}
	if len(out) != 3 || n != 3 {
		t.Fatal("expected @get, @post, @change and exactly three routes to the gateway, got", len(out), n)
	}
	if !dirtyDefined || dirtyHandle < 0 || !dirtyResponds || dirtyHandle > firstGateway {
		t.Fatal("the Caddyfile needs a handle @dirty block that answers 404 before any route to the gateway")
	}
	return out
}

// A route added to only one of the two allow-lists (gateway or proxy) fails here.
func TestRemoteAllowListMatchesCaddyfile(t *testing.T) {
	matchers := remoteCaddyMatchers(t)
	names := []string{"users", "bindings", "teams", "projects", "providers", "devices", "general", "status", "agents", "settings", "quota", "usage", "kb", "grants", "audit", "core", "export", "import", "upstreams", "upstream-push", "upstream-pull", "upstream-auto", "upstream-resolve", "upstream-links", "panels", "sync", "gateways", "sessions", "unknown"}
	names = append(names, "activity", "foundry")
	// Every first segment the gateway's own router or the core's IsAdminPath knows is a candidate too.
	for _, src := range []string{"serve_admin.go", "coreapi/admin.go"} {
		text := string(srcFile(t, src))
		if src == "coreapi/admin.go" {
			i := strings.Index(text, "func IsAdminPath")
			text = text[i : i+strings.Index(text[i:], "\nfunc adminSeg")]
			for _, m := range regexp.MustCompile(`case ((?:"[^"]+"(?:, )?)+):`).FindAllStringSubmatch(text, -1) {
				for _, n := range regexp.MustCompile(`"([^"]+)"`).FindAllStringSubmatch(m[1], -1) {
					names = append(names, n[1])
				}
			}
			continue
		}
		for _, m := range regexp.MustCompile(`(?m)^\tcase (".*"):`).FindAllStringSubmatch(text, -1) {
			for _, n := range regexp.MustCompile(`"(?:GET|POST|PUT|PATCH|DELETE) ([a-z][a-z0-9-]*)`).FindAllStringSubmatch(m[1], -1) {
				names = append(names, n[1])
			}
		}
	}
	var paths []string
	for _, n := range names {
		for _, form := range []string{"", "/1", "/1/models", "/1/history", "/1/members", "/1/assignments", "/1/depends", "/strings", "/login", "/logout", "/history", "/events", "/0", "/0/members", "/0/assignments", "/123456789012345678", "/123456789012345678/members", "/1234567890123456789", "/1234567890123456789/members", "/1a", "/1a/members", "/-1", "/-1/assignments", "/+1", "/+1/members", "/%31", "/%31/members", "/1/", "/1/members/2", "/1/assignments/2", "/1/grants", "/1/projects"} {
			paths = append(paths, adminPrefix+"/"+n+form)
		}
	}
	paths = append(paths, "/minigw/settings", "/minigw/settings/login", "/minigw/settings/logout", "/minigw/v1/projects", "/minigw/v1/member", "/v1/messages", "/minigw/sync/file")
	for _, method := range []string{"GET", "POST", "PATCH", "DELETE", "PUT", "HEAD", "OPTIONS"} {
		for _, path := range paths {
			caddy := false
			for _, m := range matchers {
				if strings.Contains(path, "%") { // @dirty answers 404 to any percent-encoding before the matchers
					break
				}
				for _, mm := range m.methods {
					caddy = caddy || mm == method && m.re.MatchString(path)
				}
			}
			gateway := remoteAdminAllowed(method, path) || method == "GET" && path == "/minigw/settings" || method == "POST" && (path == "/minigw/settings/login" || path == "/minigw/settings/logout")
			if caddy != gateway {
				t.Errorf("%s %s: proxy allows %v, gateway allows %v", method, path, caddy, gateway)
			}
		}
	}
}
