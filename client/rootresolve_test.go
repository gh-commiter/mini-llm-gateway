package main

import (
	"path/filepath"
	"testing"
)

// L2: a target on the root's own volume is still resolved (a link made with another name of the share).
func TestRealTargetResolvesOwnVolume(t *testing.T) {
	calls := 0
	old := finalPathFn
	finalPathFn = func(p string) (string, error) { calls++; return old(p) }
	defer func() { finalPathFn = old }()
	dir := t.TempDir()
	realTarget(filepath.Join(dir, "missing", "x"), []string{dir})
	if calls == 0 {
		t.Fatal("a target on the root's own volume is not resolved")
	}
}
