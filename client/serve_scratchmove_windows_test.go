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
	called := false
	protectTarget = func(d string) error {
		if ents, _ := os.ReadDir(d); len(ents) > 0 {
			t.Errorf("files in %s before the DACL", d)
		}
		called = true
		return protectScratch(d)
	}
	t.Cleanup(func() { protectTarget = protectScratch })
	if err := protectTarget(dst); err != nil {
		t.Fatal(err)
	}
	if err := scratchCopy(old, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := moveScratchDir(dst, filepath.Join(t.TempDir(), "again")); err != nil || !called {
		t.Fatalf("called=%v err=%v", called, err)
	}
}
