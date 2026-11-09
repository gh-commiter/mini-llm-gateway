//go:build !linux

package main

import "os"

// boxGit: le Linux (bwrap); drugje finalize ugnezdenega klona dela s pričakovano potjo (macOS serve je za razvoj, na
// Windows agenti v načinu worktree že tečejo brez peskovnika z vsemi pravicami uporabnika).
func boxGit(dir *os.File, mount, repo, tmp, exe string, args []string) (string, []string, []*os.File, bool) {
	return "", nil, nil, false
}

func finalizeBwrap() string { return "" }
