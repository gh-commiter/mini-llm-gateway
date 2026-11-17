package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// An option that a command's own help text names must not be answered with "unknown option": the real `minigw gw ...`
// (the test binary as minigw, its flag parser and dispatch, not the layer below) gets every advertised --flag. There is
// no gateway, so the command ends in another error; only the parser's refusal matters.
func TestGwOptionsOfTheHelpAreAccepted(t *testing.T) {
	home := t.TempDir()
	flagRe := regexp.MustCompile(`--([a-z][a-z-]*)`)
	// the control: the parser does refuse an option nobody advertises
	ctl := exec.Command(os.Args[0], "gw", "usage", "--no-such-option")
	ctl.Env = append(os.Environ(), "MINIGW_TEST_MAIN=1", "HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"), "APPDATA="+home, "USERPROFILE="+home)
	if out, _ := ctl.CombinedOutput(); !strings.Contains(string(out), "not defined") {
		t.Fatalf("the control option was not refused (the test would prove nothing): %s", out)
	}
	valueless := map[string]bool{"json": true, "csv": true, "legacy": true, "history": true, "explain": true, "yes": true, "dry-run": true, "with-private": true}
	for _, c := range []struct{ cmd, help string }{
		{"usage", usageReportHelp}, {"usage-events", lineOf(coreHelp, "gw usage-events")}, {"project-sessions", lineOf(coreHelp, "gw project-sessions")},
	} {
		flags := map[string]bool{}
		for _, m := range flagRe.FindAllStringSubmatch(c.help, -1) {
			flags[m[1]] = true
		}
		if len(flags) < 4 {
			t.Fatalf("%s: found only %v in its help", c.cmd, flags)
		}
		for f := range flags {
			args := []string{"gw", c.cmd, "--" + f}
			if !valueless[f] {
				args = append(args, "x")
			}
			cmd := exec.Command(os.Args[0], args...)
			cmd.Env = append(os.Environ(), "MINIGW_TEST_MAIN=1", "HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"), "APPDATA="+home, "USERPROFILE="+home)
			out, _ := cmd.CombinedOutput()
			if s := string(out); strings.Contains(s, "not defined") || strings.Contains(s, "unknown option") {
				t.Errorf("minigw %s: the option --%s of its help is refused: %s", strings.Join(args, " "), f, strings.SplitN(s, "\n", 2)[0])
			}
		}
	}
}

// lineOf: the line of a multi-line help that holds key.
func lineOf(help, key string) string {
	for _, l := range strings.Split(help, "\n") {
		if strings.Contains(l, key) {
			return l
		}
	}
	return ""
}
