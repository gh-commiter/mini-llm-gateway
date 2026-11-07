//go:build !linux && !windows

package main

import (
	"net"
	"os/exec"
)

// macOS: router je aplikacija Mini Gateway (PeerProcess); minigw serve na macOS je za razvoj, brez preverjanja.
func agentWrap(exe string, args []string, cfgDir string) (string, []string) { return exe, args }

func startAgent(cmd *exec.Cmd) error { return cmd.Start() }

func peerCheck(ours, peer *net.TCPAddr) error { return nil }
