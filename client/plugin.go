package main

// minigw plugin install|uninstall|status in minigw uninstall (0.5.20): nameščeni minigw je edini minigw. Vtičnik za
// Claude Code v ~/.claude/settings.json (pravila prejšnjega postflight v Casks/minigw.rb) in bin/minigw vtičnika kot
// povezava (Windows: lupina) na ta minigw. Enako kot minigw v aplikaciji (Sources/MiniGW/PluginCommand.swift).

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
)

const (
	pluginID     = "minigw@mini-gateway"
	pluginMarket = "mini-gateway"
	// #63: veja release (premika jo le skripta za izdajo) in autoUpdate: Claude Code tretjih marketplace sam ne posodablja
	pluginMarketJSON = `{"source":{"source":"github","repo":"gh-commiter/mini-llm-gateway","ref":"release"},"autoUpdate":true}`
	// vnos do 0.5.21 (brez ref in autoUpdate)
	pluginMarketOldJSON = `{"source":{"source":"github","repo":"gh-commiter/mini-llm-gateway"}}`
)

func claudeDir() string {
	h, _ := os.UserHomeDir()
	return filepath.Join(h, ".claude")
}

func claudeSettingsPath() string { return filepath.Join(claudeDir(), "settings.json") }

// pluginDataDir: CLAUDE_PLUGIN_DATA (v seji Claude Code), sicer mapa, ki jo Claude Code da vtičniku minigw@mini-gateway.
func pluginDataDir() string {
	if d := os.Getenv("CLAUDE_PLUGIN_DATA"); d != "" {
		return d
	}
	return filepath.Join(claudeDir(), "plugins", "data", "minigw-mini-gateway")
}

func pluginMarketplace() any { return jsonValue(pluginMarketJSON) }

func jsonValue(j string) any {
	var v any
	json.Unmarshal([]byte(j), &v)
	return v
}

// ourMarketplace: vnos, kot ga je zapisal ta ali starejši minigw (uporabnikov spremenjeni vnos ni naš).
func ourMarketplace(v any) bool {
	return reflect.DeepEqual(v, pluginMarketplace()) || reflect.DeepEqual(v, jsonValue(pluginMarketOldJSON))
}

// settingsObject: s[k] kot objekt (manjka ali null: nov); drug tip je napaka (ne prepišemo uporabnikove vrednosti).
func settingsObject(s map[string]any, k string) (map[string]any, error) {
	switch v := s[k].(type) {
	case map[string]any:
		return v, nil
	case nil:
		m := map[string]any{}
		s[k] = m
		return m, nil
	}
	return nil, errors.New(k + " is not a JSON object")
}

// pluginAdd: marketplace in enabledPlugins le, če ju še ni: uporabnikov »false« ostane (off). updated: le naš stari
// vnos (brez autoUpdate) je zamenjan z novim; uporabnikov spremenjeni vnos (npr. autoUpdate false) ostane.
func pluginAdd(s map[string]any) (added, updated, off bool, err error) {
	m, err := settingsObject(s, "extraKnownMarketplaces")
	if err != nil {
		return false, false, false, err
	}
	e, err := settingsObject(s, "enabledPlugins")
	if err != nil {
		return false, false, false, err
	}
	switch {
	case m[pluginMarket] == nil:
		m[pluginMarket], added = pluginMarketplace(), true
	case reflect.DeepEqual(m[pluginMarket], jsonValue(pluginMarketOldJSON)):
		m[pluginMarket], updated = pluginMarketplace(), true
	}
	if _, ok := e[pluginID]; !ok {
		e[pluginID], added = true, true
	}
	return added, updated && !added, e[pluginID] != true, nil
}

// pluginRemove: le, kar doda pluginAdd (true, naš marketplace, nov ali do 0.5.21); uporabnikov »false« ostane (off).
func pluginRemove(s map[string]any) (changed, off bool) {
	if e, _ := s["enabledPlugins"].(map[string]any); e != nil {
		if e[pluginID] == true {
			delete(e, pluginID)
			changed = true
		}
		_, off = e[pluginID]
	}
	if m, _ := s["extraKnownMarketplaces"].(map[string]any); m != nil && ourMarketplace(m[pluginMarket]) {
		delete(m, pluginMarket)
		changed = true
	}
	return changed, off
}

