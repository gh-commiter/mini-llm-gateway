package main

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
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

// Round 1: Scratch drugega prehoda, ki teče (njegov ročaj kot v protectScratch), je v seznamu »in use« in Take over
// ga zavrne (409), mapa ostane.
func TestScratchHeldSiblingNotTakenWindows(t *testing.T) {
	cfg, cur := offlineScratchConfig(t)
	other := filepath.Join(filepath.Dir(cur), "gw-bbbbbbbbbbbb")
	writeOwner(t, other, "gw-bbbbbbbbbbbb")
	h, err := openDir(other, true, func(mode os.FileMode, owner, me string, _ []scratchACE) error { return scratchDirOK(mode, owner, me) })
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(h) // drug prehod teče
	s, err := offlineServer(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if st, obj := s.adminDo("GET", "/general", nil); st != 200 || len(obj.(apiGeneral).ScratchOthers) != 1 ||
		obj.(apiGeneral).ScratchOthers[0].State != scratchInUse {
		t.Fatalf("listing: %d %+v", st, obj)
	}
	if st, obj := s.adminDo("POST", "/general/scratch/take", []byte(`{"name":"gw-bbbbbbbbbbbb"}`)); st != 409 {
		t.Fatalf("take of a held folder: %d %v", st, obj)
	}
	if scratchOwner(other) != "gw-bbbbbbbbbbbb" || scratchOwner(cur) != "gw-aaaaaaaaaaaa" {
		t.Fatal("folders changed")
	}
}
