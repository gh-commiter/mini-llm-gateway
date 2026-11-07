//go:build !linux

package main

// killInside: le Linux (Windows: agenti so v Jobu in tečejo kot uporabnik; macOS: minigw serve le za razvoj).
func killInside(string) {}
