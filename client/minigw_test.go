package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

func TestDavResolveStaysInRoot(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "a.txt"), []byte("x"), 0o644)
	os.Symlink(os.TempDir(), filepath.Join(root, "out"))
	r, _ := filepath.EvalSymlinks(root)
	if _, _, ok := davResolve("/dav/s/a.txt", "/dav/s", r); !ok {
		t.Fatal("file in root rejected")
	}
	for _, p := range []string{"/dav/s/../etc", "/dav/s/%2e%2e/x", "/dav/s/out/x", "/other/a.txt"} {
		if _, _, ok := davResolve(p, "/dav/s", r); ok {
			t.Fatalf("%s escaped root", p)
		}
	}
	if resp := davHandle("DELETE", "/dav/s", nil, nil, root, "/dav/s"); resp.Status != 403 {
		t.Fatalf("root delete allowed: %d", resp.Status)
	}
}

func TestFramesRoundTrip(t *testing.T) {
	key := make([]byte, 32)
	send, recv := newNoiseCipher(key), newNoiseCipher(key)
	msg := make([]byte, 3*maxPlain+5)
	for i := range msg {
		msg[i] = byte(i)
	}
	plain, rest, err := unframe(frames(msg, send), recv)
	if err != nil || len(rest) != 0 || string(plain) != string(msg) {
		t.Fatalf("round trip failed: %v rest=%d", err, len(rest))
	}
}

func TestShareCommandAndWanted(t *testing.T) {
	cfg, proj := t.TempDir(), t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", cfg)
	t.Setenv("APPDATA", cfg)
	t.Setenv("HOME", cfg)
	t.Setenv("MINIGW_SHARE", "")
	sub := filepath.Join(proj, "src")
	os.Mkdir(sub, 0o755)
	shareCommand([]string{"add", proj})
	if !shareWanted(sub, false) || !shareWanted(proj, false) {
		t.Fatal("added folder or its subfolder not shared")
	}
	if shareWanted(sub, true) {
		t.Fatal("shared with a local router")
	}
	if shareAllowed(filepath.VolumeName(proj)+string(filepath.Separator)) || shareAllowed(cfg) {
		t.Fatal("drive root or home folder allowed")
	}
}

func TestAutoShare(t *testing.T) {
	home, _ := os.UserHomeDir()
	yes := []string{filepath.Join(home, "Development", "x"), "/Volumes/Java/importers/app", "/srv/projects/app"}
	no := []string{"/", home, filepath.Join(home, ".ssh"), filepath.Join(home, "Library", "Mail"), "/Volumes/Java",
		"/usr/local/src", "/System/Library", "/Users/other/Documents", "/Applications/X.app", "/tmp/x"}
	for _, p := range yes {
		if !autoShare(p) {
			t.Errorf("%s should be shared", p)
		}
	}
	for _, p := range no {
		if autoShare(p) {
			t.Errorf("%s should not be shared", p)
		}
	}
}

func TestDavGetRangeAndLimit(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "a.txt"), []byte("0123456789"), 0o644)
	big, _ := os.Create(filepath.Join(root, "big.bin"))
	big.Truncate(maxServe + 1)
	big.Close()
	if r := davHandle("GET", "/p/a.txt", map[string]string{"range": "bytes=2-4"}, nil, root, "/p"); r.Status != 206 || string(r.Body) != "234" {
		t.Fatalf("range: %d %q", r.Status, r.Body)
	}
	if r := davHandle("HEAD", "/p/big.bin", nil, nil, root, "/p"); r.Status != 200 || len(r.Body) != 0 || r.Headers["Content-Length"] != strconv.Itoa(maxServe+1) {
		t.Fatalf("head: %d %v", r.Status, r.Headers)
	}
	if r := davHandle("GET", "/p/big.bin", nil, nil, root, "/p"); r.Status != 413 {
		t.Fatalf("big get: %d", r.Status)
	}
	if r := davHandle("GET", "/p/big.bin", map[string]string{"range": "bytes=0-99"}, nil, root, "/p"); r.Status != 206 || len(r.Body) != 100 {
		t.Fatalf("big range: %d %d", r.Status, len(r.Body))
	}
}

