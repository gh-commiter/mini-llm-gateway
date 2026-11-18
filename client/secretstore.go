package main

// Secrets by name (plan docs/plans/secrets-out-of-files-plan.md, step 2): ONE interface, secretStore, behind keyGet,
// keySet and keyDel (provider keys, agent tokens and API keys, upstream credentials). The backend is chosen ONCE per
// config folder, at the first use (the gateway forces it at start), in this fixed order:
//
//	(a) the credential store of the signed-in user (Windows Credential Manager, Linux Secret Service through
//	    secret-tool) when this is not container mode and the store answers a probe;
//	(b) files the platform provides, read-only: $CREDENTIALS_DIRECTORY (systemd) or $MINIGW_SECRETS_DIR (Docker or
//	    Compose secrets, for example /run/secrets); a name that is not there lives in (c);
//	(c) the last resort: gateway-keys.json next to the config (0600; Windows: the owner/SYSTEM ACL of the config folder).
//
// MINIGW_SECRETS=file forces (c). The choice is never changed while the process runs. A store that fails AFTER it was
// chosen is an ERROR for the caller, never a fall-through to a weaker place. No value is ever put in an error, a log
// or the status line (Where).

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

type secretStore interface {
	// Get: "" and a nil error = there is no such secret; where = the place that answered (never a value).
	Get(name string) (value, where string, err error)
	Set(name, value string) (where string, err error)
	Delete(name string) error
	Where() string // one line for the status: where secrets are kept, never a value
}

// ---- (c) the file ----

func keyFile(cfgDir string) string { return filepath.Join(cfgDir, "gateway-keys.json") }

// fileKeys: manjkajoča datoteka = prazno; pokvarjena = napaka (sicer bi naslednji add izbrisal vse ključe).
func fileKeys(cfgDir string) (map[string]string, error) {
	m := map[string]string{}
	data, err := os.ReadFile(keyFile(cfgDir))
	if errors.Is(err, os.ErrNotExist) {
		return m, nil
	}
	if err == nil {
		err = json.Unmarshal(data, &m)
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %v", keyFile(cfgDir), err)
	}
	return m, nil
}

func writeKeys(cfgDir string, m map[string]string) error {
	data, _ := json.MarshalIndent(m, "", "  ")
	return writeFileAtomic(keyFile(cfgDir), data)
}

type fileStore struct{ cfgDir, why string }

func (f fileStore) Get(name string) (string, string, error) {
	m, err := fileKeys(f.cfgDir)
	return m[name], keyFile(f.cfgDir), err
}

func (f fileStore) Set(name, value string) (string, error) {
	m, err := fileKeys(f.cfgDir)
	if err != nil {
		return "", err
	}
	m[name] = value
	where := keyFile(f.cfgDir) + " (mode 0600)"
	if runtime.GOOS == "linux" && !containerMode() {
		where = "WARNING: no Secret Service (secret-tool), so it is in " + where
	}
	return where, writeKeys(f.cfgDir, m)
}

func (f fileStore) Delete(name string) error {
	m, err := fileKeys(f.cfgDir)
	if err != nil || m[name] == "" {
		return err
	}
	delete(m, name)
	return writeKeys(f.cfgDir, m)
}

func (f fileStore) Where() string { return "file " + keyFile(f.cfgDir) + " (" + f.why + ")" }

// ---- (b) platform files ----

// platformStore: reads dir/<name with ":" as "_"> (read-only); everything it does not provide is in the file.
type platformStore struct {
	dir  string
	file fileStore
}

func (p platformStore) path(name string) (string, error) {
	fn := strings.ReplaceAll(name, ":", "_")
	if fn == "" || fn != filepath.Base(fn) || fn == "." || fn == ".." {
		return "", errors.New("invalid secret name")
	}
	return filepath.Join(p.dir, fn), nil
}

func (p platformStore) Get(name string) (string, string, error) {
	path, err := p.path(name)
	if err != nil {
		return "", "", err
	}
	b, err := os.ReadFile(path)
	switch {
	case err == nil:
		return strings.TrimSpace(string(b)), p.dir, nil
	case errors.Is(err, os.ErrNotExist):
		return p.file.Get(name)
	}
	return "", "", fmt.Errorf("secret file %s: %w", path, errors.Unwrap(err)) // no content: only the path and the OS reason
}

// provided: the platform has this name (a read-only secret).
func (p platformStore) provided(name string) bool {
	path, err := p.path(name)
	if err != nil {
		return false
	}
	_, err = os.Stat(path)
	return err == nil
}

