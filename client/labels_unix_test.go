//go:build !windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The whole command: whatever the labels are, claude gets exactly the user's arguments (no --minigw-* label flag), its
// exit code is minigw's, an unusable value costs exactly one warning line, and a usable one never prints anything.
func TestMinigwLabelsNeverChangeTheRun(t *testing.T) {
	dir, log := fakeClaudeDir(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	os.MkdirAll(filepath.Dir(storePath()), 0o700) // a connected profile (never contacted: the fake claude ends the run)
	os.WriteFile(storePath(), []byte(`{"url":"http://127.0.0.1:1","token":"t","device":"dev"}`), 0o600)
	run := func(env []string, args ...string) (stderr string, code int, claudeArgs string) {
		os.Remove(log)
		cmd := minigwCmd(dir, log, append([]string{"HOME=" + home, "XDG_CONFIG_HOME=" + home, "APPDATA=" + home, "USERPROFILE=" + home, "MINIGW_TEST_CLAUDE_EXIT=7"}, env...), args...)
		var se strings.Builder
		cmd.Stderr = &se
		err := cmd.Run()
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(log)
		return se.String(), code, string(b)
	}
	cases := []struct {
		name    string
		env     []string
		args    []string
		warning int
		headers []string // label headers the gateway receives (the client's side of the labels)
		absent  []string
	}{
		{"none", nil, []string{"-p", "x"}, 0, nil, []string{"x-claude-mini-phase", "x-claude-mini-activity", "x-claude-mini-task"}},
		{"all three valid", nil, []string{"--minigw-phase", "check", "--minigw-activity=Bugfix", "--minigw-task", "#149", "-p", "x"}, 0,
			[]string{"x-claude-mini-phase: check", "x-claude-mini-activity: bugfix", "x-claude-mini-task: #149", "x-claude-mini-task-source: given"}, nil},
		{"an unknown phase word", nil, []string{"--minigw-phase=remediate", "-p", "x"}, 0, []string{"x-claude-mini-phase: remediate"}, nil},
		{"an unsafe value", nil, []string{"--minigw-phase", "not a token!", "-p", "x"}, 1, nil, []string{"x-claude-mini-phase"}},
		{"a value that looks like another flag", nil, []string{"--minigw-task", "x --minigw-work=direct", "-p", "x"}, 1, nil, []string{"x-claude-mini-task", "x-claude-mini-work"}},
		{"a value with a command", []string{"MINIGW_PHASE=x; rm -rf /", "MINIGW_ACTIVITY=ok"}, []string{"-p", "x"}, 1, []string{"x-claude-mini-activity: ok"}, []string{"x-claude-mini-phase", "x-claude-mini-work"}},
		{"environment, unsafe", []string{"MINIGW_ACTIVITY=bad value", "MINIGW_TASK=ok/1"}, []string{"-p", "x"}, 1, []string{"x-claude-mini-task: ok/1"}, []string{"x-claude-mini-activity"}},
	}
	baseline := ""
	for _, c := range cases {
		stderr, code, got := run(c.env, c.args...)
		if code != 7 {
			t.Errorf("%s: exit code %d, claude's is 7 (stderr %q)", c.name, code, stderr)
		}
		got = regexp.MustCompile(`--session-id [0-9a-f-]{36}`).ReplaceAllString(got, "--session-id ID")
		if baseline == "" {
			baseline = got // the "none" case: what minigw itself adds to the user's -p x
		}
		if got != baseline || strings.Contains(got, "--minigw-") || !strings.HasSuffix(got, "-p x") {
			t.Errorf("%s: claude got %q, want %q", c.name, got, baseline)
		}
		hb, _ := os.ReadFile(log + ".headers")
		for _, h := range c.headers {
			if !strings.Contains(string(hb), h) {
				t.Errorf("%s: header %q missing from %q", c.name, h, hb)
			}
		}
		for _, h := range c.absent {
			if strings.Contains(string(hb), h) {
				t.Errorf("%s: header %q present in %q", c.name, h, hb)
			}
		}
		os.Remove(log + ".headers")
		if n := strings.Count(stderr, "ignored: "); n != c.warning || strings.Count(strings.TrimSpace(stderr), "\n")+1 > max(c.warning, 1) && strings.TrimSpace(stderr) != "" {
			t.Errorf("%s: warnings %d, want %d: %q", c.name, n, c.warning, stderr)
		}
	}
}
