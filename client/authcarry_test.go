//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// sec-fix F6: what the carry-back refuses (the real login stays, the home gets the link again) and the refresh at run start.
func TestRestoreAuthRefusesWhatIsNotAFreshSmallJSONFileOfTheUser(t *testing.T) {
	real, scratch, other := t.TempDir(), t.TempDir(), t.TempDir()
	t.Setenv("CODEX_HOME", real)
	good := `{"tokens":"REAL"}`
	realAuth := real + "/auth.json"
	os.WriteFile(realAuth, []byte(good), 0o600)
	home, _, err := agentState(scratch, codexKind, "dev", nil)
	if err != nil {
		t.Fatal(err)
	}
	auth := home + "/auth.json"
	start := time.Now().Add(-time.Second)
	reset := func() { // the real login, linked again (the generation is recorded), then the home's link is gone as if the CLI had replaced it
		os.Remove(realAuth)
		os.WriteFile(realAuth, []byte(good), 0o600)
		os.Remove(auth)
		agentState(scratch, codexKind, "dev", nil)
		releaseHome(home)
		os.Remove(auth)
	}
	secret := other + "/target.json"
	os.WriteFile(secret, []byte(`{"x":"SECRET"}`), 0o600)
	linked := func() bool { l, _ := os.Readlink(auth); return l == realAuth }
	text := func(p string) string { b, _ := os.ReadFile(p); return string(b) }

	reset() // a planted symlink in the home: not read, not carried
	os.Symlink(secret, auth)
	restoreAuth(home, codexKind, start)
	if text(realAuth) != good {
		t.Fatal("symlink in the home was carried")
	}
	reset() // a symlink at the real name is neither followed nor replaced
	os.Remove(realAuth)
	os.Symlink(secret, realAuth)
	os.WriteFile(auth, []byte(`{"tokens":"NEW"}`), 0o600)
	restoreAuth(home, codexKind, start)
	if l, _ := os.Readlink(realAuth); l != secret || text(secret) != `{"x":"SECRET"}` {
		t.Fatal("a link at the real name was followed or replaced")
	}
	hard := other + "/hard.json"
	for name, make := range map[string]func(){
		"garbage": func() { os.WriteFile(auth, []byte("not json \x00\x01"), 0o600) },
		"array":   func() { os.WriteFile(auth, []byte("[1,2]"), 0o600) },
		"huge":    func() { os.WriteFile(auth, []byte(`{"a":"`+strings.Repeat("a", maxAuthBytes+10)+`"}`), 0o600) },
		"hard link": func() {
			os.Remove(hard)
			os.WriteFile(hard, []byte(good), 0o600)
			os.Link(hard, auth)
		},
		"stale": func() {
			os.WriteFile(auth, []byte(good), 0o600)
			old := time.Now().Add(-time.Hour)
			os.Chtimes(auth, old, old)
		},
	} {
		reset()
		make()
		restoreAuth(home, codexKind, start)
		if text(realAuth) != good {
			t.Errorf("%s: the real login changed", name)
		}
		if !linked() {
			t.Errorf("%s: the file was not dropped", name)
		}
	}
	// run start: a leftover config.toml and auth.json are the links again
	reset()
	os.WriteFile(real+"/config.toml", []byte("model = \"real\"\n"), 0o600)
	os.WriteFile(home+"/config.toml", []byte("model = \"planted\"\n"), 0o600)
	os.WriteFile(auth, []byte("not json"), 0o600)
	h2, _, err := agentState(scratch, codexKind, "dev", nil)
	if err != nil || h2 != home {
		t.Fatalf("%v %v", h2, err)
	}
	if text(home+"/config.toml") != "model = \"real\"\n" || !linked() {
		t.Fatalf("not refreshed: %q linked=%v", text(home+"/config.toml"), linked())
	}
	if _, err := os.Stat(filepath.Join(other, "config.toml")); err == nil {
		t.Fatal("wrote outside")
	}
}

// R2: two overlapping runs share a home: the login one CLI refreshed is carried back whichever order they start and end, and a real login
// that is newer than the one the link was made from stays.
func TestOverlappingRunsNeitherLoseARotatedLoginNorOverwriteANewerOne(t *testing.T) {
	real, scratch := t.TempDir(), t.TempDir()
	t.Setenv("CODEX_HOME", real)
	realAuth := real + "/auth.json"
	text := func(p string) string { b, _ := os.ReadFile(p); return string(b) }
	start := func() string { // a run starts: its home (shared), held
		h, _, err := agentState(scratch, codexKind, "dev", nil)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	end := func(h string) { restoreAuth(h, codexKind, time.Now().Add(-time.Minute)); releaseHome(h) }
	isLink := func(h string) bool { l, _ := os.Readlink(h + "/auth.json"); return l == realAuth }
	replace := func(h, tok string) {
		os.Remove(h + "/auth.json")
		os.WriteFile(h+"/auth.json", []byte(`{"tokens":"`+tok+`"}`), 0o600)
	}
	fresh := func() {
		os.WriteFile(realAuth, []byte(`{"tokens":"R0"}`), 0o600)
		os.Remove(scratchHome(t, scratch) + "/auth.json")
	}
	// order 1: A's CLI rotates the token, then B starts: its start carries the file back instead of unlinking it
	fresh()
	a := start()
	replace(a, "RA")
	b := start()
	if text(realAuth) != `{"tokens":"RA"}` || !isLink(b) {
		t.Fatalf("the start unlinked the refreshed login: %q", text(realAuth))
	}
	end(a)
	end(b)
	if text(realAuth) != `{"tokens":"RA"}` || !isLink(a) {
		t.Fatalf("after the ends: %q", text(realAuth))
	}
	// order 2: A ends before the next run starts
	fresh()
	a = start()
	replace(a, "RA2")
	end(a)
	b = start()
	end(b)
	if text(realAuth) != `{"tokens":"RA2"}` || !isLink(b) {
		t.Fatalf("order 2: %q", text(realAuth))
	}
	// a newer real login (another home's run) is not overwritten by the slower run, at its end or at the next start
	for _, viaStart := range []bool{false, true} {
		fresh()
		a = start()
		os.WriteFile(realAuth, []byte(`{"tokens":"NEWER"}`), 0o600)
		replace(a, "SLOW")
		if viaStart {
			end(start())
			releaseHome(a)
		} else {
			end(a)
		}
		if text(realAuth) != `{"tokens":"NEWER"}` || !isLink(a) {
			t.Fatalf("a newer real login was overwritten (viaStart=%v): %q", viaStart, text(realAuth))
		}
	}
	// the lifecycle of one real file is serialised: many starts and ends in parallel leave the login whole
	fresh()
	a = start()
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				h := start()
				releaseHome(h)
			} else {
				restoreAuth(a, codexKind, time.Now().Add(-time.Minute))
			}
		}(i)
	}
	wg.Wait()
	if text(realAuth) != `{"tokens":"R0"}` || !isLink(a) {
		t.Fatalf("after the parallel runs: %q", text(realAuth))
	}
}

// scratchHome: the (only) home of the device "dev" in scratch.
func scratchHome(t *testing.T, scratch string) string {
	h, _, err := agentState(scratch, codexKind, "dev", nil)
	if err != nil {
		t.Fatal(err)
	}
	releaseHome(h)
	return h
}
