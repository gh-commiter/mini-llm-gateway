package main

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// linkOS: install/uninstall kot na tem sistemu, a Windows z lupinama (bin/minigw, .cmd): simbolna povezava (linux) na
// Windows rabi Developer Mode ali skrbnika; pravi minigw na Windows je tudi ne dela.
var linkOS = map[bool]string{true: "windows", false: "linux"}[runtime.GOOS == "windows"]

// pluginHome: začasna domača mapa (nikoli prava ~/.claude) in brez pravega claude.
func pluginHome(t *testing.T) (home string, calls *[]string) {
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	t.Setenv("APPDATA", filepath.Join(home, "AppData", "Roaming")) // Windows: os.UserConfigDir, os.UserCacheDir
	t.Setenv("LOCALAPPDATA", filepath.Join(home, "AppData", "Local"))
	t.Setenv("CLAUDE_PLUGIN_DATA", "")
	var c []string
	f, r := findClaudeFn, runCmd
	findClaudeFn = func() string { return "/fake/claude" }
	runCmd = func(name string, args ...string) error { c = append(c, name+" "+strings.Join(args, " ")); return nil }
	t.Cleanup(func() { findClaudeFn, runCmd = f, r })
	return home, &c
}

// A skill's allowed-tools pattern never starts with an environment assignment or has `=*` before the minigw path: that would
// pre-approve the environment prefix (MINIGW_WORK=direct ...) that makes Claude Code ask the user. Labels go as flags.
func TestSkillsAllowedToolsHaveNoEnvironmentPrefix(t *testing.T) {
	dir := filepath.Join("..", "plugins", "minigw", "skills")
	if _, err := os.Stat(dir); err != nil {
		t.Skip("the plugin folder is not here (the Linux Docker gate mounts only client/)")
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*", "SKILL.md"))
	if len(files) == 0 {
		t.Fatal("no skills found")
	}
	env := regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
	for _, f := range files {
		b, _ := os.ReadFile(f)
		for _, line := range strings.Split(string(b), "\n") {
			rest, ok := strings.CutPrefix(line, "allowed-tools:")
			if !ok {
				continue
			}
			for _, p := range strings.Split(rest, "Bash(")[1:] {
				pat := strings.TrimSuffix(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(p), ",")), ")")
				if env.MatchString(pat) || strings.Contains(pat[:max(0, strings.Index(pat, "minigw"))], "=*") {
					t.Errorf("%s: allowed-tools pattern %q starts with an environment assignment", f, pat)
				}
			}
		}
	}
}

func TestPluginSettingsMergeKeepsUserFalseAndMode(t *testing.T) {
	home, _ := pluginHome(t)
	p := filepath.Join(home, ".claude", "settings.json")
	os.MkdirAll(filepath.Dir(p), 0o700)
	os.WriteFile(p, []byte(`{"model":"opus","n":12345678901234567890,"url":"a<b>&c","enabledPlugins":{"minigw@mini-gateway":false}}`), 0o640)
	if !pluginInstall(linkOS) {
		t.Fatal("install failed")
	}
	b, _ := os.ReadFile(p)
	s := string(b)
	for _, w := range []string{`"minigw@mini-gateway": false`, `"repo": "gh-commiter/mini-llm-gateway"`, `12345678901234567890`, `"a<b>&c"`, `"model": "opus"`} {
		if !strings.Contains(s, w) {
			t.Fatalf("missing %s in\n%s", w, s)
		}
	}
	if fi, _ := os.Stat(p); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o640 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
	if !pluginUninstall(linkOS, true) {
		t.Fatal("uninstall failed")
	}
	b, _ = os.ReadFile(p)
	if !strings.Contains(string(b), `"minigw@mini-gateway": false`) || strings.Contains(string(b), "mini-llm-gateway") {
		t.Fatalf("after uninstall:\n%s", b)
	}
}

func TestPluginSettingsNewFileAndRemove(t *testing.T) {
	home, calls := pluginHome(t)
	p := filepath.Join(home, ".claude", "settings.json")
	if !pluginInstall(linkOS) {
		t.Fatal("install failed")
	}
	b, _ := os.ReadFile(p)
	if !strings.Contains(string(b), `"minigw@mini-gateway": true`) {
		t.Fatalf("not enabled:\n%s", b)
	}
	if fi, _ := os.Stat(p); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("new file mode %v", fi.Mode().Perm())
	}
	// Uporabnikov lasten marketplace z istim imenom ostane.
	os.WriteFile(p, []byte(`{"enabledPlugins":{"minigw@mini-gateway":true},"extraKnownMarketplaces":{"mini-gateway":{"source":{"source":"directory","path":"/x"}}}}`), 0o600)
	pluginUninstall(linkOS, false)
	b, _ = os.ReadFile(p)
	if strings.Contains(string(b), "minigw@mini-gateway") || !strings.Contains(string(b), `"/x"`) {
		t.Fatalf("after uninstall:\n%s", b)
	}
	if len(*calls) != 0 { // uporabnikov vnos: tudi CLI remove ne teče
		t.Fatalf("user's own marketplace: calls %v", *calls)
	}
	pluginInstall(linkOS)
	os.Remove(p)
	pluginInstall(linkOS)
	pluginUninstall(linkOS, false)
	if !slices.Contains(*calls, "/fake/claude plugin marketplace remove mini-gateway --scope user") {
		t.Fatalf("our marketplace: calls %v", *calls)
	}
	*calls = nil
	pluginUninstall(linkOS, true)
	if len(*calls) != 0 {
		t.Fatalf("upgrading ran %v", *calls)
	}
}

