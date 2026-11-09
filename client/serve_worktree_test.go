package main

import (
	"crypto/ecdh"
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Worktree na minigw serve: posnetek odjemalca (tudi neshranjeno) → klon agenta → veja nazaj; uporabnikove datoteke,
// indeks in veje ostanejo; nastavitve, hooki in filtri agenta ne tečejo ne na prehodu ne pri odjemalcu.
func TestServeWorktreeRoundTrip(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake claude is a shell script")
	}
	root := t.TempDir()
	client, marks := filepath.Join(root, "client"), filepath.Join(root, "marks")
	os.MkdirAll(filepath.Join(client, "sub"), 0o755)
	os.MkdirAll(marks, 0o755)
	gitT(t, client, "init", "-q", "-b", "main")
	writeT(t, filepath.Join(client, "a.txt"), "one\n")
	writeT(t, filepath.Join(client, "sub", "s.txt"), "s\n")
	gitT(t, client, "add", "-A")
	gitT(t, client, "commit", "-qm", "first")
	writeT(t, filepath.Join(client, "a.txt"), "one\nuncommitted\n")
	gitT(t, client, "config", "filter.evil.smudge", "touch "+marks+"/client-smudge; cat")
	for _, h := range []string{"reference-transaction", "post-checkout", "post-merge"} {
		writeT(t, filepath.Join(client, ".git/hooks", h), "#!/bin/sh\ntouch "+marks+"/hook-"+h+"\n")
		os.Chmod(filepath.Join(client, ".git/hooks", h), 0o755)
	}
	status, head := gitT(t, client, "status", "--porcelain"), gitT(t, client, "rev-parse", "HEAD")
	config, _ := os.ReadFile(filepath.Join(client, ".git/config"))

	// Agent: commit, neshranjena datoteka, .gitattributes s filtrom in podtaknjene nastavitve/hooki klona.
	fake := filepath.Join(root, "claude")
	writeT(t, fake, `#!/bin/sh
`+cliGuard+`
cat > /dev/null
[ -f ../a.txt ] && grep -q uncommitted ../a.txt && echo saw-uncommitted > ../seen.txt
n=$(ls ../agent*.txt 2>/dev/null | wc -l | tr -d ' ')
echo "agent $n" > ../agent$n.txt
git add ../agent$n.txt && git -c user.name=a -c user.email=a@x commit -qm "agent $n"
echo left > ../left.txt
echo '* filter=evil' > ../.gitattributes
# config le v zadnji zahtevi: spremenjen config (v peskovniku denyWrite) pomeni nov klon (H2), ne nadaljevanja
[ "$n" = 1 ] && printf '[core]\n\tfsmonitor = touch `+marks+`/gw-fsmonitor\n[filter "evil"]\n\tclean = touch `+marks+`/gw-clean; cat\n' >> "$GIT_DIR/config"
mkdir -p "$GIT_DIR/hooks"; printf '#!/bin/sh\ntouch `+marks+`/gw-hook\n' > "$GIT_DIR/hooks/post-commit"; chmod +x "$GIT_DIR/hooks/post-commit"
echo '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"done"}}}'
echo '{"type":"result","subtype":"success","usage":{"output_tokens":1}}'
`)
	os.Chmod(fake, 0o755)
	s, _ := testServer(t, func(c *serveConfig) { c.Claude = fake })
	c := startLoopback(t, s)

	cwd := filepath.Join(client, "sub")
	sc := &shareClient{c: c, session: "sid1", root: cwd, worktree: true, top: filepath.ToSlash(client), synced: map[string]syncEntry{}, reported: map[string]bool{}}
	if !sc.register() {
		t.Fatal("register failed: " + sc.refusal)
	}
	sc.start(2)
	defer sc.unregister()

	branch := "minigw/claude-" + sha256Hex([]byte("sid1"))[:8] + "-0"
	body := `{"model":"claude-cli-haiku","tools":[{"name":"x"}],"messages":[{"role":"user","content":"go"}]}`
	h := map[string]string{"x-claude-mini-session": "sid1", "x-claude-mini-cwd": cwd, "content-type": "application/json"}
	for i := range 2 { // druga zahteva iste seje nadaljuje na isti veji
		if i == 1 {
			body = `{"model":"claude-cli-haiku","tools":[{"name":"x"}],"messages":[{"role":"user","content":"go"},{"role":"assistant","content":"done"},{"role":"user","content":"again"}]}`
		}
		// N3b: brez pin (macOS, Linux brez gnezdenega bwrap) spremenjen config ustavi claude med tekom (nov klon velja enako)
		if st, data := call(c, "POST", "/v1/messages", h, []byte(body), 60*time.Second); (st != 200 || !strings.Contains(string(data), `"done"`)) &&
			(i == 0 || !strings.Contains(string(data), "replaced its clone's .git during the run")) {
			t.Fatalf("messages %d: %d %s", i, st, data)
		}
	}
	if m, _ := os.ReadDir(marks); len(m) != 0 {
		t.Fatalf("something ran: %v", m)
	}
	files := gitT(t, client, "ls-tree", "-r", "--name-only", branch)
	for _, f := range []string{"agent0.txt", "agent1.txt", "left.txt", ".gitattributes", "seen.txt", "sub/s.txt"} {
		if !strings.Contains("\n"+files+"\n", "\n"+f+"\n") {
			t.Fatalf("branch %s misses %s:\n%s", branch, f, files)
		}
	}
	if got := gitT(t, client, "show", branch+":a.txt"); got != "one\nuncommitted" {
		t.Fatalf("snapshot a.txt: %q", got)
	}
	if gitT(t, client, "merge-base", "--is-ancestor", head, branch) != "" || gitT(t, client, "status", "--porcelain") != status ||
		gitT(t, client, "rev-parse", "HEAD") != head || gitT(t, client, "rev-parse", "--abbrev-ref", "HEAD") != "main" {
		t.Fatal("user's repository changed")
	}
	if b, _ := os.ReadFile(filepath.Join(client, ".git/config")); string(b) != string(config) {
		t.Fatal("user's .git/config changed")
	}
	if _, err := os.Stat(filepath.Join(client, "agent0.txt")); err == nil {
		t.Fatal("agent file in the user's working copy")
	}
	// L3: na Windows Claude v načinu worktree le z »Allow commands without a sandbox«
	worktreeNeedsUnsandboxed = true
	defer func() { worktreeNeedsUnsandboxed = runtime.GOOS == "windows" }()
	if st, data := call(c, "POST", "/v1/messages", h, []byte(body), 30*time.Second); st != 400 || !strings.Contains(string(data), "Allow commands without a sandbox") {
		t.Fatalf("windows rule: %d %s", st, data)
	}
}

