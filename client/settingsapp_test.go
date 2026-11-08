package main

import "testing"

func TestShellCommandExeAndAppFlag(t *testing.T) {
	for _, c := range []struct {
		cmd, exe string
		app      bool
	}{
		{`"C:\Program Files\BraveSoftware\Brave-Browser\Application\brave.exe" --single-argument %1`, `C:\Program Files\BraveSoftware\Brave-Browser\Application\brave.exe`, true},
		{`"C:\Program Files (x86)\Microsoft\Edge\Application\MSEDGE.EXE" --single-argument %1`, `C:\Program Files (x86)\Microsoft\Edge\Application\MSEDGE.EXE`, true},
		{`C:\Tools\chrome.exe -- "%1"`, `C:\Tools\chrome.exe`, true},
		{`"C:\Program Files\Mozilla Firefox\firefox.exe" -osint -url "%1"`, `C:\Program Files\Mozilla Firefox\firefox.exe`, false},
		{`"unterminated`, ``, false},
	} {
		exe := shellCommandExe(c.cmd)
		if exe != c.exe || supportsAppFlag(exe) != c.app {
			t.Errorf("%q: got %q/%v, want %q/%v", c.cmd, exe, supportsAppFlag(exe), c.exe, c.app)
		}
	}
}
