package main

import (
	"encoding/json"
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

	// Agent: turn<n>.txt, n = število že obstoječih (vidi delo prejšnjih zagonov).
	fake := filepath.Join(root, "claude")
	writeT(t, fake, `#!/bin/sh
`+cliGuard+`
cat > /dev/null
n=$(ls turn*.txt 2>/dev/null | wc -l | tr -d ' ')
echo $n > turn$n.txt
echo '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"done"}}}'
echo '{"type":"result","subtype":"success","usage":{"output_tokens":1}}'
`)
	os.Chmod(fake, 0o755)
	gateway := func() connection {
		s, _ := testServer(t, func(c *serveConfig) { c.Claude = fake })
		return startLoopback(t, s)
	}
	c := gateway()

	// en zagon minigw: registracija, ena zahteva agentu, veja nazaj; cont = --continue, parent = --resume <id>
	turnConv := func(sid string, conv wtConv) string {
		t.Helper()
		sc := &shareClient{c: c, session: sid, root: client, worktree: true, top: client, synced: map[string]syncEntry{}, reported: map[string]bool{}}
		if !sc.register() {
			t.Fatal("register failed: " + sc.refusal)
		}
		sc.conv, sc.gateway = conv, "https://gw-a:8443"
		sc.start(2)
		defer sc.unregister()
		body := `{"model":"claude-cli-haiku","tools":[{"name":"x"}],"messages":[{"role":"user","content":"go"}]}`
		h := map[string]string{"x-claude-mini-session": sid, "x-claude-mini-cwd": client, "content-type": "application/json"}
		if st, data := call(c, "POST", "/v1/messages", h, []byte(body), 60*time.Second); st != 200 || !strings.Contains(string(data), `"done"`) {
			t.Fatalf("messages %s: %d %s", sid, st, data)
		}
		return "minigw/claude-" + sha256Hex([]byte(sid))[:8] + "-0"
	}
	turn := func(sid string, cont bool) string { // cont: -c, ki ga minigw preda kot --resume sid1 (continueSession)
		parent := ""
		if cont {
			parent = "sid1"
		}
		return turnConv(sid, wtConversation(sid, parent, false, false))
	}
	files := func(branch string) string {
		return strings.Join(strings.Fields(gitT(t, client, "ls-tree", "-r", "--name-only", branch)), ",")
	}

	b1 := turn("sid1", false)
	if f := files(b1); f != "readme.txt,turn0.txt" {
		t.Fatalf("turn 1: %s", f)
	}
	b2 := turn("sid2", true) // --continue: nova seja, isti pogovor
	if f := files(b2); f != "readme.txt,turn0.txt,turn1.txt" {
		t.Fatalf("turn 2 did not start from turn 1: %s", f)
	}
	st, _ := wtLoad(client)
	if r := st.Convs["sid1"]; r.Branch != b2 || r.Base != head || r.Head != head || r.Tree != gitT(t, client, "rev-parse", "HEAD^{tree}") {
		t.Fatalf("record: %+v", st)
	}
	if gitT(t, client, "rev-parse", "HEAD") != head || gitT(t, client, "status", "--porcelain") != "" {
		t.Fatal("the user's folder changed")
	}

	// Mapa se spremeni: nadaljuje vseeno, z vrstico; iz mape le brez veje in pri ugnezdenih.
	snap := func() string { return gitSnapshot(client, false, false) }
	check := func(what, wantTip, wantMsg string) {
		t.Helper()
		if tip, _, msg := wtContinue(client, "sid1", "https://gw-a:8443", snap(), false); tip != wantTip || msg != wantMsg {
			t.Fatalf("%s: %q %q", what, tip, msg)
		}
	}
	tip := func(b string) string { return gitT(t, client, "rev-parse", b) }
	changed := func(b string) string {
		return "worktree: continuing from " + b + "; changes in your folder since the last turn are not in the agent's copy"
	}
	check("unchanged folder", tip(b2), "")
	if tip, _, msg := wtContinue(client, "sid1", "https://gw-a:8443", snap(), true); tip != "" || msg != "worktree: starting from your folder, not from "+b2+": nested git repositories always start from your folder" {
		t.Fatalf("nested: %q %q", tip, msg)
	}
	writeT(t, filepath.Join(client, "mine.txt"), "m\n")
	check("dirty tree", tip(b2), changed(b2))
	b3 := turn("sid3", true)
	if f := files(b3); f != "readme.txt,turn0.txt,turn1.txt,turn2.txt" {
		t.Fatalf("turn 3 (dirty) did not continue: %s", f)
	}
	check("no change since turn 3", tip(b3), "")
	gitT(t, client, "add", "-A")
	gitT(t, client, "commit", "-qm", "mine")
	check("HEAD moved", tip(b3), changed(b3))
	b4 := turn("sid4", true)
	if f := files(b4); f != "readme.txt,turn0.txt,turn1.txt,turn2.txt,turn3.txt" {
		t.Fatalf("turn 4 (HEAD moved) did not continue: %s", f)
	}
	gitT(t, client, "branch", "-D", b4)
	check("deleted", "", "worktree: starting from your folder, not from "+b4+": the branch no longer exists")
	b5 := turn("sid5", true)
	if f := files(b5); f != "mine.txt,readme.txt,turn0.txt" {
		t.Fatalf("turn 5 (deleted) did not start from the folder: %s", f)
	}
	// L-B: drug prehod začne iz mape.
	if tip, _, msg := wtContinue(client, "sid1", "https://gw-b:8443", snap(), false); tip != "" ||
		msg != "worktree: starting from your folder, not from "+b5+": the conversation ran on another gateway" {
		t.Fatalf("other gateway: %q %q", tip, msg)
	}
	// L-A: veja, že v HEAD, in novejši commit uporabnika: iz mape (agent vidi novejši commit), brez vrstice.
	gitT(t, client, "merge", "-q", "--ff-only", b5)
	writeT(t, filepath.Join(client, "newer.txt"), "n\n")
	gitT(t, client, "add", "-A")
	gitT(t, client, "commit", "-qm", "newer")
	check("merged", "", "")
	b6 := turn("sid6m", true)
	if f := files(b6); f != "mine.txt,newer.txt,readme.txt,turn0.txt,turn1.txt" {
		t.Fatalf("turn after merge did not start from the folder: %s", f)
	}
	if r := mustRecord(t, client, "sid1"); r.Branch != b6 || r.Base != gitT(t, client, "rev-parse", "HEAD") {
		t.Fatalf("new chain after merge: %+v", r)
	}
	// Nov pogovor ne nadaljuje starega; --resume <id> nadaljuje svojega; izbirnik ne nadaljuje in ne zapiše.
	if wtConversation("sid6", "", false, false) != (wtConv{"", "sid6"}) || wtConversation("x", "sid1", false, false) != (wtConv{"sid1", "sid1"}) ||
		wtConversation("sid11", "", true, false) != (wtConv{}) {
		t.Fatal("conversation key")
	}
	_, p := wtLoad(client)
	if fi, err := os.Stat(p); err != nil || runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("record file: %v %v", fi, err)
	}
	// 3: fork nadaljuje starša, zapiše pod novo sejo (starševski zapis ostane).
	if c := wtConversation("f1", "sid1", false, true); c != (wtConv{"sid1", "f1"}) {
		t.Fatalf("fork: %+v", c)
	}
}

