//go:build !windows

package main

import "os"

// fixedDrives: le Windows.
func fixedDrives() ([]string, string) { return nil, "" }

// windowsBox: škatla MXC (#68) je le na Windows; spremenljivka za teste (pot škatle z lažnim wxc-exec na Macu).
var windowsBox = func() bool { return false }

// sandboxInit: le Windows (preizkus MXC ob zagonu minigw serve).
func sandboxInit(scratch, cfgDir string, logf func(string, ...any)) {}

// sandboxRewarn: le Windows (opozorilo škatle po premiku Scratch).
func sandboxRewarn(string) {}

// agentSandboxInfo: vrsta peskovnika agentov za status (Windows: mxc); drugje prazno.
func agentSandboxInfo() map[string]string { return nil }

// wxcOpen: drugje navadno odpiranje (le testi z lažnim wxc-exec).
func wxcOpen(p string) (*os.File, error) { return os.Open(p) }