// N3b: brez pin .git klona runCLI med tekom preveri klon; zamenjan .git ustavi claude (ne šele naslednji zagon).
// N3c: na Linux tudi zamenjava, ki jo agent takoj vrne (inotify; preverba vsako sekundo bi jo zgrešila).
func TestServeWorktreeSwapStopsUnpinnedClaude(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake claude is a shell script")
	}
	swaps := []string{"mv .git .g && mkdir .git"}
	if runtime.GOOS == "linux" {
		swaps = append(swaps, "mv .git .g && mkdir .git && rmdir .git && mv .g .git")
	}
	for i, swap := range swaps {
		client := t.TempDir()
		gitT(t, client, "init", "-q", "-b", "main")
		writeT(t, filepath.Join(client, "a.txt"), "one\n")
		gitT(t, client, "add", "-A")
		gitT(t, client, "commit", "-qm", "first")
		fake := filepath.Join(t.TempDir(), "claude")
		writeT(t, fake, "#!/bin/sh\ncat > /dev/null\n"+swap+"\nexec sleep 20\n")
		os.Chmod(fake, 0o755)
		s, _ := testServer(t, func(c *serveConfig) { c.Claude = fake })
		if name, _ := pinWrap(s.cfgDir, "x", nil, nil, t.Logf); name != "x" {
			t.Skip("the .git pin is on here (nested bwrap): the swap itself fails")
		}
		c := startLoopback(t, s)
		sid := fmt.Sprintf("sid%d", i)
		sc := &shareClient{c: c, session: sid, root: client, worktree: true, top: filepath.ToSlash(client), synced: map[string]syncEntry{}, reported: map[string]bool{}}
		if !sc.register() {
			t.Fatal("register failed: " + sc.refusal)
		}
		sc.start(2)
		h := map[string]string{"x-claude-mini-session": sid, "x-claude-mini-cwd": client, "content-type": "application/json"}
		start := time.Now()
		st, data := call(c, "POST", "/v1/messages", h, []byte(`{"model":"claude-cli-haiku","tools":[{"name":"x"}],"messages":[{"role":"user","content":"go"}]}`), 30*time.Second)
		sc.unregister()
		if st != 502 || !strings.Contains(string(data), "replaced its clone's .git during the run") || time.Since(start) > 10*time.Second {
			t.Fatalf("%s: after %v: %d %s", swap, time.Since(start), st, data)
		}
	}
}

func TestWorktreeTreePaths(t *testing.T) {
	tr, err := worktreeTree("/s", "dev", `C:\proj`, `C:\proj\a\b`)
	if err != nil || tr.sub != "a/b" || !strings.HasSuffix(filepath.ToSlash(tr.repo), "/s/repos/dev/"+sha256Hex([]byte("C:/proj"))[:16]+".git") {
		t.Fatalf("windows paths: %+v %v", tr, err)
	}
	// odjemalec Go pošlje top z »/« (filepath.ToSlash), cwd z »\«: isto zrcalo
	if tr2, err := worktreeTree("/s", "dev", "C:/proj", `C:\proj\a\b`); err != nil || tr2.repo != tr.repo || tr2.sub != "a/b" {
		t.Fatalf("mixed windows paths: %+v %v", tr2, err)
	}
	for _, c := range [][2]string{{"/p", "/other"}, {"/p", "/p/../x"}, {"/p", "/p/.git/hooks"}, {"", "/p"}, {"/p", "/p2"}} {
		if _, err := worktreeTree("/s", "dev", c[0], c[1]); err == nil {
			t.Fatalf("accepted %v", c)
		}
	}
}

// Agentov commit, ki ga fsck zavrne (drevo z ».git«): brez rezultata in ref zrcala ne kaže na manjkajoče objekte
// (sicer bi vsak naslednji klon zrcala padel).
func TestWorktreeFinalizeRejectedKeepsMirror(t *testing.T) {
	root := t.TempDir()
	client, repo, wt := filepath.Join(root, "c"), filepath.Join(root, "repos", "d", "r.git"), filepath.Join(root, "wt")
	os.MkdirAll(client, 0o755)
	gitT(t, client, "init", "-q", "-b", "main")
	writeT(t, filepath.Join(client, "a.txt"), "a\n")
	gitT(t, client, "add", "a.txt")
	gitT(t, client, "commit", "-qm", "first")
	head := gitT(t, client, "rev-parse", "HEAD")
	b := filepath.Join(root, "push.bundle")
	gitUser(client, nil, "update-ref", "refs/minigw/push", head)
	gitUser(client, nil, "bundle", "create", b, "refs/minigw/push")
	if ensureMirror(repo) != nil || !importBase(repo, b, head, "refs/minigw/base/x") || !makeClone(repo, wt, "minigw/claude-12345678-0", head) {
		t.Fatal("mirror or clone")
	}
	blob := gitT(t, wt, "hash-object", "-w", "--stdin")
	cmd := func(in string, a ...string) string {
		c := exec.Command("git", append([]string{"-C", wt}, a...)...)
		c.Stdin = strings.NewReader(in)
		out, err := c.Output()
		if err != nil {
			t.Fatalf("git %v: %v", a, err)
		}
		return strings.TrimSpace(string(out))
	}
	e := cmd("100644 blob "+blob+"\thooks-post-checkout\n", "mktree")
	top := cmd("040000 tree "+e+"\t.git\n", "mktree")
	c := cmd("", "-c", "user.name=a", "-c", "user.email=a@x", "commit-tree", top, "-p", head, "-m", "dot git")
	cmd("", "update-ref", "HEAD", c)
	if tip := finalize(repo, wt, "minigw/claude-12345678-0", head, "claude", filepath.Join(root, "out.bundle"), nil); tip != "" {
		t.Fatalf("finalize accepted a .git tree: %s", tip)
	}
	os.RemoveAll(wt)
	if !makeClone(repo, filepath.Join(root, "wt2"), "minigw/claude-12345678-1", head) {
		t.Fatal("mirror broken after a rejected result")
	}
}