// #66 (4): odločitev enkrat na zagon: drugi WORKTREE-PUSH istega zagona začne iz istega vrha, tudi ko je vmes prišla
// nova veja (brez povezave: vrh je v »have«, zato ni prenosa).
func TestWorktreeContinueDecidedOncePerRun(t *testing.T) {
	root := t.TempDir()
	client := filepath.Join(root, "client")
	os.MkdirAll(client, 0o755)
	gitT(t, client, "init", "-q", "-b", "main")
	writeT(t, filepath.Join(client, "r.txt"), "r\n")
	gitT(t, client, "add", "-A")
	gitT(t, client, "commit", "-qm", "first")
	head := gitT(t, client, "rev-parse", "HEAD")
	commit := func(parent, msg string) string {
		return gitT(t, client, "commit-tree", gitT(t, client, "rev-parse", "HEAD^{tree}"), "-p", parent, "-m", msg)
	}
	b1, b2 := "minigw/claude-0123abcd-0", "minigw/claude-0123abcd-1"
	t1 := commit(head, "turn 1")
	gitT(t, client, "update-ref", "refs/heads/"+b1, t1)
	st, p := wtLoad(client)
	st.Convs["s1"] = wtRecord{Branch: b1, Base: head, Head: head, Tree: gitT(t, client, "rev-parse", "HEAD^{tree}")}
	wtSave(p, st)
	sc := &shareClient{top: client, conv: wtConv{"s1", "s1"}, reported: map[string]bool{}}
	push := func(have ...string) string {
		b, _ := json.Marshal(map[string]any{"Have": have})
		r := sc.wtPush("k", davJob{Body: b})
		var out struct{ Snapshot string }
		if json.Unmarshal(r.Body, &out); r.Status != 200 {
			t.Fatalf("push: %d", r.Status)
		}
		return out.Snapshot
	}
	if got := push(t1); got != t1 {
		t.Fatalf("first push: %s", got)
	}
	t2 := commit(t1, "agent A") // veja agenta A je prišla med zagonom
	gitT(t, client, "update-ref", "refs/heads/"+b2, t2)
	if base, cont := sc.wtRemember(b2, t1); base != head || !cont {
		t.Fatalf("remember: %s %v", base, cont)
	}
	if got := push(t1, t2); got != t1 {
		t.Fatalf("second push of the run starts from %s, not from the run's base %s", got, t1)
	}
	if st, _ := wtLoad(client); st.Convs["s1"].Branch != b2 {
		t.Fatalf("next run continues from the last fetched branch: %+v", st.Convs["s1"])
	}
	if !strings.Contains(continuedLine(b2, head), "("+head+" = your state when this conversation started; this branch includes the earlier turns)") {
		t.Fatal("continued line")
	}
}

func mustRecord(t *testing.T, top, key string) wtRecord {
	t.Helper()
	st, _ := wtLoad(top)
	r, ok := st.Convs[key]
	if !ok {
		t.Fatalf("no record %s", key)
	}
	return r
}
