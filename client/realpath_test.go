package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// realOf: realPath v testu (napaka = neuspeh).
func realOf(t *testing.T, p string) string {
	t.Helper()
	r, err := realPath(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// #83: realPath sledi povezavi (Windows: stičišču), pot, ki je še ni, prek obstoječe nadrejene; visoča povezava je
// napaka, ne sestavljena pot. Povezava v dovoljeni mapi na Scratch, mapo minigw.exe ali mapo nastavitev se zazna.
func TestRealPathLinks(t *testing.T) {
	base := realOf(t, t.TempDir())
	target, scratch, exeDir, cfg, allowed := filepath.Join(base, "target"), filepath.Join(base, "scratch"),
		filepath.Join(base, "bin"), filepath.Join(base, "cfg"), filepath.Join(base, "allowed")
	for _, d := range []string{target, scratch, exeDir, cfg, allowed} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(base, "link")
	dirLink(t, target, link)
	if got := realOf(t, link); !samePath(got, target) {
		t.Errorf("realPath(link) = %s, want %s", got, target)
	}
	if got := realOf(t, filepath.Join(link, "new", "deeper")); !samePath(got, filepath.Join(target, "new", "deeper")) {
		t.Errorf("nonexistent descendant: %s", got)
	}
	gone := filepath.Join(base, "gone")
	os.Mkdir(gone, 0o755)
	dangling := filepath.Join(base, "dangling")
	dirLink(t, gone, dangling)
	os.Remove(gone)
	for _, p := range []string{dangling, filepath.Join(dangling, "x")} {
		if r, err := realPath(p); err == nil {
			t.Errorf("dangling link %s resolved to %s", p, r)
		}
	}

	toScratch, toExe, toCfg := filepath.Join(allowed, "s"), filepath.Join(allowed, "b"), filepath.Join(allowed, "deep", "c")
	os.Mkdir(filepath.Join(allowed, "deep"), 0o755)
	dirLink(t, scratch, toScratch)
	dirLink(t, exeDir, toExe)
	dirLink(t, cfg, toCfg)
	if bad := scratchOverlaps([]string{toScratch}, scratch); len(bad) != 1 {
		t.Error("allowed folder linked to Scratch not detected")
	}
	if !agentWritable(exeDir, []string{toExe}) {
		t.Error("allowed folder linked to the minigw.exe folder not detected")
	}
	if agentWritable(exeDir, []string{target}) {
		t.Error("unrelated folder counted as writable")
	}
	if _, err := checkFolder(toCfg, cfg, true); err == nil {
		t.Error("checkFolder: link to the config folder accepted")
	}
	if !under(realOf(t, filepath.Join(toCfg, "admin.secret")), cfg) {
		t.Error("deep link into the config folder not resolved to it")
	}
	if !agentWritable(exeDir, []string{dangling}) {
		t.Error("unresolvable folder must count as writable (fail closed)")
	}
	if bad := scratchOverlaps([]string{dangling}, scratch); len(bad) != 1 {
		t.Error("unresolvable allowed folder must count as overlapping (fail closed)")
	}
	if !sameReal(link, target) || sameReal(dangling, dangling) {
		t.Error("sameReal")
	}
}

// #83: mapa nastavitev dobesedno in kanonično v prepovedih orodij Claude (in denyRead peskovnika zunaj Windows).
func TestAgentArgsDenyConfigCanonical(t *testing.T) {
	s, _ := testServer(t, nil)
	base := realOf(t, t.TempDir())
	cfg, alias := filepath.Join(base, "cfg"), filepath.Join(base, "alias")
	os.Mkdir(cfg, 0o700)
	dirLink(t, cfg, alias)
	s.cfgDir = alias
	args := strings.Join(s.agentArgs(t.TempDir(), false), " ")
	for _, d := range []string{alias, cfg} {
		if !strings.Contains(args, "Read("+ruleAbs(d)+"/**)") {
			t.Errorf("no Read deny for %s: %s", d, args)
		}
	}
	if want := `"denyRead":["~",` + mustJSON(alias) + "," + mustJSON(cfg); runtime.GOOS != "windows" && !strings.Contains(args, want) {
		t.Errorf("denyRead lacks the config folder forms: %s", args)
	}
	if p := scratchPaths(alias); !slices.Contains(p, alias) || !slices.ContainsFunc(p, func(x string) bool { return samePath(x, cfg) }) {
		t.Errorf("scratchPaths(alias) = %v", p)
	}
}

// #83: nečitljiva nadrejena mapa je napaka, ne pot, sestavljena iz nerazrešenih delov.
func TestRealPathUnreadableParent(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("POSIX permissions as non-root")
	}
	d := filepath.Join(t.TempDir(), "locked")
	os.MkdirAll(filepath.Join(d, "x"), 0o755)
	os.Chmod(d, 0)
	t.Cleanup(func() { os.Chmod(d, 0o755) })
	if r, err := realPath(filepath.Join(d, "x", "new")); err == nil {
		t.Errorf("resolved through an unreadable folder: %s", r)
	}
}

// #83: Grok --deny in profil Codex imata mapo nastavitev dobesedno in kanonično.
func TestGrokCodexDenyConfigCanonical(t *testing.T) {
	base := realOf(t, t.TempDir())
	cfg, alias := filepath.Join(base, "cfg"), filepath.Join(base, "alias")
	os.Mkdir(cfg, 0o700)
	dirLink(t, cfg, alias)
	g := strings.Join(grokArgs("/w", "/p", alias, true, "", true, ""), " ")
	for _, d := range []string{alias, cfg} {
		if !strings.Contains(g, "--deny Read("+filepath.ToSlash(d)+"/**)") {
			t.Errorf("grok: no deny for %s: %s", d, g)
		}
	}
	p := strings.Join(codexTreeProfile([]string{"/w"}, []string{"/w/.git"}, scratchPaths(alias), false, "/usr/bin"), " ")
	for _, d := range []string{alias, cfg} {
		if q, _ := json.Marshal(d); !strings.Contains(p, string(q)+`="none"`) { // TOML-ključ kot v profilu (Windows: \\)
			t.Errorf("codex profile lacks %s: %s", d, p)
		}
	}
}

// #83: mapa PATH, ki je povezava (Windows: stičišče) v Scratch ali mapo nastavitev, ne gre v škatlo; tudi ne kot koren orodja.
func TestMXCPathDirsLinks(t *testing.T) {
	base := realOf(t, t.TempDir())
	scratch, cfg, home, tools := filepath.Join(base, "scratch"), filepath.Join(base, "cfg"), filepath.Join(base, "home"), filepath.Join(base, "tools", "bin")
	for _, d := range []string{filepath.Join(scratch, "bin"), cfg, home, tools} {
		os.MkdirAll(d, 0o755)
	}
	toScratch, toCfg, toolLink := filepath.Join(base, "s"), filepath.Join(base, "c"), filepath.Join(base, "t")
	dirLink(t, filepath.Join(scratch, "bin"), toScratch)
	dirLink(t, cfg, toCfg)
	dirLink(t, scratch, toolLink)
	gone := filepath.Join(base, "gone")
	os.Mkdir(gone, 0o755)
	dangling := filepath.Join(base, "dangling")
	dirLink(t, gone, dangling)
	os.Remove(gone)
	got := mxcPathDirs([]string{toScratch, toCfg, dangling, tools}, nil, home, scratch, cfg)
	if !slices.Equal(got, []string{tools}) {
		t.Errorf("path: %v", got)
	}
	if r := toolRoot(filepath.Join(toolLink, "bin"), home, scratch, cfg); r != "" {
		t.Errorf("toolRoot through a link into Scratch: %q", r)
	}
	if r := toolRoot(tools, home, scratch, cfg); r != filepath.Dir(tools) {
		t.Errorf("toolRoot: %q", r)
	}
}

// #83: mapa nastavitev, podana skozi povezavo (Windows: stičišče), je v zahtevi škatle med prepovedanimi tudi s ciljem.
func TestMXCWrapDeniesConfigTarget(t *testing.T) {
	mxcDeniesTarget(t, dirLink)
}

func mxcDeniesTarget(t *testing.T, link func(*testing.T, string, string)) {
	base := realOf(t, t.TempDir())
	scratch, cfg, work, alias := filepath.Join(base, "scratch"), filepath.Join(base, "cfg"), filepath.Join(base, "work"), filepath.Join(base, "alias")
	for _, d := range []string{scratch, cfg, work} {
		os.Mkdir(d, 0o700)
	}
	link(t, cfg, alias)
	_, args, done, err := mxcWrap("wxc", "agent", nil, agentMounts{cfgDir: alias, hide: scratch, rw: []string{work}, cwd: work, cmdEnv: agentEnv(os.Environ())})
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	var req mxcRequest
	b, _ := os.ReadFile(args[0])
	if err := json.Unmarshal(b, &req); err != nil {
		t.Fatal(err)
	}
	if !slices.ContainsFunc(req.Filesystem.Denied, func(d string) bool { return samePath(d, cfg) }) || !slices.Contains(req.Filesystem.Denied, alias) {
		t.Errorf("denied: %v", req.Filesystem.Denied)
	}
}

// #83 (L1): ovoj git: povezava (stičišče) v $MINIGW_DEPS, ki vodi ven, ne dovoli init/clone.
func TestGitWrapRefusalLinkInDeps(t *testing.T) {
	base := realOf(t, t.TempDir())
	deps, out := filepath.Join(base, "deps"), filepath.Join(base, "out")
	os.Mkdir(deps, 0o700)
	os.Mkdir(out, 0o700)
	dirLink(t, out, filepath.Join(deps, "esc"))
	env := func(k string) string {
		return map[string]string{"MINIGW_DEPS": deps, "MINIGW_SANDBOXED": "1"}[k]
	}
	if why := gitWrapRefusal(deps, []string{"init", filepath.Join(deps, "ok")}, env); why != "" {
		t.Errorf("init under deps refused: %s", why)
	}
	for _, p := range []string{filepath.Join(deps, "esc"), filepath.Join(deps, "esc", "new")} {
		if why := gitWrapRefusal(deps, []string{"init", p}, env); !strings.Contains(why, "only under $MINIGW_DEPS") {
			t.Errorf("init through a link out of deps allowed: %s %q", p, why)
		}
	}
}

// #83: Scratch skozi povezavo v mapo nastavitev zavrnjen; nestedInside/repoRoot ob nerazrešljivi poti ne.
func TestScratchCheckLinkIntoConfig(t *testing.T) {
	s, _ := testServer(t, nil)
	base := realOf(t, t.TempDir())
	cfg := filepath.Join(base, "cfg")
	os.Mkdir(cfg, 0o700)
	s.cfgDir = cfg
	link := filepath.Join(base, "link")
	dirLink(t, cfg, link)
	if why := s.scratchCheck(filepath.Join(link, "scratch"), false); !strings.Contains(why, "config folder") {
		t.Errorf("Scratch through a link into the config folder: %q", why)
	}
	gone := filepath.Join(base, "gone")
	os.Mkdir(gone, 0o755)
	dangling := filepath.Join(base, "dangling")
	dirLink(t, gone, dangling)
	os.Remove(gone)
	if s.scratchCheck(filepath.Join(dangling, "x"), false) == "" {
		t.Error("unresolvable Scratch accepted")
	}
	if repoRoot(dangling, dangling) || nestedInside(base, "dangling") {
		t.Error("unresolvable path counted as a repository root")
	}
}
