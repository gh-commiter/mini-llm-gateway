//go:build !windows

package main

// Povezava v datoteki uporabnika z dovoljenji 0600 (Linux, in macOS pri preizkusu; macOS minigw v aplikaciji
// uporablja Keychain). ponytail: na Linuxu brez Secret Service; dodaj, če bo potreben namizni keyring.

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
)

var errNotFound = errors.New("not found")

func storePath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "minigw", "connection.json")
}

// profilePath: profil »default« je prejšnja edina povezava (connection.json), drugi v connections/<ime>.json (#17).
func profilePath(name string) string {
	if name == defaultProfile {
		return storePath()
	}
	return filepath.Join(filepath.Dir(storePath()), "connections", name+".json")
}

func storeLoad(name string) ([]byte, error) {
	if !validProfile(name) {
		return nil, errNotFound
	}
	data, err := os.ReadFile(profilePath(name))
	if errors.Is(err, os.ErrNotExist) {
		return nil, errNotFound
	}
	return data, err
}

func storeSave(name string, data []byte) error {
	if !validProfile(name) {
		return errors.New("invalid profile name")
	}
	if err := os.MkdirAll(filepath.Dir(profilePath(name)), 0o700); err != nil {
		return err
	}
	return os.WriteFile(profilePath(name), data, 0o600)
}

func storeDelete(name string) {
	if validProfile(name) {
		os.Remove(profilePath(name))
	}
}

// storeNames: imena shranjenih profilov (neveljavna imena datotek prezre).
func storeNames() []string {
	var out []string
	if _, err := os.Stat(storePath()); err == nil {
		out = append(out, defaultProfile)
	}
	ents, _ := os.ReadDir(filepath.Join(filepath.Dir(storePath()), "connections"))
	for _, e := range ents {
		if n, ok := strings.CutSuffix(e.Name(), ".json"); ok && validProfile(n) && n != defaultProfile && e.Type().IsRegular() {
			out = append(out, n)
		}
	}
	return out
}

const storeName = "a private file"

// Ključi prehodov (minigw serve gateway). Linux: secret-tool (libsecret), sicer gateway-keys.json (0600) ob serve.json.
// macOS: vedno ta datoteka, nikoli Keychain (Go minigw serve na Macu je le za razvoj in preizkus; router je aplikacija).

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

func secretTool() string {
	if runtime.GOOS != "linux" {
		return ""
	}
	tool, _ := exec.LookPath("secret-tool")
	return tool
}

func osKeyGet(cfgDir, name string) (string, string) {
	if tool := secretTool(); tool != "" {
		if out, _ := command(tool, "lookup", "service", "minigw", "gateway", name).Output(); len(out) > 0 {
			return strings.TrimSpace(string(out)), "the Secret Service"
		}
	}
	m, _ := fileKeys(cfgDir)
	return m[name], keyFile(cfgDir)
}

// osKeySet: ključ nikoli v argv (secret-tool ga bere s stdin).
func osKeySet(cfgDir, name, key string) (string, error) {
	if tool := secretTool(); tool != "" {
		cmd := command(tool, "store", "--label=minigw gateway "+name, "service", "minigw", "gateway", name)
		cmd.Stdin = strings.NewReader(key)
		if cmd.Run() == nil {
			m, _ := fileKeys(cfgDir) // starejša kopija v datoteki ne sme ostati
			if _, ok := m[name]; ok {
				delete(m, name)
				writeKeys(cfgDir, m)
			}
			return "the Secret Service", nil
		}
	}
	m, err := fileKeys(cfgDir)
	if err != nil {
		return "", err
	}
	m[name] = key
	where := keyFile(cfgDir) + " (mode 0600)"
	if runtime.GOOS == "linux" {
		where = "WARNING: no Secret Service (secret-tool), so it is in " + where
	}
	return where, writeKeys(cfgDir, m)
}

func writeKeys(cfgDir string, m map[string]string) error {
	data, _ := json.MarshalIndent(m, "", "  ")
	return writeFileAtomic(keyFile(cfgDir), data)
}

func osKeyDel(cfgDir, name string) {
	if tool := secretTool(); tool != "" {
		command(tool, "clear", "service", "minigw", "gateway", name).Run()
	}
	if m, err := fileKeys(cfgDir); err == nil && m[name] != "" {
		delete(m, name)
		writeKeys(cfgDir, m)
	}
}

// readHidden: vrstica s terminala brez odmeva (stty).
func readHidden(f *os.File) (string, error) {
	off := command("stty", "-echo")
	off.Stdin = f
	if off.Run() == nil {
		defer func() {
			on := command("stty", "echo")
			on.Stdin = f
			on.Run()
		}()
	}
	return bufio.NewReader(f).ReadString('\n')
}

// Vrednost Run v registru obstaja le na Windows.
func regRunSet(string, string) error   { return errors.New("the registry exists on Windows only") }
func regRunGet(string) (string, error) { return "", errNotFound }
func regRunDel(string)                 {}
func msiInstallDir() string            { return "" }
func msiexecPath() string              { return "msiexec" }

func protectDir(string) error { return nil } // drugod: mapa 0700, datoteke 0600

// protectScratch (L2): mapa s kopijami map odjemalcev je naša (Lstat: ne povezava) in le naša (0700).
func protectScratch(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); !fi.IsDir() || !ok || int(st.Uid) != os.Getuid() {
		return errForeignOwner
	}
	if fi.Mode().Perm() != 0o700 {
		return os.Chmod(dir, 0o700)
	}
	return nil
}

var listenControl func(network, address string, c syscall.RawConn) error // drugod privzeto (brez SO_REUSEPORT)

// knownConfigDir: domača mapa iz baze uporabnikov (os/user), ne iz HOME/XDG_CONFIG_HOME.
func knownConfigDir() (string, error) {
	u, err := user.Current()
	if err != nil || u.HomeDir == "" {
		return "", errors.New("no home folder")
	}
	if runtime.GOOS == "darwin" {
		return filepath.Join(u.HomeDir, "Library", "Application Support"), nil
	}
	return filepath.Join(u.HomeDir, ".config"), nil
}

// Namestitev s programsko mapo, pladnjem in PATH je le na Windows.
func knownUserProgramsDir() string                 { return "" }
func trayMutexExists() bool                        { return false }
func closeRunningTray() error                      { return nil }
func quitRunningTray()                             {}
func startProcessDetached(string, ...string) error { return errors.New("Windows only") }
func moveFileOnReboot(string) error                { return errors.New("Windows only") }
func regEnvPathGet() (string, error)               { return "", errors.New("Windows only") }
func regEnvPathSet(string) error                   { return errors.New("Windows only") }
