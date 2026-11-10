package main

import (
	"crypto/ecdh"
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
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
	if ensureMirror(repo) != nil || importBase(repo, b, head, "refs/minigw/base/x") != nil || makeClone(repo, wt, "minigw/claude-12345678-0", head) != nil {
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
	if tip := finalize(repo, wt, "minigw/claude-12345678-0", head, "claude", filepath.Join(root, "out.bundle"), nil, nil); tip != "" {
		t.Fatalf("finalize accepted a .git tree: %s", tip)
	}
	os.RemoveAll(wt)
	if makeClone(repo, filepath.Join(root, "wt2"), "minigw/claude-12345678-1", head) != nil {
		t.Fatal("mirror broken after a rejected result")
	}
}

// Claude v klonu, ko je cwd podmapa: koren klona v peskovniku (branje, pisanje, --add-dir, pravila orodij), .git klona ne.
func TestWorktreeClaudeArgs(t *testing.T) {
	args := []string{"--allowedTools", "Read(./**)", "--disallowedTools", "Read(/cfg/**)", "--settings",
		`{"sandbox":{"filesystem":{"allowRead":["/w/sub"],"allowWrite":["/w/sub"],"denyRead":["~"]}}}`}
	got := strings.Join(worktreeClaudeArgs(args, "/w", "/m", "/t", []string{"/r/objects", "/r-k/objects"}, []string{"/w/.git", "/w/lib/.git"}), " ")
	for _, want := range []string{`"allowWrite":["/w/sub","/w","/m","/t","/w.deps","/.cache"]`, `"allowRead":["/w/sub","/w","/m","/t","/w.deps","/.cache","/r/objects","/r-k/objects"]`, "--add-dir /w",
		"Edit(//w/**)", `"denyWrite":["/w/.git/config"`, `"/w/lib/.git/config","/w/lib/.git/hooks","/w/lib/.git/info","/w/lib/.git/commondir"]`, "Edit(//w/.git/**)"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing %s in\n%s", want, got)
		}
	}
	// #70: na Windows (brez sandboxa) git agenta ni vnaprej dovoljen (git -c, alias … = poljuben ukaz); commit naredi prehod
	worktreeNeedsUnsandboxed = true
	defer func() { worktreeNeedsUnsandboxed = runtime.GOOS == "windows" }()
	args = []string{"--allowedTools", "Read(./**)", "--disallowedTools", "Read(/cfg/**)"}
	got = strings.Join(worktreeClaudeArgs(args, "/w", "/m", "/t", nil, nil), " ")
	if strings.Contains(got, "Bash(") || strings.Contains(got, "PowerShell(") || !strings.Contains(got, "Edit(//w/.git/**)") {
		t.Fatalf("Windows: git or shell rule in\n%s", got)
	}
	n := worktreeNote("/c", "/w", "minigw/claude-1", []string{"/w/.git", "/w/lib/.git"})
	if !strings.Contains(n, "Do not run git add or git commit: the gateway commits your changes on this branch when you finish.") ||
		strings.Contains(n, "Commit your work") || strings.Contains(n, "GIT_DIR") || !strings.Contains(n, "/w/lib") {
		t.Fatalf("Windows note: %s", n)
	}
	worktreeNeedsUnsandboxed = false
	if n := worktreeNote("/c", "/w", "b", []string{"/w/.git", "/w/lib/.git"}); !strings.Contains(n, "Commit your work on this branch") || !strings.Contains(n, "git -C <path>") {
		t.Fatalf("sandboxed note: %s", n)
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
if [ "$3" = debug ]; then echo '{"models":[]}'; exit 0; fi
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
	if ensureMirror(repo) != nil || importBase(repo, b, head, "refs/minigw/base/x") != nil || makeClone(repo, wt, "minigw/claude-12345678-0", head) != nil {
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
	c.Dir, c.Env = wt, append(os.Environ(), worktreeHarnessEnv(wt, filepath.Join(root, "m"), t.TempDir(), "claude")...)
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
	// #40: commondir ».« (makeClone na Linuxu) je ta .git
	for _, c := range []string{".", " .\n", "./\n", ".\n\n"} {
		writeT(t, filepath.Join(g, "commondir"), c)
		if sl.gitIntact() {
			t.Fatalf("commondir %q accepted", c)
		}
	}
	writeT(t, filepath.Join(g, "commondir"), ".\n")
	if !sl.gitIntact() {
		t.Fatal("commondir . refused")
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
		if ensureMirror(repo) != nil || importBase(repo, b, head, "refs/minigw/base/x") != nil || makeClone(repo, wt, "minigw/claude-12345678-0", head) != nil {
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
		if !status(worktreeHarnessEnv(wt, filepath.Join(root, "m"), t.TempDir(), "claude")) {
			t.Fatalf("gitfile=%v: planted filter did not run: the test does not reproduce the attack", gitfile)
		}
		env := worktreeClaudeEnv(wt, filepath.Join(root, "m"), t.TempDir(), filepath.Join(root, "gitexec"), []string{filepath.Join(wt, ".git")}, "claude")
		if env == nil {
			t.Fatal("no git wrapper")
		}
		if status(env) {
			t.Fatalf("gitfile=%v: the nested repository's filter ran under the Claude CLI environment", gitfile)
		}
		// Tudi ko je ugnezdeni klon registriran (0.5.20): git samega claude vanj vstopi z relativnim GIT_DIR, ki ga ovoj
		// ne pusti; filter iz njegovega config ne teče.
		if status(worktreeClaudeEnv(wt, filepath.Join(root, "m"), t.TempDir(), filepath.Join(root, "gitexec"), []string{filepath.Join(wt, ".git"), gd}, "claude")) {
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
	if ensureMirror(repo) != nil || importBase(repo, b, head, "refs/minigw/base/x") != nil || makeClone(repo, wt, "minigw/claude-12345678-0", head) != nil {
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
	tip := finalize(repo, wt, "minigw/claude-12345678-0", head, "claude", filepath.Join(root, "out.bundle"), nil, nil)
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
	testNestedRoundTrip(t, "")
}

// N1 (lastnik): kjer agent ne bi bil zaprt, se z ugnezdenimi repozitoriji ne začne: agent ne teče, sporočilo pove zakaj.
func TestServeWorktreeNestedRefusedAtStart(t *testing.T) {
	testNestedRoundTrip(t, "nested repositories need bwrap on this Linux gateway; use sync")
}

func testNestedRoundTrip(t *testing.T, blockNested string) {
	if runtime.GOOS == "windows" {
		t.Skip("fake claude is a shell script")
	}
	if !gitAttrSource() {
		t.Skip("git before 2.40: the client refuses nested repositories (TestNestedSnapshotRunsNoCleanFilter)")
	}
	if runtime.GOOS == "linux" && blockNested == "" && (finalizeBwrap() == "" || !claudePinOK(t.TempDir(), t.Logf)) {
		// npr. Ubuntu 24.04+ (GitHub Actions): AppArmor ne dovoli bwrap v bwrap, gateway gnezdene zavrne
		t.Skip("Linux without bwrap inside bwrap: nested repositories are refused (TestNestedBlockerMatchesSandbox)")
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
touch `+root+`/started
for p in . sub sub/deep vendor/lib nc build/_deps/x; do echo "agent" >> "$p/agent.txt"; done
grep -q uncommitted vendor/lib/f.txt && echo saw > vendor/lib/seen.txt
# B: navodilo poimenuje ugnezdene klone; agent commita v podmodulu (#40: git -C in cd prek pathWrapScript, avtor agent)
while [ $# -gt 0 ]; do [ "$1" = --append-system-prompt-file ] && grep -qE "/claude-[0-9a-f]+-0/sub[,.]" "$2" && grep -q "git -C <path>" "$2" && touch note-ok; shift; done
git add -A && git commit -qm "agent top" # cilj 1: nesledeni ugnezdeni niso gitlink
git -C sub add agent.txt && git -C sub commit -qm "agent in sub" || echo commit-failed > commit-failed.txt
(cd sub/deep && git add agent.txt && git commit -qm "agent in deep") || echo commit-failed > commit-failed.txt
echo '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"done"}}}'
echo '{"type":"result","subtype":"success","usage":{"output_tokens":1}}'
`)
	os.Chmod(fake, 0o755)
	s, _ := testServer(t, func(c *serveConfig) { c.Claude = fake })
	s.hub.nestedBlocker = func(string) string { return blockNested } // macOS serve (za razvoj) ugnezdene sicer zavrne
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
	if blockNested != "" {
		// končna napaka (400: Claude Code je ne ponovi), razlog enkrat (v odgovoru, ne še kot opomba minigw)
		if _, err := os.Stat(filepath.Join(root, "started")); err == nil || st != 400 || !strings.Contains(string(data), blockNested) ||
			strings.Contains(string(out), blockNested) {
			t.Fatalf("not refused once with a final error before the agent: %d %s\n%s", st, data, out)
		}
		return
	}
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
	if got := gitT(t, filepath.Join(client, "sub"), "log", "-1", "--format=%an <%ae> %s", branch); got != "Claude (Mini Gateway) <claude@minigw.invalid> agent in sub" {
		t.Fatalf("agent's commit in the nested clone not kept: %s", got)
	}
	if got := gitT(t, filepath.Join(client, "sub", "deep"), "log", "-1", "--format=%an %s", branch); got != "Claude (Mini Gateway) agent in deep" {
		t.Fatalf("agent's commit after cd in the nested clone: %s", got)
	}
	if files := gitT(t, client, "ls-tree", "-r", "--name-only", branch); !strings.Contains(files, "note-ok") || strings.Contains(files, "commit-failed") {
		t.Fatalf("agent note or nested commit: %s", files)
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
	// Vrhnja veja: gitlink podmodula kaže na vrh veje v podmodulu.
	subTip := gitT(t, filepath.Join(client, "sub"), "rev-parse", branch)
	if l := gitT(t, client, "ls-tree", branch, "sub"); !strings.HasPrefix(l, "160000 commit "+subTip) {
		t.Fatalf("top gitlink %q, sub branch %s", l, subTip)
	}
	if l := gitT(t, filepath.Join(client, "sub"), "ls-tree", branch, "deep"); l != "" { // nesleden v sub: ni gitlink (cilj 1)
		t.Fatalf("sub gitlink to deep: %q", l)
	}
	if gitT(t, filepath.Join(client, "nc"), "rev-list", "--count", branch) != "2" { // posnetek brez starša + delo agenta
		t.Fatal("nested repository without a commit: unexpected history")
	}
	if got := state(); got != before {
		t.Fatalf("user's repositories changed:\n%s\n%s", before, got)
	}
	// Cilj 1: nesleden ugnezden (vendor/lib s commitom, nc) ni gitlink ne v posnetku ne v veji; cherry-pick je čist.
	for _, p := range []string{"vendor/lib", "nc"} {
		if l := gitT(t, client, "ls-tree", branch, p); l != "" {
			t.Fatalf("%s in the top branch: %s", p, l)
		}
	}
	base := regexp.MustCompile(`agent work is on branch \S+: review with git diff ([0-9a-f]{40})\.\.`).FindStringSubmatch(string(out))
	if base == nil {
		t.Fatalf("no base:\n%s", out)
	}
	if o, err := exec.Command("git", "-C", client, "cherry-pick", "-n", base[1]+".."+branch).CombinedOutput(); err != nil {
		t.Fatalf("cherry-pick -n: %v %s", err, o)
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
		if ensureMirror(mirror) != nil || importBase(mirror, b, head, "refs/minigw/base/x") != nil {
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
	if makeClone(repo, wt, "minigw/claude-12345678-0", head) != nil || !cloneTarget(wt, "lib") || makeClone(n.repo, n.dir(wt), "minigw/claude-12345678-0", n.base) != nil {
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
	gitT(t, filepath.Join(top, "lib"), "config", "filter.lfs.clean", "cat") // R2: filtri ugnezdenega: ne začne
	if r := sc.wtPush("k", davJob{Body: []byte(`{"have":[],"nested":{}}`)}); r.Status != 500 || !sc.reported["nested-filter:lib"] {
		t.Fatalf("nested filter: %d %v", r.Status, sc.reported)
	}
	gitT(t, filepath.Join(top, "lib"), "config", "--remove-section", "filter.lfs")
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

// N1: zamenjava poti ugnezdenega klona s povezavo ven po preverbi (agentov proces, ki je preživel) zavrže rezultat:
// tuja mapa (skrivnost prehoda) ne pride v vejo, ref zrcala ostane prejšnji.
func TestWorktreeNestedSwapDuringFinalize(t *testing.T) {
	if runtime.GOOS == "linux" && finalizeBwrap() == "" {
		t.Skip("Linux without bwrap: nested repositories are refused")
	}
	if runtime.GOOS == "windows" {
		t.Skip("symlink")
	}
	root := t.TempDir()
	client, wt, repo := filepath.Join(root, "c"), filepath.Join(root, "wt"), filepath.Join(root, "repos", "d", "r.git")
	outside := filepath.Join(root, "outside")
	for _, d := range []string{filepath.Join(client, "lib"), outside} {
		os.MkdirAll(d, 0o755)
		gitT(t, d, "init", "-q", "-b", "main")
		writeT(t, filepath.Join(d, "f.txt"), "x\n")
		gitT(t, d, "add", "f.txt")
		gitT(t, d, "commit", "-qm", "x")
	}
	writeT(t, filepath.Join(outside, "secret.txt"), "gateway secret\n")
	n := &wtNested{path: "lib", repo: nestedMirror(repo, "lib")}
	n.base = gitT(t, filepath.Join(client, "lib"), "rev-parse", "HEAD")
	b := filepath.Join(root, "push.bundle")
	gitUser(filepath.Join(client, "lib"), nil, "update-ref", "refs/minigw/push", n.base)
	gitUser(filepath.Join(client, "lib"), nil, "bundle", "create", b, "refs/minigw/push")
	if ensureMirror(n.repo) != nil || importBase(n.repo, b, n.base, "refs/minigw/base/x") != nil || makeClone(n.repo, n.dir(wt), "minigw/claude-12345678-0", n.base) != nil {
		t.Fatal("mirror or clone")
	}
	n.git, _ = os.Lstat(filepath.Join(wt, "lib", ".git"))
	sl := &wtSlot{wt: wt, branch: "minigw/claude-12345678-0", nested: []*wtNested{n}}
	writeT(t, filepath.Join(wt, "lib", "agent.txt"), "agent\n")
	beforeNestedFinalize = func(*wtNested) { // po preverbi: lib → povezava na tujo mapo
		os.Rename(filepath.Join(wt, "lib"), filepath.Join(wt, "lib.real"))
		os.Symlink(outside, filepath.Join(wt, "lib"))
	}
	defer func() { beforeNestedFinalize = nil }()
	tip, why := finalizeNested(sl, n, "claude", filepath.Join(root, "out.bundle"), nil)
	if tip != "" || !strings.Contains(why, "moved or replaced") {
		t.Fatalf("swap accepted: %q %q", tip, why)
	}
	if r, _ := gitRun(n.repo, "", nil, "--git-dir", n.repo, "rev-parse", "-q", "--verify", "refs/heads/minigw/claude-12345678-0"); r != "" {
		t.Fatalf("mirror ref moved to %s", r)
	}
	if _, err := os.Stat(filepath.Join(root, "out.bundle")); err == nil {
		t.Fatal("bundle kept")
	}
}

// N2: povezava na poti gitlinka ali ugnezdenega repozitorija (na repozitorij zunaj mape) ni ugnezden repozitorij te
// mape: posnetek in prevzem je ne vzameta. N5/N6: imena s HFS+ prezrtimi znaki in pot v .build.
func TestNestedReposRefuseSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink")
	}
	root := t.TempDir()
	top, outside := filepath.Join(root, "top"), filepath.Join(root, "outside")
	for _, d := range []string{top, outside, filepath.Join(top, "real")} {
		os.MkdirAll(d, 0o755)
		gitT(t, d, "init", "-q", "-b", "main")
		writeT(t, filepath.Join(d, "f.txt"), "x\n")
		gitT(t, d, "add", "f.txt")
		gitT(t, d, "commit", "-qm", "x")
	}
	sha := gitT(t, outside, "rev-parse", "HEAD")
	os.Symlink(outside, filepath.Join(top, "link"))
	gitT(t, top, "update-index", "--add", "--cacheinfo", "160000,"+sha+",link") // gitlink na poti povezave
	os.MkdirAll(filepath.Join(top, "d"), 0o755)
	os.Symlink(outside, filepath.Join(top, "d", "inner"))
	if n := nestedRepos(top); !slices.Equal(n, []string{"real"}) {
		t.Fatalf("nested repos: %q", n)
	}
	if nestedInside(top, "link") || nestedInside(top, "d/inner") || !nestedInside(top, "real") {
		t.Fatal("nestedInside")
	}
	if normName(".g\u200cit") != ".git" || validNestedPath("a\u200d/b") || validNestedPath("x\ufeff") || validNestedPath(".build/x") || validNestedPath(".BUILD") {
		t.Fatal("N5/N6 names accepted")
	}
}

// Agent je vse commital sam (nič necommitanega): veja se shrani in gre nazaj (prej je preverba povezanosti ob
// »bundle unbundle« padla, ker je ref zrcala kazal na commit le v klonu).
func TestWorktreeFinalizeAllCommitted(t *testing.T) {
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
	if ensureMirror(repo) != nil || importBase(repo, b, head, "refs/minigw/base/x") != nil || makeClone(repo, wt, "minigw/claude-12345678-0", head) != nil {
		t.Fatal("mirror")
	}
	writeT(t, filepath.Join(wt, "b.txt"), "b\n")
	gitT(t, wt, "add", "b.txt")
	gitT(t, wt, "-c", "user.name=a", "-c", "user.email=a@x", "commit", "-qm", "agent")
	agentTip := gitT(t, wt, "rev-parse", "HEAD")
	if tip := finalize(repo, wt, "minigw/claude-12345678-0", head, "claude", filepath.Join(root, "out.bundle"), nil, nil); tip != agentTip {
		t.Fatalf("all-committed work not saved: %q", tip)
	}
	os.RemoveAll(wt)
	if got, _ := gitRun(repo, "", nil, "--git-dir", repo, "show", "minigw/claude-12345678-0:b.txt"); got != "b" {
		t.Fatalf("mirror lacks the agent's objects: %q", got)
	}
}

// N1 (Linux): agentov proces, ki zapusti sejo (setsid) in klon (cd /), killInside pred finalize vseeno ustavi (GIT_WORK_TREE
// klona v okolju); proces brez tega znaka ostane.
func TestKillInsideFindsEscapedAgent(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("killInside runs on Linux only")
	}
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("no setsid")
	}
	wt := t.TempDir()
	start := func(env string) *exec.Cmd {
		c := exec.Command("setsid", "sh", "-c", "cd /; exec sleep 30")
		c.Env = append(os.Environ(), env)
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		return c
	}
	agent, other := start("GIT_WORK_TREE="+wt), start("GIT_WORK_TREE=/elsewhere")
	defer other.Process.Kill()
	time.Sleep(200 * time.Millisecond)
	killInside(wt)
	done := make(chan error, 1)
	go func() { done <- agent.Wait() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		agent.Process.Kill()
		t.Fatal("escaped agent process survived killInside")
	}
	if other.Process.Signal(syscall.Signal(0)) != nil {
		t.Fatal("unrelated process was killed")
	}
}

// N1 (recenzija 2, Linux): agent, ki bi preživel, med git finalize zamenja lib s povezavo na tuj repozitorij in jo pred
// preverbo vrne. git bere le odprto mapo klona (bwrap, --ro-bind-fd): veja ima delo agenta, ne tuje skrivnosti, tuj
// objects/info ostane.
func TestWorktreeNestedSwapAndRestore(t *testing.T) {
	if runtime.GOOS != "linux" || finalizeBwrap() == "" {
		t.Skip("Linux with bwrap")
	}
	root := t.TempDir()
	client, wt, repo := filepath.Join(root, "c"), filepath.Join(root, "wt"), filepath.Join(root, "repos", "d", "r.git")
	outside := filepath.Join(root, "outside")
	for _, d := range []string{filepath.Join(client, "lib"), outside} {
		os.MkdirAll(d, 0o755)
		gitT(t, d, "init", "-q", "-b", "main")
		writeT(t, filepath.Join(d, "f.txt"), "x\n")
		gitT(t, d, "add", "f.txt")
		gitT(t, d, "commit", "-qm", "x")
	}
	writeT(t, filepath.Join(outside, "secret.txt"), "gateway secret\n")
	writeT(t, filepath.Join(outside, ".git", "objects", "info", "marker"), "keep\n")
	n := &wtNested{path: "lib", repo: nestedMirror(repo, "lib")}
	n.base = gitT(t, filepath.Join(client, "lib"), "rev-parse", "HEAD")
	b := filepath.Join(root, "push.bundle")
	gitUser(filepath.Join(client, "lib"), nil, "update-ref", "refs/minigw/push", n.base)
	gitUser(filepath.Join(client, "lib"), nil, "bundle", "create", b, "refs/minigw/push")
	if ensureMirror(n.repo) != nil || importBase(n.repo, b, n.base, "refs/minigw/base/x") != nil || makeClone(n.repo, n.dir(wt), "minigw/claude-12345678-0", n.base) != nil {
		t.Fatal("mirror or clone")
	}
	n.git, _ = os.Lstat(filepath.Join(wt, "lib", ".git"))
	sl := &wtSlot{wt: wt, branch: "minigw/claude-12345678-0", nested: []*wtNested{n}}
	writeT(t, filepath.Join(wt, "lib", "agent.txt"), "agent\n")
	duringFinalize = func() {
		os.Rename(filepath.Join(wt, "lib"), filepath.Join(wt, "lib.real"))
		os.Symlink(outside, filepath.Join(wt, "lib"))
	}
	afterNestedFinalize = func(*wtNested) {
		os.Remove(filepath.Join(wt, "lib"))
		os.Rename(filepath.Join(wt, "lib.real"), filepath.Join(wt, "lib"))
	}
	defer func() { duringFinalize, afterNestedFinalize = nil, nil }()
	tip, why := finalizeNested(sl, n, "claude", filepath.Join(root, "out.bundle"), nil)
	if tip == "" {
		t.Fatalf("not delivered: %s", why)
	}
	files, _ := gitRun(n.repo, "", nil, "--git-dir", n.repo, "ls-tree", "-r", "--name-only", tip)
	if strings.Contains(files, "secret.txt") || !strings.Contains(files, "agent.txt") {
		t.Fatalf("branch content after swap: %s", files)
	}
	if b, err := os.ReadFile(filepath.Join(outside, ".git", "objects", "info", "marker")); err != nil || string(b) != "keep\n" {
		t.Fatal("outside objects/info changed")
	}
}

// N1 (Linux): agent brez svojega imenskega prostora PID (unconfined) — ugnezdeni ne gre nazaj, sporočilo pove zakaj.
func TestNestedFinalizeFailsClosedUnconfined(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Linux only")
	}
	n := &wtNested{path: "lib", repo: filepath.Join(t.TempDir(), "r-x.git")}
	sl := &wtSlot{wt: t.TempDir(), branch: "minigw/claude-12345678-0", nested: []*wtNested{n}, unconfined: true}
	if tip, why := finalizeNested(sl, n, "claude", filepath.Join(t.TempDir(), "o"), nil); tip != "" || !strings.HasSuffix(why, "use sync") {
		t.Fatalf("%q %q", tip, why)
	}
}

// N1 (lastnik): ugnezdeni le, kjer bo agent zaprt; drugje razlog s »use sync« (preizkus v Docker z bwrap in brez).
func TestNestedBlockerMatchesSandbox(t *testing.T) {
	s, _ := testServer(t, nil)
	why := s.nestedBlocker("claude")
	switch runtime.GOOS {
	case "darwin":
		if !strings.HasSuffix(why, "use sync") {
			t.Fatalf("macOS serve: %q", why)
		}
	case "linux":
		ok := gitAttrSource() && finalizeBwrap() != "" && claudePinOK(s.cfgDir, t.Logf) // #55: git 2.40+
		if (why == "") != ok || why != "" && !strings.HasSuffix(why, "use sync") {
			t.Fatalf("linux (bwrap ok %v): %q", ok, why)
		}
		t.Logf("bwrap ok %v: %q", ok, why)
	}
}

// #55: git pred 2.40 (brez GIT_ATTR_SOURCE) ugnezdenih ne začne, z razlogom (način se ne spremeni tiho).
func TestNestedBlockerOldGit(t *testing.T) {
	s, _ := testServer(t, nil)
	old := gitAttrSource
	defer func() { gitAttrSource = old }()
	gitAttrSource = func() bool { return false }
	if why := s.nestedBlocker("codex"); why != "nested git repositories need git 2.40 or later on the gateway; update git or use sync" {
		t.Fatalf("old git: %q", why)
	}
}

// TMPDIR Claude CLI v klonu: kratek (Claude Code na Linux v njem odpre vtičnice peskovnika, največ 108 bajtov; pri 80
// znakih že odpove), le za ta tek, 0700, po teku pobrisan brez sledenja povezavam.
func TestRunTempDir(t *testing.T) {
	keep := filepath.Join(t.TempDir(), "keep")
	if runtime.GOOS != "windows" {
		t.Setenv("TMPDIR", "/tmp/"+strings.Repeat("x", 80)) // dolg TMPDIR prehoda: koren gre v /tmp
	}
	s := &server{}
	root := s.tempRoot()
	if root == "" {
		t.Fatal("no temp root")
	}
	defer os.RemoveAll(root)
	dir, done, err := runTempDir(root)
	if err != nil {
		t.Fatal(err)
	}
	other, done2, _ := runTempDir(root)
	defer done2()
	if len(dir) > maxRunTemp || filepath.Dir(dir) != root || dir == other {
		t.Fatalf("run temp %q (root %q, other %q)", dir, root, other)
	}
	if st, err := os.Stat(dir); err != nil || runtime.GOOS != "windows" && st.Mode().Perm() != 0o700 {
		t.Fatalf("mode: %v %v", st, err)
	}
	os.WriteFile(keep, []byte("x"), 0o600)
	os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o600)
	os.Symlink(filepath.Dir(keep), filepath.Join(dir, "link"))
	done()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("run temp not removed")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatal("removal followed a symlink")
	}
}

// Brez korena prehoda ni TMPDIR teka (ne v skupni začasni mapi sistema).
func TestRunTempDirNoRoot(t *testing.T) {
	if dir, _, err := runTempDir(""); err == nil {
		os.RemoveAll(dir)
		t.Fatal("run temp without a root")
	}
}

// Koren prehoda, ki ga je pobrisal čistilec /tmp, se naredi znova; podtaknjene mape z istim imenom (druga dovoljenja,
// povezava) prehod ne uporabi.
func TestTempRootRefreshed(t *testing.T) {
	s := &server{}
	r1 := s.tempRoot()
	os.RemoveAll(r1)
	r2 := s.tempRoot()
	defer os.RemoveAll(r2)
	if st, err := os.Lstat(r2); r2 == "" || r2 == r1 || err != nil || !st.IsDir() {
		t.Fatalf("root deleted: not recreated: %q → %q (%v)", r1, r2, err)
	}
	if runtime.GOOS == "windows" {
		return
	}
	os.RemoveAll(r2)
	os.Mkdir(r2, 0o755) // podtaknjena: druga dovoljenja
	defer os.RemoveAll(r2)
	r3 := s.tempRoot()
	defer os.RemoveAll(r3)
	if r3 == r2 || r3 == "" {
		t.Fatalf("planted 0755 root used: %q", r3)
	}
	os.RemoveAll(r3)
	target := t.TempDir()
	os.Chmod(target, 0o700)
	os.Symlink(target, r3) // podtaknjena: povezava
	defer os.Remove(r3)
	r4 := s.tempRoot()
	defer os.RemoveAll(r4)
	if r4 == r3 || r4 == "" {
		t.Fatalf("planted symlink root used: %q", r4)
	}
}

// Ob zagonu se pobrišejo le stari koreni tega uporabnika (mape 0700 mgw-*), ne povezave, tuja dovoljenja ali novi.
func TestCleanStaleTempRoots(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("perm bits")
	}
	base, outside := t.TempDir(), t.TempDir()
	old := time.Now().Add(-48 * time.Hour)
	mk := func(name string, perm os.FileMode, age time.Time) string {
		p := filepath.Join(base, name)
		os.Mkdir(p, perm)
		os.Chmod(p, perm)
		os.WriteFile(filepath.Join(p, "f"), nil, 0o600)
		os.Chtimes(p, age, age)
		return p
	}
	stale, open, fresh, other := mk("mgw-1", 0o700, old), mk("mgw-2", 0o755, old), mk("mgw-3", 0o700, time.Now()), mk("x-4", 0o700, old)
	os.WriteFile(filepath.Join(outside, "keep"), nil, 0o600)
	link := filepath.Join(base, "mgw-5")
	os.Symlink(outside, link)
	os.Chtimes(outside, old, old)
	cleanStaleTempRoots(base)
	if _, err := os.Lstat(stale); !os.IsNotExist(err) {
		t.Fatal("stale root kept")
	}
	for _, p := range []string{open, fresh, other, link, filepath.Join(outside, "keep")} {
		if _, err := os.Lstat(p); err != nil {
			t.Fatalf("%s removed", p)
		}
	}
}

// #67 (prej #56): koreni tempRoot niso v denyRead (v njih so le povezave runTempLink; skrit koren bi v peskovniku skril
// povezavo TMPDIR teka); podatki tekov so v $MINIGW_DEPS/tmp pod Scratch, ki je v denyRead.
func TestAgentArgsTempRootsVisibleScratchHidden(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no sandbox settings")
	}
	s, _ := testServer(t, nil)
	r := s.tempRoot()
	defer os.RemoveAll(r)
	args := strings.Join(s.agentArgs(t.TempDir(), false), " ")
	if strings.Contains(args, strconv.Quote(r)) || !strings.Contains(args, strconv.Quote(s.conf().Scratch)) {
		t.Fatalf("denyRead: %s", args)
	}
}

// #54: bundle odjemalca z drevesom ».git« zrcalo zavrne tudi na git pred 2.46 (fetch iz bundla tam ne preveri objektov);
// ref base ne ostane, tudi ne pozneje brez bundla. Isti posnetek pod drugim base (brez novih objektov) gre.
func TestImportBaseFsck(t *testing.T) {
	root := t.TempDir()
	client, repo := filepath.Join(root, "c"), filepath.Join(root, "repos", "d", "r.git")
	os.MkdirAll(client, 0o755)
	gitT(t, client, "init", "-q", "-b", "main")
	writeT(t, filepath.Join(client, "a.txt"), "a\n")
	gitT(t, client, "add", "a.txt")
	gitT(t, client, "commit", "-qm", "first")
	head := gitT(t, client, "rev-parse", "HEAD")
	mk := func(in string) string {
		c := exec.Command("git", "-C", client, "mktree")
		c.Stdin = strings.NewReader(in)
		out, err := c.Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	blob := gitT(t, client, "hash-object", "-w", "--stdin")
	evil := gitT(t, client, "commit-tree", mk("040000 tree "+mk("100644 blob "+blob+"\tpost-checkout\n")+"\t.git\n"), "-p", head, "-m", "dot git")
	bundle := func(name, tip string) string {
		b := filepath.Join(root, name)
		gitT(t, client, "update-ref", "refs/minigw/push", tip)
		gitT(t, client, "bundle", "create", b, "refs/minigw/push")
		return b
	}
	if ensureMirror(repo) != nil {
		t.Fatal("mirror")
	}
	if err := importBase(repo, bundle("evil.bundle", evil), evil, "refs/minigw/base/e"); err == nil {
		t.Fatal("a tree with .git was imported")
	} else {
		t.Log(err)
	}
	if got, _ := gitRun(repo, "", nil, "--git-dir", repo, "rev-parse", "-q", "--verify", "refs/minigw/base/e"); got != "" {
		t.Fatalf("rejected base kept: %s", got)
	}
	// posnetek brez bundla, ki kaže na objekte zavrnjenega bundla (stari git jih pusti v zrcalu)
	if importBase(repo, "", evil, "refs/minigw/base/e2") == nil {
		t.Fatal("a rejected snapshot was accepted without a bundle")
	}
	if got, _ := gitRun(repo, "", nil, "--git-dir", repo, "rev-parse", "-q", "--verify", "refs/minigw/base/e2"); got != "" {
		t.Fatalf("rejected base kept: %s", got)
	}
	b := bundle("good.bundle", head)
	if importBase(repo, b, head, "refs/minigw/base/x") != nil || importBase(repo, b, head, "refs/minigw/base/y") != nil {
		t.Fatal("good bundle refused")
	}
}

// L6: zavrnjen ugnezdeni (premaknjen, večji od 64 MB, unconfined) ne ustavi vrhnjega: vrhnja veja pride (brez vsebine
// zavrnjenega), ena vrstica pove, kateri in zakaj, klon z delom po koncu seje ostane (kept).
func TestWorktreeNestedSkippedTopDelivered(t *testing.T) {
	for _, c := range []struct{ name, agent, why string }{
		// Linux s pin: .git je točka priklopa (ostane prazna mapa), zato »could not save« namesto »moved«.
		{"moved", `rm -rf lib/.git`, `(?:the nested repository lib was moved or replaced in the agent's clone|the gateway could not save the agent's work in the nested repository lib as a branch)`},
		{"big", `head -c 70000000 /dev/urandom > lib/big.bin`, `the agent's branch in the nested repository lib is larger than 64 MB`},
		{"unconfined", `:`, `the nested repository lib does not come back on this gateway`},
		{"toolarge", `mkdir many && (cd many && seq 1 100001 | xargs touch)`, ``}, // M1: prevelik klon se ne ohrani
	} {
		t.Run(c.name, func(t *testing.T) { testNestedSkipped(t, c.agent, c.why, c.name) })
	}
}

func testNestedSkipped(t *testing.T, agent, why, kind string) {
	unconfined := kind == "unconfined"
	if runtime.GOOS == "windows" {
		t.Skip("fake claude is a shell script")
	}
	if !gitAttrSource() {
		t.Skip("git before 2.40: the client refuses nested repositories")
	}
	if runtime.GOOS == "linux" && finalizeBwrap() == "" {
		t.Skip("Linux without bwrap: nested repositories are refused")
	}
	root := t.TempDir()
	client := filepath.Join(root, "client")
	for _, d := range []string{client, filepath.Join(client, "lib"), filepath.Join(client, "other")} {
		os.MkdirAll(d, 0o755)
		gitT(t, d, "init", "-q", "-b", "main")
		writeT(t, filepath.Join(d, "f.txt"), "f\n")
		gitT(t, d, "add", "f.txt")
		gitT(t, d, "commit", "-qm", "first")
	}
	fake := filepath.Join(root, "claude")
	writeT(t, fake, `#!/bin/sh
`+cliGuard+`
cat > /dev/null
for p in . lib other; do echo "agent" >> "$p/agent.txt"; done
[ -f `+root+`/second ] && { `+agent+`; }
echo '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"done"}}}'
echo '{"type":"result","subtype":"success","usage":{"output_tokens":1}}'
`)
	os.Chmod(fake, 0o755)
	s, _ := testServer(t, func(c *serveConfig) { c.Claude = fake })
	s.hub.nestedBlocker = func(string) string { return "" }
	c := startLoopback(t, s)
	sc := &shareClient{c: c, session: "sidk", root: client, worktree: true, top: filepath.ToSlash(client), synced: map[string]syncEntry{}, reported: map[string]bool{}}
	if !sc.register() {
		t.Fatal("register failed: " + sc.refusal)
	}
	sc.start(2)
	defer sc.unregister()
	run := func(ok bool) string {
		stderr := filepath.Join(root, "stderr")
		f, _ := os.Create(stderr)
		old := os.Stderr
		os.Stderr = f
		body := `{"model":"claude-cli-haiku","tools":[{"name":"x"}],"messages":[{"role":"user","content":"go"}]}`
		h := map[string]string{"x-claude-mini-session": "sidk", "x-claude-mini-cwd": client, "content-type": "application/json"}
		st, data := call(c, "POST", "/v1/messages", h, []byte(body), 120*time.Second)
		os.Stderr = old
		f.Close()
		out, _ := os.ReadFile(stderr)
		if ok && st != 200 {
			t.Fatalf("messages: %d %s\n%s", st, data, out)
		}
		return string(out)
	}
	branch := "minigw/claude-" + sha256Hex([]byte("sidk"))[:8] + "-0"
	run(true) // prvi tek: vse pride, klon ostane za naslednjo zahtevo
	if !strings.Contains(gitT(t, filepath.Join(client, "lib"), "branch", "--list", branch), branch) {
		t.Fatal("first run: lib branch not delivered")
	}
	writeT(t, filepath.Join(root, "second"), "")
	s.hub.mu.Lock()
	var slot *wtSlot
	var tree *shareTree
	for _, sh := range s.hub.bySession {
		if sh.tree != nil {
			tree, slot = sh.tree, sh.tree.slots["claude"]
		}
	}
	if slot != nil {
		slot.unconfined = unconfined // obramba v globino: oznaka na klonu z ugnezdenimi (markUnconfined jo zavrne že ob zagonu)
	}
	s.hub.mu.Unlock()
	if slot == nil {
		t.Fatal("no slot")
	}
	libTip := gitT(t, filepath.Join(client, "lib"), "rev-parse", branch)
	topTip := gitT(t, client, "rev-parse", branch)
	out := run(false) // moved: nadzor teka agenta ustavi (zamenjan .git), delo vrhnjega vseeno pride
	if kind == "toolarge" {
		if !strings.Contains(out, "the agent's clone is too large to return") || strings.Contains(out, "kept on the gateway") {
			t.Fatalf("too large clone:\n%s", out)
		}
		sc.unregister()
		if k, _ := filepath.Glob(filepath.Join(tree.worktrees, "*-kept-*")); len(k) != 0 {
			t.Fatalf("too large clone kept: %v", k)
		}
		return
	}
	line := regexp.MustCompile(`(?m)^minigw: gateway: worktree: ` + why + `.*; the agent's files are kept on the gateway for 30 days as (worktrees/\S+-kept-\d{8}-\d{6}-\d{9}) after this session \(files only; its \.git folders renamed \.git\.agent, or nothing is kept if that fails — do not run git there\)$`).FindStringSubmatch(out)
	if line == nil {
		t.Fatalf("no skip line:\n%s", out)
	}
	scratch := filepath.Dir(filepath.Dir(filepath.Dir(tree.worktrees)))
	line[1] = filepath.Join(scratch, filepath.FromSlash(line[1])) // L3: pot v sporočilu je glede na Scratch
	if got := gitT(t, client, "rev-parse", branch); got == topTip || !strings.Contains(gitT(t, client, "show", branch+":agent.txt"), "agent\nagent") {
		t.Fatalf("top level not delivered:\n%s", out)
	}
	if files := gitT(t, client, "ls-tree", "-r", "--name-only", branch); strings.Contains(files, "lib/") || strings.Contains(files, "other/") {
		t.Fatalf("top branch has nested files: %s", files)
	}
	if got := gitT(t, filepath.Join(client, "lib"), "rev-parse", branch); got != libTip {
		t.Fatal("refused nested repository delivered")
	}
	if !unconfined && !strings.Contains(gitT(t, filepath.Join(client, "other"), "show", branch+":agent.txt"), "agent\nagent") {
		t.Fatalf("other nested repository not delivered:\n%s", out)
	}
	if unconfined { // naslednji čist tek dostavi zavrnjeno delo; šele tedaj kept ni več
		s.hub.mu.Lock()
		kept := slot.kept
		slot.unconfined = false
		s.hub.mu.Unlock()
		os.Remove(filepath.Join(root, "second"))
		if filepath.Join(filepath.Dir(slot.wt), filepath.Base(line[1])) != kept {
			t.Fatalf("kept %q, line %q", kept, line[1])
		}
		run(true)
		if got := gitT(t, filepath.Join(client, "lib"), "show", branch+":agent.txt"); got != "agent\nagent\nagent" {
			t.Fatalf("refused work not delivered by the next run: %q", got)
		}
		s.hub.mu.Lock()
		kept = slot.kept
		s.hub.mu.Unlock()
		sc.unregister()
		if _, err := os.Stat(line[1]); kept != "" || err == nil {
			t.Fatalf("clone kept after everything was delivered: %q", kept)
		}
		return
	}
	if kind == "moved" { // L1: naslednja zahteva naredi nov klon na isti poti; nedostavljeno delo prej v kept
		run(false)
		if b, err := os.ReadFile(filepath.Join(line[1], "lib", "agent.txt")); err != nil || string(b) != "agent\nagent\n" {
			t.Fatalf("kept clone before the new clone %s: %v %q", line[1], err, b)
		}
	}
	// M2: config agenta s fsmonitor v .git klona (vrhnji in ugnezdeni) ob ohranitvi ne sme več teči
	marker := filepath.Join(root, "fsmonitor-ran")
	for _, d := range []string{slot.wt, filepath.Join(slot.wt, "lib")} {
		if _, err := os.Stat(filepath.Join(d, ".git")); err == nil {
			gitT(t, d, "config", "core.fsmonitor", "touch "+marker+"; false")
		}
	}
	sc.unregister() // konec seje: klon takoj v kept (L1), razorožitev v ozadju
	if _, err := os.Stat(slot.wt); err == nil {
		t.Fatal("clone not moved to kept when the session ended")
	}
	waitFor(t, func() bool { _, err := os.Lstat(filepath.Join(line[1], ".git.agent")); return err == nil })
	for _, d := range []string{line[1], filepath.Join(line[1], "lib"), filepath.Join(line[1], "other")} {
		if _, err := os.Lstat(filepath.Join(d, ".git")); err == nil {
			t.Fatalf("%s/.git kept", d)
		}
		c := exec.Command("git", "-C", d, "status")
		c.Env = append(os.Environ(), "GIT_CEILING_DIRECTORIES="+filepath.Dir(line[1]))
		if out, err := c.CombinedOutput(); err == nil {
			t.Fatalf("git finds a repository in the kept clone %s: %s", d, out)
		}
	}
	if _, err := os.Stat(filepath.Join(line[1], ".git.agent")); err != nil {
		t.Fatal("no .git.agent in the kept clone")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the agent's fsmonitor ran")
	}
	if b, err := os.ReadFile(filepath.Join(line[1], "lib", "agent.txt")); err != nil || string(b) != "agent\nagent\n" {
		t.Fatalf("kept clone %s: %v %q", line[1], err, b)
	}
	if _, err := os.Stat(slot.wt); err == nil {
		t.Fatal("clone left at its working path")
	}
	// Zrcalo po 30 dneh gre, ohranjeni klon (mlajši od 30 dni) ne; po svojih 30 dneh gre tudi on.
	later := time.Now().Add(20 * 24 * time.Hour)
	os.Chtimes(line[1], later, later)
	pruneMirrors(scratch, time.Now().Add(31*24*time.Hour))
	if _, err := os.Stat(line[1]); err != nil {
		t.Fatal("kept clone removed with its mirror before its 30 days")
	}
	if _, err := os.Stat(tree.repo); err == nil {
		t.Fatal("mirror not pruned")
	}
	pruneMirrors(scratch, time.Now().Add(51*24*time.Hour))
	if _, err := os.Stat(line[1]); err == nil {
		t.Fatal("kept clone not pruned after 30 days")
	}
}

// L6 (N1): agent brez svojega imenskega prostora PID v klonu z ugnezdenimi ne teče (le ta zahteva); brez ugnezdenih oznaka.
func TestMarkUnconfinedRefusesNested(t *testing.T) {
	h := &shareHub{bySession: map[string]*serveShare{}}
	a, b := &wtSlot{wt: "/w/a", nested: []*wtNested{{path: "lib"}}}, &wtSlot{wt: "/w/b"}
	h.bySession["s"] = &serveShare{tree: &shareTree{slots: map[string]*wtSlot{"claude": a, "codex": b}}}
	if why := h.markUnconfined("/w/a", "codex"); !strings.Contains(why, "this request did not run; use sync") || a.unconfined {
		t.Fatalf("%q %v", why, a.unconfined)
	}
	if why := h.markUnconfined("/w/b", "codex"); why != "" || !b.unconfined {
		t.Fatalf("%q %v", why, b.unconfined)
	}
}

// L6: Codex (in Grok, Copilot) brez svojega imenskega prostora PID v klonu z ugnezdenimi: končna zavrnitev 400 pred
// zagonom, besedilo kot pri Claude CLI (sicer 502 prek classifyAgentError in Claude Code ponavlja).
func TestCodexUnconfinedNestedRefused(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake codex is a shell script")
	}
	if !gitAttrSource() {
		t.Skip("git before 2.40: the client refuses nested repositories")
	}
	root := t.TempDir()
	client := filepath.Join(root, "client")
	for _, d := range []string{client, filepath.Join(client, "lib")} {
		os.MkdirAll(d, 0o755)
		gitT(t, d, "init", "-q", "-b", "main")
		writeT(t, filepath.Join(d, "f.txt"), "f\n")
		gitT(t, d, "add", "f.txt")
		gitT(t, d, "commit", "-qm", "first")
	}
	codex := filepath.Join(root, "codex")
	writeT(t, codex, "#!/bin/sh\ntouch "+root+"/started\nexit 1\n")
	os.Chmod(codex, 0o755)
	old := agentUnconfined
	agentUnconfined = func(string) bool { return true }
	defer func() { agentUnconfined = old }()
	s, _ := testServer(t, func(c *serveConfig) {
		c.AllowUnsandboxedBash = true
		t.Setenv("CODEX_HOME", t.TempDir())
		c.Codex = agentCLI{Enabled: true, Executable: codex}
	})
	s.hub.nestedBlocker = func(string) string { return "" }
	c := startLoopback(t, s)
	sc := &shareClient{c: c, session: "sidu", root: client, worktree: true, top: filepath.ToSlash(client), synced: map[string]syncEntry{}, reported: map[string]bool{}}
	if !sc.register() {
		t.Fatal("register failed: " + sc.refusal)
	}
	sc.start(2)
	defer sc.unregister()
	h := map[string]string{"x-claude-mini-session": "sidu", "x-claude-mini-cwd": client, "content-type": "application/json"}
	body := `{"model":"claude-codex","tools":[{"name":"x"}],"messages":[{"role":"user","content":"go"}]}`
	st, data := call(c, "POST", "/v1/messages", h, []byte(body), 60*time.Second)
	want := "nested repositories need bwrap for codex on this Linux gateway (its own PID namespace); this request did not run; use sync"
	if _, err := os.Stat(filepath.Join(root, "started")); err == nil || st != 400 || !strings.Contains(string(data), want) {
		t.Fatalf("not refused before the agent: %d %s", st, data)
	}
}

// M1, L2: največ maxKept ohranjenih klonov (najstarejši gredo), starejši od meje gredo; povezava se ne sledi.
func TestPruneKept(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	writeT(t, filepath.Join(outside, "x-kept-1", "f"), "x")
	os.Symlink(filepath.Join(outside, "x-kept-1"), filepath.Join(dir, "link-kept-0"))
	now := time.Now()
	for i := 1; i <= 5; i++ {
		d := filepath.Join(dir, fmt.Sprintf("c-kept-%d", i))
		writeT(t, filepath.Join(d, "f"), "x")
		ts := now.Add(time.Duration(i) * time.Hour)
		os.Chtimes(d, ts, ts)
	}
	writeT(t, filepath.Join(dir, "c-0", "f"), "x") // klon, ki ni ohranjen
	pruneKept(dir, time.Time{})
	var left []string
	es, _ := os.ReadDir(dir)
	for _, e := range es {
		left = append(left, e.Name())
	}
	if strings.Join(left, ",") != "c-0,c-kept-3,c-kept-4,c-kept-5,link-kept-0" {
		t.Fatalf("left %v", left)
	}
	pruneKept(dir, now.Add(4*time.Hour))
	if _, err := os.Stat(filepath.Join(dir, "c-kept-3")); err == nil {
		t.Fatal("older than the cut kept")
	}
	if _, err := os.Stat(filepath.Join(outside, "x-kept-1", "f")); err != nil {
		t.Fatal("link followed")
	}
}

// M2, #4, #1: keepClone preimenuje vsak .git in HEAD golega repozitorija v klonu, nikoli prek povezave ven; klon, ki je
// povezava, ostane, kot je.
func TestKeepCloneDisarmsGit(t *testing.T) {
	root := t.TempDir()
	wt, outside, kept := filepath.Join(root, "c-0"), filepath.Join(root, "outside"), filepath.Join(root, "c-0-kept-1")
	gitT(t, root, "init", "-q", wt)
	gitT(t, root, "init", "-q", "--bare", filepath.Join(wt, "bare"))
	gitT(t, root, "init", "-q", outside)
	gitT(t, root, "init", "-q", "--bare", filepath.Join(outside, "b"))
	writeT(t, filepath.Join(wt, "sub", ".git"), "gitdir: ../x\n")
	writeT(t, filepath.Join(wt, "f.txt"), "agent\n")
	gitT(t, root, "init", "-q", filepath.Join(wt, "ro", "r"))
	t.Cleanup(func() { chmodTree(kept) })
	os.Chmod(filepath.Join(wt, "ro", "r"), 0o555) // L2: agent vzame pravice mapi
	os.Chmod(filepath.Join(wt, "ro"), 0o555)
	os.Symlink(outside, filepath.Join(wt, "link"))
	os.Symlink(filepath.Join(outside, "b"), filepath.Join(wt, "blink"))
	keepClone(wt, kept, t.Logf)
	if _, err := os.Lstat(wt); err == nil { // L1: premik takoj
		t.Fatal("clone still at its path after keepClone")
	}
	want := []string{".git.agent", "sub/.git.agent", "bare/HEAD.agent", "ro/r/.git.agent", "f.txt"}
	waitFor(t, func() bool {
		for _, p := range want {
			if _, err := os.Lstat(filepath.Join(kept, p)); err != nil {
				return false
			}
		}
		return true
	})
	for _, p := range []string{".git", "sub/.git", "bare/HEAD", "ro/r/.git"} {
		if _, err := os.Lstat(filepath.Join(kept, p)); err == nil {
			t.Fatalf("%s left", p)
		}
	}
	for _, d := range []string{kept, filepath.Join(kept, "bare"), filepath.Join(kept, "sub")} {
		c := exec.Command("git", "-C", d, "rev-parse", "--git-dir")
		c.Env = append(os.Environ(), "GIT_CEILING_DIRECTORIES="+root)
		if out, err := c.CombinedOutput(); err == nil {
			t.Fatalf("git finds a repository in %s: %s", d, out)
		}
	}
	for _, p := range []string{".git", "b/HEAD"} {
		if _, err := os.Stat(filepath.Join(outside, p)); err != nil {
			t.Fatalf("outside %s touched: %v", p, err)
		}
	}
	os.Symlink(outside, filepath.Join(root, "c-1")) // klon je povezava: nič
	keepClone(filepath.Join(root, "c-1"), filepath.Join(root, "c-1-kept-1"), t.Logf)
	if _, err := os.Stat(filepath.Join(outside, ".git")); err != nil {
		t.Fatal("outside .git touched through a linked clone")
	}
	if _, err := os.Lstat(filepath.Join(root, "c-1-kept-1")); err == nil {
		t.Fatal("linked clone kept")
	}
}

// #40: »git« na PATH agenta dela v registriranem klonu, ki vsebuje mapo ali -C <pot> (najdaljši), drugače ostane okolje
// (vrhnji klon); ugnezden .git, ki ga minigw ni registriral, ne velja; predpona /x/a ni /x/ab. Besedilo je enako kot na
// Macu (testdata/pathwrap.golden, Worktree.pathWrapScript).
func TestPathWrapScript(t *testing.T) {
	golden, err := os.ReadFile(filepath.Join("testdata", "pathwrap.golden"))
	if err != nil || string(golden) != pathWrapScript("/REAL/git") {
		t.Fatalf("pathWrapScript differs from testdata/pathwrap.golden (%v)", err)
	}
	if runtime.GOOS == "windows" {
		t.Skip("no PATH wrapper on Windows")
	}
	root := filepath.Join(t.TempDir(), "a b'c")
	real := filepath.Join(root, "real", "git")
	writeT(t, real, "#!/bin/sh\necho \"$GIT_DIR|$GIT_WORK_TREE|$GIT_COMMON_DIR|$*\"\n")
	os.Chmod(real, 0o755)
	wrap := filepath.Join(root, "bin", "git")
	writeT(t, wrap, pathWrapScript(real))
	os.Chmod(wrap, 0o755)
	top, lib, ab := filepath.Join(root, "w"), filepath.Join(root, "w", "lib"), filepath.Join(root, "x", "ab")
	for _, d := range []string{top, lib, filepath.Join(lib, "inner"), filepath.Join(root, "x", "a"), ab} {
		os.MkdirAll(filepath.Join(d, ".git"), 0o755)
	}
	os.MkdirAll(filepath.Join(lib, "src"), 0o755)
	reg := strings.Join([]string{top + "/.git", lib + "/.git", root + "/x/a/.git"}, "\n")
	harness := top + "/.git|" + top + "|" + top + "/.git"
	in := func(p string) string { return p + "/.git|" + p + "|" + p + "/.git" }
	for _, c := range []struct {
		cwd  string
		args []string
		want string
	}{
		{top, []string{"status"}, in(top)},
		{filepath.Join(lib, "src"), []string{"status"}, in(lib)},
		{top, []string{"-C", "lib", "commit", "-m", "x"}, in(lib)},
		{top, []string{"-c", "a=b", "--no-pager", "-C", "lib", "status"}, in(lib)},
		{top, []string{"-C", "lib", "-C", "src", "status"}, in(lib)},
		{top, []string{"-C", lib + "/..", "status"}, in(top)},
		{top, []string{"status", "-C", "lib"}, in(top)}, // -C le pred ukazom
		{filepath.Join(lib, "inner"), []string{"status"}, in(lib)},
		{ab, []string{"status"}, harness},
		{root, []string{"status"}, harness},
		{top, []string{"-C", "missing", "status"}, harness},
	} {
		cmd := exec.Command(wrap, c.args...)
		cmd.Dir, cmd.Env = c.cwd, append(os.Environ(), "MINIGW_GIT_DIRS="+reg) // #40: brez GIT_DIR v okolju; brez ujemanja vrhnji
		out, err := cmd.Output()
		if got := strings.TrimSpace(string(out)); err != nil || got != c.want+"|"+strings.Join(c.args, " ") {
			t.Errorf("%s: git %v: %v %q, want %q", c.cwd, c.args, err, got, c.want)
		}
	}
}

// #40: okolje agenta brez lastnikovih nastavitev git (globalne, sistemske, izključitve), avtor commitov je agent;
// vrstni red parov je enak kot v Worktree.harnessEnv na Macu.
func TestWorktreeHarnessEnvGitConfig(t *testing.T) {
	env := map[string]string{}
	for _, kv := range worktreeHarnessEnv("/w", "/m", "/t", "codex") {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	want := []string{"core.fsmonitor=false", "core.hooksPath=" + os.DevNull, "core.attributesFile=" + os.DevNull, "diff.ignoreSubmodules=all",
		"submodule.recurse=false", "core.excludesFile=" + os.DevNull, "user.name=Codex (Mini Gateway)", "user.email=codex@minigw.invalid", "safe.bareRepository=explicit",
		map[bool]string{false: "init.templateDir=", true: "core.longpaths=true"}[gitLongPaths]} // #67
	var got []string
	for i := range want {
		got = append(got, env["GIT_CONFIG_KEY_"+strconv.Itoa(i)]+"="+env["GIT_CONFIG_VALUE_"+strconv.Itoa(i)])
	}
	if env["GIT_CONFIG_GLOBAL"] != os.DevNull || env["GIT_CONFIG_NOSYSTEM"] != "1" || env["GIT_CONFIG_COUNT"] != "10" || !slices.Equal(got, want) ||
		env["MINIGW_DEPS"] != "/w.deps" || env["CLAUDE_CODE_TMPDIR"] != filepath.Join("/w.deps", "tmp") || env["GOMODCACHE"] != filepath.Join("/.cache", "go-mod") { // #67
		t.Fatalf("git config env: %v", env)
	}
	if runtime.GOOS == "windows" {
		return
	}
	gw := filepath.Join(t.TempDir(), "gitexec")
	t.Setenv("PATH", ":/usr/bin::.:bin:./x:/bin:")
	path := ""
	for _, kv := range worktreeClaudeEnv("/w", "/m", "/t", gw, []string{"/w/.git"}, "claude") {
		if p, ok := strings.CutPrefix(kv, "PATH="); ok {
			path = p
		}
		if k, _, _ := strings.Cut(kv, "="); k == "GIT_DIR" || k == "GIT_WORK_TREE" || k == "GIT_COMMON_DIR" { // #40
			t.Fatalf("Claude env exports %s", kv)
		}
	}
	codex := strings.Join(worktreeClaudeEnv("/w", "/m", "/t", gw, []string{"/w/.git"}, "codex"), "\n")
	if !strings.Contains(codex, "GIT_DIR="+os.DevNull) || !strings.Contains(codex, "GIT_COMMON_DIR="+os.DevNull) || !strings.Contains(codex, "MINIGW_CLONE=/w") ||
		strings.Contains("\n"+codex, "\nPATH=") { // M2: harness Codex ne dela v klonu
		t.Fatalf("Codex env: %s", codex)
	}
	if path != filepath.Join(gw, "bin")+":/usr/bin:/bin" {
		t.Fatalf("PATH without the git wrapper first: %q", path)
	}
	if b, err := os.ReadFile(filepath.Join(gw, "bin", "git")); err != nil || !strings.Contains(string(b), "MINIGW_GIT_DIRS") {
		t.Fatalf("no PATH wrapper: %v", err)
	}
}

// A3: .build klona (TMPDIR agenta) ni v git status agenta (makeClone: info/exclude).
func TestCloneExcludesBuild(t *testing.T) {
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
	if ensureMirror(repo) != nil || importBase(repo, b, head, "refs/minigw/base/x") != nil || makeClone(repo, wt, "minigw/claude-12345678-0", head) != nil {
		t.Fatal("mirror or clone")
	}
	writeT(t, filepath.Join(wt, ".build", "tmp", "x"), "x\n")
	if st := gitT(t, wt, "status", "--porcelain"); st != "" {
		t.Fatalf("clone status: %q", st)
	}
}

// waitFor: ok v 5 s (razorožitev ohranjenega klona teče v ozadju).
func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	for i := 0; i < 100 && !ok(); i++ {
		time.Sleep(50 * time.Millisecond)
	}
	if !ok() {
		t.Fatal("timed out")
	}
}

// L2, L4: zamenjan koren (povezava) se ne razoroži in ne sledi; mapa brez pisanja dobi 0700 in uspe.
func TestDisarmKeptFailsClosed(t *testing.T) {
	root := t.TempDir()
	kept := filepath.Join(root, "c-0-kept-1")
	gitT(t, root, "init", "-q", kept)
	os.Symlink(kept, filepath.Join(root, "l-kept-1"))
	if disarmKept(filepath.Join(root, "l-kept-1")) {
		t.Fatal("linked kept clone disarmed")
	}
	if _, err := os.Lstat(filepath.Join(kept, ".git")); err != nil {
		t.Fatal("link target touched")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores folder permissions")
	}
	t.Cleanup(func() { chmodTree(kept) })
	os.Chmod(kept, 0o500)
	if !disarmKept(kept) { // mapa brez pisanja: 0700 in znova uspe
		t.Fatal("folder without write permission not disarmed")
	}
}

// chmodTree: pravice za pospravljanje (mape 555 v testu).
func chmodTree(dir string) {
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			os.Chmod(p, 0o755)
		}
		return nil
	})
}

// #40 M2: pinPaths: .git vseh klonov in mape do ugnezdenih, predniki pred potomci, brez korena klona.
func TestPinPaths(t *testing.T) {
	wt := filepath.Join("/w", "c")
	got := pinPaths(wt, []string{filepath.Join(wt, ".git"), filepath.Join(wt, "a", "b", ".git"), filepath.Join(wt, "a", "x", ".git")})
	want := []string{filepath.Join(wt, ".git"), filepath.Join(wt, "a"), filepath.Join(wt, "a", "b"), filepath.Join(wt, "a", "b", ".git"),
		filepath.Join(wt, "a", "x"), filepath.Join(wt, "a", "x", ".git")}
	if !slices.Equal(got, want) {
		t.Fatalf("pinPaths: %v", got)
	}
}

// #40: več kot maxPins točk priklopa (globoki ugnezdeni kloni) = zavrnitev.
func TestTooDeep(t *testing.T) {
	wt := filepath.Join("/w", "c")
	gits, d := []string{filepath.Join(wt, ".git")}, wt
	for i := 0; i < 255; i++ { // 1 + 255 × (mapa + .git) = 511
		d = filepath.Join(d, "a")
		gits = append(gits, filepath.Join(d, ".git"))
	}
	if tooDeep(wt, gits) {
		t.Fatal("511 pins refused")
	}
	if !tooDeep(wt, append(gits, filepath.Join(d, "b", "x", ".git"))) {
		t.Fatal("514 pins accepted")
	}
}

// #40: ugnezdeni klon, katerega pot je zamenjana s povezavo na isti .git, prestane gitIntact, nestedIntact pa ne:
// klon se ne uporabi znova.
func TestSlotNestedIntact(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks")
	}
	wt := t.TempDir()
	os.MkdirAll(filepath.Join(wt, ".git"), 0o700)
	os.MkdirAll(filepath.Join(wt, "a", "lib", ".git"), 0o700)
	writeT(t, filepath.Join(wt, ".git", "config"), "c")
	writeT(t, filepath.Join(wt, "a", "lib", ".git", "config"), "n")
	n := &wtNested{path: "a/lib", cfg: []byte("n")}
	n.git, _ = os.Lstat(filepath.Join(wt, "a", "lib", ".git"))
	sl := &wtSlot{wt: wt, cfg: []byte("c"), nested: []*wtNested{n}}
	sl.git, _ = os.Lstat(filepath.Join(wt, ".git"))
	if !sl.gitIntact() || !sl.nestedIntact() {
		t.Fatal("fresh slot refused")
	}
	if os.Rename(filepath.Join(wt, "a"), filepath.Join(wt, "b")) != nil || os.Symlink("b", filepath.Join(wt, "a")) != nil {
		t.Fatal("swap")
	}
	if !sl.gitIntact() || sl.nestedIntact() {
		t.Fatalf("linked path: gitIntact %v, nestedIntact %v", sl.gitIntact(), sl.nestedIntact())
	}
}

// Poročilo Windows e1f30bb (Air → HPX360): 502 »Cloning … failed« brez vrstice v dnevniku. Vsak neuspel korak
// worktreeMount zapiše korak in razlog gita (tu: odgovor minigw ni 200; klon v mapo, ki je ni mogoče narediti).
func TestWorktreeMountLogsEveryFailure(t *testing.T) {
	root := t.TempDir()
	client := filepath.Join(root, "c")
	os.MkdirAll(client, 0o755)
	gitT(t, client, "init", "-q", "-b", "main")
	writeT(t, filepath.Join(client, "a.txt"), "a\n")
	gitT(t, client, "add", "a.txt")
	gitT(t, client, "commit", "-qm", "first")
	head := gitT(t, client, "rev-parse", "HEAD")
	b := filepath.Join(root, "push.bundle")
	gitT(t, client, "update-ref", "refs/minigw/push", head)
	gitT(t, client, "bundle", "create", "-q", b, "refs/minigw/push")
	bundle, _ := os.ReadFile(b)
	notDir := filepath.Join(root, "file")
	writeT(t, notDir, "x")
	for _, c := range []struct {
		status    int
		worktrees string
		want      string
	}{
		{500, filepath.Join(root, "wts"), "status 500"},
		{200, filepath.Join(notDir, "wts"), "cannot remove the old clone"},
	} {
		h := newShareHub()
		var logged []string
		h.logf = func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) }
		tree := &shareTree{repo: filepath.Join(root, "repos", "d", "r.git"), worktrees: c.worktrees, slots: map[string]*wtSlot{}}
		key, _ := h.registerTree("s", "/p", "", tree, "dev")
		go func() {
			j := <-h.bySession["s"].jobs
			if c.status == 200 && h.worktreePut(key, 0, 0, bundle, "dev") != 204 {
				t.Error("bundle upload")
			}
			h.reply(key, "dev", davReply{ID: j.ID, Response: davResponse{Status: c.status, Body: []byte(`{"snapshot":"` + head + `","nested":[]}`)}})
		}()
		if dir, _ := h.worktreeMount(h.bySession["s"], "claude"); dir != "" || len(logged) != 1 || !strings.Contains(logged[0], c.want) {
			t.Fatalf("%q: dir %q, log %q", c.want, dir, logged)
		}
		if c.status == 200 { // git's own line comes with the step
			err := makeClone(tree.repo, filepath.Join(root, "wt"), "minigw/bad..name", head)
			if err == nil || !strings.Contains(err.Error(), "git checkout of the snapshot: exit status 128: fatal:") {
				t.Fatalf("checkout error: %v", err)
			}
		}
	}
}