func (p platformStore) Set(name, value string) (string, error) {
	if p.provided(name) {
		return "", errors.New(name + " is provided by the platform (" + p.dir + ", read-only): change it there")
	}
	return p.file.Set(name, value)
}

func (p platformStore) Delete(name string) error {
	if p.provided(name) {
		return errors.New(name + " is provided by the platform (" + p.dir + ", read-only): remove it there")
	}
	return p.file.Delete(name)
}

func (p platformStore) Where() string {
	return "platform files " + p.dir + " (read-only; others in file " + keyFile(p.file.cfgDir) + ")"
}

// ---- (a) the user's credential store ----

// sysStore: the callbacks are the platform's (secretstore_windows.go, secretstore_other.go). Their error is an error
// of the store itself ("not found" is "", nil). legacy: an older copy in the file (Linux before step 2) is still read
// when the store has none, and is removed on a write: the places did not change.
type sysStore struct {
	label  string
	get    func(name string) (string, error)
	set    func(name, value string) error
	del    func(name string) error
	legacy *fileStore
}

func (s sysStore) Get(name string) (string, string, error) {
	v, err := s.get(name)
	if err != nil {
		return "", "", err // chosen, then failed: an error, not the file
	}
	if v == "" && s.legacy != nil {
		return s.legacy.Get(name)
	}
	return v, s.label, nil
}

func (s sysStore) Set(name, value string) (string, error) {
	if err := s.set(name, value); err != nil {
		return "", err
	}
	if s.legacy != nil {
		if m, _ := fileKeys(s.legacy.cfgDir); m[name] != "" {
			s.legacy.Delete(name)
		}
	}
	return s.label, nil
}

func (s sysStore) Delete(name string) error {
	err := s.del(name)
	if s.legacy != nil {
		if lerr := s.legacy.Delete(name); err == nil {
			err = lerr
		}
	}
	return err
}

func (s sysStore) Where() string { return s.label }

// ---- the choice ----

// secretEnv: everything the choice looks at, so tests inject it (they never touch a real store).
type secretEnv struct {
	container bool
	forceFile bool
	dirs      []string                      // candidates for (b), in order
	sys       func(cfgDir string) *sysStore // nil result: no store answers for this session
}

func osSecretEnv() secretEnv {
	e := secretEnv{container: containerMode(), forceFile: os.Getenv("MINIGW_SECRETS") == "file", sys: osSysStore}
	for _, k := range []string{"CREDENTIALS_DIRECTORY", "MINIGW_SECRETS_DIR"} {
		if d := os.Getenv(k); d != "" {
			e.dirs = append(e.dirs, d)
		}
	}
	return e
}

func chooseSecretStore(cfgDir string, e secretEnv) secretStore {
	why := "no credential store for this session"
	switch {
	case e.forceFile:
		why = "forced by MINIGW_SECRETS=file"
	case e.container:
		why = "container mode"
	default:
		if e.sys != nil {
			if s := e.sys(cfgDir); s != nil {
				return s
			}
		}
	}
	f := fileStore{cfgDir, why}
	if !e.forceFile {
		for _, d := range e.dirs {
			if st, err := os.Stat(d); err == nil && st.IsDir() {
				return platformStore{filepath.Clean(d), f}
			}
		}
	}
	return f
}

var secretChoice = struct {
	sync.Mutex
	env    func() secretEnv
	stores map[string]secretStore
}{env: osSecretEnv, stores: map[string]secretStore{}}

// secretsFor: the store of this config folder; chosen at the first call, the same one for the life of the process.
func secretsFor(cfgDir string) secretStore {
	secretChoice.Lock()
	defer secretChoice.Unlock()
	s, ok := secretChoice.stores[cfgDir]
	if !ok {
		s = chooseSecretStore(cfgDir, secretChoice.env())
		secretChoice.stores[cfgDir] = s
	}
	return s
}

// The names keyGet, keySet and keyDel stay the seam of the code and of the tests (they replace them).
// A failed read is "" (as before: "no key"); callers that must tell the two apart use secretsFor(cfgDir).Get.
var keyGet = func(cfgDir, name string) (string, string) {
	v, where, _ := secretsFor(cfgDir).Get(name)
	return v, where
}
var keySet = func(cfgDir, name, value string) (string, error) { return secretsFor(cfgDir).Set(name, value) }
var keyDel = func(cfgDir, name string) { secretsFor(cfgDir).Delete(name) }
