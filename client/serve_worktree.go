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
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

type shareTree struct {
	repo, worktrees, sub string             // golo zrcalo, mapa klonov, pot cwd v repozitoriju ("" = koren)
	slots                map[string]*wtSlot // agent → klon (zaporedne zahteve iste seje nadaljujejo v njem)
	pushing              string             // bundle, ki ga minigw prav zdaj nalaga (ugnezdeni: <pushing>.<i>)
	results              []string           // bundli, ki jih minigw prav zdaj prevzema (0 = vrhnji, i = i-ti ugnezdeni v WORKTREE-PULL)
	resultErr            string             // L5: zakaj rezultata ni (odjemalec ga dobi ob WORKTREE-PULL)
}

type wtSlot struct {
	wt, branch, base string
	git              os.FileInfo // H2: .git klona ob kloniranju (zamenjan .git = nov config, filtri, atributi)
	cfg              []byte      // .git/config ob kloniranju
	nested           []*wtNested // ugnezdeni repozitoriji odjemalca: vsak svoj klon v wt na svoji poti, starš pred otrokom
	unconfined       bool        // N1 (Linux): agent je tekel brez svojega imenskega prostora PID (lahko je preživel)
}

// wtNested: klon ugnezdenega repozitorija (podmodul ali nesleden) iz svojega zrcala, na isti veji kot vrhnji.
type wtNested struct {
	path, repo, base string      // pot v klonu (»/«), golo zrcalo, posnetek odjemalca
	git              os.FileInfo // H2 kot wtSlot.git
	cfg              []byte
}

func (n *wtNested) dir(wt string) string { return filepath.Join(wt, filepath.FromSlash(n.path)) }

// gitDirs: .git vseh klonov slota (vrhnji prvi): edini GIT_DIR, ki ga ovoj pusti, in .git, ki jih ščitijo peskovniki.
func (sl *wtSlot) gitDirs() []string {
	out := []string{filepath.Join(sl.wt, ".git")}
	for _, n := range sl.nested {
		out = append(out, filepath.Join(n.dir(sl.wt), ".git"))
	}
	return out
}

// gitIntact (H2): .git vsakega klona (vrhnji in ugnezdeni) je še ista mapa z istim config, brez info/attributes in
// commondir. Claude Code poganja git sam, zunaj svojega peskovnika (git status ob zagonu): zamenjan .git (mv .git; nov
// config s filter.*.clean ali info/attributes) bi izvedel ukaz agenta brez peskovnika. config, hooks, info in commondir
// so v denyWrite.
func (sl *wtSlot) gitIntact() bool {
	if !gitSame(filepath.Join(sl.wt, ".git"), sl.git, sl.cfg) {
		return false
	}
	for _, n := range sl.nested {
		if !gitSame(filepath.Join(n.dir(sl.wt), ".git"), n.git, n.cfg) {
			return false
		}
	}
	return true
}

func gitSame(g string, git os.FileInfo, cfg []byte) bool {
	st, err := os.Lstat(g)
	if err != nil || git == nil || !os.SameFile(st, git) {
		return false
	}
	b, err := os.ReadFile(filepath.Join(g, "config"))
	for _, p := range []string{filepath.Join("info", "attributes"), "commondir"} {
		if _, e := os.Lstat(filepath.Join(g, p)); !os.IsNotExist(e) {
			return false
		}
	}
	return err == nil && bytes.Equal(b, cfg)
}

// nestedIntact (H1): pot ugnezdenega klona so same prave mape (ne povezava ven) in njegov .git je še ista mapa.
func nestedIntact(wt string, n *wtNested) bool {
	d := wt
	for _, c := range strings.Split(n.path, "/") {
		if d = filepath.Join(d, c); !realDir(d) {
			return false
		}
	}
	st, err := os.Lstat(filepath.Join(d, ".git"))
	return err == nil && n.git != nil && os.SameFile(st, n.git)
}

// worktreeNeedsUnsandboxed (L3): na Windows Claude v načinu worktree le z »Allow commands without a sandbox«.
var worktreeNeedsUnsandboxed = runtime.GOOS == "windows"

// gitTimeout (L5): git prehoda (uvoz, klon, finalize) se ustavi, če traja dlje.
var gitTimeout = 10 * time.Minute

// cloneProblem: klon, ki ga »git add -A« ne sme zapisati v zrcalo (L5: meje kot kopija sync) ali nestedProblem; "" = v redu.
func cloneProblem(wt, repo string, nested []*wtNested) string {
	if why := nestedProblem(wt, repo, nested); why != "" {
		return why
	}
	files, bytes := 0, int64(0)
	filepath.WalkDir(wt, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && strings.EqualFold(d.Name(), ".git") {
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

// nestedProblem (N9): .git (mapa ali datoteka) pod korenom klona na poti, ki je git ne prezre, razen točno klonov
// ugnezdenih repozitorijev odjemalca (nested, vsak s svojim zrcalom): repozitorij, ki ga je naredil agent, ne pride
// nazaj, veja ne gre odjemalcu, klon se ne uporabi znova. V prezrti poti (build/_deps od CMake FetchContent) je v redu.
// O prezrtju odloči git repozitorija, v katerem je (check-ignore z ovojem in nastavitvami njegovega zrcala, kot
// finalize: .git klona je agentov). V vsakem klonu se izpusti le točno njegov ».git«.
func nestedProblem(wt, repo string, nested []*wtNested) string {
	own := map[string]bool{}
	for _, n := range nested {
		own[n.dir(wt)] = true
	}
	check := func(root, repo, prefix string) string {
		var found []string
		filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.Name() == ".git" && filepath.Dir(p) == root {
				return filepath.SkipDir
			}
			if d.IsDir() && own[p] && p != root {
				return filepath.SkipDir // svoj klon, preveri ga njegov krog
			}
			if strings.EqualFold(d.Name(), ".git") {
				rel, _ := filepath.Rel(root, filepath.Dir(p))
				found = append(found, filepath.ToSlash(rel)+"/")
				if d.IsDir() {
					return filepath.SkipDir
				}
			}
			return nil
		})
		if len(found) == 0 {
			return ""
		}
		ignored := map[string]bool{}
		if env := gitWrapEnv([]string{repo}, gitExecDir(repo)); env != nil {
			out, _ := gitRunInput(repo, root, env, strings.Join(found, "\n")+"\n", "--git-dir", repo, "--work-tree", root, "check-ignore", "--stdin")
			for _, l := range strings.Split(out, "\n") {
				ignored[l] = true
			}
		}
		for _, n := range found {
			if !ignored[n] {
				return "changes inside the nested repository " + prefix + strings.TrimSuffix(n, "/") + " do not come back in worktree mode; use sync"
			}
		}
		return ""
	}
	if why := check(wt, repo, ""); why != "" {
		return why
	}
	for _, n := range nested {
		if why := check(n.dir(wt), n.repo, n.path+"/"); why != "" {
			return why
		}
	}
	return ""
}

// gitExecDir: mapa ovoja gitExecWrap v Scratch zrcala repo (scratch/repos/<naprava>/<id>.git → scratch/gitexec).
func gitExecDir(repo string) string {
	return filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(repo))), "gitexec")
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
	return gitRunInput(repo, cwd, env, "", args...)
}