// Poročilo Windows 73eeb13 (Air → HPX360): klon ~132 znakov + pot posnetka 141 = 274 > MAX_PATH, checkout »Filename too
// long«. Na Windows git prehoda (gitRun) in okolje agenta nosita core.longpaths=true; drugje ne.
func TestWorktreeGitLongPaths(t *testing.T) {
	old := gitLongPaths
	t.Cleanup(func() { gitLongPaths = old })
	for _, on := range []bool{false, true} {
		gitLongPaths = on
		got, _ := gitRun(filepath.Join(t.TempDir(), "r.git"), "", nil, "config", "core.longpaths")
		env := map[string]string{}
		for _, kv := range worktreeHarnessEnv("/w", "/m", "/t", "claude") {
			k, v, _ := strings.Cut(kv, "=")
			env[k] = v
		}
		pair := env["GIT_CONFIG_COUNT"] == "10" && env["GIT_CONFIG_KEY_9"] == "core.longpaths" && env["GIT_CONFIG_VALUE_9"] == "true"
		if (got == "true") != on || pair != on || !on && env["GIT_CONFIG_KEY_9"] != "init.templateDir" { // #67: drugje par 9 init.templateDir
			t.Fatalf("longpaths %v: git %q env %v", on, got, env)
		}
	}
	if runtime.GOOS != "windows" {
		t.Skip("MAX_PATH checkout only on Windows")
	}
	gitLongPaths = true
	root := t.TempDir()
	client := filepath.Join(root, "c")
	deep := filepath.Join(client, strings.Repeat("d", 100), strings.Repeat("e", 100), strings.Repeat("f", 60)+".txt")
	os.MkdirAll(filepath.Dir(deep), 0o755)
	gitT(t, client, "init", "-q", "-b", "main")
	gitT(t, client, "config", "core.longpaths", "true")
	writeT(t, deep, "x\n")
	gitT(t, client, "add", "-A")
	gitT(t, client, "commit", "-qm", "deep")
	head := gitT(t, client, "rev-parse", "HEAD")
	b := filepath.Join(root, "push.bundle")
	gitT(t, client, "update-ref", "refs/minigw/push", head)
	gitT(t, client, "bundle", "create", "-q", b, "refs/minigw/push")
	repo, wt := filepath.Join(root, "repos", "d", "r.git"), filepath.Join(root, "worktrees", strings.Repeat("o", 43), strings.Repeat("1", 16), "claude-12345678-0")
	if ensureMirror(repo) != nil || importBase(repo, b, head, "refs/minigw/base/x") != nil {
		t.Fatal("mirror")
	}
	if err := makeClone(repo, wt, "minigw/claude-12345678-0", head); err != nil {
		t.Fatalf("long path clone: %v", err)
	}
	if _, err := os.Stat(filepath.Join(wt, strings.TrimPrefix(deep, client))); err != nil {
		t.Fatal(err)
	}
}

