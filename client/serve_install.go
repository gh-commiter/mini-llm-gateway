package main

// minigw serve install|uninstall: router ob prijavi, za tega uporabnika in brez skrbniških pravic.
// Windows: vrednost HKCU …\Run (faza 3b: minigw-tray.exe); Linux: uporabniška enota systemd; macOS: LaunchAgent.

import (
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// systemdQuote: argument v ExecStart (narekovaji, \ in % ubežani).
func systemdQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%").Replace(s) + `"`
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
	case "windows":
		return `"` + exe + `" serve --config "` + cfgPath + `"` // poti na Windows ne vsebujejo »"«
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

func serviceCommand(sub, goos, cfgPath string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	cfgPath, _ = filepath.Abs(cfgPath)
	if _, err := os.Stat(cfgPath); sub == "install" && err != nil {
		return "", errors.New("no " + cfgPath + ": run minigw serve init first")
	}
	uid := fmt.Sprintf("gui/%d", os.Getuid())
	p := servicePath(goos)
	switch goos + " " + sub {
	case "windows install":
		if err := runValueSet(runValueName, serviceFile(goos, exe, cfgPath)); err != nil {
			return "", err
		}
		return "minigw serve starts at sign-in (HKCU Run " + runValueName + "); to start it now run: minigw serve", nil
	case "windows uninstall":
		runValueDel(runValueName)
		return "minigw serve no longer starts at sign-in (the config is kept)", nil
	case "linux install", "darwin install":
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
