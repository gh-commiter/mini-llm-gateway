//go:build !windows

package main

// Hostile repositories (issue 144): each test builds a repository whose own config, attributes or alternates try to run a
// command (a marker file proves it ran) and calls the product function that touches such a repository. A positive control
// (the same trap fired by an unguarded git call) proves the trap works with the local git.

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// plainGit runs git with no product guard (the positive control and the setup of hostile repositories), isolated from the
// user's own configuration.
func plainGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0",
		"GIT_AUTHOR_NAME=a", "GIT_AUTHOR_EMAIL=a@a", "GIT_COMMITTER_NAME=a", "GIT_COMMITTER_EMAIL=a@a")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// hostileRepo: a repository with one commit and a tracked file; the caller adds the trap to its config.
type hostile struct {
	t      *testing.T
	top    string
	marker string
}

func newHostile(t *testing.T) *hostile {
	t.Helper()
	root := t.TempDir()
	h := &hostile{t: t, top: filepath.Join(root, "top"), marker: filepath.Join(root, "MARKER")}
	os.MkdirAll(h.top, 0o755)
	plainGit(t, h.top, "init", "-q")
	os.WriteFile(filepath.Join(h.top, "f.txt"), []byte("one\n"), 0o644)
	plainGit(t, h.top, "add", "f.txt")
	plainGit(t, h.top, "commit", "-q", "-m", "x")
	return h
}

// trap is a shell command that creates the marker.
func (h *hostile) trap() string { return "touch " + h.marker }

func (h *hostile) ran() bool { _, err := os.Stat(h.marker); return err == nil }

func (h *hostile) must(why string, ran bool) {
	h.t.Helper()
	if h.ran() != ran {
		h.t.Fatalf("%s: marker present %v, want %v", why, h.ran(), ran)
	}
}

func (h *hostile) reset() { os.Remove(h.marker) }

// T1: the user's repository config redirects the bundle path with url.<x>.insteadOf to an ext:: transport (allowed by the
// same config): gitFetchBranch fetches the gateway's bundle with the user's configuration.
func TestAuditFetchBundleInsteadOfExt(t *testing.T) {
	h := newHostile(t)
	src := filepath.Join(filepath.Dir(h.top), "src")
	os.MkdirAll(src, 0o755)
	plainGit(t, src, "init", "-q", "-b", "minigw-src")
	plainGit(t, src, "commit", "-q", "--allow-empty", "-m", "s")
	bundle := filepath.Join(filepath.Dir(h.top), "in.bundle")
	plainGit(t, src, "bundle", "create", bundle, "HEAD")
	plainGit(t, h.top, "config", "protocol.ext.allow", "always")
	plainGit(t, h.top, "config", "url.ext::sh -c touch% "+h.marker+"% #.insteadOf", "/") // ext:: escapes a space as "% "
	// the positive control: an unguarded fetch with the same config fires the trap
	ctl := exec.Command("git", "-C", h.top, "fetch", "--no-tags", "--quiet", bundle, "+HEAD:refs/minigw/c")
	ctl.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	cout, _ := ctl.CombinedOutput()
	t.Logf("control output: %s", cout)
	h.must("positive control (unguarded fetch)", true)
	h.reset()
	gitFetchBranch(h.top, bundle, "minigw/claude-0123abcd-1")
	h.must("gitFetchBranch", false)
}

// T2: commit.gpgSign with a hostile gpg.program in the repository's config: gitSnapshot makes a commit with commit-tree.
func TestAuditSnapshotSigning(t *testing.T) {
	h := newHostile(t)
	script := filepath.Join(filepath.Dir(h.top), "gpg.sh")
	os.WriteFile(script, []byte("#!/bin/sh\n"+h.trap()+"\ncat >/dev/null\nexit 1\n"), 0o755)
	plainGit(t, h.top, "config", "gpg.program", script)
	plainGit(t, h.top, "config", "commit.gpgsign", "true")
	os.WriteFile(filepath.Join(h.top, "f.txt"), []byte("changed\n"), 0o644) // an uncommitted change: a snapshot commit is made
	exec.Command("git", "-C", h.top, "-c", "user.name=a", "-c", "user.email=a@a", "commit", "-q", "--allow-empty", "-m", "control").Run()
	h.must("positive control (git commit signs)", true)
	h.reset()
	gitSnapshot(h.top, false, false)
	h.must("gitSnapshot", false)
}