// #61: .git, ki ga je agent naredil v začetni mapi harnessa ali mapi nad njo (datoteka gitdir:), zavrne zagon; .git v
// prezrtem build/_deps zunaj te poti in registriran ugnezdeni klon kot začetna mapa ne; povezavi ne sledi.
func TestForeignGit(t *testing.T) {
	root := t.TempDir()
	wt, out := filepath.Join(root, "wt"), filepath.Join(root, "out")
	for _, d := range []string{".git/info", "lib/.git", "lib/src", "app/src", "build/_deps/foo/.git", "../out/x/.git"} {
		if err := os.MkdirAll(filepath.Join(wt, filepath.FromSlash(d)), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	os.WriteFile(filepath.Join(wt, ".gitignore"), []byte("build/\n"), 0o600)
	gits := []string{filepath.Join(wt, ".git"), filepath.Join(wt, "lib", ".git")}
	cwd := filepath.Join(wt, "app", "src")
	for _, d := range []string{cwd, wt, filepath.Join(wt, "lib", "src")} {
		if why := foreignGit(wt, d, gits); why != "" {
			t.Fatalf("%s refused: %s", d, why)
		}
	}
	if why := foreignGit(wt, filepath.Join(wt, "lib", "src"), gits[:1]); !strings.Contains(why, "a .git appeared at lib/.git") {
		t.Fatalf("unregistered nested .git not refused: %q", why)
	}
	os.WriteFile(filepath.Join(wt, "app", ".git"), []byte("gitdir: "+out+"/x/.git\n"), 0o600)
	if why := foreignGit(wt, cwd, gits); why != "The agent's clone was changed (a .git appeared at app/.git): start a new conversation" {
		t.Fatalf("planted .git not refused: %q", why)
	}
	if runtime.GOOS == "windows" {
		return
	}
	os.RemoveAll(filepath.Join(wt, "app"))
	if err := os.Symlink(out, filepath.Join(wt, "app")); err != nil {
		t.Fatal(err)
	}
	if why := foreignGit(wt, filepath.Join(wt, "app", "x"), gits); !strings.Contains(why, "a link at app") {
		t.Fatalf("link not refused: %q", why)
	}
}

// #61 M1: git harnessa (po absolutni poti, okolje worktreeClaudeEnv brez GIT_DIR) v mapi z golim repozitorijem agenta ga ne
// uporabi (safe.bareRepository=explicit); brez tega para ga (test ponovi napad).
func TestHarnessGitRefusesPlantedBareRepo(t *testing.T) {
	git, err := gitExe()
	if err != nil || runtime.GOOS == "windows" { // na Windows ima harness GIT_DIR klona
		t.Skip("git missing or Windows")
	}
	root := t.TempDir()
	wt := filepath.Join(root, "wt")
	os.MkdirAll(wt, 0o700)
	gitT(t, wt, "init", "-q", "-b", "main")
	gitT(t, wt, "init", "-q", "--bare", "app")
	env := worktreeClaudeEnv(wt, filepath.Join(root, "m"), t.TempDir(), filepath.Join(root, "gitexec"), []string{filepath.Join(wt, ".git")}, "claude")
	if env == nil {
		t.Fatal("no env")
	}
	gitDir := func(count string) error {
		c := exec.Command(git, "rev-parse", "--git-dir")
		c.Dir = filepath.Join(wt, "app")
		for _, kv := range os.Environ() {
			if !strings.HasPrefix(kv, "GIT_") {
				c.Env = append(c.Env, kv)
			}
		}
		c.Env = append(append(c.Env, env...), "GIT_CONFIG_COUNT="+count)
		return c.Run()
	}
	if err := gitDir("8"); err != nil {
		t.Fatalf("the test does not reproduce the bare repository: %v", err)
	}
	if gitDir("9") == nil {
		t.Fatal("harness git used the planted bare repository")
	}
}

// #61: seja v prezrti podmapi app; agent v 1. krogu naredi app/.git, 2. krog v istem klonu je zavrnjen z natančnim
// sporočilom (kot Swift worktreePlantedGitOnCwdPathRefusedNextTurn).
func TestServeWorktreePlantedGitOnCwdPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake claude is a shell script")
	}
	client := t.TempDir()
	gitT(t, client, "init", "-q", "-b", "main")
	writeT(t, filepath.Join(client, ".gitignore"), "app/\n")
	writeT(t, filepath.Join(client, "app", "x.txt"), "x\n")
	gitT(t, client, "add", "-A")
	gitT(t, client, "commit", "-qm", "first")
	fake := filepath.Join(t.TempDir(), "claude")
	writeT(t, fake, `#!/bin/sh
`+cliGuard+`
cat > /dev/null
mkdir -p app && echo 'gitdir: /nowhere' > app/.git
echo '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"done"}}}'
echo '{"type":"result","subtype":"success","usage":{"output_tokens":1}}'
`)
	os.Chmod(fake, 0o755)
	s, _ := testServer(t, func(c *serveConfig) { c.Claude = fake })
	c := startLoopback(t, s)
	cwd := filepath.Join(client, "app")
	sc := &shareClient{c: c, session: "sid61", root: cwd, worktree: true, top: filepath.ToSlash(client), synced: map[string]syncEntry{}, reported: map[string]bool{}}
	if !sc.register() {
		t.Fatal("register failed: " + sc.refusal)
	}
	sc.start(2)
	defer sc.unregister()
	h := map[string]string{"x-claude-mini-session": "sid61", "x-claude-mini-cwd": cwd, "content-type": "application/json"}
	body := `{"model":"claude-cli-haiku","tools":[{"name":"x"}],"messages":[{"role":"user","content":"go"}]}`
	if st, data := call(c, "POST", "/v1/messages", h, []byte(body), 60*time.Second); st != 200 || !strings.Contains(string(data), `"done"`) {
		t.Fatalf("turn 1: %d %s", st, data)
	}
	body = `{"model":"claude-cli-haiku","tools":[{"name":"x"}],"messages":[{"role":"user","content":"go"},{"role":"assistant","content":"done"},{"role":"user","content":"again"}]}`
	if st, data := call(c, "POST", "/v1/messages", h, []byte(body), 60*time.Second); st != 400 ||
		!strings.Contains(string(data), "The agent's clone was changed (a .git appeared at app/.git): start a new conversation") {
		t.Fatalf("turn 2: %d %s", st, data)
	}
}

