package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"
)

// Direct mode lets the agent write git's own data (gitDirWritable), so the owner always sees what it did in the repository and is warned about what git could
// carry into the work tree later (#116 B1): at the start of the share the client takes the state of the repository, at the end it compares (HEAD, refs, index,
// merge state), prints one summary line, warns about protected paths in new commits or in the index (the staged ones it resets, never touching the work
// tree) and about a merge, cherry-pick or revert the agent left in progress. The Swift client does the same (GitWatch.swift); client/testdata/git-watch.json
// is asserted by both.
type gitState struct {
	Head    string            `json:"head"`    // branch HEAD is on ("" = detached or unborn)
	Refs    map[string]string `json:"refs"`    // refname → object id
	Index   map[string]string `json:"index"`   // path → "mode id stage"
	Pending []string          `json:"pending"` // MERGE_HEAD, CHERRY_PICK_HEAD, REVERT_HEAD that exist
	Cached  []string          `json:"cached"`  // end state only: paths where the index differs from HEAD (what is staged and not committed); nil = all
}

type gitCommit struct {
	ID    string   `json:"id"`
	Files []string `json:"files"`
}

const gitWatchNames = 20 // names shown per list

func gitNames(l []string) string {
	sort.Strings(l)
	if len(l) > gitWatchNames {
		return strings.Join(l[:gitWatchNames], ", ") + fmt.Sprintf(" and %d more", len(l)-gitWatchNames)
	}
	return strings.Join(l, ", ")
}

// gitWatchReport: the lines for the owner and the staged protected paths to reset. Pure: the fixture asserts it.
func gitWatchReport(start, end gitState, commits []gitCommit, isProtected func(string) bool) (lines []string, reset []string) {
	short := func(r string) string { return strings.TrimPrefix(strings.TrimPrefix(r, "refs/heads/"), "refs/tags/") }
	var newBranches, movedBranches, newTags, deleted []string
	for r, id := range end.Refs {
		old, had := start.Refs[r]
		switch {
		case strings.HasPrefix(r, "refs/heads/") && !had:
			newBranches = append(newBranches, short(r))
		case strings.HasPrefix(r, "refs/heads/") && old != id:
			movedBranches = append(movedBranches, short(r))
		case strings.HasPrefix(r, "refs/tags/") && !had:
			newTags = append(newTags, short(r))
		}
	}
	for r := range start.Refs {
		if _, ok := end.Refs[r]; !ok {
			deleted = append(deleted, short(r))
		}
	}
	var staged []string
	notCommitted := func(p string) bool { return end.Cached == nil || slices.Contains(end.Cached, p) } // a path the agent committed is in the index too
	for p, v := range end.Index {
		if start.Index[p] != v && notCommitted(p) {
			staged = append(staged, p)
		}
	}
	for p := range start.Index {
		if _, ok := end.Index[p]; !ok && notCommitted(p) {
			staged = append(staged, p)
		}
	}
	var parts []string
	if end.Head != start.Head {
		if end.Head == "" {
			parts = append(parts, "HEAD is detached")
		} else {
			parts = append(parts, "HEAD now on "+end.Head)
		}
	}
	if len(commits) > 0 {
		parts = append(parts, fmt.Sprintf("%d new commit(s)", len(commits)))
	}
	for _, p := range []struct {
		what string
		l    []string
	}{{"new branches", newBranches}, {"moved branches", movedBranches}, {"new tags", newTags}, {"deleted refs", deleted}} {
		if len(p.l) > 0 {
			parts = append(parts, p.what+": "+gitNames(p.l))
		}
	}
	if len(staged) > 0 {
		parts = append(parts, fmt.Sprintf("%d staged path(s)", len(staged)))
	}
	if len(parts) > 0 {
		lines = append(lines, "minigw: git in this folder, changed by the agent: "+strings.Join(parts, "; "))
	}
	var committed []string
	for _, c := range commits {
		for _, f := range c.Files {
			if isProtected(f) && !slices.Contains(committed, f) {
				committed = append(committed, f)
			}
		}
	}
	if len(committed) > 0 {
		lines = append(lines, "minigw: WARNING: the agent committed a protected path: "+gitNames(committed)+" - review before git checkout, reset --hard, restore, stash pop or your next commit")
	}
	for _, p := range staged {
		if isProtected(p) {
			reset = append(reset, p)
		}
	}
	sort.Strings(reset)
	if len(reset) > 0 {
		lines = append(lines, "minigw: WARNING: the agent staged a protected path: "+gitNames(slices.Clone(reset))+" - removed from the index for you (your files were not touched)")
	}
	var pending []string
	for _, p := range end.Pending {
		if !slices.Contains(start.Pending, p) {
			pending = append(pending, p)
		}
	}
	if len(pending) > 0 {
		lines = append(lines, "minigw: WARNING: a merge, cherry-pick or revert is in progress, started by the agent ("+strings.Join(pending, ", ")+"): finish it or cancel it (git merge --abort) after you have reviewed it")
	}
	return
}

// gitWatch: the repository of a direct-mode share, if the folder is the top of a repository with a plain .git folder (a gitdir: file is not watched).
type gitWatch struct {
	root  string
	start gitState
}