func TestSyncIgnoreRules(t *testing.T) {
	r := parseIgnore("# komentar\n*.log\n/out\nsrc/gen/\n!keep.log\n")
	cases := []struct {
		rel  string
		dir  bool
		want bool
	}{
		{".git", true, true}, {"a/node_modules", true, true}, {"x/.DS_Store", false, true},
		{"build", false, false}, {"a/b/debug.log", false, true}, {"keep.log", false, true},
		{"out", true, true}, {"a/out", true, false}, {"src/gen", true, true}, {"lib/src/gen", true, false},
		{"a.swift.minigw-conflict", false, true}, {"main.swift", false, false},
	}
	for _, c := range cases {
		if got := ignored(r, c.rel, c.dir); got != c.want {
			t.Errorf("ignored(%q, dir=%v) = %v", c.rel, c.dir, got)
		}
	}
}

func TestSyncScanAndPaths(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "proj")
	for _, d := range []string{".git", "node_modules/x", "src"} {
		os.MkdirAll(filepath.Join(root, d), 0o755)
	}
	for _, f := range []string{".git/HEAD", "node_modules/x/i.js", "src/a.swift", "app.log"} {
		os.WriteFile(filepath.Join(root, f), []byte(f), 0o644)
	}
	os.WriteFile(filepath.Join(root, ".gitignore"), []byte("*.log\r\n"), 0o644)
	os.WriteFile(filepath.Join(base, "secret"), []byte("no"), 0o644)
	os.Symlink(base, filepath.Join(root, "link"))
	files, skipped := syncScan(root, nil, true)
	if len(files) != 2 || files["src/a.swift"].SHA256 != sha256Hex([]byte("src/a.swift")) || files[".gitignore"].Path == "" || len(skipped) != 0 {
		t.Fatalf("scan: %v %v", files, skipped)
	}
	fake := files["src/a.swift"]
	fake.SHA256 = "cached" // brez ponovnega zgoščevanja, ko sta velikost in čas enaka
	if again, _ := syncScan(root, map[string]syncEntry{"src/a.swift": fake}, true); again["src/a.swift"].SHA256 != "cached" {
		t.Fatal("rehashed an unchanged file")
	}
	for _, p := range []string{"src/a.swift", "src/new/b.txt"} {
		if _, ok := syncResolve(p, root); !ok {
			t.Errorf("%s rejected", p)
		}
	}
	for _, p := range []string{"../secret", "/etc/passwd", "src/../../secret", "src//a", `a\b`, "", "link/secret", "link/new.txt"} {
		if _, ok := syncResolve(p, root); ok {
			t.Errorf("%q escaped root", p)
		}
	}
}

