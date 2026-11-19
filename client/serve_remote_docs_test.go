package main

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The table of remotely reachable routes in docs/docker.md must be exactly what remoteAdminAllowed lets through. The table is
// expanded to concrete (method, path) pairs ({id}: the numeric ids the policy accepts; {name}: any one segment), and the
// allow-list is probed over the same domain: every segment word the table uses, several ids (0, 1, 42, 18 digits) and the
// forms that must be refused (1a, 19 digits, -1, +1), with the methods GET, HEAD, OPTIONS, POST, PUT, PATCH and DELETE, to
// depth 4 over the whole domain and depth 5 over a reduced one. Both directions must match, so a changed id rule, a new route
// or a documented route the code does not serve fails. Skipped when the docs tree is absent (a client/ checkout alone).
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
	type route struct {
		methods []string
		path    string
	}
	var routes []route
	cell := regexp.MustCompile("`([^`]+)`")
	words := map[string]bool{"x": true}
	for _, line := range strings.Split(text[i:j], "\n") {
		if !strings.HasPrefix(line, "| ") || strings.HasPrefix(line, "| Methods") {
			continue
		}
		cols := strings.Split(line, "|")
		if len(cols) < 4 {
			t.Fatal("bad table row", line)
		}
		var ms []string
		for _, m := range strings.Split(strings.TrimSpace(cols[1]), ",") {
			ms = append(ms, strings.TrimSpace(m))
		}
		for _, p := range cell.FindAllStringSubmatch(cols[2], -1) {
			routes = append(routes, route{ms, p[1]})
			for _, w := range strings.Split(p[1], "/") {
				if !strings.HasPrefix(w, "{") {
					words[w] = true
				}
			}
		}
	}
	if len(routes) == 0 {
		t.Fatal("the table is empty")
	}
	for _, w := range []string{"notes", "history", "restore", "versions", "search", "faq", "kb", "submissions"} { // routes that must stay refused
		words[w] = true
	}
	validIDs := []string{"0", "1", "42", "999999999999999999"}  // 1 to 18 digits
	badIDs := []string{"1a", "9999999999999999999", "-1", "+1"} // 19 digits, a sign, a letter
	var wordList []string
	for w := range words {
		wordList = append(wordList, w)
	}
	sort.Strings(wordList)
	domain := append(append(append([]string{}, wordList...), validIDs...), badIDs...)
	reduced := append(append([]string{}, wordList...), "1", "42", "1a")
	methods := []string{"GET", "HEAD", "OPTIONS", "POST", "PUT", "PATCH", "DELETE"}

	table := map[string]bool{}
	for _, r := range routes {
		concrete := []string{r.path}
		for _, ph := range []struct {
			tag  string
			with []string
		}{{"{id}", validIDs}, {"{name}", domain}} {
			var next []string
			for _, c := range concrete {
				if !strings.Contains(c, ph.tag) {
					next = append(next, c)
					continue
				}
				for _, w := range ph.with {
					next = append(next, strings.Replace(c, ph.tag, w, 1))
				}
			}
			concrete = next
		}
		for _, c := range concrete {
			for _, m := range r.methods {
				table[m+" "+c] = true
			}
		}
	}
	allowed := map[string]bool{}
	var walk func(prefix []string, dom []string, depth int)
	walk = func(prefix []string, dom []string, depth int) {
		if len(prefix) > 0 {
			p := strings.Join(prefix, "/")
			for _, m := range methods {
				if remoteAdminAllowed(m, adminPrefix+"/"+p) {
					allowed[m+" "+p] = true
				}
			}
		}
		if len(prefix) == depth {
			return
		}
		for _, v := range dom {
			walk(append(append([]string{}, prefix...), v), dom, depth)
		}
	}
	walk(nil, domain, 4)
	walk(nil, reduced, 5)
	var missing, extra []string
	for k := range allowed {
		if !table[k] {
			missing = append(missing, k)
		}
	}
	for k := range table {
		// a table entry beyond the probed depth cannot be checked by probing: it must be accepted by a direct call
		if !allowed[k] && remoteAdminAllowed(strings.SplitN(k, " ", 2)[0], adminPrefix+"/"+strings.SplitN(k, " ", 2)[1]) == false {
			extra = append(extra, k)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) > 0 || len(extra) > 0 {
		t.Fatalf("docs/docker.md table and remoteAdminAllowed disagree:\nallowed but not in the table: %.40v (%d)\nin the table but refused: %.40v (%d)", missing, len(missing), extra, len(extra))
	}
	// the id boundaries per documented route: every {id} takes exactly 1 to 18 digits
	for _, r := range routes {
		if !strings.Contains(r.path, "{id}") {
			continue
		}
		for _, m := range r.methods {
			for _, id := range validIDs {
				if !remoteAdminAllowed(m, adminPrefix+"/"+strings.Replace(r.path, "{id}", id, 1)) {
					t.Errorf("%s %s with id %s is refused but documented", m, r.path, id)
				}
			}
			for _, id := range append(append([]string{}, badIDs...), "", "%31", "1.5", "1/2") {
				if remoteAdminAllowed(m, adminPrefix+"/"+strings.Replace(r.path, "{id}", id, 1)) {
					t.Errorf("%s %s with the id %q is allowed", m, r.path, id)
				}
			}
		}
	}
}
