package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestDavResolveStaysInRoot(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "a.txt"), []byte("x"), 0o644)
	dirLink(t, os.TempDir(), filepath.Join(root, "out")) // Windows brez pravice: stičišče
	r, _ := filepath.EvalSymlinks(root)
	if _, _, ok := davResolve("/dav/s/a.txt", "/dav/s", r); !ok {
		t.Fatal("file in root rejected")
	}
	for _, p := range []string{"/dav/s/../etc", "/dav/s/%2e%2e/x", "/dav/s/out/x", "/other/a.txt"} {
		if _, _, ok := davResolve(p, "/dav/s", r); ok {
			t.Fatalf("%s escaped root", p)
		}
	}
	// I1: Windows ne shrani imena s piko ali presledkom na koncu, naprave (tudi s končnico) niso datoteke
	for _, n := range []string{"a.", "a ", "CON", "nul.txt", "Com1", "lpt9.log", "COM¹", "aux .x", "CONIN$"} {
		if !winBadName(n) {
			t.Errorf("%q accepted", n)
		}
	}
	for _, n := range []string{"a.txt", "console", "com10", "nul1", ".git", "COM", "lptx"} {
		if winBadName(n) {
			t.Errorf("%q refused", n)
		}
	}
	if resp := davHandle("DELETE", "/dav/s", nil, nil, root, "/dav/s"); resp.Status != 403 {
		t.Fatalf("root delete allowed: %d", resp.Status)
	}
}

