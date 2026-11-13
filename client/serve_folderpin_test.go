package main

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestAllowedFolderSnapshotAfterLinkSwap(t *testing.T) {
	for _, parent := range []bool{false, true} {
		t.Run(map[bool]string{false: "folder", true: "parent"}[parent], func(t *testing.T) {
			base := realOf(t, t.TempDir())
			first, second, alias := filepath.Join(base, "first"), filepath.Join(base, "second"), filepath.Join(base, "alias")
			for _, p := range []string{first, second} {
				if err := os.MkdirAll(filepath.Join(p, "child"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			dirLink(t, first, alias)
			configured, target := alias, first
			if parent {
				configured = filepath.Join(alias, "child")
				target = filepath.Join(first, "child")
			}
			s, _ := testServer(t, nil)
			c := s.conf()
			c.AllowedFolders = []string{configured}
			if _, err := checkAgent(&c, s.cfgDir, []string{configured}); err != nil {
				t.Fatal(err)
			}
			checked := s.checkedAgentFolders(c, base)
			if runtime.GOOS == "windows" { // #93: names stay (mapped drives); the launch check catches the swap
				target = configured
			}
			if !slices.Equal(checked.paths(), []string{target}) {
				t.Fatalf("checked: %v", checked)
			}
			// Retarget after validation, before permissions or sandbox construction.
			if err := os.Remove(alias); err != nil {
				t.Fatal(err)
			}
			dirLink(t, second, alias)
			if runtime.GOOS == "windows" {
				if err := checked.verify(); err == nil {
					t.Fatal("retargeted junction accepted at launch")
				}
				return
			}
			args := strings.Join(s.agentArgs(base, false, checked.paths()), " ")
			if strings.Contains(args, alias) || strings.Contains(args, second) || !strings.Contains(args, "--add-dir "+target) {
				t.Fatalf("permissions followed changed link: %s", args)
			}
			request := mxcRequestFor(mxcSpec{rw: checked.paths()})
			if !slices.Equal(request.Filesystem.ReadWrite, []string{target}) {
				t.Fatalf("MXC rules followed changed link: %v", request.Filesystem)
			}
			held, err := checked.open()
			if err != nil {
				t.Fatal(err)
			}
			defer closeFolders(held)
			if err := checked.verify(); err != nil {
				t.Fatal(err)
			}
			// Replacing the checked target itself must fail the identity check.
			if err := os.Rename(target, target+"-old"); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(target, 0700); err != nil {
				t.Fatal(err)
			}
			if err := checked.verify(); err == nil {
				t.Fatal("replacement identity accepted")
			}
			other, err := checked.open()
			closeFolders(other)
			if err == nil {
				t.Fatal("replacement opened as checked target")
			}
		})
	}
}
