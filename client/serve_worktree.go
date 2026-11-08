package main

// --minigw-work=worktree na minigw serve (kot Worktree v Sources/RouterCore/Worktree.swift): agent dela v klonu git na
// veji minigw/<agent>-<seja8>-0; rezultat gre k odjemalcu kot veja (fetch le naprej, brez hookov in filtrov).
//
// 1. WORKTREE-PUSH: minigw pošlje posnetek (HEAD + neshranjene spremembe) kot bundle brez objektov, ki jih zrcalo ima.
// 2. Prehod ga uvozi v golo zrcalo (scratch/repos/<naprava>/<repo>.git) in naredi »git clone --shared« (scratch/worktrees).
// 3. Po agentu prehod neshranjene spremembe shrani v commit z nastavitvami zrcala in pošlje bundle veje (WORKTREE-PULL).
//
// Agentov klon je ves agentov (tudi .git/config, hooks): prehod v njem nikoli ne zažene git z njegovim .git; bere le
// HEAD in ref, objekte prek GIT_ALTERNATE_OBJECT_DIRECTORIES z nastavitvami zrcala. Git je pravi program (LookPath ob
// prvi rabi), brez sistemske in uporabniške nastavitve in hookov.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

type shareTree struct {
	repo, worktrees, sub string             // golo zrcalo, mapa klonov, pot cwd v repozitoriju ("" = koren)
	slots                map[string]*wtSlot // agent → klon (zaporedne zahteve iste seje nadaljujejo v njem)
	pushing, result      string             // bundle, ki ga minigw prav zdaj nalaga / prevzema
	resultErr            string             // L5: zakaj rezultata ni (odjemalec ga dobi ob WORKTREE-PULL)
}

type wtSlot struct{ wt, branch, base string }

// worktreeNeedsUnsandboxed (L3): na Windows Claude v načinu worktree le z »Allow commands without a sandbox«.
var worktreeNeedsUnsandboxed = runtime.GOOS == "windows"

// gitTimeout (L5): git prehoda (uvoz, klon, finalize) se ustavi, če traja dlje.
var gitTimeout = 10 * time.Minute

// cloneTooLarge (L5): klon, ki ga »git add -A« ne sme zapisati v zrcalo (meje kot kopija sync); "" = v redu.
func cloneTooLarge(wt string) string {
	files, bytes := 0, int64(0)
	filepath.WalkDir(wt, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && d.Name() == ".git" && filepath.Dir(p) == wt {
			return filepath.SkipDir
		}
		if d.Type().IsRegular() {
			files++
			if fi, err := d.Info(); err == nil {
				bytes += fi.Size()
			}
		}
		if files > maxSyncFiles || bytes > maxSyncBytes {
			return filepath.SkipAll
		}
		return nil
	})
	if files > maxSyncFiles || bytes > maxSyncBytes {
		return fmt.Sprintf("the agent's clone is too large to return (more than %d files or %d GB); add build output to .gitignore", maxSyncFiles, maxSyncBytes>>30)
	}
	return ""
}

const maxIncoming = int64(4) << 30 // največji bundle odjemalca (prvi prenos velikega repozitorija)

var gitExe = sync.OnceValues(func() (string, error) { return exec.LookPath("git") })

// gitEmpty: prazna mapa hookov in prazna globalna nastavitev ob zrcalih (prenosljivo: na Windows ni /dev/null za git).
func gitEmpty(repo string) (hooks, cfg string) {
	d := filepath.Join(filepath.Dir(filepath.Dir(repo)), ".minigw-git")
	hooks, cfg = filepath.Join(d, "hooks"), filepath.Join(d, "config")
	os.MkdirAll(hooks, 0o700)
	if f, err := os.OpenFile(cfg, os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
		f.Close()
	}
	return hooks, cfg
}

