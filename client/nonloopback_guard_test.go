package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// A test process must not listen outside 127.0.0.1: the macOS firewall asks the owner for permission for every unsigned program that accepts
// connections (a go test binary is one). A real need goes through the signed utility mg-test-net (Sources/MGTestNet, scripts/test-net-install.sh),
// which a test can exec. This scan keeps non-loopback listeners from coming back; the Swift side is NonLoopbackListenerGuardTests.

var nonLoopbackListener = []*regexp.Regexp{
	regexp.MustCompile(`Listen(Packet|TCP|UDP)?\("[a-z0-9]+",\s*("(0\.0\.0\.0)?:|"\[::\]|fmt\.Sprintf\(":)`),
	regexp.MustCompile(`ListenAndServe(TLS)?\("(0\.0\.0\.0)?:`),
	regexp.MustCompile(`"0\.0\.0\.0`),
	regexp.MustCompile(`\[::\]`),
	regexp.MustCompile(`net\.ListenConfig`),
	regexp.MustCompile(`Interface(Addrs|s)\(\)`),
}

// file, text that is allowed there, why
var nonLoopbackAllowed = []struct{ file, text, why string }{
	{"serve_admin_test.go", `"address":"0.0.0.0"`, "the admin API validates the address setting (saved for the next start); nothing binds in the test"},
	{"serve_admin_test.go", `0.0.0.0 refused`, "the failure message of the line above"},
}

func TestNoTestListensOutsideLoopbackWithoutAReason(t *testing.T) {
	var files []string
	filepath.WalkDir(".", func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, "_test.go") {
			files = append(files, p)
		}
		return nil
	})
	used := map[string]bool{}
	for _, f := range files {
		if filepath.Base(f) == "nonloopback_guard_test.go" {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for n, line := range strings.Split(string(b), "\n") {
			for _, re := range nonLoopbackListener {
				if !re.MatchString(line) {
					continue
				}
				ok := false
				for _, a := range nonLoopbackAllowed {
					if a.file == filepath.Base(f) && strings.Contains(line, a.text) {
						ok, used[a.file+a.text] = true, true
					}
				}
				if !ok {
					t.Errorf("%s:%d listens or reads addresses outside loopback: %s\nuse mg-test-net (exec it) or list it in nonLoopbackAllowed with the reason", f, n+1, strings.TrimSpace(line))
				}
			}
		}
	}
	for _, a := range nonLoopbackAllowed {
		if a.why == "" || !used[a.file+a.text] {
			t.Errorf("stale allow-list entry %s %s: remove it", a.file, a.text)
		}
	}
}