// Windows: stičišče (mklink /J, brez pravic) ven iz korena: filepath.EvalSymlinks ga od Go 1.23 ne razreši, finalPath da.
func TestDavResolveRefusesJunction(t *testing.T) {
	base := t.TempDir()
	base, _ = filepath.EvalSymlinks(base)
	root, out := filepath.Join(base, "r"), filepath.Join(base, "out")
	os.MkdirAll(filepath.Join(root, "in"), 0o755)
	os.MkdirAll(out, 0o755)
	os.WriteFile(filepath.Join(base, "secret"), []byte("no"), 0o644)
	junction(t, out, filepath.Join(root, "j"))
	junction(t, filepath.Join(root, "in"), filepath.Join(root, "ok"))
	for _, p := range []string{"/s/j", "/s/j/x", "/s/j/new/y"} {
		if _, _, ok := davResolve(p, "/s", root); ok {
			t.Errorf("%s escaped root through a junction", p)
		}
	}
	if _, ok := syncResolve("j/x", root); ok {
		t.Error("sync: j/x escaped root through a junction")
	}
	if _, _, ok := davResolve("/s/ok/x", "/s", root); !ok {
		t.Error("junction inside root refused")
	}
	if r := davHandle("PUT", "/s/j/x", nil, []byte("y"), root, "/s"); r.Status != 403 {
		t.Errorf("PUT through junction: %d", r.Status)
	}
	if _, err := os.Stat(filepath.Join(out, "x")); err == nil {
		t.Error("written outside root")
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
	t.Setenv("USERPROFILE", cfg) // os.UserHomeDir na Windows
	t.Setenv("MINIGW_SHARE", "")
	sub := filepath.Join(proj, "src")
	os.Mkdir(sub, 0o755)
	shareCommand([]string{"add", proj})
	if shareWanted(sub, false) == nil || shareWanted(proj, false) == nil {
		t.Fatal("added folder or its subfolder not shared")
	}
	// Aplikacija na tem Macu dela v mapi sami; minigw serve na tem računalniku (Windows, Linux) jo potrebuje deljeno (#9).
	if (shareWanted(sub, true) != nil) != (runtime.GOOS != "darwin") {
		t.Fatal("local gateway: shared on macOS, or not shared on Windows/Linux")
	}
	if shareRefusal(filepath.VolumeName(proj)+string(filepath.Separator)) == "" || shareRefusal(cfg) == "" {
		t.Fatal("drive root or home folder allowed")
	}
	dirLink(t, cfg, filepath.Join(proj, "home")) // Windows brez pravice: stičišče
	if shareRefusal(filepath.Join(proj, "home")) == "" || shareRefusal(cfg+string(filepath.Separator)) == "" {
		t.Fatal("home folder allowed through a link or with a trailing separator")
	}
}

func TestAutoShare(t *testing.T) {
	home, _ := os.UserHomeDir()
	tmp := t.TempDir()
	for _, k := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(k, tmp)
	}
	mk := func(p ...string) string {
		d := filepath.Join(append([]string{tmp}, p...)...)
		os.MkdirAll(d, 0o755)
		return d
	}
	// podmapa začasne mape (tudi %TEMP% v AppData) da, začasna mapa sama ne (0.5.19: privzeti auto jo kopira), skrita ne,
	// neobstoječa (lastnik neznan) ne
	yes := []string{filepath.Join(home, "Development", "x"), "/Volumes/Java/importers/app", "/srv/projects/app", mk("proj"), mk("proj", "src")}
	no := []string{"/", home, filepath.Join(home, ".ssh"), filepath.Join(home, "Library", "Mail"), "/Volumes/Java",
		"/usr/local/src", "/System/Library", "/Users/other/Documents", "/Applications/X.app", tmp, mk(".hidden", "x"),
		filepath.Join(tmp, "missing", "x")}
	if runtime.GOOS != "windows" {
		x, err := os.MkdirTemp("/tmp", "autoshare")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(x) })
		yes = append(yes, x)
		no = append(no, "/tmp", "/private/tmp", "/private/tmp/../etc", "/tmp/.X11-unix")
		if runtime.GOOS == "darwin" {
			yes = append(yes, "/private"+x) // F8: tudi /private/tmp/… in /private/var/folders/…
		}
		// L2: Claude Code: claude-<uid> in prvi dve ravni ne, globlje da
		c := "claude-" + strconv.Itoa(os.Getuid())
		yes = append(yes, mk(c, "p", "s", "scratchpad"), mk(c, "p", "s", "scratchpad", "x"))
		no = append(no, mk(c), mk(c, "p"), mk(c, "p", "s"))
		// L2: mapa drugega uporabnika pod /tmp ne
		if es, err := os.ReadDir("/private/tmp"); err == nil {
			for _, e := range es {
				if d := filepath.Join("/tmp", e.Name()); e.IsDir() && !strings.HasPrefix(e.Name(), ".") && !ownedByMe(d) {
					no = append(no, filepath.Join(d, "x"))
					break
				}
			}
		}
	}
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
	// L1: TMPDIR=/, domača mapa ali nad njo ne odpre ničesar
	for _, bad := range []string{"/", home, filepath.Dir(home)} {
		for _, k := range []string{"TMPDIR", "TMP", "TEMP"} {
			t.Setenv(k, bad)
		}
		for _, p := range []string{filepath.Join(home, ".ssh"), filepath.Join(home, "Library", "x"), filepath.Join(filepath.Dir(home), "other", "x")} {
			if autoShare(p) {
				t.Errorf("TMPDIR=%s: %s should not be shared", bad, p)
			}
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
	dirLink(t, base, filepath.Join(root, "link"))
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
	if !state["run.sh"].Exec && runtime.GOOS != "windows" { // Windows: brez bitov x
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
	if runtime.GOOS == "windows" {
		return
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
	if st, _ := os.Stat(filepath.Join(root, "run.sh")); st.Mode().Perm()&0o100 == 0 && runtime.GOOS != "windows" {
		t.Fatalf("exec bit not applied: %v", st.Mode())
	}
	if strings.Join(done, ",") != "../gone,run.sh,../bad" {
		t.Fatalf("done: %v", done)
	}
}

// Uporabnikov bit x ostane; agentov chmod +x s spremembo vsebine pride do odjemalca.
func TestSyncApplyRespectsUserExecBit(t *testing.T) {
	noExecBits(t)
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
		".Claude/Settings.Local.json", "sub/.vscode/tasks.json", ".envrc", "x/.ENVRC", ".claude/hooks/x.sh", ".claude/hooks",
		".Claude/Commands/c.md", ".claude/agents/a.md", "sub/.claude/skills/s/SKILL.md", ".husky/pre-commit", ".husky", "a/.githooks/pre-push"} {
		if !protectedPath(p) {
			t.Errorf("%q not protected", p)
		}
	}
	for _, p := range []string{"src/git.go", ".gitignore", ".claude/notes.md", "src/hooks/use.ts", "skills/a.md", ".vscode/settings.json", "envrc"} {
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
	links := []string{"up"}
	if os.Symlink(filepath.Join(base, "secret"), filepath.Join(root, "d", "link")) == nil {
		links = append(links, "link")
	} else if runtime.GOOS != "windows" {
		t.Fatal("symlink failed")
	}
	dirLink(t, base, filepath.Join(root, "d", "up")) // Windows brez pravice: stičišče
	r := davHandle("COPY", "/dav/s/d", map[string]string{"destination": "http://x/dav/s/e"}, nil, root, "/dav/s")
	if r.Status != 201 {
		t.Fatalf("COPY: %d", r.Status)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "e", "f.txt")); string(b) != "ok" {
		t.Fatal("file not copied")
	}
	for _, p := range links {
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
	// N10: na Windows vsako kratko ime 8.3 (druga pot do .claude, .vscode, .husky, .githooks)
	windowsNames = true
	defer func() { windowsNames = runtime.GOOS == "windows" }()
	for _, p := range []string{"CLAUDE~1/settings.json", "VSCODE~1/tasks.json", "HUSKY~1/pre-commit", "a/GITHOO~1/x", "x~2.txt"} {
		if !protectedPath(p) {
			t.Errorf("%s should be protected on Windows", p)
		}
	}
	root := t.TempDir()
	r, _ := filepath.EvalSymlinks(root)
	if _, _, ok := davResolve("/dav/s/CLAUDE~1/settings.json", "/dav/s", r); ok {
		t.Error("8.3 short name accepted by davResolve")
	}
	for _, p := range []string{"src/a~b.txt", "src/~x", "src/foo.~1~", "src/longname~1", "src/a~1.json5", "src/x~1y"} {
		if protectedPath(p) {
			t.Errorf("%s is not an 8.3 short name", p)
		}
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
	dirLink(t, filepath.Join(".git", "hooks"), filepath.Join(root, "hooks")) // Windows brez pravice: stičišče
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

// #2: imena, ki se razlikujejo le v velikih črkah, ne gredo na router.
func TestCaseCollisions(t *testing.T) {
	got := caseCollisions(map[string]syncEntry{"A.txt": {}, "a.txt": {}, "src/B.go": {}, "src/b.GO": {}, "ok.txt": {}})
	if strings.Join(got, ",") != "A.txt,a.txt,src/B.go,src/b.GO" {
		t.Fatalf("collisions: %v", got)
	}
}

// #6: datoteka samo za branje se posodobi in ostane samo za branje (Windows: atribut dol in nazaj gor).
func TestSyncWriteReadOnlyFile(t *testing.T) {
	full := filepath.Join(t.TempDir(), "ro.txt")
	os.WriteFile(full, []byte("old"), 0o444)
	if err := syncWrite(full, []byte("new"), 0, false, true); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(full)
	st, _ := os.Stat(full)
	if string(b) != "new" || st.Mode().Perm()&0o200 != 0 {
		t.Fatalf("read-only file: %q %v", b, st.Mode())
	}
}

// #7: claude ne podeduje seje in prijave starševskega Claude Code (401 na prehodu); naše vrednosti in drugo ostane.
var fakeEnvDump = fake("env-dump", func(p fakeParams, _ []string) { fakeWrite(p["out"], strings.Join(os.Environ(), "\n")+"\n") })

func TestClaudeChildEnvDropsParentSession(t *testing.T) {
	bin, out := t.TempDir(), filepath.Join(t.TempDir(), "env")
	fakeCLI(t, bin, "claude", fakeEnvDump, fakeParams{"out": out})
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	for k, v := range map[string]string{"CLAUDECODE": "1", "CLAUDE_CODE_ENTRYPOINT": "claude-desktop", "CLAUDE_CODE_OAUTH_TOKEN": "x",
		"CLAUDE_CODE_SSE_PORT": "1", "ANTHROPIC_BASE_URL": "http://parent", "ANTHROPIC_MODEL": "m", "ANTHROPIC_API_KEY": "k",
		"CLAUDE_CODE_GIT_BASH_PATH": "/bash", "MINIGW_KEEP": "1"} {
		t.Setenv(k, v)
	}
	if runClaude(nil, []string{"ANTHROPIC_BASE_URL=http://gw", "ANTHROPIC_AUTH_TOKEN=t", "ANTHROPIC_API_KEY=",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1"}, false) != 0 {
		t.Fatal("fake claude failed")
	}
	b, _ := os.ReadFile(out)
	got := strings.Split(strings.TrimSpace(string(b)), "\n")
	for _, bad := range []string{"CLAUDECODE=", "CLAUDE_CODE_ENTRYPOINT=", "CLAUDE_CODE_OAUTH_TOKEN=", "CLAUDE_CODE_SSE_PORT=",
		"ANTHROPIC_MODEL=", "ANTHROPIC_BASE_URL=http://parent", "ANTHROPIC_API_KEY=k"} {
		if slices.ContainsFunc(got, func(l string) bool { return strings.HasPrefix(l, bad) }) {
			t.Errorf("child env has %s", bad)
		}
	}
	for _, want := range []string{"ANTHROPIC_BASE_URL=http://gw", "ANTHROPIC_AUTH_TOKEN=t", "ANTHROPIC_API_KEY=",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "CLAUDE_CODE_GIT_BASH_PATH=/bash", "MINIGW_KEEP=1"} {
		if !slices.Contains(got, want) {
			t.Errorf("child env lacks %s", want)
		}
	}
}

// #8: --model …@profil izbere profil za ves zagon, model gre naprej brez pripone.
func TestModelProfileSelectsTheGateway(t *testing.T) {
	defer func() { profileFlag = "" }()
	names := []string{"default", "local"}
	profileFlag = ""
	a := modelProfile([]string{"-p", "--model", "claude-cli-opus@local", "hi"}, names)
	if profileFlag != "local" || a[2] != "claude-cli-opus" {
		t.Fatalf("got %q %v", profileFlag, a)
	}
	profileFlag = ""
	a = modelProfile([]string{"--model=claude-opus-4@20250514"}, names) // @ brez profila: model ostane cel
	if profileFlag != "" || a[0] != "--model=claude-opus-4@20250514" {
		t.Fatalf("got %q %v", profileFlag, a)
	}
	a = modelProfile([]string{"--model=gpt@default"}, names)
	if profileFlag != "default" || a[0] != "--model=gpt" {
		t.Fatalf("got %q %v", profileFlag, a)
	}
}

// #10: »a.txt« → »A.txt« (APFS/NTFS: isti datoteki) ne sme izbrisati vira; COPY nase in mapa v podmapo = 403.
func TestDavMoveSameFileKeepsContent(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "a.txt"), []byte("keep"), 0o644)
	os.MkdirAll(filepath.Join(root, "d", "sub"), 0o755)
	do := func(m, src, dst string) int {
		return davHandle(m, "/dav/s/"+src, map[string]string{"destination": "http://x/dav/s/" + dst}, nil, root, "/dav/s").Status
	}
	if s := do("COPY", "a.txt", "a.txt"); s != 403 {
		t.Fatalf("COPY onto itself: %d", s)
	}
	if s := do("MOVE", "a.txt", "A.txt"); s != 201 {
		t.Fatalf("case-only MOVE: %d", s)
	}
	if b, err := os.ReadFile(filepath.Join(root, "A.txt")); err != nil || string(b) != "keep" {
		t.Fatalf("content lost: %q %v", b, err)
	}
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if e.Name() == "a.txt" || strings.HasPrefix(e.Name(), ".minigw") {
			t.Fatalf("left behind: %s", e.Name())
		}
	}
	if s := do("MOVE", "A.txt", "A.txt"); s != 201 {
		t.Fatalf("MOVE onto itself: %d", s)
	}
	if b, _ := os.ReadFile(filepath.Join(root, "A.txt")); string(b) != "keep" {
		t.Fatal("MOVE onto itself lost content")
	}
	for _, m := range []string{"MOVE", "COPY"} {
		if s := do(m, "d", "d/sub/d"); s != 403 {
			t.Fatalf("%s into own child: %d", m, s)
		}
	}
}

// #2: direktni WebDAV ne piše v zaščitene poti (kot sync), tudi prek povezave; branje ostane.
func TestDavRefusesProtectedWrites(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, ".git", "hooks"), 0o755)
	os.WriteFile(filepath.Join(root, ".git", "HEAD"), []byte("ref"), 0o644)
	os.WriteFile(filepath.Join(root, "a.txt"), []byte("x"), 0o644)
	dirLink(t, filepath.Join(root, ".git", "hooks"), filepath.Join(root, "hooks"))
	h := func(m, p, dst string) int {
		hd := map[string]string{}
		if dst != "" {
			hd["destination"] = "http://x/dav/s/" + dst
		}
		return davHandle(m, "/dav/s/"+p, hd, []byte("y"), root, "/dav/s").Status
	}
	for _, c := range [][3]string{
		{"PUT", ".git/config", ""}, {"PUT", ".GIT/config", ""}, {"PUT", "git~1/config", ""}, {"PUT", ".envrc", ""},
		{"PUT", ".claude/settings.local.json", ""}, {"PUT", ".vscode/tasks.json", ""}, {"MKCOL", ".git/x", ""},
		{"DELETE", ".git/HEAD", ""}, {"DELETE", ".git", ""}, {"LOCK", ".git/new", ""}, {"PROPPATCH", ".git/HEAD", ""},
		{"MOVE", "a.txt", ".git/hooks/pre-commit"}, {"COPY", "a.txt", ".envrc"}, {"MOVE", ".git", "g"},
		{"PUT", ".claude/hooks/x.sh", ""}, {"MKCOL", ".claude/skills", ""}, {"PUT", ".claude/commands/c.md", ""},
		{"PUT", ".husky/pre-commit", ""}, {"MOVE", "a.txt", ".githooks/pre-push"}, {"PUT", "sub/.claude/agents/a.md", ""},
	} {
		if s := h(c[0], c[1], c[2]); s != 403 {
			t.Fatalf("%s %s %s: %d", c[0], c[1], c[2], s)
		}
	}
	if s := h("PUT", "hooks/pre-commit", ""); s != 403 {
		t.Fatalf("PUT through link: %d", s)
	}
	if s := h("GET", ".git/HEAD", ""); s != 200 {
		t.Fatalf("GET .git/HEAD: %d", s)
	}
	if s := h("COPY", ".git/HEAD", "head.txt"); s != 201 {
		t.Fatalf("COPY out of .git: %d", s)
	}
	if s := h("PUT", "b.txt", ""); s != 201 {
		t.Fatalf("PUT b.txt: %d", s)
	}
}

// M1: vrstica o zavrnitvi enkrat na pot, kontrolni znaki iz imena nikoli surovi, največ maxRefusedShown poti.
func TestDavRefusalLine(t *testing.T) {
	var l refusalLog
	for _, p := range []string{".git/\x1b]52;c;eA==\x07", ".git/a\nminigw: fake", ".git/\rb", ".git/\u202etxt.exe"} {
		line := l.line(p)
		if line == "" || strings.ContainsAny(strings.TrimSuffix(line, "\n"), "\x1b\n\r\x07\u202e") {
			t.Fatalf("%q -> %q", p, line)
		}
		if again := l.line(p); again != "" {
			t.Fatalf("printed twice: %q", again)
		}
	}
	for i := len(l.paths); i < maxRefusedShown; i++ {
		if l.line(".git/"+strconv.Itoa(i)) == "" {
			t.Fatal("below the cap")
		}
	}
	if line := l.line(".git/more"); !strings.Contains(line, "not shown") {
		t.Fatalf("over the cap: %q", line)
	}
	if l.line(".git/more2") != "" || len(l.paths) != maxRefusedShown {
		t.Fatal("cap not held")
	}
}

// #9: PUT obdrži pravice obstoječe datoteke.
func TestDavPutKeepsMode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no unix modes")
	}
	root := t.TempDir()
	p := filepath.Join(root, "build.sh")
	os.WriteFile(p, []byte("old"), 0o755)
	os.Chmod(p, 0o755)
	if r := davHandle("PUT", "/dav/s/build.sh", nil, []byte("new"), root, "/dav/s"); r.Status != 204 {
		t.Fatalf("PUT: %d", r.Status)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o755 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
}

