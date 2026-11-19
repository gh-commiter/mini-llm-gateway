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
	// Outbound connections to this machine's own name or LAN address (the firewall prompts for these as well, and the result depends on the network):
	regexp.MustCompile(`os\.Hostname\(\)`),
	regexp.MustCompile(`(net|Resolver)\.Lookup\w+\((ctx|context\.\w+\(\)|t\.Context\(\)),\s*[^"\s]`), // with a context: a host that is not a literal
	regexp.MustCompile(`Resolver\.LookupIP\(\w+(\.\w+\(\))?,\s*"[a-z0-9]+",\s*[^"\s]`),
	regexp.MustCompile(`net\.Lookup(Host|IP|Addr)\([^"\s]`), // without one
	regexp.MustCompile(`(Dial\w*|http\.(Get|Post|Head|PostForm)|NewRequest\w*)\(.*(\b10\.\d+\.\d+\.\d+|\b172\.(1[6-9]|2\d|3[01])\.\d+\.\d+|\b192\.168\.\d+\.\d+|[a-z0-9-]+\.local([:/"]|$))`),
}

// file, text that is allowed there, why
var nonLoopbackAllowed = []struct{ file, text, why string }{
	{"serve_admin_test.go", `"address":"0.0.0.0"`, "the admin API validates the address setting (saved for the next start); nothing binds in the test"},
	{"serve_admin_test.go", `0.0.0.0 refused`, "the failure message of the line above"},
}

// A source line is a hit when any pattern matches; the guard test and its own matcher test both use it.
func nonLoopbackHit(line string) bool {
	for _, re := range nonLoopbackListener {
		if re.MatchString(line) {
			return true
		}
	}
	return false
}

// The matcher must catch planted outbound examples and let data-only lines through (the guard reads sources, so the planted lines live here, which it skips).
func TestNonLoopbackGuardMatchesOutboundExamples(t *testing.T) {
	for _, line := range []string{
		`host, _ := os.Hostname(); u := "http://" + host + ":8080"`,
		`addrs, _ := net.LookupHost(name)`,
		`addrs, err := net.DefaultResolver.LookupIP(ctx, "ip", host)`,
		`c, err := net.Dial("tcp", "192.168.1.5:80")`,
		`resp, _ := http.Get("http://10.1.2.3:4000/x")`,
		`req, _ := http.NewRequest("GET", "http://172.20.0.4/x", nil)`,
		`c, _ := net.DialTimeout("tcp", "mini.local:8765", time.Second)`,
	} {
		if !nonLoopbackHit(line) {
			t.Errorf("not caught: %s", line)
		}
	}
	for _, line := range []string{
		`fakeResolver(t, map[string]string{"litellm": "172.22.0.3"})`,
		`for _, u := range []string{"http://10.1.2.3:4000/key/info", "http://litellm.local:4000/key/info"} {`,
		`{"PUT", ".claude/settings.local.json", ""}`,
		`resp, _ := http.Get("http://192.0.2.1/x")`,
		`net.LookupHost("localhost")`,
		`c, _ := net.Dial("tcp", "127.0.0.1:80")`,
	} {
		if nonLoopbackHit(line) {
			t.Errorf("caught data: %s", line)
		}
	}
}

func TestNoTestListensOutsideLoopbackWithoutAReason(t *testing.T) {
	var files []string
	filepath.WalkDir(".", func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, "_test.go") {
			files = append(files, p)
		}
		return nil
	})
	if len(files) < 2 { // only this file (or none): the sources are not next to the test program
		t.Skip("needs the source tree; not in the suite exe")
	}
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
			{
				if !nonLoopbackHit(line) {
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