// #67: git init/clone agenta le v $MINIGW_DEPS in le v peskovniku (oznaka SANDBOX_RUNTIME od Claude Code ali MINIGW_SANDBOXED
// od prehoda): tam navaden git (tudi submodule update prek ovoja exec), drugje zavrnitev z namigom in klon nespremenjen;
// brez oznake (harness) v $MINIGW_DEPS ostane pravilo klona; MINIGW_NETWORK_OFF zavrne oddaljen clone, lokalnega ne.
func TestPathWrapDeps(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no PATH wrapper on Windows")
	}
	root, _ := filepath.EvalSymlinks(t.TempDir())
	wt := filepath.Join(root, "worktrees", "d", "id", "claude-12345678-0")
	deps := depsDir(wt)
	gitT(t, root, "init", "-q", "-b", "main", wt)
	gitT(t, wt, "commit", "-q", "--allow-empty", "-m", "c")
	head := gitT(t, wt, "rev-parse", "HEAD")
	sub, src := filepath.Join(root, "sub"), filepath.Join(root, "src")
	for _, d := range []string{sub, src} {
		gitT(t, root, "init", "-q", "-b", "main", d)
		gitT(t, d, "commit", "-q", "--allow-empty", "-m", d)
	}
	gitT(t, src, "-c", "protocol.file.allow=always", "submodule", "add", "-q", sub, "s")
	gitT(t, src, "commit", "-q", "-m", "s")
	os.MkdirAll(filepath.Join(deps, "tmp"), 0o700)
	gw := filepath.Join(root, "gitexec")
	env := worktreeClaudeEnv(wt, filepath.Join(root, "m"), filepath.Join(root, "t"), gw, []string{filepath.Join(wt, ".git")}, "claude")
	if env == nil || !slices.Contains(env, "MINIGW_DEPS="+deps) || !slices.Contains(env, "GIT_CONFIG_KEY_9=init.templateDir") {
		t.Fatalf("env: %v", env)
	}
	run := func(dir string, extra []string, args ...string) (string, int) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		for _, kv := range agentEnv(os.Environ()) {
			if !strings.HasPrefix(kv, "GIT_") && !strings.HasPrefix(kv, "PATH=") {
				cmd.Env = append(cmd.Env, kv)
			}
		}
		cmd.Env = append(append(cmd.Env, env...), extra...)
		for _, kv := range env {
			if p, ok := strings.CutPrefix(kv, "PATH="); ok {
				cmd.Path = filepath.Join(strings.Split(p, ":")[0], "git") // git z PATH agenta (ovoj)
			}
		}
		out, err := cmd.CombinedOutput()
		code := 0
		if e, ok := err.(*exec.ExitError); ok {
			code = e.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out)), code
	}
	hint := "minigw: create repositories only under $MINIGW_DEPS (" + deps + ")"
	for _, mark := range []string{"SANDBOX_RUNTIME=1", "MINIGW_SANDBOXED=1"} {
		sb := []string{mark, "GIT_CONFIG_PARAMETERS='protocol.file.allow'='always'"}
		os.RemoveAll(deps)
		os.MkdirAll(deps, 0o700)
		// 1: init in clone v $MINIGW_DEPS, commit in podmodul v klonu agenta; brez predlog (vzorčni hooki)
		if out, c := run(wt, sb, "init", "-q", filepath.Join(deps, "x")); c != 0 {
			t.Fatalf("%s init in deps: %d %s", mark, c, out)
		}
		if _, err := os.Stat(filepath.Join(deps, "x", ".git", "hooks")); err == nil {
			t.Fatalf("%s: templates copied", mark)
		}
		if out, c := run(deps, sb, "clone", "-q", "--config", "advice.detachedHead=false", src, "y"); c != 0 {
			t.Fatalf("%s clone in deps: %d %s", mark, c, out)
		}
		y := filepath.Join(deps, "y")
		writeT(t, filepath.Join(y, "f"), "f\n")
		if out, c := run(y, sb, "add", "f"); c != 0 {
			t.Fatalf("%s add: %s", mark, out)
		}
		if out, c := run(y, sb, "commit", "-qm", "f"); c != 0 {
			t.Fatalf("%s commit: %s", mark, out)
		}
		if out, c := run(y, sb, "submodule", "update", "-q", "--init"); c != 0 {
			t.Fatalf("%s submodule update: %d %s", mark, c, out)
		}
		if out, _ := run(y, sb, "rev-parse", "--absolute-git-dir"); out != filepath.Join(y, ".git") {
			t.Fatalf("%s rev-parse in deps: %s", mark, out)
		}
		// 2: init/clone v klonu in zunaj: zavrnitev z namigom
		for _, a := range [][]string{{"init", "-q", "build/_deps/foo"}, {"clone", "-q", src, "build/_deps/bar"}, {"init", "-q", filepath.Join(root, "z")},
			{"init", "-q", filepath.Join(deps, "n", "..", "..", "esc")}, {"clone", "-q", "--separate-git-dir", filepath.Join(wt, "g"), src, filepath.Join(deps, "s")}} {
			if out, c := run(wt, sb, a...); c != 128 || !strings.HasPrefix(out, hint) {
				t.Fatalf("%s git %v: %d %q", mark, a, c, out)
			}
		}
		// 4: brez omrežja oddaljen clone ne, lokalen da
		if out, c := run(deps, append(sb, "MINIGW_NETWORK_OFF=Network for builds"), "clone", "-q", "https://example.invalid/a.git"); c != 128 || !strings.Contains(out, "(Network for builds)") {
			t.Fatalf("%s remote clone without network: %d %q", mark, c, out)
		}
		if out, c := run(deps, append(sb, "MINIGW_NETWORK_OFF=Network for builds"), "clone", "-q", src, "w"); c != 0 {
			t.Fatalf("%s local clone without network: %s", mark, out)
		}
	}
	// 3: brez oznake (harness): v $MINIGW_DEPS git klona, init/clone zavrnjena; read-only (brez MINIGW_DEPS) svoje sporočilo
	if out, _ := run(filepath.Join(deps, "y"), nil, "rev-parse", "--absolute-git-dir"); out != filepath.Join(wt, ".git") {
		t.Fatalf("harness in deps: %s", out)
	}
	if out, c := run(deps, nil, "init", "-q", "h"); c != 128 || !strings.Contains(out, "need the agent sandbox") {
		t.Fatalf("harness init in deps: %d %q", c, out)
	}
	if out, c := run(deps, []string{"SANDBOX_RUNTIME=1", "MINIGW_DEPS="}, "init", "-q", "h"); c != 128 || out != "minigw: this run cannot write (read-only)" {
		t.Fatalf("read-only init: %d %q", c, out)
	}
	if gitT(t, wt, "rev-parse", "HEAD") != head || gitT(t, wt, "status", "--porcelain") != "" {
		t.Fatal("clone changed")
	}
	if _, err := os.Stat(filepath.Join(wt, "build")); err == nil {
		t.Fatal("repository made in the clone")
	}
}