// #2: vsebina premaknjene/kopirane/izbrisane mape ne sme ustvariti ali odstraniti zaščitene datoteke.
func TestDavRefusesProtectedTreeBypass(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"stage", "x", ".claude", "old"} {
		os.MkdirAll(filepath.Join(root, d), 0o755)
	}
	os.WriteFile(filepath.Join(root, "stage", "settings.json"), []byte("{}"), 0o644)
	os.WriteFile(filepath.Join(root, "x", "tasks.json"), []byte("{}"), 0o644)
	os.WriteFile(filepath.Join(root, ".claude", "settings.json"), []byte("{}"), 0o644)
	h := func(m, p, d string) int {
		hd := map[string]string{}
		if d != "" {
			hd["destination"] = "http://x/dav/s/" + d
		}
		return davHandle(m, "/dav/s/"+p, hd, nil, root, "/dav/s").Status
	}
	for _, c := range [][3]string{
		{"MOVE", "stage", ".claude"}, {"MOVE", "stage", ".Claude"}, {"COPY", "x", ".vscode"}, {"DELETE", ".claude", ""},
		{"MOVE", ".claude", "y"}, {"MOVE", "old", ".claude"}, {"COPY", "old", ".claude"},
	} {
		if s := h(c[0], c[1], c[2]); s != 403 {
			t.Fatalf("%s %s %s: %d", c[0], c[1], c[2], s)
		}
	}
	if _, err := os.Stat(filepath.Join(root, ".claude", "settings.json")); err != nil {
		t.Fatal(".claude/settings.json removed")
	}
	if _, err := os.Stat(filepath.Join(root, ".vscode")); err == nil {
		t.Fatal(".vscode planted")
	}
	if s := h("COPY", ".claude", "c2"); s != 201 {
		t.Fatalf("COPY .claude out: %d", s)
	}
	if s := h("MOVE", "stage", "s2"); s != 201 {
		t.Fatalf("MOVE stage: %d", s)
	}
}