// gitRun: git prehoda z varovali (brez hookov, fsmonitor, podmodulov, protokolov razen datotek, vzdrževanja).
func gitRun(repo, cwd string, env []string, args ...string) (string, error) {
	exe, err := gitExe()
	if err != nil {
		return "", errors.New("git not found on the gateway (install git)")
	}
	hooks, cfg := gitEmpty(repo)
	guard := []string{"-c", "core.hooksPath=" + hooks, "-c", "core.fsmonitor=false", "-c", "submodule.recurse=false",
		"-c", "protocol.allow=never", "-c", "protocol.file.allow=always", "-c", "maintenance.auto=false", "-c", "gc.auto=0",
		"-c", "core.commitGraph=false", "-c", "core.multiPackIndex=false"}
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout) // L5
	defer cancel()
	cmd := commandContext(ctx, exe, append(guard, args...)...)
	cmd.WaitDelay = 5 * time.Second
	cmd.Dir = cwd
	for _, e := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(e), "GIT_") {
			cmd.Env = append(cmd.Env, e)
		}
	}
	cmd.Env = append(append(cmd.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+cfg, "GIT_TERMINAL_PROMPT=0"), env...)
	var out bytes.Buffer
	cmd.Stdout = &out
	err = cmd.Run()
	return strings.TrimSpace(out.String()), err
}

// ensureMirror: golo zrcalo; info/exclude: izhod gradnje nikoli v vejo (tudi brez .gitignore projekta).
func ensureMirror(repo string) error {
	if _, err := os.Stat(filepath.Join(repo, "HEAD")); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(repo), 0o700); err != nil {
		return err
	}
	if _, err := gitRun(repo, "", nil, "init", "-q", "--bare", repo); err != nil {
		return err
	}
	var ex []string
	for _, p := range defaultIgnore {
		if p != ".git/" {
			ex = append(ex, p)
		}
	}
	os.MkdirAll(filepath.Join(repo, "info"), 0o700)
	return os.WriteFile(filepath.Join(repo, "info", "exclude"), []byte(strings.Join(ex, "\n")+"\n"), 0o600)
}

// mirrorTips: vrhovi refov zrcala (odjemalec pošlje le manjkajoče).
func mirrorTips(repo string) []string {
	out, _ := gitRun(repo, "", nil, "--git-dir", repo, "for-each-ref", "--format=%(objectname)")
	tips := []string{}
	for _, l := range strings.Split(out, "\n") {
		if shaRe.MatchString(l) {
			tips = append(tips, l)
		}
	}
	return tips
}

// importBase: bundle odjemalca (ref refs/minigw/push) kot base zrcala; brez bundla mora posnetek že biti v zrcalu.
func importBase(repo, bundle, snapshot, base string) bool {
	if bundle != "" {
		if !isBundle(bundle) {
			return false
		}
		if _, err := gitRun(repo, "", nil, "--git-dir", repo, "-c", "transfer.fsckObjects=true", "-c", "fetch.fsckObjects=true",
			"fetch", "-q", "--no-tags", bundle, "+refs/minigw/push:"+base); err != nil {
			return false
		}
	} else if _, err := gitRun(repo, "", nil, "--git-dir", repo, "update-ref", base, snapshot); err != nil {
		return false
	}
	got, _ := gitRun(repo, "", nil, "--git-dir", repo, "rev-parse", base)
	return got == snapshot
}

// makeClone: klon za agenta na veji iz posnetka (objekti zrcala le za branje, novi v klonu).
func makeClone(repo, wt, branch, snapshot string) bool {
	os.RemoveAll(wt) // ostanek prekinjenega zagona
	os.MkdirAll(filepath.Dir(wt), 0o700)
	if _, err := gitRun(repo, "", nil, "clone", "-q", "--shared", "--no-checkout", repo, wt); err != nil {
		return false
	}
	_, err := gitRun(repo, "", nil, "-C", wt, "checkout", "-q", "-b", branch, snapshot)
	return err == nil
}

// readSmall: datoteka do 4 KB brez sledenja povezavi na zadnjem delu poti.
func readSmall(p string) string {
	if st, err := os.Lstat(p); err != nil || !st.Mode().IsRegular() || st.Size() > 4096 {
		return ""
	}
	b, _ := os.ReadFile(p)
	return strings.TrimSpace(string(b))
}

