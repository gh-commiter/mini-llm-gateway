package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// folder makes a real folder under a temp dir and returns its canonical path (macOS temp dirs are links).
func folder(t *testing.T, parts ...string) string {
	t.Helper()
	p := filepath.Join(parts...)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := realFolder(p)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func hint(t *testing.T, dir, slug string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, hintFile), []byte(slug+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestProjectForResolution(t *testing.T) { // contracts section 3, case 15
	const gw = "gw1"
	root := folder(t, t.TempDir())
	linked := folder(t, root, "work", "fakture")
	sub := folder(t, linked, "a", "b")
	st := projectState{Links: []projectLink{{Gateway: gw, Project: 1, Slug: "fakture", Path: linked}}}
	cases := []struct {
		name  string
		setup func() (cwd string)
		st    projectState
		gw    string
		want  projectState3
		slug  string // the applying link's slug, or the hint when there is no link
	}{
		{"no link, no hint", func() string { return folder(t, root, "plain", "x") }, st, gw, noLink, ""},
		{"no link + hint in an ancestor", func() string {
			d := folder(t, root, "hinted")
			hint(t, d, "eureko")
			return folder(t, d, "deep", "er")
		}, st, gw, noLink, "eureko"},
		{"hint above the repository root is not looked at", func() string {
			d := folder(t, root, "outer")
			hint(t, d, "eureko")
			repo := folder(t, d, "repo")
			folder(t, repo, ".git")
			return folder(t, repo, "src")
		}, st, gw, noLink, ""},
		{"link ok at the root and below", func() string { return sub }, st, gw, linkOK, "fakture"},
		{"link ok, hint agrees", func() string { hint(t, linked, "fakture"); return sub }, st, gw, linkOK, "fakture"},
		{"link + hint with another slug", func() string { hint(t, linked, "eureko"); return sub }, st, gw, linkHintDiff, "fakture"},
		{"a hint below the linked root is ignored", func() string {
			l := folder(t, root, "w2", "p")
			hint(t, folder(t, l, "inner"), "other")
			return folder(t, l, "inner")
		}, projectState{Links: []projectLink{{Gateway: gw, Project: 2, Slug: "p2", Path: filepath.Join(root, "w2", "p")}}}, gw, linkOK, "p2"},
		{"copied folder has no link", func() string { return folder(t, root, "work", "fakture-copy") }, st, gw, noLink, ""},
		{"moved folder has no link", func() string {
			moved := folder(t, root, "gone")
			return moved
		}, projectState{Links: []projectLink{{Gateway: gw, Project: 1, Slug: "fakture", Path: filepath.Join(root, "work", "fakture-old")}}}, gw, noLink, ""},
		{"another gateway's link does not apply", func() string { return sub }, st, "gw2", noLink, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			os.Remove(filepath.Join(linked, hintFile)) // each case starts without the root hint
			cwd := c.setup()
			res, err := projectFor(c.st, c.gw, cwd)
			if err != nil {
				t.Fatal(err)
			}
			if res.State != c.want {
				t.Fatalf("state %v, want %v (%+v)", res.State, c.want, res)
			}
			got := res.Hint
			if res.State != noLink {
				got = res.Link.Slug
			}
			if got != c.slug {
				t.Fatalf("slug %q, want %q", got, c.slug)
			}
		})
	}
}

func TestProjectForNearestWins(t *testing.T) {
	root := folder(t, t.TempDir(), "r")
	inner := folder(t, root, "mod", "deep")
	st := projectState{Links: []projectLink{
		{Gateway: "g", Project: 1, Slug: "outer", Path: root},
		{Gateway: "g", Project: 2, Slug: "inner", Path: inner},
	}}
	for cwd, want := range map[string]string{
		folder(t, inner, "x"): "inner", inner: "inner", folder(t, root, "mod"): "outer", root: "outer",
	} {
		res, err := projectFor(st, "g", cwd)
		if err != nil || res.Link.Slug != want {
			t.Errorf("%s: %+v %v, want %s", cwd, res.Link, err, want)
		}
	}
}

func TestProjectForSymlinkIntoLinkedTree(t *testing.T) {
	root := folder(t, t.TempDir(), "r")
	inside := folder(t, root, "sub")
	outside := folder(t, t.TempDir(), "o")
	link := filepath.Join(outside, "shortcut")
	if err := os.Symlink(inside, link); err != nil {
		t.Skip("no symlinks here:", err)
	}
	st := projectState{Links: []projectLink{{Gateway: "g", Project: 1, Slug: "p", Path: root}}}
	res, err := projectFor(st, "g", link)
	if err != nil || res.State != linkOK || res.RealPath != inside {
		t.Fatalf("%+v %v", res, err)
	}
	// the reverse: a link stored for the real path also applies to a path through a symlinked parent
	parentLink := filepath.Join(outside, "parent")
	if err := os.Symlink(root, parentLink); err != nil {
		t.Skip(err)
	}
	if res, _ := projectFor(st, "g", filepath.Join(parentLink, "sub")); res.State != linkOK {
		t.Fatalf("through a parent link: %+v", res)
	}
}

func TestProjectForCaseInsensitiveVolumes(t *testing.T) {
	root := folder(t, t.TempDir(), "CaseTest")
	sub := folder(t, root, "Sub")
	upper := filepath.Join(filepath.Dir(root), strings.ToUpper(filepath.Base(root)))
	a, err1 := fileIDOf(root)
	b, err2 := fileIDOf(upper)
	if err1 != nil || err2 != nil || a != b {
		t.Skipf("this volume is case-sensitive (%s)", runtime.GOOS)
	}
	st := projectState{Links: []projectLink{{Gateway: "g", Project: 1, Slug: "p", Path: root}}}
	res, err := projectFor(st, "g", filepath.Join(upper, "Sub"))
	if err != nil || res.State != linkOK {
		t.Fatalf("%s: %+v %v", sub, res, err)
	}
}

func TestHintFileIsOnlyAPlainSlug(t *testing.T) {
	d := folder(t, t.TempDir(), "h")
	for content, want := range map[string]string{
		"fakture\n": "fakture", "  fakture \r\nsecond line": "fakture", "": "", "Fakture": "", "a": "", "ab cd": "", "../x": "", strings.Repeat("a", 300): "",
	} {
		os.WriteFile(filepath.Join(d, hintFile), []byte(content), 0o644)
		if got := readHint(d); got != want {
			t.Errorf("%q: %q, want %q", content, got, want)
		}
	}
	os.Remove(filepath.Join(d, hintFile))
	if err := os.Symlink("/etc/hostname", filepath.Join(d, hintFile)); err == nil && readHint(d) != "" {
		t.Error("a symlinked hint was read")
	}
}

func TestProjectStateRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "projects.json")
	if st, err := loadProjectState(p); err != nil || len(st.Links) != 0 {
		t.Fatal(st, err)
	}
	want := projectState{Links: []projectLink{{Gateway: "k", Project: 7, Slug: "s", Path: "/x"}}}
	if err := saveProjectState(p, want); err != nil {
		t.Fatal(err)
	}
	got, err := loadProjectState(p)
	if err != nil || len(got.Links) != 1 || got.Links[0] != want.Links[0] || got.Version != 1 {
		t.Fatalf("%+v %v", got, err)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
			t.Fatalf("mode %v", fi.Mode())
		}
	}
	os.WriteFile(p, []byte("{broken"), 0o600)
	if _, err := loadProjectState(p); err == nil {
		t.Fatal("a broken state file must be an error, not an empty state")
	}
}
