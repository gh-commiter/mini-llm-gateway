package main

// --minigw-work=worktree (kot Worktree v Sources/RouterCore/Worktree.swift): posnetek stanja kot git bundle k routerju,
// veja agenta nazaj le kot ref (fsck, brez hookov, podmodulov, FETCH_HEAD in checkouta).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// gitUser: git uporabnika (njegove nastavitve), brez njegovih hookov ob začasnem refu in posnetku.
func gitUser(dir string, env []string, args ...string) (string, error) {
	cmd := command("git", append([]string{"-c", "core.hooksPath=" + os.DevNull, "-c", "core.fsmonitor=false"}, args...)...)
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

// snapshotBlocker: zakaj ni posnetka ("" = je): repozitorij brez commita (kot Swift Worktree.snapshotBlocker). Mapa v
// njem načina nikoli ne spremeni: ugnezden repozitorij brez commita gre iz posnetka (nestedNoCommit).
func snapshotBlocker(top string) string {
	if !gitHasCommit(top) {
		return "the git repository " + oneLine(top) + " has no commit yet, so worktree mode is not possible"
	}
	return ""
}

// maxNestedRepos: največ ugnezdenih repozitorijev v enem zagonu worktree (več: napaka, način ostane).
const maxNestedRepos = 32

// nestedRepos: neprezrti ugnezdeni repozitoriji v top na kateri koli globini (podmoduli z mapo .git in nesledeni, s
// commitom ali brez), relativne poti »/«, starš pred otrokom; največ maxNestedRepos+1 (več je napaka). Gitlinki iz indeksa,
// nesledene mape git strne (--directory), zato le v njih poiščemo .git po datotečnem sistemu in vprašamo check-ignore.
// Prezrte poti (build/_deps) niso tu.
func nestedRepos(top string) []string {
	var out []string
	var walk func(rel string)
	walk = func(rel string) {
		for _, p := range nestedIn(filepath.Join(top, filepath.FromSlash(rel))) {
			if len(out) > maxNestedRepos {
				return
			}
			if rel != "" {
				p = rel + "/" + p
			}
			if !nestedInside(top, p) {
				continue // N2: povezava v poti ali repozitorij zunaj mape: ni ugnezden repozitorij te mape
			}
			out = append(out, p)
			walk(p)
		}
	}
	walk("")
	return out
}

// nestedInside (N2): ugnezden repozitorij rel je res v top: vsak del poti je prava mapa (ne povezava ali spoj), je svoj
// koren (git ne najde nadrejenega) in leži pod top tudi po razrešitvi poti. Tudi ob prevzemu (wtFetch).
func nestedInside(top, rel string) bool {
	d := top
	for _, c := range strings.Split(rel, "/") {
		if d = filepath.Join(d, c); !realDir(d) {
			return false
		}
	}
	t, rt := gitToplevel(d), realPath(top)
	return t != "" && realPath(t) == realPath(d) && strings.HasPrefix(realPath(d), rt+string(filepath.Separator))
}

// gitlinks: poti gitlinkov (160000) v indeksu repozitorija dir.
func gitlinks(dir string) map[string]bool {
	out := map[string]bool{}
	for _, l := range gitZ(dir, "", "ls-files", "-s", "-z") {
		if m, p, ok := strings.Cut(l, "\t"); ok && strings.HasPrefix(m, "160000 ") {
			out[p] = true
		}
	}
	return out
}

// nestedIn: neposredni ugnezdeni repozitoriji repozitorija dir (relativne poti brez »/« na koncu).
func nestedIn(dir string) []string {
	isRepo := func(p string) bool {
		_, err := os.Lstat(filepath.Join(dir, filepath.FromSlash(p), ".git"))
		return err == nil
	}
	var out, deep []string
	for p := range gitlinks(dir) { // podmoduli, le izvlečeni
		if isRepo(p) {
			out = append(out, p)
		}
	}
	slices.Sort(out)
	for _, d := range gitZ(dir, "", "ls-files", "-o", "--exclude-standard", "--directory", "--no-empty-directory", "-z") {
		if !strings.HasSuffix(d, "/") {
			continue
		}
		if isRepo(d) {
			out = append(out, strings.TrimSuffix(d, "/"))
			continue
		}
		root := filepath.Join(dir, filepath.FromSlash(d))
		filepath.WalkDir(root, func(p string, e os.DirEntry, err error) error {
			if err != nil || !e.IsDir() || p == root {
				return nil
			}
			if _, err := os.Lstat(filepath.Join(p, ".git")); err == nil {
				if rel, err := filepath.Rel(dir, p); err == nil {
					deep = append(deep, filepath.ToSlash(rel)+"/")
				}
				return filepath.SkipDir // ugnezdene v njem najde njegov nestedIn
			}
			return nil
		})
	}
	if len(deep) == 0 {
		return out
	}
	ignored := map[string]bool{}
	for _, p := range gitZ(dir, strings.Join(deep, "\x00")+"\x00", "check-ignore", "--stdin", "-z") {
		ignored[p] = true
	}
	for _, p := range deep {
		if !ignored[p] {
			out = append(out, strings.TrimSuffix(p, "/"))
		}
	}
	return out
}

// nestedChildren: neposredni otroci repozitorija rel ("" = top) med ugnezdenimi, relativno nanj.
func nestedChildren(rel string, nested []string) []string {
	var out []string
	for _, p := range nested {
		r, ok := p, rel == ""
		if !ok {
			r, ok = strings.CutPrefix(p, rel+"/")
		}
		if !ok || p == rel {
			continue
		}
		inner := false
		for _, q := range nested {
			if q != p && strings.HasPrefix(p, q+"/") && (rel == "" || strings.HasPrefix(q, rel+"/")) {
				inner = true
			}
		}
		if !inner {
			out = append(out, r)
		}
	}
	return out
}

// validNestedPath: relativna pot ugnezdenega repozitorija (odjemalec in prehod; H1): deli niso prazni, ».«, »..« ali
// .git, brez »\«, »:«, kontrolnih znakov (seznam MINIGW_GIT_DIRS je ločen z novo vrstico) in znakov, ki jih HFS+
// prezre (N5); ne v .build klona (N6: TMPDIR agenta).
func validNestedPath(p string) bool {
	if p == "" || len(p) > 1024 || strings.ContainsAny(p, "\\:") || normName(strings.Split(p, "/")[0]) == ".build" {
		return false
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f || hfsIgnorable(r) {
			return false
		}
	}
	for _, c := range strings.Split(p, "/") {
		if c == "" || c == "." || c == ".." || isGitPath(c) {
			return false
		}
	}
	return true
}

// nestedKey: ključ zrcala ugnezdenega repozitorija (odjemalec in prehod).
func nestedKey(p string) string { return sha256Hex([]byte(p))[:16] }

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
// skip: poti, ki ne gredo v posnetek (ugnezdeni repozitoriji brez commita). root: repozitorij brez commita (ugnezden)
// dobi commit brez starša iz kopije svojega indeksa in delovne mape.
// noAttr (N3, ugnezdeni): brez atributov (GIT_ATTR_SOURCE = prazno drevo, core.attributesFile prazen), zato git add ne
// požene filtra clean, ki bi ga določila .gitattributes ugnezdenega repozitorija. Git pred 2.40: brez posnetka.
func gitSnapshot(top string, root, noAttr bool, skip ...string) string {
	if noAttr && !gitAttrSource() {
		return ""
	}
	head, err := gitUser(top, nil, "rev-parse", "--verify", "-q", "HEAD^{commit}")
	if (err != nil || !shaRe.MatchString(head)) && !root {
		return ""
	}
	dir, err := os.MkdirTemp("", "minigw-") // 0700
	if err != nil {
		return ""
	}
	defer os.RemoveAll(dir)
	env := []string{"GIT_INDEX_FILE=" + filepath.Join(dir, "index")}
	if noAttr {
		env = append(env, "GIT_ATTR_SOURCE=4b825dc642cb6eb9a060e54bf8d69288fbee4904", "GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=core.attributesFile", "GIT_CONFIG_VALUE_0="+os.DevNull)
	}
	if root {
		head = ""
		if idx, _ := gitUser(top, nil, "rev-parse", "--git-path", "index"); idx != "" {
			if !filepath.IsAbs(idx) {
				idx = filepath.Join(top, idx)
			}
			if b, err := os.ReadFile(idx); err == nil && os.WriteFile(filepath.Join(dir, "index"), b, 0o600) != nil {
				return ""
			}
		}
	} else if _, err := gitUser(top, env, "read-tree", "HEAD"); err != nil {
		return ""
	}
	add := []string{"add", "-A", "--", "."}
	for _, p := range skip {
		add = append(add, ":(exclude,literal)"+p)
	}
	if _, err := gitUser(top, env, add...); err != nil {
		return ""
	}
	tree, err := gitUser(top, env, "write-tree")
	if err != nil || !shaRe.MatchString(tree) {
		return ""
	}
	if t, _ := gitUser(top, nil, "rev-parse", "HEAD^{tree}"); head != "" && t == tree {
		return head
	}
	ct := []string{"-c", "user.name=minigw", "-c", "user.email=minigw@localhost", "commit-tree", tree, "-m", "minigw: your uncommitted changes"}
	if head != "" {
		ct = append(ct, "-p", head)
	}
	c, err := gitUser(top, nil, ct...)
	if err != nil || !shaRe.MatchString(c) {
		return ""
	}
	return c
}

// gitAttrSource: git pozna GIT_ATTR_SOURCE (2.40+); starejši ga tiho prezre, --attr-source pa zavrne.
var gitAttrSource = sync.OnceValue(func() bool {
	_, err := gitUser("", nil, "--attr-source=4b825dc642cb6eb9a060e54bf8d69288fbee4904", "version")
	return err == nil
})

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
	if err := fsckIncoming(top); err != nil { // #58: ločeno od fetch (npr. delni klon brez bloba .gitattributes)
		return "the gateway's branch failed the object check (fsck): " + err.Error()
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

// fsckIncoming: fetch iz bundla pred git 2.46 ne preveri objektov (fetch.fsckObjects prezre). Zato novi objekti
// refs/minigw/incoming (brez dosegljivih iz drugih refov): pack-objects in index-pack --fsck-objects v prazen
// repozitorij (kot finalize prehoda); zavrne npr. drevo z ».git«. nil = v redu, sicer razlog (prva vrstica gita).
func fsckIncoming(top string) error {
	refs, err := gitUser(top, nil, "for-each-ref", "--format=%(objectname) %(refname)")
	if err != nil {
		return errors.New("cannot list refs")
	}
	in := "refs/minigw/incoming\n--not\n"
	for _, l := range strings.Split(refs, "\n") {
		if sha, ref, _ := strings.Cut(l, " "); shaRe.MatchString(sha) && ref != "refs/minigw/incoming" {
			in += sha + "\n"
		}
	}
	// Objekti uporabnika kot alternates: drevo agenta sme kazati na obstoječ blob (npr. .gitattributes), fsck ga prebere.
	objs, err := gitUser(top, nil, "rev-parse", "--git-path", "objects")
	if err != nil || objs == "" {
		return errors.New("cannot find the objects folder")
	}
	if !filepath.IsAbs(objs) {
		objs = filepath.Join(top, objs)
	}
	// Mapa ob objects (kot fsckBase prehoda): pack-objects sestavi paket v objects/pack in ga preimenuje v dir; začasna
	// mapa na drugem disku (repozitorij zunaj TMP) da »cross-device link«.
	dir, err := os.MkdirTemp(filepath.Dir(objs), "minigw-fsck-") // 0700
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	// #58: pack-objects in index-pack z mejo (velik repozitorij, zastal disk)
	ctx, cancel := context.WithTimeout(context.Background(), fsckTimeout)
	defer cancel()
	git := func(cwd, input string, args ...string) (string, error) {
		cmd := commandContext(ctx, "git", append([]string{"-c", "core.hooksPath=" + os.DevNull, "-c", "core.fsmonitor=false"}, args...)...)
		cmd.Dir = cwd
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		cmd.Stdin = strings.NewReader(input)
		cmd.WaitDelay = 5 * time.Second // zaganjalnik Git for Windows lahko drži cevi odprte po ubitju
		var out, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &stderr
		if err := cmd.Run(); err != nil {
			if line, _, _ := strings.Cut(strings.TrimSpace(stderr.String()), "\n"); line != "" {
				return "", errors.New(clean(line))
			}
			return "", err
		}
		return strings.TrimSpace(out.String()), nil
	}
	pack, err := git(top, in, "pack-objects", "--revs", "-q", filepath.Join(dir, "p"))
	if err != nil {
		return err
	}
	if !shaRe.MatchString(pack) {
		return errors.New("pack-objects gave no pack")
	}
	fsck := filepath.Join(dir, "fsck.git")
	if _, err := gitUser(dir, nil, "init", "-q", "--bare", fsck); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(fsck, "objects", "info", "alternates"), []byte(filepath.ToSlash(objs)+"\n"), 0o600); err != nil {
		return err
	}
	_, err = git(dir, "", "--git-dir", fsck, "index-pack", "--fsck-objects", filepath.Join(dir, "p-"+pack+".pack"))
	return err
}

// fsckTimeout: meja za pack-objects in index-pack v fsckIncoming (velik repozitorij).
var fsckTimeout = 10 * time.Minute

// wtPush: WORKTREE-PUSH — posnetek, bundle po delih do 32 MB, odgovor {"snapshot", "nested": [{"path", "snapshot"}]}.
// Ugnezdeni repozitoriji (vsak svoj posnetek in bundle, x-nested: i) le s prehodom, ki jih pozna (»nested« v zahtevi);
// starejši prehod: napaka, ne tiho brez njih. Način se nikoli ne spremeni.
func (s *shareClient) wtPush(k string, job davJob) davResponse {
	var req struct {
		Have   []string
		Nested map[string][]string // ključ zrcala (nestedKey) → vrhovi; nil = prehod ugnezdenih ne pozna
	}
	json.Unmarshal(job.Body, &req)
	nested := nestedRepos(s.top)
	fail := func(key, msg string) davResponse {
		if !s.reported[key] {
			s.reported[key] = true
			s.note(msg)
		}
		return dav(500, nil, nil)
	}
	switch {
	case len(nested) > maxNestedRepos:
		return fail("nested-max", fmt.Sprintf("worktree: %s has more than %d nested git repositories; worktree mode takes at most %d; use sync", oneLine(s.top), maxNestedRepos, maxNestedRepos))
	case len(nested) > 0 && req.Nested == nil:
		return fail("nested-old", "worktree: the gateway does not support nested repositories ("+oneLine(nested[0])+"); update the gateway")
	}
	for _, p := range nested {
		if !validNestedPath(p) {
			return fail("nested-name", "worktree: the nested repository "+oneLine(p)+" has a name worktree mode cannot use (\":\", \"\\\", control characters or in .build); use sync")
		}
	}
	for _, p := range nested { // R2: filtri ugnezdenega ne tečejo (N3), zato z njimi ne začne (način ostane)
		if f, _ := gitUser(filepath.Join(s.top, filepath.FromSlash(p)), nil, "config", "--local", "--includes", "--get-regexp", `^filter\.`); f != "" {
			return fail("nested-filter:"+p, "worktree: the nested repository "+oneLine(p)+" defines git filters (filter.* in its .git/config), which minigw does not run for nested repositories; use sync")
		}
	}
	if len(nested) > 0 && !gitAttrSource() { // N3: brez --attr-source bi tekli filtri ugnezdenih
		return fail("nested-git", "worktree: nested git repositories need git 2.40 or later; update git or use sync")
	}
	// skip: neposredni otroci, ki v indeksu starša niso gitlink (nesledeni, s commitom ali brez), ali so brez commita:
	// v posnetek starša ne gredo (imajo svojega); gitlink podmodula ostane.
	skip := func(rel string) (out []string) {
		dir := filepath.Join(s.top, filepath.FromSlash(rel))
		links := gitlinks(dir)
		for _, c := range nestedChildren(rel, nested) {
			if !links[c] || !gitHasCommit(filepath.Join(dir, filepath.FromSlash(c))) {
				out = append(out, c)
			}
		}
		return out
	}
	snap := gitSnapshot(s.top, false, false, skip("")...)
	if snap == "" {
		return fail("snapshot", "worktree: cannot take a snapshot of "+s.top+" (git add or write-tree failed)")
	}
	// #66: isti pogovor nadaljuje na vrhu svoje prejšnje veje, če se mapa od takrat ni spremenila
	tip, r, why := wtContinue(s.top, s.conv, snap, len(nested) > 0)
	if why != "" && !s.reported["continue:"+r.Branch] {
		s.reported["continue:"+r.Branch] = true
		s.note("worktree: starting from your folder, not from " + r.Branch + ": " + why)
	}
	if s.chain == nil {
		s.chain = map[string]wtRecord{}
	}
	if tip != "" {
		snap = tip
		s.chain[tip] = r
	} else if _, ok := s.chain[snap]; !ok {
		head, _ := gitUser(s.top, nil, "rev-parse", "HEAD")
		s.chain[snap] = wtRecord{Base: snap, Head: head}
	}
	if !s.wtUpload(k, s.top, snap, req.Have, 0) {
		return dav(500, nil, nil)
	}
	type nestedSnap struct {
		Path     string `json:"path"`
		Snapshot string `json:"snapshot"`
	}
	reply := []nestedSnap{}
	for i, p := range nested {
		dir := filepath.Join(s.top, filepath.FromSlash(p))
		sn := gitSnapshot(dir, !gitHasCommit(dir), true, skip(p)...)
		if sn == "" {
			return fail("snapshot:"+p, "worktree: cannot take a snapshot of the nested repository "+oneLine(p)+" (git add or write-tree failed)")
		}
		if !s.wtUpload(k, dir, sn, req.Nested[nestedKey(p)], i+1) {
			return dav(500, nil, nil)
		}
		reply = append(reply, nestedSnap{p, sn})
	}
	if s.snaps = append(s.snaps, snap); len(s.snaps) > 64 {
		s.snaps = s.snaps[1:]
	}
	if s.nested == nil {
		s.nested = map[string][]string{}
	}
	for _, n := range reply {
		if s.nested[n.Path] = append(s.nested[n.Path], n.Snapshot); len(s.nested[n.Path]) > 64 {
			s.nested[n.Path] = s.nested[n.Path][1:]
		}
	}
	body, _ := json.Marshal(map[string]any{"snapshot": snap, "nested": reply})
	return dav(200, nil, body)
}

// wtUpload: bundle posnetka repozitorija dir brez objektov, ki jih zrcalo ima, po delih do 32 MB (idx > 0: ugnezden).
func (s *shareClient) wtUpload(k, dir, snap string, have []string, idx int) bool {
	tmp, err := os.MkdirTemp("", "minigw-") // 0700
	if err != nil {
		return false
	}
	defer os.RemoveAll(tmp)
	name := filepath.Join(tmp, "push.bundle")
	made, err := gitBundle(dir, snap, have, name)
	if err != nil {
		s.note("worktree: git bundle failed")
		return false
	}
	if !made {
		return true
	}
	b, err := os.Open(name)
	if err != nil {
		return false
	}
	defer b.Close()
	h := nestedHeader(map[string]string{"x-share-key": k}, idx)
	buf := make([]byte, 32<<20)
	for part := 0; ; part++ {
		n, rerr := io.ReadFull(b, buf)
		if n > 0 {
			h["x-part"] = strconv.Itoa(part)
			if st, _ := call(s.c, "PUT", "/minigw/worktree/bundle", h, buf[:n], 10*time.Minute); st != 204 {
				s.note("worktree: upload to the gateway failed")
				return false
			}
		}
		if rerr != nil {
			return true
		}
	}
}

// wtPull: WORKTREE-PULL — bundle veje agenta in prevzem kot ref; ugnezdeni (le poti, ki jih je ta odjemalec poslal)
// vsak v svoj repozitorij na isto ime veje. nestedOnly: veja vrhnjega repozitorija se ni spremenila.
func (s *shareClient) wtPull(k string, job davJob) davResponse {
	var req struct {
		Branch     string
		NestedOnly bool
		Nested     []struct{ Path string }
	}
	if json.Unmarshal(job.Body, &req) != nil || !branchRe.MatchString(req.Branch) {
		return dav(403, nil, nil)
	}
	ok := true
	if !req.NestedOnly {
		ok = s.wtFetch(k, s.top, "", req.Branch, 0)
	}
	for i, n := range req.Nested {
		if len(s.nested[n.Path]) == 0 {
			s.note("worktree: the gateway named a nested repository minigw did not send; ignored")
			ok = false
			continue
		}
		ok = s.wtFetch(k, filepath.Join(s.top, filepath.FromSlash(n.Path)), n.Path, req.Branch, i+1) && ok
	}
	if !ok {
		return dav(500, nil, nil)
	}
	return dav(200, nil, nil)
}

// nestedHeader: glava x-nested (i-ti ugnezdeni, 0 = vrhnji brez glave); parseNested: njena vrednost na prehodu (-1 = napačna).
func nestedHeader(h map[string]string, idx int) map[string]string {
	if idx > 0 {
		h["x-nested"] = strconv.Itoa(idx)
	}
	return h
}

func parseNested(v string) int {
	if v == "" {
		return 0
	}
	if n, err := strconv.Atoi(v); err == nil && n >= 0 && n <= maxNestedRepos {
		return n
	}
	return -1
}

// wtFetch: rezultat idx (0 = vrhnji) s prehoda kot veja v repozitorij dir (rel: pot ugnezdenega, "" = vrhnji).
func (s *shareClient) wtFetch(k, dir, rel, branch string, idx int) bool {
	what := branch
	if rel != "" {
		what = branch + " of the nested repository " + oneLine(rel)
		if !nestedInside(s.top, rel) { // N2: povezava v poti ali repozitorij zunaj mape; sicer bi git našel nadrejeni
			s.note("worktree: the nested repository " + oneLine(rel) + " is no longer a git repository here; " + branch + " was not fetched into it")
			return false
		}
	}
	st, data := call(s.c, "GET", "/minigw/worktree/result", nestedHeader(map[string]string{"x-share-key": k}, idx), nil, 10*time.Minute)
	if st != 200 {
		var e struct {
			Error struct{ Message string } `json:"error"`
		}
		why := fmt.Sprintf("HTTP %d", st)
		if json.Unmarshal(data, &e) == nil && e.Error.Message != "" {
			why = oneLine(e.Error.Message)
		}
		s.note(fmt.Sprintf("worktree: cannot get %s from the gateway: %s", what, why))
		return false
	}
	tmp, err := os.MkdirTemp("", "minigw-") // 0700
	if err != nil {
		return false
	}
	defer os.RemoveAll(tmp)
	name := filepath.Join(tmp, "result.bundle")
	if err := os.WriteFile(name, data, 0o600); err != nil {
		return false
	}
	if why := gitFetchBranch(dir, name, branch); why != "" {
		if rel != "" {
			why = "nested repository " + oneLine(rel) + ": " + why
		}
		s.note("worktree: " + why)
		return false
	}
	if rel == "" {
		s.branchNote(branch, s.wtRemember(branch, agentBase(dir, branch, s.snaps)))
	} else {
		s.branchNote(branch, agentBase(dir, branch, s.nested[rel]), rel)
	}
	return true
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
// nested: pot ugnezdenega repozitorija (M2: stroga oblika »… in nested repository <pot>: review with git -C <pot> …«
// le za pot iz safePathRe; drugačna dobi vrstico brez ukazov, ki je skillsi ne vzamejo).
func branchLine(branch, base string, nested ...string) string {
	r := base + ".." + branch
	if len(nested) == 0 {
		return "agent work is on branch " + branch + ": review with git diff " + r + " (" + base + " = your state when the agent started), " +
			"then bring it in with git cherry-pick " + r + " or git diff --binary " + r + " | git apply"
	}
	p := nested[0]
	if !safePathRe.MatchString(p) {
		return "the agent's work in a nested repository (its name has characters minigw does not print in a command) is on branch " +
			branch + " there, starting at " + base
	}
	g := "git -C " + p + " "
	return "agent work is on branch " + branch + " in nested repository " + p + ": review with " + g + "diff " + r + " (" + base +
		" = your state in " + p + " when the agent started), then bring it in with " + g + "cherry-pick " + r + " or " + g +
		"diff --binary " + r + " | " + g + "apply"
}

// safePathRe: pot ugnezdenega repozitorija, ki gre v vrstico o veji (enaka v skillsih in odjemalcu Swift).
var safePathRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._+-]*(/[A-Za-z0-9_][A-Za-z0-9._+-]*)*$`)

// branchNote: vrstica o veji enkrat na vejo, repozitorij in začetek v zagonu (pod syncMu).
func (s *shareClient) branchNote(branch, base string, nested ...string) {
	if k := "branch:" + strings.Join(nested, "") + ":" + branch + "@" + base; !s.reported[k] {
		s.reported[k] = true
		s.note(branchLine(branch, base, nested...))
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

// #66: naslednji zagon istega pogovora (--continue, --resume) začne iz vrha prejšnje veje pogovora, ne iz mape
// uporabnika (ta se ne spremeni), zato agent vidi svoje prejšnje delo. Zapis v <git-path>/minigw-continue.json.

// wtRecord: zadnja veja pogovora; Base = stanje uporabnika ob začetku verige (začetek obsega), Head = njegov HEAD tedaj.
type wtRecord struct {
	Branch string `json:"branch"`
	Base   string `json:"base"`
	Head   string `json:"head"`
}

type wtState struct {
	Last  string              `json:"last"` // zadnji pogovor v tem repozitoriju (za --continue)
	Convs map[string]wtRecord `json:"convs"`
}

func wtStatePath(top string) string {
	p, _ := gitUser(top, nil, "rev-parse", "--git-path", "minigw-continue.json")
	if p != "" && !filepath.IsAbs(p) {
		p = filepath.Join(top, p)
	}
	return p
}

func wtLoad(top string) (wtState, string) {
	st, p := wtState{}, wtStatePath(top)
	if b, err := os.ReadFile(p); err == nil {
		json.Unmarshal(b, &st)
	}
	if st.Convs == nil {
		st.Convs = map[string]wtRecord{}
	}
	return st, p
}

// wtSave: zamenjava datoteke naenkrat.
func wtSave(p string, st wtState) {
	if p == "" {
		return
	}
	b, _ := json.Marshal(st)
	if os.WriteFile(p+".tmp", b, 0o600) == nil {
		os.Rename(p+".tmp", p)
	}
}

// wtConversation: ključ pogovora — --continue: zadnji pogovor v repozitoriju; sicer seja (nova, --resume <id>,
// --session-id). Zapiše ga kot zadnjega (naslednji --continue nadaljuje ta pogovor).
func wtConversation(top, sid string, cont bool) string {
	st, p := wtLoad(top)
	key := sid
	if cont && st.Last != "" {
		key = st.Last
	}
	if st.Last != key {
		st.Last = key
		wtSave(p, st)
	}
	return key
}

// wtContinue: vrh prejšnje veje pogovora conv, če je mapa od zadnjega posnetka verige enaka (isti HEAD, isto drevo
// posnetka snap); "" = posnetek snap (why: zakaj ne, "" = brez sporočila: ni veje ali je že v HEAD).
// ponytail: ugnezdeni repozitoriji ne nadaljujejo (vsak bi rabil svojo verigo in gitlink starša); tedaj posnetek mape.
func wtContinue(top, conv, snap string, nested bool) (tip string, r wtRecord, why string) {
	if conv == "" {
		return "", r, ""
	}
	st, _ := wtLoad(top)
	r, ok := st.Convs[conv]
	if !ok || !branchRe.MatchString(r.Branch) || !shaRe.MatchString(r.Base) || !shaRe.MatchString(r.Head) {
		return "", r, ""
	}
	t, err := gitUser(top, nil, "rev-parse", "-q", "--verify", "refs/heads/"+r.Branch+"^{commit}")
	if err != nil || !shaRe.MatchString(t) {
		return "", r, "the branch no longer exists"
	}
	if _, err := gitUser(top, nil, "merge-base", "--is-ancestor", t, "HEAD"); err == nil {
		return "", r, "" // veja je že v HEAD (merge): posnetek mape jo ima
	}
	if nested {
		return "", r, "nested git repositories always start from your folder"
	}
	head, _ := gitUser(top, nil, "rev-parse", "HEAD")
	a, _ := gitUser(top, nil, "rev-parse", snap+"^{tree}")
	b, _ := gitUser(top, nil, "rev-parse", r.Base+"^{tree}")
	if head != r.Head || a == "" || a != b {
		return "", r, "your folder changed since the last turn"
	}
	return t, r, ""
}

// wtRemember: veja branch (začetek sent = poslani posnetek ali vrh) kot zadnja veja pogovora.
func (s *shareClient) wtRemember(branch, sent string) string {
	r, ok := s.chain[sent]
	if !ok {
		st, _ := wtLoad(s.top) // npr. prehod je ponovno uporabil klon seje (brez WORKTREE-PUSH): veriga iz zapisa
		if r, ok = st.Convs[s.conv]; !ok || !shaRe.MatchString(r.Base) {
			return sent
		}
		if _, err := gitUser(s.top, nil, "merge-base", "--is-ancestor", r.Base, "refs/heads/"+branch); err != nil {
			return sent
		}
	}
	if s.conv != "" {
		st, p := wtLoad(s.top)
		for k, o := range st.Convs { // pogovori, katerih veje ni več (uporabnik jo je izbrisal)
			if _, err := gitUser(s.top, nil, "rev-parse", "-q", "--verify", "refs/heads/"+o.Branch+"^{commit}"); err != nil || !branchRe.MatchString(o.Branch) {
				delete(st.Convs, k)
			}
		}
		st.Convs[s.conv] = wtRecord{Branch: branch, Base: r.Base, Head: r.Head}
		st.Last = s.conv
		wtSave(p, st)
	}
	return r.Base
}
