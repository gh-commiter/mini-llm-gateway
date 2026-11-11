package main

import (
	"os"
	"path/filepath"
	"strings"
)

// isolateGit: git testov brez uporabnikovih nastavitev (globalne, sistemske, iz okolja: GIT_CONFIG_PARAMETERS ali
// GIT_CONFIG_COUNT, ki jih da npr. okolje agenta prehoda); nastavitve, ki jih test rabi, poda sam. dir: prazna globalna.
func isolateGit(dir string) error {
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); strings.HasPrefix(strings.ToUpper(k), "GIT_CONFIG") {
			os.Unsetenv(k)
		}
	}
	global := filepath.Join(dir, "gitconfig")
	if err := os.WriteFile(global, nil, 0o600); err != nil {
		return err
	}
	os.Setenv("GIT_CONFIG_GLOBAL", global)
	os.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	return nil
}