// Claude v klonu, ko je cwd podmapa: koren klona v peskovniku (branje, pisanje, --add-dir, pravila orodij), .git klona ne.
func TestWorktreeClaudeArgs(t *testing.T) {
	args := []string{"--allowedTools", "Read(./**)", "--disallowedTools", "Read(/cfg/**)", "--settings",
		`{"sandbox":{"filesystem":{"allowRead":["/w/sub"],"allowWrite":["/w/sub"],"denyRead":["~"]}}}`}
	got := strings.Join(worktreeClaudeArgs(args, "/w", "/m", []string{"/r/objects", "/r-k/objects"}, []string{"/w/.git", "/w/lib/.git"}), " ")
	for _, want := range []string{`"allowWrite":["/w/sub","/w","/m"]`, `"allowRead":["/w/sub","/w","/m","/r/objects","/r-k/objects"]`, "--add-dir /w",
		"Edit(//w/**)", `"denyWrite":["/w/.git/config"`, `"/w/lib/.git/config","/w/lib/.git/hooks","/w/lib/.git/info","/w/lib/.git/commondir"]`, "Edit(//w/.git/**)"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %s in\n%s", want, got)
		}
	}
}

// Codex v klonu (Linux: pod bwrap): commit in necommitano pride kot veja; .git/config klona je le za branje (protect).
func TestServeWorktreeCodexSandbox(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake codex is a shell script")
	}
	root := t.TempDir()
	client := filepath.Join(root, "client")
	os.MkdirAll(client, 0o755)
	gitT(t, client, "init", "-q", "-b", "main")
	writeT(t, filepath.Join(client, "a.txt"), "a\n")
	gitT(t, client, "add", "-A")
	gitT(t, client, "commit", "-qm", "first")
	codex := filepath.Join(root, "codex")
	writeT(t, codex, `#!/bin/sh
if [ "$1" = debug ]; then echo '{"models":[]}'; exit 0; fi
out=""; prev=""
for a in "$@"; do [ "$prev" = "-o" ] && out="$a"; prev="$a"; done
cat > /dev/null
echo codex > codex.txt; git add codex.txt && git -c user.name=c -c user.email=c@x commit -qm codex
if echo "[core]" >> "$GIT_DIR/config" 2>/dev/null; then echo written > cfgwrite.txt; else echo denied > cfgwrite.txt; fi
echo '{"type":"thread.started","thread_id":"t1"}'
echo "done" > "$out"
`)
	os.Chmod(codex, 0o755)
	s, _ := testServer(t, func(c *serveConfig) {
		c.AllowUnsandboxedBash = runtime.GOOS != "linux" || bwrapReady(t.TempDir()) == "" // brez bwrap le z izrecnim dovoljenjem
		t.Setenv("CODEX_HOME", t.TempDir())
		c.Codex = agentCLI{Enabled: true, Executable: codex}
	})
	c := startLoopback(t, s)
	sc := &shareClient{c: c, session: "sidx", root: client, worktree: true, top: filepath.ToSlash(client), synced: map[string]syncEntry{}, reported: map[string]bool{}}
	if !sc.register() {
		t.Fatal("register failed: " + sc.refusal)
	}
	sc.start(2)
	defer sc.unregister()
	h := map[string]string{"x-claude-mini-session": "sidx", "x-claude-mini-cwd": client, "content-type": "application/json"}
	body := `{"model":"claude-codex","tools":[{"name":"x"}],"messages":[{"role":"user","content":"go"}]}`
	if st, data := call(c, "POST", "/v1/messages", h, []byte(body), 60*time.Second); st != 200 || !strings.Contains(string(data), "done") {
		t.Fatalf("codex: %d %s", st, data)
	}
	branch := "minigw/codex-" + sha256Hex([]byte("sidx"))[:8] + "-0"
	if gitT(t, client, "show", branch+":codex.txt") != "codex" {
		t.Fatal("codex commit not on the branch")
	}
	got := gitT(t, client, "show", branch+":cfgwrite.txt")
	if runtime.GOOS == "linux" && bwrapReady(t.TempDir()) != "" && got != "denied" {
		t.Fatalf("the clone's .git/config was writable under bwrap: %s", got)
	}
	t.Logf("clone .git/config write: %s", got)
}