// editSettings: prebere settings.json (skozi povezavo, npr. dotfiles), uredi in zapiše atomarno z isto pravico (lahko
// ima ključe v env; nova 0600). Neveljaven JSON: napaka, datoteka ostane.
func editSettings(path string, edit func(map[string]any) (bool, error)) error {
	real := path
	if r, err := filepath.EvalSymlinks(path); err == nil {
		real = r
	}
	mode, s := os.FileMode(0o600), map[string]any{}
	data, err := os.ReadFile(real)
	switch {
	case err == nil:
		var v any
		d := json.NewDecoder(bytes.NewReader(data))
		d.UseNumber() // števila ostanejo, kot so
		if !json.Valid(data) || d.Decode(&v) != nil {
			return errors.New(path + " is not valid JSON: not changed (fix it, or add the plugin in Claude Code: /plugin install " + pluginID + ")")
		}
		var ok bool
		if s, ok = v.(map[string]any); !ok {
			return errors.New(path + " is not a JSON object: not changed")
		}
		if fi, err := os.Stat(real); err == nil {
			mode = fi.Mode().Perm()
		}
	case !errors.Is(err, os.ErrNotExist):
		return err
	}
	changed, err := edit(s)
	if err != nil {
		return errors.New(path + ": " + err.Error() + ": not changed")
	}
	if !changed {
		return nil
	}
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(s); err != nil {
		return err
	}
	if err := writeFileAtomic(real, b.Bytes()); err != nil {
		return err
	}
	return os.Chmod(real, mode)
}