// #67: $MINIGW_DEPS gre s klonom (removeClone), pri ohranjenem klonu (kept) ne ostane; prazna .maven in .cache ne
// zadržita mape klonov, polna ostaneta.
func TestDepsLifetime(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	for _, d := range []string{a, depsDir(a) + "/tmp", b, depsDir(b) + "/x", filepath.Join(dir, ".maven"), filepath.Join(dir, ".cache")} {
		os.MkdirAll(d, 0o700)
	}
	keepClone(b, b+"-kept-1", t.Logf)
	if _, err := os.Stat(depsDir(b)); err == nil {
		t.Fatal("deps kept with the clone")
	}
	waitFor(t, func() bool { _, err := os.Stat(depsDir(b + "-kept-1")); return os.IsNotExist(err) })
	removeClone(a)
	if _, err := os.Stat(depsDir(a)); err == nil {
		t.Fatal("deps not removed with the clone")
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal("clone folder removed while a kept clone is there")
	}
	os.RemoveAll(b + "-kept-1")
	writeT(t, filepath.Join(dir, ".cache", "go-mod", "x"), "x")
	removeClone(filepath.Join(dir, "c"))
	if _, err := os.Stat(filepath.Join(dir, ".cache", "go-mod", "x")); err != nil {
		t.Fatal("cache with content removed")
	}
	os.RemoveAll(filepath.Join(dir, ".cache", "go-mod"))
	removeClone(filepath.Join(dir, "c"))
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("empty clone folder with empty .maven and .cache not removed")
	}
}