// gitRunInput: gitRun z vhodom na stdin.
func gitRunInput(repo, cwd string, env []string, input string, args ...string) (string, error) {
	return gitRunBox(nil, repo, cwd, env, input, args...)
}

// gitBox (N1): git v bwrap z delovnim drevesom ugnezdenega klona iz odprte mape dir, pripete na mount (boxGit).
type gitBox struct {
	dir        *os.File
	mount, tmp string
}

// gitRunBox: gitRunInput, z box v bwrap (boxGit); brez bwrap napaka (nikoli brez peskovnika po poti).
func gitRunBox(box *gitBox, repo, cwd string, env []string, input string, args ...string) (string, error) {
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
	name, argv := exe, append(guard, args...)
	var files []*os.File
	if box != nil {
		ok := false
		if name, argv, files, ok = boxGit(box.dir, box.mount, repo, box.tmp, exe, append([]string{"-C", cwd}, argv...)); !ok {
			return "", errors.New("bwrap is not available for the nested repository")
		}
		cwd = box.tmp
	}
	cmd := commandContext(ctx, name, argv...)
	cmd.ExtraFiles = files
	cmd.WaitDelay = 5 * time.Second
	cmd.Dir = cwd
	for _, e := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(e), "GIT_") {
			cmd.Env = append(cmd.Env, e)
		}
	}
	cmd.Env = append(append(cmd.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+cfg, "GIT_TERMINAL_PROMPT=0"), env...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stdin = &out, strings.NewReader(input)
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
		_, err := gitRun(repo, "", nil, "--git-dir", repo, "-c", "transfer.fsckObjects=true", "-c", "fetch.fsckObjects=true",
			"fetch", "-q", "--no-tags", bundle, "+refs/minigw/push:"+base)
		if err != nil || !fsckBase(repo, base) {
			gitRun(repo, "", nil, "--git-dir", repo, "update-ref", "-d", base)
			return false
		}
	} else if _, err := gitRun(repo, "", nil, "--git-dir", repo, "update-ref", base, snapshot); err != nil {
		return false
	}
	got, _ := gitRun(repo, "", nil, "--git-dir", repo, "rev-parse", base)
	return got == snapshot
}

// fsckBase (#54): fetch iz bundla pred git 2.46 ne preveri objektov (fetch.fsckObjects prezre), novejši pa ne objektov,
// ki so v zrcalu že brez refa. Zato vsi objekti base, nedosegljivi iz drugih refov: pack-objects in index-pack
// --fsck-objects v prazen repozitorij z objekti zrcala kot alternates (kot finalize in fsckIncoming odjemalca).
func fsckBase(repo, base string) bool {
	refs, err := gitRun(repo, "", nil, "--git-dir", repo, "for-each-ref", "--format=%(objectname) %(refname)")
	if err != nil {
		return false
	}
	in := base + "\n--not\n"
	for _, l := range strings.Split(refs, "\n") {
		if sha, ref, _ := strings.Cut(l, " "); shaRe.MatchString(sha) && ref != base {
			in += sha + "\n"
		}
	}
	dir, err := os.MkdirTemp(repo, "minigw-fsck-") // 0700, v zrcalu (ne v skupni začasni mapi)
	if err != nil {
		return false
	}
	defer os.RemoveAll(dir)
	pack, err := gitRunInput(repo, dir, nil, in, "--git-dir", repo, "pack-objects", "--revs", "-q", filepath.Join(dir, "p"))
	if err != nil || !shaRe.MatchString(pack) {
		return false
	}
	fsck := filepath.Join(dir, "fsck.git")
	if _, err := gitRun(repo, dir, nil, "init", "-q", "--bare", fsck); err != nil {
		return false
	}
	objs, err := filepath.Abs(filepath.Join(repo, "objects"))
	if err != nil || os.WriteFile(filepath.Join(fsck, "objects", "info", "alternates"), []byte(filepath.ToSlash(objs)+"\n"), 0o600) != nil {
		return false
	}
	_, err = gitRun(repo, dir, nil, "--git-dir", fsck, "index-pack", "--fsck-objects", filepath.Join(dir, "p-"+pack+".pack"))
	return err == nil
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
	return headFrom(func(p string) string { return readSmall(filepath.Join(gitDir, filepath.FromSlash(p))) })
}

// cloneHeadRoot: cloneHead prek os.Root (N1: brez poti z deli, ki jih agent lahko zamenja).
func cloneHeadRoot(r *os.Root) string {
	return headFrom(func(p string) string {
		if st, err := r.Lstat(filepath.FromSlash(p)); err != nil || !st.Mode().IsRegular() || st.Size() > 4096 {
			return ""
		}
		b, _ := r.ReadFile(filepath.FromSlash(p))
		return strings.TrimSpace(string(b))
	})
}

