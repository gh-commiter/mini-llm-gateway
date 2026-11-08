package main

import "strings"

// Čiste pomožne funkcije za odpiranje nastavitev v lastnem oknu (Windows, tray_windows.go); brez oznake gradnje, da jih
// preizkusi tudi go test na drugih sistemih.

// shellCommandExe: pot do programa iz ukaza lupine (HKCR\<ProgId>\shell\open\command), z narekovaji ali brez.
func shellCommandExe(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	if strings.HasPrefix(cmd, `"`) {
		if i := strings.Index(cmd[1:], `"`); i >= 0 {
			return cmd[1 : i+1]
		}
		return ""
	}
	if i := strings.Index(strings.ToLower(cmd), ".exe"); i >= 0 {
		return cmd[:i+4]
	}
	return ""
}

// supportsAppFlag: Chromiumov brskalnik z zastavico --app=URL (okno brez zavihkov).
func supportsAppFlag(exe string) bool {
	if i := strings.LastIndexAny(exe, `\/`); i >= 0 {
		exe = exe[i+1:]
	}
	switch strings.ToLower(exe) {
	case "msedge.exe", "chrome.exe", "brave.exe", "vivaldi.exe":
		return true
	}
	return false
}
