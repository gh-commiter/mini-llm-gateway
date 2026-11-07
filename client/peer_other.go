//go:build !linux && !windows

package main

import (
	"net"
	"os"
	"os/exec"
)

// macOS: router je aplikacija Mini Gateway (PeerProcess); minigw serve na macOS je za razvoj, brez preverjanja.
func agentWrap(exe string, args []string, m agentMounts) (string, []string) { return exe, args }

func agentSandboxed(cfgDir string) bool { return true }

func hardenRouter() {}

func agentReaperMain([]string) { os.Exit(2) }

func startAgent(cmd *exec.Cmd) error { return cmd.Start() }

func peerCheck(ours, peer *net.TCPAddr) error { return nil }