// M1/M2/L3: ugnezden .git ne izgine prek starša; vir v cilju = 403; trda povezava ne pusti začasnega imena.
func TestDavNestedGitAncestorAndHardLink(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "repoA", "sub", ".git"), 0o755)
	os.WriteFile(filepath.Join(root, "repoA", "sub", ".git", "HEAD"), []byte("ref"), 0o644)
	os.MkdirAll(filepath.Join(root, "other"), 0o755)
	os.MkdirAll(filepath.Join(root, "a", "b"), 0o755)
	os.WriteFile(filepath.Join(root, "a", "b", "f"), []byte("keep"), 0o644)
	h := func(m, p, d string) int {
		hd := map[string]string{}
		if d != "" {
			hd["destination"] = "http://x/dav/s/" + d
		}
		return davHandle(m, "/dav/s/"+p, hd, nil, root, "/dav/s").Status
	}
	cases := [][3]string{{"DELETE", "repoA", ""}, {"MOVE", "other", "repoA"}, {"COPY", "other", "repoA"},
		{"MOVE", "repoA", "x"}, {"MOVE", "a/b", "a"}, {"COPY", "a/b", "a"}}
	if runtime.GOOS == "darwin" || runtime.GOOS == "windows" { // »A« je »a« le na neobčutljivem datotečnem sistemu
		cases = append(cases, [3]string{"MOVE", "a/b", "A"})
	}
	for _, c := range cases {
		if s := h(c[0], c[1], c[2]); s != 403 {
			t.Fatalf("%s %s %s: %d", c[0], c[1], c[2], s)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "repoA", "sub", ".git", "HEAD")); err != nil {
		t.Fatal("nested .git removed")
	}
	if b, _ := os.ReadFile(filepath.Join(root, "a", "b", "f")); string(b) != "keep" {
		t.Fatal("source inside destination lost")
	}
	os.WriteFile(filepath.Join(root, "h1"), []byte("hl"), 0o644)
	if os.Link(filepath.Join(root, "h1"), filepath.Join(root, "h2")) != nil {
		return
	}
	if s := h("MOVE", "h1", "h2"); s != 201 {
		t.Fatalf("hard link MOVE: %d", s)
	}
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".minigw") {
			t.Fatalf("left behind: %s", e.Name())
		}
	}
	if b, _ := os.ReadFile(filepath.Join(root, "h2")); string(b) != "hl" {
		t.Fatal("hard link content lost")
	}
}