// H1 (1. plast): dovoljena mapa okoli Scratch ali v njem: ob zagonu ne velja (Claude in Codex izklopljena), PATCH 400.
func TestAllowedFolderAroundScratch(t *testing.T) {
	parent := t.TempDir()
	scratch := filepath.Join(parent, "scratch")
	os.MkdirAll(scratch, 0o700)
	k, _ := ecdh.X25519().GenerateKey(rand.Reader)
	c := serveConfig{Token: "tok", Key: base64url(k.Bytes()), Tools: defaultTools, PermissionMode: "dontAsk", Scratch: scratch,
		AllowedFolders: []string{parent}, Codex: agentCLI{Enabled: true}}
	cfg := filepath.Join(t.TempDir(), "serve.json")
	writeServeConfig(cfg, c) // PATCH spodaj piše v datoteko
	s, err := newServer(c, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.conf(); !got.ClaudeDisabled || got.Codex.Enabled || len(got.AllowedFolders) != 0 {
		t.Fatalf("startup with an allowed folder around Scratch: %+v", got)
	}
	if info := s.claudeInfo(); !strings.Contains(info.Off, parent) || !slices.Equal(info.LeftOut, []string{parent}) { // gw status, gw claude, stran
		t.Fatalf("no reason or left-out folder: %+v", info)
	}
	// Le vklop ne gre (serve.json ima še mapo okoli Scratch); stran shrani mape brez nje, nato stikalo vklopi brez
	// ponovnega zagona; vklop razlog počisti, kasnejši ročni izklop ga nima.
	for _, body := range []string{`{"enabled":true}`, `{"permissionMode":"acceptEdits"}`, `{"path":""}`} { // vse razen map
		if st, b := s.adminDo("PATCH", "/agents/claude", []byte(body)); st != 400 || s.claudeInfo().Off == "" || !strings.Contains(fmt.Sprint(b), "first save") {
			t.Fatalf("%s without saving the folders: %d %v", body, st, b)
		}
	}
	for _, body := range []string{`{"folders":""}`, `{"enabled":true}`, `{"enabled":false}`} {
		if st, b := s.adminDo("PATCH", "/agents/claude", []byte(body)); st != 200 {
			t.Fatalf("%s: %d %v", body, st, b)
		}
	}
	if off := s.claudeInfo().Off; off != "" {
		t.Fatalf("stale reason: %q", off)
	}
	inside := filepath.Join(scratch, "repos")
	os.MkdirAll(inside, 0o700)
	for _, f := range []string{parent, inside} {
		cc := s.conf()
		cc.AllowedFolders = []string{f}
		if _, err := checkAgent(&cc, s.cfgDir); err == nil || !strings.Contains(err.Error(), "scratch folder") {
			t.Fatalf("%s accepted: %v", f, err)
		}
	}
}

// L5: git prehoda ima rok; razlog brez rezultata gre odjemalcu (413 s sporočilom), ne tiho 404.
func TestWorktreeLimits(t *testing.T) {
	old := gitTimeout
	t.Cleanup(func() { gitTimeout = old })
	gitTimeout = time.Nanosecond
	if _, err := gitRun(filepath.Join(t.TempDir(), "repos", "d", "r.git"), "", nil, "--version"); err == nil {
		t.Fatal("git ran past its timeout")
	}
	if d := t.TempDir(); cloneProblem(d, filepath.Join(d, "repos", "d", "r.git"), nil) != "" {
		t.Fatal("empty clone too large")
	}
	h := newShareHub()
	key, _ := h.registerTree("s", "/p", "", &shareTree{repo: "/r", worktrees: t.TempDir(), slots: map[string]*wtSlot{}}, "dev")
	if _, st, _ := h.worktreeResult(key, "dev", 0); st != 404 {
		t.Fatalf("no result: %d", st)
	}
	h.mu.Lock()
	h.bySession["s"].tree.resultErr = "the agent's clone is too large"
	h.mu.Unlock()
	if _, st, why := h.worktreeResult(key, "dev", 0); st != 413 || !strings.Contains(why, "too large") {
		t.Fatalf("reason: %d %s", st, why)
	}
}

// Končni pregled L1: privzeti Scratch in dovoljena mapa okoli njega: PATCH general zavrne; --add-dir je ne dobi.
// L2: preimenovanje ponudnika v zasedeno ime brez razlike v velikih črkah (Bar → foo, ko obstaja Foo) je 400, ključ ostane.
func TestScratchOverlapAndProviderRename(t *testing.T) {
	s, _, base := liveServer(t, nil)
	host := strings.TrimPrefix(base, "http://")
	parent, defParent := filepath.Dir(s.conf().Scratch), filepath.Dir(defaultScratch())
	// mimo checkAgent: starejša nastavitev z mapama okoli testnega in privzetega Scratch
	s.update(func(c *serveConfig) error { c.AllowedFolders = []string{parent, defParent}; return nil })
	if st, b := adminNoise(t, s, base, s.admin, "PATCH", adminPrefix+"/general", `{"scratch":""}`); st != 400 || !strings.Contains(b, "contains it") {
		t.Fatalf("default scratch with an allowed folder around it: %d %s", st, b)
	}
	if strings.Contains(strings.Join(s.agentArgs(t.TempDir(), false), " "), "--add-dir "+parent) {
		t.Fatal("allowed folder around Scratch passed to claude")
	}
	s.update(func(c *serveConfig) error {
		c.Gateways = append(c.Gateways, serveGateway{Name: "Foo", BaseURL: "https://foo.example"}, serveGateway{Name: "Bar", BaseURL: "https://bar.example"})
		return nil
	})
	keySet(s.cfgDir, "Bar", "key-bar")
	t.Cleanup(func() { keyDel(s.cfgDir, "Bar"); keyDel(s.cfgDir, "foo") })
	if st, b := adminNoise(t, s, base, s.admin, "PATCH", adminPrefix+"/providers/Bar", `{"name":"foo"}`); st != 400 || !strings.Contains(b, "already exists") {
		t.Fatalf("rename to a taken name: %d %s", st, b)
	}
	if k, _ := keyGet(s.cfgDir, "foo"); k != "" {
		t.Fatal("key moved for a refused rename")
	}
	if _, ok := findGateway(s.conf(), "Bar"); !ok {
		t.Fatal("Bar renamed")
	}
	_ = host
}

// Poročilo Windows #17: po zadnjem klonu ne ostane prazna mapa .maven; z vsebino (predpomnilnik) ali ob drugem klonu ostane.
func TestRemoveCloneDropsEmptyMaven(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "id")
	a, b, maven := filepath.Join(dir, "a"), filepath.Join(dir, "b"), filepath.Join(dir, ".maven")
	for _, d := range []string{a, b, maven} {
		os.MkdirAll(d, 0o700)
	}
	removeClone(a)
	if _, err := os.Stat(maven); err != nil {
		t.Fatal(".maven removed while another clone is there")
	}
	removeClone(b)
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("empty clones folder stays: %v", err)
	}
}

// H1: povezava kjerkoli v podmapi (a -> domača mapa, cwd a/.ssh; b -> ../outside) ne prestavi agenta iz klona.
func TestWorktreeDirSymlink(t *testing.T) {
	root := t.TempDir()
	wt, home := filepath.Join(root, "wt"), filepath.Join(root, "home")
	os.MkdirAll(filepath.Join(wt, "x", "y"), 0o755)
	os.MkdirAll(filepath.Join(home, ".ssh"), 0o700)
	os.MkdirAll(filepath.Join(root, "outside", "z"), 0o755)
	if os.Symlink(home, filepath.Join(wt, "a")) != nil || os.Symlink(filepath.Join("..", "outside"), filepath.Join(wt, "b")) != nil {
		t.Skip("no symlinks")
	}
	os.Symlink(filepath.Join(home, ".ssh"), filepath.Join(wt, "x", "s"))
	for _, sub := range []string{"a", "a/.ssh", "b", "b/z", "x/s", "x/../a/.ssh", "missing"} {
		if got := worktreeDir(wt, sub); got != wt {
			t.Fatalf("%s: %s", sub, got)
		}
	}
	if got := worktreeDir(wt, "x/y"); got != filepath.Join(wt, "x", "y") {
		t.Fatal(got)
	}
}