func TestSyncApplyConflicts(t *testing.T) {
	root := t.TempDir()
	put := func(p, s string) {
		os.MkdirAll(filepath.Dir(filepath.Join(root, p)), 0o755)
		os.WriteFile(filepath.Join(root, p), []byte(s), 0o644)
	}
	read := func(p string) string {
		b, err := os.ReadFile(filepath.Join(root, p))
		if err != nil {
			return "<none>"
		}
		return string(b)
	}
	for _, p := range []string{"a.txt", "b.txt", "c.txt", "d.txt"} {
		put(p, "v1 "+p)
	}
	state, _ := syncScan(root, nil, true)
	put("c.txt", "user c") // uporabnik med agentom
	put("d.txt", "user d")
	blobs := map[string][]byte{"a.txt": []byte("agent a"), "c.txt": []byte("agent c"), "new/n.txt": []byte("agent n")}
	ch := syncChanges{Deleted: []string{"b.txt", "d.txt"}}
	for _, p := range []string{"a.txt", "c.txt", "new/n.txt"} {
		ch.Changed = append(ch.Changed, syncEntry{Path: p, Size: int64(len(blobs[p])), Mtime: 1700000000123, SHA256: sha256Hex(blobs[p])})
	}
	notes, done := syncApply(ch, root, state, func(p string) []byte { return blobs[p] })
	if strings.Join(done, ",") != "b.txt,d.txt,a.txt,c.txt,new/n.txt" {
		t.Fatalf("done: %v", done)
	}
	if read("a.txt") != "agent a" || read("new/n.txt") != "agent n" || read("b.txt") != "<none>" {
		t.Fatalf("not applied: %q %q %q", read("a.txt"), read("new/n.txt"), read("b.txt"))
	}
	if read("c.txt") != "user c" || read("c.txt.minigw-conflict") != "agent c" || read("d.txt") != "user d" {
		t.Fatalf("conflict overwritten: %q %q %q", read("c.txt"), read("c.txt.minigw-conflict"), read("d.txt"))
	}
	if len(notes) != 2 || !strings.HasPrefix(notes[0], "conflict:") || !strings.HasPrefix(notes[1], "conflict:") {
		t.Fatalf("notes: %v", notes)
	}
	if st, _ := os.Stat(filepath.Join(root, "a.txt")); st.ModTime().UnixMilli() != 1700000000123 || state["a.txt"].Mtime != 1700000000123 {
		t.Fatal("mtime not taken from the router")
	}
}

// Dva agenta hkrati iz iste različice (router pošlje Base): različni datoteki prideta obe; isto datoteko drugi ne
// prepiše (spor), tudi ko jo je prvi že zapisal in je stanje odjemalca že novo.
func TestSyncApplyParallelRunsUseBase(t *testing.T) {
	root := t.TempDir()
	read := func(p string) string {
		b, err := os.ReadFile(filepath.Join(root, p))
		if err != nil {
			return "<none>"
		}
		return string(b)
	}
	for _, p := range []string{"a.txt", "b.txt", "c.txt", "d.txt"} {
		os.WriteFile(filepath.Join(root, p), []byte("v1"), 0o644)
	}
	state, _ := syncScan(root, nil, true)
	v1 := sha256Hex([]byte("v1"))
	run := func(files map[string]string, deleted ...string) syncChanges {
		ch := syncChanges{Deleted: deleted, Base: map[string]string{}}
		for _, p := range deleted {
			ch.Base[p] = v1
		}
		for p, s := range files {
			ch.Changed = append(ch.Changed, syncEntry{Path: p, Size: int64(len(s)), Mtime: 1, SHA256: sha256Hex([]byte(s))})
			ch.Base[p] = v1
		}
		sort.Slice(ch.Changed, func(i, j int) bool { return ch.Changed[i].Path < ch.Changed[j].Path })
		return ch
	}
	apply := func(ch syncChanges, blobs map[string]string) []string {
		notes, _ := syncApply(ch, root, state, func(p string) []byte { return []byte(blobs[p]) })
		return notes
	}
	first := map[string]string{"a.txt": "agent1", "c.txt": "one"}
	second := map[string]string{"b.txt": "agent2", "c.txt": "two", "d.txt": "two"}
	if n := apply(run(first, "d.txt"), first); len(n) != 0 {
		t.Fatalf("first run: %v", n)
	}
	n := apply(run(second), second)
	if read("a.txt") != "agent1" || read("b.txt") != "agent2" {
		t.Fatalf("different files: %q %q", read("a.txt"), read("b.txt"))
	}
	if read("c.txt") != "one" || read("c.txt.minigw-conflict") != "two" || read("d.txt") != "<none>" || read("d.txt.minigw-conflict") != "two" {
		t.Fatalf("second run overwrote the first: %q %q %q", read("c.txt"), read("c.txt.minigw-conflict"), read("d.txt"))
	}
	if len(n) != 2 || !strings.HasPrefix(n[0], "conflict:") || !strings.HasPrefix(n[1], "conflict:") {
		t.Fatalf("notes: %v", n)
	}
}

