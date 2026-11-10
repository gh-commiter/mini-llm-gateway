//go:build !windows

package main

import "os/exec"

func hideConsole(*exec.Cmd) {}

func refreshPath() {}

// startClient: Job odjemalca je le na Windows (peer_windows.go); drugje kot prej.
func startClient(cmd *exec.Cmd) error { return cmd.Start() }
