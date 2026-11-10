package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// Windows: škatla MXC (mxc.go) je vklopljena, ko preizkus ob zagonu minigw serve uspe; sicer ostane vse kot prej
// (agenti le z »Allow commands without a sandbox«).
var mxcState struct {
	mu     sync.Mutex
	ready  bool
	reason string
}

// windowsBox: agenti (razen Codex) tečejo v škatli MXC.
func windowsBox() bool {
	mxcState.mu.Lock()
	defer mxcState.mu.Unlock()
	return mxcState.ready
}

// windowsBuild: »26200.9711« iz registra (le za razlog na strani z nastavitvami; odloči preizkus).
func windowsBuild() string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SOFTWARE\Microsoft\Windows NT\CurrentVersion`, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	b, _, err := k.GetStringValue("CurrentBuild")
	if err != nil {
		return ""
	}
	if ubr, _, err := k.GetIntegerValue("UBR"); err == nil {
		return fmt.Sprintf("%s.%d", b, ubr)
	}
	return b
}

// sandboxInit: preizkus ob zagonu (pred strežbo, zato brez tekmovanja z zahtevami); uspeh vklopi škatlo in z njo
// git agenta v klonu (#70 velja le brez škatle).
func sandboxInit(scratch, cfgDir string, logf func(string, ...any)) {
	cleanGitWraps(filepath.Join(scratch, "gitexec"))
	old, _ := filepath.Glob(filepath.Join(scratch, "tmp", "mxc", "mxc-*.json")) // N1: zahteve prejšnjega procesa (žetoni)
	for _, p := range old {
		os.Remove(p)
	}
	why := ""
	if b := windowsBuild(); buildTooOld(b) { // pred preizkusom: jasen razlog
		why = "needs Windows 11 build 26100.9550 / 26200.9550 or later (KB5124010 or any later cumulative update); this computer has " + b
	} else {
		why = mxcProbe(wxcExe(), minigwExe(), scratch, cfgDir)
	}
	mxcState.mu.Lock()
	mxcState.ready, mxcState.reason = why == "", why
	mxcState.mu.Unlock()
	worktreeNeedsUnsandboxed = why != ""
	if why == "" {
		logf("agents run in the Windows sandbox (MXC %s)", mxcRelease)
		return
	}
	if b := windowsBuild(); b != "" && strings.Contains(why, "127.0.0.1") { // build le pri napaki loopback
		why += "; this computer has " + b
		mxcState.mu.Lock()
		mxcState.reason = why
		mxcState.mu.Unlock()
	}
	logf("Windows sandbox (MXC) not available: %s; agents run only with \"Allow commands without a sandbox\"", why)
}

// agentSandboxInfo: {"kind":"mxc","version":"1.0.1"} ali {"kind":"","reason":"…"} (status, stran z nastavitvami, TUI).
func agentSandboxInfo() map[string]string {
	mxcState.mu.Lock()
	defer mxcState.mu.Unlock()
	if mxcState.ready {
		return map[string]string{"kind": "mxc", "version": mxcRelease}
	}
	r := mxcState.reason
	if r == "" {
		r = "not checked (the gateway is not running)"
	}
	return map[string]string{"kind": "", "reason": r}
}

// wxcOpen (L1): branje z deljenjem le za branje: dokler je odprta, je ni mogoče zamenjati, spremeniti ali izbrisati
// (zagon jo odpre le za branje in izvajanje).
func wxcOpen(p string) (*os.File, error) {
	u, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(u, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(h), p), nil
}

// cleanGitWraps (#68): stare kopije ovoja git (gitexec\win-<druga zgoščena>) ob zagonu; trenutna ostane.
func cleanGitWraps(gw string) {
	keep := "win-" + selfHash()
	ents, _ := os.ReadDir(gw)
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), "win-") && e.Name() != keep {
			os.RemoveAll(filepath.Join(gw, e.Name()))
		}
	}
}
