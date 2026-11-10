package main

import (
	"os"
	"path/filepath"
	"testing"
)

// HPX360: ročaj mape Scratch prepreči preimenovanje, dokler prehod teče; scratchLocked to vidi kot »in use«; naša druga
// odpiranja (scratchFolderMine, ponovni protectScratch) med držanjem uspejo.
func TestScratchHoldBlocksRenameWindows(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "uros")
	t.Cleanup(releaseScratch)
	if err := protectScratch(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(dir, dir+"-renametest"); err == nil {
		t.Fatal("renamed while held")
	}
	if !scratchLocked(dir) {
		t.Fatal("not in use while held")
	}
	if !scratchFolderMine(dir) {
		t.Fatal("second open while held failed")
	}
	if err := protectScratch(dir); err != nil {
		t.Fatalf("protect again while held: %v", err)
	}
	releaseScratch()
	if scratchLocked(dir) {
		t.Fatal("in use after release")
	}
	if err := os.Rename(dir, dir+"-renametest"); err != nil {
		t.Fatalf("rename after release: %v", err)
	}
}