func TestPluginSettingsInvalidRefused(t *testing.T) {
	home, _ := pluginHome(t)
	p := filepath.Join(home, ".claude", "settings.json")
	os.MkdirAll(filepath.Dir(p), 0o700)
	// L7: ne-objekt v enabledPlugins ali extraKnownMarketplaces: install zavrne, uninstall nima česa odstraniti
	nonObject := map[string]bool{`{"enabledPlugins":[]}`: true, `{"enabledPlugins":1}`: true, `{"extraKnownMarketplaces":"x"}`: true,
		`{"extraKnownMarketplaces":[1],"enabledPlugins":{}}`: true}
	for _, bad := range []string{`{"a":1,}`, `[]`, `null`, ``, `{"a":1} x`, `{"enabledPlugins":[]}`, `{"enabledPlugins":1}`,
		`{"extraKnownMarketplaces":"x"}`, `{"extraKnownMarketplaces":[1],"enabledPlugins":{}}`} {
		os.WriteFile(p, []byte(bad), 0o600)
		if pluginInstall(linkOS) {
			t.Fatalf("%q accepted", bad)
		}
		if pluginUninstall(linkOS, true) && !nonObject[bad] {
			t.Fatalf("%q accepted by uninstall", bad)
		}
		if b, _ := os.ReadFile(p); string(b) != bad {
			t.Fatalf("%q changed to %q", bad, b)
		}
	}
}

func TestPluginSettingsThroughSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink")
	}
	home, _ := pluginHome(t)
	real := filepath.Join(home, "dotfiles", "settings.json")
	os.MkdirAll(filepath.Dir(real), 0o700)
	os.WriteFile(real, []byte("{}"), 0o644)
	p := filepath.Join(home, ".claude", "settings.json")
	os.MkdirAll(filepath.Dir(p), 0o700)
	os.Symlink(real, p)
	pluginInstall(linkOS)
	if fi, _ := os.Lstat(p); fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("link replaced")
	}
	if b, _ := os.ReadFile(real); !strings.Contains(string(b), pluginID) {
		t.Fatalf("not written through the link: %s", b)
	}
}

func TestPluginShimContent(t *testing.T) {
	exe := `C:\Users\O'Brien\AppData\Local\Programs\Mini Gateway 100%\minigw.exe`
	if got, want := shimSh(exe), "#!/bin/sh\n# Mini Gateway: the installed minigw\nexec 'C:/Users/O'\\''Brien/AppData/Local/Programs/Mini Gateway 100%/minigw.exe' \"$@\"\n"; got != want {
		t.Fatalf("sh shim\n%q\n%q", got, want)
	}
	t.Setenv("LOCALAPPDATA", `C:\Users\O'Brien\AppData\Local`)
	if got, want := shimCmd(exe), "@setlocal DisableDelayedExpansion\r\n@\"C:\\Users\\O'Brien\\AppData\\Local\\Programs\\Mini Gateway 100%%\\minigw.exe\" %*\r\n"; got != want {
		t.Fatalf("cmd shim\n%q\n%q", got, want)
	}
	// MSI: kot install-minigw.sh, dobesedno %LOCALAPPDATA%
	msi := `c:\users\o'brien\appdata\local\Programs\Mini Gateway\minigw.exe`
	if got, want := shimCmd(msi), "@setlocal DisableDelayedExpansion\r\n@\"%LOCALAPPDATA%\\Programs\\Mini Gateway\\minigw.exe\" %*\r\n"; got != want {
		t.Fatalf("msi cmd shim\n%q\n%q", got, want)
	}
}

