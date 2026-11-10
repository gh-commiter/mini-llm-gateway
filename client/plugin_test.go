package main

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// pluginHome: začasna domača mapa (nikoli prava ~/.claude) in brez pravega claude.
func pluginHome(t *testing.T) (home string, calls *[]string) {
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("XDG_CACHE_HOME", filepath.Join(home, ".cache"))
	t.Setenv("CLAUDE_PLUGIN_DATA", "")
	var c []string
	f, r := findClaudeFn, runCmd
	findClaudeFn = func() string { return "/fake/claude" }
	runCmd = func(name string, args ...string) error { c = append(c, name+" "+strings.Join(args, " ")); return nil }
	t.Cleanup(func() { findClaudeFn, runCmd = f, r })
	return home, &c
}

func TestPluginSettingsMergeKeepsUserFalseAndMode(t *testing.T) {
	home, _ := pluginHome(t)
	p := filepath.Join(home, ".claude", "settings.json")
	os.MkdirAll(filepath.Dir(p), 0o700)
	os.WriteFile(p, []byte(`{"model":"opus","n":12345678901234567890,"url":"a<b>&c","enabledPlugins":{"minigw@mini-gateway":false}}`), 0o640)
	if !pluginInstall("linux") {
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
	if !pluginUninstall("linux", true) {
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
	if !pluginInstall("linux") {
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
	pluginUninstall("linux", false)
	b, _ = os.ReadFile(p)
	if strings.Contains(string(b), "minigw@mini-gateway") || !strings.Contains(string(b), `"/x"`) {
		t.Fatalf("after uninstall:\n%s", b)
	}
	if !slices.Contains(*calls, "/fake/claude plugin marketplace remove mini-gateway --scope user") {
		t.Fatalf("calls %v", *calls)
	}
	*calls = nil
	pluginUninstall("linux", true)
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
		if pluginInstall("linux") {
			t.Fatalf("%q accepted", bad)
		}
		if pluginUninstall("linux", true) && !nonObject[bad] {
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
	pluginInstall("linux")
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
	if !pluginInstall("linux") {
		t.Fatal("install failed")
	}
	runCmd = func(string, ...string) error { return os.WriteFile(p, []byte(`{}`), 0o600) }
	r, w, _ := os.Pipe()
	stdout := os.Stdout
	os.Stdout = w
	pluginUninstall("linux", false)
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
