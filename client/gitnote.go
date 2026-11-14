package main

import "strings"

// Git limits of a run, told to the agent in its system note (#112; the Mac's GitNote in GitNote.swift says the same, client/testdata/git-notes.json is
// asserted by both). The lists are computed from the share guard's rules (protectedPathFor), not written by hand: a command is "possible" when every
// path it writes in .git is writable in direct mode, "not possible" when every one is refused.
type gitOp struct {
	name  string
	paths []string // paths inside .git the command writes
}

var gitOps = []gitOp{
	{"add", []string{"index.lock", "index", "objects/ab/cdef"}},
	{"commit", []string{"index.lock", "COMMIT_EDITMSG", "refs/heads/x", "logs/HEAD", "objects/ab/cdef"}},
	{"commit --amend", []string{"index.lock", "COMMIT_EDITMSG", "refs/heads/x", "logs/HEAD", "ORIG_HEAD"}},
	{"branch", []string{"refs/heads/x", "logs/refs/heads/x"}},
	{"tag", []string{"refs/tags/x", "TAG_EDITMSG"}},
	{"checkout", []string{"HEAD", "index.lock", "logs/HEAD"}},
	{"stash", []string{"refs/stash", "index.stash.123", "logs/refs/stash"}},
	{"merge", []string{"MERGE_HEAD", "MERGE_MSG", "MERGE_MODE", "ORIG_HEAD", "AUTO_MERGE"}},
	{"single cherry-pick/revert", []string{"CHERRY_PICK_HEAD", "REVERT_HEAD", "AUTO_MERGE", "MERGE_MSG"}},
	{"rebase", []string{"rebase-merge/head-name", "rebase-apply/patch"}},
	{"multi-commit cherry-pick/revert", []string{"sequencer/todo"}},
	{"git worktree add", []string{"worktrees/x/HEAD", "worktrees/x/gitdir"}},
	{"changing .git/config, hooks, info/attributes or remotes", []string{"config.lock", "hooks/pre-commit", "info/attributes", "remotes/origin"}},
}

// gitDirectLists: the commands that work in direct mode and those that do not, from the guard's rules.
func gitDirectLists() (works, refused []string) {
	for _, op := range gitOps {
		w, r := true, true
		for _, p := range op.paths {
			if protectedPathFor(".git/"+p, true) {
				w = false
			} else {
				r = false
			}
		}
		if w {
			works = append(works, op.name)
		} else if r {
			refused = append(refused, op.name)
		}
	}
	return
}

// gitNote: the git sentence of the agent's note. mode: direct, sync or worktree ("" for the worktree: its note already says how commits work);
// shell: the agent can run commands; readOnly: --minigw-ro or a cap.
func gitNote(mode string, shell, readOnly bool) string {
	switch {
	case !shell:
		return "Git: you have no shell here, so no git commands, and the file tools cannot touch .git. If the task needs a commit, say so; the user commits your changes."
	case readOnly:
		return "Git: the folder is read-only for you: no changes and no commits."
	case mode == "sync":
		return "Git: there is no .git in this copy (git data is not copied), so git commands do not see the user's repository and nothing you commit goes back. Do not run git init or git commit here; the changed files are copied back and the user commits them."
	case mode == "direct":
		works, refused := gitDirectLists()
		return "Git: you can commit in this folder: " + strings.Join(works, ", ") + ". Not possible in this folder: " + strings.Join(refused, ", ") +
			". Use merge instead of rebase. If the task needs one of these, say so and tell the user to run it themselves or to use worktree mode (minigw --minigw-work=worktree)."
	}
	return ""
}

// gitRefusalHint: where a refused git path has an alternative, the refusal names it (the client's stderr line and the 403 text).
func gitRefusalHint(rel string) string {
	c := strings.Split(strings.ToLower(rel), "/")
	for i, n := range c {
		if n != ".git" {
			continue
		}
		if i+1 >= len(c) {
			return ""
		}
		switch c[i+1] {
		case "rebase-merge", "rebase-apply":
			return "rebase is not available in direct mode; use merge, or worktree mode"
		case "sequencer":
			return "a cherry-pick or revert of several commits is not available in direct mode; do them one by one, or use worktree mode"
		case "worktrees":
			return "git worktree is not available in direct mode; use worktree mode (minigw --minigw-work=worktree)"
		}
	}
	return ""
}
