package main

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// Testni program teče tudi sam (GOOS=windows go test -c, navaden uporabnik brez Developer Mode): brez izvorne kode
// ob sebi in brez pravice za simbolne povezave. Ti pomočniki tak test preskočijo z razlogom, namesto da padejo.

// srcFile: datoteka izvorne mape (relativno na client/); preskok, ko testni program teče drugje.
func srcFile(t *testing.T, rel string) []byte {
	t.Helper()
	b, err := os.ReadFile(rel)
	if errors.Is(err, fs.ErrNotExist) {
		t.Skip("needs the client/ source tree (" + rel + "); run with go test or from the client folder")
	}
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// symlinkOrSkip: simbolna povezava; na Windows brez pravice (Developer Mode ali skrbnik) preskok.
func symlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skip("symlinks need Developer Mode or admin on Windows: " + err.Error())
		}
		t.Fatal(err)
	}
}

// dirLink: povezava na mapo, ki jo zmore vsak uporabnik: simbolna, na Windows brez pravice stičišče (mklink /J,
// kot ga naredi uporabnik ali pnpm). Pobeg skozi oboje mora biti zavrnjen.
func dirLink(t *testing.T, target, link string) {
	t.Helper()
	if os.Symlink(target, link) == nil {
		return
	}
	if runtime.GOOS != "windows" {
		t.Fatalf("symlink %s → %s failed", link, target)
	}
	if !filepath.IsAbs(target) { // stičišče hrani absolutno pot; relativna velja od mape povezave
		target = filepath.Join(filepath.Dir(link), target)
	}
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Fatalf("mklink /J: %v %s", err, out)
	}
}

// junction: na Windows stičišče (brez pravic); drugje preskok.
func junction(t *testing.T, target, link string) {
	t.Helper()
	if runtime.GOOS != "windows" {
		t.Skip("junctions are Windows only")
	}
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput(); err != nil {
		t.Fatalf("mklink /J: %v %s", err, out)
	}
}

// noExecBits: Windows nima bitov x (os.Stat jih izpelje iz končnice).
func noExecBits(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no exec bits")
	}
}
