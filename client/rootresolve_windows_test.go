package main

import "testing"

// L2: a link to a UNC or device path must not make the process connect to that host.
func TestRealTargetNeverResolvesUNC(t *testing.T) {
	calls := 0
	old := finalPathFn
	finalPathFn = func(p string) (string, error) { calls++; return old(p) }
	defer func() { finalPathFn = old }()
	bases := []string{`C:\share`}
	for _, target := range []string{`\\attacker\share\x`, `\\?\UNC\attacker\share\x`, `D:\other`} {
		if got := realTarget(trimFinalPrefix(target), bases); got != trimFinalPrefix(target) {
			t.Fatalf("%s resolved to %s", target, got)
		}
	}
	if calls != 0 {
		t.Fatalf("finalPath called %d times for targets off the root's volume", calls)
	}
	realTarget(`C:\Windows`, bases)
	if calls == 0 {
		t.Fatal("a target on the root's own drive is not resolved")
	}
}
