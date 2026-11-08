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

// sameUserPeer: na macOS to preveri aplikacija (PeerProcess); minigw serve tu brez preverjanja → vedno z žetonom.
func sameUserPeer(ours, peer *net.TCPAddr) bool { return false }

// listenerIsOurs: na macOS je Foundry Local stvar aplikacije (Foundry.swift preveri uid); minigw serve tu je za razvoj.
func listenerIsOurs(port int) bool { return true }

func listenerExes(int) ([]string, bool) { return nil, false }

// bwrapReady: bwrap je le na Linuxu.
func bwrapReady(string) string { return "" }
