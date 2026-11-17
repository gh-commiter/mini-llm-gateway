package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const remoteTestPassword = "a private password for settings"

func remoteFixture(t *testing.T) (*remoteAuth, string, *time.Time) {
	t.Helper()
	config := filepath.Join(t.TempDir(), "serve.json")
	if err := remotePasswordCommand(config, nil, strings.NewReader(remoteTestPassword+"\n")); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	a, err := newRemoteAuth(config, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	return a, config, &now
}
func remoteLogin(t *testing.T, a *remoteAuth) string {
	t.Helper()
	token, retry, err := a.login("192.0.2.1", "", remoteTestPassword)
	if err != nil || retry != 0 || len(token) != 64 {
		t.Fatalf("login: %v %v", retry, err)
	}
	return token
}

func TestRemotePasswordRecord(t *testing.T) {
	_, config, _ := remoteFixture(t)
	p, _, err := loadRemotePassword(remotePasswordPath(config))
	if err != nil {
		t.Fatal(err)
	}
	if p.Memory != 65536 || p.Iterations != 3 || p.Parallelism != 1 {
		t.Fatal("unexpected defaults")
	}
	if runtime.GOOS != "windows" {
		st, _ := os.Stat(remotePasswordPath(config))
		if st.Mode().Perm() != 0600 {
			t.Fatal(st.Mode())
		}
	}
	cases := []func(*remotePassword){func(p *remotePassword) { p.Memory = ^uint32(0) }, func(p *remotePassword) { p.Memory = 1 }, func(p *remotePassword) { p.Iterations = 0 }, func(p *remotePassword) { p.Iterations = 7 }, func(p *remotePassword) { p.Parallelism = 255 }, func(p *remotePassword) { p.Version = 1 }, func(p *remotePassword) { p.Algorithm = "argon2i" }, func(p *remotePassword) { p.Salt = nil }, func(p *remotePassword) { p.Hash = nil }}
	for i, mutate := range cases {
		bad := p
		mutate(&bad)
		b, _ := json.Marshal(bad)
		path := filepath.Join(t.TempDir(), "password")
		if err := writeFileAtomic(path, b); err != nil {
			t.Fatal(err)
		}
		if _, _, err := loadRemotePassword(path); err == nil {
			t.Fatalf("accepted case %d", i)
		}
	}
	if runtime.GOOS != "windows" {
		if err := os.Chmod(remotePasswordPath(config), 0644); err != nil {
			t.Fatal(err)
		}
		if _, _, err := loadRemotePassword(remotePasswordPath(config)); err == nil {
			t.Fatal("accepted public record")
		}
	}
}

type remoteUnreadable struct{}

func (remoteUnreadable) Read([]byte) (int, error) { panic("must not read input") }
func TestRemotePasswordCommandInput(t *testing.T) {
	config := filepath.Join(t.TempDir(), "serve.json")
	if err := remotePasswordCommand(config, []string{"secret"}, remoteUnreadable{}); err == nil {
		t.Fatal("accepted argument")
	}
	t.Setenv("MINIGW_REMOTE_PASSWORD", remoteTestPassword)
	t.Setenv("MGW_PASSWORD", remoteTestPassword)
	for _, input := range []string{"", "short", strings.Repeat("x", 1025), strings.Repeat("x", 1024) + "\ntruncated", strings.Repeat("x", 1024) + "\r\ntruncated"} {
		if err := remotePasswordCommand(config, nil, strings.NewReader(input)); err == nil {
			t.Fatal("accepted invalid stdin")
		}
	}
	if _, err := os.Stat(remotePasswordPath(config)); !os.IsNotExist(err) {
		t.Fatal("wrote credential without valid stdin")
	}
}

func TestRemoteWrongPasswordLockoutRecovery(t *testing.T) {
	a, config, now := remoteFixture(t)
	for i := 0; i < 5; i++ {
		token, retry, err := a.login("192.0.2.1", "", "wrong")
		if token != "" || err != errRemoteLogin || (i == 4 && retry != 15*time.Minute) {
			t.Fatalf("attempt %d: %q %v %v", i, token, retry, err)
		}
	}
	a, err := newRemoteAuth(config, a.now)
	if err != nil {
		t.Fatal(err)
	}
	if token, retry, err := a.login("192.0.2.1", "", remoteTestPassword); token != "" || retry != 15*time.Minute || err != errRemoteLogin {
		t.Fatal("lockout lost on restart")
	}
	*now = now.Add(15 * time.Minute)
	if !a.valid(remoteLogin(t, a)) {
		t.Fatal("recovery failed")
	}
}

func TestRemoteLimiterBounds(t *testing.T) {
	a, _, now := remoteFixture(t)
	for i := 0; i < remoteSourceCap; i++ {
		a.locks.Sources[fmt.Sprintf("198.51.%d.%d", i/256, i%256)] = remoteSource{Window: *now}
	}
	if _, retry, err := a.login("203.0.113.1", "", remoteTestPassword); retry == 0 || err == nil {
		t.Fatal("source cap bypassed")
	}
	if len(a.locks.Sources) != remoteSourceCap {
		t.Fatal("source map grew")
	}
	*now = now.Add(15 * time.Minute)
	a.purge(*now)
	if len(a.locks.Sources) != 0 {
		t.Fatal("stale sources retained")
	}
	a.hashing = 2
	if _, retry, err := a.login("203.0.113.1", "", remoteTestPassword); retry == 0 || err == nil {
		t.Fatal("hash concurrency bypassed")
	}
	a.hashing = 0
	a.locks.Sources["203.0.113.1"] = remoteSource{Window: *now, Attempts: 5}
	if _, retry, err := a.login("203.0.113.1", "", remoteTestPassword); retry != time.Minute || err == nil {
		t.Fatal("per-source rate bypassed")
	}
	a.locks.GlobalUntil = now.Add(time.Minute)
	if _, retry, err := a.login("203.0.113.2", "", remoteTestPassword); retry != time.Minute || err == nil {
		t.Fatal("global backoff bypassed")
	}
}

func TestRemoteSessionExpiry(t *testing.T) {
	a, _, now := remoteFixture(t)
	token := remoteLogin(t, a)
	if _, ok := a.sessions[sha256.Sum256([]byte(token))]; !ok {
		t.Fatal("session not indexed by digest")
	}
	*now = now.Add(30 * time.Minute)
	if a.valid(token) {
		t.Fatal("idle expiry bypassed")
	}
	token = remoteLogin(t, a)
	for i := 0; i < 31; i++ {
		*now = now.Add(15 * time.Minute)
		if !a.valid(token) {
			t.Fatal("expired early")
		}
	}
	*now = now.Add(15 * time.Minute)
	if a.valid(token) {
		t.Fatal("absolute expiry bypassed")
	}
}

func TestRemoteSessionInvalidation(t *testing.T) {
	for _, event := range []string{"logout", "password", "token", "disable", "restart"} {
		t.Run(event, func(t *testing.T) {
			a, config, _ := remoteFixture(t)
			token := remoteLogin(t, a)
			switch event {
			case "logout":
				if err := a.logout(token, "192.0.2.1", ""); err != nil {
					t.Fatal(err)
				}
			case "password":
				if err := remotePasswordCommand(config, nil, strings.NewReader("a different private password")); err != nil {
					t.Fatal(err)
				}
			case "token":
				a.revoke()
			case "disable":
				a.disable()
			case "restart":
				var err error
				a, err = newRemoteAuth(config, a.now)
				if err != nil {
					t.Fatal(err)
				}
			}
			if a.valid(token) {
				t.Fatal("session survived " + event)
			}
		})
	}
}

func TestRemoteSessionCapAndSeparation(t *testing.T) {
	a, _, now := remoteFixture(t)
	for i := 0; i < remoteSessionCap; i++ {
		a.sessions[sha256.Sum256([]byte(fmt.Sprint(i)))] = remoteSession{*now, *now}
	}
	if _, retry, err := a.login("192.0.2.1", "", remoteTestPassword); retry == 0 || err == nil {
		t.Fatal("session cap bypassed")
	}
	var local settingsState
	id, ok := local.login(local.newLogin())
	if !ok {
		t.Fatal("local login")
	}
	if a.valid(id) {
		t.Fatal("accepted local session")
	}
	a.sessions = map[[32]byte]remoteSession{}
	if local.valid(remoteLogin(t, a)) {
		t.Fatal("local accepted remote session")
	}
}

func TestRemoteAuditSecretsAndRotation(t *testing.T) {
	a, _, _ := remoteFixture(t)
	token := remoteLogin(t, a)
	a.login("192.0.2.2", "", "wrong-secret")
	if err := a.logout(token, "192.0.2.1", "req_0123456789abcdef0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(a.dir, "remote-settings.audit.jsonl")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{remoteTestPassword, token, "wrong-secret", "argon2", "salt", "hash"} {
		if strings.Contains(string(b), secret) {
			t.Fatal("secret in audit")
		}
	}
	if err := a.audit("secret", "ok", "", ""); err == nil {
		t.Fatal("arbitrary action accepted")
	}
	if err := a.audit("login", "ok", "", remoteTestPassword); err == nil {
		t.Fatal("arbitrary request id accepted")
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.WriteString(f, strings.Repeat("x", 1<<20))
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err = a.audit("mutation", "ok", "192.0.2.1", ""); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(path + ".1"); err != nil {
		t.Fatal("not rotated", err)
	}
	if runtime.GOOS != "windows" {
		for _, p := range []string{path, path + ".1"} {
			st, _ := os.Stat(p)
			if st.Mode().Perm() != 0600 {
				t.Fatal("public audit")
			}
		}
	}
}

func TestRemoteMalformedLockoutsFailClosed(t *testing.T) {
	a, config, now := remoteFixture(t)
	for _, data := range []string{`{"sources":{"not-ip":{}}}`, `{"globalFailures":21}`, `{"sources":{},"globalUntil":"2099-01-01T00:00:00Z"}`, `{"sources":{},"unknown":true}`, `{"globalFailures":-1}`, `{"sources":{"192.0.2.1":{"attempts":6}}}`, `{"sources":{"192.0.2.1":{"failures":-1}}}`, strings.Repeat("x", 256<<10)} {
		if err := writeFileAtomic(a.lockPath(), []byte(data)); err != nil {
			t.Fatal(err)
		}
		if _, err := newRemoteAuth(config, func() time.Time { return *now }); err == nil {
			t.Fatal("accepted invalid locks", data)
		}
	}
}

func TestRemoteLoginResponse(t *testing.T) {
	a, _, now := remoteFixture(t)
	w := httptest.NewRecorder()
	if status := a.loginResponse(w, "192.0.2.1", "", "wrong"); status != 401 || !strings.Contains(w.Body.String(), errRemoteLogin.Error()) {
		t.Fatal("wrong password response", status)
	}
	a.locks.GlobalUntil = now.Add(1500 * time.Millisecond)
	w = httptest.NewRecorder()
	if status := a.loginResponse(w, "192.0.2.2", "", remoteTestPassword); status != 429 || w.Header().Get("Retry-After") != "2" || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("throttle response", status, w.Header())
	}
	*now = now.Add(2 * time.Second)
	w = httptest.NewRecorder()
	if status := a.loginResponse(w, "192.0.2.2", "", remoteTestPassword); status != 200 {
		t.Fatal("login response", status)
	}
}

func TestRemoteDistributedBackoffPersisted(t *testing.T) {
	a, config, now := remoteFixture(t)
	for i := 0; i < 20; i++ {
		if _, _, err := a.login(fmt.Sprintf("192.0.2.%d", i+1), "", "wrong"); err != errRemoteLogin {
			t.Fatal(err)
		}
	}
	a, err := newRemoteAuth(config, a.now)
	if err != nil {
		t.Fatal(err)
	}
	if _, retry, err := a.login("203.0.113.1", "", remoteTestPassword); err != errRemoteLogin || retry != time.Minute {
		t.Fatal("account backoff bypassed")
	}
	*now = now.Add(time.Minute)
	if !a.valid(remoteLogin(t, a)) {
		t.Fatal("account recovery failed")
	}
}

func TestRemoteGatewayTokenRotationRevokes(t *testing.T) {
	a, _, _ := remoteFixture(t)
	token := remoteLogin(t, a)
	s, _, base := liveServer(t, nil)
	s.remoteAuth = a
	status, _ := adminNoise(t, s, base+adminPrefix, s.admin, "POST", "/general/token", "")
	if status != 200 {
		t.Fatal(status)
	}
	if a.valid(token) {
		t.Fatal("token rotation retained remote session")
	}
}

func TestRemoteAuditFailureDeniesLogin(t *testing.T) {
	a, _, _ := remoteFixture(t)
	if err := os.Rename(filepath.Join(a.dir, "remote-settings.audit.jsonl"), filepath.Join(a.dir, "audit.saved")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(a.dir, "remote-settings.audit.jsonl"), 0700); err != nil {
		t.Fatal(err)
	}
	if token, _, err := a.login("192.0.2.1", "", remoteTestPassword); token != "" || err == nil {
		t.Fatal("login without audit")
	}
	if len(a.sessions) != 0 {
		t.Fatal("issued unaudited session")
	}
}

func TestRemoteInflightLoginRevocation(t *testing.T) {
	for _, event := range []string{"password", "token", "disable"} {
		t.Run(event, func(t *testing.T) {
			a, config, _ := remoteFixture(t)
			entered, release, done := make(chan struct{}), make(chan struct{}), make(chan string, 1)
			var once sync.Once
			defer once.Do(func() { close(release) })
			a.derive = func(_ []byte, p remotePassword) []byte { close(entered); <-release; return p.Hash }
			go func() { token, _, _ := a.login("192.0.2.1", "", remoteTestPassword); done <- token }()
			<-entered
			switch event {
			case "password":
				if err := remotePasswordCommand(config, nil, strings.NewReader("a replacement private password")); err != nil {
					t.Fatal(err)
				}
			case "token":
				a.revoke()
			case "disable":
				a.disable()
			}
			once.Do(func() { close(release) })
			if token := <-done; token != "" {
				t.Fatal("inflight login survived " + event)
			}
		})
	}
}

func TestRemoteConcurrentHashLimit(t *testing.T) {
	a, _, _ := remoteFixture(t)
	entered, release, done := make(chan struct{}, 2), make(chan struct{}), make(chan struct{}, 2)
	a.derive = func(_ []byte, p remotePassword) []byte { entered <- struct{}{}; <-release; return p.Hash }
	for i := 1; i <= 2; i++ {
		go func(i int) { a.login(fmt.Sprintf("192.0.2.%d", i), "", remoteTestPassword); done <- struct{}{} }(i)
	}
	<-entered
	<-entered
	_, retry, err := a.login("192.0.2.3", "", remoteTestPassword)
	close(release)
	<-done
	<-done
	if retry == 0 || err != errRemoteLogin {
		t.Fatal("more than two hashes admitted")
	}
}

func TestRemoteMappedAddressAndZones(t *testing.T) {
	a, _, now := remoteFixture(t)
	a.locks.Sources["192.0.2.1"] = remoteSource{Window: *now, Locked: now.Add(time.Minute)}
	if _, retry, err := a.login("::ffff:192.0.2.1", "", remoteTestPassword); retry == 0 || err != errRemoteLogin {
		t.Fatal("mapped address bypassed lockout")
	}
	if _, _, err := a.login("fe80::1%zone", "", remoteTestPassword); err != errRemoteLogin {
		t.Fatal("zoned source accepted")
	}
}

func TestRemoteMissingCredentialRevokes(t *testing.T) {
	a, config, _ := remoteFixture(t)
	token := remoteLogin(t, a)
	if err := os.Rename(remotePasswordPath(config), remotePasswordPath(config)+".saved"); err != nil {
		t.Fatal(err)
	}
	if a.valid(token) || len(a.sessions) != 0 {
		t.Fatal("session survived missing credential")
	}
}

func TestRemoteLockoutFilePrivate(t *testing.T) {
	a, _, _ := remoteFixture(t)
	a.login("192.0.2.1", "", "wrong")
	st, err := os.Stat(a.lockPath())
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && st.Mode().Perm() != 0600 {
		t.Fatal("public lockouts")
	}
}

func TestRemotePasswordCLIWhileStopped(t *testing.T) {
	config := filepath.Join(t.TempDir(), "serve.json")
	cmd := exec.Command(os.Args[0], "gw", "settings", "password", "--config", config)
	cmd.Env = append(os.Environ(), "MINIGW_TEST_MAIN=1", "MINIGW_AGENT_URL=", "MINIGW_AGENT_AUTH_FILE=", "HOME="+t.TempDir(), "XDG_CONFIG_HOME="+t.TempDir(), "MINIGW_REMOTE_PASSWORD=wrong environment password")
	cmd.Stdin = strings.NewReader(remoteTestPassword + "\n")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("stopped CLI: %v %s", err, out)
	}
	a, err := newRemoteAuth(config, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !a.valid(remoteLogin(t, a)) {
		t.Fatal("stdin password not stored")
	}
	if _, _, err := a.login("192.0.2.2", "", "wrong environment password"); err != errRemoteLogin {
		t.Fatal("accepted environment password")
	}
}

func TestRemoteMissingCredentialDoesNotFloodAudit(t *testing.T) {
	config := filepath.Join(t.TempDir(), "serve.json")
	a, err := newRemoteAuth(config, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		if a.valid(strings.Repeat("0", 64)) {
			t.Fatal("accepted missing credential")
		}
	}
	if _, err := os.Stat(filepath.Join(a.dir, "remote-settings.audit.jsonl")); !os.IsNotExist(err) {
		t.Fatal("missing credential flooded audit", err)
	}
}