// H2: zamenjan .git klona (nov config s filtrom, info/attributes) ali podtaknjen commondir: klon ni več v redu, zato
// claude v njem ne začne (git status samega claude bi filter izvedel zunaj peskovnika).
func TestWorktreeGitSwapRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("filter command is a shell script")
	}
	root := t.TempDir()
	client, repo, wt := filepath.Join(root, "c"), filepath.Join(root, "repos", "d", "r.git"), filepath.Join(root, "wt")
	os.MkdirAll(client, 0o755)
	gitT(t, client, "init", "-q", "-b", "main")
	writeT(t, filepath.Join(client, "a.txt"), "a\n")
	gitT(t, client, "add", "a.txt")
	gitT(t, client, "commit", "-qm", "first")
	head := gitT(t, client, "rev-parse", "HEAD")
	b := filepath.Join(root, "push.bundle")
	gitUser(client, nil, "update-ref", "refs/minigw/push", head)
	gitUser(client, nil, "bundle", "create", b, "refs/minigw/push")
	if ensureMirror(repo) != nil || !importBase(repo, b, head, "refs/minigw/base/x") || !makeClone(repo, wt, "minigw/claude-12345678-0", head) {
		t.Fatal("mirror or clone")
	}
	sl := &wtSlot{wt: wt}
	sl.git, _ = os.Lstat(filepath.Join(wt, ".git"))
	sl.cfg, _ = os.ReadFile(filepath.Join(wt, ".git", "config"))
	h := newShareHub()
	h.bySession["s"] = &serveShare{tree: &shareTree{slots: map[string]*wtSlot{"claude": sl}}}
	if !h.worktreeIntact(wt) || h.worktreeIntact(filepath.Join(root, "other")) {
		t.Fatal("intact clone refused or unknown clone accepted")
	}
	// Agent: mv .git .g, nov .git s filtrom (kopija), filter vsem datotekam.
	g := filepath.Join(wt, ".git")
	if err := os.Rename(g, filepath.Join(wt, ".g")); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("cp", "-R", filepath.Join(wt, ".g"), g).CombinedOutput(); err != nil {
		t.Fatal(string(out))
	}
	marker := filepath.Join(root, "ran")
	f, _ := os.OpenFile(filepath.Join(g, "config"), os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("[filter \"x\"]\n\tclean = touch " + marker + "; cat\n")
	f.Close()
	os.MkdirAll(filepath.Join(g, "info"), 0o755)
	writeT(t, filepath.Join(g, "info", "attributes"), "* filter=x\n")
	writeT(t, filepath.Join(wt, "a.txt"), "b\n") // ista velikost: git status prebere vsebino (filter clean)
	// Napad je resničen: git status z okoljem agenta filter izvede.
	c := exec.Command("git", "--no-optional-locks", "status", "--porcelain")
	c.Dir, c.Env = wt, append(os.Environ(), worktreeHarnessEnv(wt, filepath.Join(root, "m"))...)
	c.Run()
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("planted filter did not run: the test does not reproduce the attack")
	}
	if sl.gitIntact() || h.worktreeIntact(wt) {
		t.Fatal("swapped .git accepted")
	}
	// Isti .git, a podtaknjen info/attributes ali commondir.
	os.RemoveAll(g)
	os.Rename(filepath.Join(wt, ".g"), g)
	if !sl.gitIntact() {
		t.Fatal("restored .git refused")
	}
	for _, p := range []string{filepath.Join("info", "attributes"), "commondir"} {
		writeT(t, filepath.Join(g, p), "x\n")
		if sl.gitIntact() {
			t.Fatalf("%s accepted", p)
		}
		os.Remove(filepath.Join(g, p))
	}
}

// N9: agent v klonu naredi ugnezden repozitorij (mapa .git ali datoteka gitdir:) s svojim filtrom, gitlink v indeksu in
// .gitmodules z ignore=none; git status samega claude (okolje worktreeClaudeEnv) filtra ne sme izvesti, cloneProblem
// klon zavrne.
func TestWorktreeNestedRepoFilterDoesNotRun(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows: Claude in worktree needs unsandboxed Bash; filter command is a shell script")
	}
	for _, gitfile := range []bool{false, true} {
		root := t.TempDir()
		client, repo, wt := filepath.Join(root, "c"), filepath.Join(root, "repos", "d", "r.git"), filepath.Join(root, "wt")
		os.MkdirAll(client, 0o755)
		gitT(t, client, "init", "-q", "-b", "main")
		writeT(t, filepath.Join(client, "a.txt"), "a\n")
		gitT(t, client, "add", "a.txt")
		gitT(t, client, "commit", "-qm", "first")
		head := gitT(t, client, "rev-parse", "HEAD")
		b := filepath.Join(root, "push.bundle")
		gitUser(client, nil, "update-ref", "refs/minigw/push", head)
		gitUser(client, nil, "bundle", "create", b, "refs/minigw/push")
		if ensureMirror(repo) != nil || !importBase(repo, b, head, "refs/minigw/base/x") || !makeClone(repo, wt, "minigw/claude-12345678-0", head) {
			t.Fatal("mirror or clone")
		}
		if cloneProblem(wt, repo, nil) != "" {
			t.Fatal("fresh clone refused")
		}
		// Agent (v peskovniku smeta pisati v klon in njegov indeks).
		sub := filepath.Join(wt, "sub")
		if gitfile {
			os.MkdirAll(filepath.Join(wt, "build"), 0o755)
			gitT(t, wt, "init", "-q", "--separate-git-dir="+filepath.Join(wt, "build", "g"), "sub")
		} else {
			gitT(t, wt, "init", "-q", "sub")
		}
		writeT(t, filepath.Join(sub, "f"), "hello\n")
		gitT(t, sub, "add", "f")
		gitT(t, sub, "commit", "-qm", "s")
		sha := gitT(t, sub, "rev-parse", "HEAD")
		gd := gitT(t, sub, "rev-parse", "--absolute-git-dir")
		marker := filepath.Join(root, "ran")
		f, _ := os.OpenFile(filepath.Join(gd, "config"), os.O_APPEND|os.O_WRONLY, 0)
		f.WriteString("[filter \"x\"]\n\tclean = touch " + marker + "; cat\n")
		f.Close()
		os.MkdirAll(filepath.Join(gd, "info"), 0o755)
		writeT(t, filepath.Join(gd, "info", "attributes"), "* filter=x\n")
		writeT(t, filepath.Join(sub, "f"), "hellO\n")
		writeT(t, filepath.Join(wt, ".gitmodules"), "[submodule \"sub\"]\n\tpath = sub\n\turl = ./sub\n\tignore = none\n")
		gitT(t, wt, "add", ".gitmodules")
		gitT(t, wt, "update-index", "--add", "--cacheinfo", "160000,"+sha+",sub")
		status := func(env []string) bool {
			os.Remove(marker)
			c := exec.Command("git", "--no-optional-locks", "status", "--porcelain")
			c.Dir, c.Env = wt, append(os.Environ(), env...)
			c.Run()
			_, err := os.Stat(marker)
			return err == nil
		}
		// Napad je resničen: brez ovoja (le nastavitve) filter teče.
		if !status(worktreeHarnessEnv(wt, filepath.Join(root, "m"))) {
			t.Fatalf("gitfile=%v: planted filter did not run: the test does not reproduce the attack", gitfile)
		}
		env := worktreeClaudeEnv(wt, filepath.Join(root, "m"), filepath.Join(root, "gitexec"), []string{filepath.Join(wt, ".git")})
		if env == nil {
			t.Fatal("no git wrapper")
		}
		if status(env) {
			t.Fatalf("gitfile=%v: the nested repository's filter ran under the Claude CLI environment", gitfile)
		}
		// Tudi ko je ugnezdeni klon registriran (0.5.20): git samega claude vanj vstopi z relativnim GIT_DIR, ki ga ovoj
		// ne pusti; filter iz njegovega config ne teče.
		if status(worktreeClaudeEnv(wt, filepath.Join(root, "m"), filepath.Join(root, "gitexec"), []string{filepath.Join(wt, ".git"), gd})) {
			t.Fatalf("gitfile=%v: the registered nested repository's filter ran", gitfile)
		}
		if why := cloneProblem(wt, repo, nil); why != "changes inside the nested repository sub do not come back in worktree mode; use sync" {
			t.Fatalf("gitfile=%v: nested repository accepted: %q", gitfile, why)
		}
		// Ovoj pusti git klona samega (commit z vzdrževanjem).
		c := exec.Command("git", "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-qm", "agent", "--allow-empty")
		c.Dir, c.Env = wt, append(os.Environ(), env...)
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("commit in the clone: %v %s", err, out)
		}
	}
}

