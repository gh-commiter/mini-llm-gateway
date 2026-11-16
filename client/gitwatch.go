package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
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
	HeadID  string            `json:"headId"`  // object id HEAD points to, also when it is detached ("" = unborn)
	Refs    map[string]string `json:"refs"`    // refname → object id
	Index   map[string]string `json:"index"`   // path → "mode id stage"
	Pending []string          `json:"pending"` // MERGE_HEAD, CHERRY_PICK_HEAD, REVERT_HEAD that exist
	Cached  []string          `json:"cached"`  // end state only: paths where the index differs from HEAD (what is staged and not committed); nil = all
	// IndexHash: sha256 of `git ls-files -s -z`. Partial (a start state read back from the store): Index holds only the protected paths, so the staged paths are
	// told only for those, and a changed IndexHash says that the index changed.
	IndexHash string `json:"indexHash"`
	Partial   bool   `json:"partial"`
	// Incomplete: end state only: what the check could not read in full (output over the limit, a git command that failed). Never a clean result: the report says so.
	Incomplete []string `json:"incomplete"`
}

type gitCommit struct {
	ID    string   `json:"id"`
	Files []string `json:"files"`
	Modes []string `json:"modes"` // "path old->new": the file mode or type changed in this commit (0.7.1: a typechange, 120000 -> 100644, is named)
}

const gitWatchNames = 20 // names shown per list

