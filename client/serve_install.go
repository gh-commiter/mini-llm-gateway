package main

// minigw serve install|uninstall: router ob prijavi, za tega uporabnika in brez skrbniških pravic.
// Windows: vrednost HKCU …\Run (faza 3b: minigw-tray.exe); Linux: uporabniška enota systemd; macOS: LaunchAgent.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	runValueName = "MiniGatewayRouter"
	systemdUnit  = "minigw-serve.service"
	launchdLabel = "dev.trojar.minigw-serve"
)

// Zunanji ukazi in register: testi jih zamenjajo.
var (
	runCmd = func(name string, args ...string) error {
		out, err := exec.Command(name, args...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("%s %s: %v %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	runValueSet, runValueGet, runValueDel = regRunSet, regRunGet, regRunDel
)

func servicePath(goos string) string {
	home, _ := os.UserHomeDir()
	switch goos {
	case "linux":
		dir, _ := os.UserConfigDir()
		return filepath.Join(dir, "systemd", "user", systemdUnit)
	case "darwin":
		return filepath.Join(home, "Library", "LaunchAgents", launchdLabel+".plist")
	}
	return ""
}

// systemdQuote: argument v ExecStart (narekovaji, \, % in $ ubežani; novih vrstic serviceCommand ne dovoli).
func systemdQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%", "$", "$$").Replace(s) + `"`
}

func xmlText(s string) string {
	var b strings.Builder
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

// serviceFile: vsebina enote, LaunchAgenta ali ukazne vrstice za Run.
func serviceFile(goos, exe, cfgPath string) string {
	switch goos {
	case "linux":
		return "[Unit]\nDescription=Mini Gateway router (minigw serve)\nAfter=network-online.target\n\n[Service]\nExecStart=" +
			systemdQuote(exe) + " serve --config " + systemdQuote(cfgPath) + "\nRestart=on-failure\n\n[Install]\nWantedBy=default.target\n"
	case "darwin":
		home, _ := os.UserHomeDir()
		return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key><string>` + launchdLabel + `</string>
	<key>ProgramArguments</key>
	<array><string>` + xmlText(exe) + `</string><string>serve</string><string>--config</string><string>` + xmlText(cfgPath) + `</string></array>
	<key>RunAtLoad</key><true/>
	<key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
	<key>StandardErrorPath</key><string>` + xmlText(filepath.Join(home, "Library", "Logs", "minigw-serve.log")) + `</string>
</dict>
</plist>
`
	case "windows": // pladenj, če je ob exe; poti na Windows ne vsebujejo »"«
		if tray := trayBeside(exe); tray != "" {
			return `"` + tray + `" tray --config "` + cfgPath + `"`
		}
		return `"` + exe + `" serve --config "` + cfgPath + `"`
	}
	return ""
}

func serviceInstalled(goos string) bool {
	if goos == "windows" {
		v, _ := runValueGet(runValueName)
		return v != ""
	}
	if p := servicePath(goos); p != "" {
		_, err := os.Stat(p)
		return err == nil
	}
	return false
}

// Namestitev: Windows %LocalAppData%\Programs\Mini Gateway (minigw.exe, minigw-tray.exe, PATH, Run, zagon pladnja);
// Linux in macOS ~/.local/bin/minigw. Klici sistema so spremenljivke, da jih testi zamenjajo.
var (
	userProgramsDir = knownUserProgramsDir // Windows: FOLDERID_UserProgramFiles
	trayRunning     = trayMutexExists
	trayClose       = closeRunningTray // WM_CLOSE in čakanje, da mutex izgine (tudi posodobitev)
	startDetached   = startProcessDetached
	envPathGet      = regEnvPathGet
	envPathSet      = regEnvPathSet // HKCU\Environment Path (REG_EXPAND_SZ) in WM_SETTINGCHANGE
	moveOnReboot    = moveFileOnReboot
	userHomeFn      = userHome // testi: začasna mapa namesto prave ~/.local/bin
)

// installDir: kam namestitev kopira minigw.
func installDir(goos string) string {
	if goos == "windows" {
		if d := userProgramsDir(); d != "" {
			return filepath.Join(d, "Mini Gateway")
		}
		return ""
	}
	return filepath.Join(userHomeFn(), ".local", "bin")
}

func userHome() string {
	if u, err := user.Current(); err == nil && u.HomeDir != "" {
		return u.HomeDir
	}
	h, _ := os.UserHomeDir()
	return h
}

func fileSHA256(p string) (string, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	return hex.EncodeToString(h.Sum(nil)), n, err
}

// installFile: začasno ime v cilju, preimenovanje, nato preverjanje velikosti in sha256 (want "" = iz vira).
func installFile(src, dst string) error {
	if a, err := os.Stat(src); err == nil {
		if b, err := os.Stat(dst); err == nil && os.SameFile(a, b) {
			return nil // že teče od tam
		}
	}
	want, size, err := fileSHA256(src)
	if err != nil {
		return err
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return installData(data, dst, want, size)
}

func installData(data []byte, dst, want string, size int64) error {
	if err := writeFileAtomic(dst, data); err != nil {
		return err
	}
	os.Chmod(dst, 0o755)
	got, n, err := fileSHA256(dst)
	if err != nil || got != want || n != size {
		os.Remove(dst)
		return fmt.Errorf("%s: copy does not match (sha256 or size)", dst)
	}
	return nil
}

// pathWith: PATH z mapo dir (le če je še ni; primerjava brez razlike v velikih črkah in končni »\«).
func pathWith(path, dir string) (string, bool) {
	for _, e := range strings.Split(path, ";") {
		if strings.EqualFold(strings.TrimRight(e, `\`), strings.TrimRight(dir, `\`)) {
			return path, false
		}
	}
	if path == "" {
		return dir, true
	}
	return strings.TrimRight(path, ";") + ";" + dir, true
}

// pathWithout: PATH brez naše mape (drugi vnosi ostanejo, kot so).
func pathWithout(path, dir string) string {
	var keep []string
	for _, e := range strings.Split(path, ";") {
		if e != "" && !strings.EqualFold(strings.TrimRight(e, `\`), strings.TrimRight(dir, `\`)) {
			keep = append(keep, e)
		}
	}
	return strings.Join(keep, ";")
}

// Izdaja na GitHubu: le https in le gostitelji izdaj GitHuba (preusmeritev na CDN), kot install-minigw.sh vtičnika.
var (
	releaseBase  = "https://github.com/gh-commiter/mini-llm-gateway/releases/download/v" + version
	releaseHosts = map[string]bool{"github.com": true, "objects.githubusercontent.com": true, "release-assets.githubusercontent.com": true}
	releaseHTTP  = &http.Client{Timeout: 5 * time.Minute, CheckRedirect: releaseRedirect}
)

func releaseRedirect(r *http.Request, via []*http.Request) error {
	if r.URL.Scheme != "https" || !releaseHosts[r.URL.Hostname()] || len(via) > 5 {
		return fmt.Errorf("refused redirect to %s", r.URL.Redacted())
	}
	return nil
}

func releaseGet(name string, limit int64) ([]byte, error) {
	u := releaseBase + "/" + name
	if pu, err := url.Parse(u); err != nil || pu.Scheme != "https" || !releaseHosts[pu.Hostname()] {
		return nil, errors.New("release URL must be https on GitHub: " + u)
	}
	resp, err := releaseHTTP.Get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("%s: HTTP %d", u, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

// downloadVerified: sredstvo te izdaje, preverjeno po SHA256SUMS iste izdaje.
func downloadVerified(asset, dst string) error {
	sums, err := releaseGet("SHA256SUMS", 64<<10)
	if err != nil {
		return err
	}
	want := ""
	for _, line := range strings.Split(string(sums), "\n") {
		if f := strings.Fields(line); len(f) == 2 && strings.TrimPrefix(f[1], "*") == asset {
			want = strings.ToLower(f[0])
		}
	}
	if want == "" {
		return errors.New(asset + " is not in SHA256SUMS of v" + version)
	}
	data, err := releaseGet(asset, 128<<20)
	if err != nil {
		return err
	}
	if sum := sha256.Sum256(data); hex.EncodeToString(sum[:]) != want {
		return errors.New(asset + ": sha256 does not match SHA256SUMS")
	}
	return installData(data, dst, want, int64(len(data)))
}

// installWindows: kopija v programsko mapo, pladenj, PATH, Run in zagon pladnja; brez skrbniških pravic.
func installWindows(exe, cfgPath string) (string, error) {
	if err := protectConfigDir(filepath.Dir(cfgPath)); errors.Is(err, errForeignOwner) {
		return "", errors.New(filepath.Dir(cfgPath) + " " + err.Error() + ": use a config folder of your own")
	}
	dir := installDir("windows")
	if dir == "" {
		return "", errors.New("cannot find the user programs folder (LocalAppData\\Programs)")
	}
	if err := trayClose(); err != nil {
		return "", err
	}
	if err := installFile(exe, filepath.Join(dir, "minigw.exe")); err != nil {
		return "", err
	}
	tray, note := filepath.Join(dir, "minigw-tray.exe"), ""
	if src := trayBeside(exe); src != "" {
		if err := installFile(src, tray); err != nil {
			return "", err
		}
	} else if err := downloadVerified("minigw-tray-windows-"+runtime.GOARCH+".exe", tray); err != nil {
		tray, note = "", "\nNo tray icon: minigw-tray.exe was not next to minigw.exe and the download failed ("+err.Error()+"); minigw serve starts at sign-in instead."
	}
	path, _ := envPathGet()
	if np, add := pathWith(path, dir); add {
		if err := envPathSet(np); err != nil {
			return "", err
		}
	}
	run := `"` + filepath.Join(dir, "minigw.exe") + `" serve --config "` + cfgPath + `"`
	if tray != "" {
		run = `"` + tray + `" tray --config "` + cfgPath + `"`
	}
	if err := runValueSet(runValueName, run); err != nil {
		return "", err
	}
	what := "minigw serve runs at sign-in"
	if tray != "" {
		if err := startDetached(tray, "tray", "--config", cfgPath); err != nil {
			return "", err
		}
		what = "Mini Gateway is in the notification area"
	}
	return "installed to " + dir + "; " + what + "; open a new terminal for minigw on PATH" + note, nil
}

func uninstallWindows(cfgPath string) (string, error) {
	dir := installDir("windows")
	trayClose()
	runValueDel(runValueName)
	if path, err := envPathGet(); err == nil && dir != "" {
		if np := pathWithout(path, dir); np != path {
			envPathSet(np)
		}
	}
	var later []string
	for _, f := range []string{"minigw.exe", "minigw-tray.exe", ""} {
		p := filepath.Join(dir, f)
		if dir == "" || os.Remove(p) == nil || errors.Is(errFromStat(p), os.ErrNotExist) {
			continue
		}
		if moveOnReboot(p) == nil {
			later = append(later, p)
		} else {
			later = append(later, p+" (delete it yourself)")
		}
	}
	msg := "Mini Gateway removed from " + dir + ", sign-in and PATH; the config and keys stay in " + filepath.Dir(cfgPath) +
		" and Credential Manager"
	if len(later) > 0 {
		msg += "\nIn use, removed after the next restart: " + strings.Join(later, ", ")
	}
	return msg, nil
}

func errFromStat(p string) error { _, err := os.Stat(p); return err }

// installedInfo: mapa namestitve, različica tam in ali teče pladenj (za status).
func installedInfo(goos string) string {
	dir := installDir(goos)
	exe := filepath.Join(dir, map[bool]string{true: "minigw.exe", false: "minigw"}[goos == "windows"])
	out, err := exec.Command(exe, "--version").Output()
	if dir == "" || err != nil {
		return "not installed in " + dir + " (minigw serve install)"
	}
	s := "installed in " + dir + ": " + strings.TrimSpace(string(out))
	if goos == "windows" {
		s += map[bool]string{true: ", tray running", false: ", tray not running"}[trayRunning()]
	}
	return s
}

func serviceCommand(sub, goos, cfgPath string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	cfgPath, _ = filepath.Abs(cfgPath)
	if strings.ContainsAny(exe+cfgPath, "\r\n\x00") || (goos == "windows" && strings.Contains(exe+cfgPath, `"`)) {
		return "", errors.New("the program or config path contains a line break or quote: move it to a plain path")
	}
	if _, err := os.Stat(cfgPath); sub == "install" && err != nil {
		return "", errors.New("no " + cfgPath + ": run minigw serve init first")
	}
	uid := fmt.Sprintf("gui/%d", os.Getuid())
	p := servicePath(goos)
	switch goos + " " + sub {
	case "windows install":
		return installWindows(exe, cfgPath)
	case "windows uninstall":
		return uninstallWindows(cfgPath)
	case "linux install", "darwin install":
		target := filepath.Join(installDir(goos), "minigw")
		if err := installFile(exe, target); err != nil {
			return "", err
		}
		exe = target
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(p, []byte(serviceFile(goos, exe, cfgPath)), 0o644); err != nil {
			return "", err
		}
		if goos == "linux" {
			if err := runCmd("systemctl", "--user", "daemon-reload"); err != nil {
				return "", err
			}
			if err := runCmd("systemctl", "--user", "enable", "--now", systemdUnit); err != nil {
				return "", err
			}
			return "minigw serve runs now and at sign-in (" + p + "); log: journalctl --user -u " + systemdUnit +
				"\nTo keep it running after you sign out: loginctl enable-linger " + os.Getenv("USER"), nil
		}
		runCmd("launchctl", "bootout", uid, p) // že naložen: znova
		if err := runCmd("launchctl", "bootstrap", uid, p); err != nil {
			return "", err
		}
		return "minigw serve runs now and at sign-in (" + p + ")", nil
	case "linux uninstall":
		runCmd("systemctl", "--user", "disable", "--now", systemdUnit)
		os.Remove(p)
		runCmd("systemctl", "--user", "daemon-reload")
		return "minigw serve stopped and no longer starts at sign-in (the config is kept)", nil
	case "darwin uninstall":
		runCmd("launchctl", "bootout", uid, p)
		os.Remove(p)
		return "minigw serve stopped and no longer starts at sign-in (the config is kept)", nil
	}
	return "", errors.New("minigw serve install is not supported on " + goos)
}
