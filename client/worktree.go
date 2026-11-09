package main

// --minigw-work=worktree (kot Worktree v Sources/RouterCore/Worktree.swift): posnetek stanja kot git bundle k routerju,
// veja agenta nazaj le kot ref (fsck, brez hookov, podmodulov, FETCH_HEAD in checkouta).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// gitUser: git uporabnika (njegove nastavitve), brez njegovih hookov ob začasnem refu in posnetku.
func gitUser(dir string, env []string, args ...string) (string, error) {
	cmd := command("git", append([]string{"-c", "core.hooksPath=" + os.DevNull}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), append([]string{"GIT_TERMINAL_PROMPT=0"}, env...)...)
	var out bytes.Buffer
	cmd.Stdout = &out
	err := cmd.Run()
	return strings.TrimSpace(out.String()), err
}

var shaRe = regexp.MustCompile(`^[0-9a-f]{40}$`)
var branchRe = regexp.MustCompile(`^minigw/[a-z]+-[0-9a-f]{8}-[0-9]+$`)

func gitToplevel(cwd string) string {
	top, err := gitUser(cwd, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return ""
	}
	return filepath.FromSlash(top)
}

// repoRoot: top je ta mapa (worktree le v korenu repozitorija: nadrejeni, npr. dotfiles v domači mapi ali monorepo,
// bi poslal tudi datoteke zunaj nje; M1).
func repoRoot(top, cwd string) bool {
	rt, rc := realPath(top), realPath(cwd)
	return top != "" && (rt == rc || runtime.GOOS == "windows" && strings.EqualFold(rt, rc))
}

// workTop: repozitorij za worktree; auto le v korenu repozitorija (podmapa → sync), izrecni worktree tudi nadrejeni.
func workTop(work, cwd string) string {
	if work != "auto" && work != "worktree" {
		return ""
	}
	top := gitToplevel(cwd)
	if work == "auto" && !repoRoot(top, cwd) {
		return ""
	}
	return top
}

// gitHasCommit: repozitorij ima HEAD (brez njega ni posnetka za worktree).
func gitHasCommit(top string) bool {
	head, err := gitUser(top, nil, "rev-parse", "--verify", "-q", "HEAD^{commit}")
	return err == nil && shaRe.MatchString(head)
}

// snapshotBlocker: zakaj ni posnetka ("" = je): repozitorij brez commita ali neprezrt ugnezden repozitorij brez commita
// (git init; git add -A ga ne more dodati), zato sync (kot Swift Worktree.snapshotBlocker). Prezrte poti (build/_deps) gredo.
func snapshotBlocker(top string) string {
	if !gitHasCommit(top) {
		return "the git repository " + oneLine(top) + " has no commit yet, so worktree mode is not possible"
	}
	if p := nestedNoCommit(top); p != "" {
		return "the nested repository " + oneLine(p) + " in " + oneLine(top) + " has no commit (commit in it or add it to .gitignore), so worktree mode is not possible"
	}
	return ""
}

// nestedNoCommit: prvi neprezrt ugnezden repozitorij brez commita v top, relativna pot; "" = ni ga. Nesledene mape git
// strne (--directory), zato le v njih poiščemo .git po datotečnem sistemu in vprašamo check-ignore.
func nestedNoCommit(top string) string {
	var deep []string
	for _, d := range gitZ(top, "", "ls-files", "-o", "--exclude-standard", "--directory", "--no-empty-directory", "-z") {
		if !strings.HasSuffix(d, "/") {
			continue
		}
		if _, err := os.Lstat(filepath.Join(top, filepath.FromSlash(d), ".git")); err == nil {
			if !gitHasCommit(filepath.Join(top, filepath.FromSlash(d))) {
				return strings.TrimSuffix(d, "/")
			}
			continue
		}
		filepath.WalkDir(filepath.Join(top, filepath.FromSlash(d)), func(p string, e os.DirEntry, err error) error {
			if err == nil && e.Name() == ".git" {
				if rel, err := filepath.Rel(top, filepath.Dir(p)); err == nil {
					deep = append(deep, filepath.ToSlash(rel)+"/")
				}
				if e.IsDir() {
					return filepath.SkipDir
				}
			}
			return nil
		})
	}
	if len(deep) == 0 {
		return ""
	}
	ignored := map[string]bool{}
	for _, p := range gitZ(top, strings.Join(deep, "\x00")+"\x00", "check-ignore", "--stdin", "-z") {
		ignored[p] = true
	}
	for _, p := range deep {
		if !ignored[p] && !gitHasCommit(filepath.Join(top, filepath.FromSlash(p))) {
			return strings.TrimSuffix(p, "/")
		}
	}
	return ""
}

