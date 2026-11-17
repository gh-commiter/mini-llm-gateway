package main

// Labels of a run for the gateway's usage reports: --minigw-phase (MINIGW_PHASE), --minigw-activity (MINIGW_ACTIVITY),
// --minigw-task (MINIGW_TASK). They are data, never a reason to refuse or delay a run: a missing label is empty, an
// unusual value that is a safe token is sent as given, and a value that is not one is dropped with one warning line.

import (
	"io"
	"os"
	"path/filepath"
	"strings"

	"minigw/core"
)

type runLabels struct {
	phase, activity, task string
	taskSource            string // given, branch or "" (phase and activity are always "given" when set)
}

// takeLabels removes the label flags from args (the last one wins; a flag beats the environment) and validates them.
// warn is called once per dropped value.
func takeLabels(args []string, env func(string) string, warn func(string)) (l runLabels, rest []string) {
	vals := map[string]string{"phase": env("MINIGW_PHASE"), "activity": env("MINIGW_ACTIVITY"), "task": env("MINIGW_TASK")}
	for i := 0; i < len(args); i++ {
		name, val, hasVal, matched := "", "", false, false
		for _, n := range []string{"phase", "activity", "task"} {
			flag := "--minigw-" + n
			switch {
			case args[i] == flag:
				name, matched = n, true
				if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") { // a following option is not the value: it stays
					val, hasVal = args[i+1], true
					i++
				}
			case strings.HasPrefix(args[i], flag+"="):
				name, val, hasVal, matched = n, strings.TrimPrefix(args[i], flag+"="), true, true
			}
			if matched {
				break
			}
		}
		if !matched {
			rest = append(rest, args[i])
			continue
		}
		if !hasVal {
			warn("--minigw-" + name + " has no value: ignored")
			continue
		}
		vals[name] = val
	}
	check := func(name string, safe func(string) string) string {
		v := vals[name]
		if v == "" {
			return ""
		}
		if s := safe(v); s != "" {
			return s
		}
		warn("--minigw-" + name + " ignored: " + strings.ToUpper(name[:1]) + name[1:] + " must be a short token (letters, digits and . _ -" + map[string]string{"task": " # /"}[name] + ")")
		return ""
	}
	l.phase, l.activity = check("phase", core.SafeWordLabel), check("activity", core.SafeWordLabel)
	if l.task = check("task", core.SafeTaskLabel); l.task != "" {
		l.taskSource = "given"
	}
	return l, rest
}

// withBranch fills an empty task with the branch of the git repository the folder is in, when the folder is linked to a
// project (detached HEAD or no repository: nothing). It reads files only; no git process, no code of the checkout runs.
func (l *runLabels) withBranch(cwd string, linkRoot func(string) string, branch func(cwd, root string) string) {
	if l.task != "" {
		return
	}
	root := linkRoot(cwd)
	if root == "" {
		return
	}
	if b := core.SafeTaskLabel(branch(cwd, root)); b != "" {
		l.task, l.taskSource = b, "branch"
	}
}

// headers are the request headers that carry the labels (each only when set).
func (l runLabels) headers() string {
	h := ""
	for _, x := range []struct{ name, v string }{{"phase", l.phase}, {"activity", l.activity}, {"task", l.task}, {"task-source", l.taskSource}} {
		if x.v != "" {
			h += "\nx-claude-mini-" + x.name + ": " + x.v
		}
	}
	return h
}

// linkRoot: the folder the link was made for (the folder or a parent of cwd that is a linked project folder, on any
// gateway), "" when none. The branch is looked for only up to there.
func linkRoot(cwd string) string {
	st, err := loadProjectState(projectStatePath())
	if err != nil {
		return ""
	}
	real, err := realFolder(cwd)
	if err != nil {
		return ""
	}
	for dir := real; ; dir = filepath.Dir(dir) {
		for _, l := range st.Links {
			if sameProjectPath(dir, l.Path) {
				return dir
			}
		}
		if filepath.Dir(dir) == dir {
			return ""
		}
	}
}

// gitBranch reads the current branch from the repository's HEAD file: "" for a detached HEAD, no repository or anything
// unexpected. A worktree's .git file is followed one step ("gitdir: <path>"). The search goes up from dir to root (the
// linked folder's root) and no further: a repository above the linked folder gives no branch for a project it is not part of.
func gitBranch(dir, root string) string {
	if real, err := realFolder(dir); err == nil {
		dir = real
	}
	for d := dir; ; d = filepath.Dir(d) {
		g := filepath.Join(d, ".git")
		fi, err := os.Lstat(g)
		if err == nil && fi.Mode()&os.ModeSymlink == 0 {
			gitDir := g
			if fi.Mode().IsRegular() {
				rel, ok := strings.CutPrefix(strings.TrimSpace(readGitSmall(g)), "gitdir:")
				if !ok {
					return ""
				}
				gitDir = strings.TrimSpace(rel)
				if !filepath.IsAbs(gitDir) {
					gitDir = filepath.Join(d, gitDir)
				}
			} else if !fi.IsDir() {
				return ""
			}
			// Only <gitDir>/HEAD is ever read from there, as a small regular file (never a FIFO, a link or a directory).
			ref, ok := strings.CutPrefix(strings.TrimSpace(readGitSmall(filepath.Join(gitDir, "HEAD"))), "ref: refs/heads/")
			if !ok {
				return ""
			}
			return ref
		}
		if filepath.Dir(d) == d || sameProjectPath(d, root) { // the link's root is the last folder looked at
			return ""
		}
	}
}

// readGitSmall reads a small file of a repository: opened once without following a link and without waiting for a FIFO
// (openMarker, as the knowledge-base import does), checked on the descriptor (a regular file of at most 4096 bytes),
// else "".
func readGitSmall(path string) string {
	f, err := openMarker(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	if fi, err := f.Stat(); err != nil || !fi.Mode().IsRegular() || fi.Size() > 4096 {
		return ""
	}
	b, _ := io.ReadAll(io.LimitReader(f, 4096))
	return string(b)
}