// Agent zamenja datoteko »a« z mapo »a/b«: izbris »a« gre pred zapisom »a/b«; in nazaj (prazna mapa → datoteka).
func TestSyncApplyFileToFolder(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "a"), []byte("file"), 0o644)
	state, _ := syncScan(root, nil, true)
	b := []byte("new")
	ch := syncChanges{Deleted: []string{"a"}, Changed: []syncEntry{{Path: "a/b", Size: 3, Mtime: 1, SHA256: sha256Hex(b)}},
		Base: map[string]string{"a": sha256Hex([]byte("file"))}}
	if notes, done := syncApply(ch, root, state, func(string) []byte { return b }); len(done) != 2 || len(notes) != 0 {
		t.Fatalf("file → folder: %v %v", notes, done)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "a", "b")); string(got) != "new" {
		t.Fatalf("a/b = %q", got)
	}
	f := []byte("file again")
	ch = syncChanges{Deleted: []string{"a/b"}, Changed: []syncEntry{{Path: "a", Size: 10, Mtime: 1, SHA256: sha256Hex(f)}},
		Base: map[string]string{"a/b": sha256Hex(b)}}
	if notes, done := syncApply(ch, root, state, func(string) []byte { return f }); len(done) != 2 || len(notes) != 0 {
		t.Fatalf("folder → file: %v %v", notes, done)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "a")); string(got) != "file again" {
		t.Fatalf("a = %q", got)
	}
}

func TestSyncApplySkipsIgnoredAndKeepsMode(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "run.sh"), []byte("old"), 0o755)
	state, _ := syncScan(root, nil, true)
	if !state["run.sh"].Exec {
		t.Fatal("exec flag missing")
	}
	blobs := map[string][]byte{"run.sh": []byte("new"), "node_modules/p/i.js": []byte("x"), ".git/config": []byte("evil"), "tool": []byte("t")}
	ch := syncChanges{}
	for _, p := range []string{"run.sh", "node_modules/p/i.js", ".git/config", "tool"} {
		ch.Changed = append(ch.Changed, syncEntry{Path: p, Size: int64(len(blobs[p])), Mtime: 1700000000000, SHA256: sha256Hex(blobs[p]), Exec: p == "tool" || p == "run.sh"})
	}
	_, done := syncApply(ch, root, state, func(p string) []byte { return blobs[p] })
	if len(done) != 4 {
		t.Fatalf("done: %v", done)
	}
	for _, p := range []string{"node_modules", ".git"} {
		if _, err := os.Lstat(filepath.Join(root, p)); err == nil {
			t.Fatalf("%s written although ignored", p)
		}
	}
	if st, _ := os.Stat(filepath.Join(root, "run.sh")); st.Mode().Perm() != 0o755 {
		t.Fatalf("run.sh mode %v", st.Mode())
	}
	if st, _ := os.Stat(filepath.Join(root, "tool")); st.Mode().Perm()&0o100 == 0 {
		t.Fatalf("tool mode %v", st.Mode())
	}
}

