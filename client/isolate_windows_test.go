//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runIsolated (L-d): ves tek na Windows brez prave namestitve in profila. Okolje (%APPDATA%, %LOCALAPPDATA%,
// %USERPROFILE%, HOME) kaže v lastno mapo pod pravo %LOCALAPPDATA% (ne %TEMP%: nonTempDir rabi Scratch zunaj začasnih
// map); vse, kar okolje prezre, je preusmerjeno: znane mape (SHGetKnownFolderPath: RoamingAppData, UserProgramFiles),
// domača mapa iz baze uporabnikov, register (HKCU Run, HKCU\Environment Path, namestitev MSI), Credential Manager
// (predpona credNS, po teku izbrisano), nastavitve git (isolateGit) ter pladenj in zagon procesov (pravi pladenj se ne zapre). Če česa ni mogoče
// preusmeriti: izhod 2 brez testov.
func runIsolated(m *testing.M) int {
	fail := func(why string) int {
		fmt.Fprintln(os.Stderr, "minigw tests: cannot isolate from the real profile ("+why+"); no tests run")
		return 2
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return fail("no %LOCALAPPDATA%")
	}
	d, err := os.MkdirTemp(base, "minigw-test-profile-")
	if err != nil {
		return fail(err.Error())
	}
	defer os.RemoveAll(d)
	roaming, local := filepath.Join(d, "Roaming"), filepath.Join(d, "Local")
	for _, p := range []string{roaming, local} {
		if err := os.MkdirAll(p, 0o700); err != nil {
			return fail(err.Error())
		}
	}
	for k, v := range map[string]string{"APPDATA": roaming, "LOCALAPPDATA": local, "USERPROFILE": d, "HOME": d} {
		os.Setenv(k, v)
	}
	if err := isolateGit(d); err != nil {
		return fail(err.Error())
	}
	knownConfigDirFn = func() (string, error) { return roaming, nil }
	folderLockDir = filepath.Join(d, "locks") // #82: pomočnik TestFolderLockAcrossProcesses dobi isto mapo (MINIGW_TEST_LOCK_DIR)
	userHomeFn = func() string { return d }
	userProgramsDir = func() string { return filepath.Join(local, "Programs") }
	reg := map[string]string{}
	runValueSet = func(n, v string) error { reg[n] = v; return nil }
	runValueGet = func(n string) (string, error) { return reg[n], nil }
	runValueDel = func(n string) { delete(reg, n) }
	envPath := `C:\Windows`
	envPathGet = func() (string, error) { return envPath, nil }
	envPathSet = func(v string) error { envPath = v; return nil }
	msiDir = func() string { return "" }
	msiPart = func(string) bool { return false }
	moveOnReboot = func(string) error { return errors.New("not in tests") }
	trayRunning = func() bool { return false }
	trayClose = func() error { return nil }
	startDetached = func(string, ...string) error { return errors.New("not in tests") }
	credNS = fmt.Sprintf("minigw-test-%d:", os.Getpid())
	defer credPurge(credNS)

	// Preverba: vse poti uporabnika so v d.
	cfg, err1 := os.UserConfigDir()
	cache, err2 := os.UserCacheDir()
	home, err3 := os.UserHomeDir()
	cb, err4 := configBase()
	if err := errors.Join(err1, err2, err3, err4); err != nil {
		return fail(err.Error())
	}
	for _, p := range []string{cfg, cache, home, cb, claudeDir(), userHomeFn(), userProgramsDir()} {
		if !strings.EqualFold(p, d) && !hasPathPrefix(p, d) {
			return fail(p + " is outside " + d)
		}
	}
	return m.Run()
}