var (
	gitWatchMaxCommits       = 200     // new commits whose paths are checked
	gitWatchMaxOutput  int64 = 1 << 20 // bytes of one git command's output
)

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
	var newBranches, movedBranches, newTags, movedTags, otherRefs, replaced, deleted []string
	for r, id := range end.Refs {
		old, had := start.Refs[r]
		if strings.HasPrefix(r, "refs/replace/") && (!had || old != id) {
			replaced = append(replaced, strings.TrimPrefix(r, "refs/replace/"))
		}
		switch {
		case strings.HasPrefix(r, "refs/heads/") && !had:
			newBranches = append(newBranches, short(r))
		case strings.HasPrefix(r, "refs/heads/") && old != id:
			movedBranches = append(movedBranches, short(r))
		case strings.HasPrefix(r, "refs/tags/") && !had:
			newTags = append(newTags, short(r))
		case strings.HasPrefix(r, "refs/tags/") && old != id: // an existing tag now names another object: it can hide a commit that is already in the history
			movedTags = append(movedTags, short(r))
		case !strings.HasPrefix(r, "refs/heads/") && !strings.HasPrefix(r, "refs/tags/") && (!had || old != id): // every other namespace: stash, notes, replace, remotes, ...
			otherRefs = append(otherRefs, r)
		}
	}
	for r := range start.Refs {
		if _, ok := end.Refs[r]; !ok {
			deleted = append(deleted, short(r))
		}
	}
	var staged []string
	notCommitted := func(p string) bool { return end.Cached == nil || slices.Contains(end.Cached, p) } // a path the agent committed is in the index too
	counted := func(p string) bool { return !start.Partial || isProtected(p) }                         // a partial start state knows only the protected paths
	for p, v := range end.Index {
		if counted(p) && start.Index[p] != v && notCommitted(p) {
			staged = append(staged, p)
		}
	}
	for p := range start.Index {
		if _, ok := end.Index[p]; !ok && counted(p) && notCommitted(p) {
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
	} else if end.Head == "" && end.HeadID != start.HeadID {
		parts = append(parts, "detached HEAD moved") // no branch moves with it: only the object id says so
	}
	if len(commits) > 0 {
		parts = append(parts, fmt.Sprintf("%d new commit(s)", len(commits)))
	}
	for _, p := range []struct {
		what string
		l    []string
	}{{"new branches", newBranches}, {"moved branches", movedBranches}, {"new tags", newTags}, {"moved tags", movedTags}, {"other refs changed", otherRefs}, {"deleted refs", deleted}} {
		if len(p.l) > 0 {
			parts = append(parts, p.what+": "+gitNames(p.l))
		}
	}
	if len(staged) > 0 && !start.Partial {
		parts = append(parts, fmt.Sprintf("%d staged path(s)", len(staged)))
	} else if start.Partial && start.IndexHash != end.IndexHash {
		parts = append(parts, "the index changed")
	}
	if len(parts) > 0 {
		lines = append(lines, "minigw: git in this folder, changed by the agent: "+strings.Join(parts, "; "))
	}
	// 0.7.1: a mode or type change (a symbolic link that became a regular file, the executable bit) in a new commit or in the index is named, whatever else the run did
	var modeChanged []string
	for _, c := range commits {
		for _, m := range c.Modes {
			if !slices.Contains(modeChanged, m) {
				modeChanged = append(modeChanged, m)
			}
		}
	}
	if !start.Partial {
		for p, v := range end.Index {
			if sv, ok := start.Index[p]; ok {
				if sf, ef := strings.Fields(sv), strings.Fields(v); len(sf) == 3 && len(ef) == 3 && sf[2] == ef[2] && sf[0] != ef[0] {
					if m := p + " " + sf[0] + "->" + ef[0]; !slices.Contains(modeChanged, m) {
						modeChanged = append(modeChanged, m)
					}
				}
			}
		}
	}
	if len(modeChanged) > 0 {
		sort.Strings(modeChanged)
		lines = append(lines, "minigw: WARNING: the agent changed the mode or type of "+gitNames(modeChanged)+" (120000 is a symbolic link, 100755 an executable file): review it before git checkout, reset --hard, restore, stash pop or your next commit")
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
	var dashed []string
	for r, id := range end.Refs {
		if old, had := start.Refs[r]; (!had || old != id) && slices.ContainsFunc(strings.Split(r, "/"), func(c string) bool { return strings.HasPrefix(c, "-") }) {
			dashed = append(dashed, r)
		}
	}
	if len(dashed) > 0 { // the guard refuses them as loose refs, packed-refs is writable: a script of yours could read such a name as an option
		lines = append(lines, "minigw: WARNING: the agent created a ref with a name that starts with \"-\" ("+gitNames(dashed)+"): a script of yours could take it for an option - review it, then git update-ref -d <name>")
	}
	if len(replaced) > 0 {
		lines = append(lines, "minigw: WARNING: the agent created or changed a replacement ref ("+gitNames(replaced)+"): git shows other content than the real commits while it exists - review it, then git replace -d <name>")
	}
	if len(end.Incomplete) > 0 {
		lines = append(lines, "minigw: WARNING: the check of git in this folder is INCOMPLETE ("+strings.Join(end.Incomplete, "; ")+"): the absence of other warnings is not a clean result - review the repository before git checkout, reset --hard, restore, stash pop or your next commit")
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
	file  string // the stored copy of the start state (gitwatch_store.go)
}

// --no-replace-objects: the watch reads the real objects, never what a refs/replace entry says they are. core.commitGraph=false and core.multiPackIndex=false: the agent can
// write objects/info/commit-graph* and the multi-pack-index (git gc needs the first), and a forged one would show a commit's files as another's. The Swift watch has the same
// (Worktree.guardrails, GitWatch.pins); client/testdata/git-watch-pins.json is asserted by both.
var gitWatchPins = []string{"--no-replace-objects", "--no-optional-locks", "-c", "core.fsmonitor=false", "-c", "core.commitGraph=false", "-c", "core.multiPackIndex=false", "-c", "core.hooksPath=" + os.DevNull, "-c", "core.attributesFile=" + os.DevNull,
	"-c", "core.quotePath=false", "-c", "diff.external=", "-c", "submodule.recurse=false"}

var gitWatchEnv = []string{"GIT_TERMINAL_PROMPT=0", "GIT_PAGER=cat", "GIT_OPTIONAL_LOCKS=0", "GIT_NO_REPLACE_OBJECTS=1", "GIT_EXTERNAL_DIFF=", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull}

// gitWatchRun runs git with the guardrails (no hooks, no fsmonitor, no external diff, no optional locks, no replacements) and at most 1 MB of output in 20 s.
// ok is false when git failed or the output was longer than the limit (the part read is returned): a caller never takes it for the whole.
func gitWatchRun(root string, input string, args ...string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := commandContext(ctx, "git", append(slices.Clone(gitWatchPins), args...)...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), gitWatchEnv...)
	if input != "" {
		cmd.Stdin = strings.NewReader(input)
	}
	out, err := cmd.StdoutPipe()
	if err != nil || cmd.Start() != nil {
		return "", false
	}
	b, _ := io.ReadAll(io.LimitReader(out, gitWatchMaxOutput))
	n, _ := io.Copy(io.Discard, out)
	return string(b), cmd.Wait() == nil && n == 0
}

func gitWatchState(root string) (gitState, bool) {
	st := gitState{Refs: map[string]string{}, Index: map[string]string{}}
	if fi, err := os.Lstat(filepath.Join(root, ".git")); err != nil || !fi.IsDir() {
		return st, false
	}
	refs, ok := gitWatchRun(root, "", "for-each-ref", "--format=%(refname) %(objectname)") // every namespace: heads, tags, stash, notes, replace, packed refs too
	if !ok && refs == "" {
		return st, false
	}
	if !ok {
		st.Incomplete = append(st.Incomplete, "refs not read in full")
	}
	for _, l := range strings.Split(refs, "\n") {
		if r, id, ok := strings.Cut(l, " "); ok {
			st.Refs[r] = id
		}
	}
	if h, ok := gitWatchRun(root, "", "symbolic-ref", "-q", "--short", "HEAD"); ok {
		st.Head = strings.TrimSpace(h)
	}
	if id, ok := gitWatchRun(root, "", "rev-parse", "-q", "--verify", "HEAD"); ok { // also when HEAD is detached: no ref names that commit
		st.HeadID = strings.TrimSpace(id)
	}
	idx, ok := gitWatchRun(root, "", "ls-files", "-s", "-z")
	if !ok {
		st.Incomplete = append(st.Incomplete, "index not read in full")
	}
	sum := sha256.Sum256([]byte(idx))
	st.IndexHash = hex.EncodeToString(sum[:])
	for _, e := range strings.Split(idx, "\x00") {
		if meta, p, ok := strings.Cut(e, "\t"); ok {
			if f := strings.Fields(meta); len(f) == 3 {
				st.Index[p] = f[0] + " " + f[1] + " " + f[2]
			}
		}
	}
	cached, ok := gitWatchRun(root, "", "diff", "--cached", "--name-only", "--no-renames", "--no-ext-diff", "--no-textconv", "-z")
	if !ok {
		st.Incomplete = append(st.Incomplete, "staged paths not read in full")
	}
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
	gitWatchRecoverAtStart(root) // a run in this folder that did not finish (X2)
	if st, ok := gitWatchState(root); ok {
		w := &gitWatch{root: root, start: st}
		if !w.save() {
			fmt.Fprintln(os.Stderr, "minigw: git watch: the start state of this repository could not be stored (too many refs or no writable settings folder): if this client is killed, the report of what the agent did is lost")
		}
		return w
	}
	return nil
}

// gitWatchRecoverAtStart: the runs of this folder that did not finish, reported at the start of every share (not only a direct-mode one that watches the repository itself).
func gitWatchRecoverAtStart(root string) {
	for _, l := range gitWatchRecover(root, os.Getpid(), gitWatchPidAlive) {
		fmt.Fprintln(os.Stderr, l)
	}
}

// lines: the comparison of the stored start state with the repository now; the staged protected paths are taken out of the index (plumbing: the work tree is not touched).
func (w *gitWatch) lines() []string {
	l, _ := w.compare(nil, true)
	return l
}

// compare: lines and whether the check was complete. The history is read from every ref and from HEAD itself (a detached HEAD is on no ref), minus what the start
// state already had; merge commits are diffed against each parent (-m), so the paths a merge brings in show up.
// restore false (the share did not drain: a request may still be writing): nothing is taken out of the index, the staged protected paths are only reported.
func (w *gitWatch) compare(incomplete []string, restore bool) ([]string, bool) {
	end, ok := gitWatchState(w.root)
	if !ok { // the start was watchable, so this is not "nothing to watch": the repository cannot be read now (damaged refs, a removed .git)
		return []string{"minigw: WARNING: the check of git in this folder is INCOMPLETE (the repository could not be read at the end" + strings.Join(append([]string{""}, incomplete...), "; ") +
			"): review the repository before git checkout, reset --hard, restore, stash pop or your next commit"}, false
	}
	end.Incomplete = append(end.Incomplete, incomplete...)
	tips, old := gitWatchTips(end), gitWatchTips(w.start)
	var commits []gitCommit
	if len(tips) > 0 {
		// Framing that a file name cannot forge: the commit ids come alone (rev-list), then each commit's names alone (diff-tree, NUL-separated, raw, no header at all).
		args := append([]string{"rev-list", "-n", strconv.Itoa(gitWatchMaxCommits + 1)}, tips...)
		if len(old) > 0 {
			args = append(args, "--not")
			args = append(args, old...)
		}
		out, ok := gitWatchRun(w.root, "", args...)
		if !ok {
			end.Incomplete = append(end.Incomplete, "history not read in full")
		}
		ids := strings.Fields(out)
		if len(ids) > gitWatchMaxCommits {
			ids = ids[:gitWatchMaxCommits]
			end.Incomplete = append(end.Incomplete, fmt.Sprintf("more than %d new commits: only the first %d were checked", gitWatchMaxCommits, gitWatchMaxCommits))
		}
		for _, id := range ids {
			// -m: a merge is diffed against each parent; --root: the first commit has files too
			raw, ok := gitWatchRun(w.root, "", "diff-tree", "-m", "-r", "-z", "--raw", "--no-renames", "--root", "--no-commit-id", "--no-ext-diff", id)
			if !ok {
				end.Incomplete = append(end.Incomplete, "files of a commit not read in full")
			}
			files, modes := parseGitRaw(raw)
			commits = append(commits, gitCommit{ID: id, Files: files, Modes: modes})
		}
	}
	lines, reset := gitWatchReport(w.start, end, commits, protectedPath)
	if len(reset) > 0 && !restore {
		lines = append(lines, "minigw: the staged protected paths were NOT removed from the index (a request of the agent was still running): run git reset -- "+strings.Join(reset, " "))
	} else if len(reset) > 0 && !w.resetIndex(reset) {
		lines = append(lines, "minigw: could not remove the staged protected paths from the index: run git reset -- "+strings.Join(reset, " "))
	}
	return lines, len(end.Incomplete) == 0
}

// gitSpecialMode: the modes a mounted folder cannot keep: a symbolic link and an executable file.
func gitSpecialMode(m string) bool { return m == "120000" || m == "100755" }

// parseGitRaw: the output of `git diff-tree -z --raw --no-commit-id`: ":oldmode newmode oldid newid STATUS" NUL path NUL, per entry. The names as parseGitNames gives them, and
// the entries whose mode or type changed (both modes present and different: a typechange T, or the executable bit), as "path old->new".
func parseGitRaw(out string) (files, modes []string) {
	toks := strings.Split(out, "\x00")
	for i := 0; i+1 < len(toks); i++ {
		meta, ok := strings.CutPrefix(toks[i], ":")
		if !ok {
			continue
		}
		path := toks[i+1]
		i++
		if path != "" && !slices.Contains(files, path) {
			files = append(files, path)
		}
		if f := strings.Fields(meta); len(f) >= 5 && f[0] != f[1] && (f[0] != "000000" && f[1] != "000000" || gitSpecialMode(f[0]) || gitSpecialMode(f[1])) { // an added or deleted link or executable too: a rename with a type change is a delete and an add
			if m := path + " " + f[0] + "->" + f[1]; !slices.Contains(modes, m) {
				modes = append(modes, m)
			}
		}
	}
	return
}

// parseGitNames: the output of `git diff-tree -z --name-only --no-commit-id`: the names, NUL-separated and raw (a quote, a newline or a control character in a name is not
// C-quoted), each once (-m lists a merge's names once per parent).
func parseGitNames(out string) []string {
	var names []string
	for _, t := range strings.Split(out, "\x00") {
		if t != "" && !slices.Contains(names, t) {
			names = append(names, t)
		}
	}
	return names
}

// gitWatchTips: the object ids to start the history from: every ref and HEAD.
func gitWatchTips(st gitState) []string {
	var t []string
	for _, id := range st.Refs {
		t = append(t, id)
	}
	if st.HeadID != "" {
		t = append(t, st.HeadID)
	}
	slices.Sort(t)
	return slices.Compact(t)
}

// report: at the end of the share. Prints to stderr, then forgets the stored start state.
// An incomplete check keeps the stored start state: the next start in the folder checks again (gitWatchRecover). drained false: the requests of the share did not
// finish in time, the agent may still be writing.
func (w *gitWatch) report(drained bool) {
	var why []string
	if !drained {
		why = append(why, "requests of the agent were still running")
	}
	lines, complete := w.compare(why, drained)
	for _, l := range lines {
		fmt.Fprintln(os.Stderr, l)
	}
	if complete {
		w.forget()
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
