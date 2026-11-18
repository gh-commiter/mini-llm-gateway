package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/argon2"
)

const remoteSessionCap = 50
const remoteSourceCap = 1024
const remoteTrustedCap = 16
const remoteTrustFor = 8 * time.Hour
const remoteSessionHeader = "X-Minigw-Remote-Session"

var errRemoteLogin = errors.New("Sign in failed")

type remotePassword struct {
	Algorithm   string `json:"algorithm"`
	Version     int    `json:"version"`
	Memory      uint32 `json:"memory"`
	Iterations  uint32 `json:"iterations"`
	Parallelism uint8  `json:"parallelism"`
	Salt        []byte `json:"salt"`
	Hash        []byte `json:"hash"`
}

func (p remotePassword) validate() error {
	if p.Algorithm != "argon2id" || p.Version != argon2.Version || p.Memory < 64*1024 || p.Memory > 128*1024 || p.Iterations < 3 || p.Iterations > 6 || p.Parallelism != 1 || len(p.Salt) != 16 || len(p.Hash) != 32 {
		return errors.New("invalid remote password record")
	}
	return nil
}

func remotePasswordPath(config string) string {
	return filepath.Join(filepath.Dir(config), "remote-settings.password.json")
}

func loadRemotePassword(path string) (remotePassword, [32]byte, error) {
	var p remotePassword
	f, err := os.Open(path)
	if err != nil {
		return p, [32]byte{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() || runtime.GOOS != "windows" && st.Mode().Perm()&0077 != 0 {
		return p, [32]byte{}, errors.New("remote password record must be private")
	}
	b, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil || len(b) > 4096 {
		return p, [32]byte{}, errors.New("invalid remote password record")
	}
	if err = decodeStrict(b, &p); err != nil {
		return p, [32]byte{}, err
	}
	return p, sha256.Sum256(b), p.validate()
}

func remotePasswordCommand(config string, args []string, in io.Reader) error {
	if len(args) != 0 {
		return errors.New("usage: minigw gw settings password (password on stdin or hidden terminal input)")
	}
	var b []byte
	var err error
	if f, ok := in.(*os.File); ok && isTerminal(f) {
		fmt.Fprint(os.Stderr, "Remote settings password: ")
		var line string
		line, err = readHidden(f)
		fmt.Fprintln(os.Stderr)
		b = []byte(line)
	} else {
		b, err = io.ReadAll(io.LimitReader(in, 1027))
	}
	if err != nil {
		return err
	}
	if len(b) > 1026 {
		return errors.New("password must contain 12 to 1024 bytes")
	}
	b = []byte(strings.TrimSuffix(strings.TrimSuffix(string(b), "\n"), "\r"))
	if len(b) < 12 || len(b) > 1024 {
		return errors.New("password must contain 12 to 1024 bytes")
	}
	if err = os.MkdirAll(filepath.Dir(config), 0700); err != nil {
		return err
	}
	if err = protectDir(filepath.Dir(config)); err != nil {
		return err
	}
	p := remotePassword{Algorithm: "argon2id", Version: argon2.Version, Memory: 64 * 1024, Iterations: 3, Parallelism: 1, Salt: make([]byte, 16)}
	if _, err = rand.Read(p.Salt); err != nil {
		return err
	}
	p.Hash = argon2.IDKey(b, p.Salt, p.Iterations, p.Memory, p.Parallelism, 32)
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if err = writeFileAtomic(remotePasswordPath(config), data); err != nil {
		return err
	}
	a := remoteAuth{dir: filepath.Dir(config), now: time.Now}
	if err = a.audit("revoke", "ok", "", ""); err != nil {
		return fmt.Errorf("password saved, but audit failed: %w", err)
	}
	return nil
}

type remoteSession struct{ Created, Used time.Time }
type remoteSource struct {
	Window   time.Time `json:"window"`
	Attempts int       `json:"attempts"`
	Failures int       `json:"failures"`
	Locked   time.Time `json:"locked"`
}
type remoteLocks struct {
	Sources        map[string]remoteSource `json:"sources"`
	Trusted        map[string]time.Time    `json:"trusted,omitempty"` // sources (IPv6: /64) with a good sign-in in the last 8 h; skip only the global lockout
	GlobalUntil    time.Time               `json:"globalUntil"`
	GlobalWindow   time.Time               `json:"globalWindow"`
	GlobalFailures int                     `json:"globalFailures"`
}

type remoteAuth struct {
	mu         sync.Mutex
	now        func() time.Time
	dir        string
	credential [32]byte
	sessions   map[[32]byte]remoteSession
	locks      remoteLocks
	hashing    int
	epoch      uint64
	disabled   bool
	derive     func([]byte, remotePassword) []byte
}

func newRemoteAuth(config string, now func() time.Time) (*remoteAuth, error) {
	if now == nil {
		now = time.Now
	}
	a := &remoteAuth{now: now, dir: filepath.Dir(config), sessions: map[[32]byte]remoteSession{}}
	if runtime.GOOS == "windows" { // once here and in the password command, not on every request
		if err := protectDir(a.dir); err != nil {
			return nil, err
		}
	}
	a.derive = func(password []byte, p remotePassword) []byte {
		return argon2.IDKey(password, p.Salt, p.Iterations, p.Memory, p.Parallelism, 32)
	}
	// A missing credential cannot authenticate; current rechecks it before use.
	_, a.credential, _ = loadRemotePassword(remotePasswordPath(config))
	a.locks.Sources = map[string]remoteSource{}
	f, err := os.Open(a.lockPath())
	if errors.Is(err, os.ErrNotExist) {
		return a, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 256<<10))
	if err != nil {
		return nil, err
	}
	if err = decodeStrict(b, &a.locks); err != nil {
		return nil, err
	}
	if len(a.locks.Sources) > remoteSourceCap || a.locks.GlobalFailures < 0 || a.locks.GlobalFailures > 20 {
		return nil, errors.New("invalid remote lockouts")
	}
	// A clock stepped backwards leaves timestamps in the future: clamp them, do not refuse.
	t := now()
	clamp := func(v *time.Time, max time.Time) {
		if v.After(max) {
			*v = max
		}
	}
	clamp(&a.locks.GlobalUntil, t.Add(time.Minute))
	clamp(&a.locks.GlobalWindow, t)
	for ip, s := range a.locks.Sources {
		if _, err := netip.ParseAddr(ip); err != nil || s.Attempts < 0 || s.Attempts > 5 || s.Failures < 0 || s.Failures > 5 {
			return nil, errors.New("invalid remote lockouts")
		}
		clamp(&s.Locked, t.Add(15*time.Minute))
		clamp(&s.Window, t)
		a.locks.Sources[ip] = s
	}
	if len(a.locks.Trusted) > remoteTrustedCap {
		return nil, errors.New("invalid remote lockouts")
	}
	for ip, v := range a.locks.Trusted {
		if _, err := netip.ParseAddr(ip); err != nil {
			return nil, errors.New("invalid remote lockouts")
		}
		clamp(&v, t)
		a.locks.Trusted[ip] = v
	}
	if a.locks.Sources == nil {
		a.locks.Sources = map[string]remoteSource{}
	}
	return a, nil
}
func (a *remoteAuth) lockPath() string { return filepath.Join(a.dir, "remote-settings.lockouts.json") }
func (a *remoteAuth) saveLocks() error {
	b, err := json.Marshal(a.locks)
	if err != nil {
		return err
	}
	return writeFileAtomic(a.lockPath(), b)
}
func (a *remoteAuth) audit(action, result, ip, rid string) error {
	return a.auditOperation(action, result, ip, rid, "")
}

func (a *remoteAuth) auditOperation(action, result, ip, rid, operation string) error {
	allowed := map[string]bool{"login": true, "logout": true, "revoke": true, "mutation": true, "lockout": true}
	outcomes := map[string]bool{"ok": true, "denied": true, "limited": true, "error": true, "started": true}
	if !allowed[action] || !outcomes[result] {
		return errors.New("invalid audit event")
	}
	if operation != "" {
		switch operation {
		case "POST providers", "PATCH providers/{}", "DELETE providers/{}", "POST providers/{}/models", "POST devices", "PATCH devices/{}", "DELETE devices/{}":
		default:
			return errors.New("invalid audit operation")
		}
		if action != "mutation" {
			return errors.New("invalid audit action")
		}
	}
	if ip != "" {
		if _, err := netip.ParseAddr(ip); err != nil {
			return err
		}
	}
	if rid != "" && (len(rid) != 36 || !strings.HasPrefix(rid, "req_")) {
		return errors.New("invalid audit request id")
	}
	if rid != "" {
		if _, err := hex.DecodeString(rid[4:]); err != nil {
			return errors.New("invalid audit request id")
		}
	}
	path := filepath.Join(a.dir, "remote-settings.audit.jsonl")
	if st, err := os.Lstat(path); err == nil {
		if !st.Mode().IsRegular() {
			return errors.New("invalid audit file")
		}
		if st.Size() >= 1<<20 {
			if err = os.Rename(path, path+".1"); err != nil {
				return err
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err = f.Chmod(0600); err != nil {
		return err
	}
	b, _ := json.Marshal(struct {
		Time                                                time.Time
		Principal, IP, RequestID, Action, Result, Operation string
	}{a.now().UTC(), "remote-admin", ip, rid, action, result, operation})
	if _, err = f.Write(append(b, '\n')); err != nil {
		return err
	}
	return f.Sync()
}
func (a *remoteAuth) purge(now time.Time) {
	for k, s := range a.sessions {
		if !now.Before(s.Used.Add(30*time.Minute)) || !now.Before(s.Created.Add(8*time.Hour)) {
			delete(a.sessions, k)
		}
	}
	for ip, s := range a.locks.Sources {
		if !now.Before(s.Locked) && !now.Before(s.Window.Add(15*time.Minute)) {
			delete(a.locks.Sources, ip)
		}
	}
	for ip, v := range a.locks.Trusted {
		if !now.Before(v.Add(remoteTrustFor)) {
			delete(a.locks.Trusted, ip)
		}
	}
}

// remoteSourceKey: IPv4 as is (IPv4-mapped unmapped), IPv6 by its /64 so one network cannot mint endless sources.
func remoteSourceKey(addr netip.Addr) string {
	addr = addr.Unmap()
	if addr.Is6() {
		return netip.PrefixFrom(addr, 64).Masked().Addr().String()
	}
	return addr.String()
}

// evictSource makes room: the oldest unlocked source goes first, then the one locked longest ago.
func (a *remoteAuth) evictSource(now time.Time) {
	victim, locked, at := "", true, time.Time{}
	for k, s := range a.locks.Sources {
		l, t := now.Before(s.Locked), s.Window
		if l {
			t = s.Locked
		}
		if victim == "" || locked && !l || locked == l && t.Before(at) {
			victim, locked, at = k, l, t
		}
	}
	delete(a.locks.Sources, victim)
}
func (a *remoteAuth) revoke() {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sessions = map[[32]byte]remoteSession{}
	a.epoch++
	a.audit("revoke", "ok", "", "")
}

func (a *remoteAuth) disable() {
	if a == nil {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.disabled = true
	a.epoch++
	a.sessions = map[[32]byte]remoteSession{}
	a.audit("revoke", "ok", "", "")
}
func (a *remoteAuth) current() (remotePassword, error) {
	p, d, err := loadRemotePassword(filepath.Join(a.dir, "remote-settings.password.json"))
	if d != a.credential || err != nil && len(a.sessions) != 0 {
		a.sessions = map[[32]byte]remoteSession{}
		a.epoch++
		a.credential = d
		if auditErr := a.audit("revoke", "ok", "", ""); err == nil && auditErr != nil {
			err = auditErr
		}
	}
	return p, err
}

func (a *remoteAuth) login(ip, rid, password string) (token string, retry time.Duration, err error) {
	addr, e := netip.ParseAddr(ip)
	if e != nil || addr.Zone() != "" {
		return "", 0, errRemoteLogin
	}
	ip = remoteSourceKey(addr)
	a.mu.Lock()
	if a.disabled {
		a.mu.Unlock()
		return "", 0, errRemoteLogin
	}
	now := a.now()
	a.purge(now)
	s, exists := a.locks.Sources[ip]
	until := s.Locked
	if a.locks.GlobalUntil.After(until) && !a.locks.Trusted[ip].After(now.Add(-remoteTrustFor)) { // trusted: only its own limit applies
		until = a.locks.GlobalUntil
	}
	if now.Before(until) {
		a.audit("lockout", "limited", ip, rid)
		a.mu.Unlock()
		return "", until.Sub(now), errRemoteLogin
	}
	if a.hashing >= 2 {
		a.mu.Unlock()
		return "", time.Minute, errRemoteLogin
	}
	if !exists && len(a.locks.Sources) >= remoteSourceCap {
		a.evictSource(now)
	}
	if !now.Before(s.Window.Add(time.Minute)) {
		s.Window = now
		s.Attempts = 0
	}
	if s.Attempts >= 5 {
		a.mu.Unlock()
		return "", s.Window.Add(time.Minute).Sub(now), errRemoteLogin
	}
	s.Attempts++
	a.locks.Sources[ip] = s
	if e = a.saveLocks(); e != nil {
		a.mu.Unlock()
		return "", 0, errRemoteLogin
	}
	p, e := a.current()
	if e != nil {
		a.mu.Unlock()
		return "", 0, errRemoteLogin
	}
	generation := a.credential
	epoch := a.epoch
	a.hashing++
	a.mu.Unlock()
	var hash []byte
	if len(password) <= 1024 {
		hash = a.derive([]byte(password), p)
	}
	good := subtle.ConstantTimeCompare(hash, p.Hash) == 1
	a.mu.Lock()
	defer a.mu.Unlock()
	a.hashing--
	now = a.now()
	a.current()
	if generation != a.credential || epoch != a.epoch || a.disabled {
		return "", 0, errRemoteLogin
	}
	s = a.locks.Sources[ip]
	if !good {
		s.Failures++
		if s.Failures >= 5 {
			s.Failures = 5
			s.Locked = now.Add(15 * time.Minute)
		}
		a.locks.Sources[ip] = s
		if !now.Before(a.locks.GlobalWindow.Add(time.Minute)) {
			a.locks.GlobalWindow = now
			a.locks.GlobalFailures = 0
		}
		a.locks.GlobalFailures++
		if a.locks.GlobalFailures >= 20 {
			a.locks.GlobalFailures = 20
			a.locks.GlobalUntil = now.Add(time.Minute)
		}
		if a.saveLocks() != nil || a.audit("login", "denied", ip, rid) != nil {
			return "", 0, errRemoteLogin
		}
		if now.Before(s.Locked) {
			return "", s.Locked.Sub(now), errRemoteLogin
		}
		return "", 0, errRemoteLogin
	}
	a.purge(now)
	if len(a.sessions) >= remoteSessionCap {
		return "", time.Minute, errRemoteLogin
	}
	var raw [32]byte
	if _, e = rand.Read(raw[:]); e != nil {
		return "", 0, errRemoteLogin
	}
	token = hex.EncodeToString(raw[:])
	if a.audit("login", "ok", ip, rid) != nil {
		return "", 0, errRemoteLogin
	}
	s.Failures = 0
	a.locks.Sources[ip] = s
	if a.locks.Trusted == nil {
		a.locks.Trusted = map[string]time.Time{}
	}
	if _, ok := a.locks.Trusted[ip]; !ok && len(a.locks.Trusted) >= remoteTrustedCap {
		oldest := ""
		for k, v := range a.locks.Trusted {
			if oldest == "" || v.Before(a.locks.Trusted[oldest]) {
				oldest = k
			}
		}
		delete(a.locks.Trusted, oldest)
	}
	a.locks.Trusted[ip] = now
	if a.saveLocks() != nil {
		return "", 0, errRemoteLogin
	}
	a.sessions[sha256.Sum256([]byte(token))] = remoteSession{now, now}
	return token, 0, nil
}
func (a *remoteAuth) valid(token string) bool {
	if a == nil || len(token) != 64 {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.disabled {
		return false
	}
	if _, err := a.current(); err != nil {
		return false
	}
	now := a.now()
	a.purge(now)
	key := sha256.Sum256([]byte(token))
	s, ok := a.sessions[key]
	if ok {
		s.Used = now
		a.sessions[key] = s
	}
	return ok
}
func (a *remoteAuth) logout(token, ip, rid string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.sessions, sha256.Sum256([]byte(token)))
	return a.audit("logout", "ok", ip, rid)
}

// The transport supplies a verified source and a server-generated request ID.
func (a *remoteAuth) loginResponse(w http.ResponseWriter, ip, rid, password string) int {
	w.Header().Set("Cache-Control", "no-store")
	token, retry, err := a.login(ip, rid, password)
	if retry > 0 {
		seconds := int64((retry + time.Second - 1) / time.Second)
		w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
		return writeError(w, rid, http.StatusTooManyRequests, errRemoteLogin.Error())
	}
	if err != nil {
		return writeError(w, rid, http.StatusUnauthorized, errRemoteLogin.Error())
	}
	return writeJSON(w, rid, http.StatusOK, map[string]string{"session": token})
}
