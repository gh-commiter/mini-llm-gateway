package main

import (
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
	if _, err := checkFolder(toCfg, cfg); err == nil {
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
