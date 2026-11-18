package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const secretTestValue = "SECRET-VALUE-4f9c1d"

// fakeSys: an in-memory credential store; calls counts every use (a store that must not be tried stays at 0).
func fakeSys(cfgDir string, withLegacy bool) (*sysStore, map[string]string, *int) {
	m, calls := map[string]string{}, new(int)
	s := &sysStore{label: "fake credential store",
		get: func(n string) (string, error) { *calls++; return m[n], nil },
		set: func(n, v string) error { *calls++; m[n] = v; return nil },
		del: func(n string) error { *calls++; delete(m, n); return nil }}
	if withLegacy {
		s.legacy = &fileStore{cfgDir: cfgDir}
	}
	return s, m, calls
}

func TestSecretChoiceOrder(t *testing.T) {
	dir, plat := t.TempDir(), t.TempDir()
	sys, _, calls := fakeSys(dir, false)
	cases := []struct {
		name  string
		env   secretEnv
		kind  string
		where string
	}{
		{"signed-in user with a store", secretEnv{sys: func(string) *sysStore { return sys }}, "sys", "fake credential store"},
		{"store absent", secretEnv{sys: func(string) *sysStore { return nil }}, "file", "(no credential store for this session)"},
		{"no probe at all", secretEnv{}, "file", "(no credential store for this session)"},
		{"container never tries the store", secretEnv{container: true, sys: func(string) *sysStore { t.Fatal("store tried in container mode"); return nil }}, "file", "(container mode)"},
		{"platform directory", secretEnv{dirs: []string{plat}}, "platform", "platform files " + plat + " (read-only; others in file " + keyFile(dir) + ")"},
		{"the store comes before platform files", secretEnv{dirs: []string{plat}, sys: func(string) *sysStore { return sys }}, "sys", "fake credential store"},
		{"container with platform directory", secretEnv{container: true, dirs: []string{plat}}, "platform", "platform files"},
		{"missing platform directory is skipped", secretEnv{dirs: []string{filepath.Join(plat, "nope")}}, "file", "no credential store"},
		{"forced file", secretEnv{forceFile: true, dirs: []string{plat}, sys: func(string) *sysStore { t.Fatal("store tried when forced"); return nil }}, "file", "(forced by MINIGW_SECRETS=file)"},
	}
	for _, c := range cases {
		s := chooseSecretStore(dir, c.env)
		kind := ""
		switch s.(type) {
		case *sysStore:
			kind = "sys"
		case platformStore:
			kind = "platform"
		case fileStore:
			kind = "file"
		}
		if kind != c.kind || !strings.Contains(s.Where(), c.where) {
			t.Errorf("%s: %T %q", c.name, s, s.Where())
		}
	}
	if *calls != 0 {
		t.Errorf("choosing must not read or write a secret (%d calls)", *calls)
	}
}

func TestSecretChosenOnceAndFailureIsAnError(t *testing.T) {
	dir := t.TempDir()
	sys, _, _ := fakeSys(dir, true)
	broken := false
	sys.get = func(string) (string, error) {
		if broken {
			return "", errors.New("the keyring is locked")
		}
		return "", nil
	}
	sys.set = func(string, string) error {
		if broken {
			return errors.New("the keyring is locked")
		}
		return nil
	}
	env := secretEnv{sys: func(string) *sysStore { return sys }}
	secretChoice.Lock()
	old := secretChoice.env
	secretChoice.env, secretChoice.stores = func() secretEnv { return env }, map[string]secretStore{}
	secretChoice.Unlock()
	t.Cleanup(func() {
		secretChoice.Lock()
		secretChoice.env, secretChoice.stores = old, map[string]secretStore{}
		secretChoice.Unlock()
	})
	first := secretsFor(dir)
	env = secretEnv{forceFile: true} // the world changes: the choice does not
	if second := secretsFor(dir); second.Where() != first.Where() || second.Where() != "fake credential store" {
		t.Fatalf("the choice changed while running: %q then %q", first.Where(), second.Where())
	}
	// a legacy copy in the file would be a weaker place: after the store failed, it is NOT consulted and NOT written
	writeKeys(dir, map[string]string{"old": secretTestValue})
	broken = true
	if v, _, err := first.Get("old"); err == nil || v != "" {
		t.Fatalf("a failed store read fell through: %q %v", v, err)
	}
	if _, err := first.Set("new", secretTestValue); err == nil {
		t.Fatal("a failed store write did not return an error")
	}
	m, _ := fileKeys(dir)
	if _, ok := m["new"]; ok || m["old"] != secretTestValue {
		t.Fatalf("the file changed after the store failed: %v", m)
	}
	// the code's seam reports the error as "no key", never as a key from a weaker place
	if v, _ := keyGet(dir, "old"); v != "" {
		t.Fatalf("keyGet fell through: %q", v)
	}
	if _, err := keySet(dir, "new", secretTestValue); err == nil {
		t.Fatal("keySet swallowed the store error")
	}
}

