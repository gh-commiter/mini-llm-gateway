//go:build !windows

package main

// codexFinalPath: le Windows (stičišča); drugje zadošča filepath.EvalSymlinks.
func codexFinalPath(string) string { return "" }
