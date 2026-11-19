package main

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The table of remotely reachable routes in docs/docker.md must be exactly what remoteAdminAllowed lets through. The table is
// expanded to concrete (method, path) pairs; every pair the allow-list accepts over a small vocabulary of path segments must be
// in it, and every pair in it must be accepted. Skipped when the docs tree is absent (a client/ checkout alone).
func TestRemoteRoutesTableMatchesTheAllowList(t *testing.T) {
	doc, err := os.ReadFile("../docs/docker.md")
	if err != nil {
		t.Skip("needs the docs tree: " + err.Error())
	}
	text := string(doc)
	i, j := strings.Index(text, "<!-- remote-routes:begin"), strings.Index(text, "<!-- remote-routes:end -->")
	if i < 0 || j < i {
		t.Fatal("docs/docker.md has no remote-routes markers")
	}
	cell := regexp.MustCompile("`([^`]+)`")
	table := map[string]bool{}
	for _, line := range strings.Split(text[i:j], "\n") {
		if !strings.HasPrefix(line, "| ") || strings.HasPrefix(line, "| Methods") {
			continue
		}
		cols := strings.Split(line, "|")
		if len(cols) < 4 {
			t.Fatal("bad table row", line)
		}
		for _, m := range strings.Split(strings.TrimSpace(cols[1]), ",") {
			for _, p := range cell.FindAllStringSubmatch(cols[2], -1) {
				for _, concrete := range expandRoute(p[1], remoteDocsVocab) {
					table[strings.TrimSpace(m)+" "+concrete] = true
				}
			}
		}
	}
	if len(table) == 0 {
		t.Fatal("the table is empty")
	}
	vocab := remoteDocsVocab
	methods := []string{"GET", "POST", "PUT", "PATCH", "DELETE"}
	allowed := map[string]bool{}
	var walk func(prefix []string)
	walk = func(prefix []string) {
		if len(prefix) > 0 {
			path := adminPrefix + "/" + strings.Join(prefix, "/")
			for _, m := range methods {
				if remoteAdminAllowed(m, path) {
					allowed[m+" "+strings.Join(prefix, "/")] = true
				}
			}
		}
		if len(prefix) == 4 {
			return
		}
		for _, v := range vocab {
			walk(append(append([]string{}, prefix...), v))
		}
	}
	walk(nil)
	var missing, extra []string
	for k := range allowed {
		if !table[k] {
			missing = append(missing, k)
		}
	}
	for k := range table {
		if !allowed[k] {
			extra = append(extra, k)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 || len(extra) > 0 {
		t.Fatalf("docs/docker.md table and remoteAdminAllowed disagree:\nallowed but not in the table: %v\nin the table but refused: %v", missing, extra)
	}
}

var remoteDocsVocab = []string{"providers", "devices", "users", "bindings", "teams", "projects", "kb", "submissions", "notes", "accept", "reject", "models", "history", "members", "assignments", "versions", "search", "faq", "1", "x"}

// expandRoute: a documented path with {id} (a number) or {name} (any one segment) as concrete paths over the test vocabulary.
func expandRoute(p string, vocab []string) []string {
	out := []string{p}
	for _, ph := range []struct {
		tag  string
		with []string
	}{{"{id}", []string{"1"}}, {"{name}", vocab}} {
		var next []string
		for _, o := range out {
			if !strings.Contains(o, ph.tag) {
				next = append(next, o)
				continue
			}
			for _, w := range ph.with {
				next = append(next, strings.ReplaceAll(o, ph.tag, w))
			}
		}
		out = next
	}
	return out
}