func headFrom(readSmall func(string) string) string {
	h := readSmall("HEAD")
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
	if v := readSmall(ref); shaRe.MatchString(v) {
		return v
	}
	for _, l := range strings.Split(readSmall("packed-refs"), "\n") {
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
// links: neposredni ugnezdeni klon (pot v wt) → njegov novi vrh ("" = nespremenjen): git add ga izpusti, gitlink na tej
// poti (podmodul) dobi novi vrh.
// Procesi agenta so ob tem končani (Linux: pomočnik pobije potomce; Windows: agenti tam tečejo brez peskovnika in
// imajo že vse pravice uporabnika). ponytail: brez sandbox-exec kot na Macu; dodaj bwrap za git add, če agenti
// na Linuxu kdaj ostanejo živi po koncu zahteve.
// nested (N1, ugnezden klon): mapa klona, odprta prek os.Root iz korena klona prehoda; .git le prek nje, git v bwrap z
// drevesom, pripetim iz te mape (gitBox); wt se tedaj ne uporablja.
func finalize(repo, wt, branch, base, agent, out string, links map[string]string, nested *os.Root) string {
	git := filepath.Join(wt, ".git")
	var root *os.Root
	var err error
	if nested != nil {
		root, err = nested.OpenRoot(".git")
	} else if realDir(git) {
		// L2: objects/info klona (alternates, commit-graph) je agentov: zamenja ga le alternates zrcala, vse relativno na
		// .git (os.Root: povezava ali spoj na objects ali info ne preusmeri brisanja ali pisanja ven).
		root, err = os.OpenRoot(git)
	} else {
		return ""
	}
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
	var box *gitBox
	if nested != nil {
		d, err := nested.Open(".")
		if err != nil {
			return ""
		}
		defer d.Close()
		wt = filepath.Join(tmp, "wt") // točka priklopa v bwrap
		if os.Mkdir(wt, 0o700) != nil {
			return ""
		}
		box = &gitBox{dir: d, mount: wt, tmp: tmp}
		if duringFinalize != nil {
			duringFinalize()
		}
	}
	objects := filepath.Join(wt, ".git", "objects")
	wrap := gitWrapEnv([]string{repo}, gitExecDir(repo)) // N9: tudi git prehoda v klonu ne vstopi v ugnezden repozitorij
	if wrap == nil {
		return ""
	}
	env := append([]string{"GIT_ALTERNATE_OBJECT_DIRECTORIES=" + objects}, wrap...)
	g := func(e []string, cwd string, a ...string) (string, error) {
		return gitRunBox(box, repo, cwd, append(append([]string{}, env...), e...), "", append([]string{"--git-dir", repo}, a...)...)
	}
	ancestor := func(a, b string) bool { _, err := g(nil, tmp, "merge-base", "--is-ancestor", a, b); return err == nil }
	tip := cloneHeadRoot(root)
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
	add := []string{"add", "-A", "--", "."}
	for p := range links {
		add = append(add, ":(exclude,literal)"+p)
	}
	if _, err := g(e, tmp, "read-tree", tip); err == nil {
		if _, err := g(e, wt, add...); err == nil {
			for p, t := range links {
				if l, _ := g(e, tmp, "ls-files", "-s", "--", ":(literal)"+p); t != "" && strings.HasPrefix(l, "160000 ") && !strings.Contains(l, "\n") {
					g(e, tmp, "update-index", "--cacheinfo", "160000,"+t+","+p)
				}
			}
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
	restore := func() { // ref zrcala nazaj: sicer bi kazal na objekte, ki jih ni (le v klonu)
		if shaRe.MatchString(prev) {
			gitRun(repo, tmp, nil, "--git-dir", repo, "update-ref", "refs/heads/"+branch, prev)
		} else {
			gitRun(repo, tmp, nil, "--git-dir", repo, "update-ref", "-d", "refs/heads/"+branch)
		}
	}
	// Bundle in preverba v začasni mapi (v bwrap je pisljiva le ta in zrcalo), nato v out.
	b := filepath.Join(tmp, "result.bundle")
	_, err = g(nil, tmp, "bundle", "create", b, "refs/heads/"+branch, "^"+base)
	if err == nil && isBundle(b) {
		// I1: fsck vseh objektov base..tip (tudi tistih, ki jih je git add že zapisal v zrcalo; fetch bi jih preskočil):
		// pack-objects base..tip in index-pack --fsck-objects v prazen repozitorij (zavrne npr. drevo z ».git«).
		// Paket v mapo zrcala (git ga sestavi v objects/pack zrcala; preimenovanje čez mejo priklopa v bwrap ne gre).
		var pack string
		pdir := filepath.Join(repo, "minigw-fsck")
		os.RemoveAll(pdir)
		defer os.RemoveAll(pdir)
		if os.Mkdir(pdir, 0o700) != nil {
			err = errors.New("fsck folder")
		} else if pack, err = gitRunBox(box, repo, tmp, env, tip+"\n^"+base+"\n", "--git-dir", repo, "pack-objects", "--revs", "-q",
			filepath.Join(pdir, "p")); err == nil && shaRe.MatchString(pack) {
			fsck := filepath.Join(tmp, "fsck.git")
			if _, err = gitRun(repo, tmp, nil, "init", "-q", "--bare", fsck); err == nil {
				_, err = gitRun(repo, tmp, nil, "--git-dir", fsck, "index-pack", "--fsck-objects", filepath.Join(pdir, "p-"+pack+".pack"))
			}
		} else if err == nil {
			err = errors.New("pack-objects")
		}
	}
	if err == nil && isBundle(b) {
		// Objekti tudi v zrcalo (klon izgine), brez objektov klona. Ref pred tem nazaj: ko je agent vse commital sam,
		// kaže na commit le v klonu (preverba povezanosti bi padla), potem znova na vrh.
		restore()
		_, err = gitRun(repo, tmp, nil, "--git-dir", repo, "bundle", "unbundle", b)
		if err == nil {
			_, err = gitRun(repo, tmp, nil, "--git-dir", repo, "update-ref", "refs/heads/"+branch, tip)
		}
		if err == nil {
			err = os.Rename(b, out)
		}
	}
	if err != nil || !isBundle(out) {
		restore()
		os.Remove(out)
		return ""
	}
	return tip
}

// worktreeHarnessEnv: okolje procesov agenta v klonu (kot Worktree.harnessEnv): nastavitve iz okolja preglasijo
// .git/config klona (fsmonitor, hooki, atributi), GIT_ATTR_SOURCE = prazno drevo izklopi .gitattributes, GIT_DIR in
// GIT_WORK_TREE: ugnezden .git agenta ne velja; Maven dobi svoj lokalni repozitorij. tmp: TMPDIR agenta.
func worktreeHarnessEnv(wt, maven, tmp string) []string {
	return []string{"GIT_CONFIG_COUNT=5", "GIT_CONFIG_KEY_0=core.fsmonitor", "GIT_CONFIG_VALUE_0=false",
		"GIT_CONFIG_KEY_1=core.hooksPath", "GIT_CONFIG_VALUE_1=" + os.DevNull, // ne v klonu: tja agent lahko piše
		"GIT_CONFIG_KEY_2=core.attributesFile", "GIT_CONFIG_VALUE_2=" + os.DevNull,
		// N9: status in diff ne vstopita v podmodul (submodule.<ime>.ignore iz .gitmodules to preglasi: gitExecWrap)
		"GIT_CONFIG_KEY_3=diff.ignoreSubmodules", "GIT_CONFIG_VALUE_3=all", "GIT_CONFIG_KEY_4=submodule.recurse", "GIT_CONFIG_VALUE_4=false",
		"GIT_ATTR_SOURCE=4b825dc642cb6eb9a060e54bf8d69288fbee4904",
		"GIT_DIR=" + filepath.Join(wt, ".git"), "GIT_WORK_TREE=" + wt, "GIT_COMMON_DIR=" + filepath.Join(wt, ".git"),
		"MAVEN_OPTS=-Dmaven.repo.local=" + maven + " -Djava.io.tmpdir=" + tmp, "TMPDIR=" + tmp, "TMP=" + tmp, "TEMP=" + tmp}
}

// gitExecWrap (N9): GIT_EXEC_PATH za Claude CLI v klonu. Git zažene vgnezden git (stanje podmodula: git status v
// ugnezdenem repozitoriju z GIT_DIR=.git, kjer so config in atributi agentovi, torej njegov filter) z »git« iz mape
// exec, zato je tam ovoj: pusti le git, katerega GIT_DIR je točno eden od registriranih .git klonov (MINIGW_GIT_DIRS,
// ločeni z novo vrstico, ki jih agent v okolju claude ne more spremeniti), drugo zavrne. Ostale datoteke so povezave na pravo mapo exec (git-upload-pack …). dir ni pisljiv za
// agente (Scratch); "" = ni mogoče (runCLI tedaj zavrne).
func gitExecWrap(dir string) string {
	gitWrapMu.Lock()
	defer gitWrapMu.Unlock()
	exe, err := gitExe()
	if err != nil {
		return ""
	}
	out, err := command(exe, "--exec-path").Output()
	real := strings.TrimSpace(string(out))
	if err != nil || !filepath.IsAbs(real) {
		return ""
	}
	script := wrapScript(filepath.Join(real, "git"))
	if b, err := os.ReadFile(filepath.Join(dir, "git")); err == nil && string(b) == script {
		return dir
	}
	os.RemoveAll(dir)
	ents, err := os.ReadDir(real)
	if err != nil || os.MkdirAll(dir, 0o700) != nil {
		return ""
	}
	for _, e := range ents {
		if e.Name() != "git" && os.Symlink(filepath.Join(real, e.Name()), filepath.Join(dir, e.Name())) != nil {
			return ""
		}
	}
	if os.WriteFile(filepath.Join(dir, "git"), []byte(script), 0o700) != nil {
		return ""
	}
	return dir
}

var gitWrapMu sync.Mutex

// wrapScript: ovoj gitExecWrap (enak v Worktree.gitExecWrap na Macu): točno ujemanje GIT_DIR z enim od nepraznih vnosov.
func wrapScript(git string) string {
	return "#!/bin/sh\n# minigw (N9): git runs only in the clones minigw registered, not in a nested repository the agent made\n" +
		"ok=; set -f; IFS='\n'\n" +
		`for d in $MINIGW_GIT_DIRS; do [ -n "$GIT_DIR" ] && [ "$GIT_DIR" = "$d" ] && ok=1; done` + "\n" +
		`[ -n "$ok" ] || { echo "minigw: git in a nested repository is off in worktree mode" >&2; exit 128; }` + "\n" +
		"exec '" + strings.ReplaceAll(git, "'", `'\''`) + `' "$@"` + "\n"
}

// worktreeClaudeEnv: okolje procesa Claude CLI v klonu: worktreeHarnessEnv, drugje kot na Windows še ovoj gitExecWrap
// v mapi gw (N9; na Windows teče Claude v worktree le z »Allow commands without a sandbox«). gits: .git vseh klonov
// (vrhnji prvi, hub.worktreeGits). nil = ovoja ni.
func worktreeClaudeEnv(wt, maven, tmp, gw string, gits []string) []string {
	env := worktreeHarnessEnv(wt, maven, tmp)
	if runtime.GOOS == "windows" {
		return env
	}
	w := gitWrapEnv(gits, gw)
	if w == nil {
		return nil
	}
	return append(env, w...)
}

// gitWrapEnv: GIT_EXEC_PATH z ovojem gitExecWrap v gw in MINIGW_GIT_DIRS (edini GIT_DIR, ki jih ovoj pusti; absolutne
// poti brez nove vrstice); na Windows prazno (ne nil), nil = ovoja ni.
func gitWrapEnv(gitDirs []string, gw string) []string {
	if runtime.GOOS == "windows" {
		return []string{}
	}
	for _, d := range gitDirs {
		if !filepath.IsAbs(d) || strings.ContainsAny(d, "\n\r") {
			return nil
		}
	}
	if len(gitDirs) == 0 || gitExecWrap(gw) == "" {
		return nil
	}
	return []string{"GIT_EXEC_PATH=" + gw, "MINIGW_GIT_DIRS=" + strings.Join(gitDirs, "\n")}
}

// worktreeNote: navodilo agentu v klonu.
// Besedilo kot ClaudeCLI.mountNote na Macu, brez stavka o minigw run (ta je le na Macu).
// gits: .git vseh klonov (hub.worktreeGits; ugnezdeni za prvim) — agent jih dobi poimenovane z ukazom za commit v njih.
func worktreeNote(clientCwd, dir, branch string, gits []string) string {
	return "The user's folder " + clientCwd + " (on their computer, a git repository) is a clone here at " + dir + ", your working" +
		" directory, on branch " + branch + ". Map paths under " + clientCwd + " to it. Commit your work on this branch; changes you leave" +
		" uncommitted are committed for you at the end. The branch goes back to the user's repository; their files are not changed." +
		" Build output (.build, target, DerivedData) is never committed." + nestedClonesNote(gits) +
		" Do not create git repositories or submodules here outside ignored folders: then nothing comes back (use sync for that)."
}

// nestedClonesNote (B): ugnezdeni kloni po imenu in kako commitati v njih (git sam dela v vrhnjem: GIT_DIR je nastavljen).
func nestedClonesNote(gits []string) string {
	if len(gits) < 2 {
		return ""
	}
	var dirs []string
	for _, g := range gits[1:] {
		dirs = append(dirs, filepath.Dir(g))
	}
	return " These nested repositories and submodules are their own clones on the same branch, and their changes come back too: " +
		strings.Join(dirs, ", ") + ". Plain git here works on the top repository; to commit in a nested one run" +
		" GIT_DIR=<path>/.git GIT_COMMON_DIR=<path>/.git GIT_WORK_TREE=<path> git … with its path. Changes you leave uncommitted there are committed for you."
}

// worktreeReadOnlyNote: agent brez pisanja v klonu (kot ClaudeCLI.mountNote z branch in readOnly na Macu).
func worktreeReadOnlyNote(clientCwd, dir string) string {
	return "The user's folder " + clientCwd + " (on their computer, a git repository) is a clone here at " + dir + ", your working" +
		" directory, read-only for you: do not try to create, change or delete files in it. Map paths under " + clientCwd + " to it." +
		" Nothing goes back to the user, so give your answer in your reply."
}

// worktreeClaudeArgs: v peskovniku Claude Code (ne Windows) še pisanje v Maven agentov in branje objektov zrcal; .git
// vseh klonov (gits: config, hooks, info, commondir) ni pisljiv, Edit/Write vanj ne (nastavitve in hooke bere git
// samega claude). objects: objekti zrcal (vrhnji in ugnezdeni). tmp: TMPDIR teka (runTempDir).
func worktreeClaudeArgs(args []string, wt, maven, tmp string, objects, gits []string) []string {
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
				fs["allowWrite"] = append(toStrings(fs["allowWrite"]), wt, maven, tmp) // koren klona (tudi .git za commit), ko je cwd podmapa
				fs["allowRead"] = append(append(toStrings(fs["allowRead"]), wt, maven, tmp), objects...)
				var deny []string
				for _, g := range gits {
					for _, d := range []string{"config", "hooks", "info", "commondir"} {
						deny = append(deny, filepath.Join(g, d))
					}
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

// worktreeMount: klon seje in agenta (nov iz posnetka odjemalca); "" ob napaki; refused: zakaj prehod ne začne (nestedBlocker). Kliče se pod zaklepom mape (lock).
func (h *shareHub) worktreeMount(s *serveShare, agent string) (dir, refused string) {
	h.mu.Lock()
	t, ro := s.tree, s.readOnly
	slot := t.slots[agent]
	h.mu.Unlock()
	if ro {
		return "", ""
	}
	if slot != nil && slot.gitIntact() && nestedProblem(slot.wt, t.repo, slot.nested) == "" { // sicer nov klon (makeClone pobriše starega)
		return worktreeDir(slot.wt, t.sub), ""
	}
	short := sha256Hex([]byte(s.session))[:8]
	name := agent + "-" + short + "-0"
	branch, wt, base := "minigw/"+name, filepath.Join(t.worktrees, name), "refs/minigw/base/"+name
	if ensureMirror(t.repo) != nil {
		return "", ""
	}
	touchUsed(t.repo) // pruneMirrors: zadnja raba
	incoming := t.repo + ".incoming"
	clean := func() {
		for i := 0; i <= maxNestedRepos; i++ { // tudi ostanki prekinjenega zagona (ugnezdeni: <incoming>.<i>)
			os.Remove(incomingFile(incoming, i))
		}
	}
	clean()
	defer clean()
	h.mu.Lock()
	t.pushing = incoming
	h.mu.Unlock()
	// »nested«: vrhovi obstoječih zrcal ugnezdenih repozitorijev po ključu (nestedKey); prisotnost pove odjemalcu, da
	// prehod ugnezdene pozna (starejši prehod ga ne pošlje, odjemalec z ugnezdenimi tedaj odpove).
	have := map[string][]string{}
	if es, err := os.ReadDir(filepath.Dir(t.repo)); err == nil {
		prefix := strings.TrimSuffix(filepath.Base(t.repo), ".git") + "-"
		for _, e := range es {
			if k, ok := strings.CutSuffix(strings.TrimPrefix(e.Name(), prefix), ".git"); ok && len(k) == 16 && strings.HasPrefix(e.Name(), prefix) && e.IsDir() {
				have[k] = mirrorTips(filepath.Join(filepath.Dir(t.repo), e.Name()))
			}
		}
	}
	body, _ := json.Marshal(map[string]any{"have": mirrorTips(t.repo), "nested": have})
	r := h.sendJob(s, "WORKTREE-PUSH", body, 900*time.Second)
	h.mu.Lock()
	t.pushing = ""
	h.mu.Unlock()
	var reply struct {
		Snapshot string `json:"snapshot"`
		Nested   []struct {
			Path     string `json:"path"`
			Snapshot string `json:"snapshot"`
		} `json:"nested"`
	}
	if r == nil || r.Status != 200 || json.Unmarshal(r.Body, &reply) != nil || !shaRe.MatchString(reply.Snapshot) || len(reply.Nested) > maxNestedRepos {
		return "", ""
	}
	// N1 (lastnik): kjer agent ne bi tekel v svojem imenskem prostoru PID ali git ob finalize ne v bwrap, se z ugnezdenimi
	// repozitoriji ne začne (nič se ne izgubi); po teku ostane še zavrnitev v finalizeNested.
	if len(reply.Nested) > 0 && h.nestedBlocker != nil {
		if why := h.nestedBlocker(agent); why != "" {
			h.logf("worktree %s (%s): %s", branch, s.session, why)
			return "", why // končna napaka (400) z razlogom, brez opombe: sicer dvakrat
		}
	}
	file := ""
	if _, err := os.Stat(incoming); err == nil {
		file = incoming
	}
	if !importBase(t.repo, file, reply.Snapshot, base) || !makeClone(t.repo, wt, branch, reply.Snapshot) {
		return "", ""
	}
	// Ugnezdeni: vsak v svoje zrcalo (<repo>-<ključ>.git) in klon na svoji poti v klonu (H1: pot je preverjena, vsak
	// obstoječ del prava mapa, cilj prazen); starš pred otrokom.
	var nested []*wtNested
	seen := map[string]bool{}
	for i, n := range reply.Nested {
		in := incomingFile(incoming, i+1)
		if !validNestedPath(n.Path) || seen[n.Path] || !shaRe.MatchString(n.Snapshot) {
			h.logf("worktree %s (%s): bad nested repository from the client", branch, s.session)
			return "", ""
		}
		seen[n.Path] = true
		mirror := nestedMirror(t.repo, n.Path)
		if ensureMirror(mirror) != nil {
			return "", ""
		}
		touchUsed(mirror)
		file := ""
		if _, err := os.Stat(in); err == nil {
			file = in
		}
		if !importBase(mirror, file, n.Snapshot, base) {
			return "", ""
		}
		nested = append(nested, &wtNested{path: n.Path, repo: mirror, base: n.Snapshot})
	}
	slices.SortStableFunc(nested, func(a, b *wtNested) int { return strings.Count(a.path, "/") - strings.Count(b.path, "/") })
	for _, n := range nested {
		if !cloneTarget(wt, n.path) || !makeClone(n.repo, n.dir(wt), branch, n.base) {
			h.logf("worktree %s (%s): cannot clone the nested repository %s", branch, s.session, n.path)
			return "", ""
		}
		n.git, _ = os.Lstat(filepath.Join(n.dir(wt), ".git"))
		n.cfg, _ = os.ReadFile(filepath.Join(n.dir(wt), ".git", "config"))
		// Cilj 1: nesleden ugnezdeni (v drevesu starša ni gitlink) v info/exclude klona starša, preden agent teče (info je
		// zanj denyWrite): git add -A agenta v staršu ga ne zapiše kot gitlink.
		prepo, psnap, pdir, rel := t.repo, reply.Snapshot, wt, n.path
		if q := nestedParent(nested, n.path); q != nil {
			prepo, psnap, pdir, rel = q.repo, q.base, q.dir(wt), strings.TrimPrefix(n.path, q.path+"/")
		}
		if l, _ := gitRun(prepo, "", nil, "--git-dir", prepo, "ls-tree", psnap, "--", rel); !strings.HasPrefix(l, "160000 ") {
			f, err := os.OpenFile(filepath.Join(pdir, ".git", "info", "exclude"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
			if err != nil {
				return "", ""
			}
			f.WriteString("\n/" + excludeEscape(rel) + "\n")
			f.Close()
		}
	}
	for _, d := range []string{filepath.Join(wt, ".build", "tmp"), filepath.Join(t.worktrees, ".maven")} {
		os.MkdirAll(d, 0o700)
	}
	sl := &wtSlot{wt: wt, branch: branch, base: reply.Snapshot, nested: nested}
	sl.git, _ = os.Lstat(filepath.Join(wt, ".git"))
	sl.cfg, _ = os.ReadFile(filepath.Join(wt, ".git", "config"))
	if !sl.gitIntact() {
		return "", ""
	}
	h.mu.Lock()
	t.slots[agent] = sl
	h.mu.Unlock()
	return worktreeDir(wt, t.sub), ""
}

// nestedBlocker (N1): zakaj agent z ugnezdenimi repozitoriji na tem prehodu ne sme začeti ("" = sme). Linux: git ob
// finalize v bwrap in agent v svojem imenskem prostoru PID (Claude: pin z gnezdenim bwrap; Codex, Grok, Copilot: bwrap).
// macOS minigw serve (za razvoj): nikoli. Windows: agenti v načinu worktree že tečejo brez peskovnika.
func (s *server) nestedBlocker(agent string) string {
	const tail = "; use sync"
	switch {
	case !gitAttrSource(): // #55: starejši git GIT_ATTR_SOURCE tiho prezre (filtri .gitattributes ugnezdenih bi tekli)
		return "nested git repositories need git 2.40 or later on the gateway; update git or use sync"
	case runtime.GOOS == "darwin":
		return "nested repositories need the Mac app or a Linux gateway with bwrap" + tail
	case runtime.GOOS != "linux":
		return ""
	case finalizeBwrap() == "":
		return "nested repositories need bwrap on this Linux gateway (install bubblewrap and allow unprivileged user namespaces)" + tail
	case agent == "claude" && !claudePinOK(s.cfgDir, s.log.Printf):
		return "nested repositories need bwrap inside bwrap for Claude CLI on this Linux gateway; on Ubuntu 24.04 the admin allows it with an AppArmor profile that gives /usr/bin/bwrap userns, or sysctl kernel.apparmor_restrict_unprivileged_userns=0 (docs/usage.md, gateway requirements for nested repositories)" + tail
	case agent != "claude" && !agentSandboxed(s.cfgDir):
		return "nested repositories need bwrap for " + agent + " on this Linux gateway" + tail
	}
	return ""
}

// beforeNestedFinalize, duringFinalize, afterNestedFinalize: le za teste (N1: zamenjava poti pred, med in po git).
var beforeNestedFinalize, afterNestedFinalize func(n *wtNested)
var duringFinalize func()

// finalizeNested (N1): finalize ugnezdenega klona. Mapa klona se odpre prek os.Root iz pravega korena klona prehoda (brez
// poti z deli, ki jih agent lahko zamenja; povezava ven je zavrnjena) in je res ta klon (.git isti inode). Na Linux git
// bere drevo le iz te odprte mape (bwrap, --ro-bind-fd), zato poznejša zamenjava poti s povezavo ne preusmeri ničesar;
// brez bwrap ali če je agent tekel brez lastnega imenskega prostora PID (unconfined) ugnezdeni ne gre nazaj. Drugje
// (Windows: agenti brez peskovnika; macOS serve za razvoj) pričakovana pot. Po git še preverba poti; ob neujemanju
// rezultat in ref zrcala nazaj. Vrne vrh ali razlog.
func finalizeNested(slot *wtSlot, n *wtNested, agent, out string, links map[string]string) (string, string) {
	moved := "the nested repository " + n.path + " was moved or replaced in the agent's clone"
	if runtime.GOOS == "linux" && (finalizeBwrap() == "" || slot.unconfined) {
		return "", "the nested repository " + n.path + " does not come back on this gateway: that needs bwrap with its own PID namespace for the agent; use sync"
	}
	rr, err := os.OpenRoot(realPath(slot.wt))
	if err != nil {
		return "", moved
	}
	defer rr.Close()
	nr, err := rr.OpenRoot(filepath.FromSlash(n.path))
	if err != nil {
		return "", moved
	}
	defer nr.Close()
	if st, err := nr.Lstat(".git"); err != nil || n.git == nil || !os.SameFile(st, n.git) || !nestedIntact(slot.wt, n) {
		return "", moved
	}
	prev, _ := gitRun(n.repo, "", nil, "--git-dir", n.repo, "rev-parse", "-q", "--verify", "refs/heads/"+slot.branch+"^{commit}")
	if beforeNestedFinalize != nil {
		beforeNestedFinalize(n)
	}
	var tip string
	if runtime.GOOS == "linux" {
		tip = finalize(n.repo, "", slot.branch, n.base, agent, out, links, nr)
	} else {
		tip = finalize(n.repo, filepath.Join(realPath(slot.wt), filepath.FromSlash(n.path)), slot.branch, n.base, agent, out, links, nil)
	}
	if afterNestedFinalize != nil {
		afterNestedFinalize(n)
	}
	if !nestedIntact(slot.wt, n) {
		if shaRe.MatchString(prev) {
			gitRun(n.repo, "", nil, "--git-dir", n.repo, "update-ref", "refs/heads/"+slot.branch, prev)
		} else {
			gitRun(n.repo, "", nil, "--git-dir", n.repo, "update-ref", "-d", "refs/heads/"+slot.branch)
		}
		os.Remove(out)
		return "", moved
	}
	if tip == "" {
		return "", "the gateway could not save the agent's work in the nested repository " + n.path + " as a branch (see the gateway log)"
	}
	return tip, ""
}

// nestedParent: najbližji ugnezdeni repozitorij, ki vsebuje p (nil = vrhnji).
func nestedParent(nested []*wtNested, p string) *wtNested {
	var best *wtNested
	for _, n := range nested {
		if strings.HasPrefix(p, n.path+"/") && (best == nil || len(n.path) > len(best.path)) {
			best = n
		}
	}
	return best
}

// excludeEscape: pot kot dobesedni vzorec .gitignore (\ pred *, ?, [, \ in presledkom na koncu).
func excludeEscape(p string) string {
	var b strings.Builder
	for i, r := range p {
		if strings.ContainsRune(`*?[\`, r) || r == ' ' && i == len(p)-1 {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// incomingFile: bundle i-tega repozitorija med WORKTREE-PUSH (0 = vrhnji).
func incomingFile(incoming string, i int) string {
	if i == 0 {
		return incoming
	}
	return incoming + "." + strconv.Itoa(i)
}

// nestedMirror: golo zrcalo ugnezdenega repozitorija na poti p ob zrcalu repo (scratch/repos/<naprava>/<id>-<ključ>.git;
// pruneMirrors in gitExecDir ga obravnavata kot vsako zrcalo).
func nestedMirror(repo, p string) string {
	return strings.TrimSuffix(repo, ".git") + "-" + nestedKey(p) + ".git"
}

// cloneTarget (H1): klon ugnezdenega repozitorija gre v wt/p: vsak obstoječ del poti je prava mapa (ne povezava ali spoj,
// npr. sledena povezava a -> $HOME in pot a/x), cilj ne obstaja ali je prazna mapa (gitlink).
func cloneTarget(wt, p string) bool {
	d := wt
	parts := strings.Split(p, "/")
	for i, c := range parts {
		d = filepath.Join(d, c)
		st, err := os.Lstat(d)
		if os.IsNotExist(err) {
			return true
		}
		if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return false
		}
		if i == len(parts)-1 {
			es, err := os.ReadDir(d)
			return err == nil && len(es) == 0
		}
	}
	return false
}

// worktreeDir: podmapa sub v klonu; vsak del poti prava mapa (H1: povezava ali spoj kjerkoli v poti bi delovno mapo
// agenta prestavil iz klona, npr. a -> $HOME in cwd a/.ssh), sicer koren klona.
func worktreeDir(wt, sub string) string {
	d := wt
	for _, c := range strings.Split(filepath.ToSlash(sub), "/") {
		if c == "" || c == "." {
			continue
		}
		if d = filepath.Join(d, c); c == ".." || !realDir(d) {
			return wt // podmapa, ki je git ne pozna (prazna), ali pot ven
		}
	}
	return d
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

// worktreeGits: .git vseh klonov v wt (vrhnji prvi, nato ugnezdeni) in objekti zrcal (vrhnji prvi): za ovoj git,
// denyWrite, pin in nadzor premikov (tudi klicateljev klon pri vgnezdenem agentu).
func (h *shareHub) worktreeGits(wt string) (gits, objects []string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, s := range h.bySession {
		if s.tree == nil {
			continue
		}
		for _, sl := range s.tree.slots {
			if sl.wt == wt {
				objects = []string{filepath.Join(s.tree.repo, "objects")}
				for _, n := range sl.nested {
					objects = append(objects, filepath.Join(n.repo, "objects"))
				}
				return sl.gitDirs(), objects
			}
		}
	}
	return []string{filepath.Join(wt, ".git")}, nil
}

// markUnconfined (N1, Linux): agent v klonu wt je tekel brez svojega imenskega prostora PID; ugnezdeni ne gredo nazaj.
func (h *shareHub) markUnconfined(wt string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, s := range h.bySession {
		if s.tree != nil {
			for _, sl := range s.tree.slots {
				if sl.wt == wt {
					sl.unconfined = true
				}
			}
		}
	}
}

// worktreeIntact (H2): klon wt (tudi klicateljev pri vgnezdenem agentu) ima še svoj .git (wtSlot.gitIntact).
func (h *shareHub) worktreeIntact(wt string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, s := range h.bySession {
		if s.tree == nil {
			continue
		}
		for _, sl := range s.tree.slots {
			if sl.wt == wt {
				return sl.gitIntact()
			}
		}
	}
	return false
}

// worktreeBack: po agentu veje v zrcalih (ugnezdeni najprej, globlji pred plitvejšimi: starš dobi novi gitlink) in
// prevzem pri odjemalcu (WORKTREE-PULL: veja, nestedOnly, spremenjeni ugnezdeni).
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
	why, tip := cloneProblem(slot.wt, t.repo, slot.nested), ""
	type changed struct {
		Path string `json:"path"`
		Tip  string `json:"tip"`
	}
	var nested []changed
	results := []string{out}
	links := map[string]map[string]string{"": {}} // repozitorij (pot, "" = vrhnji) → neposredni otrok → novi vrh
	for _, n := range slot.nested {
		links[n.path] = map[string]string{}
	}
	parent := func(p string) (string, string) {
		if q := nestedParent(slot.nested, p); q != nil {
			return q.path, strings.TrimPrefix(p, q.path+"/")
		}
		return "", p
	}
	for i := len(slot.nested) - 1; i >= 0 && why == ""; i-- { // obratno: globlji pred plitvejšimi
		n := slot.nested[i]
		o := fmt.Sprintf("%s.result.%d", t.repo, i+1)
		defer os.Remove(o)
		nt := ""
		if nt, why = finalizeNested(slot, n, agent, o, links[n.path]); why != "" {
			break
		}
		p, rel := parent(n.path)
		switch {
		case nt == n.base:
			links[p][rel] = ""
		default:
			links[p][rel] = nt
			if st, err := os.Stat(o); err != nil || st.Size() > maxSyncFile {
				why = "the agent's branch in the nested repository " + n.path + " is larger than 64 MB; it stays on the gateway's mirror (" + slot.branch + ")"
			}
			nested = append(nested, changed{n.path, nt})
			results = append(results, o)
		}
	}
	if why == "" {
		if tip = finalize(t.repo, slot.wt, slot.branch, slot.base, agent, out, links[""], nil); tip == slot.base && len(nested) == 0 {
			return // agent ni spremenil ničesar
		} else if tip == "" {
			why = "the gateway could not save the agent's work as a branch (see the gateway log)"
		} else if st, err := os.Stat(out); tip != slot.base && (err != nil || st.Size() > maxSyncFile) {
			why = "the agent's branch is larger than 64 MB; it stays on the gateway's mirror (" + slot.branch + ")"
		}
	}
	msg := map[string]any{"branch": slot.branch, "tip": tip}
	if why != "" {
		h.logf("worktree %s (%s): %s", slot.branch, s.session, why)
		results, msg["tip"] = []string{""}, ""
	} else {
		msg["nestedOnly"], msg["nested"] = tip == slot.base, nested
	}
	h.mu.Lock()
	t.results, t.resultErr = results, why
	h.mu.Unlock()
	body, _ := json.Marshal(msg)
	h.sendJob(s, "WORKTREE-PULL", body, 300*time.Second)
	h.mu.Lock()
	t.results, t.resultErr = nil, ""
	h.mu.Unlock()
}

// worktreePut: del bundla odjemalca med WORKTREE-PUSH (del 0 začne datoteko; nested > 0: i-ti ugnezdeni). Status HTTP.
func (h *shareHub) worktreePut(key string, part, nested int, data []byte, owner string) int {
	h.mu.Lock()
	s := h.share(key, owner)
	file := ""
	if s != nil && s.tree != nil {
		s.lastSeen = time.Now()
		file = s.tree.pushing
	}
	h.mu.Unlock()
	if file == "" || part < 0 || nested < 0 || nested > maxNestedRepos {
		return 409
	}
	base := file // I2: vrednost, prebrana pod h.mu
	file = incomingFile(file, nested)
	flags := os.O_WRONLY | os.O_APPEND
	if part == 0 {
		flags |= os.O_CREATE | os.O_TRUNC
	}
	f, err := os.OpenFile(file, flags, 0o600)
	if err != nil {
		return 409
	}
	defer f.Close()
	total := int64(0) // N4: vsi bundli enega WORKTREE-PUSH skupaj največ maxIncoming
	for i := 0; i <= maxNestedRepos; i++ {
		if st, err := os.Stat(incomingFile(base, i)); err == nil {
			total += st.Size()
		}
	}
	if st, _ := f.Stat(); st == nil || total+int64(len(data)) > maxIncoming {
		os.Remove(file)
		return 413
	}
	if _, err := f.Write(data); err != nil {
		return 500
	}
	return 204
}

// worktreeResult: bundle veje za odjemalca med WORKTREE-PULL (nested > 0: i-ti ugnezdeni iz sporočila).
func (h *shareHub) worktreeResult(key, owner string, nested int) ([]byte, int, string) {
	h.mu.Lock()
	s := h.share(key, owner)
	file, why := "", ""
	if s != nil && s.tree != nil {
		s.lastSeen = time.Now()
		if nested >= 0 && nested < len(s.tree.results) {
			file = s.tree.results[nested]
		}
		if why = s.tree.resultErr; nested != 0 {
			why = ""
		}
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
		go removeClone(sl.wt)
	}
	t.slots = map[string]*wtSlot{}
}

// removeClone: klon, nato še prazen .maven in mapa klonov, ko v njej ni drugega klona (sicer ostane prazna mapa;
// .maven z vsebino je predpomnilnik Maven in ostane do odstranitve zrcala).
func removeClone(wt string) {
	os.RemoveAll(wt)
	dir := filepath.Dir(wt)
	if es, _ := os.ReadDir(dir); len(es) == 1 && es[0].Name() == ".maven" && os.Remove(filepath.Join(dir, ".maven")) == nil {
		os.Remove(dir)
	}
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

// note: sporočilo uporabniku seje prek minigw (WORKTREE-NOTE na stderr); starejši minigw ga zavrne.
func (h *shareHub) note(session, msg string) {
	h.mu.Lock()
	s := h.bySession[session]
	h.mu.Unlock()
	if s != nil {
		body, _ := json.Marshal(map[string]string{"message": msg})
		go h.sendJob(s, "WORKTREE-NOTE", body, 10*time.Second)
	}
}

// isWorktree: seja dela v načinu worktree (pred mount: Grok in agenti brez pisanja ga zavrnejo).
func (h *shareHub) isWorktree(session string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	s := h.bySession[session]
	return s != nil && s.tree != nil
}

// maxRunTemp: najdaljša pot runTempDir. Claude Code na Linux v TMPDIR odpre vtičnice peskovnika (pot vtičnice največ
// 108 bajtov; TMPDIR z 80 znaki že odpove), zato ne mapa v klonu.
const maxRunTemp = 40

// tempBase: začasna mapa sistema za korene runTempDir ali /tmp, če je ta predolga (Windows: vedno svoja).
func tempBase() string {
	if base := os.TempDir(); runtime.GOOS == "windows" || len(base) <= maxRunTemp-24 {
		return base
	}
	return "/tmp"
}

// shortTempRoot: nov koren prehoda za runTempDir (0700, naključno ime mgw-*); "" = ni bilo mogoče.
func shortTempRoot() string {
	d, err := os.MkdirTemp(tempBase(), "mgw-")
	if err != nil {
		return ""
	}
	if r, err := filepath.EvalSymlinks(d); err == nil { // macOS: /tmp → /private/tmp (pravila peskovnika)
		d = r
	}
	return d
}

// ownTempRoot: p je mapa (ne povezava) tega uporabnika z dovoljenji 0700 (Windows: le mapa).
func ownTempRoot(p string) bool {
	fi, err := os.Lstat(p)
	if err != nil || !fi.IsDir() {
		return false
	}
	return runtime.GOOS == "windows" || fi.Mode().Perm() == 0o700 && ownedByMe(p)
}

// tempRoot (M1): koren prehoda za TMPDIR tekov; vsi agenti Claude ga imajo v denyRead (agentArgs). Čistilec /tmp
// (macOS po 3 dneh, systemd-tmpfiles po 10) ga lahko pobriše, drug uporabnik pa nato podtakne mapo z istim imenom:
// zato se pred vsakim tekom preveri in po potrebi naredi nov; ob vsakem teku se dotakne (mtime). "" = ga ni.
func (s *server) tempRoot() string {
	s.tmpMu.Lock()
	defer s.tmpMu.Unlock()
	if s.tmpRoot == "" || !ownTempRoot(s.tmpRoot) {
		if s.tmpRoot = shortTempRoot(); s.tmpRoot != "" {
			s.tmpRoots = append(s.tmpRoots, s.tmpRoot)
		}
	}
	if s.tmpRoot != "" {
		now := time.Now()
		os.Chtimes(s.tmpRoot, now, now)
	}
	return s.tmpRoot
}

// tempRoots (#56): obstoječi koreni tega procesa; starejši imajo lahko še TMPDIR živih tekov.
func (s *server) tempRoots() []string {
	s.tmpMu.Lock()
	defer s.tmpMu.Unlock()
	var out []string
	for _, r := range s.tmpRoots {
		if _, err := os.Lstat(r); err == nil {
			out = append(out, r)
		}
	}
	return out
}

// staleTempRoot: koren, ki ga ob zagonu pobrišemo (prehod istega uporabnika ga ni rabil dan; živ prehod si ga naredi znova).
const staleTempRoot = 24 * time.Hour

// cleanStaleTempRoots (L2): ob zagonu pobriše stare korene mgw-* v base, ki so mape tega uporabnika z 0700 (povezav ne
// sledi, tujih in novejših ne pobriše).
func cleanStaleTempRoots(base string) {
	ents, _ := os.ReadDir(base)
	for _, e := range ents {
		p := filepath.Join(base, e.Name())
		if fi, err := os.Lstat(p); err == nil && strings.HasPrefix(e.Name(), "mgw-") && ownTempRoot(p) && time.Since(fi.ModTime()) > staleTempRoot {
			os.RemoveAll(p)
		}
	}
}

// dropTempRoot (L2): ob koncu prehoda pobriše koren.
func (s *server) dropTempRoot() {
	s.tmpMu.Lock()
	defer s.tmpMu.Unlock()
	if s.tmpRoot != "" && ownTempRoot(s.tmpRoot) {
		os.RemoveAll(s.tmpRoot)
	}
	s.tmpRoot = ""
}

// runTempDir: TMPDIR enega teka Claude CLI v klonu (0700, naključno ime pod root iz tempRoot); done jo pobriše
// (RemoveAll ne sledi povezavam). Brez korena napaka (L1: ne v skupni začasni mapi).
func runTempDir(root string) (dir string, done func(), err error) {
	if root == "" {
		return "", nil, errors.New("no gateway temporary folder")
	}
	if dir, err = os.MkdirTemp(root, ""); err != nil {
		return "", nil, err
	}
	if !ownTempRoot(root) { // koren se je vmes zamenjal (čistilec + tuja mapa): ne uporabi
		os.RemoveAll(dir)
		return "", nil, errors.New("gateway temporary folder changed")
	}
	return dir, func() { os.RemoveAll(dir) }, nil
}
