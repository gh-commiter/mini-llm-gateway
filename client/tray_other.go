//go:build !windows

package main

// Ikona v območju za obvestila je le na Windows; drugod router teče kot minigw serve (serve install).
func trayRun(string, bool) {
	fail("the tray icon is for Windows; run minigw serve here (minigw serve install starts it at sign-in)")
}