var gitWatchPins = []string{"--no-optional-locks", "-c", "core.fsmonitor=false", "-c", "core.hooksPath=" + os.DevNull, "-c", "core.attributesFile=" + os.DevNull,
	"-c", "core.quotePath=false", "-c", "diff.external=", "-c", "submodule.recurse=false"}

// gitWatchRun runs git with the guardrails (no hooks, no fsmonitor, no external diff, no optional locks) and at most 1 MB of output in 20 s.
func gitWatchRun(root string, input string, args ...string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := commandContext(ctx, "git", append(slices.Clone(gitWatchPins), args...)...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_PAGER=cat", "GIT_OPTIONAL_LOCKS=0", "GIT_EXTERNAL_DIFF=", "GIT_CONFIG_NOSYSTEM=1")
	if input != "" {
		cmd.Stdin = strings.NewReader(input)
	}
	out, err := cmd.StdoutPipe()
	if err != nil || cmd.Start() != nil {
		return "", false
	}
	b, _ := io.ReadAll(io.LimitReader(out, 1<<20))
	io.Copy(io.Discard, out)
	return string(b), cmd.Wait() == nil
}

func gitWatchState(root string) (gitState, bool) {
	st := gitState{Refs: map[string]string{}, Index: map[string]string{}}
	if fi, err := os.Lstat(filepath.Join(root, ".git")); err != nil || !fi.IsDir() {
		return st, false
	}
	refs, ok := gitWatchRun(root, "", "for-each-ref", "--format=%(refname) %(objectname)")
	if !ok {
		return st, false
	}
	for _, l := range strings.Split(refs, "\n") {
		if r, id, ok := strings.Cut(l, " "); ok {
			st.Refs[r] = id
		}
	}
	if h, ok := gitWatchRun(root, "", "symbolic-ref", "-q", "--short", "HEAD"); ok {
		st.Head = strings.TrimSpace(h)
	}
	idx, _ := gitWatchRun(root, "", "ls-files", "-s", "-z")
	for _, e := range strings.Split(idx, "\x00") {
		if meta, p, ok := strings.Cut(e, "\t"); ok {
			if f := strings.Fields(meta); len(f) == 3 {
				st.Index[p] = f[0] + " " + f[1] + " " + f[2]
			}
		}
	}
	cached, _ := gitWatchRun(root, "", "diff", "--cached", "--name-only", "--no-renames", "--no-ext-diff", "--no-textconv", "-z")
	st.Cached = []string{}
	for _, p := range strings.Split(cached, "\x00") {
		if p != "" {
			st.Cached = append(st.Cached, p)
		}
	}
	for _, n := range []string{"MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD"} {
		if _, err := os.Lstat(filepath.Join(root, ".git", n)); err == nil {
			st.Pending = append(st.Pending, n)
		}
	}
	return st, true
}

func gitWatchStart(root string) *gitWatch {
	if st, ok := gitWatchState(root); ok {
		return &gitWatch{root: root, start: st}
	}
	return nil
}

// report: at the end of the share. Prints to stderr; resets the staged protected paths in the index (plumbing: the work tree is not touched).
func (w *gitWatch) report() {
	end, ok := gitWatchState(w.root)
	if !ok {
		return
	}
	var tips, old []string
	for _, id := range end.Refs {
		tips = append(tips, id)
	}
	for _, id := range w.start.Refs {
		old = append(old, id)
	}
	var commits []gitCommit
	if len(tips) > 0 {
		args := append([]string{"log", "--no-renames", "--name-only", "--format=@@%H", "-n", "200"}, tips...)
		if len(old) > 0 {
			args = append(args, "--not")
			args = append(args, old...)
		}
		out, _ := gitWatchRun(w.root, "", args...)
		for _, l := range strings.Split(out, "\n") {
			switch {
			case strings.HasPrefix(l, "@@"):
				commits = append(commits, gitCommit{ID: l[2:]})
			case l != "" && len(commits) > 0:
				commits[len(commits)-1].Files = append(commits[len(commits)-1].Files, l)
			}
		}
	}
	lines, reset := gitWatchReport(w.start, end, commits, protectedPath)
	if len(reset) > 0 && !w.resetIndex(reset) {
		lines = append(lines, "minigw: could not remove the staged protected paths from the index: run git reset -- "+strings.Join(reset, " "))
	}
	for _, l := range lines {
		fmt.Fprintln(os.Stderr, l)
	}
}

// resetIndex puts the start state of the paths back into the index (git update-index --index-info: mode, id, stage, path; id 0 removes the entry).
func (w *gitWatch) resetIndex(paths []string) bool {
	var in bytes.Buffer
	for _, p := range paths {
		if strings.ContainsAny(p, "\n\x00") {
			return false
		}
		if v, ok := w.start.Index[p]; ok {
			f := strings.Fields(v)
			fmt.Fprintf(&in, "%s %s %s\t%s\n", f[0], f[1], f[2], p)
		} else {
			fmt.Fprintf(&in, "0 %s\t%s\n", strings.Repeat("0", 40), p)
		}
	}
	_, ok := gitWatchRun(w.root, in.String(), "update-index", "--index-info")
	return ok
}