// Neuspešen PUT: stanje poti ostane staro (prevzem nato zazna spor); poti zunaj manifesta se ne pošljejo.
func TestSyncPushKeepsOldStateOnFailedUpload(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "a.txt"), []byte("v1"), 0o644)
	os.WriteFile(filepath.Join(root, "secret.log"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(root, ".gitignore"), []byte("*.log\n"), 0o644)
	puts := []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/minigw/sync/manifest":
			json.NewEncoder(w).Encode(map[string]any{"need": []string{"a.txt", "secret.log"}, "kept": []string{}})
		case "/minigw/sync/file":
			puts = append(puts, r.Header.Get("x-sync-path"))
			w.WriteHeader(409)
		}
	}))
	defer srv.Close()
	old := syncEntry{Path: "a.txt", Size: 2, Mtime: 1, SHA256: "old"}
	s := &shareClient{c: connection{URL: srv.URL}, root: root, sync: true, synced: map[string]syncEntry{"a.txt": old}, reported: map[string]bool{}}
	if !s.syncPush("k") {
		t.Fatal("push failed")
	}
	if strings.Join(puts, ",") != "a.txt" {
		t.Fatalf("uploaded %v", puts)
	}
	if s.synced["a.txt"] != old || s.synced[".gitignore"].SHA256 == "" {
		t.Fatalf("state %v", s.synced)
	}
	if _, ok := s.synced["secret.log"]; ok {
		t.Fatal("ignored file in state")
	}
}

// Pull ni uspel → uporabnik spremeni (ali izbriše) datoteko → push (router jo vrne kot »kept«) → pull: spor, ne prepis.
func TestSyncKeptIsNotTreatedAsSynced(t *testing.T) {
	root := t.TempDir()
	put := func(p, s string) { os.WriteFile(filepath.Join(root, p), []byte(s), 0o644) }
	read := func(p string) string {
		b, err := os.ReadFile(filepath.Join(root, p))
		if err != nil {
			return "<none>"
		}
		return string(b)
	}
	put("x.txt", "v1")
	put("y.txt", "v1")
	synced, _ := syncScan(root, nil, true) // stanje po zadnjem uspešnem prenosu
	put("x.txt", "user edit")
	os.Remove(filepath.Join(root, "y.txt"))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"need": []string{}, "kept": []string{"x.txt", "y.txt"}})
	}))
	defer srv.Close()
	s := &shareClient{c: connection{URL: srv.URL}, root: root, sync: true, synced: synced, reported: map[string]bool{}}
	if !s.syncPush("k") {
		t.Fatal("push failed")
	}
	if s.synced["x.txt"].SHA256 != sha256Hex([]byte("v1")) || s.synced["y.txt"].SHA256 != sha256Hex([]byte("v1")) {
		t.Fatalf("kept path marked as synced: %v", s.synced)
	}
	agent := []byte("agent")
	ch := syncChanges{}
	for _, p := range []string{"x.txt", "y.txt"} {
		ch.Changed = append(ch.Changed, syncEntry{Path: p, Size: 5, Mtime: 1, SHA256: sha256Hex(agent)})
	}
	notes, _ := syncApply(ch, root, s.synced, func(string) []byte { return agent })
	if read("x.txt") != "user edit" || read("x.txt.minigw-conflict") != "agent" {
		t.Fatalf("user edit overwritten: %q", read("x.txt"))
	}
	if read("y.txt") != "<none>" || read("y.txt.minigw-conflict") != "agent" || len(notes) != 2 {
		t.Fatalf("deleted file came back silently: %q %v", read("y.txt"), notes)
	}
}

func TestSyncApplyExecOnlyAndInvalidPath(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "run.sh"), []byte("x"), 0o644)
	state, _ := syncScan(root, nil, true)
	e := state["run.sh"]
	e.Exec = true
	_, done := syncApply(syncChanges{Changed: []syncEntry{e, {Path: "../bad", SHA256: "z"}}, Deleted: []string{"../gone"}}, root, state,
		func(string) []byte { return nil })
	if st, _ := os.Stat(filepath.Join(root, "run.sh")); st.Mode().Perm()&0o100 == 0 {
		t.Fatalf("exec bit not applied: %v", st.Mode())
	}
	if strings.Join(done, ",") != "../gone,run.sh,../bad" {
		t.Fatalf("done: %v", done)
	}
}

