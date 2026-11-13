//go:build !linux && !windows

package main

import (
	"net"
	"os"
	"os/exec"
)

// macOS in drugi: router je aplikacija Mini Gateway (PeerProcess); minigw serve tu je za razvoj, brez preverjanja.
func agentWrap(exe string, args []string, m agentMounts) (string, []string, func(), error) {
	if windowsBox() && !m.unboxed { // le testi (škatla MXC z lažnim wxc-exec, mxc_test.go)
		return boxWrap(exe, args, m)
	}
	return exe, args, func() {}, nil
}

func pinWrap(cfgDir, exe string, args, pins []string, logf func(string, ...any)) (string, []string) {
	return exe, args
}

// claudePinOK: pin (bwrap) je le na Linux.
func claudePinOK(cfgDir string, logf func(string, ...any)) bool { return false }

// watchGitMoves (N3c): le Linux (inotify); drugje runCLI preverja klon vsako sekundo (macOS serve je za razvoj, na
// Windows teče Claude v worktree le z »Allow commands without a sandbox«).
func watchGitMoves(dirs []string, moved func()) (stop func(), ok bool) { return nil, false }

// agentSandboxed: brez preverjenega sandboxa, ki skrije skrivnosti routerja: Codex in Grok le z izrecnim dovoljenjem (F4).
func agentSandboxed(cfgDir string) bool { return false }

func hardenRouter() {}

func agentReaperMain([]string) { os.Exit(2) }

// initReaper: le Linux (PID 1 v vsebniku).
func initReaper() {}

func startAgent(cmd *exec.Cmd) error { return cmd.Start() }

// stopAgentTree: Job teka je le na Windows.
func stopAgentTree(*exec.Cmd) error { return nil }

func peerCheck(ours, peer *net.TCPAddr) error { return nil }

// peerInTree: na macOS to preveri aplikacija (PeerProcess); minigw serve tu je za razvoj, brez preverjanja.
func peerInTree(ours, peer *net.TCPAddr, root int) bool { return true }

// sameUserPeer: na macOS to preveri aplikacija (PeerProcess); minigw serve tu brez preverjanja → vedno z žetonom.
func sameUserPeer(ours, peer *net.TCPAddr) bool { return false }

// confinedApp: le Linux in Windows (sameUserPeer).
func confinedApp(int) bool { return false }

// listenerIsOurs: na macOS je Foundry Local stvar aplikacije (Foundry.swift preveri uid); minigw serve tu je za razvoj.
func listenerIsOurs(port int) bool { return true }

func listenerExes(int) ([]string, bool) { return nil, false }

// bwrapReady: bwrap je le na Linuxu.
func bwrapReady(string) string { return "" }

func pinAllowedBinds(cfgDir string, args []string, folders checkedFolders, held []*os.File) ([]string, []*os.File) {
	return args, nil
}

func allowedClaudeWrap(cfgDir, name string, args []string, folders checkedFolders, held []*os.File, readOnly bool, logf func(string, ...any)) (string, []string, []*os.File) {
	return name, args, nil
}

func logBwrapMode(string, func(string, ...any)) {}
