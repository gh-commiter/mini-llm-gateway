//go:build !linux && !windows

package main

import (
	"net"
	"os"
	"os/exec"
)

// macOS in drugi: router je aplikacija Mini Gateway (PeerProcess); minigw serve tu je za razvoj, brez preverjanja.
func agentWrap(exe string, args []string, m agentMounts) (string, []string) { return exe, args }

// agentSandboxed: brez preverjenega sandboxa, ki skrije skrivnosti routerja: Codex in Grok le z izrecnim dovoljenjem (F4).
func agentSandboxed(cfgDir string) bool { return false }

func hardenRouter() {}

func agentReaperMain([]string) { os.Exit(2) }

func startAgent(cmd *exec.Cmd) error { return cmd.Start() }

func peerCheck(ours, peer *net.TCPAddr) error { return nil }