// Windows: skilli v Git Bash kličejo "${CLAUDE_PLUGIN_DATA}/bin/minigw"; MSYS najde datoteko brez pripone ali z .exe,
// ne .cmd. Zato bin/minigw (sh) za Git Bash in bin/minigw.cmd za cmd/PowerShell; oba kličeta nameščeni minigw.exe.
// install-minigw.sh piše enako vsebino.
func shimSh(exe string) string {
	return "#!/bin/sh\n# Mini Gateway: the installed minigw\nexec '" + strings.ReplaceAll(strings.ReplaceAll(exe, `\`, "/"), "'", `'\''`) + "' \"$@\"\n"
}

// shimCmd: brez zakasnjene razširitve (»!« v poti); pot MSI kot %LOCALAPPDATA%\… (kot install-minigw.sh, enako
// besedilo, da ga uninstall prepozna), sicer ne-ASCII pot kot kratka 8.3 (kodna stran cmd).
func shimCmd(exe string) string {
	p := strings.ReplaceAll(exe, "%", "%%")
	if l := strings.TrimRight(os.Getenv("LOCALAPPDATA"), `\`); l != "" && strings.EqualFold(exe, l+`\Programs\Mini Gateway\minigw.exe`) {
		p = `%LOCALAPPDATA%\Programs\Mini Gateway\minigw.exe`
	} else if strings.ContainsFunc(exe, func(r rune) bool { return r > 127 }) {
		p = strings.ReplaceAll(shortPath(exe), "%", "%%")
	}
	return "@setlocal DisableDelayedExpansion\r\n@\"" + p + "\" %*\r\n"
}

func sameFile(a, b string) bool {
	x, err1 := os.Stat(a)
	y, err2 := os.Stat(b)
	return err1 == nil && err2 == nil && os.SameFile(x, y)
}

// selfPath: ta minigw, kot ga je poklical uporabnik ali brew (/opt/homebrew/bin/minigw, ne različica v Caskroom), da
// povezava velja tudi po nadgradnji.
func selfPath() string {
	exe, _ := os.Executable()
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		exe = r
	}
	if p, err := exec.LookPath(os.Args[0]); err == nil && runtime.GOOS != "windows" {
		if abs, err := filepath.Abs(p); err == nil && sameFile(abs, exe) {
			return abs
		}
	}
	return exe
}

// pluginLink: bin/minigw vtičnika → self. "" = self je že minigw vtičnika (prenesen, brez namestitve).
func pluginLink(goos, self string) (string, error) {
	bin := filepath.Join(pluginDataDir(), "bin")
	link := filepath.Join(bin, "minigw")
	if sameFile(filepath.Dir(self), bin) {
		return "", nil
	}
	if goos != "windows" && !safePath(self) { // L3, #48: tuj ali vsem zapisljiv minigw ali katera koli mapa nad njim
		return "", errors.New(self + " or a folder above it is writable by others or not owned by you or root: move minigw to a safe folder")
	}
	if err := os.MkdirAll(bin, 0o755); err != nil {
		return "", err
	}
	if goos == "windows" {
		if err := writeFileAtomic(link, []byte(shimSh(self))); err != nil {
			return "", err
		}
		if err := writeFileAtomic(link+".cmd", []byte(shimCmd(self))); err != nil {
			return "", err
		}
		os.Remove(link + ".exe") // prejšnja kopija vtičnika; v rabi ostane, a bin/minigw ima prednost
		return link, nil
	}
	if t, err := os.Readlink(link); err != nil || t != self {
		os.Remove(link)
		if err := os.Symlink(self, link); err != nil {
			return "", err
		}
	}
	return link, nil
}

// pluginUnlink: odstrani bin/minigw vtičnika, le če kaže na self. true = odstranjen.
func pluginUnlink(goos, self string) bool {
	link := filepath.Join(pluginDataDir(), "bin", "minigw")
	if goos == "windows" {
		b, err := os.ReadFile(link)
		if err != nil || !strings.EqualFold(string(b), shimSh(self)) {
			return false
		}
		if c, err := os.ReadFile(link + ".cmd"); err == nil && strings.EqualFold(string(c), shimCmd(self)) {
			os.Remove(link + ".cmd")
		}
		return os.Remove(link) == nil
	}
	fi, err := os.Lstat(link)
	if err != nil || fi.Mode()&os.ModeSymlink == 0 {
		return false
	}
	if t, _ := os.Readlink(link); t != self && !sameFile(link, self) {
		return false
	}
	return os.Remove(link) == nil
}

// findClaude: claude v PATH ali na običajnih mestih (brew teče z ozkim PATH).
func findClaude() string {
	if p, err := exec.LookPath("claude"); err == nil {
		return p
	}
	h, _ := os.UserHomeDir()
	for _, d := range []string{filepath.Join(h, ".local", "bin"), "/opt/homebrew/bin", "/usr/local/bin"} {
		if p, err := exec.LookPath(filepath.Join(d, "claude")); err == nil {
			return p
		}
	}
	return ""
}

var findClaudeFn = findClaude // testi: brez pravega claude

const offNote = `; turned off there ("` + pluginID + `": false), left as is`

// pluginInstall: false = kaj ni uspelo (izpisano na stderr).
func pluginInstall(goos string) bool {
	ok, path, ch, up, off := true, claudeSettingsPath(), false, false, false
	err := editSettings(path, func(s map[string]any) (c bool, err error) {
		ch, up, off, err = pluginAdd(s)
		return ch || up, err
	})
	switch {
	case err != nil:
		fmt.Fprintln(os.Stderr, "minigw: "+err.Error())
		ok = false
	case ch:
		say("Claude Code plugin " + pluginID + ": added to " + path + map[bool]string{true: offNote, false: "; Claude Code installs it the next time claude starts (or /reload-plugins) and updates it automatically from the release branch (autoUpdate)"}[off])
	case up:
		say("Claude Code plugin " + pluginID + ": already in " + path + "; marketplace " + pluginMarket + " now updates automatically from the release branch (autoUpdate)" + map[bool]string{true: offNote}[off])
	default:
		say("Claude Code plugin " + pluginID + ": already in " + path + map[bool]string{true: offNote}[off])
	}
	self := selfPath()
	switch link, err := pluginLink(goos, self); {
	case err != nil:
		fmt.Fprintln(os.Stderr, "minigw: the plugin's minigw is not linked ("+err.Error()+"); a new Claude Code session sets it up")
		ok = false
	case link == "":
		say("Plugin's minigw: this one (" + self + ")")
	default:
		say("Plugin's minigw: " + link + " → " + self)
	}
	return ok
}

// pluginUninstall: upgrading (brew upgrade, MSI): marketplace in povezava ostaneta, da vtičnik ne izgine med sejo.
func pluginUninstall(goos string, upgrading bool) bool {
	ok := true
	path := claudeSettingsPath()
	had, hadOff, ours := false, false, false // pred »marketplace remove«, ki vnose v settings.json odstrani že sam
	editSettings(path, func(s map[string]any) (bool, error) {
		m, _ := s["extraKnownMarketplaces"].(map[string]any)
		ours = ourMarketplace(m[pluginMarket]) // pred pluginRemove; uporabnikov vnos (fork, ref, autoUpdate false) ostane kot v pluginRemove
		had, hadOff = pluginRemove(s)
		return false, nil
	})
	if c := findClaudeFn(); c != "" && !upgrading && ours {
		if err := runCmd(c, "plugin", "marketplace", "remove", pluginMarket, "--scope", "user"); err != nil {
			say("Claude Code: marketplace " + pluginMarket + " not removed (claude plugin marketplace remove failed)")
		} else {
			say("Claude Code: marketplace " + pluginMarket + " removed")
		}
	}
	if !upgrading && pluginUnlink(goos, selfPath()) {
		say("Plugin's minigw: link removed (" + filepath.Join(pluginDataDir(), "bin", "minigw") + ")")
	}
	if _, err := os.Stat(path); err != nil {
		return ok
	}
	ch, off := false, false
	err := editSettings(path, func(s map[string]any) (bool, error) {
		ch, off = pluginRemove(s)
		return ch, nil
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "minigw: "+err.Error())
		return false
	}
	say("Claude Code plugin " + pluginID + ": " + map[bool]string{true: "removed from ", false: "nothing to remove in "}[ch || had] + path + map[bool]string{true: offNote}[off || hadOff])
	return ok
}

// pluginVersion: različica nameščenega vtičnika (installed_plugins.json → installPath/.claude-plugin/plugin.json).
func pluginVersion() (string, string) {
	var inst struct {
		Plugins map[string][]struct{ InstallPath, Version string }
	}
	data, _ := os.ReadFile(filepath.Join(claudeDir(), "plugins", "installed_plugins.json"))
	if json.Unmarshal(data, &inst) != nil || len(inst.Plugins[pluginID]) == 0 {
		return "", ""
	}
	p := inst.Plugins[pluginID][0]
	var pj struct{ Version string }
	if b, err := os.ReadFile(filepath.Join(p.InstallPath, ".claude-plugin", "plugin.json")); err == nil && json.Unmarshal(b, &pj) == nil && pj.Version != "" {
		p.Version = pj.Version
	}
	return clean(p.Version), p.InstallPath
}

// pluginBinState: kam kaže bin/minigw vtičnika.
func pluginBinState(goos, self string) string {
	bin := filepath.Join(pluginDataDir(), "bin")
	link := filepath.Join(bin, "minigw")
	if goos == "windows" {
		if b, err := os.ReadFile(link); err == nil {
			if strings.EqualFold(string(b), shimSh(self)) {
				return link + " → " + self + " (this minigw)"
			}
			return link + " (another minigw: minigw plugin install)"
		}
		link += ".exe"
	}
	fi, err := os.Lstat(link)
	switch {
	case err != nil:
		return "not set up (minigw plugin install, or a new Claude Code session)"
	case fi.Mode()&os.ModeSymlink != 0:
		t, _ := os.Readlink(link)
		return link + " → " + t + map[bool]string{true: " (this minigw)", false: " (another minigw: minigw plugin install)"}[t == self || sameFile(link, self)]
	case sameFile(link, self):
		return link + " (this minigw)"
	}
	v := installedVersion(link)
	return link + " (a copy: minigw " + map[bool]string{true: "?", false: v}[v == ""] + ")"
}

func pluginStatus(goos string) {
	self := selfPath()
	say("minigw:     " + version + " (" + self + ")")
	cv := "not found (the agent skills need the claude command)"
	if c := findClaudeFn(); c != "" {
		out, _ := command(c, "--version").Output()
		if f := strings.Fields(string(out)); len(f) > 0 && strings.Trim(f[0], "0123456789.") == "" {
			cv = f[0] + " (" + c + ")"
		} else {
			cv = "? (" + c + ")"
		}
	}
	say("Claude Code: " + cv)
	if v, p := pluginVersion(); v != "" {
		say("Plugin:     " + v + " (" + p + ")")
		if versionLess(v, version) { // #63: starejši vtičnik (marketplace brez autoUpdate ali še ne osvežen)
			say("            older than minigw " + version + "; update it, then restart claude:")
			say("              claude plugin marketplace update " + pluginMarket)
			say("              claude plugin update " + pluginID)
		}
	} else {
		say("Plugin:     not installed in Claude Code (minigw plugin install, then start claude)")
	}
	path := claudeSettingsPath()
	state := "not added (minigw plugin install)"
	if data, err := os.ReadFile(path); err == nil {
		var s struct{ EnabledPlugins map[string]any }
		if json.Unmarshal(data, &s) != nil {
			state = "settings.json is not valid JSON"
		} else if v, ok := s.EnabledPlugins[pluginID]; ok {
			state = map[bool]string{true: "on", false: "off (enabledPlugins false)"}[v == true]
		}
	}
	say("Settings:   " + pluginID + " " + state + " (" + path + ")")
	say("Plugin bin: " + pluginBinState(goos, self))
	ps := loadProfiles()
	if len(ps) == 0 {
		say("Gateway:    not connected (minigw connect)")
		return
	}
	c, err := routeErr(ps[0])
	if err != nil {
		say("Gateway:    " + ps[0].conn.URL + " (" + oneLine(err.Error()) + ")")
		return
	}
	st, ready := getJSON(c, "/health/ready")
	if _, ok := ready["providers"]; st != 200 || !ok { // providers le s sprejetim žetonom
		say("Gateway:    " + ps[0].conn.URL + " (not connected: minigw status)")
		return
	}
	v, _ := ready["version"].(string)
	if v == "" || strings.Trim(v, "0123456789.") != "" {
		v = "unknown (older than 0.5.20)"
	}
	say("Gateway:    " + ps[0].conn.URL + ", version " + v)
}

// refuseRoot (L5): kot root bi minigw pisal v korenove ~/.claude in povezave, ki jih nato uporablja uporabnik.
func refuseRoot(cmd string) {
	if runtime.GOOS != "windows" && os.Geteuid() == 0 {
		fail("run minigw " + cmd + " as yourself, not as root")
	}
}

func pluginCommand(args []string) {
	refuseRoot("plugin")
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch {
	case sub == "install" && len(args) == 1:
		if !pluginInstall(runtime.GOOS) {
			os.Exit(1)
		}
	case sub == "uninstall" && (len(args) == 1 || len(args) == 2 && args[1] == "--upgrading"):
		if !pluginUninstall(runtime.GOOS, len(args) == 2) {
			os.Exit(1)
		}
	case sub == "status" && len(args) == 1:
		pluginStatus(runtime.GOOS)
	default:
		fail("usage: minigw plugin install | uninstall [--upgrading] | status")
	}
}

// pluginDataSafe (L1): le <claude>/plugins/data/minigw…, absolutna in počiščena pot.
func pluginDataSafe(d string) bool {
	return filepath.IsAbs(d) && filepath.Clean(d) == d && filepath.Dir(d) == filepath.Join(claudeDir(), "plugins", "data") &&
		strings.HasPrefix(filepath.Base(d), "minigw")
}

// uninstallStep: ena stvar, ki jo minigw uninstall odstrani (seznam najprej, nato potrditev).
type uninstallStep struct {
	what string
	do   func() error
}

// uninstallSteps: kar je minigw dodal na tem računalniku in obstaja; kept = kar ostane brez --purge;
// busy = mapa scratch ostane, ker pladenj (prehod MSI) teče in jo agenti morda uporabljajo (L2).
func uninstallSteps(goos string, purge bool) (steps []uninstallStep, kept []string, busy string) {
	data := pluginDataDir()
	steps = append(steps, uninstallStep{"Claude Code plugin " + pluginID + " (settings entries, marketplace, " + data + ")", func() error {
		ok := pluginUninstall(goos, false)
		if pluginDataSafe(data) { // L1: CLAUDE_PLUGIN_DATA bi lahko kazal drugam
			if err := os.RemoveAll(data); err != nil {
				return err
			}
		}
		if !ok {
			return errors.New("see above")
		}
		return nil
	}})
	trayUp := false // pladenj teče in ga noben korak ne zapre (MSI: CloseTray ga je zaprl že prej)
	switch goos {
	case "windows":
		trayUp = trayRunning()
		if v, _ := runValueGet(runValueName); v != "" && msiDir() == "" { // MSI: vnos Run je njegov
			trayUp = false
			steps = append(steps, uninstallStep{"tray at sign-in (HKCU Run " + runValueName + ")", func() error {
				trayClose()
				runValueDel(runValueName)
				return nil
			}})
		}
	default:
		if p := servicePath(goos); p != "" && errFromStat(p) == nil {
			what := map[bool]string{true: "systemd user unit ", false: "LaunchAgent "}[goos == "linux"] + p
			steps = append(steps, uninstallStep{what, func() error {
				_, err := serviceCommand("uninstall", goos, defaultServeConfigPath())
				return err
			}})
		}
	}
	if cache, err := os.UserCacheDir(); err == nil && errFromStat(filepath.Join(cache, "minigw")) == nil {
		d := filepath.Join(cache, "minigw")
		if trayUp {
			busy = "scratch folder " + d + " (the Mini Gateway tray is running and its agents may use it: quit the tray, then run minigw uninstall again)"
		} else {
			steps = append(steps, uninstallStep{"scratch folder " + d, func() error { return os.RemoveAll(d) }})
		}
	}
	var dirs []string
	for _, f := range []func() (string, error){os.UserConfigDir, configBase} {
		if d, err := f(); err == nil && errFromStat(filepath.Join(d, "minigw")) == nil && !slices.Contains(dirs, filepath.Join(d, "minigw")) {
			dirs = append(dirs, filepath.Join(d, "minigw"))
		}
	}
	d, derr := deviceLoad()
	names, device := profileNames(), d != "" || derr != nil
	if !purge {
		if len(names) > 0 {
			kept = append(kept, "profiles "+strings.Join(names, ", ")+" ("+storeName+")")
		}
		if device {
			kept = append(kept, "device key ("+storeName+")")
		}
		for _, d := range dirs {
			kept = append(kept, "settings folder "+d)
		}
		return steps, kept, busy
	}
	if len(names) > 0 {
		steps = append(steps, uninstallStep{"profiles " + strings.Join(names, ", ") + " (" + storeName + ")", func() error {
			for _, n := range names {
				storeDelete(n)
			}
			return nil
		}})
	}
	if device {
		steps = append(steps, uninstallStep{"device key (" + storeName + ")", func() error { deviceDelete(); return nil }})
	}
	cfg := defaultServeConfigPath()
	if c, err := loadServeConfig(cfg); err == nil { // ključi ponudnikov (Credential Manager, secret-tool)
		steps = append(steps, uninstallStep{"provider keys of minigw serve", func() error {
			for _, g := range c.Gateways {
				keyDel(filepath.Dir(cfg), g.Name)
			}
			keyDel(filepath.Dir(cfg), copilotKey)
			return nil
		}})
	}
	for _, d := range dirs {
		steps = append(steps, uninstallStep{"settings folder " + d, func() error { return os.RemoveAll(d) }})
	}
	return steps, kept, busy
}

// binaryHint: kako odstraniti sam minigw (namestil ga je MSI, brew ali minigw serve install).
func binaryHint(goos string) string {
	self := selfPath()
	switch {
	case goos == "windows" && msiDir() != "":
		return "minigw itself (" + self + "): Settings → Apps → Installed apps → Mini Gateway → Uninstall"
	case goos == "windows":
		return "minigw itself (" + self + "): delete " + self
	}
	return "minigw itself (" + self + "): rm " + self
}

func uninstallCommand(args []string) {
	purge, yes := false, false
	for _, a := range args {
		switch a {
		case "--purge":
			purge = true
		case "--yes", "-y":
			yes = true
		default:
			fail("usage: minigw uninstall [--purge] [--yes]   (--purge also removes settings, profiles and the device key)")
		}
	}
	refuseRoot("uninstall")
	steps, kept, busy := uninstallSteps(runtime.GOOS, purge)
	say("minigw uninstall removes:")
	for _, s := range steps {
		say("  - " + s.what)
	}
	for _, k := range kept {
		say("Kept (--purge removes it): " + k)
	}
	if busy != "" {
		say("Kept: " + busy)
	}
	say("Not removed: " + binaryHint(runtime.GOOS))
	if !yes {
		if !stdinTerminal() {
			fail("stdin is not a terminal: confirm with minigw uninstall --yes")
		}
		fmt.Print("Remove them? [y/N] ")
		line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
		if a := strings.ToLower(strings.TrimSpace(line)); a != "y" && a != "yes" {
			say("Nothing removed")
			return
		}
	}
	failed := false
	for _, s := range steps {
		if err := s.do(); err != nil {
			fmt.Fprintln(os.Stderr, "minigw: "+s.what+": not removed")
			failed = true
		}
	}
	if failed {
		os.Exit(1)
	}
	say("Removed.")
}