// N9 (odločitev 0.5.20): ugnezden repozitorij v poti, ki jo git prezre (build/_deps od CMake FetchContent), ne ustavi
// vrnitve veje; v poti, ki je git ne prezre (podmodul, ugnezden repozitorij), veja ne gre nazaj, sporočilo pove zakaj.
func TestWorktreeNestedRepoIgnoredDelivered(t *testing.T) {
	root := t.TempDir()
	client, repo, wt := filepath.Join(root, "c"), filepath.Join(root, "repos", "d", "r.git"), filepath.Join(root, "wt")
	os.MkdirAll(client, 0o755)
	gitT(t, client, "init", "-q", "-b", "main")
	writeT(t, filepath.Join(client, "a.txt"), "a\n")
	writeT(t, filepath.Join(client, ".gitignore"), "build/\n")
	gitT(t, client, "add", "-A")
	gitT(t, client, "commit", "-qm", "first")
	head := gitT(t, client, "rev-parse", "HEAD")
	b := filepath.Join(root, "push.bundle")
	gitUser(client, nil, "update-ref", "refs/minigw/push", head)
	gitUser(client, nil, "bundle", "create", b, "refs/minigw/push")
	if ensureMirror(repo) != nil || !importBase(repo, b, head, "refs/minigw/base/x") || !makeClone(repo, wt, "minigw/claude-12345678-0", head) {
		t.Fatal("mirror or clone")
	}
	dep := filepath.Join(wt, "build", "_deps", "foo-src") // FetchContent
	os.MkdirAll(dep, 0o755)
	gitT(t, dep, "init", "-q")
	writeT(t, filepath.Join(dep, "x.c"), "int x;\n")
	writeT(t, filepath.Join(wt, "a.txt"), "agent\n")
	if why := cloneProblem(wt, repo, nil); why != "" {
		t.Fatalf("ignored nested repository refused: %s", why)
	}
	tip := finalize(repo, wt, "minigw/claude-12345678-0", head, "claude", filepath.Join(root, "out.bundle"), nil)
	if tip == "" || tip == head {
		t.Fatalf("not delivered: %q", tip)
	}
	if files := gitT(t, repo, "ls-tree", "-r", "--name-only", tip); strings.Contains(files, "build/") || !strings.Contains(files, "a.txt") {
		t.Fatalf("branch content: %s", files)
	}
	lib := filepath.Join(wt, "vendor", "lib") // nesledeni ugnezdeni repozitorij
	os.MkdirAll(lib, 0o755)
	gitT(t, lib, "init", "-q")
	if why := cloneProblem(wt, repo, nil); why != "changes inside the nested repository vendor/lib do not come back in worktree mode; use sync" {
		t.Fatalf("non-ignored nested repository: %q", why)
	}
}

// N9: ovoj pusti le git, katerega GIT_DIR je točno eden od registriranih .git (vrhnji in ugnezdeni kloni); prazen
// seznam ali prazen GIT_DIR ne velja, relativna ».git« (git v podmodulu), predpona, vnos z glob znaki in repozitorij, ki
// ga je naredil agent, tudi ne.
func TestGitExecWrapNeedsGitDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no wrapper on Windows")
	}
	gw := gitExecWrap(filepath.Join(t.TempDir(), "gitexec"))
	if gw == "" {
		t.Fatal("no wrapper")
	}
	run := func(gitDir, want string) error {
		c := exec.Command(filepath.Join(gw, "git"), "--version")
		c.Env = append(os.Environ(), "GIT_DIR="+gitDir, "MINIGW_GIT_DIRS="+want)
		return c.Run()
	}
	reg := "/w/.git\n/w/lib/.git\n/w/lib/sub/.git"
	for _, bad := range [][2]string{{"", ""}, {"", reg}, {".git", "/w/.git"}, {".git", reg}, {"/w/evil/.git", reg}, {"/w/lib", reg},
		{"/w/lib/.git/", reg}, {"/w/x/.git", "/w/*/.git"}, {"/w/.git", "\n\n"}} {
		if run(bad[0], bad[1]) == nil {
			t.Fatalf("wrapper ran git for GIT_DIR %q with %q", bad[0], bad[1])
		}
	}
	for _, d := range []string{"/w/.git", "/w/lib/.git", "/w/lib/sub/.git"} {
		if err := run(d, reg); err != nil {
			t.Fatalf("wrapper refused the registered %s: %v", d, err)
		}
	}
	if env := gitWrapEnv([]string{"/w/.git", "rel/.git"}, gw); env != nil {
		t.Fatal("relative git dir accepted")
	}
	if env := gitWrapEnv([]string{"/w/a\nb/.git"}, gw); env != nil {
		t.Fatal("git dir with a newline accepted")
	}
}