// gitZ: git v top (brez hookov) z izhodom -z, razdeljenim na NUL (brez TrimSpace: imena s presledki ostanejo).
func gitZ(top, stdin string, args ...string) []string {
	cmd := command("git", append([]string{"-c", "core.hooksPath=" + os.DevNull, "-c", "core.fsmonitor=false"}, args...)...)
	cmd.Dir = top
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	cmd.Stdin = strings.NewReader(stdin)
	out, _ := cmd.Output()
	var r []string
	for _, f := range strings.Split(string(out), "\x00") {
		if f != "" {
			r = append(r, f)
		}
	}
	return r
}

// gitSnapshot: HEAD ali commit z neshranjenimi spremembami (začasni indeks: indeks, veje in mapa uporabnika ostanejo).
func gitSnapshot(top string) string {
	head, err := gitUser(top, nil, "rev-parse", "--verify", "-q", "HEAD^{commit}")
	if err != nil || !shaRe.MatchString(head) {
		return ""
	}
	dir, err := os.MkdirTemp("", "minigw-") // 0700
	if err != nil {
		return ""
	}
	defer os.RemoveAll(dir)
	env := []string{"GIT_INDEX_FILE=" + filepath.Join(dir, "index")}
	if _, err := gitUser(top, env, "read-tree", "HEAD"); err != nil {
		return ""
	}
	if _, err := gitUser(top, env, "add", "-A"); err != nil {
		return ""
	}
	tree, err := gitUser(top, env, "write-tree")
	if err != nil || !shaRe.MatchString(tree) {
		return ""
	}
	if t, _ := gitUser(top, nil, "rev-parse", "HEAD^{tree}"); t == tree {
		return head
	}
	c, err := gitUser(top, nil, "-c", "user.name=minigw", "-c", "user.email=minigw@localhost", "commit-tree", tree, "-p", head,
		"-m", "minigw: your uncommitted changes")
	if err != nil || !shaRe.MatchString(c) {
		return ""
	}
	return c
}

// gitBundle: bundle posnetka brez objektov, ki jih router ima. made=false: ni česa poslati.
func gitBundle(top, snapshot string, have []string, file string) (made bool, err error) {
	ref := "refs/minigw/push"
	if _, err := gitUser(top, nil, "update-ref", ref, snapshot); err != nil {
		return false, err
	}
	defer gitUser(top, nil, "update-ref", "-d", ref)
	args := []string{"bundle", "create", file, ref}
	for _, h := range have {
		if !shaRe.MatchString(h) {
			continue
		}
		if h == snapshot {
			return false, nil
		}
		if _, err := gitUser(top, nil, "cat-file", "-e", h+"^{commit}"); err == nil {
			args = append(args, "^"+h)
		}
	}
	if _, err := gitUser(top, nil, args...); err != nil {
		if _, e := os.Stat(file); e != nil {
			return false, nil // »Refusing to create empty bundle«
		}
		return false, err
	}
	return true, nil
}

// isBundle: datoteka je git bundle (v2 ali v3), ne npr. pot do repozitorija.
func isBundle(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	b := make([]byte, 32)
	n, _ := io.ReadFull(f, b)
	line, _, _ := strings.Cut(string(b[:n]), "\n")
	return line == "# v2 git bundle" || line == "# v3 git bundle"
}