func TestUnsafeClaudeFlagsInsideClaudeCode(t *testing.T) {
	in := map[string]string{"CLAUDECODE": "1"}
	env := func(k string) string { return in[k] }
	for _, a := range [][]string{
		{"--dangerously-skip-permissions", "-p", "x"}, {"-p", "--permission-mode", "bypassPermissions", "x"},
		{"-p", "--permission-mode=bypassPermissions", "x"}, {"-p", "--allowedTools", "Bash", "x"}, {"--settings", "s.json", "-p", "x"},
		{"--mcp-config", "m.json"}, {"--plugin-dir", "d"}, {"--add-dir", "/"}, {"-p", "--model", "m", "--allow-dangerously-skip-permissions"},
		{"-pc", "x"}, {"-p=1"}, {"--Model", "m"}, {"-p", "--model", "m", "--input-format", "stream-json"},
		{"-p", "--input-format=text"}, {"-r", "id", "-p", "x"}, {"--resume", "id"}, {"-c", "-p", "x"}, {"--session-id", "s", "-p"},
	} {
		if unsafeClaudeFlag(a, env) == "" {
			t.Errorf("%q accepted", a)
		}
	}
	for _, a := range [][]string{
		{"-p", "--model", "claude-codex-gpt-6.1-sol", "task"}, {"-p", "--model", "--dangerously-skip-permissions", "x"}, // vrednost --model
		{"-p", "--model=m", "--output-format", "stream-json", "--verbose", "x"}, {"-p", "--", "--add-dir"}, {"--help"},
		{"-p", "--model", "m", "--", "- fix x"}, nil,
	} {
		if f := unsafeClaudeFlag(a, env); f != "" {
			t.Errorf("%q refused (%s)", a, f)
		}
	}
	if unsafeClaudeFlag([]string{"--dangerously-skip-permissions"}, func(string) string { return "" }) != "" {
		t.Error("refused outside Claude Code")
	}
	in["MINIGW_ALLOW_UNSAFE_FLAGS"] = "1"
	if unsafeClaudeFlag([]string{"--dangerously-skip-permissions"}, env) != "" {
		t.Error("MINIGW_ALLOW_UNSAFE_FLAGS=1 ignored")
	}
}