func TestPluginLinkOnlyOurs(t *testing.T) {
	home, _ := pluginHome(t)
	bin := filepath.Join(home, ".claude", "plugins", "data", "minigw-mini-gateway", "bin")
	self := filepath.Join(home, "inst", "minigw")
	os.MkdirAll(filepath.Dir(self), 0o755)
	os.WriteFile(self, []byte("x"), 0o755)

	// Windows: lupini, kopija vtičnika odstranjena; tuja lupina ostane.
	os.MkdirAll(bin, 0o755)
	os.WriteFile(filepath.Join(bin, "minigw.exe"), []byte("old copy"), 0o755)
	if l, err := pluginLink("windows", self); err != nil || l != filepath.Join(bin, "minigw") {
		t.Fatalf("link %q %v", l, err)
	}
	if b, _ := os.ReadFile(filepath.Join(bin, "minigw.cmd")); string(b) != shimCmd(self) || errFromStat(filepath.Join(bin, "minigw.exe")) == nil {
		t.Fatalf("cmd %q or copy kept", b)
	}
	if pluginUnlink("windows", filepath.Join(home, "other", "minigw.exe")) || !pluginUnlink("windows", self) || errFromStat(filepath.Join(bin, "minigw.cmd")) == nil {
		t.Fatal("windows unlink")
	}
	if runtime.GOOS == "windows" {
		return
	}
	// macOS/Linux: povezava; tuja povezava ostane.
	if _, err := pluginLink("linux", self); err != nil {
		t.Fatal(err)
	}
	if tgt, _ := os.Readlink(filepath.Join(bin, "minigw")); tgt != self {
		t.Fatalf("link → %q", tgt)
	}
	if !strings.Contains(pluginBinState("linux", self), "(this minigw)") {
		t.Fatal(pluginBinState("linux", self))
	}
	other := filepath.Join(home, "other")
	os.WriteFile(other, []byte("y"), 0o755)
	if pluginUnlink("linux", other) || !pluginUnlink("linux", self) {
		t.Fatal("unix unlink")
	}
	// minigw, ki ga je prenesel vtičnik (v bin): ostane, brez povezave nase.
	own := filepath.Join(bin, "minigw")
	os.WriteFile(own, []byte("downloaded"), 0o755)
	if l, err := pluginLink("linux", own); err != nil || l != "" {
		t.Fatalf("self in bin: %q %v", l, err)
	}
	if b, _ := os.ReadFile(own); string(b) != "downloaded" {
		t.Fatal("own copy replaced")
	}
}

// L2: pladenj prehoda MSI teče → mapa scratch (kloni, .incoming agentov) ostane; zaprt pladenj (MSI CloseTray) ali
// korak, ki ga zapre (namestitev brez MSI) → mapa gre.
func TestUninstallKeepsScratchWhileTrayRuns(t *testing.T) {
	home, _ := pluginHome(t)
	t.Setenv("LOCALAPPDATA", home)
	cache, _ := os.UserCacheDir()
	os.MkdirAll(filepath.Join(cache, "minigw"), 0o700)
	o1, o2, o3 := trayRunning, msiDir, runValueGet
	t.Cleanup(func() { trayRunning, msiDir, runValueGet = o1, o2, o3 })
	scratch := "scratch folder " + filepath.Join(cache, "minigw")
	for _, c := range []struct {
		up          bool
		msi, run    string
		keep, steps bool
	}{{true, `C:\MG`, "tray", true, false}, {false, `C:\MG`, "tray", false, true}, {true, "", "tray", false, true}, {true, "", "", true, false}} {
		trayRunning = func() bool { return c.up }
		msiDir = func() string { return c.msi }
		runValueGet = func(string) (string, error) { return c.run, nil }
		steps, _, busy := uninstallSteps("windows", false)
		listed := slices.ContainsFunc(steps, func(s uninstallStep) bool { return s.what == scratch })
		if listed != c.steps || strings.HasPrefix(busy, scratch+" (the Mini Gateway tray is running") != c.keep {
			t.Fatalf("%+v: listed %v busy %q", c, listed, busy)
		}
	}
	if _, _, busy := uninstallSteps("linux", false); busy != "" { // Linux: korak systemd ustavi serve pred mapo
		t.Fatalf("linux busy %q", busy)
	}
}

