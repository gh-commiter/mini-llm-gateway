package main

import (
	"regexp"
	"strings"
	"testing"
)

// globMatch: the Edit(...) path globs of editDenyRules: ./ = the folder, **/ = any folders (also none), /** = anything below, * = within a name,
// [xX] = a character class.
func globMatch(glob, path string) bool {
	g := strings.TrimPrefix(glob, "./")
	var re strings.Builder
	for i := 0; i < len(g); i++ {
		switch {
		case strings.HasPrefix(g[i:], "**/"):
			re.WriteString("(?:.*/)?")
			i += 2
		case strings.HasPrefix(g[i:], "/**"):
			re.WriteString("/.*")
			i += 2
		case g[i] == '*':
			re.WriteString("[^/]*")
		case g[i] == '[':
			j := strings.IndexByte(g[i:], ']')
			re.WriteString(g[i : i+j+1])
			i += j
		default:
			re.WriteString(regexp.QuoteMeta(string(g[i])))
		}
	}
	return regexp.MustCompile("^" + re.String() + "$").MatchString(path)
}

func editDenies(path string) bool {
	for _, r := range editDenyRules() {
		if globMatch(strings.TrimSuffix(strings.TrimPrefix(r, "Edit("), ")"), path) {
			return true
		}
	}
	return false
}

// Would have caught the Windows name forms: every path of the protectedPath table that exists on macOS and Linux must be denied by the edit rules.
// Not covered (and edit is refused on Windows for them): trailing dots and spaces, 8.3 short names, and Unicode folds such as "ſ" (APFS, ext4 and
// NTFS do not treat them as the same name; only HFS+ ignorables would).
func TestEditRulesCoverProtectedPaths(t *testing.T) {
	covered := 0
	for _, p := range protectedCases {
		exotic := false
		for _, c := range strings.Split(p, "/") {
			exotic = exotic || strings.HasSuffix(c, ".") || strings.HasSuffix(c, " ") || strings.Contains(c, "~")
		}
		if exotic {
			continue
		}
		covered++
		// a protected folder itself (".husky") is judged by a file below it: Edit works on files
		if !protectedPath(p) || !(editDenies(p) || editDenies(p+"/x")) {
			t.Errorf("%q is protected but the edit rules allow it", p)
		}
	}
	if covered < 12 {
		t.Fatalf("only %d paths checked", covered)
	}
	for _, p := range unprotectedCases {
		if editDenies(p) {
			t.Errorf("%q is not protected but the edit rules deny it", p)
		}
	}
	// the matcher itself: it must be able to say no
	if globMatch("./.[gG][iI][tT]/**", "src/.git/x") || !globMatch("./**/.[gG][iI][tT]/**", "src/.git/x") || globMatch("./.[eE][nN][vV][rR][cC]", "x/.envrc") {
		t.Error("glob matcher")
	}
}

func TestEditRefusedOnWindows(t *testing.T) {
	if editWindowsRefusal("windows") == "" || editWindowsRefusal("darwin") != "" || editWindowsRefusal("linux") != "" {
		t.Error("edit is refused on Windows only")
	}
	if !strings.Contains(editWindowsRefusal("windows"), "not available on Windows yet") {
		t.Error("message")
	}
}