func TestSysStoreLegacyFileKeepsPlaces(t *testing.T) {
	dir := t.TempDir()
	sys, m, _ := fakeSys(dir, true)
	writeKeys(dir, map[string]string{"gw": "old-key"})
	if v, where, err := sys.Get("gw"); v != "old-key" || err != nil || where != keyFile(dir) {
		t.Fatalf("legacy read: %q %q %v", v, where, err)
	}
	if where, err := sys.Set("gw", secretTestValue); err != nil || where != "fake credential store" || m["gw"] != secretTestValue {
		t.Fatalf("set: %q %v", where, err)
	}
	if f, _ := fileKeys(dir); len(f) != 0 {
		t.Fatalf("an older copy stayed in the file: %v", f)
	}
	sys.Delete("gw")
	if _, ok := m["gw"]; ok {
		t.Fatal("not deleted")
	}
}

func TestPlatformStoreReadOnly(t *testing.T) {
	dir, plat := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(plat, "agent_claude"), []byte(secretTestValue+"\n"), 0o400)
	s := chooseSecretStore(dir, secretEnv{dirs: []string{plat}})
	if v, where, err := s.Get("agent:claude"); v != secretTestValue || where != plat || err != nil {
		t.Fatalf("get: %q %q %v", v, where, err)
	}
	if _, err := s.Set("agent:claude", "x"); err == nil || strings.Contains(err.Error(), secretTestValue) {
		t.Fatalf("a platform secret was written: %v", err)
	}
	if err := s.Delete("agent:claude"); err == nil {
		t.Fatal("a platform secret was deleted")
	}
	// what the platform does not provide lives in the file
	if _, err := s.Set("gw", "k1"); err != nil {
		t.Fatal(err)
	}
	if m, _ := fileKeys(dir); m["gw"] != "k1" {
		t.Fatalf("file: %v", m)
	}
	if v, _, _ := s.Get("gw"); v != "k1" {
		t.Fatalf("get gw: %q", v)
	}
	for _, bad := range []string{"../x", "a/b", ""} {
		if _, _, err := s.Get(bad); err == nil {
			t.Errorf("name %q accepted", bad)
		}
	}
	// a platform file that cannot be read is an error, not the file
	os.WriteFile(filepath.Join(plat, "dir_secret"), nil, 0o600)
	os.Remove(filepath.Join(plat, "dir_secret"))
	os.Mkdir(filepath.Join(plat, "dir_secret"), 0o700)
	if _, _, err := s.Get("dir:secret"); err == nil {
		t.Fatal("an unreadable platform secret fell through")
	}
}

func TestFileStoreMode(t *testing.T) {
	dir := t.TempDir()
	s := chooseSecretStore(dir, secretEnv{})
	if _, err := s.Set("gw", secretTestValue); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" { // Windows: the ACL of the config folder (protectDir), not mode bits
		if st, err := os.Stat(keyFile(dir)); err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("mode: %v %v", st, err)
		}
	}
	if v, _, _ := s.Get("gw"); v != secretTestValue {
		t.Fatalf("get: %q", v)
	}
	s.Delete("gw")
	if b, _ := os.ReadFile(keyFile(dir)); strings.Contains(string(b), secretTestValue) {
		t.Fatal("not deleted")
	}
	// a damaged file is an error, never an empty list that the next write would replace
	os.WriteFile(keyFile(dir), []byte("{broken"), 0o600)
	if _, err := s.Set("gw", "x"); err == nil {
		t.Fatal("damaged file overwritten")
	}
}

