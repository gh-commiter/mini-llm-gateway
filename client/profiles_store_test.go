//go:build !windows

package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// #17: prejšnja edina povezava je profil »default«; drugi ob njej; izbira privzetega; --minigw-profile.
func TestProfilesStoreAndMigration(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	old := profileFlag
	t.Cleanup(func() { profileFlag = old })

	// Obstoječa namestitev: le connection.json.
	os.MkdirAll(filepath.Dir(storePath()), 0o700)
	os.WriteFile(storePath(), []byte(`{"url":"http://mini.local:8765","token":"t1","device":"dev"}`), 0o600)
	if n := profileNames(); !slices.Equal(n, []string{"default"}) {
		t.Fatalf("migration: %v", n)
	}
	if ps := loadProfiles(); len(ps) != 1 || ps[0].name != "default" || ps[0].conn.Token != "t1" {
		t.Fatalf("default: %+v", ps)
	}
	saveProfile("win", connection{URL: "http://hpx:8765", Token: "t2", Device: clientDevice()})
	os.WriteFile(filepath.Join(filepath.Dir(storePath()), "connections", "Bad Name.json"), []byte("{}"), 0o600)
	if n := profileNames(); !slices.Equal(n, []string{"default", "win"}) {
		t.Fatalf("names: %v", n)
	}
	if st, _ := os.Stat(profilePath("win")); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode())
	}
	if c := loadProfile("win"); c == nil || c.Device == nil || *c.Device != "dev" { // en ključ naprave
		t.Fatalf("win: %+v", c)
	}
	// Ključ naprave prenesen v svojo datoteko (0600); ostane, ko ni več nobenega profila (disconnect).
	if st, err := os.Stat(deviceFile()); err != nil || st.Mode().Perm() != 0o600 || deviceLoad() != "dev" {
		t.Fatalf("device key not migrated: %v", err)
	}
	if ps := loadProfiles(); ps[0].name != "default" || ps[1].name != "win" {
		t.Fatalf("order: %+v", ps)
	}
	profilesCommand([]string{"default", "win"})
	if ps := loadProfiles(); ps[0].name != "win" || ps[1].name != "default" {
		t.Fatalf("default win: %+v", ps)
	}
	profileFlag = "default"
	if ps := loadProfiles(); len(ps) != 1 || ps[0].name != "default" {
		t.Fatalf("--minigw-profile: %+v", ps)
	}
	profileFlag = ""
	storeDelete("win")
	if ps := loadProfiles(); len(ps) != 1 || ps[0].name != "default" { // izbrani privzeti ne obstaja več
		t.Fatalf("after delete: %+v", ps)
	}
	// L1: neveljavno ime ne doseže datoteke izven connections/ (npr. ../connection = privzeti profil).
	storeDelete("../connection")
	if c := loadProfile("../connection"); c != nil || storeSave("../x", []byte("{}")) == nil {
		t.Fatal("invalid name reached the store")
	}
	if _, err := os.Stat(storePath()); err != nil {
		t.Fatal("../connection deleted the default profile")
	}
	for _, n := range []string{"", "Win", "-x", "a b", "a/b", "..", strings.Repeat("a", 33), "x@y"} {
		if validProfile(n) {
			t.Fatalf("valid: %q", n)
		}
	}
}

func TestTrustedGroup(t *testing.T) {
	if !trustedGroup("darwin", 80, "") || trustedGroup("darwin", 20, "") || trustedGroup("darwin", 12345, "") {
		t.Error("darwin: only admin trusted (not staff or other groups)")
	}
	if trustedGroup("linux", 80, "") && os.Getgid() != 80 {
		t.Error("linux: a group that is not the user's primary group trusted")
	}
	u, err := user.Current()
	g, gerr := user.LookupGroupId(strconv.Itoa(os.Getgid()))
	if runtime.GOOS != "linux" || err != nil || gerr != nil || g.Name != u.Username {
		return // brez zasebne skupine uporabnika
	}
	if !trustedGroup("linux", uint32(os.Getgid()), g.Name+":x:"+g.Gid+":\n") {
		t.Error("linux: user private group not trusted")
	}
	if trustedGroup("linux", uint32(os.Getgid()), g.Name+":x:"+g.Gid+":mallory\n") {
		t.Error("linux: user private group with another member trusted")
	}
}

// minigw reconnect: nov žeton, naslov in ključ naprave ostaneta; ključ naprave preživi brisanje vseh profilov.
func TestReconnectAndDeviceKeySurvivesDisconnect(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	old := profileFlag
	t.Cleanup(func() { profileFlag = old })
	profileFlag = ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer new" {
			w.WriteHeader(401)
			return
		}
		w.Write([]byte(`{"providers":{},"data":[]}`))
	}))
	defer srv.Close()
	saveProfile(defaultProfile, connection{URL: srv.URL, Token: "old", Device: clientDevice()})
	dev := *loadProfile(defaultProfile).Device
	if !reconnectCommand([]string{"--token", "new"}) {
		t.Fatal("reconnect: not connected")
	}
	if c := loadProfile(defaultProfile); c == nil || c.Token != "new" || c.URL != srv.URL || c.Key != nil || *c.Device != dev {
		t.Fatalf("after reconnect: %+v", c)
	}
	storeDelete(defaultProfile)
	if len(profileNames()) != 0 || *clientDevice() != dev {
		t.Fatal("device key lost with the last profile")
	}
	steps, kept := uninstallSteps("linux", false)
	if !slices.Contains(kept, "device key ("+storeName+")") {
		t.Fatalf("kept %v", kept)
	}
	if steps, _ = uninstallSteps("linux", true); !slices.ContainsFunc(steps, func(s uninstallStep) bool { return s.what == "device key ("+storeName+")" }) {
		t.Fatal("purge does not list the device key")
	}
}

func TestNewToken(t *testing.T) {
	k := "oldkey"
	for line, want := range map[string]string{"t": "t oldkey", "t#n": "t n", "": "!", "#": "!", "#n": "!", "t#": "!"} {
		tok, key, ok := newToken(line, &k)
		got := "!"
		if ok {
			got = tok + " " + *key
		}
		if got != want {
			t.Errorf("%q: %q, want %q", line, got, want)
		}
	}
}
