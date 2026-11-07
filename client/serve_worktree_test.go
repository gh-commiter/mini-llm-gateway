package main

import (
	"os"
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
