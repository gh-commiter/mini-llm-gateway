package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// Every Go module linked into a shipped binary must have an entry (module and version) in THIRD-PARTY-NOTICES.md,
// which scripts/third-party-notices.sh writes. A new or updated dependency fails here until the script is run.
func TestThirdPartyNoticesCoverLinkedModules(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go tool")
	}
	notices, err := os.ReadFile("../THIRD-PARTY-NOTICES.md")
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range [][2]string{{"darwin", "arm64"}, {"windows", "amd64"}, {"windows", "arm64"}, {"linux", "amd64"}, {"linux", "arm64"}} {
		cmd := exec.Command("go", "list", "-deps", "-f", "{{with .Module}}{{if not .Main}}{{.Path}} {{.Version}}{{end}}{{end}}", ".", "./cmd/minigw-tui", "./cmd/minigw-core")
		cmd.Env = append(os.Environ(), "GOOS="+target[0], "GOARCH="+target[1], "CGO_ENABLED=0")
		out, err := cmd.Output()
		if err != nil {
			t.Fatalf("go list %s/%s: %v", target[0], target[1], err)
		}
		for _, line := range strings.Split(string(out), "\n") {
			f := strings.Fields(line)
			if len(f) != 2 || strings.HasPrefix(f[0], "dev.trojar/") { // dev.trojar/…: our own code
				continue
			}
			if !strings.Contains(string(notices), "| "+f[0]+" | "+f[1]+" |") {
				t.Errorf("%s %s (%s/%s) has no entry in THIRD-PARTY-NOTICES.md: run scripts/third-party-notices.sh", f[0], f[1], target[0], target[1])
			}
		}
	}
}