// gitFetchBranch: le objekti in refs/heads/<veja> (fsck), .git/config nespremenjen. Brez sile: če ima veja tukaj commite,
// ki jih agentova nima, ostane uporabnikova. "" = uspeh.
func gitFetchBranch(top, bundle, branch string) string {
	if !branchRe.MatchString(branch) {
		return "invalid branch name from the gateway"
	}
	if !isBundle(bundle) {
		return "the gateway did not send a git bundle"
	}
	if cur, _ := gitUser(top, nil, "symbolic-ref", "-q", "HEAD"); cur == "refs/heads/"+branch {
		return branch + " is checked out here; not updated"
	}
	_, err := gitUser(top, nil, "-c", "fetch.fsckObjects=true", "-c", "transfer.fsckObjects=true",
		"-c", "fetch.recurseSubmodules=false", "-c", "submodule.recurse=false", "-c", "fetch.writeCommitGraph=false",
		"-c", "maintenance.auto=false", "-c", "gc.auto=0",
		"fetch", "--no-tags", "--no-write-fetch-head", "--no-recurse-submodules", "--quiet",
		bundle, "+refs/heads/"+branch+":refs/minigw/incoming")
	defer gitUser(top, nil, "update-ref", "-d", "refs/minigw/incoming")
	nw, _ := gitUser(top, nil, "rev-parse", "-q", "--verify", "refs/minigw/incoming^{commit}")
	if err != nil || !shaRe.MatchString(nw) {
		return "git fetch of " + branch + " failed (the gateway's branch was rejected)"
	}
	// Premik veje z git fetch brez sile: git sam zavrne nehitro posodobitev in vejo, izvlečeno v kateri koli delovni mapi.
	if _, e := gitUser(top, nil, "-c", "fetch.recurseSubmodules=false", "-c", "submodule.recurse=false", "-c", "maintenance.auto=false",
		"-c", "gc.auto=0", "fetch", "--no-tags", "--no-write-fetch-head", "--no-recurse-submodules", "--quiet",
		".", "refs/minigw/incoming:refs/heads/"+branch); e == nil {
		return ""
	}
	old, _ := gitUser(top, nil, "rev-parse", "-q", "--verify", "refs/heads/"+branch)
	if shaRe.MatchString(old) {
		if _, e := gitUser(top, nil, "merge-base", "--is-ancestor", old, nw); e != nil {
			return branch + " has commits here that the agent's version does not have; kept yours, the agent's work was not fetched"
		}
	}
	return "cannot update " + branch + " (is it checked out in a worktree?)"
}

// wtPush: WORKTREE-PUSH — posnetek, bundle po delih do 32 MB, odgovor {"snapshot"}.
func (s *shareClient) wtPush(k string, job davJob) davResponse {
	var req struct{ Have []string }
	json.Unmarshal(job.Body, &req)
	snap := gitSnapshot(s.top)
	if snap == "" {
		why := snapshotBlocker(s.top)
		if why == "" {
			why = "git could not record the folder's state"
		}
		if k := "snapshot:" + why; !s.reported[k] {
			s.reported[k] = true
			s.note("worktree: cannot take a snapshot: " + why + "; use sync (MINIGW_WORK=sync)")
		}
		return dav(500, nil, nil)
	}
	dir, err := os.MkdirTemp("", "minigw-") // 0700
	if err != nil {
		return dav(500, nil, nil)
	}
	defer os.RemoveAll(dir)
	name := filepath.Join(dir, "push.bundle")
	made, err := gitBundle(s.top, snap, req.Have, name)
	if err != nil {
		s.note("worktree: git bundle failed")
		return dav(500, nil, nil)
	}
	if made {
		b, err := os.Open(name)
		if err != nil {
			return dav(500, nil, nil)
		}
		defer b.Close()
		buf := make([]byte, 32<<20)
		for part := 0; ; part++ {
			n, rerr := io.ReadFull(b, buf)
			if n > 0 {
				if st, _ := call(s.c, "PUT", "/minigw/worktree/bundle", map[string]string{"x-share-key": k, "x-part": strconv.Itoa(part)},
					buf[:n], 10*time.Minute); st != 204 {
					s.note("worktree: upload to the gateway failed")
					return dav(500, nil, nil)
				}
			}
			if rerr != nil {
				break
			}
		}
	}
	if s.snaps = append(s.snaps, snap); len(s.snaps) > 64 {
		s.snaps = s.snaps[1:]
	}
	body, _ := json.Marshal(map[string]string{"snapshot": snap})
	return dav(200, nil, body)
}