// cloneHead: HEAD agentovega klona brez git v njem (»ref: refs/heads/…« → loose ref ali packed-refs).
func cloneHead(gitDir string) string {
	h := readSmall(filepath.Join(gitDir, "HEAD"))
	ref, ok := strings.CutPrefix(h, "ref: ")
	if !ok {
		if shaRe.MatchString(h) {
			return h
		}
		return ""
	}
	if !strings.HasPrefix(ref, "refs/") || strings.Contains(ref, "..") || strings.ContainsAny(ref, "\x00\\") {
		return ""
	}
	if v := readSmall(filepath.Join(gitDir, filepath.FromSlash(ref))); shaRe.MatchString(v) {
		return v
	}
	for _, l := range strings.Split(readSmall(filepath.Join(gitDir, "packed-refs")), "\n") {
		if strings.HasSuffix(l, " "+ref) && shaRe.MatchString(l[:min(40, len(l))]) {
			return l[:40]
		}
	}
	return ""
}

// realDir: pot je prava mapa (ne povezava ali spoj).
func realDir(p string) bool {
	st, err := os.Lstat(p)
	return err == nil && st.IsDir() && st.Mode()&os.ModeSymlink == 0
}

// finalize: commiti agenta + njegove neshranjene spremembe (commit prehoda) → refs/heads/<branch> zrcala in bundle od
// posnetka do vrha v out. Vse z nastavitvami zrcala; objekti klona le kot dodatni vir. Vrne vrh ali "".
// Procesi agenta so ob tem končani (Linux: pomočnik pobije potomce; Windows: agenti tam tečejo brez peskovnika in
// imajo že vse pravice uporabnika). ponytail: brez sandbox-exec kot na Macu; dodaj bwrap za git add, če agenti
// na Linuxu kdaj ostanejo živi po koncu zahteve.
func finalize(repo, wt, branch, base, agent, out string) string {
	git, objects := filepath.Join(wt, ".git"), filepath.Join(wt, ".git", "objects")
	if !realDir(git) {
		return ""
	}
	// L2: objects/info klona (alternates, commit-graph) je agentov: zamenja ga le alternates zrcala, vse relativno na .git
	// (os.Root: povezava ali spoj na objects ali info ne preusmeri brisanja ali pisanja ven).
	root, err := os.OpenRoot(git)
	if err != nil {
		return ""
	}
	defer root.Close()
	if st, err := root.Lstat("objects"); err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return ""
	}
	info := filepath.Join("objects", "info")
	if err := root.RemoveAll(info); err != nil || root.Mkdir(info, 0o755) != nil {
		return ""
	}
	if root.WriteFile(filepath.Join(info, "alternates"), []byte(filepath.ToSlash(filepath.Join(repo, "objects"))+"\n"), 0o644) != nil {
		return ""
	}
	tmp, err := os.MkdirTemp(filepath.Dir(repo), ".finalize-")
	if err != nil {
		return ""
	}
	defer os.RemoveAll(tmp)
	env := []string{"GIT_ALTERNATE_OBJECT_DIRECTORIES=" + objects}
	g := func(e []string, cwd string, a ...string) (string, error) {
		return gitRun(repo, cwd, append(append([]string{}, env...), e...), append([]string{"--git-dir", repo}, a...)...)
	}
	ancestor := func(a, b string) bool { _, err := g(nil, tmp, "merge-base", "--is-ancestor", a, b); return err == nil }
	tip := cloneHead(git)
	if tip == "" || !ancestor(base, tip) { // vrh mora biti potomec posnetka (agent ne podtakne tuje zgodovine)
		tip = base
	}
	parents := []string{tip} // veja se pri odjemalcu le premika naprej
	prev, _ := g(nil, tmp, "rev-parse", "-q", "--verify", "refs/heads/"+branch+"^{commit}")
	if p := prev; shaRe.MatchString(p) && p != tip {
		if ancestor(tip, p) {
			tip, parents = p, []string{p}
		} else if !ancestor(p, tip) {
			parents = []string{tip, p}
		}
	}
	e := []string{"GIT_INDEX_FILE=" + filepath.Join(tmp, "index"), "GIT_WORK_TREE=" + wt}
	if _, err := g(e, tmp, "read-tree", tip); err == nil {
		if _, err := g(e, wt, "add", "-A"); err == nil {
			t, _ := g(e, tmp, "write-tree")
			old, _ := g(nil, tmp, "rev-parse", tip+"^{tree}")
			if shaRe.MatchString(t) && (len(parents) > 1 || old != t) {
				a := []string{"-c", "user.name=Mini Gateway", "-c", "user.email=minigw@localhost", "commit-tree", t}
				for _, p := range parents {
					a = append(a, "-p", p)
				}
				if c, _ := g(nil, tmp, append(a, "-m", "minigw: uncommitted changes of "+agent)...); shaRe.MatchString(c) {
					tip = c
				}
			}
		}
	}
	if _, err := g(nil, tmp, "update-ref", "refs/heads/"+branch, tip); err != nil {
		return ""
	}
	os.Remove(out)
	if tip == base {
		return tip // agent ni spremenil ničesar
	}
	_, err = g(nil, tmp, "bundle", "create", out, "refs/heads/"+branch, "^"+base)
	if err == nil && isBundle(out) {
		_, err = gitRun(repo, tmp, nil, "--git-dir", repo, "-c", "transfer.fsckObjects=true", "-c", "fetch.fsckObjects=true",
			"bundle", "unbundle", out) // objekti tudi v zrcalo (klon izgine); fsck zavrne npr. drevo z ».git«
	}
	if err != nil || !isBundle(out) { // ref zrcala nazaj: sicer bi kazal na objekte, ki jih ni (le v klonu)
		if shaRe.MatchString(prev) {
			g(nil, tmp, "update-ref", "refs/heads/"+branch, prev)
		} else {
			g(nil, tmp, "update-ref", "-d", "refs/heads/"+branch)
		}
		os.Remove(out)
		return ""
	}
	return tip
}

