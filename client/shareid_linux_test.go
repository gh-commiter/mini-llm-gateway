package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// #84: / in /home (ko je dom pod njo) sta zavrnjena; bind mount (le kot root) je koren priklopa.
func TestShareRefusalLinux(t *testing.T) {
	dirs := []string{"/"}
	if home, _ := os.UserHomeDir(); strings.HasPrefix(home, "/home/") {
		dirs = append(dirs, "/home")
	}
	for _, d := range dirs {
		if shareRefusal(d) == "" {
			t.Errorf("%s allowed", d)
		}
	}
	home, proj := fakeHome(t)
	if shareRefusal(filepath.Dir(home)) == "" {
		t.Errorf("%s (contains home) allowed", filepath.Dir(home))
	}
	mnt := t.TempDir()
	if err := exec.Command("mount", "--bind", proj, mnt).Run(); err != nil {
		t.Skip("bind mount needs root")
	}
	defer exec.Command("umount", mnt).Run()
	if shareRefusal(mnt) == "" || shareRefusal(filepath.Join(mnt, ".")) == "" {
		t.Errorf("bind mount %s allowed", mnt)
	}
}