func TestUninstallSteps(t *testing.T) {
	home, calls := pluginHome(t)
	cfg, _ := os.UserConfigDir()
	cache, _ := os.UserCacheDir()
	if runtime.GOOS == "windows" { // izolirana shramba (credNS) je prazna: profil default ustvari test
		saveProfile("default", connection{})
	}
	unit := servicePath("linux")
	for _, f := range []string{unit, filepath.Join(cache, "minigw", "scratch", "x"), filepath.Join(cfg, "minigw", "connection.json")} {
		os.MkdirAll(filepath.Dir(f), 0o700)
		os.WriteFile(f, []byte("{}"), 0o600)
	}
	os.MkdirAll(filepath.Join(home, ".claude", "plugins", "data", "minigw-mini-gateway", "bin"), 0o700)

	steps, kept, _ := uninstallSteps("linux", false)
	var what []string
	for _, s := range steps {
		what = append(what, s.what)
	}
	list := strings.Join(what, "\n")
	for _, w := range []string{"Claude Code plugin minigw@mini-gateway", "systemd user unit " + unit, "scratch folder " + filepath.Join(cache, "minigw")} {
		if !strings.Contains(list, w) {
			t.Fatalf("missing %q in\n%s", w, list)
		}
	}
	if strings.Contains(list, "profiles") || !slices.ContainsFunc(kept, func(k string) bool { return strings.HasPrefix(k, "profiles default") }) ||
		!slices.Contains(kept, "settings folder "+filepath.Join(cfg, "minigw")) {
		t.Fatalf("kept %v / steps\n%s", kept, list)
	}
	// Brez --purge: le začasne poti tega testa (vse pod home), nastavitve ostanejo.
	for _, s := range steps {
		if err := s.do(); err != nil {
			t.Fatalf("%s: %v", s.what, err)
		}
	}
	for _, gone := range []string{unit, filepath.Join(cache, "minigw"), filepath.Join(home, ".claude", "plugins", "data", "minigw-mini-gateway")} {
		if errFromStat(gone) == nil {
			t.Fatalf("%s kept", gone)
		}
	}
	if errFromStat(filepath.Join(cfg, "minigw", "connection.json")) != nil {
		t.Fatal("profile removed without --purge")
	}
	if !slices.ContainsFunc(*calls, func(c string) bool { return strings.Contains(c, "disable --now minigw-serve.service") }) {
		t.Fatalf("calls %v", *calls)
	}
	// --purge: na seznamu tudi profili in mapa nastavitev (koraki se tu ne izvedejo: configBase je prava domača mapa).
	steps, kept, _ = uninstallSteps("linux", true)
	what = nil
	for _, s := range steps {
		what = append(what, s.what)
	}
	if len(kept) != 0 || !slices.Contains(what, "profiles default ("+storeName+")") || !slices.Contains(what, "settings folder "+filepath.Join(cfg, "minigw")) {
		t.Fatalf("purge %v %v", what, kept)
	}
	// --purge names a non-empty core folder (the project database) before it is removed.
	core := filepath.Join(cfg, "minigw", coreDirName)
	if slices.Contains(nonEmptyCoreDirs(), core) {
		t.Fatalf("empty core folder %s listed", core)
	}
	if err := os.MkdirAll(core, 0o700); err != nil || os.WriteFile(filepath.Join(core, "core.db"), []byte("x"), 0o600) != nil {
		t.Fatal(err)
	}
	if !slices.Contains(nonEmptyCoreDirs(), core) {
		t.Fatalf("core folder %s not listed: %v", core, nonEmptyCoreDirs())
	}
}

func TestPluginDataSafeAndUnsafeDir(t *testing.T) {
	home, _ := pluginHome(t)
	data := filepath.Join(home, ".claude", "plugins", "data")
	for d, ok := range map[string]bool{
		filepath.Join(data, "minigw-mini-gateway"): true, filepath.Join(data, "other"): false, data + "/x/../minigw": false,
		filepath.Join(home, "minigw"): false, "minigw": false, filepath.Join(data, "minigw", "bin"): false,
	} {
		if pluginDataSafe(d) != ok {
			t.Errorf("pluginDataSafe(%q) != %v", d, ok)
		}
	}
	if runtime.GOOS == "windows" {
		return
	}
	dir := filepath.Join(home, "shared")
	os.MkdirAll(dir, 0o755)
	os.Chmod(dir, 0o777)
	self := filepath.Join(dir, "minigw")
	os.WriteFile(self, []byte("x"), 0o755)
	if _, err := pluginLink("linux", self); err == nil || !strings.Contains(err.Error(), "writable by others") {
		t.Fatalf("world-writable folder linked: %v", err)
	}
	os.Chmod(dir, 0o755)
	os.Chmod(self, 0o777)
	if _, err := pluginLink("linux", self); err == nil {
		t.Fatal("world-writable minigw linked")
	}
	os.Chmod(self, 0o755)
	if _, err := pluginLink("linux", self); err != nil {
		t.Fatal(err)
	}
	// Homebrew: mapa drwxrwxr-x uporabnik:admin je varna; o+w ne, tudi z zaupano skupino.
	// staff (vsak lokalni uporabnik) ne.
	if runtime.GOOS == "darwin" && os.Chown(dir, -1, 20) == nil {
		os.Chmod(dir, 0o775)
		if _, err := pluginLink("darwin", self); err == nil {
			t.Fatal("group-writable staff folder linked")
		}
		os.Chmod(dir, 0o755)
	}
	if runtime.GOOS == "darwin" && os.Chown(dir, -1, 80) == nil {
		os.Chmod(dir, 0o775)
		if _, err := pluginLink("darwin", self); err != nil {
			t.Fatalf("group-writable admin folder refused: %v", err)
		}
		os.Chmod(dir, 0o777)
		if _, err := pluginLink("darwin", self); err == nil {
			t.Fatal("o+w admin folder linked")
		}
		os.Chmod(dir, 0o755)
	}
}

// »claude plugin marketplace remove« sam odstrani vnose iz settings.json: uninstall vseeno pove, da so odstranjeni.
func TestPluginUninstallReportsEntriesRemovedByMarketplace(t *testing.T) {
	home, _ := pluginHome(t)
	p := filepath.Join(home, ".claude", "settings.json")
	if !pluginInstall(linkOS) {
		t.Fatal("install failed")
	}
	runCmd = func(string, ...string) error { return os.WriteFile(p, []byte(`{}`), 0o600) }
	r, w, _ := os.Pipe()
	stdout := os.Stdout
	os.Stdout = w
	pluginUninstall(linkOS, false)
	os.Stdout = stdout
	w.Close()
	out, _ := io.ReadAll(r)
	if !strings.Contains(string(out), "minigw@mini-gateway: removed from "+p) {
		t.Fatalf("output:\n%s", out)
	}
}

