//go:build !windows

package main

import "path/filepath"

// codexFinalPath: le Windows (stičišča); drugje zadošča filepath.EvalSymlinks.
func codexFinalPath(string) string { return "" }

// finalPath: kanonična pot za preverbo »ostane v korenu« (na Windows tudi skozi stičišča).
func finalPath(p string) (string, error) { return filepath.EvalSymlinks(p) }
