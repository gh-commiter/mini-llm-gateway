//go:build !windows

package main

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestGatewayHintSharedCases(t *testing.T) {
	var fx struct {
		Cases []struct {
			Name, Message string
			Status        int
			Profile, URL  string
			Profiles      int
			Out           string
		}
	}
	readFixture(t, "gateway-hint.json", &fx)
	for _, c := range fx.Cases {
		if got := gatewayHintText(c.Message, c.Status, c.Profile, c.URL, c.Profiles); got != c.Out {
			t.Errorf("%s: %q", c.Name, got)
		}
	}
	if e := (gwStatusError{errors.New("x"), 404}); !errors.As(error(e), new(gwStatusError)) {
		t.Fatal("not unwrappable")
	}
}

// MINIGW_PROFILE=<name> is the environment form of --minigw-profile: an unknown name is the same error as with the flag, the
// flag wins over it, and a refusal names the profile it came from when there are several.
func TestMinigwProfileEnvironment(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	saveProfile("mini", connection{URL: "http://127.0.0.1:1", Token: "t"})
	saveProfile("win", connection{URL: "http://127.0.0.1:2", Token: "t"})
	if got := withGatewayHint(gatewayError(404, nil, nil)); !strings.Contains(got, "not found or not visible\nhint: this answer came from profile ") || !strings.Contains(got, "--minigw-profile <name>") {
		t.Fatalf("%q", got)
	}
	if got := withGatewayHint(gatewayError(429, nil, nil)); strings.Contains(got, "hint:") {
		t.Fatalf("%q", got)
	}
	run := func(env []string, args ...string) string {
		cmd := exec.Command(os.Args[0], args...)
		cmd.Env = append(os.Environ(), append([]string{"MINIGW_TEST_MAIN=1", "HOME=" + home, "XDG_CONFIG_HOME=" + filepath.Join(home, ".config"), "APPDATA=" + home, "USERPROFILE=" + home}, env...)...)
		out, _ := cmd.CombinedOutput()
		return string(out)
	}
	byFlag := run(nil, "project", "list", "--minigw-profile", "nope")
	byEnv := run([]string{"MINIGW_PROFILE=nope"}, "project", "list")
	if !strings.Contains(byFlag, "no profile nope (minigw profiles lists them)") || byEnv != byFlag {
		t.Fatalf("flag %q, environment %q", byFlag, byEnv)
	}
	if got := run([]string{"MINIGW_PROFILE=nope"}, "project", "list", "--minigw-profile", "win"); strings.Contains(got, "no profile") {
		t.Fatalf("the environment beat the flag: %q", got)
	}
	if got := run([]string{"MINIGW_PROFILE=win"}, "project", "list"); strings.Contains(got, "no profile") {
		t.Fatalf("a known profile from the environment was refused: %q", got)
	}
}
