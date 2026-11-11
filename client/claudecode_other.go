//go:build !linux && !darwin && !windows

package main

import "errors"

// procLookup: drugi sistemi verige ne berejo (claudeCaller: Claude Code, razen s terminalom na stdin).
func procLookup() procFunc {
	return func(int) (int, string, []string, error) { return 0, "", nil, errors.New("not supported") }
}