// #67: TMPDIR teka Claude je kratka povezava v korenu prehoda na $MINIGW_DEPS/tmp (podatki pod Scratch); done odstrani le
// povezavo; koren, ki ni naš (ne 0700), je ne dobi.
func TestRunTempLink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no link on Windows")
	}
	root := shortTempRoot()
	if root == "" {
		t.Fatal("no temp root")
	}
	defer os.RemoveAll(root)
	target := filepath.Join(t.TempDir(), "c.deps", "tmp")
	link, done, err := runTempLink(root, target)
	if err != nil || len(link)+len("/claude-")+len(strconv.Itoa(os.Getuid())) > 44 {
		t.Fatalf("link %q: %v", link, err)
	}
	if dst, _ := os.Readlink(link); dst != target {
		t.Fatalf("link points to %q", dst)
	}
	writeT(t, filepath.Join(link, "probe"), "x")
	if _, err := os.Stat(filepath.Join(target, "probe")); err != nil {
		t.Fatal("write did not land in the deps folder")
	}
	done()
	if _, err := os.Lstat(link); err == nil {
		t.Fatal("link left after the run")
	}
	if _, err := os.Stat(filepath.Join(target, "probe")); err != nil {
		t.Fatal("done removed the target")
	}
	os.Chmod(root, 0o755)
	if _, _, err := runTempLink(root, target); err == nil {
		t.Fatal("link in a root that is not private")
	}
	if es, _ := os.ReadDir(root); len(es) != 0 {
		t.Fatalf("left in the root: %v", es)
	}
}