// #51: minigw-tui se zažene le iz varne mape in ni zapisljiv za druge.
func TestSettingsTUIPathRefusesUnsafe(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("ACL")
	}
	dir := filepath.Join(t.TempDir(), "bin")
	os.MkdirAll(dir, 0o755)
	self, tui := filepath.Join(dir, "minigw"), filepath.Join(dir, "minigw-tui")
	if _, err := settingsTUIPath("linux", self); err == nil || !strings.Contains(err.Error(), "not installed") {
		t.Fatalf("missing tui: %v", err)
	}
	os.WriteFile(tui, []byte("x"), 0o755)
	if p, err := settingsTUIPath("linux", self); err != nil || p != tui {
		t.Fatalf("safe tui: %q %v", p, err)
	}
	os.Chmod(tui, 0o757)
	if _, err := settingsTUIPath("linux", self); err == nil {
		t.Fatal("world-writable minigw-tui started")
	}
	os.Chmod(tui, 0o755)
	os.Chmod(dir, 0o777)
	defer os.Chmod(dir, 0o755)
	if _, err := settingsTUIPath("linux", self); err == nil {
		t.Fatal("minigw-tui in a world-writable folder started")
	}
}

// #48: povezava vtičnika in minigw-tui le, če nobena mapa nad minigw ni zapisljiva za druge (višje: razen sticky).
func TestSafePathAncestors(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("ACL")
	}
	top := filepath.Join(t.TempDir(), "top")
	dir := filepath.Join(top, "a", "bin")
	os.MkdirAll(dir, 0o755)
	self := filepath.Join(dir, "minigw")
	os.WriteFile(self, []byte("x"), 0o755)
	os.WriteFile(filepath.Join(dir, "minigw-tui"), []byte("x"), 0o755)
	if !safePath(self) {
		t.Fatal("safe path refused")
	}
	os.Chmod(top, 0o777)
	defer os.Chmod(top, 0o755)
	if _, err := pluginLink("linux", self); err == nil {
		t.Fatal("linked under a world-writable ancestor")
	}
	if _, err := settingsTUIPath("linux", self); err == nil {
		t.Fatal("minigw-tui under a world-writable ancestor")
	}
	// povezava iz varne mape na minigw pod mapo, ki je vsem zapisljiva
	link := filepath.Join(t.TempDir(), "minigw")
	os.Symlink(self, link)
	if safePath(link) {
		t.Fatal("symlink into an unsafe folder accepted")
	}
	os.Chmod(top, 0o777|os.ModeSticky) // sticky (kot /tmp): višje mape smejo
	if !safePath(self) {
		t.Fatal("sticky ancestor refused")
	}
	os.Chmod(filepath.Join(top, "a"), 0o777|os.ModeSticky) // mapa minigw sama: nikoli
	defer os.Chmod(filepath.Join(top, "a"), 0o755)
	if safePath(filepath.Join(top, "a", "bin")) {
		t.Fatal("sticky parent of the file accepted")
	}
}

// #48 (M2): višja mapa s sticky velja le, če je last roota ali uporabnika (ne tuja mapa 1777); kot root (Docker).
func TestSafePathForeignSticky(t *testing.T) {
	if runtime.GOOS == "windows" || os.Getuid() != 0 {
		t.Skip("needs root to chown")
	}
	top := filepath.Join(t.TempDir(), "top")
	dir := filepath.Join(top, "a", "bin")
	os.MkdirAll(dir, 0o755)
	self := filepath.Join(dir, "minigw")
	os.WriteFile(self, []byte("x"), 0o755)
	os.Chmod(top, 0o777|os.ModeSticky)
	if !safePath(self) {
		t.Fatal("root's sticky folder refused")
	}
	os.Chown(top, 1234, 1234)
	if safePath(self) {
		t.Fatal("foreign sticky folder accepted")
	}
}

