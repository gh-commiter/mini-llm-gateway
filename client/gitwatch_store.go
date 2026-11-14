package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// X2 (#116): the start state is stored while the share runs, so that a killed client does not lose the report: the next start in the folder compares and reports
// (gitWatchRecover). One file per folder and process in the client's state folder (<settings folder>/minigw/gitwatch, 0700; the files 0600), never inside the
// repository: the ids of HEAD and refs, a hash of the index and the index entries of the protected paths, no file contents. The Swift client keeps the same
// (GitWatch.swift); client/testdata/git-watch.json has the report rows with a partial start state. A stored file is untrusted on read: at most 4 MB, strict
// parse, plausible names and ids, else it is ignored with one line.
type gitWatchStored struct {
	Version   int               `json:"version"`
	Root      string            `json:"root"`
	PID       int               `json:"pid"`
	Head      string            `json:"head"`
	Refs      map[string]string `json:"refs"`
	Pending   []string          `json:"pending"`
	IndexHash string            `json:"indexHash"`
	Protected map[string]string `json:"protected"` // index entries of the protected paths: "mode id stage"
}

const (
	gitWatchMaxFile = 4 << 20
	gitWatchMaxRefs = 20000
	gitWatchMaxProt = 1000
)

var (
	gitIDRe    = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
	gitEntryRe = regexp.MustCompile(`^[0-7]{6} [0-9a-f]{40}([0-9a-f]{24})? [0-3]$`)
)

func gitWatchDir() string {
	d, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(d, "minigw", "gitwatch")
}

// gitWatchKey: the folder's real path, hashed (the file name).
func gitWatchKey(root string) string {
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	sum := sha256.Sum256([]byte(root))
	return hex.EncodeToString(sum[:16])
}

func gitWatchFile(root string, pid int) string {
	d := gitWatchDir()
	if d == "" {
		return ""
	}
	return filepath.Join(d, fmt.Sprintf("%s-%d.json", gitWatchKey(root), pid))
}

func (st gitState) stored(root string, pid int, isProtected func(string) bool) gitWatchStored {
	s := gitWatchStored{Version: 1, Root: root, PID: pid, Head: st.Head, Refs: st.Refs, Pending: st.Pending, IndexHash: st.IndexHash, Protected: map[string]string{}}
	for p, v := range st.Index {
		if isProtected(p) && len(s.Protected) < gitWatchMaxProt {
			s.Protected[p] = v
		}
	}
	return s
}

func (w *gitWatch) save() {
	f := gitWatchFile(w.root, os.Getpid())
	if f == "" || len(w.start.Refs) > gitWatchMaxRefs {
		return
	}
	data, err := json.Marshal(w.start.stored(w.root, os.Getpid(), protectedPath))
	if err != nil || len(data) > gitWatchMaxFile || os.MkdirAll(filepath.Dir(f), 0o700) != nil {
		return
	}
	tmp := f + ".tmp"
	if os.WriteFile(tmp, data, 0o600) == nil && os.Rename(tmp, f) == nil {
		w.file = f
	}
}

func (w *gitWatch) forget() {
	if w.file != "" {
		os.Remove(w.file)
		w.file = ""
	}
}

// parseGitWatchStored: strict. An error says why the file is ignored.
func parseGitWatchStored(data []byte, root string) (gitState, error) {
	var s gitWatchStored
	if len(data) > gitWatchMaxFile {
		return gitState{}, fmt.Errorf("too large")
	}
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil || dec.More() {
		return gitState{}, fmt.Errorf("not valid")
	}
	if s.Version != 1 || s.Root != root || len(s.Refs) > gitWatchMaxRefs || len(s.Protected) > gitWatchMaxProt || len(s.Pending) > 3 || len(s.IndexHash) != 64 {
		return gitState{}, fmt.Errorf("unexpected content")
	}
	okName := func(n string) bool {
		return n != "" && len(n) <= 1024 && !strings.ContainsAny(n, "\x00\n\r") && !strings.HasPrefix(n, "-")
	}
	if len(s.Head) > 1024 || strings.ContainsAny(s.Head, "\x00\n\r") {
		return gitState{}, fmt.Errorf("bad head")
	}
	st := gitState{Head: s.Head, Refs: s.Refs, Index: s.Protected, Pending: s.Pending, IndexHash: s.IndexHash, Partial: true}
	if st.Refs == nil {
		st.Refs = map[string]string{}
	}
	if st.Index == nil {
		st.Index = map[string]string{}
	}
	for r, id := range st.Refs {
		if !strings.HasPrefix(r, "refs/") || !okName(r) || !gitIDRe.MatchString(id) {
			return gitState{}, fmt.Errorf("bad ref")
		}
	}
	for p, v := range st.Index {
		if !okName(p) || !gitEntryRe.MatchString(v) {
			return gitState{}, fmt.Errorf("bad index entry")
		}
	}
	for _, n := range st.Pending {
		if !slices.Contains([]string{"MERGE_HEAD", "CHERRY_PICK_HEAD", "REVERT_HEAD"}, n) {
			return gitState{}, fmt.Errorf("bad state name")
		}
	}
	return st, nil
}

// gitWatchRecover: files of runs that did not finish (their process is gone) in this folder: compare and report, prefixed, then remove them.
func gitWatchRecover(root string, self int, alive func(int) bool) []string {
	d := gitWatchDir()
	if d == "" {
		return nil
	}
	files, _ := filepath.Glob(filepath.Join(d, gitWatchKey(root)+"-*.json"))
	var out []string
	for _, f := range files {
		var pid int
		if _, err := fmt.Sscanf(strings.TrimSuffix(strings.TrimPrefix(filepath.Base(f), gitWatchKey(root)+"-"), ".json"), "%d", &pid); err != nil || pid == self || alive(pid) {
			continue
		}
		fh, err := os.Open(f)
		if err != nil {
			continue
		}
		data, _ := io.ReadAll(io.LimitReader(fh, gitWatchMaxFile+1))
		fh.Close()
		os.Remove(f)
		st, err := parseGitWatchStored(data, root)
		if err != nil {
			out = append(out, "minigw: ignoring a damaged git watch state file ("+err.Error()+")")
			continue
		}
		for _, l := range (&gitWatch{root: root, start: st}).lines() {
			out = append(out, strings.Replace(l, "minigw: ", "minigw: from the previous run that did not finish: ", 1))
		}
	}
	return out
}
