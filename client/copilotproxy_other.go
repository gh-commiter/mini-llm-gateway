//go:build !linux

package main

import "os/exec"

// proxyDies: posrednik D-Bus je le na Linuxu.
func proxyDies(*exec.Cmd) {}
