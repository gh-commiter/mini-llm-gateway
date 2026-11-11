package main

// #81 (0.5.25, lastnik D1): Copilot se prijavi le z uporabnikovim »copilot login« na prehodu (kot uporabnik prehoda);
// brez žetona, GitHub CLI (gh auth token) ali PAT. Žeton ostane v sistemski shrambi ključev (Secret Service na Linuxu,
// Keychain na Macu), copilot ga prebere sam. Svež COPILOT_HOME vsake zahteve dobi nov config.json le s kazalcem na
// prijavo (loggedInUsers, lastLoggedInUser iz prave ~/.copilot/config.json; po pravilu, brez drugih ključev). Žeton v
// navadnem besedilu (Linux brez Secret Service: authTokens/copilotTokens v config.json) se ne kopira: Copilot tam ni na
// voljo. Na Linuxu copilot doseže shrambo ključev le prek xdg-dbus-proxy (le org.freedesktop.secrets), ne cele seje
// D-Bus (systemd --user bi zagnal program zunaj bwrap). Ukazi lupine Copilot na Linuxu niso na voljo: Secret Service
// nima pravic po programih, ukaz bi prebral vse ključe odklenjene zbirke.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

const copilotSignIn = "sign in with `copilot login` on the gateway as the gateway user"

// copilotLinuxCommands: zakaj na Linuxu ni ukazov Copilot (nastavitve, opomba seje).
const copilotLinuxCommands = "Copilot commands are not available on a Linux gateway: a command could read the keyring that holds Copilot's sign-in"

