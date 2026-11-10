//go:build windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// M1: ownedByMe zavrne junction (mklink /J, brez skrbniških pravic) namesto mape; mxcWrap ne da dovoljenja skozenj.
func TestOwnedByMeJunctionM1(t *testing.T) {
	scratch, target := t.TempDir(), t.TempDir()
	real, junction := filepath.Join(scratch, "real"), filepath.Join(scratch, "deps")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("cmd", "/c", "mklink", "/J", junction, target).CombinedOutput(); err != nil {
		t.Fatalf("mklink /J: %v %s", err, out)
	}
	if !ownedByMe(real) {
		t.Error("own real folder not owned by me")
	}
	if ownedByMe(junction) {
		t.Error("junction accepted as an own folder")
	}
	if got := scratchLink(filepath.Join(junction, "tmp"), scratch); got != junction {
		t.Errorf("scratchLink = %q, want %q", got, junction)
	}
	if _, _, _, err := mxcWrap("wxc", `C:\Windows\System32\cmd.exe`, nil, agentMounts{cfgDir: t.TempDir(), hide: scratch, rw: []string{junction}}); err == nil {
		t.Error("mxcWrap granted through a junction")
	}
}