// worktreeHarnessEnv: okolje procesov agenta v klonu (kot Worktree.harnessEnv): nastavitve iz okolja preglasijo
// .git/config klona (fsmonitor, hooki, atributi), GIT_ATTR_SOURCE = prazno drevo izklopi .gitattributes, GIT_DIR in
// GIT_WORK_TREE: ugnezden .git agenta ne velja; Maven dobi svoj lokalni repozitorij.
func worktreeHarnessEnv(wt, maven string) []string {
	tmp := filepath.Join(wt, ".build", "tmp")
	return []string{"GIT_CONFIG_COUNT=3", "GIT_CONFIG_KEY_0=core.fsmonitor", "GIT_CONFIG_VALUE_0=false",
		"GIT_CONFIG_KEY_1=core.hooksPath", "GIT_CONFIG_VALUE_1=" + os.DevNull, // ne v klonu: tja agent lahko piše
		"GIT_CONFIG_KEY_2=core.attributesFile", "GIT_CONFIG_VALUE_2=" + os.DevNull,
		"GIT_ATTR_SOURCE=4b825dc642cb6eb9a060e54bf8d69288fbee4904",
		"GIT_DIR=" + filepath.Join(wt, ".git"), "GIT_WORK_TREE=" + wt, "GIT_COMMON_DIR=" + filepath.Join(wt, ".git"),
		"MAVEN_OPTS=-Dmaven.repo.local=" + maven + " -Djava.io.tmpdir=" + tmp, "TMPDIR=" + tmp, "TMP=" + tmp, "TEMP=" + tmp}
}