// 0.5.20 (»worktree povsod«): podmodul (s commiti), nesleden ugnezden repozitorij s commitom in brez, globina 2 in
// prezrt ugnezden repozitorij. Agent ureja v vseh; vsak neprezrt dobi svojo vejo nazaj v svoj repozitorij, vrhnja
// veja zapiše novi gitlink podmodula; prezrti ostane zunaj; repozitoriji uporabnika (HEAD, indeks, config) ostanejo.
func TestServeWorktreeNestedRoundTrip(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake claude is a shell script")
	}
	root := t.TempDir()
	client, src := filepath.Join(root, "client"), filepath.Join(root, "subsrc")
	for _, d := range []string{client, src} {
		os.MkdirAll(d, 0o755)
		gitT(t, d, "init", "-q", "-b", "main")
	}
	writeT(t, filepath.Join(src, "s.txt"), "s\n")
	gitT(t, src, "add", "-A")
	gitT(t, src, "commit", "-qm", "sub")
	writeT(t, filepath.Join(client, "a.txt"), "a\n")
	writeT(t, filepath.Join(client, ".gitignore"), "build/\n")
	gitT(t, client, "add", "-A")
	gitT(t, client, "-c", "protocol.file.allow=always", "submodule", "add", "-q", src, "sub")
	gitT(t, client, "commit", "-qm", "first")
	repos := map[string]bool{"sub": true, "sub/deep": true, "vendor/lib": true, "nc": false} // pot → s commitom
	for p, commit := range repos {
		if p == "sub" {
			continue
		}
		d := filepath.Join(client, p)
		os.MkdirAll(d, 0o755)
		gitT(t, d, "init", "-q", "-b", "main")
		writeT(t, filepath.Join(d, "f.txt"), p+"\n")
		if commit {
			gitT(t, d, "add", "-A")
			gitT(t, d, "commit", "-qm", p)
		}
	}
	writeT(t, filepath.Join(client, "vendor", "lib", "f.txt"), "vendor/lib\nuncommitted\n")
	dep := filepath.Join(client, "build", "_deps", "x")
	os.MkdirAll(dep, 0o755)
	gitT(t, dep, "init", "-q")
	writeT(t, filepath.Join(dep, "d.txt"), "dep\n")
	state := func() string {
		var b strings.Builder
		for _, p := range []string{"", "sub", "sub/deep", "vendor/lib", "nc"} {
			d := filepath.Join(client, p)
			h, _ := gitUser(d, nil, "rev-parse", "-q", "--verify", "HEAD")
			st, _ := gitUser(d, nil, "status", "--porcelain", "--ignore-submodules=all")
			fmt.Fprintf(&b, "%s %s %s\n", p, h, st)
		}
		return b.String()
	}
	before := state()

	fake := filepath.Join(root, "claude")
	writeT(t, fake, `#!/bin/sh
`+cliGuard+`
cat > /dev/null
for p in . sub sub/deep vendor/lib nc build/_deps/x; do echo "agent" >> "$p/agent.txt"; done
grep -q uncommitted vendor/lib/f.txt && echo saw > vendor/lib/seen.txt
echo '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"done"}}}'
echo '{"type":"result","subtype":"success","usage":{"output_tokens":1}}'
`)
	os.Chmod(fake, 0o755)
	s, _ := testServer(t, func(c *serveConfig) { c.Claude = fake })
	c := startLoopback(t, s)
	sc := &shareClient{c: c, session: "sidn", root: client, worktree: true, top: filepath.ToSlash(client), synced: map[string]syncEntry{}, reported: map[string]bool{}}
	if !sc.register() {
		t.Fatal("register failed: " + sc.refusal)
	}
	sc.start(2)
	defer sc.unregister()

	stderr := filepath.Join(root, "stderr")
	f, _ := os.Create(stderr)
	old := os.Stderr
	os.Stderr = f
	body := `{"model":"claude-cli-haiku","tools":[{"name":"x"}],"messages":[{"role":"user","content":"go"}]}`
	h := map[string]string{"x-claude-mini-session": "sidn", "x-claude-mini-cwd": client, "content-type": "application/json"}
	st, data := call(c, "POST", "/v1/messages", h, []byte(body), 60*time.Second)
	os.Stderr = old
	f.Close()
	out, _ := os.ReadFile(stderr)
	if st != 200 || !strings.Contains(string(data), `"done"`) {
		t.Fatalf("messages: %d %s\n%s", st, data, out)
	}
	branch := "minigw/claude-" + sha256Hex([]byte("sidn"))[:8] + "-0"
	for _, p := range []string{"", "sub", "sub/deep", "vendor/lib", "nc"} {
		d := filepath.Join(client, p)
		files, err := gitUser(d, nil, "ls-tree", "-r", "--name-only", branch)
		if err != nil || !strings.Contains("\n"+files+"\n", "\nagent.txt\n") {
			t.Fatalf("%q: branch %s: %v\n%s\n%s", p, branch, err, files, out)
		}
		line := "agent work is on branch " + branch + " in nested repository " + p + ": review with git -C " + p + " diff "
		if p == "" {
			line = "agent work is on branch " + branch + ": review with git diff "
		}
		if !strings.Contains(string(out), "minigw: "+line) {
			t.Fatalf("%q: no branch line:\n%s", p, out)
		}
	}
	if !strings.Contains(gitT(t, filepath.Join(client, "vendor", "lib"), "ls-tree", "-r", "--name-only", branch), "seen.txt") {
		t.Fatal("agent did not see the nested repository's uncommitted change")
	}
	if strings.Contains(string(out), "build/_deps") || gitT(t, dep, "branch", "--list", "minigw/*") != "" {
		t.Fatalf("ignored nested repository delivered:\n%s", out)
	}
	if got := gitT(t, client, "ls-tree", "-r", "--name-only", branch); strings.Contains(got, "build/") || strings.Contains(got, "nc/") ||
		strings.Contains(got, "vendor/lib/") || strings.Contains(got, "sub/") {
		t.Fatalf("top branch has nested files: %s", got)
	}
	// Vrhnja veja: gitlink podmodula kaže na vrh veje v podmodulu (ta vsebuje commit z gitlinkom sub/deep).
	subTip := gitT(t, filepath.Join(client, "sub"), "rev-parse", branch)
	if l := gitT(t, client, "ls-tree", branch, "sub"); !strings.HasPrefix(l, "160000 commit "+subTip) {
		t.Fatalf("top gitlink %q, sub branch %s", l, subTip)
	}
	if l := gitT(t, filepath.Join(client, "sub"), "ls-tree", branch, "deep"); !strings.HasPrefix(l, "160000 commit "+gitT(t, filepath.Join(client, "sub", "deep"), "rev-parse", branch)) {
		t.Fatalf("sub gitlink to deep: %q", l)
	}
	if gitT(t, filepath.Join(client, "nc"), "rev-list", "--count", branch) != "2" { // posnetek brez starša + delo agenta
		t.Fatal("nested repository without a commit: unexpected history")
	}
	if got := state(); got != before {
		t.Fatalf("user's repositories changed:\n%s\n%s", before, got)
	}
}

