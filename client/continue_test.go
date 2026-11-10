package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// #66: --continue v načinu worktree: drugi zagon začne na veji prvega (prehod Go z lažnim claude), nadomestne poti
// (spremenjena mapa, premaknjen HEAD, izbrisana ali že združena veja) začnejo iz mape.
func TestWorktreeContinueConversation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake claude is a shell script")
	}
	root := t.TempDir()
	client := filepath.Join(root, "client")
	os.MkdirAll(client, 0o755)
	gitT(t, client, "init", "-q", "-b", "main")
	writeT(t, filepath.Join(client, "readme.txt"), "r\n")
	gitT(t, client, "add", "-A")
	gitT(t, client, "commit", "-qm", "first")
	head := gitT(t, client, "rev-parse", "HEAD")

	// Agent: brez a.txt naredi a.txt, z njim b.txt (vidi delo prejšnjega zagona).
	fake := filepath.Join(root, "claude")
	writeT(t, fake, `#!/bin/sh
`+cliGuard+`
cat > /dev/null
if [ -f a.txt ]; then echo b > b.txt; else echo a > a.txt; fi
echo '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"done"}}}'
echo '{"type":"result","subtype":"success","usage":{"output_tokens":1}}'
`)
	os.Chmod(fake, 0o755)
	s, _ := testServer(t, func(c *serveConfig) { c.Claude = fake })
	c := startLoopback(t, s)

	turn := func(sid string, cont bool) string { // en zagon minigw: registracija, ena zahteva agentu, veja nazaj
		t.Helper()
		sc := &shareClient{c: c, session: sid, root: client, worktree: true, top: client, synced: map[string]syncEntry{}, reported: map[string]bool{}}
		if !sc.register() {
			t.Fatal("register failed: " + sc.refusal)
		}
		sc.conv = wtConversation(client, sid, cont)
		sc.start(2)
		defer sc.unregister()
		body := `{"model":"claude-cli-haiku","tools":[{"name":"x"}],"messages":[{"role":"user","content":"go"}]}`
		h := map[string]string{"x-claude-mini-session": sid, "x-claude-mini-cwd": client, "content-type": "application/json"}
		if st, data := call(c, "POST", "/v1/messages", h, []byte(body), 60*time.Second); st != 200 || !strings.Contains(string(data), `"done"`) {
			t.Fatalf("messages %s: %d %s", sid, st, data)
		}
		return "minigw/claude-" + sha256Hex([]byte(sid))[:8] + "-0"
	}
	files := func(branch string) string {
		return strings.Join(strings.Fields(gitT(t, client, "ls-tree", "-r", "--name-only", branch)), ",")
	}

	b1 := turn("sid1", false)
	if f := files(b1); f != "a.txt,readme.txt" {
		t.Fatalf("turn 1: %s", f)
	}
	b2 := turn("sid2", true) // --continue: nova seja, isti pogovor
	if f := files(b2); f != "a.txt,b.txt,readme.txt" {
		t.Fatalf("turn 2 did not start from turn 1: %s", f)
	}
	st, _ := wtLoad(client)
	if r := st.Convs["sid1"]; st.Last != "sid1" || r.Branch != b2 || r.Base != head || r.Head != head {
		t.Fatalf("record: %+v", st)
	}
	if gitT(t, client, "rev-parse", "HEAD") != head || gitT(t, client, "status", "--porcelain") != "" {
		t.Fatal("the user's folder changed")
	}

	// Nadomestne poti (wtContinue neposredno).
	snap := func() string { return gitSnapshot(client, false, false) }
	if tip, _, why := wtContinue(client, "sid1", snap(), false); tip != gitT(t, client, "rev-parse", b2) || why != "" {
		t.Fatalf("unchanged folder: %q %q", tip, why)
	}
	if tip, _, why := wtContinue(client, "sid1", snap(), true); tip != "" || !strings.Contains(why, "nested") {
		t.Fatalf("nested: %q %q", tip, why)
	}
	writeT(t, filepath.Join(client, "mine.txt"), "m\n")
	if tip, _, why := wtContinue(client, "sid1", snap(), false); tip != "" || why != "your folder changed since the last turn" {
		t.Fatalf("dirty tree: %q %q", tip, why)
	}
	b3 := turn("sid3", true) // spremenjena mapa: iz mape (brez a.txt), nato veriga od tu
	if f := files(b3); f != "a.txt,mine.txt,readme.txt" {
		t.Fatalf("turn 3 (dirty): %s", f)
	}
	gitT(t, client, "add", "-A")
	gitT(t, client, "commit", "-qm", "mine")
	if tip, _, why := wtContinue(client, "sid1", snap(), false); tip != "" || why != "your folder changed since the last turn" {
		t.Fatalf("HEAD moved: %q %q", tip, why)
	}
	b4 := turn("sid4", true)
	gitT(t, client, "branch", "-D", b4)
	if tip, _, why := wtContinue(client, "sid1", snap(), false); tip != "" || why != "the branch no longer exists" {
		t.Fatalf("deleted: %q %q", tip, why)
	}
	b5 := turn("sid5", true)
	gitT(t, client, "merge", "-q", "--ff-only", b5)
	if tip, _, why := wtContinue(client, "sid1", snap(), false); tip != "" || why != "" {
		t.Fatalf("merged: %q %q", tip, why)
	}
	// Nov pogovor (brez --continue) ne nadaljuje starega; --resume <id> nadaljuje svojega.
	if wtConversation(client, "sid6", false) != "sid6" || wtConversation(client, "sid1", false) != "sid1" ||
		wtConversation(client, "sid7", true) != "sid1" {
		t.Fatal("conversation key")
	}
}