// #63: autoUpdate na našem vnosu; stari vnos (do 0.5.21) nadgradimo, uporabnikovega ne; oba sta naša za odstranitev.
func TestPluginMarketplaceAutoUpdate(t *testing.T) {
	const old = `{"source":{"source":"github","repo":"gh-commiter/mini-llm-gateway"}}`
	const own = `{"source":{"source":"github","repo":"gh-commiter/mini-llm-gateway","ref":"release"},"autoUpdate":false}`
	settings := func(market string) map[string]any {
		j := `{"enabledPlugins":{"minigw@mini-gateway":true}}`
		if market != "" {
			j = `{"enabledPlugins":{"minigw@mini-gateway":true},"extraKnownMarketplaces":{"mini-gateway":` + market + `}}`
		}
		return jsonValue(j).(map[string]any)
	}
	market := func(s map[string]any) any { return s["extraKnownMarketplaces"].(map[string]any)["mini-gateway"] }

	s := map[string]any{} // nova
	if a, u, off, err := pluginAdd(s); !a || u || off || err != nil || market(s).(map[string]any)["autoUpdate"] != true ||
		market(s).(map[string]any)["source"].(map[string]any)["ref"] != "release" {
		t.Fatalf("fresh: %v %v %v %v %v", a, u, off, err, s)
	}
	s = settings(old) // nadgradnja starega
	if a, u, _, _ := pluginAdd(s); a || !u || !reflect.DeepEqual(market(s), pluginMarketplace()) {
		t.Fatalf("old: %v %v %v", a, u, s)
	}
	if a, u, _, _ := pluginAdd(s); a || u { // drugič nič
		t.Fatalf("again: %v %v", a, u)
	}
	s = settings(own) // uporabnikov false ostane
	if a, u, _, _ := pluginAdd(s); a || u || !reflect.DeepEqual(market(s), jsonValue(own)) {
		t.Fatalf("own: %v %v %v", a, u, s)
	}
	if ch, _ := pluginRemove(s); !ch || market(s) == nil {
		t.Fatalf("own removed: %v", s)
	}
	for _, m := range []string{old, pluginMarketJSON} { // odstranitev obeh naših
		s = settings(m)
		if ch, _ := pluginRemove(s); !ch || len(s["extraKnownMarketplaces"].(map[string]any)) != 0 || len(s["enabledPlugins"].(map[string]any)) != 0 {
			t.Fatalf("remove %s: %v", m, s)
		}
	}
}

// hooks.json of the plugin: the kb hook entries (brief kb7b) exist, call only the plugin's own scripts, finish within a minute
// and never use exit code 2 (a hook that blocks the session).
func TestPluginHooksFile(t *testing.T) {
	var f struct {
		Hooks map[string][]struct {
			Matcher string
			Hooks   []struct {
				Type, Command string
				Timeout       int
			}
		}
	}
	// The Linux Docker gate and the Windows suite get only client/: the plugin folder is checked where the whole repository is.
	if _, err := os.Stat("../plugins"); os.IsNotExist(err) {
		t.Skip("needs the repository tree (../plugins)")
	}
	if err := json.Unmarshal(srcFile(t, "../plugins/minigw/hooks/hooks.json"), &f); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"SessionStart": "kb-hook.sh\" start", "PreCompact": "kb-hook.sh\" compact", "Stop": "kb-hook.sh\" stop", "SessionEnd": "kb-hook.sh\" end"}
	for event, suffix := range want {
		found := false
		for _, g := range f.Hooks[event] {
			for _, h := range g.Hooks {
				found = found || strings.HasSuffix(h.Command, suffix)
			}
		}
		if !found {
			t.Errorf("%s: no entry ending in %s", event, suffix)
		}
	}
	if m := f.Hooks["PreCompact"][0].Matcher; m != "manual|auto" {
		t.Errorf("PreCompact matcher %q", m)
	}
	scripts := map[string]bool{}
	for event, groups := range f.Hooks {
		for _, g := range groups {
			for _, h := range g.Hooks {
				if h.Type != "command" || !strings.HasPrefix(h.Command, `"${CLAUDE_PLUGIN_ROOT}/scripts/`) || strings.Contains(h.Command, "exit") {
					t.Errorf("%s: unexpected command %q", event, h.Command)
				}
				if h.Timeout < 1 || h.Timeout > 60 {
					t.Errorf("%s: timeout %d", event, h.Timeout)
				}
				name := strings.TrimPrefix(strings.SplitN(h.Command, `"`, 3)[1], "${CLAUDE_PLUGIN_ROOT}/scripts/")
				scripts[name] = true
			}
		}
	}
	for name := range scripts {
		body := string(srcFile(t, "../plugins/minigw/scripts/"+name))
		if strings.Contains(body, "exit 2") {
			t.Errorf("%s uses exit code 2", name)
		}
	}
	if !scripts["kb-hook.sh"] || !scripts["install-minigw.sh"] {
		t.Errorf("scripts %v", scripts)
	}
}

// installRun configures runInstallScript: a fake Mac (or Linux) with fake uname, codesign (valid inside the temp dir
// unless the file says UNSIGNED), curl (answers the releases/latest redirect and, with sums, SHA256SUMS(.sig); nothing
// else is ever downloaded), ssh-keygen (accepts), plutil (no app version), stat and claude.
type installRun struct {
	goos      string // "" = Darwin
	installed string // what a fake client on PATH (Linux: ~/.local/bin) prints for --version; "-" = none
	latest    string
	min       string // MIN_CLIENT content; "" = 0.7.32, "-" = no file
	sums      string // content of the release's SHA256SUMS; "" = offline
	preLink   bool   // the plugin's bin/minigw already links to the installed client
	ownCopy   string // a regular, unsigned bin/minigw that writes the marker file when it runs
}