// Uporabnikov bit x ostane; agentov chmod +x s spremembo vsebine pride do odjemalca.
func TestSyncApplyRespectsUserExecBit(t *testing.T) {
	root := t.TempDir()
	a, b := filepath.Join(root, "a.sh"), filepath.Join(root, "b.sh")
	os.WriteFile(a, []byte("v1"), 0o644)
	os.WriteFile(b, []byte("v1"), 0o644)
	state, _ := syncScan(root, nil, true)
	mode := func(p string) os.FileMode { st, _ := os.Stat(p); return st.Mode().Perm() }
	os.Chmod(b, 0o755) // uporabnik po prenosu
	e := state["b.sh"]
	notes, _ := syncApply(syncChanges{Changed: []syncEntry{e}}, root, state, func(string) []byte { return nil })
	if mode(b) != 0o755 || len(notes) != 1 || !strings.HasPrefix(notes[0], "kept your file mode") {
		t.Fatalf("user x bit changed: %v %v", mode(b), notes)
	}
	agent := []byte("agent")
	edit := syncEntry{Path: "b.sh", Size: 5, Mtime: 1, SHA256: sha256Hex(agent)}
	syncApply(syncChanges{Changed: []syncEntry{edit}}, root, state, func(string) []byte { return agent })
	if mode(b) != 0o755 {
		t.Fatalf("agent edit cleared the user's x bit: %v", mode(b))
	}
	x := syncEntry{Path: "a.sh", Size: 5, Mtime: 2, SHA256: sha256Hex(agent), Exec: true}
	syncApply(syncChanges{Changed: []syncEntry{x}}, root, state, func(string) []byte { return agent })
	if mode(a) != 0o755 {
		t.Fatalf("agent chmod +x not applied: %v", mode(a))
	}
}

// Router ni zaupanja vreden: SYNC-* le v načinu sync, SYNC-PULL ne pri --minigw-ro, WebDAV le v direct.
func TestShareJobsOnlyInTheirMode(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "a.txt"), []byte("x"), 0o644)
	ro := &shareClient{root: root, sync: true, readOnly: true, synced: map[string]syncEntry{}, reported: map[string]bool{}}
	if r := ro.handle(davJob{Method: "SYNC-PULL"}, "k"); r.Status != 403 {
		t.Fatalf("ro SYNC-PULL: %d", r.Status)
	}
	direct := &shareClient{root: root, synced: map[string]syncEntry{}, reported: map[string]bool{}}
	for _, m := range []string{"SYNC-PULL", "SYNC-PUSH", "SYNC-X"} {
		if r := direct.handle(davJob{Method: m}, "k"); r.Status != 403 {
			t.Fatalf("direct %s: %d", m, r.Status)
		}
	}
	rw := &shareClient{root: root, sync: true}
	if r := rw.handle(davJob{Method: "DELETE", Path: "/dav/s/a.txt", Prefix: "/dav/s"}, "k"); r.Status != 403 {
		t.Fatalf("sync WebDAV: %d", r.Status)
	}
	if r := direct.handle(davJob{Method: "GET", Path: "/dav/s/a.txt", Prefix: "/dav/s"}, "k"); r.Status != 200 {
		t.Fatalf("direct GET: %d", r.Status)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "a.txt")); string(b) != "x" {
		t.Fatal("folder changed")
	}
}