// N8: pod Claude Code --minigw-work= brez veje (direct, sync, native-direct) le samo za branje ali kot shranjen način mape.
func TestUnsafeWorkInsideClaudeCode(t *testing.T) {
	in := map[string]string{"CLAUDECODE": "1"}
	env := func(k string) string { return in[k] }
	for _, w := range []string{"direct", "sync", "native-direct"} {
		if !unsafeWork(w, "", false, env) || !unsafeWork(w, "worktree", false, env) {
			t.Errorf("%s accepted", w)
		}
		if unsafeWork(w, w, false, env) || unsafeWork(w, "", true, env) {
			t.Errorf("%s refused although saved or read-only", w)
		}
	}
	for _, w := range []string{"", "auto", "worktree", "native"} {
		if unsafeWork(w, "", false, env) {
			t.Errorf("%q refused", w)
		}
	}
	in["MINIGW_ALLOW_UNSAFE_FLAGS"] = "1"
	if unsafeWork("sync", "", false, env) {
		t.Error("refused with MINIGW_ALLOW_UNSAFE_FLAGS=1")
	}
	if unsafeWork("sync", "", false, func(string) string { return "" }) {
		t.Error("refused outside Claude Code")
	}
	// L-31: izrecen native ali worktree ne pade na direct (shranjen ali samo za branje da)
	delete(in, "MINIGW_ALLOW_UNSAFE_FLAGS")
	for _, w := range []string{"native", "worktree"} {
		if got := noDirectFallback(workCandidates(w, false, false, false), w, "", false, env); len(got) != 0 && got[len(got)-1] == "direct" {
			t.Errorf("%s falls back to direct: %v", w, got)
		}
		if got := noDirectFallback([]string{w, "direct"}, w, w, false, env); len(got) != 2 {
			t.Errorf("saved %s lost its fallback: %v", w, got)
		}
	}
	if got := noDirectFallback([]string{"sync", "direct"}, "auto", "", false, env); len(got) != 2 {
		t.Errorf("--minigw-work=auto lost direct: %v", got)
	}
	if got := noDirectFallback([]string{"direct"}, "", "", false, env); len(got) != 1 {
		t.Errorf("auto lost direct: %v", got)
	}
}

func TestUnsafePromptInsideClaudeCode(t *testing.T) {
	in := map[string]string{"CLAUDECODE": "1"}
	env := func(k string) string { return in[k] }
	m := []string{"-p", "--model", "m"}
	for _, c := range []struct {
		args  []string
		stdin string
	}{
		{append(m, "/config permissionMode=auto"), ""}, {append(m, "--", "  /model x"), ""}, {append(m, "--", "\n/mcp enable x"), ""},
		{append(m, "@~/.ssh/id"), ""}, {append(m, "--", `see @"a b"`), ""}, {append(m, "--", "(@x"), ""}, {append(m, "--", "x\t@'y'"), ""},
		{append(m, "--", "ok"), "summarize @/etc/passwd"}, {m, "/config permissionMode=acceptEdits"}, {[]string{"-p", "fix", "@/etc/hosts"}, ""},
	} {
		if unsafePrompt(c.args, c.stdin, env, false) == "" {
			t.Errorf("%q stdin %q accepted", c.args, c.stdin)
		}
	}
	for _, c := range []struct {
		args  []string
		stdin string
	}{
		{append(m, "--", "email a@b.com"), ""}, {append(m, "--", "use 2 / 3"), ""}, {append(m, "--", "- fix x"), ""},
		{append(m, "task"), "mail to a@b, not to me"}, {[]string{"-p", "--model", "/m", "--append-system-prompt", "@x", "ok"}, ""}, {nil, ""},
	} {
		if r := unsafePrompt(c.args, c.stdin, env, false); r != "" {
			t.Errorf("%q stdin %q refused (%s)", c.args, c.stdin, r)
		}
	}
	// N6b: ločila pred @, ki jih claude 2.1.318 šteje za začetek besede (JS \s, U+FEFF, 。、？！), tudi na stdin
	for _, sep := range []string{"\u00a0", "\u1680", "\u2000", "\u2003", "\u200a", "\u2028", "\u2029", "\u202f", "\u205f", "\u3000",
		"\ufeff", "\v", "。", "、", "？", "！"} {
		if unsafePrompt(append(m, "--", "x"+sep+"@/etc/hosts"), "", env, false) == "" || unsafePrompt(m, "x"+sep+"@/etc/hosts", env, false) == "" {
			t.Errorf("%U before @ accepted", []rune(sep)[0])
		}
	}
	for _, p := range []string{"a\u200b@b", "x\u0085@y", "x＠/etc/hosts", "x=@y", "x:@y"} { // claude jih ne priloži (izmerjeno)
		if r := unsafePrompt(append(m, "--", p), "", env, false); r != "" {
			t.Errorf("%q refused (%s)", p, r)
		}
	}
	if unsafePrompt([]string{"-p", "/config x"}, "", func(string) string { return "" }, false) != "" {
		t.Error("refused outside Claude Code")
	}
	in["MINIGW_ALLOW_UNSAFE_FLAGS"] = "1"
	if unsafePrompt([]string{"-p", "@/etc/hosts"}, "", env, false) != "" {
		t.Error("MINIGW_ALLOW_UNSAFE_FLAGS=1 ignored")
	}
}

