package main

// Project links of the client (docs/plans/contracts-0.6.0.md section 3): which gateway project a folder tree belongs
// to. The state is private to this user and outside every repository: <settings folder>/minigw/projects.json.

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
)

// projectLink: one folder tree linked to one project of one gateway. Path is the canonical real path of the folder;
// Slug is for display only (the project id is what counts).
type projectLink struct {
	Gateway string `json:"gateway"` // the gateway's public key (base64url), not the profile name
	Project int64  `json:"project"`
	Slug    string `json:"slug"`
	Path    string `json:"path"`
}

type projectState struct {
	Version int           `json:"version"`
	Links   []projectLink `json:"links"`
}

const hintFile = ".minigw-project"

var hintRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}$`)

func projectStatePath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "minigw", "projects.json")
}

func loadProjectState(path string) (projectState, error) {
	var st projectState
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err == nil {
		err = json.Unmarshal(data, &st)
	}
	if err != nil {
		return projectState{}, errors.New("cannot read " + path + ": " + err.Error())
	}
	return st, nil
}

func saveProjectState(path string, st projectState) error {
	st.Version = 1
	data, _ := json.MarshalIndent(st, "", "  ")
	os.MkdirAll(filepath.Dir(path), 0o700)
	return writeFileAtomic(path, append(data, '\n'))
}

// gatewayID: the gateway's public key (the identity), so a renamed or second profile of the same gateway shares
// its links. A local plain profile without a key is told apart by its address.
func gatewayID(c connection) string {
	if c.Key != nil {
		return *c.Key
	}
	return "url:" + c.URL
}

// realFolder: the canonical real path of a folder (symlinks resolved; Windows: the final path).
func realFolder(dir string) (string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	return finalPath(abs)
}

// sameProjectPath: the platform's name rules (Windows: case-insensitive), or the same folder by identity (also macOS and
// other case-insensitive volumes, short names): a path is compared by name first, then by fileIDOf.
func sameProjectPath(a, b string) bool {
	if a == b || runtime.GOOS == "windows" && strings.EqualFold(a, b) {
		return true
	}
	ia, err1 := fileIDOf(a)
	ib, err2 := fileIDOf(b)
	return err1 == nil && err2 == nil && ia == ib
}

// readHint: the slug in a .minigw-project file (one line); "" if there is none or it is not a plain file with a
// valid slug. It only ever feeds a suggestion or a check; it never creates a link.
func readHint(dir string) string {
	p := filepath.Join(dir, hintFile)
	if fi, err := os.Lstat(p); err != nil || !fi.Mode().IsRegular() || fi.Size() > 256 {
		return ""
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	line, _, _ := strings.Cut(strings.ReplaceAll(string(b), "\r", ""), "\n")
	if line = strings.TrimSpace(line); hintRe.MatchString(line) {
		return line
	}
	return ""
}

type projectState3 int

const (
	noLink       projectState3 = iota // continue without a project (maybe with a suggestion)
	linkOK                            // the link applies
	linkHintDiff                      // link and a hint with another slug: refused
)

type projectResolution struct {
	State    projectState3
	Link     projectLink // the applying link (linkOK, linkHintDiff)
	Hint     string      // the slug of the hint, "" = no hint
	HintDir  string      // where it was found
	RealPath string      // the folder, symlinks resolved
}

// projectFor resolves the project of a folder (section 3): resolve symlinks, take the stored link of this gateway
// whose path is the folder or its nearest ancestor. The hint is read only from the linked root; without a link
// from the folder and its ancestors up to the repository (.git) or the filesystem root.
// Path comparison: sameProjectPath (names by the platform's rules, or the same folder by identity). The agent-request
// path calls this with the request's folder.
func projectFor(st projectState, gateway, cwd string) (projectResolution, error) {
	real, err := realFolder(cwd)
	if err != nil {
		return projectResolution{}, errors.New("cannot resolve " + cwd + ": " + err.Error())
	}
	res := projectResolution{RealPath: real}
	var links []projectLink
	for _, l := range st.Links {
		if l.Gateway == gateway {
			links = append(links, l)
		}
	}
	for dir := real; ; dir = filepath.Dir(dir) {
		if i := slices.IndexFunc(links, func(l projectLink) bool { return sameProjectPath(dir, l.Path) }); i >= 0 {
			res.Link, res.State = links[i], linkOK
			if h := readHint(dir); h != "" {
				res.Hint, res.HintDir = h, dir
				if h != res.Link.Slug {
					res.State = linkHintDiff
				}
			}
			return res, nil
		}
		if parent := filepath.Dir(dir); parent == dir {
			break
		}
	}
	for dir := real; ; dir = filepath.Dir(dir) {
		if h := readHint(dir); h != "" {
			res.Hint, res.HintDir = h, dir
			break
		}
		if _, err := os.Lstat(filepath.Join(dir, ".git")); err == nil || filepath.Dir(dir) == dir {
			break // the repository root (or the filesystem root) is the last folder looked at
		}
	}
	return res, nil
}

// projectHeaderFor: the header line for the agent requests of a run in cwd, or "" (section 3). A link sends the project
// id; link + hint with another slug is an error (the request is not sent); no link continues without a project and
// prints the hint's suggestion (once per minigw run). Not used inside an agent run: the gateway inherits the project.
func projectHeaderFor(cwd string, c connection, statePath string) (string, error) {
	if agentConn() != nil {
		return "", nil
	}
	st, err := loadProjectState(statePath)
	if err != nil {
		return "", err
	}
	res, err := projectFor(st, gatewayID(c), cwd)
	if err != nil {
		return "", nil // a folder that cannot be resolved has no link
	}
	switch res.State {
	case linkOK:
		return "x-minigw-project: " + strconv.FormatInt(res.Link.Project, 10), nil
	case linkHintDiff:
		return "", errors.New(hintDiffMessage(res))
	}
	if res.Hint != "" {
		fmt.Fprintf(os.Stderr, "minigw: this folder suggests project %s (%s); run minigw project link %s. Continuing without a project.\n", res.Hint, hintFile, res.Hint)
	}
	return "", nil
}

func hintDiffMessage(res projectResolution) string {
	return fmt.Sprintf("this folder is linked to %s but %s says %s: run minigw project unlink, link again or remove the hint", res.Link.Slug, hintFile, res.Hint)
}
