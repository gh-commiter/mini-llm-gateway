//go:build !windows

package main

// Povezava v datoteki uporabnika z dovoljenji 0600 (Linux, in macOS pri preizkusu; macOS minigw v aplikaciji
// uporablja Keychain). ponytail: na Linuxu brez Secret Service; dodaj, če bo potreben namizni keyring.

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

var errNotFound = errors.New("not found")

func storePath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = os.TempDir()
	}
	return filepath.Join(dir, "minigw", "connection.json")
}

func storeLoad() ([]byte, error) {
	data, err := os.ReadFile(storePath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, errNotFound
	}
	return data, err
}

func storeSave(data []byte) error {
	if err := os.MkdirAll(filepath.Dir(storePath()), 0o700); err != nil {
		return err
	}
	return os.WriteFile(storePath(), data, 0o600)
}

func storeDelete() { os.Remove(storePath()) }

const storeName = "a private file"

// Ključi prehodov (minigw serve gateway). macOS: Keychain prek ukaza security (Go minigw serve na Macu je le za
// preizkus; aplikacija ima svoj Keychain). Linux: secret-tool (libsecret), sicer gateway-keys.json (0600) ob serve.json.

func keyFile(cfgDir string) string { return filepath.Join(cfgDir, "gateway-keys.json") }

func fileKeys(cfgDir string) map[string]string {
	m := map[string]string{}
	data, _ := os.ReadFile(keyFile(cfgDir))
	json.Unmarshal(data, &m)
	return m
}

func osKeyGet(cfgDir, name string) (string, string) {
	if runtime.GOOS == "darwin" {
		out, _ := exec.Command("security", "find-generic-password", "-s", "minigw", "-a", "gateway/"+name, "-w").Output()
		return strings.TrimSpace(string(out)), "the Keychain"
	}
	if tool, _ := exec.LookPath("secret-tool"); tool != "" {
		if out, _ := exec.Command(tool, "lookup", "service", "minigw", "gateway", name).Output(); len(out) > 0 {
			return strings.TrimSpace(string(out)), "the Secret Service"
		}
	}
	return fileKeys(cfgDir)[name], keyFile(cfgDir)
}

// osKeySet: ključ nikoli v argv (security -i in secret-tool ga bereta s stdin).
func osKeySet(cfgDir, name, key string) (string, error) {
	if runtime.GOOS == "darwin" {
		if strings.ContainsAny(key, `"\`) {
			return "", errors.New("the key must not contain \" or \\")
		}
		cmd := exec.Command("security", "-i")
		cmd.Stdin = strings.NewReader(`add-generic-password -U -s minigw -a "gateway/` + name + `" -w "` + key + "\"\n")
		if out, err := cmd.CombinedOutput(); err != nil || len(out) > 0 {
			return "", errors.New("security: " + strings.TrimSpace(string(out)))
		}
		return "the Keychain", nil
	}
	if tool, _ := exec.LookPath("secret-tool"); tool != "" {
		cmd := exec.Command(tool, "store", "--label=minigw gateway "+name, "service", "minigw", "gateway", name)
		cmd.Stdin = strings.NewReader(key)
		if cmd.Run() == nil {
			m := fileKeys(cfgDir) // starejša kopija v datoteki ne sme ostati
			if _, ok := m[name]; ok {
				delete(m, name)
				writeKeys(cfgDir, m)
			}
			return "the Secret Service", nil
		}
	}
	m := fileKeys(cfgDir)
	m[name] = key
	return "WARNING: no Secret Service (secret-tool), so it is in " + keyFile(cfgDir) + " (mode 0600)", writeKeys(cfgDir, m)
}

func writeKeys(cfgDir string, m map[string]string) error {
	data, _ := json.MarshalIndent(m, "", "  ")
	os.MkdirAll(cfgDir, 0o700)
	if err := os.WriteFile(keyFile(cfgDir), data, 0o600); err != nil {
		return err
	}
	return os.Chmod(keyFile(cfgDir), 0o600)
}

func osKeyDel(cfgDir, name string) {
	if runtime.GOOS == "darwin" {
		exec.Command("security", "delete-generic-password", "-s", "minigw", "-a", "gateway/"+name).Run()
		return
	}
	if tool, _ := exec.LookPath("secret-tool"); tool != "" {
		exec.Command(tool, "clear", "service", "minigw", "gateway", name).Run()
	}
	if m := fileKeys(cfgDir); m[name] != "" {
		delete(m, name)
		writeKeys(cfgDir, m)
	}
}

// readHidden: vrstica s terminala brez odmeva (stty).
func readHidden(f *os.File) (string, error) {
	off := exec.Command("stty", "-echo")
	off.Stdin = f
	if off.Run() == nil {
		defer func() {
			on := exec.Command("stty", "echo")
			on.Stdin = f
			on.Run()
		}()
	}
	return bufio.NewReader(f).ReadString('\n')
}
