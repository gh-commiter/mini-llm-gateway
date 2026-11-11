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
