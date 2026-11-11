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
	// koren datotečnega sistema z domom ali / (kot bazen btrfs ob @home) je zavrnjen; bind mount projekta
	// (devcontainer /workspaces/<projekt>, nosilec Docker) je dovoljen
	root, mnt := t.TempDir(), t.TempDir()
	if err := exec.Command("mount", "--bind", "/", root).Run(); err != nil {
		t.Skip("bind mount needs root")
	}
	defer exec.Command("umount", root).Run()
	if shareRefusal(root) == "" {
		t.Errorf("bind mount of / at %s allowed", root)
	}
	if err := exec.Command("mount", "--bind", proj, mnt).Run(); err != nil {
		t.Fatal(err)
	}
	defer exec.Command("umount", mnt).Run()
	if why := shareRefusal(mnt); why != "" {
		t.Errorf("bind mount of a project at %s refused: %s", mnt, why)
	}
}