// T3: a clean filter named by the repository's own config and attributes (the top repository is the user's own: the
// snapshot runs its filters on purpose, so that Git LFS pointers are made); a nested repository never does (N3).
func TestAuditSnapshotCleanFilter(t *testing.T) {
	h := newHostile(t)
	os.WriteFile(filepath.Join(h.top, ".gitattributes"), []byte("*.txt filter=x\n"), 0o644)
	plainGit(t, h.top, "config", "filter.x.clean", "sh -c '"+h.trap()+"; cat'")
	os.WriteFile(filepath.Join(h.top, "f.txt"), []byte("changed\n"), 0o644)
	plainGit(t, h.top, "add", "f.txt") // positive control: add runs the clean filter
	h.must("positive control", true)
	h.reset()
	os.WriteFile(filepath.Join(h.top, "f.txt"), []byte("changed again\n"), 0o644)
	gitSnapshot(h.top, false, true) // nested: no attributes
	h.must("gitSnapshot of a nested repository", false)
	gitSnapshot(h.top, false, false)
	t.Logf("top repository snapshot ran the clean filter: %v", h.ran())
}

// T4: core.alternateRefsCommand with objects/info/alternates: fetch from "." (the local branch move) starts upload-pack.
func TestAuditLocalFetchAlternateRefsCommand(t *testing.T) {
	h := newHostile(t)
	alt := filepath.Join(filepath.Dir(h.top), "alt")
	os.MkdirAll(alt, 0o755)
	plainGit(t, alt, "init", "-q", "--bare")
	os.WriteFile(filepath.Join(h.top, ".git", "objects", "info", "alternates"), []byte(filepath.Join(alt, "objects")+"\n"), 0o644)
	plainGit(t, h.top, "config", "core.alternateRefsCommand", h.trap()+"; true")
	plainGit(t, h.top, "update-ref", "refs/minigw/incoming", "HEAD")
	ctl := exec.Command("git", "-C", h.top, "fetch", "--no-tags", "--quiet", ".", "refs/minigw/incoming:refs/heads/probe")
	ctl.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	ctl.Run()
	h.must("positive control (unguarded local fetch)", true)
	h.reset()
	plainGit(t, h.top, "branch", "-D", "probe")
	// the product's own call of that shape:
	gitUser(h.top, nil, "-c", "fetch.recurseSubmodules=false", "-c", "submodule.recurse=false", "-c", "maintenance.auto=false",
		"-c", "gc.auto=0", "fetch", "--no-tags", "--no-write-fetch-head", "--no-recurse-submodules", "--quiet",
		".", "refs/minigw/incoming:refs/heads/probe2")
	h.must("the local branch move of gitFetchBranch", false)
}

