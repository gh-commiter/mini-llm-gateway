package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	out, err := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@x"}, args...)...).Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}

func writeT(t *testing.T, path, text string) {
	t.Helper()
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Router, kot ga naredi Worktree.swift: golo zrcalo iz bundla odjemalca, klon agenta, bundle veje nazaj.
func TestWorktreeClientRoundTripAndMaliciousBranch(t *testing.T) {
	root := t.TempDir()
	client, marks := filepath.Join(root, "client"), filepath.Join(root, "marks")
	os.MkdirAll(client, 0o755)
	os.MkdirAll(marks, 0o755)
	gitT(t, client, "init", "-q", "-b", "main")
	writeT(t, filepath.Join(client, "a.txt"), "one\n")
	gitT(t, client, "add", "a.txt")
	gitT(t, client, "commit", "-qm", "first")
	writeT(t, filepath.Join(client, "a.txt"), "one\nuncommitted\n")
	writeT(t, filepath.Join(client, "new.txt"), "untracked\n")
	// filter in hooki uporabnika: prevzem veje jih ne sme zagnati
	gitT(t, client, "config", "filter.evil.smudge", "touch "+marks+"/smudge; cat")
	for _, h := range []string{"reference-transaction", "post-checkout", "post-merge"} {
		writeT(t, filepath.Join(client, ".git/hooks", h), "#!/bin/sh\ntouch "+marks+"/hook-"+h+"\n")
		os.Chmod(filepath.Join(client, ".git/hooks", h), 0o755)
	}
	status, head := gitT(t, client, "status", "--porcelain"), gitT(t, client, "rev-parse", "HEAD")
	config, _ := os.ReadFile(filepath.Join(client, ".git/config"))

	top := gitToplevel(filepath.Join(client))
	snap := gitSnapshot(top)
	if !shaRe.MatchString(snap) || snap == head {
		t.Fatalf("snapshot %q", snap)
	}
	b := filepath.Join(root, "push.bundle")
	if made, err := gitBundle(top, snap, nil, b); !made || err != nil {
		t.Fatalf("bundle: %v %v", made, err)
	}
	mirror, wt := filepath.Join(root, "mirror.git"), filepath.Join(root, "wt")
	gitT(t, root, "init", "-q", "--bare", mirror)
	gitT(t, mirror, "fetch", "-q", b, "+refs/minigw/push:refs/minigw/base/x")
	if made, _ := gitBundle(top, snap, []string{snap}, filepath.Join(root, "again.bundle")); made {
		t.Fatal("router already has the snapshot: nothing to send")
	}
	gitT(t, root, "clone", "-q", "--shared", "--no-checkout", mirror, wt)
	branch := "minigw/claude-0123abcd-0"
	gitT(t, wt, "checkout", "-q", "-b", branch, snap)
	writeT(t, filepath.Join(wt, ".gitattributes"), "* filter=evil\n")
	writeT(t, filepath.Join(wt, ".gitmodules"), "[submodule \"x\"]\n\tpath = x\n\turl = file://"+marks+"/evil\n")
	gitT(t, wt, "add", "-A")
	gitT(t, wt, "commit", "-qm", "agent")
	result := filepath.Join(root, "result.bundle")
	gitT(t, wt, "bundle", "create", result, "refs/heads/"+branch, "^"+snap)

	if why := gitFetchBranch(top, result, branch); why != "" {
		t.Fatal(why)
	}
	if gitT(t, client, "show", branch+":.gitattributes") != "* filter=evil" || gitT(t, client, "show", branch+"~1:new.txt") != "untracked" {
		t.Fatal("branch content")
	}
	if gitFetchBranch(top, result, "main") == "" || gitFetchBranch(top, result, "minigw/../main") == "" {
		t.Fatal("a ref outside minigw/ was accepted")
	}

	// Drevo z ».git« (sestavi ga agent z mktree): fsck ga zavrne, veja ostane.
	blob := gitT(t, wt, "hash-object", "-w", "--stdin")
	mk := func(in string) string {
		cmd := exec.Command("git", "-C", wt, "mktree")
		cmd.Stdin = strings.NewReader(in)
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	evil := mk("040000 tree " + mk("100644 blob "+blob+"\tpost-checkout\n") + "\t.git\n")
	c := gitT(t, wt, "commit-tree", evil, "-p", "HEAD", "-m", "dot git")
	gitT(t, wt, "update-ref", "refs/heads/"+branch, c)
	gitT(t, wt, "bundle", "create", result, "refs/heads/"+branch, "^"+snap)
	if gitFetchBranch(top, result, branch) == "" {
		t.Fatal("a tree with .git passed fsck")
	}

	if m, _ := os.ReadDir(marks); len(m) != 0 {
		t.Fatalf("something ran: %v", m)
	}
	if now, _ := os.ReadFile(filepath.Join(client, ".git/config")); string(now) != string(config) {
		t.Fatal(".git/config changed")
	}
	if gitT(t, client, "status", "--porcelain") != status || gitT(t, client, "rev-parse", "HEAD") != head {
		t.Fatal("the user's files or HEAD changed")
	}
	if _, err := os.Stat(filepath.Join(client, ".git/FETCH_HEAD")); err == nil {
		t.Fatal("FETCH_HEAD written")
	}
	if refs := gitT(t, client, "for-each-ref", "refs/minigw"); refs != "" {
		t.Fatalf("temporary ref left: %s", refs)
	}
}

func TestWorkCandidatesAndReadOnly(t *testing.T) {
	cases := []struct {
		work         string
		ro, smb, git bool
		want         string
	}{
		{"auto", true, true, true, "direct"},
		{"auto", false, true, true, "native,worktree,sync"},
		{"auto", false, false, true, "worktree,sync"},
		{"auto", false, false, false, "sync,direct"},
		{"worktree", false, false, false, "direct"},
		{"worktree", false, false, true, "worktree,sync"},
		{"sync", false, false, true, "sync"},
		{"native-direct", false, true, true, "native-direct,direct"},
	}
	for _, c := range cases {
		if got := strings.Join(workCandidates(c.work, c.ro, c.smb, c.git), ","); got != c.want {
			t.Errorf("%+v: %s", c, got)
		}
	}
	if w, ro := workMode("", &sharedFolder{Path: "/p", Work: "worktree"}, true); w != "direct" || !ro {
		t.Errorf("worktree read-only: %s %v", w, ro)
	}
	if w, ro := workMode("native-direct", nil, true); w != "direct" || !ro {
		t.Errorf("native-direct read-only: %s %v", w, ro)
	}
	if w, _ := workMode("auto", &sharedFolder{Path: "/p", Work: "sync"}, false); w != "auto" {
		t.Errorf("flag wins: %s", w)
	}
	// Brez izbranega načina auto (kot z Windows/Linux); izrecni direct in shranjen način ostaneta.
	if w, ro := workMode("", nil, false); w != "auto" || ro {
		t.Errorf("default: %s %v", w, ro)
	}
	if w, _ := workMode("direct", nil, false); w != "direct" {
		t.Errorf("explicit direct: %s", w)
	}
	if w, _ := workMode("", &sharedFolder{Path: "/p", Work: "direct"}, false); w != "direct" {
		t.Errorf("saved direct: %s", w)
	}
	if w, ro := workMode("", &sharedFolder{Path: "/p", ReadOnly: true}, false); w != "auto" || !ro ||
		workCandidates(w, ro, false, true)[0] != "direct" {
		t.Errorf("saved --ro: %s %v", w, ro)
	}
	// M1: auto (privzeti ali izrecni) le v korenu repozitorija; izrecni worktree tudi v nadrejenem.
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	os.Mkdir(sub, 0o755)
	if !repoRoot(dir, dir) || repoRoot(dir, sub) || repoRoot("", dir) {
		t.Error("repoRoot")
	}
	if out, err := exec.Command("git", "init", "-q", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	if workTop("auto", dir) == "" || workTop("auto", sub) != "" || workTop("worktree", sub) == "" || workTop("sync", dir) != "" {
		t.Errorf("workTop: %q %q %q", workTop("auto", dir), workTop("auto", sub), workTop("worktree", sub))
	}
}

// M4: le git bundle; L5: veja, na katero je uporabnik commital, ostane njegova (brez sile).
func TestWorktreeFetchChecksBundleAndKeepsUserCommits(t *testing.T) {
	root := t.TempDir()
	client, other := filepath.Join(root, "client"), filepath.Join(root, "other")
	os.MkdirAll(client, 0o755)
	gitT(t, client, "init", "-q", "-b", "main")
	writeT(t, filepath.Join(client, "a.txt"), "a\n")
	gitT(t, client, "add", "-A")
	gitT(t, client, "commit", "-qm", "a")
	gitT(t, root, "clone", "-q", client, other) // »router«: veje agenta in bundli
	branch := "minigw/claude-0123abcd-0"
	notBundle := filepath.Join(root, "x.bundle")
	writeT(t, notBundle, "not a bundle\n")
	if why := gitFetchBranch(client, notBundle, branch); why != "the gateway did not send a git bundle" {
		t.Fatalf("not a bundle: %q", why)
	}
	if gitFetchBranch(client, filepath.Join(client, ".git"), branch) == "" {
		t.Fatal("a repository path was fetched as a bundle")
	}

	base := gitT(t, other, "rev-parse", "HEAD")
	agent := func(msg string) string { // vrh agenta na base, kot bundle veje
		c := gitT(t, other, "commit-tree", gitT(t, other, "rev-parse", "HEAD^{tree}"), "-p", base, "-m", msg)
		gitT(t, other, "update-ref", "refs/heads/"+branch, c)
		b := filepath.Join(root, msg+".bundle")
		gitT(t, other, "bundle", "create", b, "refs/heads/"+branch, "^"+base)
		return b
	}
	b1 := agent("one")
	if why := gitFetchBranch(client, b1, branch); why != "" {
		t.Fatal(why)
	}
	mine := gitT(t, client, "commit-tree", gitT(t, client, "rev-parse", branch+"^{tree}"), "-p", branch, "-m", "mine")
	gitT(t, client, "update-ref", "refs/heads/"+branch, mine)
	if why := gitFetchBranch(client, agent("two"), branch); !strings.Contains(why, "kept yours") {
		t.Fatalf("non-fast-forward: %q", why)
	}
	if gitT(t, client, "rev-parse", branch) != mine {
		t.Fatal("the user's commit was overwritten")
	}

	// N3: veja, izvlečena v povezani delovni mapi, se ne premakne (git fetch brez sile).
	gitT(t, other, "fetch", "-q", client, "+refs/heads/"+branch+":refs/heads/"+branch) // »router« ima zdaj uporabnikov vrh …
	linked := filepath.Join(root, "linked")
	gitT(t, client, "worktree", "add", "-q", linked, branch)
	c := gitT(t, other, "commit-tree", gitT(t, other, "rev-parse", "HEAD^{tree}"), "-p", mine, "-m", "three")
	gitT(t, other, "update-ref", "refs/heads/"+branch, c) // … in hitro posodobitev nanj
	b3 := filepath.Join(root, "three.bundle")
	gitT(t, other, "bundle", "create", b3, "refs/heads/"+branch, "^"+base)
	if why := gitFetchBranch(client, b3, branch); !strings.Contains(why, "checked out") {
		t.Fatalf("checked out in a linked worktree: %q", why)
	}
	if gitT(t, client, "rev-parse", branch) != mine {
		t.Fatal("a branch checked out in a linked worktree was moved")
	}
}

// #16: posnetek vzame nove datoteke, ne pa prezrtih (.gitignore, .git/info/exclude, globalni core.excludesFile).
// #10: repozitorij brez commita nima posnetka (auto in worktree tedaj takoj na sync).
func TestSnapshotSkipsIgnoredFilesAndNeedsACommit(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "repo")
	os.MkdirAll(repo, 0o755)
	writeT(t, filepath.Join(root, "global-ignore"), ".omc/\n")
	writeT(t, filepath.Join(root, "gitconfig"), "[core]\n\texcludesFile = "+filepath.ToSlash(filepath.Join(root, "global-ignore"))+"\n")
	t.Setenv("GIT_CONFIG_GLOBAL", filepath.Join(root, "gitconfig"))
	gitT(t, repo, "init", "-q", "-b", "main")
	if gitHasCommit(repo) || gitSnapshot(repo) != "" {
		t.Fatal("snapshot without a commit")
	}
	writeT(t, filepath.Join(repo, ".gitignore"), "build/\n")
	gitT(t, repo, "add", ".gitignore")
	gitT(t, repo, "commit", "-qm", "first")
	writeT(t, filepath.Join(repo, ".git/info/exclude"), "secret.txt\n")
	for _, f := range []string{"new.txt", "build/out.o", ".omc/state.json", "secret.txt"} {
		writeT(t, filepath.Join(repo, f), "x\n")
	}
	snap := gitSnapshot(repo)
	if !gitHasCommit(repo) || !shaRe.MatchString(snap) {
		t.Fatalf("snapshot %q", snap)
	}
	if got := gitT(t, repo, "ls-tree", "-r", "--name-only", snap); got != ".gitignore\nnew.txt" {
		t.Fatalf("snapshot has %q", got)
	}
	// Neprezrt ugnezden repozitorij brez commita: ni posnetka, razlog ga imenuje (ne zunanjega, ime očiščeno); prezrt ali
	// s commitom gre (gitlink).
	if why := snapshotBlocker(repo); why != "" {
		t.Fatalf("blocker without nested repo: %q", why)
	}
	gitT(t, filepath.Join(repo, "build"), "init", "-q")
	if why := snapshotBlocker(repo); why != "" {
		t.Fatalf("ignored nested repo: %q", why)
	}
	lib := filepath.Join(repo, "vendor", "lib")
	os.MkdirAll(lib, 0o755)
	gitT(t, lib, "init", "-q")
	if why := snapshotBlocker(repo); gitSnapshot(repo) != "" || !strings.Contains(why, "nested repository vendor/lib in ") {
		t.Fatalf("nested repo without commit: %q", why)
	}
	writeT(t, filepath.Join(lib, "a.txt"), "x\n")
	gitT(t, lib, "add", "a.txt")
	gitT(t, lib, "commit", "-qm", "lib")
	if why := snapshotBlocker(repo); why != "" || !shaRe.MatchString(gitSnapshot(repo)) {
		t.Fatalf("nested repo with a commit: %q", why)
	}
	os.RemoveAll(filepath.Join(repo, "vendor"))
	deep := filepath.Join(repo, "new", "x\ny") // globoko v nesledeni mapi, ime z novo vrstico
	os.MkdirAll(deep, 0o755)
	writeT(t, filepath.Join(repo, "new", "f.txt"), "x\n")
	gitT(t, deep, "init", "-q")
	if why := snapshotBlocker(repo); !strings.Contains(why, "nested repository new/x y in ") {
		t.Fatalf("deep nested repo: %q", why)
	}
}

// M1 (auto-default): vrstica o veji kaže le delo agenta — od posnetka (neshranjene spremembe in nesledene datoteke
// uporabnika), ne od HEAD; pozneje poslan posnetek, ki ni prednik veje, se ne uporabi.
func TestBranchLineShowsAgentOnlyRange(t *testing.T) {
	top := t.TempDir()
	gitT(t, top, "init", "-q", "-b", "main")
	writeT(t, filepath.Join(top, "a.txt"), "a\n")
	gitT(t, top, "add", "-A")
	gitT(t, top, "commit", "-qm", "a")
	writeT(t, filepath.Join(top, "a.txt"), "mine\n")     // neshranjena sprememba
	writeT(t, filepath.Join(top, "secret.key"), "key\n") // nesledena datoteka
	snap := gitSnapshot(top)
	if snap == "" || snap == gitT(t, top, "rev-parse", "HEAD") {
		t.Fatalf("snapshot %q", snap)
	}
	branch := "minigw/codex-0123abcd-0"
	blob := gitT(t, top, "hash-object", "-w", "--stdin") // prazen agent.txt na drevesu posnetka
	ls := gitT(t, top, "ls-tree", snap) + "\n100644 blob " + blob + "\tagent.txt\n"
	mk := exec.Command("git", "-C", top, "mktree")
	mk.Stdin = strings.NewReader(ls)
	out, err := mk.Output()
	if err != nil {
		t.Fatal(err)
	}
	tip := gitT(t, top, "commit-tree", strings.TrimSpace(string(out)), "-p", snap, "-m", "agent")
	gitT(t, top, "update-ref", "refs/heads/"+branch, tip)
	writeT(t, filepath.Join(top, "a.txt"), "later\n")
	later := gitSnapshot(top) // druga zahteva, drug agent: ni prednik te veje

	base := agentBase(top, branch, []string{snap, later})
	if base != snap {
		t.Fatalf("base %s, want snapshot %s", base, snap)
	}
	if d := gitT(t, top, "diff", "--name-only", base+".."+branch); d != "agent.txt" {
		t.Fatalf("agent-only range has %q", d)
	}
	line := branchLine(branch, base)
	if !strings.Contains(line, "git diff "+snap+".."+branch) || !strings.Contains(line, "git cherry-pick "+snap+".."+branch) ||
		strings.Contains(line, "HEAD") {
		t.Fatal(line)
	}
	if agentBase(top, branch, []string{later}) != "HEAD" {
		t.Fatal("unrelated snapshot used as base")
	}
}

// R1: veja iz sporočila prehoda (native) le, če obstaja in je začetek njen prednik.
func TestDeliveredBranchChecksAncestry(t *testing.T) {
	top := t.TempDir()
	gitT(t, top, "init", "-q", "-b", "main")
	writeT(t, filepath.Join(top, "a.txt"), "a\n")
	gitT(t, top, "add", "-A")
	gitT(t, top, "commit", "-qm", "a")
	base := gitT(t, top, "rev-parse", "HEAD")
	branch := "minigw/claude-0123abcd-1"
	tip := gitT(t, top, "commit-tree", gitT(t, top, "rev-parse", "HEAD^{tree}"), "-p", base, "-m", "agent")
	other := gitT(t, top, "commit-tree", gitT(t, top, "rev-parse", "HEAD^{tree}"), "-m", "unrelated")
	if deliveredBranch(top, branch, base) {
		t.Fatal("missing branch accepted")
	}
	gitT(t, top, "update-ref", "refs/heads/"+branch, tip)
	if !deliveredBranch(top, branch, base) || deliveredBranch(top, branch, other) || deliveredBranch(top, "main", base) ||
		deliveredBranch(top, branch, "HEAD") {
		t.Fatal("deliveredBranch")
	}
}