// 0.5.20: nestedProblem sprejme točno registrirane ugnezdene klone, repozitorij, ki ga agent naredi v klonu (tudi v
// ugnezdenem), pa zavrne; cloneTarget (H1) ne klonira skozi povezavo ali v neprazno mapo; nestedIntact zazna zamenjan klon.
func TestWorktreeNestedClonesRegistered(t *testing.T) {
	root := t.TempDir()
	client, wt := filepath.Join(root, "c"), filepath.Join(root, "wt")
	repo := filepath.Join(root, "repos", "d", "r.git")
	os.MkdirAll(filepath.Join(client, "lib"), 0o755)
	for _, d := range []string{client, filepath.Join(client, "lib")} {
		gitT(t, d, "init", "-q", "-b", "main")
		writeT(t, filepath.Join(d, "f.txt"), "x\n")
		gitT(t, d, "add", "f.txt")
		gitT(t, d, "commit", "-qm", "x")
	}
	push := func(dir, mirror string) string {
		head := gitT(t, dir, "rev-parse", "HEAD")
		b := filepath.Join(root, "push.bundle")
		os.Remove(b)
		gitUser(dir, nil, "update-ref", "refs/minigw/push", head)
		gitUser(dir, nil, "bundle", "create", b, "refs/minigw/push")
		if ensureMirror(mirror) != nil || !importBase(mirror, b, head, "refs/minigw/base/x") {
			t.Fatal("mirror")
		}
		return head
	}
	head := push(client, repo)
	n := &wtNested{path: "lib", repo: nestedMirror(repo, "lib")}
	n.base = push(filepath.Join(client, "lib"), n.repo)
	if !strings.HasSuffix(n.repo, "r-"+nestedKey("lib")+".git") || filepath.Dir(n.repo) != filepath.Dir(repo) {
		t.Fatalf("nested mirror %s", n.repo)
	}
	if !makeClone(repo, wt, "minigw/claude-12345678-0", head) || !cloneTarget(wt, "lib") || !makeClone(n.repo, n.dir(wt), "minigw/claude-12345678-0", n.base) {
		t.Fatal("clone")
	}
	n.git, _ = os.Lstat(filepath.Join(wt, "lib", ".git"))
	nested := []*wtNested{n}
	if why := cloneProblem(wt, repo, nil); !strings.Contains(why, "nested repository lib ") {
		t.Fatalf("unregistered clone accepted: %q", why)
	}
	if why := cloneProblem(wt, repo, nested); why != "" || !nestedIntact(wt, n) {
		t.Fatalf("registered clone refused: %q", why)
	}
	evil := filepath.Join(wt, "lib", "x", "evil")
	os.MkdirAll(evil, 0o755)
	gitT(t, evil, "init", "-q")
	if why := cloneProblem(wt, repo, nested); why != "changes inside the nested repository lib/x/evil do not come back in worktree mode; use sync" {
		t.Fatalf("agent's repository inside a nested clone: %q", why)
	}
	os.RemoveAll(filepath.Join(wt, "lib", "x"))
	// H1: povezava v poti, neprazen cilj
	os.Symlink(root, filepath.Join(wt, "link"))
	os.MkdirAll(filepath.Join(wt, "full"), 0o755)
	writeT(t, filepath.Join(wt, "full", "f"), "x")
	for _, p := range []string{"link/x", "link", "full", "f.txt/x"} {
		if cloneTarget(wt, p) {
			t.Fatalf("clone target %s accepted", p)
		}
	}
	if !cloneTarget(wt, "new/deep/x") {
		t.Fatal("new path refused")
	}
	os.Rename(filepath.Join(wt, "lib"), filepath.Join(wt, "lib2"))
	os.Symlink(filepath.Join(wt, "lib2"), filepath.Join(wt, "lib"))
	if nestedIntact(wt, n) {
		t.Fatal("nested clone behind a symlink accepted")
	}
	sl := &wtSlot{wt: wt, nested: nested}
	sl.git, _ = os.Lstat(filepath.Join(wt, ".git"))
	sl.cfg, _ = os.ReadFile(filepath.Join(wt, ".git", "config"))
	if sl.gitIntact() {
		t.Fatal("slot intact with a moved nested clone")
	}
	if got := sl.gitDirs(); !slices.Equal(got, []string{filepath.Join(wt, ".git"), filepath.Join(wt, "lib", ".git")}) {
		t.Fatalf("git dirs %q", got)
	}
}

// 0.5.20: odjemalec z ugnezdenim repozitorijem in starejšim prehodom (brez »nested« v WORKTREE-PUSH) odpove (način
// ostane, ugnezdeni ne izginejo tiho); več kot maxNestedRepos: napaka.
func TestWorktreePushNestedNeedsGateway(t *testing.T) {
	top := t.TempDir()
	gitT(t, top, "init", "-q", "-b", "main")
	writeT(t, filepath.Join(top, "a.txt"), "a\n")
	gitT(t, top, "add", "-A")
	gitT(t, top, "commit", "-qm", "a")
	os.MkdirAll(filepath.Join(top, "lib"), 0o755)
	gitT(t, filepath.Join(top, "lib"), "init", "-q")
	sc := &shareClient{top: top, worktree: true, reported: map[string]bool{}}
	if r := sc.wtPush("k", davJob{Body: []byte(`{"have":[]}`)}); r.Status != 500 || !sc.reported["nested-old"] {
		t.Fatalf("old gateway: %d %v", r.Status, sc.reported)
	}
	for i := range maxNestedRepos {
		os.MkdirAll(filepath.Join(top, "n", strconv.Itoa(i)), 0o755)
		gitT(t, filepath.Join(top, "n", strconv.Itoa(i)), "init", "-q")
	}
	if n := nestedRepos(top); len(n) != maxNestedRepos+1 {
		t.Fatalf("found %d", len(n))
	}
	if r := sc.wtPush("k", davJob{Body: []byte(`{"have":[],"nested":{}}`)}); r.Status != 500 || !sc.reported["nested-max"] {
		t.Fatalf("too many: %d %v", r.Status, sc.reported)
	}
}