// No value in the status line, the status answer or a log, whichever place is chosen.
func TestSecretNeverInStatus(t *testing.T) {
	dir, plat := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(plat, "plat"), []byte(secretTestValue), 0o400)
	sys, _, _ := fakeSys(dir, true)
	for name, e := range map[string]secretEnv{
		"sys": {sys: func(string) *sysStore { return sys }}, "file": {}, "platform": {dirs: []string{plat}}, "container": {container: true},
	} {
		s := chooseSecretStore(dir, e)
		s.Set("gw", secretTestValue)
		s.Get("gw")
		line := s.Where()
		b, _ := json.Marshal(apiStatus{Secrets: line})
		if strings.Contains(line+string(b), secretTestValue) || line == "" || strings.Contains(line, "\n") {
			t.Errorf("%s: %q", name, line)
		}
	}
}

// The status answer of a running gateway carries the line, and `minigw gw status` prints it (a value is nowhere).
func TestStatusCarriesSecretsLine(t *testing.T) {
	s, cfg, _ := liveServer(t, nil)
	keySet(s.cfgDir, "probe-name", secretTestValue)
	_, st := s.adminDo("GET", "/status", nil)
	sb, _ := json.Marshal(st)
	var got struct{ Secrets string }
	if err := json.Unmarshal(sb, &got); err != nil { // decoded: a Windows path has its backslashes doubled in the JSON text
		t.Fatal(err)
	}
	if want := "file " + keyFile(s.cfgDir) + " (forced by MINIGW_SECRETS=file)"; got.Secrets != want || strings.Contains(got.Secrets, secretTestValue) {
		t.Fatalf("status secrets: %q, want %q", got.Secrets, want)
	}
	out := stdout(t, func() { routerCommand([]string{"status", "--config", cfg}, strings.NewReader("")) })
	if !strings.Contains(out, "secrets: file "+keyFile(s.cfgDir)+" (forced by MINIGW_SECRETS=file)") || strings.Contains(out, secretTestValue) {
		t.Fatalf("minigw gw status: %q", out)
	}
}

// Item names and places that the other programs (the Mac app, minigw on the Mac, the Windows tray) rely on: the same
// fixture is read by Tests/RouterCoreTests/SecretNamesTests.swift.
func TestSecretNamesPinned(t *testing.T) {
	b, err := os.ReadFile("testdata/secret-names.json")
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		AgentLogin, AgentAPIKey, Upstream string
		WindowsGatewayTarget              string
		SecretToolAttrs                   []string
		GatewayKeysFile                   string
		PlatformFileName                  map[string]string
	}
	if err := json.Unmarshal(b, &f); err != nil {
		t.Fatal(err)
	}
	if got := agentLoginKey("claude"); got != strings.Replace(f.AgentLogin, "<kind>", "claude", 1) {
		t.Errorf("agent login item: %q", got)
	}
	if got := agentAPIKeyName("codex"); got != strings.Replace(f.AgentAPIKey, "<kind>", "codex", 1) {
		t.Errorf("agent API key item: %q", got)
	}
	if got := upstreamKeyName("x"); got != strings.Replace(f.Upstream, "<name>", "x", 1) {
		t.Errorf("upstream item: %q", got)
	}
	if filepath.Base(keyFile("d")) != f.GatewayKeysFile {
		t.Errorf("key file: %q", keyFile("d"))
	}
	for name, want := range f.PlatformFileName {
		if p, _ := (platformStore{dir: "d"}).path(name); filepath.Base(p) != want {
			t.Errorf("platform file for %q: %q", name, p)
		}
	}
}

// The Secret Service and Credential Manager item shapes are in the code of the platform files; pinned by reading them,
// because a test must not start secret-tool or call the Credential Manager.
func TestPlatformItemShapesPinned(t *testing.T) {
	b, _ := os.ReadFile("testdata/secret-names.json")
	var f struct {
		WindowsGatewayTarget string
		SecretToolAttrs      []string
	}
	json.Unmarshal(b, &f)
	win, _ := os.ReadFile("secretstore_windows.go")
	oth, _ := os.ReadFile("secretstore_other.go")
	if !strings.Contains(string(win), `"`+f.WindowsGatewayTarget+`"`) {
		t.Errorf("Windows target %q not in secretstore_windows.go", f.WindowsGatewayTarget)
	}
	if !strings.Contains(string(oth), `"lookup", "service", "minigw", "gateway", name`) || !strings.Contains(string(oth), `"service", "minigw", "gateway", name`) ||
		strings.Join(f.SecretToolAttrs, " ") != "service minigw gateway <name>" {
		t.Error("secret-tool attributes changed")
	}
}
