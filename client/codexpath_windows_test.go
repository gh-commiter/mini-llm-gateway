//go:build windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// M1: razširjena pot za surovi CreateFile; trimFinalPrefix jo vrne nazaj.
func TestExtendedPath(t *testing.T) {
	for in, want := range map[string]string{
		`C:\a\..\b`: `\\?\C:\b`, `C:/a/b`: `\\?\C:\a\b`, `\\srv\share\x`: `\\?\UNC\srv\share\x`,
		`\\?\C:\x`: `\\?\C:\x`, `\\?\UNC\srv\s`: `\\?\UNC\srv\s`, `\\.\pipe\p`: `\\.\pipe\p`,
	} {
		if got := extendedPath(in); got != want {
			t.Errorf("extendedPath(%q) = %q, want %q", in, got, want)
		}
	}
	for _, p := range []string{`C:\b`, `\\srv\share\x`} {
		if got := trimFinalPrefix(extendedPath(p)); got != p {
			t.Errorf("round trip %q: %q", p, got)
		}
	}
}

// M1: pot nad 260 znaki (LongPathsEnabled=0 je privzeto) ostane dovoljena; stičišče pred globoko potjo ven ne.
func TestDavResolveLongPathAndJunction(t *testing.T) {
	base, _ := filepath.EvalSymlinks(t.TempDir())
	root, out := filepath.Join(base, "r"), filepath.Join(base, "out")
	seg := strings.Repeat("d", 50)
	deep := filepath.Join(seg, seg, seg, seg, seg, seg) // 306 znakov pod korenom
	if err := os.MkdirAll(filepath.Join(root, deep), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(out, deep), 0o755); err != nil {
		t.Fatal(err)
	}
	if len(filepath.Join(root, deep)) <= 260 {
		t.Fatal("path not long enough")
	}
	if _, err := finalPath(filepath.Join(root, deep)); err != nil {
		t.Fatalf("finalPath of a long path: %v", err)
	}
	rel := "/s/" + filepath.ToSlash(deep)
	for _, p := range []string{rel, rel + "/new.txt"} {
		if _, _, ok := davResolve(p, "/s", root); !ok {
			t.Errorf("long path refused: %d chars", len(p))
		}
	}
	if r := davHandle("PUT", rel+"/f.txt", nil, []byte("x"), root, "/s"); r.Status >= 300 {
		t.Errorf("PUT long path: %d", r.Status)
	}
	junction(t, out, filepath.Join(root, "j"))
	for _, p := range []string{"/s/j/" + filepath.ToSlash(deep), "/s/j/" + filepath.ToSlash(deep) + "/x"} {
		if _, _, ok := davResolve(p, "/s", root); ok {
			t.Errorf("escaped root through a junction before a long path: %d chars", len(p))
		}
	}
	if _, ok := syncResolve("j/"+filepath.ToSlash(deep)+"/x", root); ok {
		t.Error("sync: escaped through a junction before a long path")
	}
}

// L-a: kratko ime 8.3 v poti (odprto ime, FILE_NAME_OPENED) se zavrne.
func TestHasShortName(t *testing.T) {
	for p, want := range map[string]bool{`\\srv\share\PROGRA~1\x`: true, `C:\Users\ANA~1`: true, `C:\a~b\c`: false, `C:\Users\ana\x.txt`: false} {
		if hasShortName(p) != want {
			t.Errorf("%q", p)
		}
	}
}
