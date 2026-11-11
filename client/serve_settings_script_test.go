package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Stran z nastavitvami: skripta mora biti veljaven JavaScript (0.5.24 krog 3: manjkajoč oklepaj je ustavil celo stran).
// Preveri node --check, kjer je node; sicer preskoči.
func TestSettingsScriptSyntax(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed")
	}
	page := strings.NewReplacer("{{NONCE}}", "n", "{{PICKER}}", "false").Replace(settingsPage)
	m := regexp.MustCompile(`(?s)<script[^>]*>(.*?)</script>`).FindAllStringSubmatch(page, -1)
	if len(m) == 0 {
		t.Fatal("no script in settings.html")
	}
	for i, s := range m {
		f := filepath.Join(t.TempDir(), "s.js")
		if err := os.WriteFile(f, []byte(s[1]), 0o600); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command(node, "--check", f).CombinedOutput(); err != nil {
			t.Fatalf("script %d: %v\n%s", i, err, out)
		}
	}
}