// worktreeNote: navodilo agentu v klonu.
// Besedilo kot ClaudeCLI.mountNote na Macu, brez stavka o minigw run (ta je le na Macu).
func worktreeNote(clientCwd, dir, branch string) string {
	return "The user's folder " + clientCwd + " (on their computer, a git repository) is a clone here at " + dir + ", your working" +
		" directory, on branch " + branch + ". Map paths under " + clientCwd + " to it. Commit your work on this branch; changes you leave" +
		" uncommitted are committed for you at the end. The branch goes back to the user's repository; their files are not changed." +
		" Build output (.build, target, DerivedData) is never committed."
}

// worktreeClaudeArgs: v peskovniku Claude Code (ne Windows) še pisanje v Maven agentov in branje objektov zrcala; .git
// klona (config, hooks, info, commondir) ni pisljiv, Edit/Write vanj ne (nastavitve in hooke bere git samega claude).
func worktreeClaudeArgs(args []string, wt, maven, objects string) []string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] != "--settings" {
			continue
		}
		var st map[string]any
		if json.Unmarshal([]byte(args[i+1]), &st) != nil {
			continue
		}
		if sb, _ := st["sandbox"].(map[string]any); sb != nil {
			if fs, _ := sb["filesystem"].(map[string]any); fs != nil {
				fs["allowWrite"] = append(toStrings(fs["allowWrite"]), wt, maven) // koren klona (tudi .git za commit), ko je cwd podmapa
				fs["allowRead"] = append(toStrings(fs["allowRead"]), wt, maven, objects)
				var deny []string
				for _, d := range []string{"config", "hooks", "info", "commondir"} {
					deny = append(deny, filepath.Join(wt, ".git", d))
				}
				fs["denyWrite"] = deny
			}
		}
		b, _ := json.Marshal(st)
		args[i+1] = string(b)
	}
	args = append(args, "--add-dir", wt) // koren klona tudi za orodja Read/Edit, ko je cwd podmapa
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--allowedTools" && args[i+1] != "" {
			for _, t := range []string{"Read", "Edit", "Write", "MultiEdit", "Glob", "Grep"} {
				args[i+1] += "," + t + "(" + ruleAbs(wt) + "/**)"
			}
			break
		}
	}
	var edits []string
	for _, p := range []string{"/.git", "/.git/**", "/**/.git", "/**/.git/**"} {
		edits = append(edits, "Edit("+ruleAbs(wt)+p+")", "Write("+ruleAbs(wt)+p+")")
	}
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--disallowedTools" {
			args[i+1] = strings.Join(append([]string{args[i+1]}, edits...), ",")
			return args
		}
	}
	return append(args, "--disallowedTools", strings.Join(edits, ","))
}

