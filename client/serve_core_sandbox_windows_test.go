//go:build windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// 0.6.0 task 7a (#106), Windows: a junction (needs no privilege) from an allowed folder to the core folder or to the
// config folder is refused as an allowed folder, and the box request denies the config folder by its final path.
func TestJunctionToTheCoreFolderIsRefused(t *testing.T) {
	base := realOf(t, t.TempDir())
	cfg, work := filepath.Join(base, "cfg"), filepath.Join(base, "work")
	for _, d := range []string{filepath.Join(cfg, coreDirName), work} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for name, target := range map[string]string{"core": filepath.Join(cfg, coreDirName), "cfg": cfg} {
		j := filepath.Join(work, name)
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", j, target).CombinedOutput(); err != nil {
			t.Fatalf("mklink: %v %s", err, out)
		}
		if _, err := checkFolder(j, cfg, false); err == nil {
			t.Errorf("a junction to %s is allowed as an agent folder", target)
		}
		// through the junction the deny list names the final path (the box matches final paths)
		viaJunction := scratchPaths(j)
		if fin, err := finalPath(j); err != nil || len(viaJunction) < 2 || viaJunction[1] != fin {
			t.Errorf("deny list of %s: %v (final %q, %v)", j, viaJunction, fin, err)
		}
	}
}
