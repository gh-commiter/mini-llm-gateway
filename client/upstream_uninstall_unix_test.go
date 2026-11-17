//go:build !windows

package main

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// `minigw uninstall --purge` also removes the credentials of upstream gateways (upstream:<name>), which are in no config to be found by name;
// the provider keys it does not list are left alone by that step.
func TestUninstallPurgeRemovesUpstreamCredentials(t *testing.T) {
	pluginHome(t)
	dir := filepath.Dir(defaultServeConfigPath())
	if err := writeKeys(dir, map[string]string{"upstream:hub": "secret-1", "upstream:team": "secret-2", "openrouter": "key"}); err != nil {
		t.Fatal(err)
	}
	steps, _, _ := uninstallSteps("linux", true)
	i := slices.IndexFunc(steps, func(s uninstallStep) bool { return strings.HasPrefix(s.what, "upstream gateway credentials") })
	if i < 0 || !strings.Contains(steps[i].what, "upstream:hub") || !strings.Contains(steps[i].what, "upstream:team") {
		t.Fatalf("no step for the upstream credentials: %v", steps)
	}
	// without --purge nothing about them (they are kept like the profiles)
	for _, s := range func() []uninstallStep { st, _, _ := uninstallSteps("linux", false); return st }() {
		if strings.HasPrefix(s.what, "upstream gateway credentials") {
			t.Fatal("removed without --purge")
		}
	}
	if err := steps[i].do(); err != nil {
		t.Fatal(err)
	}
	m, err := fileKeys(dir)
	if err != nil || len(m) != 1 || m["openrouter"] != "key" {
		t.Fatalf("%v %v", m, err)
	}
}
