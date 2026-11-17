package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// gitFolder makes a repository with the given origin in a temp folder, with a temp HOME (no user config is read).
func gitFolder(t *testing.T, origin string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", origin}} {
		c := exec.Command("git", args...)
		c.Dir = dir
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	return dir
}

func TestGitOriginIsTheRawConfigValue(t *testing.T) {
	dir := gitFolder(t, "https://tok:secret@Host.example/Eureko/Fakture.git")
	if got := gitOrigin(dir); got != "https://tok:secret@Host.example/Eureko/Fakture.git" {
		t.Fatal(got)
	}
	// An insteadOf rule must not rewrite it: the raw config value is what the folder says.
	c := exec.Command("git", "config", "--local", "url.git@other.example:.insteadOf", "https://tok:secret@Host.example/")
	c.Dir = dir
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatal(err, string(out))
	}
	if got := gitOrigin(dir); got != "https://tok:secret@Host.example/Eureko/Fakture.git" {
		t.Fatal("insteadOf applied:", got)
	}
	sub := dir + "/src"
	os.Mkdir(sub, 0o755)
	if got := gitOrigin(sub); got == "" {
		t.Fatal("a subfolder finds the repository's origin")
	}
	if got := gitOrigin(t.TempDir()); got != "" {
		t.Fatal("a folder outside a repository has no origin:", got)
	}
}

func TestStripUserinfo(t *testing.T) {
	for in, want := range map[string]string{
		"https://tok:secret@host.example/a/b.git": "https://host.example/a/b.git",
		"ssh://git@host.example:22/a/b":           "ssh://host.example:22/a/b",
		"git@host.example:a/b.git":                "git@host.example:a/b.git",
		"https://host.example/a@b/c":              "https://host.example/a@b/c",
		"  https://u@h.example/a/b \n":            "https://h.example/a/b",
		"/srv/git/a@b":                            "/srv/git/a@b",
		"https://u:p@ss@host.example/a/b":         "https://host.example/a/b",
	} {
		if got := stripUserinfo(in); got != want {
			t.Errorf("%q: %q want %q", in, got, want)
		}
	}
}

// The whole path through a real gateway: the owner or a project admin registers the folder's remote, a member links by it.
func TestRemoteIdentityFlowThroughGateway(t *testing.T) {
	c := newClientFx(t)
	dir := gitFolder(t, "https://tok:secret@Host.example/Eureko/Fakture.git")
	_, err := c.run("ana", dir, "", "project", "link")
	wantErr(t, err, "no project of yours is registered for this git remote (host.example/eureko/fakture); run minigw project link <slug>")
	_, err = c.run("ana", dir, "", "project", "remote", "add", "--here", "fakture")
	wantErr(t, err, "not allowed")
	if out := c.must("fani", dir, "", "project", "remote", "add", "--here", "fakture"); out != "registered host.example/eureko/fakture for project fakture\n" {
		t.Fatal(out)
	}
	if out := c.must("eva", dir, "", "project", "remote", "list", "fakture"); out != "host.example/eureko/fakture\n" {
		t.Fatal(out)
	}
	if out := c.must("ana", dir, "", "project", "link"); !strings.HasPrefix(out, "found project fakture by git remote host.example/eureko/fakture\nlinked ") {
		t.Fatal(out)
	}
	c.must("ana", dir, "", "project", "unlink")
	_, err = c.run("cene", dir, "", "project", "link") // not a member of fakture (grant only)
	wantErr(t, err, "no project of yours is registered")
	// The credentials never reached the gateway.
	var n int
	c.f.ok(c.f.s.core.DB().QueryRow(`SELECT count(*) FROM project_remotes WHERE identity LIKE '%secret%' OR identity LIKE '%tok%'`).Scan(&n))
	if n != 0 {
		t.Fatal("credentials stored")
	}
	if out := c.must("fani", dir, "", "project", "remote", "remove", "git@host.example:eureko/fakture.git", "fakture"); out != "removed host.example/eureko/fakture from project fakture\n" {
		t.Fatal(out)
	}
	_, err = c.run("ana2", t.TempDir(), "", "project", "link")
	wantErr(t, err, "no git remote origin in this folder; run minigw project link <slug>")
}

func TestKBFAQThroughGateway(t *testing.T) {
	c := newClientFx(t)
	dir := t.TempDir()
	e, out, _ := c.env("ana", dir, "Press the button.")
	if err := e.kb([]string{"submit", "fakture", "--kind", "faq", "--key", "q1", "--title", "How do I reset?", "-"}); err != nil {
		t.Fatal(err)
	}
	if out.String() != "submission 1 pending\n" {
		t.Fatal(out.String())
	}
	out.Reset()
	if err := e.kb([]string{"submissions", "fakture"}); err != nil || !strings.Contains(out.String(), "How do I reset?  (faq)\n") {
		t.Fatal(out.String(), err)
	}
	owner := &adminClient{off: c.f.s}
	review := stdout(t, func() { kbAdmin(owner, []string{"kb", "review"}, "", "") })
	if !strings.Contains(review, "How do I reset?  (faq)\n") {
		t.Fatal(review)
	}
	stdout(t, func() { kbAdmin(owner, []string{"kb", "accept", "1"}, "", "") })
	out.Reset()
	if err := e.kb([]string{"faq", "fakture"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "1  How do I reset?  data  ") || !strings.HasSuffix(out.String(), "\nPress the button.\n") {
		t.Fatal(out.String())
	}
	out.Reset()
	if err := e.kb([]string{"search", "fakture", "button"}); err != nil || !strings.Contains(out.String(), "How do I reset?  project 1  data  ") || !strings.Contains(strings.SplitN(out.String(), "\n", 2)[0], "  (faq)") {
		t.Fatal(out.String(), err)
	}
	// bor is a developer too: the data faq is visible; eva (reader of fakture) reads data as well.
	other, o2, _ := c.env("bor", dir, "")
	if err := other.kb([]string{"faq", "fakture", "--json"}); err != nil || !strings.HasPrefix(o2.String(), `{"entries":[{"answer":"Press the button."`) {
		t.Fatal(o2.String(), err)
	}
	for _, bad := range [][]string{{"submit", "fakture", "--kind", "rumor", "-"}, {"faq", "a", "b"}} {
		if err := e.kb(bad); err == nil {
			t.Fatal("accepted", bad)
		}
	}
	err := (&projectEnv{agent: true}).kb([]string{"faq"})
	if err == nil || err.Error() != agentRefusal {
		t.Fatal(err)
	}
}
