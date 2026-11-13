package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Windows: DACL le za lastnika (protectScratch) na cilju, preden je v njem katerakoli datoteka.
func TestMoveScratchProtectsTargetFirstWindows(t *testing.T) {
	old, dst := filepath.Join(t.TempDir(), "old"), filepath.Join(t.TempDir(), "new")
	t.Cleanup(releaseScratch) // Windows: ročaj Scratch bi preprečil brisanje TempDir
	scratchFixture(t, old)
	called, src := false, old
	protectTarget = func(d string) error { // po preimenovanju (vira ni več) so datoteke že v cilju
		called = true
		if _, err := os.Lstat(src); err == nil {
			if ents, _ := os.ReadDir(d); len(ents) > 0 {
				t.Errorf("files in %s before the DACL", d)
			}
		}
		return protectScratch(d)
	}
	t.Cleanup(func() { protectTarget = protectScratch })
	if err := protectTarget(dst); err != nil {
		t.Fatal(err)
	}
	if err := scratchCopy(old, dst); err != nil {
		t.Fatal(err)
	}
	src = dst
	if _, err := moveScratchDir(dst, filepath.Join(t.TempDir(), "again")); err != nil || !called {
		t.Fatalf("called=%v err=%v", called, err)
	}
}

func TestMoveScratchNetworkRefusalLeavesNoTarget(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(map[bool]string{false: "no source", true: "source"}[existing], func(t *testing.T) {
			base := t.TempDir()
			old, dst := filepath.Join(base, "old"), filepath.Join(base, "new")
			if existing {
				scratchFixture(t, old)
			}
			scratchRemoteVolume = func(p string) bool { return p == dst }
			t.Cleanup(func() { scratchRemoteVolume = remoteVolume })
			if _, err := moveScratchDir(old, dst); !errors.Is(err, errScratchNetwork) {
				t.Fatalf("network refusal: %v", err)
			}
			if _, err := os.Lstat(dst); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("target remains: %v", err)
			}
			if existing {
				checkFixture(t, old)
			}
		})
	}
}
