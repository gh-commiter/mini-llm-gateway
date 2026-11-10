package main

import (
	"fmt"
	"sync"

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
	why := mxcProbe(wxcExe(), minigwExe(), scratch, cfgDir)
	mxcState.mu.Lock()
	mxcState.ready, mxcState.reason = why == "", why
	mxcState.mu.Unlock()
	worktreeNeedsUnsandboxed = why != ""
	if why == "" {
		logf("agents run in the Windows sandbox (MXC %s)", mxcRelease)
		return
	}
	if b := windowsBuild(); b != "" {
		why += " (Windows 11 26100.9278 / 26200.9278 or later needed, loopback .9550; this computer has " + b + ")"
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
