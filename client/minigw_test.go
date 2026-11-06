package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	files, skipped := syncScan(root, nil)
	if len(files) != 2 || files["src/a.swift"].SHA256 != sha256Hex([]byte("src/a.swift")) || files[".gitignore"].Path == "" || len(skipped) != 0 {
		t.Fatalf("scan: %v %v", files, skipped)
	}
	fake := files["src/a.swift"]
	fake.SHA256 = "cached" // brez ponovnega zgoščevanja, ko sta velikost in čas enaka
	if again, _ := syncScan(root, map[string]syncEntry{"src/a.swift": fake}); again["src/a.swift"].SHA256 != "cached" {
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
	state, _ := syncScan(root, nil)
	put("c.txt", "user c") // uporabnik med agentom
	put("d.txt", "user d")
	blobs := map[string][]byte{"a.txt": []byte("agent a"), "c.txt": []byte("agent c"), "new/n.txt": []byte("agent n")}
	ch := syncChanges{Deleted: []string{"b.txt", "d.txt"}}
	for _, p := range []string{"a.txt", "c.txt", "new/n.txt"} {
		ch.Changed = append(ch.Changed, syncEntry{Path: p, Size: int64(len(blobs[p])), Mtime: 1700000000123, SHA256: sha256Hex(blobs[p])})
	}
	notes, done := syncApply(ch, root, state, func(p string) []byte { return blobs[p] })
	if strings.Join(done, ",") != "a.txt,c.txt,new/n.txt,b.txt,d.txt" {
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

func TestSyncApplySkipsIgnoredAndKeepsMode(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "run.sh"), []byte("old"), 0o755)
	state, _ := syncScan(root, nil)
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
	synced, _ := syncScan(root, nil) // stanje po zadnjem uspešnem prenosu
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
	state, _ := syncScan(root, nil)
	e := state["run.sh"]
	e.Exec = true
	_, done := syncApply(syncChanges{Changed: []syncEntry{e, {Path: "../bad", SHA256: "z"}}, Deleted: []string{"../gone"}}, root, state,
		func(string) []byte { return nil })
	if st, _ := os.Stat(filepath.Join(root, "run.sh")); st.Mode().Perm()&0o100 == 0 {
		t.Fatalf("exec bit not applied: %v", st.Mode())
	}
	if strings.Join(done, ",") != "run.sh,../bad,../gone" {
		t.Fatalf("done: %v", done)
	}
}

// Uporabnikov bit x ostane; agentov chmod +x s spremembo vsebine pride do odjemalca.
func TestSyncApplyRespectsUserExecBit(t *testing.T) {
	root := t.TempDir()
	a, b := filepath.Join(root, "a.sh"), filepath.Join(root, "b.sh")
	os.WriteFile(a, []byte("v1"), 0o644)
	os.WriteFile(b, []byte("v1"), 0o644)
	state, _ := syncScan(root, nil)
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
	state, _ := syncScan(root, nil)
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