func TestSyncNeverTouchesGitOrCommandSettings(t *testing.T) {
	for _, p := range []string{".git/config", ".GIT/hooks/post-checkout", "a/.Git./hooks/x", ".git /x", ".claude/settings.json",
		".Claude/Settings.Local.json", "sub/.vscode/tasks.json", ".envrc", "x/.ENVRC"} {
		if !protectedPath(p) {
			t.Errorf("%q not protected", p)
		}
	}
	for _, p := range []string{"src/git.go", ".gitignore", ".claude/agents/a.md", ".vscode/settings.json", "envrc"} {
		if protectedPath(p) {
			t.Errorf("%q protected", p)
		}
	}
	if caseFold && !ignored(parseIgnore(""), "Node_Modules", true) {
		t.Error("ignore rules are case-sensitive on a case-insensitive file system")
	}
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, ".GIT"), 0o755)
	os.WriteFile(filepath.Join(root, ".GIT", "config"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(root, "a.txt"), []byte("y"), 0o644)
	state, _ := syncScan(root, nil, true)
	if len(state) != 1 {
		t.Fatalf(".GIT synced: %v", state)
	}
	evil := []byte("#!/bin/sh")
	ch := syncChanges{Deleted: []string{".GIT/config"}}
	for _, p := range []string{".GIT/hooks/post-checkout", ".claude/settings.json", ".envrc"} {
		ch.Changed = append(ch.Changed, syncEntry{Path: p, Size: int64(len(evil)), Mtime: 1, SHA256: sha256Hex(evil), Exec: true})
	}
	notes, done := syncApply(ch, root, state, func(string) []byte { return evil })
	if len(done) != 4 || len(notes) != 4 {
		t.Fatalf("notes %v done %v", notes, done)
	}
	for _, p := range []string{".GIT/hooks", ".claude", ".envrc"} {
		if _, err := os.Lstat(filepath.Join(root, p)); err == nil {
			t.Fatalf("%s written", p)
		}
	}
	if _, err := os.Stat(filepath.Join(root, ".GIT", "config")); err != nil {
		t.Fatal(".GIT/config deleted")
	}
}

// COPY ne sledi vgnezdenim simbolnim povezavam iz mape.
func TestDavCopySkipsNestedSymlinks(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	os.MkdirAll(filepath.Join(root, "d"), 0o755)
	os.WriteFile(filepath.Join(base, "secret"), []byte("no"), 0o644)
	os.WriteFile(filepath.Join(root, "d", "f.txt"), []byte("ok"), 0o644)
	if err := os.Symlink(filepath.Join(base, "secret"), filepath.Join(root, "d", "link")); err != nil {
		t.Skip("no symlinks:", err)
	}
	os.Symlink(base, filepath.Join(root, "d", "up"))
	r := davHandle("COPY", "/dav/s/d", map[string]string{"destination": "http://x/dav/s/e"}, nil, root, "/dav/s")
	if r.Status != 201 {
		t.Fatalf("COPY: %d", r.Status)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "e", "f.txt")); string(b) != "ok" {
		t.Fatal("file not copied")
	}
	for _, p := range []string{"link", "up"} {
		if _, err := os.Lstat(filepath.Join(root, "e", p)); err == nil {
			t.Fatalf("symlink %s followed or copied", p)
		}
	}
}

func TestProtectedPathShortAndFoldedNames(t *testing.T) {
	for _, p := range []string{"GIT~1/hooks/post-checkout", "git~12/config", ".claude/ſettings.json", ".vscode/taſks.json"} {
		if !protectedPath(p) {
			t.Errorf("%s should be protected", p)
		}
	}
	if protectedPath("src/git~x.txt") {
		t.Error("git~x is not a short name")
	}
}

// .gitignore kot povezava ven iz korena ali FIFO: prezrta, brez čakanja.
func TestSyncIgnoreSkipsLinkedOrSpecialGitignore(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "root")
	os.Mkdir(root, 0o755)
	os.WriteFile(filepath.Join(base, "outside"), []byte("*.txt\n"), 0o644)
	gi := filepath.Join(root, ".gitignore")
	if os.Symlink(filepath.Join(base, "outside"), gi) == nil && ignored(loadIgnore(root, false), "a.txt", false) {
		t.Fatal(".gitignore followed a link out of the root")
	}
	os.Remove(gi)
	if exec.Command("mkfifo", gi).Run() == nil && ignored(loadIgnore(root, false), "a.txt", false) { // brez mkfifo (Windows): preskoči
		t.Fatal("FIFO .gitignore was read")
	}
	os.Remove(gi)
	os.WriteFile(gi, []byte("*.txt\n"), 0o644)
	if !ignored(loadIgnore(root, false), "a.txt", false) {
		t.Fatal("regular .gitignore not read")
	}
	// Odjemalec: povezan .gitignore (dotfiles) velja; .env ne gre na router.
	os.Remove(gi)
	os.WriteFile(filepath.Join(base, "outside"), []byte(".env\n"), 0o644)
	os.WriteFile(filepath.Join(root, ".env"), []byte("x"), 0o644)
	if os.Symlink(filepath.Join(base, "outside"), gi) == nil {
		if files, _ := syncScan(root, nil, true); files[".env"].SHA256 != "" {
			t.Fatal("client scan ignored a linked .gitignore")
		}
	}
}