type installResult struct{ out, exe, marker string }

func runInstallScript(t *testing.T, o installRun) installResult {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("needs sh")
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("needs sh")
	}
	// The Linux Docker gate and the Windows suite get only client/: the plugin folder is checked where the whole repository is.
	if _, err := os.Stat("../plugins"); os.IsNotExist(err) {
		t.Skip("needs the repository tree (../plugins)")
	}
	body := srcFile(t, "../plugins/minigw/scripts/install-minigw.sh")
	tmp := t.TempDir()
	if r, err := filepath.EvalSymlinks(tmp); err == nil {
		tmp = r
	}
	write := func(rel, content string) string {
		p := filepath.Join(tmp, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write("plugin/scripts/install-minigw.sh", string(body))
	script := filepath.Join(tmp, "plugin/scripts/install-minigw.sh")
	min := o.min
	if min == "" {
		min = "0.7.32\n"
	}
	if min != "-" {
		write("plugin/scripts/MIN_CLIENT", min)
	}
	osName := "Darwin"
	if o.goos == "linux" {
		osName = "Linux"
	}
	write("fb/uname", "#!/bin/sh\n[ \"$1\" = -s ] && echo "+osName+" || echo arm64\n")
	write("fb/codesign", "#!/bin/sh\nfor a; do l=$a; done\ngrep -q UNSIGNED \"$l\" 2>/dev/null && exit 1\ncase $l in "+tmp+"/*) exit 0 ;; esac\nexit 1\n")
	write("fb/ssh-keygen", "#!/bin/sh\nexit 0\n")
	write("fb/stat", "#!/bin/sh\necho \"$(id -u) 755\"\n")
	write("fb/curl", `#!/bin/sh
case "$*" in *redirect_url*) echo "https://github.com/x/releases/tag/v`+o.latest+`"; exit 0 ;; esac
for a; do [ "$p" = -o ] && out=$a; u=$a; p=$a; done
[ -f `+tmp+`/sums ] || exit 22
case $u in */SHA256SUMS) cat `+tmp+`/sums > "$out" ;; */SHA256SUMS.sig) echo sig > "$out" ;; *) exit 22 ;; esac
`)
	if o.sums != "" {
		write("sums", o.sums)
	}
	write("fb/plutil", "#!/bin/sh\nexit 1\n")
	write("fb/claude", "#!/bin/sh\nexit 0\n")
	home := filepath.Join(tmp, "home")
	inst := filepath.Join(tmp, "inst", "minigw")
	if o.goos == "linux" {
		inst = filepath.Join(home, ".local", "bin", "minigw")
	}
	if o.installed != "-" {
		write(strings.TrimPrefix(inst, tmp+"/"), "#!/bin/sh\ncase $1 in --version) echo '"+o.installed+"' ;; status) exit 0 ;; esac\n")
	}
	data := filepath.Join(tmp, "data")
	exe := filepath.Join(data, "bin", "minigw")
	marker := filepath.Join(tmp, "marker")
	if o.preLink {
		if err := os.MkdirAll(filepath.Dir(exe), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(inst, exe); err != nil {
			t.Fatal(err)
		}
	}
	if o.ownCopy != "" {
		write("data/bin/minigw", "#!/bin/sh\n# UNSIGNED\ntouch "+marker+"\necho '"+o.ownCopy+"'\n")
	}
	cmd := exec.Command(sh, script)
	cmd.Env = []string{"HOME=" + home, "CLAUDE_PLUGIN_DATA=" + data,
		"PATH=" + filepath.Join(tmp, "fb") + string(os.PathListSeparator) + filepath.Join(tmp, "inst") + string(os.PathListSeparator) + "/usr/bin:/bin"}
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Errorf("the script must exit 0: %v\n%s", err, out)
	}
	return installResult{string(out), exe, marker}
}

func (r installResult) linked() bool { _, err := os.Readlink(r.exe); return err == nil }

func (r installResult) mustSay(t *testing.T, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(r.out, w) {
			t.Errorf("output lacks %q:\n%s", w, r.out)
		}
	}
}

func TestInstallScriptOldClientOfflineIsUsedAndSaidSo(t *testing.T) {
	r := runInstallScript(t, installRun{installed: "minigw 0.7.0", latest: "0.7.32"})
	if !r.linked() {
		t.Errorf("offline, an authentic old client is better than none:\n%s", r.out)
	}
	r.mustSay(t, "minigw plugin: using the installed minigw 0.7.0, older than 0.7.32 which this plugin's commands need; some commands will not work until it is updated (brew upgrade --cask")
}

func TestInstallScriptOldLinkedClientOfflineIsSaidSo(t *testing.T) {
	r := runInstallScript(t, installRun{installed: "minigw 0.7.0", latest: "0.7.32", preLink: true})
	r.mustSay(t, "using the installed minigw 0.7.0, older than 0.7.32")
}

