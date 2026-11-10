package main

import (
	"os"
	"path/filepath"
	"testing"
)

// Windows: DACL le za lastnika (protectScratch) na cilju, preden je v njem katerakoli datoteka.
func TestMoveScratchProtectsTargetFirstWindows(t *testing.T) {
	old, dst := filepath.Join(t.TempDir(), "old"), filepath.Join(t.TempDir(), "new")
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