// #11: zaščita velja za kanonično pot: povezava hooks → .git/hooks ne pusti pisanja ali brisanja v .git.
func TestSyncProtectedThroughLink(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, ".git", "hooks"), 0o755)
	os.WriteFile(filepath.Join(root, ".git", "hooks", "old"), []byte("x"), 0o644)
	if os.Symlink(filepath.Join(".git", "hooks"), filepath.Join(root, "hooks")) != nil {
		t.Skip("no symlinks here")
	}
	data := []byte("#!/bin/sh\necho owned\n")
	sum := sha256Hex(data)
	ch := syncChanges{Changed: []syncEntry{{Path: "hooks/pre-commit", Size: int64(len(data)), SHA256: sum},
		{Path: "src/ok.txt", Size: int64(len(data)), SHA256: sum}}, Deleted: []string{"hooks/old"}}
	notes, _ := syncApply(ch, root, map[string]syncEntry{"hooks/old": {Path: "hooks/old", SHA256: sha256Hex([]byte("x"))}},
		func(string) []byte { return data })
	if _, err := os.Stat(filepath.Join(root, ".git", "hooks", "pre-commit")); err == nil {
		t.Fatalf("wrote into .git through a link: %v", notes)
	}
	if _, err := os.Stat(filepath.Join(root, ".git", "hooks", "old")); err != nil {
		t.Fatalf("deleted from .git through a link: %v", notes)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "src", "ok.txt")); string(b) != string(data) {
		t.Fatalf("normal file not written: %v", notes)
	}
	if !strings.Contains(strings.Join(notes, "\n"), "protected path (through a link)") {
		t.Fatalf("notes: %v", notes)
	}
}

// #20: osnovo pošlje router; neznana različica ne prepiše uporabnikove spremembe med izvajanjem.
func TestSyncBaseMustBeAVersionWeSent(t *testing.T) {
	for _, known := range []bool{false, true} {
		root := t.TempDir()
		os.WriteFile(filepath.Join(root, "a.txt"), []byte("user edit"), 0o644)
		state := map[string]syncEntry{"a.txt": {Path: "a.txt", SHA256: sha256Hex([]byte("orig"))}}
		agent := []byte("agent")
		ch := syncChanges{Changed: []syncEntry{{Path: "a.txt", Size: int64(len(agent)), SHA256: sha256Hex(agent)}},
			Base:  map[string]string{"a.txt": sha256Hex([]byte("user edit"))}, // router je izvedel trenutno različico
			Known: func(p, sha string) bool { return known }}
		syncApply(ch, root, state, func(string) []byte { return agent })
		got, _ := os.ReadFile(filepath.Join(root, "a.txt"))
		conflict, _ := os.ReadFile(filepath.Join(root, "a.txt.minigw-conflict"))
		if !known && (string(got) != "user edit" || string(conflict) != "agent") {
			t.Fatalf("unknown base overwrote the user's edit: %q / %q", got, conflict)
		}
		if known && string(got) != "agent" { // različica, ki jo je odjemalec res poslal: prepis je pravilen
			t.Fatalf("known base: %q", got)
		}
	}
}