// T5: the gateway receives a client's bundle. A tree cannot carry .git (no config, hooks or info/attributes); it can carry
// .gitattributes naming a filter (no config defines it on the gateway) and .gitmodules whose update command is a shell
// command (refused by the object check: gitmodulesUpdate). The mirror and the clone have their own, empty configuration.
func TestAuditGatewayMirrorAndCloneOfAHostileBundle(t *testing.T) {
	for _, withModules := range []bool{true, false} {
		h := newHostile(t)
		os.WriteFile(filepath.Join(h.top, ".gitattributes"), []byte("*.txt filter=x\n* export-subst\n"), 0o644)
		if withModules {
			os.WriteFile(filepath.Join(h.top, ".gitmodules"), []byte("[submodule \"m\"]\n\tpath = m\n\turl = ./m\n\tupdate = !"+h.trap()+"\n"), 0o644)
		}
		plainGit(t, h.top, "add", ".")
		plainGit(t, h.top, "commit", "-q", "-m", "hostile tree")
		snapshot := plainGit(t, h.top, "rev-parse", "HEAD")
		plainGit(t, h.top, "update-ref", "refs/minigw/push", "HEAD")
		bundle := filepath.Join(filepath.Dir(h.top), "push.bundle")
		plainGit(t, h.top, "bundle", "create", bundle, "refs/minigw/push")
		scratch := t.TempDir()
		repo := filepath.Join(scratch, "repos", "dev", "r.git")
		if err := ensureMirror(repo); err != nil {
			t.Fatal(err)
		}
		err := importBase(repo, bundle, snapshot, "refs/minigw/base")
		if withModules {
			if err == nil {
				t.Fatal("a .gitmodules with a shell update command was accepted")
			}
			h.must("import of a hostile .gitmodules", false)
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := fsckBase(repo, "refs/minigw/base"); err != nil {
			t.Fatal(err)
		}
		wt := filepath.Join(scratch, "clones", "wt")
		if err := makeClone(repo, wt, "minigw/claude-0123abcd-1", snapshot); err != nil {
			t.Fatal(err)
		}
		h.must("mirror, fsck and clone of a bundle with hostile attributes", false)
		// the control: the same tree with the filter configured in the clone would run it
		ctl := filepath.Join(scratch, "control")
		plainGit(t, scratch, "clone", "-q", "--no-checkout", repo, ctl)
		plainGit(t, ctl, "config", "filter.x.smudge", h.trap()+"; cat")
		plainGit(t, ctl, "checkout", "-q", "-b", "c", snapshot)
		h.must("positive control (filter configured in the clone)", true)
	}
}

// hostileConfig fills a repository's own config, attributes and info/attributes with every trap that names a program.
func (h *hostile) hostileConfig() {
	t := h.t
	os.WriteFile(filepath.Join(h.top, ".git", "info", "attributes"), []byte("*.txt filter=x diff=x\n"), 0o644)
	for k, v := range map[string]string{"core.fsmonitor": h.trap() + "; echo", "diff.external": h.trap(), "diff.x.textconv": h.trap() + "; cat",
		"diff.x.command": h.trap(), "filter.x.clean": h.trap() + "; cat", "filter.x.smudge": h.trap() + "; cat", "core.pager": h.trap() + "; cat",
		"core.editor": h.trap(), "credential.helper": "!" + h.trap(), "core.sshCommand": h.trap(), "gpg.program": h.trap(), "core.alternateRefsCommand": h.trap()} {
		plainGit(t, h.top, "config", k, v)
	}
	os.WriteFile(filepath.Join(h.top, "f.txt"), []byte("changed\n"), 0o644) // a tracked file whose stat differs
	hook := filepath.Join(h.top, ".git", "hooks", "post-index-change")
	os.MkdirAll(filepath.Dir(hook), 0o755)
	os.WriteFile(hook, []byte("#!/bin/sh\n"+h.trap()+"\n"), 0o755)
}

// T6: every call of the git watch (user's repository) with every trap in place.
func TestAuditGitWatchCalls(t *testing.T) {
	h := newHostile(t)
	h.hostileConfig()
	ctl := exec.Command("git", "-C", h.top, "status")
	ctl.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1", "GIT_PAGER=cat")
	ctl.Run()
	h.must("positive control (git status)", true)
	h.reset()
	ids := plainGit(t, h.top, "rev-parse", "HEAD")
	for _, args := range [][]string{
		{"for-each-ref", "--format=%(refname) %(objectname)"}, {"symbolic-ref", "-q", "--short", "HEAD"}, {"rev-parse", "-q", "--verify", "HEAD"},
		{"ls-files", "-s", "-z"}, {"diff", "--cached", "--name-only", "--no-renames", "--no-ext-diff", "--no-textconv", "-z"},
		{"diff-tree", "-m", "-r", "-z", "--raw", "--no-renames", "--root", "--no-commit-id", "--no-ext-diff", ids},
	} {
		gitWatchRun(h.top, "", args...)
		h.must("gitWatchRun "+args[0], false)
	}
	gitWatchRun(h.top, "100644 "+ids+"\t0\tf.txt\n", "update-index", "--index-info") // as the watch's own restore of an index entry
	h.must("gitWatchRun update-index --index-info", false)
}

// T7: the other places that read a user's repository: the origin of a folder, the toplevel, the hook's state (kb_hook has
// its own test), the nested-repository list and the plain gitUser queries.
func TestAuditOriginToplevelAndQueries(t *testing.T) {
	h := newHostile(t)
	h.hostileConfig()
	plainGit(t, h.top, "remote", "add", "origin", "https://example.invalid/x.git")
	if got := gitOrigin(h.top); got != "https://example.invalid/x.git" {
		t.Fatal(got)
	}
	h.must("gitOrigin", false)
	gitToplevel(h.top)
	h.must("gitToplevel", false)
	for _, args := range [][]string{{"rev-parse", "--verify", "-q", "HEAD^{commit}"}, {"rev-parse", "HEAD^{tree}"}, {"symbolic-ref", "-q", "HEAD"},
		{"for-each-ref", "--format=%(objectname) %(refname)"}, {"rev-parse", "--git-path", "objects"}, {"cat-file", "-e", "HEAD^{commit}"},
		{"merge-base", "--is-ancestor", "HEAD", "HEAD"}, {"config", "--local", "--includes", "--get-regexp", `^filter\.`}, {"update-ref", "refs/minigw/t", "HEAD"}} {
		gitUser(h.top, nil, args...)
		h.must("gitUser "+args[0], false)
	}
	gitZ(h.top, "", "ls-files", "-z")
	h.must("gitZ", false)
	gitHasCommit(h.top)
	h.must("gitHasCommit", false)
}

// Every place of the product that starts git must carry the shared guard (gitNoExecPins/gitNoExecEnv, gitGuardPins, or the
// gitWatch and hook guards named below) near the invocation. A new git call elsewhere fails here: add the guard, or list it
// with the reason it needs none.
func TestEveryGitInvocationCarriesAGuard(t *testing.T) {
	type site struct{ file, guard, why string }
	allowed := []site{
		{"gitwatch.go", "gitWatchPins", "the git watch: its own pins and env (asserted against git-watch-pins.json and the Swift watch)"},
		{"kb_hook.go", "gitGuardEnv", "hook state: rev-parse and ls-files with the agent guard environment and no filter-running command"},
		{"project_cmd.go", "gitNoExecPins", "origin lookup: config --local --get"},
		{"worktree.go", "gitNoExecPins", "client side: the user's repository"},
		{"serve_worktree.go", "gitGuardPins", "gateway side: mirror and clone, gateway-owned configuration"},
	}
	call := regexp.MustCompile(`(?:commandContext|command|exec\.Command|exec\.CommandContext)\(\s*(?:ctx,\s*)?(?:"git"|name\b|exe\b)`)
	files, _ := filepath.Glob("*.go")
	if len(files) == 0 {
		t.Skip("needs the client/ source tree; not in the suite exe")
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, _ := os.ReadFile(f)
		lines := strings.Split(string(b), "\n")
		for i, l := range lines {
			if strings.HasPrefix(strings.TrimSpace(l), "//") || !call.MatchString(l) || !(strings.Contains(l, `"git"`) || strings.Contains(l, "exe") || strings.Contains(l, "name, argv")) {
				continue
			}
			if !strings.Contains(l, `"git"`) && !(f == "serve_worktree.go" && strings.Contains(l, "name, argv")) {
				continue
			}
			ok := false
			for _, s := range allowed {
				if s.file == f {
					lo, hi := max(0, i-25), min(len(lines), i+8)
					ok = strings.Contains(strings.Join(lines[lo:hi], "\n"), s.guard)
				}
			}
			if !ok {
				t.Errorf("%s:%d starts git without a listed guard near it: %s", f, i+1, strings.TrimSpace(l))
			}
		}
	}
}