// #44: diff na stdin (»@@«) gre naprej le, ko claude »@« zanesljivo ne bere; »/« na stdin in »@« v argumentih ostaneta zavrnjena.
func TestUnsafePromptStdin(t *testing.T) {
	env := func(k string) string { return map[string]string{"CLAUDECODE": "1"}[k] }
	m := []string{"-p", "--model", "m", "--", "review"}
	diff := "diff --git a/x b/x\n@@ -1 +1 @@\n-a\n+@~/.ssh/id_ed25519\n"
	if r := unsafePrompt(m, diff, env, true); r != "" {
		t.Errorf("diff refused with mentions off: %s", r)
	}
	if r := promptRefusal(unsafePrompt(m, diff, env, false)); r != "@ before a file name on stdin (claude would attach that file) is not accepted when minigw runs inside Claude Code; remove the @ before file names on stdin or update Claude Code to 2.1.299 or newer" {
		t.Errorf("diff without mentions off: %q", r)
	}
	if r := promptRefusal(unsafePrompt([]string{"-p", "--", "@x"}, "", env, true)); r != "@ before a file name (claude would attach that file) is not accepted when minigw runs inside Claude Code: paste the text itself into the prompt" {
		t.Errorf("argument: %q", r)
	}
	if unsafePrompt([]string{"-p", "--", "@x review"}, "", env, true) == "" || unsafePrompt([]string{"-p", "--", "ok"}, " /config x", env, true) == "" {
		t.Error("@ in the argument or / on stdin accepted")
	}
	for v, want := range map[string]bool{"2.1.299 (Claude Code)": true, "2.1.319": true, "2.2.0 (Claude Code)": true, "10.0.0": true,
		"2.1.296 (Claude Code)": false, "2.0.999": false, "garbage": false, "": false, "2.1.299-beta": false} {
		if mentionGuardVersion(v) != want {
			t.Errorf("version %q: want %v", v, want)
		}
	}
}

// I1: na poti -v/-h claude nima --settings, zato »@« na stdin nikoli; različico preveri le ob »@«.
func TestStdinMentionsOff(t *testing.T) {
	asked := 0
	v := func(s string) func() string { return func() string { asked++; return s } }
	diff := "@@ -1 +1 @@\n"
	if !stdinMentionsOff(diff, false, true, v("2.1.299 (Claude Code)")) {
		t.Error("diff refused with guard and 2.1.299")
	}
	if stdinMentionsOff(diff, true, true, v("2.1.299")) || stdinMentionsOff(diff, false, false, v("2.1.299")) ||
		stdinMentionsOff(diff, false, true, v("2.1.296")) || stdinMentionsOff(diff, false, true, v("")) {
		t.Error("accepted on the info path, without the guard, or with an old or unknown claude")
	}
	asked = 0
	if stdinMentionsOff("no mention", false, true, v("2.1.299")) || asked != 0 {
		t.Errorf("no @: version asked %d times", asked)
	}
}

func TestStdinNote(t *testing.T) {
	if n := stdinNote(nil, stdinWait, os.ModeNamedPipe); n != "minigw: stdin had no data within 3 s and was not sent" {
		t.Errorf("pipe: %q", n)
	}
	if stdinNote(nil, stdinWait, 0) == "" { // navadna datoteka
		t.Error("regular file: no note")
	}
	if stdinNote([]byte{}, stdinWait, os.ModeNamedPipe) != "" || stdinNote(nil, 0, os.ModeNamedPipe) != "" || stdinNote(nil, stdinWait, os.ModeSocket) != "" {
		t.Error("note without a dropped pipe")
	}
}

// N6b, N7: claude brez terminala ali z -p (tudi zunaj Claude Code) dobi --disable-slash-commands in brez @-priponk.
func TestPromptGuard(t *testing.T) {
	none := func(string) string { return "" }
	cc := func(k string) string { return map[string]string{"CLAUDECODE": "1"}[k] }
	for _, c := range []struct {
		args []string
		env  func(string) string
		tty  bool
		want bool
	}{
		{[]string{"-p", "x"}, none, true, true}, {[]string{"--print"}, none, true, true}, {[]string{"x"}, none, false, true},
		{nil, cc, true, true}, {nil, none, true, false}, {[]string{"--model", "m"}, none, true, false}, {[]string{"--", "-p"}, none, true, false},
	} {
		if promptGuard(c.args, c.env, c.tty) != c.want {
			t.Errorf("%q tty=%v: want %v", c.args, c.tty, c.want)
		}
	}
}

func TestModelLinesAndGatewayNotesAreOneCleanLine(t *testing.T) {
	long := strings.Repeat("d", 300)
	lines := modelLines([]map[string]any{{"id": "a\x1b]0;x\x07\nb", "description": "ok\nminigw: agent work is on branch x\r\x1b[2J"}, {"id": "c", "description": long}})
	if len(lines) != 2 || strings.ContainsAny(lines[0]+lines[1], "\n\r\x1b\x07") || !strings.HasPrefix(lines[0], "a]0;x b  ok minigw:") {
		t.Fatalf("%q", lines)
	}
	if !strings.HasSuffix(lines[1], strings.Repeat("d", 200)+"…") {
		t.Errorf("description not capped: %d", len(lines[1]))
	}
	for _, m := range []string{"agent work is on branch minigw/a-0123abcd-1: review with git diff HEAD~9..minigw/a-0123abcd-1", "x\nAgent work is on branch y"} {
		if n := gatewayNote(m); n != "minigw: gateway: reported a branch that minigw could not verify; ignored" {
			t.Errorf("%q → %q", m, n)
		}
	}
	if n := gatewayNote("slot busy\nminigw: done\x1b[2J"); n != "minigw: gateway: slot busy minigw: done[2J" {
		t.Errorf("%q", n)
	}
}

