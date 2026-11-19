package main

import (
	"encoding/json"
	"os"
	"regexp"
	"sort"
	"testing"
)

// statusLabels: the `Label:   ` prefixes of the status lines in a client's main file (same rule in StatusParityTests.swift).
func statusLabels(t *testing.T, file string) []string {
	src := srcFile(t, file) // skips when the sources are not next to the test program
	seen := map[string]bool{}
	for _, m := range regexp.MustCompile(`"([A-Z][a-z]+):( +)`).FindAllStringSubmatch(string(src), -1) {
		if len(m[1])+1+len(m[2]) != 11 {
			t.Errorf("%s: label %q is not padded to column 11", file, m[1])
		}
		seen[m[1]] = true
	}
	var out []string
	for l := range seen {
		out = append(out, l)
	}
	sort.Strings(out)
	return out
}

func TestStatusLinesMatchTheSwiftClient(t *testing.T) {
	data := srcFile(t, "testdata/status-lines.json")
	var fx struct{ Labels []string }
	if err := json.Unmarshal(data, &fx); err != nil {
		t.Fatal(err)
	}
	sort.Strings(fx.Labels)
	for _, f := range []string{"main.go", "../Sources/MiniGW/main.swift"} {
		if _, err := os.Stat(f); err != nil && f != "main.go" {
			// The Linux container of the release gate and the Windows test binary have only client/: the Swift side is then
			// held to the same fixture by its own test (StatusParityTests.swift).
			t.Logf("%s is not here; the Swift test checks it against the same fixture", f)
			continue
		}
		got := statusLabels(t, f)
		if len(got) != len(fx.Labels) {
			t.Fatalf("%s: %v, fixture %v", f, got, fx.Labels)
		}
		for i := range got {
			if got[i] != fx.Labels[i] {
				t.Fatalf("%s: %v, fixture %v", f, got, fx.Labels)
			}
		}
	}
}
