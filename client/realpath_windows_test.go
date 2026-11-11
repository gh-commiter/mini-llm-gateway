//go:build windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// #83: stičišče (EvalSymlinks mu od Go 1.23 ne sledi) v dovoljeni mapi na Scratch, mapo minigw.exe ali mapo nastavitev
// se zazna; askOnPath takrat ne da »minigw« na PATH; prepovedi Claude imajo končno pot mape nastavitev.
func TestRealPathJunctionsWindows(t *testing.T) {
	base := realOf(t, t.TempDir())
	scratch, exeDir, cfg, allowed := filepath.Join(base, "scratch"), filepath.Join(base, "bin"), filepath.Join(base, "cfg"), filepath.Join(base, "allowed")
	for _, d := range []string{scratch, exeDir, cfg, allowed} {
		os.Mkdir(d, 0o755)
	}
	os.WriteFile(filepath.Join(cfg, "admin.secret"), []byte("x"), 0o600)
	js, jb, jc := filepath.Join(allowed, "s"), filepath.Join(allowed, "b"), filepath.Join(allowed, "c")
	junction(t, scratch, js)
	junction(t, exeDir, jb)
	junction(t, cfg, jc)
	if r, _ := filepath.EvalSymlinks(js); samePath(r, scratch) {
		t.Log("EvalSymlinks follows junctions here")
	}
	if !samePath(realOf(t, js), scratch) || !samePath(realOf(t, filepath.Join(js, "new", "x")), filepath.Join(scratch, "new", "x")) {
		t.Error("junction not resolved")
	}
	if len(scratchOverlaps([]string{js}, scratch)) != 1 {
		t.Error("allowed junction to Scratch not detected")
	}
	if !agentWritable(exeDir, []string{jb}) {
		t.Error("allowed junction to the minigw.exe folder not detected")
	}
	if _, err := checkFolder(jc, cfg, true); err == nil {
		t.Error("checkFolder accepted a junction to the config folder")
	}
	if !under(realOf(t, filepath.Join(jc, "admin.secret")), cfg) {
		t.Error("junction into the config folder not resolved")
	}
	s, _ := testServer(t, nil)
	s.cfgDir = jc // mapa nastavitev, podana skozi stičišče: prepoved tudi za končno pot
	if args := strings.Join(s.agentArgs(t.TempDir(), false), " "); !strings.Contains(args, "Read("+ruleAbs(cfg)+"/**)") {
		t.Errorf("no deny for the final config path: %s", args)
	}
	os.Remove(scratch) // stičišče na izbrisano mapo: napaka, ne pot skozi stičišče
	if r, err := realPath(filepath.Join(js, "x")); err == nil {
		t.Errorf("dangling junction resolved to %s", r)
	}
	if len(scratchOverlaps([]string{allowed}, js)) != 1 {
		t.Error("unresolvable Scratch must overlap (fail closed)")
	}
}

// #83: ime 8.3 da isto (dolgo) končno pot; tudi za pot, ki je še ni.
func TestRealPathShortNameWindows(t *testing.T) {
	long := filepath.Join(realOf(t, t.TempDir()), "a long folder name")
	os.Mkdir(long, 0o755)
	u, _ := windows.UTF16PtrFromString(long)
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n, err := windows.GetShortPathName(u, &buf[0], uint32(len(buf)))
	short := windows.UTF16ToString(buf[:n])
	if err != nil || n == 0 || short == long {
		t.Skip("8.3 names are off on this volume")
	}
	if got := realOf(t, short); !samePath(got, long) {
		t.Errorf("realPath(%s) = %s, want %s", short, got, long)
	}
	if got := realOf(t, filepath.Join(short, "new")); !samePath(got, filepath.Join(long, "new")) {
		t.Errorf("nonexistent under 8.3: %s", got)
	}
	if !sameReal(short, long) || len(scratchOverlaps([]string{short}, long)) != 1 {
		t.Error("8.3 alias of Scratch not detected")
	}
}

// #83: navidezen pogon (subst, kot preslikan pogon brez omrežja) in pot UNC: realPath vrne eno obliko, prekrivanje se zazna.
func TestRealPathSubstAndUNCWindows(t *testing.T) {
	dir := realOf(t, t.TempDir())
	os.Mkdir(filepath.Join(dir, "scratch"), 0o755)
	drive := ""
	for c := 'Z'; c >= 'M'; c-- {
		if _, err := os.Stat(string(c) + `:\`); err != nil {
			drive = string(c) + ":"
			break
		}
	}
	if drive != "" { // odklopljen pogon: pot ostane (ni povezave), ni napaka, ki bi izklopila agente ob zagonu
		if got, err := realPath(drive + `\proj\x`); err != nil || got != drive+`\proj\x` {
			t.Errorf("missing drive: %q %v", got, err)
		}
	}
	if drive != "" && exec.Command("subst", drive, dir).Run() == nil {
		t.Cleanup(func() { exec.Command("subst", drive, "/d").Run() })
		if got := realOf(t, drive+`\scratch`); !samePath(got, filepath.Join(dir, "scratch")) {
			t.Errorf("subst drive: %s", got)
		}
		if len(scratchOverlaps([]string{drive + `\`}, filepath.Join(dir, "scratch"))) != 1 {
			t.Error("allowed subst drive containing Scratch not detected")
		}
	} else {
		t.Log("no free drive letter or subst failed")
	}
	unc := `\\localhost\` + strings.Replace(dir, ":", "$", 1) // administrativna skupna raba (le, če je dosegljiva)
	if _, err := os.Stat(unc); err != nil {
		t.Logf("UNC %s not reachable: %v", unc, err)
		return
	}
	got, err := realPath(filepath.Join(unc, "new", "x"))
	if err != nil {
		t.Fatalf("UNC: %v", err)
	}
	if !strings.HasPrefix(got, `\\`) || !strings.HasSuffix(got, `\new\x`) {
		t.Errorf("UNC form: %s", got)
	}
	if strings.HasPrefix(got, `\\?\`) {
		t.Errorf("extended prefix kept: %s", got)
	}
}

// #83: mapa nastavitev skozi stičišče (mklink /J), podana mxcWrap: Denied ima tudi cilj.
func TestMXCWrapDeniesConfigJunctionWindows(t *testing.T) {
	mxcDeniesTarget(t, junction)
}

// HPX360 (0.5.25): stičišče sredi poti (C:\work\x\proj, x = stičišče) — EvalSymlinks pade, checkFolder vzame finalPath;
// pot skozi stičišče v mapo nastavitev ostane zavrnjena.
func TestCheckFolderJunctionInParentWindows(t *testing.T) {
	base := realOf(t, t.TempDir())
	own, cfg := filepath.Join(base, "own"), filepath.Join(base, "cfg")
	os.MkdirAll(filepath.Join(own, "proj"), 0o755)
	os.MkdirAll(filepath.Join(cfg, "sub"), 0o755)
	junction(t, own, filepath.Join(base, "x"))
	junction(t, cfg, filepath.Join(base, "y"))
	real, err := checkFolder(filepath.Join(base, "x", "proj"), cfg, false)
	if err != nil || !samePath(real, filepath.Join(own, "proj")) {
		t.Fatalf("folder behind a junction: %q %v", real, err)
	}
	if _, err := checkFolder(filepath.Join(base, "y", "sub"), cfg, false); err == nil {
		t.Fatal("folder inside the config folder through a junction in the parent accepted")
	}
}