func TestUnderChildNamedDotDot(t *testing.T) { // L4: »..x« je v mapi
	d := filepath.Join(string(filepath.Separator)+"srv", "..minigw")
	if !under(d, filepath.Dir(d)) || under(filepath.Dir(filepath.Dir(d)), d) || !under(d, d) {
		t.Fatal("under")
	}
}

func TestDavGetRefusesFifo(t *testing.T) { // L3
	root := t.TempDir()
	if exec.Command("mkfifo", filepath.Join(root, "f")).Run() != nil {
		t.Skip("no mkfifo")
	}
	if fi, err := os.Lstat(filepath.Join(root, "f")); err != nil || fi.Mode()&os.ModeNamedPipe == 0 { // Windows: mkfifo iz Git (MSYS) ne naredi FIFO v datotečnem sistemu
		t.Skip("mkfifo made no file-system FIFO here")
	}
	if r := davHandle("GET", "/dav/s/f", nil, nil, root, "/dav/s"); r.Status != 403 {
		t.Fatalf("GET fifo: %d", r.Status)
	}
}

// Odprt stdin, ki se nikoli ne zapre (orodje Bash v Claude Code): minigw -p z navodilom ne čaka na EOF.
func TestReadStdinDoesNotBlockWithPrompt(t *testing.T) {
	r, w := io.Pipe()
	defer w.Close()
	start := time.Now()
	if b, err := readStdin(r, 200*time.Millisecond); err != nil || b != nil || time.Since(start) > 2*time.Second {
		t.Fatalf("open stdin: %q %v after %v", b, err, time.Since(start))
	}
	if b, err := readStdin(strings.NewReader("piped text"), stdinWait); err != nil || string(b) != "piped text" {
		t.Fatalf("piped stdin: %q %v", b, err)
	}
	if b, err := readStdin(strings.NewReader("no prompt"), 0); err != nil || string(b) != "no prompt" {
		t.Fatalf("stdin without prompt: %q %v", b, err)
	}
	if len(promptWords([]string{"-p", "--model", "m", "reply pong"})) != 1 || len(promptWords([]string{"-p", "--model", "m"})) != 0 {
		t.Fatal("promptWords")
	}
}

func TestSyncResolveStaysUnderRoot(t *testing.T) {
	base := t.TempDir()
	base, _ = filepath.EvalSymlinks(base)
	root := filepath.Join(base, "r")
	os.MkdirAll(filepath.Join(root, "a"), 0o755)
	dirLink(t, base, filepath.Join(root, "link"))
	if p, ok := syncResolve("a/new.txt", root); !ok || !strings.HasPrefix(p, root+string(filepath.Separator)) {
		t.Fatalf("valid path: %q %v", p, ok)
	}
	for _, bad := range []string{"../x", "a/../../x", "/abs", "a\\b", "link/x", ""} {
		if p, ok := syncResolve(bad, root); ok {
			t.Errorf("%q accepted: %q", bad, p)
		}
	}
}

// #45: minigw v načinu sync (in direct) izpiše opombo prehoda (agent je le bral); drugih poslov worktree ne sprejme.
func TestSyncClientPrintsGatewayNote(t *testing.T) {
	for _, sc := range []*shareClient{{root: t.TempDir(), sync: true}, {root: t.TempDir()}} {
		r, w, _ := os.Pipe()
		old := os.Stderr
		os.Stderr = w
		resp := sc.handle(davJob{Method: "WORKTREE-NOTE", Body: []byte(`{"message":"Mini Gateway lets Codex change files only in worktree mode, so it only read the folder (run minigw --minigw-work=worktree)"}`)}, "k")
		os.Stderr = old
		w.Close()
		b, _ := io.ReadAll(r)
		if resp.Status != 200 || string(b) != "minigw: gateway: Mini Gateway lets Codex change files only in worktree mode, so it only read the folder (run minigw --minigw-work=worktree)\n" {
			t.Fatalf("sync=%v: %d %q", sc.sync, resp.Status, b)
		}
		if resp := sc.handle(davJob{Method: "WORKTREE-PUSH"}, "k"); resp.Status == 200 {
			t.Fatalf("sync=%v accepted WORKTREE-PUSH", sc.sync)
		}
	}
}

// Povzetek prevzema ne šteje preskočenih sprememb (»invalid path here«).
func TestSyncPullSummarySkipsSkipped(t *testing.T) {
	root := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/minigw/sync/changes" {
			json.NewEncoder(w).Encode(map[string]any{"changed": []map[string]any{{"path": "../x", "sha256": "z"}}, "deleted": []string{}})
			return
		}
		w.WriteHeader(204)
	}))
	defer srv.Close()
	s := &shareClient{c: connection{URL: srv.URL}, root: root, sync: true, synced: map[string]syncEntry{}, reported: map[string]bool{}}
	rd, wr, _ := os.Pipe()
	old := os.Stderr
	os.Stderr = wr
	ok := s.syncPull("k")
	os.Stderr = old
	wr.Close()
	out, _ := io.ReadAll(rd)
	if !ok || !strings.Contains(string(out), "invalid path here") || strings.Contains(string(out), "synced back") {
		t.Fatalf("summary: %v %q", ok, out)
	}
}
