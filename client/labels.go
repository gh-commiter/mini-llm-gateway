package main

// Labels of a run for the gateway's usage reports: --minigw-phase (MINIGW_PHASE), --minigw-activity (MINIGW_ACTIVITY),
// --minigw-task (MINIGW_TASK). They are data, never a reason to refuse or delay a run: a missing label is empty, an
// unusual value that is a safe token is sent as given, and a value that is not one is dropped with one warning line.

import (
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
				if i+1 < len(args) {
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
func (l *runLabels) withBranch(cwd string, linked func(string) bool, branch func(string) string) {
	if l.task != "" || !linked(cwd) {
		return
	}
	if b := core.SafeTaskLabel(branch(cwd)); b != "" {
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

// folderLinked: the folder or a parent is a linked project folder (on any gateway).
func folderLinked(cwd string) bool {
	st, err := loadProjectState(projectStatePath())
	if err != nil {
		return false
	}
	real, err := realFolder(cwd)
	if err != nil {
		return false
	}
	for dir := real; ; dir = filepath.Dir(dir) {
		for _, l := range st.Links {
			if sameProjectPath(dir, l.Path) {
				return true
			}
		}
		if filepath.Dir(dir) == dir {
			return false
		}
	}
}

// gitBranch reads the current branch from the repository's HEAD file: "" for a detached HEAD, no repository or anything
// unexpected. A worktree's .git file is followed one step ("gitdir: <path>").
func gitBranch(dir string) string {
	for d := dir; ; d = filepath.Dir(d) {
		g := filepath.Join(d, ".git")
		fi, err := os.Lstat(g)
		if err == nil && fi.Mode()&os.ModeSymlink == 0 {
			gitDir := g
			if fi.Mode().IsRegular() {
				b := readGitSmall(g)
				rel, ok := strings.CutPrefix(strings.TrimSpace(b), "gitdir:")
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
			ref, ok := strings.CutPrefix(strings.TrimSpace(readGitSmall(filepath.Join(gitDir, "HEAD"))), "ref: refs/heads/")
			if !ok {
				return ""
			}
			return ref
		}
		if filepath.Dir(d) == d {
			return ""
		}
	}
}

func readGitSmall(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	buf := make([]byte, 4096)
	n, _ := f.Read(buf)
	return string(buf[:n])
}
