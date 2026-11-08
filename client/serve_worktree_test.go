package main

import (
	"crypto/ecdh"
	"crypto/rand"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
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
cat > /dev/null
[ -f ../a.txt ] && grep -q uncommitted ../a.txt && echo saw-uncommitted > ../seen.txt
n=$(ls ../agent*.txt 2>/dev/null | wc -l | tr -d ' ')
echo "agent $n" > ../agent$n.txt
git add ../agent$n.txt && git -c user.name=a -c user.email=a@x commit -qm "agent $n"
echo left > ../left.txt
echo '* filter=evil' > ../.gitattributes
printf '[core]\n\tfsmonitor = touch `+marks+`/gw-fsmonitor\n[filter "evil"]\n\tclean = touch `+marks+`/gw-clean; cat\n' >> "$GIT_DIR/config"
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
		if st, data := call(c, "POST", "/v1/messages", h, []byte(body), 60*time.Second); st != 200 || !strings.Contains(string(data), `"done"`) {
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

func TestWorktreeTreePaths(t *testing.T) {
	tr, err := worktreeTree("/s", "dev", `C:\proj`, `C:\proj\a\b`)
	if err != nil || tr.sub != "a/b" || !strings.HasSuffix(filepath.ToSlash(tr.repo), "/s/repos/dev/"+sha256Hex([]byte("C:/proj"))[:16]+".git") {
		t.Fatalf("windows paths: %+v %v", tr, err)
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
	if tip := finalize(repo, wt, "minigw/claude-12345678-0", head, "claude", filepath.Join(root, "out.bundle")); tip != "" {
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
	got := strings.Join(worktreeClaudeArgs(args, "/w", "/m", "/r/objects"), " ")
	for _, want := range []string{`"allowWrite":["/w/sub","/w","/m"]`, `"allowRead":["/w/sub","/w","/m","/r/objects"]`, "--add-dir /w",
		"Edit(//w/**)", `"denyWrite":["/w/.git/config"`, "Edit(//w/.git/**)"} {
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
	s, err := newServer(c, filepath.Join(t.TempDir(), "serve.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got := s.conf(); !got.ClaudeDisabled || got.Codex.Enabled || len(got.AllowedFolders) != 0 {
		t.Fatalf("startup with an allowed folder around Scratch: %+v", got)
	}
	if off := s.claudeInfo().Off; !strings.Contains(off, parent) { // razlog v gw status, gw claude in na strani
		t.Fatalf("no reason: %q", off)
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
	if cloneTooLarge(t.TempDir()) != "" {
		t.Fatal("empty clone too large")
	}
	h := newShareHub()
	key, _ := h.registerTree("s", "/p", "", &shareTree{repo: "/r", worktrees: t.TempDir(), slots: map[string]*wtSlot{}}, "dev")
	if _, st, _ := h.worktreeResult(key, "dev"); st != 404 {
		t.Fatalf("no result: %d", st)
	}
	h.mu.Lock()
	h.bySession["s"].tree.resultErr = "the agent's clone is too large"
	h.mu.Unlock()
	if _, st, why := h.worktreeResult(key, "dev"); st != 413 || !strings.Contains(why, "too large") {
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
	if strings.Contains(strings.Join(s.agentArgs(t.TempDir()), " "), "--add-dir "+parent) {
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
