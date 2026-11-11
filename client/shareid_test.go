package main

import (
	"os"
	"path/filepath"
	"testing"
)

// fakeHome: začasna domača mapa (tudi mapa nastavitev) s projektom; vrne dom in projekt.
func fakeHome(t *testing.T) (string, string) {
	t.Helper()
	home, _ := filepath.EvalSymlinks(t.TempDir())
	home = filepath.Join(home, "verylonghomefolder")
	proj := filepath.Join(home, "proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"HOME", "USERPROFILE", "XDG_CONFIG_HOME", "APPDATA"} {
		t.Setenv(k, home)
	}
	return home, proj
}

// #84: istovetnost, ne niz — dom, povezava ali stičišče nanj, mape nad njim in neobstoječa mapa so zavrnjeni.
func TestShareRefusalIdentity(t *testing.T) {
	home, proj := fakeHome(t)
	link := filepath.Join(t.TempDir(), "home")
	dirLink(t, home, link)
	for _, d := range []string{home, link, filepath.Dir(home), filepath.Dir(filepath.Dir(home)), filepath.Join(home, "missing")} {
		if shareRefusal(d) == "" {
			t.Errorf("%s allowed", d)
		}
	}
	for _, d := range []string{proj, filepath.Join(link, "proj")} {
		if why := shareRefusal(d); why != "" {
			t.Errorf("%s refused: %s", d, why)
		}
	}
}

// #84: mapa s seznama, dodana pred 0.5.25 in zdaj zavrnjena, ob uporabi ni deljena; veljavna je.
func TestSharedFolderForRechecks(t *testing.T) {
	home, proj := fakeHome(t)
	f := sharedFoldersFile()
	os.MkdirAll(filepath.Dir(f), 0o700)
	os.WriteFile(f, []byte(formatSharedFolders([]sharedFolder{{Path: filepath.Dir(home)}, {Path: proj}})), 0o600)
	if sharedFolderFor(filepath.Join(filepath.Dir(home), "other")) != nil {
		t.Error("parent of home used as a share")
	}
	if s := sharedFolderFor(filepath.Join(proj, "src")); s == nil || s.Path != proj {
		t.Errorf("project share not used: %v", s)
	}
}

// H1 (#84): deli se cwd sam; $PWD, ki je povezava (na Windows stičišče) na dom v projektu, ni deljen, tudi ne z
// MINIGW_SHARE=1.
func TestShareWantedRefusesHomeLinkCwd(t *testing.T) {
	home, proj := fakeHome(t)
	t.Setenv("MINIGW_SHARE", "")
	shareCommand([]string{"add", proj})
	if shareWanted(proj, false) == nil {
		t.Fatal("project not shared")
	}
	link := filepath.Join(proj, "home")
	dirLink(t, home, link)
	t.Chdir(link)
	t.Setenv("PWD", link)
	cwd, _ := os.Getwd()
	if shareWanted(cwd, false) != nil {
		t.Errorf("cwd %s (link to home) shared", cwd)
	}
	t.Setenv("MINIGW_SHARE", "1")
	if shareWanted(cwd, false) != nil {
		t.Errorf("cwd %s (link to home) shared with MINIGW_SHARE=1", cwd)
	}
}

// L6 (#84): koren, zamenjan med sejo (druga mapa na isti poti), zavrne vse posle: WebDAV in sync.
func TestShareRootReplacedRefused(t *testing.T) {
	root := filepath.Join(t.TempDir(), "p")
	os.Mkdir(root, 0o755)
	s := &shareClient{root: root, sync: true, synced: map[string]syncEntry{}, reported: map[string]bool{}}
	s.rootID, s.rootErr = fileIDOf(root)
	if !s.sameRoot() {
		t.Fatal("same root refused")
	}
	os.Rename(root, root+".old")
	os.Mkdir(root, 0o755)
	for _, m := range []string{"SYNC-PUSH", "SYNC-PULL"} {
		if r := s.handle(davJob{Method: m}, ""); r.Status != 403 {
			t.Errorf("%s on a replaced root: %d", m, r.Status)
		}
	}
	s.sync = false
	if s.handle(davJob{Method: "PROPFIND", Path: "/"}, "").Status != 403 {
		t.Error("PROPFIND on a replaced root accepted")
	}
}

// #84: mapa, zamenjana med preverbo (shareWanted) in start, ni deljena.
func TestShareRootSwappedBeforeStart(t *testing.T) {
	_, proj := fakeHome(t)
	t.Setenv("MINIGW_SHARE", "1")
	checked := shareWanted(proj, false)
	if checked == nil {
		t.Fatal("project not shared")
	}
	os.Rename(proj, proj+".old")
	os.Mkdir(proj, 0o755)
	s := &shareClient{root: proj, checked: checked, c: connection{URL: "http://127.0.0.1:1"}}
	s.start(0)
	if s.rootErr == nil || s.sameRoot() {
		t.Error("folder swapped after the check was shared")
	}
}
