//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"strings"
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
	reset := func() { os.Remove(realAuth); os.WriteFile(realAuth, []byte(good), 0o600); os.Remove(auth) }
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
	os.WriteFile(auth, []byte("{}"), 0o600)
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