func toStrings(v any) []string {
	var out []string
	l, _ := v.([]any)
	for _, x := range l {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	if l == nil {
		out, _ = v.([]string)
	}
	return out
}

// --- shareHub ---

// worktreeMount: klon seje in agenta (nov iz posnetka odjemalca); "" ob napaki. Kliče se pod zaklepom mape (lock).
func (h *shareHub) worktreeMount(s *serveShare, agent string) string {
	h.mu.Lock()
	t, ro := s.tree, s.readOnly
	slot := t.slots[agent]
	h.mu.Unlock()
	if ro {
		return ""
	}
	if slot != nil && realDir(filepath.Join(slot.wt, ".git")) {
		return worktreeDir(slot.wt, t.sub)
	}
	short := sha256Hex([]byte(s.session))[:8]
	name := agent + "-" + short + "-0"
	branch, wt, base := "minigw/"+name, filepath.Join(t.worktrees, name), "refs/minigw/base/"+name
	if ensureMirror(t.repo) != nil {
		return ""
	}
	touchUsed(t.repo) // pruneMirrors: zadnja raba
	incoming := t.repo + ".incoming"
	os.Remove(incoming)
	h.mu.Lock()
	t.pushing = incoming
	h.mu.Unlock()
	body, _ := json.Marshal(map[string][]string{"have": mirrorTips(t.repo)})
	r := h.sendJob(s, "WORKTREE-PUSH", body, 900*time.Second)
	h.mu.Lock()
	t.pushing = ""
	h.mu.Unlock()
	defer os.Remove(incoming)
	var reply struct {
		Snapshot string `json:"snapshot"`
	}
	if r == nil || r.Status != 200 || json.Unmarshal(r.Body, &reply) != nil || !shaRe.MatchString(reply.Snapshot) {
		return ""
	}
	file := ""
	if _, err := os.Stat(incoming); err == nil {
		file = incoming
	}
	if !importBase(t.repo, file, reply.Snapshot, base) || !makeClone(t.repo, wt, branch, reply.Snapshot) {
		return ""
	}
	for _, d := range []string{filepath.Join(wt, ".build", "tmp"), filepath.Join(t.worktrees, ".maven")} {
		os.MkdirAll(d, 0o700)
	}
	h.mu.Lock()
	t.slots[agent] = &wtSlot{wt: wt, branch: branch, base: reply.Snapshot}
	h.mu.Unlock()
	return worktreeDir(wt, t.sub)
}

func worktreeDir(wt, sub string) string {
	d := filepath.Join(wt, filepath.FromSlash(sub))
	if realDir(d) {
		return d
	}
	return wt // podmapa, ki je git ne pozna (prazna)
}

// worktreeOf: klon, veja, Maven in objekti zrcala seje za tega agenta (za okolje in peskovnik agenta).
func (h *shareHub) worktreeOf(session, agent string) (wt, branch, maven, objects string, ok bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.bySession[session]
	if s == nil || s.tree == nil || s.tree.slots[agent] == nil {
		return "", "", "", "", false
	}
	sl := s.tree.slots[agent]
	return sl.wt, sl.branch, filepath.Join(s.tree.worktrees, ".maven"), filepath.Join(s.tree.repo, "objects"), true
}

// worktreeBack: po agentu veja v zrcalu in prevzem pri odjemalcu (WORKTREE-PULL).
func (h *shareHub) worktreeBack(s *serveShare, agent string) {
	h.mu.Lock()
	t := s.tree
	slot := t.slots[agent]
	h.mu.Unlock()
	if slot == nil {
		return
	}
	killInside(slot.wt)
	out := t.repo + ".result"
	defer os.Remove(out)
	why, tip := cloneTooLarge(slot.wt), ""
	if why == "" {
		if tip = finalize(t.repo, slot.wt, slot.branch, slot.base, agent, out); tip == slot.base {
			return // agent ni spremenil ničesar
		} else if tip == "" {
			why = "the gateway could not save the agent's work as a branch (see the gateway log)"
		} else if st, err := os.Stat(out); err != nil || st.Size() > maxSyncFile {
			why = "the agent's branch is larger than 64 MB; it stays on the gateway's mirror (" + slot.branch + ")"
		}
	}
	if why != "" {
		h.logf("worktree %s (%s): %s", slot.branch, s.session, why)
	}
	h.mu.Lock()
	t.result, t.resultErr = out, why
	h.mu.Unlock()
	body, _ := json.Marshal(map[string]string{"branch": slot.branch, "tip": tip})
	h.sendJob(s, "WORKTREE-PULL", body, 300*time.Second)
	h.mu.Lock()
	t.result, t.resultErr = "", ""
	h.mu.Unlock()
}

// worktreePut: del bundla odjemalca med WORKTREE-PUSH (del 0 začne datoteko). Status HTTP.
func (h *shareHub) worktreePut(key string, part int, data []byte, owner string) int {
	h.mu.Lock()
	s := h.share(key, owner)
	file := ""
	if s != nil && s.tree != nil {
		s.lastSeen = time.Now()
		file = s.tree.pushing
	}
	h.mu.Unlock()
	if file == "" || part < 0 {
		return 409
	}
	flags := os.O_WRONLY | os.O_APPEND
	if part == 0 {
		flags |= os.O_CREATE | os.O_TRUNC
	}
	f, err := os.OpenFile(file, flags, 0o600)
	if err != nil {
		return 409
	}
	defer f.Close()
	if st, _ := f.Stat(); st == nil || st.Size()+int64(len(data)) > maxIncoming {
		os.Remove(file)
		return 413
	}
	if _, err := f.Write(data); err != nil {
		return 500
	}
	return 204
}

// worktreeResult: bundle veje za odjemalca med WORKTREE-PULL.
func (h *shareHub) worktreeResult(key, owner string) ([]byte, int, string) {
	h.mu.Lock()
	s := h.share(key, owner)
	file, why := "", ""
	if s != nil && s.tree != nil {
		s.lastSeen = time.Now()
		file, why = s.tree.result, s.tree.resultErr
	}
	h.mu.Unlock()
	switch {
	case why != "":
		return nil, 413, why
	case file == "":
		return nil, 404, "No result"
	}
	if st, err := os.Lstat(file); err != nil || !st.Mode().IsRegular() || st.Size() > maxSyncFile {
		return nil, 413, "the agent's branch is larger than 64 MB"
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return nil, 500, err.Error()
	}
	return b, 200, ""
}

// dropClones: ob koncu seje kloni gredo (veje ostanejo v zrcalu in pri odjemalcu). Pod h.mu.
func dropClones(t *shareTree) {
	for _, sl := range t.slots {
		go os.RemoveAll(sl.wt)
	}
	t.slots = map[string]*wtSlot{}
}

// pruneMirrors: zrcala (scratch/repos/<naprava>/<repo>.git), ki jih 30 dni ni uporabil noben agent, in njihovi kloni.
func pruneMirrors(scratch string, now time.Time) {
	repos := filepath.Join(scratch, "repos")
	owners, _ := os.ReadDir(repos)
	for _, o := range owners {
		if !o.IsDir() || strings.HasPrefix(o.Name(), ".") {
			continue
		}
		list, _ := os.ReadDir(filepath.Join(repos, o.Name()))
		for _, e := range list {
			name, ok := strings.CutSuffix(e.Name(), ".git")
			if !ok || !e.IsDir() {
				continue
			}
			repo := filepath.Join(repos, o.Name(), e.Name())
			used := time.Time{}
			if st, err := os.Stat(repo + ".used"); err == nil {
				used = st.ModTime()
			} else if st, err := os.Stat(repo); err == nil {
				used = st.ModTime()
			}
			if now.Sub(used) > 30*24*time.Hour {
				os.RemoveAll(repo)
				os.Remove(repo + ".used")
				os.RemoveAll(filepath.Join(scratch, "worktrees", o.Name(), name))
			}
		}
	}
}

// worktreeTree: zrcalo in kloni za repozitorij odjemalca (top, cwd: poti odjemalca, na Windows z »\«).
func worktreeTree(scratch, owner, top, cwd string) (*shareTree, error) {
	top, cwd = strings.ReplaceAll(top, `\`, "/"), strings.ReplaceAll(cwd, `\`, "/")
	top = strings.TrimSuffix(top, "/")
	if top == "" || (cwd != top && !strings.HasPrefix(cwd, top+"/")) {
		return nil, errors.New("Worktree mode needs a writable git repository that contains the folder")
	}
	sub := strings.Trim(strings.TrimPrefix(cwd, top), "/")
	if sub != "" {
		for _, p := range strings.Split(sub, "/") {
			if p == "" || p == "." || p == ".." || strings.ContainsAny(p, "\x00:") || isGitPath(p) {
				return nil, errors.New("Bad folder path")
			}
		}
	}
	id := sha256Hex([]byte(top))[:16]
	return &shareTree{repo: filepath.Join(scratch, "repos", owner, id+".git"), worktrees: filepath.Join(scratch, "worktrees", owner, id),
		sub: sub, slots: map[string]*wtSlot{}}, nil
}

// isWorktree: seja dela v načinu worktree (pred mount: Grok in agenti brez pisanja ga zavrnejo).
func (h *shareHub) isWorktree(session string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.bySession[session]
	return s != nil && s.tree != nil
}
