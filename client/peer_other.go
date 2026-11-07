//go:build !linux && !windows

package main

import (
	"net"
	"os/exec"
)

// macOS: router je aplikacija Mini Gateway (PeerProcess); minigw serve na macOS je za razvoj, brez preverjanja.
func startAgent(cmd *exec.Cmd) error { return cmd.Start() }

func peerCheck(ours, peer *net.TCPAddr) error { return nil }