// wtPull: WORKTREE-PULL — bundle veje agenta in prevzem kot ref.
func (s *shareClient) wtPull(k string, job davJob) davResponse {
	var req struct{ Branch string }
	if json.Unmarshal(job.Body, &req) != nil || !branchRe.MatchString(req.Branch) {
		return dav(403, nil, nil)
	}
	st, data := call(s.c, "GET", "/minigw/worktree/result", map[string]string{"x-share-key": k}, nil, 10*time.Minute)
	if st != 200 {
		var e struct {
			Error struct{ Message string } `json:"error"`
		}
		why := fmt.Sprintf("HTTP %d", st)
		if json.Unmarshal(data, &e) == nil && e.Error.Message != "" {
			why = oneLine(e.Error.Message)
		}
		s.note(fmt.Sprintf("worktree: cannot get %s from the gateway: %s", req.Branch, why))
		return dav(500, nil, nil)
	}
	dir, err := os.MkdirTemp("", "minigw-") // 0700
	if err != nil {
		return dav(500, nil, nil)
	}
	defer os.RemoveAll(dir)
	name := filepath.Join(dir, "result.bundle")
	if err := os.WriteFile(name, data, 0o600); err != nil {
		return dav(500, nil, nil)
	}
	if why := gitFetchBranch(s.top, name, req.Branch); why != "" {
		s.note("worktree: " + why)
		return dav(500, nil, nil)
	}
	s.branchNote(req.Branch, agentBase(s.top, req.Branch, s.snaps))
	return dav(200, nil, nil)
}

// agentBase: začetek dela agenta — zadnji poslani posnetek, ki je prednik veje (HEAD z neshranjenimi spremembami in
// nesledenimi datotekami uporabnika; ti niso delo agenta). Brez njega HEAD. Prehodu ne verjame: le posnetki tega odjemalca.
func agentBase(top, branch string, sent []string) string {
	for i := len(sent) - 1; i >= 0; i-- {
		if _, err := gitUser(top, nil, "merge-base", "--is-ancestor", sent[i], "refs/heads/"+branch); err == nil {
			return sent[i]
		}
	}
	return "HEAD"
}

// deliveredBranch (R1): veja in začetek iz sporočila prehoda (native) le, če sta veljavna, veja obstaja v repozitoriju
// uporabnika in je začetek njen prednik.
func deliveredBranch(root, branch, base string) bool {
	if !branchRe.MatchString(branch) || !shaRe.MatchString(base) {
		return false
	}
	top := gitToplevel(root)
	if top == "" {
		return false
	}
	if _, err := gitUser(top, nil, "rev-parse", "--verify", "-q", "refs/heads/"+branch+"^{commit}"); err != nil {
		return false
	}
	_, err := gitUser(top, nil, "merge-base", "--is-ancestor", base, "refs/heads/"+branch)
	return err == nil
}

// branchLine: vrstica o dostavljeni veji (enaka v odjemalcu Swift): le delo agenta, brez posnetka uporabnika.
func branchLine(branch, base string) string {
	r := base + ".." + branch
	return "agent work is on branch " + branch + ": review with git diff " + r + " (" + base + " = your state when the agent started), " +
		"then bring it in with git cherry-pick " + r + " or git diff --binary " + r + " | git apply"
}

// branchNote: vrstica o veji enkrat na vejo in začetek v zagonu (pod syncMu).
func (s *shareClient) branchNote(branch, base string) {
	if k := "branch:" + branch + "@" + base; !s.reported[k] {
		s.reported[k] = true
		s.note(branchLine(branch, base))
	}
}

// workCandidates: načini po vrsti (kot SharedFolders.candidates): auto → native (shema SMB) → worktree (git) → sync;
// repozitorij git nikoli v direct za pisanje.
func workCandidates(work string, readOnly, smb, git bool) []string {
	switch {
	case work == "auto" && readOnly:
		return []string{"direct"}
	case work == "auto":
		var out []string
		if smb {
			out = append(out, "native")
		}
		if git { // repozitorij git nikoli v direct za pisanje (agenti tam ne morejo commitati, .git samo za branje)
			return append(out, "worktree", "sync")
		}
		return append(out, "sync", "direct")
	case work == "native" || work == "native-direct":
		return []string{work, "direct"}
	case work == "worktree" && git:
		return []string{"worktree", "sync"}
	case work == "worktree":
		return []string{"direct"}
	}
	return []string{work}
}