func TestInstallScriptCurrentInstalledClientIsLinked(t *testing.T) {
	r := runInstallScript(t, installRun{installed: "minigw 0.7.32", latest: "0.7.32"})
	if l, err := os.Readlink(r.exe); err != nil || !strings.HasSuffix(l, "/inst/minigw") {
		t.Errorf("not linked (%q, %v):\n%s", l, err, r.out)
	}
	if strings.Contains(r.out, "older") || strings.Contains(r.out, "available") {
		t.Errorf("unexpected message:\n%s", r.out)
	}
}

func TestInstallScriptMacWithoutAppHintsAtNewerRelease(t *testing.T) {
	r := runInstallScript(t, installRun{installed: "minigw 0.7.32", latest: "0.7.33"})
	if !r.linked() {
		t.Errorf("not linked:\n%s", r.out)
	}
	r.mustSay(t, "minigw 0.7.32 is installed, 0.7.33 is available: brew upgrade --cask")
}

// A version that is not strict digits and dots counts as too old (never as new enough); a -suffix is cut and the numbers compared.
func TestInstallScriptOddVersionsDoNotPassTheMinimum(t *testing.T) {
	for _, v := range []string{"minigw v0.7.99", "minigw 0.7.99.1.2", "minigw ", "minigw 0.7.", "garbage"} {
		r := runInstallScript(t, installRun{installed: v, latest: "0.7.32"})
		r.mustSay(t, "using the installed minigw")
		if strings.Contains(r.out, "connected to Mini Gateway") && !strings.Contains(r.out, "older than") && !strings.Contains(r.out, "could not be read") {
			t.Errorf("%q passed silently:\n%s", v, r.out)
		}
	}
	r := runInstallScript(t, installRun{installed: "minigw 0.7.99-beta", latest: "0.7.99"})
	if !r.linked() || strings.Contains(r.out, "older") || strings.Contains(r.out, "could not be read") {
		t.Errorf("0.7.99-beta should count as 0.7.99:\n%s", r.out)
	}
	r = runInstallScript(t, installRun{installed: "minigw 0.7.4-beta", latest: "0.7.32"})
	r.mustSay(t, "installed minigw 0.7.4, older than 0.7.32")
}

func TestInstallScriptMinimumFile(t *testing.T) {
	for _, m := range []string{"-", "0.7.x\n", "{\"version\": \"0.7.32\"}\n", "0.7.32.1.2\n", "\n"} {
		r := runInstallScript(t, installRun{installed: "minigw 0.7.31", latest: "0.7.31", min: m})
		r.mustSay(t, "MIN_CLIENT is missing or invalid", "0.7.30")
		if strings.Contains(r.out, "older than") {
			t.Errorf("min %q: the literal fallback 0.7.30 accepts 0.7.31:\n%s", m, r.out)
		}
	}
	r := runInstallScript(t, installRun{installed: "minigw 0.7.32", latest: "0.7.32", min: "0.7.40\n"})
	r.mustSay(t, "older than 0.7.40")
}

// Authenticate before executing (macOS): a cached copy that fails the signature check is never run, not even for --version.
func TestInstallScriptCachedUnsignedCopyIsNeverExecuted(t *testing.T) {
	sums := "VERSION 0.7.5\n" + strings.Repeat("a", 64) + "  minigw.zip\n"
	r := runInstallScript(t, installRun{installed: "-", latest: "0.7.32", sums: sums, ownCopy: "minigw 0.7.99"})
	if _, err := os.Stat(r.marker); err == nil {
		t.Errorf("an unsigned cached binary was executed:\n%s", r.out)
	}
	r = runInstallScript(t, installRun{installed: "-", latest: "0.7.32", ownCopy: "minigw 0.7.99"})
	if _, err := os.Stat(r.marker); err == nil {
		t.Errorf("an unsigned cached binary was executed offline:\n%s", r.out)
	}
}

func TestInstallScriptLinuxOldClientOffline(t *testing.T) {
	r := runInstallScript(t, installRun{goos: "linux", installed: "minigw 0.7.0", latest: "0.7.32"})
	if !r.linked() {
		t.Errorf("not linked:\n%s", r.out)
	}
	r.mustSay(t, "using the installed minigw 0.7.0, older than 0.7.32")
}

func TestPluginMinClientIsThePluginVersion(t *testing.T) {
	if _, err := os.Stat("../plugins"); os.IsNotExist(err) {
		t.Skip("needs the repository tree (../plugins)")
	}
	var m struct{ Version string }
	if err := json.Unmarshal(srcFile(t, "../plugins/minigw/.claude-plugin/plugin.json"), &m); err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimRight(string(srcFile(t, "../plugins/minigw/scripts/MIN_CLIENT")), "\n"); got != m.Version || !regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+\n$`).Match(srcFile(t, "../plugins/minigw/scripts/MIN_CLIENT")) {
		t.Errorf("scripts/MIN_CLIENT is %q, the plugin version is %q: bump-version.sh must write both", got, m.Version)
	}
}