var (
	copilotHostRe  = regexp.MustCompile(`^(https://)?[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?(\.[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?)*$`)
	copilotLoginRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,99}$`) // GitHub; _ za Enterprise Managed Users
)

// copilotUser: en LoggedInUser (host, login obvezna; kind, derivedFrom neobvezna niza natisljivega ASCII); drugi
// ključi ali druga oblika = nil.
func copilotUser(raw json.RawMessage) map[string]string {
	var o map[string]any
	if json.Unmarshal(raw, &o) != nil || o == nil {
		return nil
	}
	u := map[string]string{}
	for k, v := range o {
		s, ok := v.(string)
		if !ok {
			return nil
		}
		switch k {
		case "host":
			if len(s) > 253 || !copilotHostRe.MatchString(s) {
				return nil
			}
		case "login":
			if !copilotLoginRe.MatchString(s) {
				return nil
			}
		case "kind", "derivedFrom":
			if s == "" || len(s) > 256 || strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r > 0x7e }) {
				return nil
			}
		default:
			return nil
		}
		u[k] = s
	}
	if u["host"] == "" || u["login"] == "" {
		return nil
	}
	return u
}

// copilotLoginPointer: nov config.json za svež COPILOT_HOME (le loggedInUsers in lastLoggedInUser iz real/config.json)
// ali razlog, zakaj prijave ni mogoče uporabiti. Vrednosti žetona nikoli ne bere v spremenljivko, ne kopira in ne izpiše.
func copilotLoginPointer(real string) ([]byte, string) {
	p := filepath.Join(real, "config.json")
	if fi, err := os.Lstat(p); err != nil || !fi.Mode().IsRegular() {
		return nil, "Copilot is not signed in on the gateway: " + copilotSignIn
	}
	f, err := openMarker(p) // brez sledenja povezavi
	if err != nil {
		return nil, "Copilot's sign-in (" + p + ") cannot be read: " + copilotSignIn
	}
	b, err := io.ReadAll(io.LimitReader(f, 1<<20))
	f.Close()
	if err != nil {
		return nil, "Copilot's sign-in (" + p + ") cannot be read: " + copilotSignIn
	}
	var lines []string // copilot začne datoteko s celimi vrsticami »// …« (niz JSON ne more čez vrstico)
	for _, l := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(l), "//") {
			lines = append(lines, l)
		}
	}
	var top map[string]json.RawMessage
	if json.Unmarshal([]byte(strings.Join(lines, "\n")), &top) != nil {
		return nil, "Copilot's sign-in (" + p + ") is not in the expected form: " + copilotSignIn
	}
	for k, v := range top { // žeton v navadnem besedilu (ni shrambe ključev): ne kopira se
		if s := string(v); strings.Contains(strings.ToLower(k), "token") && s != "true" && s != "false" {
			return nil, "Copilot's sign-in on the gateway is stored as plain text (no system keyring, e.g. a headless Linux), " +
				"which Mini Gateway does not use: Copilot needs the sign-in in the system keyring (Secret Service, Keychain)"
		}
	}
	var list []json.RawMessage
	if json.Unmarshal(top["loggedInUsers"], &list) != nil || len(list) == 0 {
		return nil, "Copilot is not signed in on the gateway: " + copilotSignIn
	}
	out := map[string]any{}
	if len(list) > 32 {
		return nil, "Copilot's sign-in (" + p + ") is not in the expected form: " + copilotSignIn
	}
	users := []map[string]string{}
	for _, raw := range list {
		u := copilotUser(raw)
		if u == nil {
			return nil, "Copilot's sign-in (" + p + ") is not in the expected form: " + copilotSignIn
		}
		users = append(users, u)
	}
	out["loggedInUsers"] = users
	if raw, ok := top["lastLoggedInUser"]; ok && string(raw) != "null" {
		u := copilotUser(raw)
		if u == nil {
			return nil, "Copilot's sign-in (" + p + ") is not in the expected form: " + copilotSignIn
		}
		out["lastLoggedInUser"] = u
	}
	conf, _ := json.Marshal(out)
	return conf, ""
}

// copilotLogin: kazalec na prijavo za svež COPILOT_HOME ali razlog, zakaj Copilot ni na voljo (Windows, prijave ni,
// žeton v navadnem besedilu, na Linuxu brez Secret Service ali xdg-dbus-proxy).
func copilotLogin() ([]byte, string) {
	if copilotUnsupported {
		return nil, copilotUnsupportedMsg
	}
	home, _ := os.UserHomeDir()
	conf, why := copilotLoginPointer(filepath.Join(home, ".copilot"))
	if why == "" && runtime.GOOS == "linux" {
		why = copilotKeyring()
	}
	return conf, why
}

// copilotKeyring (Linux): "" = Secret Service je na seji D-Bus uporabnika (teče ali se zažene ob klicu) in
// xdg-dbus-proxy je nameščen; sicer razlog. Spremenljivka za teste.
var copilotKeyring = func() string {
	if lookPath("xdg-dbus-proxy") == "" {
		return "Copilot needs xdg-dbus-proxy on a Linux gateway (Copilot reaches only the keyring through it, from its sandbox): install the xdg-dbus-proxy package"
	}
	if !secretService(sessionBus()) {
		return "Copilot needs a Secret Service keyring (e.g. GNOME Keyring or KWallet) on the gateway user's D-Bus session; " +
			"a headless Linux gateway is not supported. With a keyring: " + copilotSignIn
	}
	return ""
}

// sessionBus: naslov seje D-Bus uporabnika (DBUS_SESSION_BUS_ADDRESS ali $XDG_RUNTIME_DIR/bus) ali "".
func sessionBus() string {
	if a := os.Getenv("DBUS_SESSION_BUS_ADDRESS"); a != "" {
		return a
	}
	if rt := os.Getenv("XDG_RUNTIME_DIR"); rt != "" {
		if fi, err := os.Stat(filepath.Join(rt, "bus")); err == nil && fi.Mode()&os.ModeSocket != 0 {
			return "unix:path=" + dbusEscape(filepath.Join(rt, "bus"))
		}
	}
	return ""
}

// secretService: org.freedesktop.secrets je med imeni ali imeni za zagon na seji (busctl, sicer dbus-send; 5 s).
func secretService(bus string) bool {
	if bus == "" {
		return false
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, m := range []string{"ListNames", "ListActivatableNames"} {
		var args []string
		exe := lookPath("busctl")
		if exe != "" {
			args = []string{"--user", "call", "org.freedesktop.DBus", "/org/freedesktop/DBus", "org.freedesktop.DBus", m}
		} else if exe = lookPath("dbus-send"); exe != "" {
			args = []string{"--session", "--print-reply", "--dest=org.freedesktop.DBus", "/org/freedesktop/DBus", "org.freedesktop.DBus." + m}
		} else {
			return false
		}
		cmd := commandContext(ctx, exe, args...)
		cmd.Env = append(os.Environ(), "DBUS_SESSION_BUS_ADDRESS="+bus)
		if out, err := cmd.Output(); err == nil && secretServiceListed(string(out)) {
			return true
		}
	}
	return false
}

// secretServiceListed: izpis busctl (as N "a" "b") ali dbus-send (string "a") vsebuje točno ime org.freedesktop.secrets.
func secretServiceListed(out string) bool { return strings.Contains(out, `"org.freedesktop.secrets"`) }

// dbusEscape: vrednost v naslovu D-Bus (le [-0-9A-Za-z_/.\*] dobesedno, drugo %xx).
func dbusEscape(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || strings.IndexByte("-_/.\\*", c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02x", c)
		}
	}
	return b.String()
}

// copilotBusProxy (Linux): xdg-dbus-proxy na vtičnici dir/bus, ki pusti le org.freedesktop.secrets (in Hello). Vrne
// naslov za DBUS_SESSION_BUS_ADDRESS copilota in stop. Spremenljivka za teste.
var copilotBusProxy = func(dir string) (string, func(), error) {
	sock := filepath.Join(dir, "bus")
	cmd := command(lookPath("xdg-dbus-proxy"), sessionBus(), sock, "--filter", "--talk=org.freedesktop.secrets")
	if err := cmd.Start(); err != nil {
		return "", nil, errors.New("copilot: cannot start xdg-dbus-proxy: " + err.Error())
	}
	stop := func() { cmd.Process.Kill(); cmd.Wait() }
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if fi, err := os.Stat(sock); err == nil && fi.Mode()&os.ModeSocket != 0 {
			return "unix:path=" + dbusEscape(sock), stop, nil
		}
	}
	stop()
	return "", nil, errors.New("copilot: xdg-dbus-proxy did not start (the keyring is not reachable)")
}
