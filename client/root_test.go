package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Resolve first, then replace a parent: the operation must enforce confinement again.
func TestRootOperationsRefuseSwappedParent(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "x"), []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "source"), []byte("inside"), 0o600); err != nil {
		t.Fatal(err)
	}
	rooted, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer rooted.Close()
	if _, _, ok := davResolve("/s/d/x", "/s", root); !ok {
		t.Fatal("initial path refused")
	}
	if err := os.Rename(filepath.Join(root, "d"), filepath.Join(root, "old")); err != nil {
		t.Fatal(err)
	}
	target, err := filepath.Rel(root, outside)
	if err != nil {
		t.Fatal(err)
	}
	dirLink(t, target, filepath.Join(root, "d"))
	tests := []struct {
		name string
		op   func() error
	}{
		{"PUT", func() error { return rootWriteAtomic(rooted, "d/x", []byte("bad")) }},
		{"sync write", func() error { return syncWrite(rooted, "d/x", []byte("bad"), 0, false, false) }},
		{"COPY source", func() error { return rootCopyTree(rooted, "d/x", "copy") }},
		{"COPY destination", func() error { return rootCopyTree(rooted, "source", "d/copy") }},
		{"MOVE source", func() error { return rooted.Rename("d/x", "moved") }},
		{"MOVE destination", func() error { return rooted.Rename("source", "d/moved") }},
		{"DELETE", func() error { return rooted.RemoveAll("d/x") }},
		{"MKCOL", func() error { return rooted.Mkdir("d/new", 0o755) }},
		{"LOCK", func() error {
			f, err := rooted.OpenFile("d/new", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
			if f != nil {
				f.Close()
			}
			return err
		}},
		{"GET", func() error {
			f, err := rooted.Open("d/x")
			if f != nil {
				f.Close()
			}
			return err
		}},
		{"PROPFIND", func() error { _, err := rootReadDir(rooted, "d"); return err }},
		{"mode", func() error { return rootChmod(rooted, "d/x", 0o777) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.op(); err == nil {
				t.Fatal("escaped root after successful path check")
			}
		})
	}
	if _, ok := syncEntryOf(rooted, "d/x", "d/x", nil); ok {
		t.Error("sync read escaped")
	}
	b, err := os.ReadFile(filepath.Join(outside, "x"))
	if err != nil || string(b) != "outside" {
		t.Fatalf("outside changed: %q, %v", b, err)
	}
	entries, err := os.ReadDir(outside)
	if err != nil || len(entries) != 1 {
		t.Fatalf("outside entries: %v, %v", entries, err)
	}
}

func TestSyncApplyRefusesParentSwappedDuringFetch(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	data := []byte("gateway")
	ch := syncChanges{Changed: []syncEntry{{Path: "d/x", SHA256: sha256Hex(data), Size: int64(len(data))}}}
	notes, done := syncApply(ch, root, map[string]syncEntry{}, func(string) []byte {
		if err := os.Rename(filepath.Join(root, "d"), filepath.Join(root, "old")); err != nil {
			t.Fatal(err)
		}
		target, err := filepath.Rel(root, outside)
		if err != nil {
			t.Fatal(err)
		}
		dirLink(t, target, filepath.Join(root, "d"))
		return data
	})
	if len(done) != 0 || !strings.Contains(strings.Join(notes, "\n"), "cannot write") {
		t.Fatalf("notes=%v done=%v", notes, done)
	}
	if _, err := os.Stat(filepath.Join(outside, "x")); !os.IsNotExist(err) {
		t.Fatalf("outside file: %v", err)
	}
}

func TestSyncPutRefusesParentSwappedBeforeWrite(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	h := newShareHub()
	key, ok := h.register("s1", "/c", "", false, root, "dev")
	if !ok {
		t.Fatal("register")
	}
	data := []byte("gateway")
	entry := syncEntry{Path: "d/x", SHA256: sha256Hex(data), Size: int64(len(data))}
	if _, _, ok := h.syncManifest(key, []syncEntry{entry}, true, "dev"); !ok {
		t.Fatal("manifest")
	}
	write := syncPutWrite
	t.Cleanup(func() { syncPutWrite = write })
	syncPutWrite = func(rooted *os.Root, p string, b []byte, m int64, x, k bool) error {
		if err := os.Rename(filepath.Join(root, "d"), filepath.Join(root, "old")); err != nil {
			t.Fatal(err)
		}
		dirLink(t, outside, filepath.Join(root, "d"))
		return write(rooted, p, b, m, x, k)
	}
	if status := h.syncPut(key, "d/x", data, "dev"); status != 500 {
		t.Fatalf("PUT: %d", status)
	}
	if _, err := os.Stat(filepath.Join(outside, "x")); !os.IsNotExist(err) {
		t.Fatalf("outside file: %v", err)
	}
}

func TestDavRelativeLinkInsideRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, "d", filepath.Join(root, "link"))
	if r := davHandle("PUT", "/s/link/x", nil, []byte("inside"), root, "/s"); r.Status != 201 {
		t.Fatalf("PUT: %d", r.Status)
	}
	if r := davHandle("GET", "/s/link/x", nil, nil, root, "/s"); r.Status != 200 || string(r.Body) != "inside" {
		t.Fatalf("GET: %+v", r)
	}
}

func TestDavRefusesAbsoluteLinkInsideRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	dirLink(t, filepath.Join(root, "d"), filepath.Join(root, "link"))
	if r := davHandle("PUT", "/s/link/x", nil, []byte("inside"), root, "/s"); r.Status < 400 || !strings.Contains(string(r.Body), linkRefusal) {
		t.Fatalf("PUT: %d", r.Status)
	}
	if _, err := os.Stat(filepath.Join(root, "d", "x")); !os.IsNotExist(err) {
		t.Fatalf("unexpected file: %v", err)
	}
}

func TestMutationsFollowRelativeLinkInsideRoot(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "d"), 0700); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, "d", filepath.Join(root, "link"))
	if r := davHandle("PUT", "/s/link/file", nil, []byte("dav"), root, "/s"); r.Status != 201 {
		t.Fatalf("PUT: %+v", r)
	}
	data := []byte("sync")
	notes, done := syncApply(syncChanges{Changed: []syncEntry{{Path: "link/new", SHA256: sha256Hex(data), Size: int64(len(data))}}}, root,
		map[string]syncEntry{}, func(string) []byte { return data })
	if len(notes) != 0 || len(done) != 1 {
		t.Fatalf("notes=%v done=%v", notes, done)
	}
	for name, want := range map[string]string{"file": "dav", "new": "sync"} {
		if b, err := os.ReadFile(filepath.Join(root, "d", name)); err != nil || string(b) != want {
			t.Fatalf("%s: %q %v", name, b, err)
		}
	}
}

func TestSyncAbsoluteLinkExplainsRefusal(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "d"), 0700); err != nil {
		t.Fatal(err)
	}
	dirLink(t, filepath.Join(root, "d"), filepath.Join(root, "link"))
	data := []byte("gateway")
	notes, done := syncApply(syncChanges{Changed: []syncEntry{{Path: "link/new", SHA256: sha256Hex(data)}}}, root,
		map[string]syncEntry{}, func(string) []byte { return data })
	if len(done) != 0 || !strings.Contains(strings.Join(notes, "\n"), linkRefusal) {
		t.Fatalf("notes=%v done=%v", notes, done)
	}
	if _, err := os.Stat(filepath.Join(root, "d", "new")); !os.IsNotExist(err) {
		t.Fatalf("absolute link target changed: %v", err)
	}
}

func TestSyncPutAllowsUsersProtectedFiles(t *testing.T) {
	h := newShareHub()
	root := t.TempDir()
	key, ok := h.register("s1", "/c", "", false, root, "dev")
	if !ok {
		t.Fatal("register")
	}
	data := []byte("user settings")
	for _, p := range []string{".claude/settings.json", ".claude/hooks/user", ".envrc", ".vscode/tasks.json"} {
		e := syncEntry{Path: p, Size: int64(len(data)), SHA256: sha256Hex(data)}
		if _, _, ok := h.syncManifest(key, []syncEntry{e}, true, "dev"); !ok {
			t.Fatal("manifest")
		}
		if status := h.syncPut(key, p, data, "dev"); status != 204 {
			t.Fatalf("upload %s: %d", p, status)
		}
		if b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(p))); err != nil || string(b) != string(data) {
			t.Fatalf("uploaded %s: %q %v", p, b, err)
		}
	}
}

func TestSyncApplyRefusesProtectedParentSwappedDuringFetch(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"d", ".claude"} {
		if err := os.Mkdir(filepath.Join(root, d), 0700); err != nil {
			t.Fatal(err)
		}
	}
	data := []byte("gateway")
	ch := syncChanges{Changed: []syncEntry{{Path: "d/settings.json", SHA256: sha256Hex(data), Size: int64(len(data))}}}
	notes, done := syncApply(ch, root, map[string]syncEntry{}, func(string) []byte {
		if err := os.Rename(filepath.Join(root, "d"), filepath.Join(root, "old")); err != nil {
			t.Fatal(err)
		}
		symlinkOrSkip(t, ".claude", filepath.Join(root, "d"))
		return data
	})
	if len(done) != 0 || !strings.Contains(strings.Join(notes, "\n"), "protected path") {
		t.Fatalf("notes=%v done=%v", notes, done)
	}
	if _, err := os.Stat(filepath.Join(root, ".claude", "settings.json")); !os.IsNotExist(err) {
		t.Fatalf("protected file: %v", err)
	}
}

func TestDavRefusesProtectedParentSwappedBeforeMutation(t *testing.T) {
	for _, method := range []string{"PUT", "MKCOL", "DELETE", "LOCK", "MOVE", "COPY", "MOVE source"} {
		t.Run(method, func(t *testing.T) {
			root := t.TempDir()
			for _, d := range []string{"d", ".claude"} {
				if err := os.Mkdir(filepath.Join(root, d), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(filepath.Join(root, "source"), []byte("safe"), 0600); err != nil {
				t.Fatal(err)
			}
			// An existing protected target must also survive DELETE and MOVE of the source.
			if method == "DELETE" || method == "MOVE source" {
				for _, d := range []string{"d", ".claude"} {
					if err := os.WriteFile(filepath.Join(root, d, "settings.json"), []byte("original"), 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			old := davBeforeMutation
			t.Cleanup(func() { davBeforeMutation = old })
			davBeforeMutation = func() {
				if err := os.Rename(filepath.Join(root, "d"), filepath.Join(root, "old")); err != nil {
					t.Fatal(err)
				}
				symlinkOrSkip(t, ".claude", filepath.Join(root, "d"))
			}
			p := "/s/d/settings.json"
			headers := map[string]string{}
			verb := method
			if method == "MOVE" || method == "COPY" {
				p = "/s/source"
				headers["destination"] = "/s/d/settings.json"
			}
			if method == "MOVE source" {
				verb = "MOVE"
				headers["destination"] = "/s/moved"
			}
			r := davHandle(verb, p, headers, []byte("bad"), root, "/s")
			if r.Status < 400 {
				t.Fatalf("mutation accepted: %+v", r)
			}
			b, err := os.ReadFile(filepath.Join(root, ".claude", "settings.json"))
			if method == "DELETE" || method == "MOVE source" {
				if err != nil || string(b) != "original" {
					t.Fatalf("protected changed: %q %v", b, err)
				}
			} else if !os.IsNotExist(err) {
				t.Fatalf("protected created: %q %v", b, err)
			}
		})
	}
}

func TestMutationUsesPinnedParent(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"d", ".claude"} {
		if err := os.Mkdir(filepath.Join(root, d), 0700); err != nil {
			t.Fatal(err)
		}
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	parent, name, err := mutationParent(r, "d/settings.json")
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	if err := os.Rename(filepath.Join(root, "d"), filepath.Join(root, "old")); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, ".claude", filepath.Join(root, "d"))
	if err := parent.WriteFile(name, []byte("safe"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, ".claude", name)); !os.IsNotExist(err) {
		t.Fatalf("protected file: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(root, "old", name)); err != nil || string(b) != "safe" {
		t.Fatalf("pinned file: %q %v", b, err)
	}
}

func TestRootChmodRefusesProtectedParentSwap(t *testing.T) {
	root := t.TempDir()
	for _, d := range []string{"d", ".claude"} {
		if err := os.Mkdir(filepath.Join(root, d), 0700); err != nil {
			t.Fatal(err)
		}
	}
	target := filepath.Join(root, ".claude", "settings.json")
	if err := os.WriteFile(target, []byte("safe"), 0400); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(target, 0600) })
	before, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if protectedCanonical(filepath.Join(root, "d", "settings.json"), root) {
		t.Fatal("initial refusal")
	}
	if err := os.Rename(filepath.Join(root, "d"), filepath.Join(root, "old")); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, ".claude", filepath.Join(root, "d"))
	if err := rootChmod(r, "d/settings.json", 0777); err == nil {
		t.Fatal("chmod accepted")
	}
	st, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != before.Mode().Perm() {
		t.Fatalf("mode changed: %v", st.Mode())
	}
}

func TestMutationOpenRefusesFinalLink(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "target"), []byte("safe"), 0600); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, "target", filepath.Join(root, "link"))
	r, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	f, err := mutationOpenFile(r, "link", os.O_WRONLY|os.O_TRUNC, 0600)
	if f != nil {
		f.Close()
	}
	if err == nil {
		t.Fatal("followed final link")
	}
	if b, err := os.ReadFile(filepath.Join(root, "target")); err != nil || string(b) != "safe" {
		t.Fatalf("target changed: %q %v", b, err)
	}
}

func TestTrustedIgnoreOpensExternalLink(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	target := filepath.Join(outside, "ignore")
	if err := os.WriteFile(target, []byte("ignored.txt\n"), 0600); err != nil {
		t.Fatal(err)
	}
	symlinkOrSkip(t, target, filepath.Join(root, ".gitignore"))
	if !ignored(loadIgnore(root, true), "ignored.txt", false) {
		t.Fatal("trusted external ignore not loaded")
	}
	if ignored(loadIgnore(root, false), "ignored.txt", false) {
		t.Fatal("untrusted external ignore loaded")
	}
}

func TestRemoveOldCopiesUsesPinnedRoot(t *testing.T) {
	base := t.TempDir()
	root, outside := filepath.Join(base, "copies"), filepath.Join(base, "outside")
	old := time.Now().Add(-15 * 24 * time.Hour)
	for _, d := range []string{root, outside} {
		p := filepath.Join(d, "dev", "old")
		if err := os.MkdirAll(p, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	r, err := os.OpenRoot(root)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	saved := filepath.Join(base, "saved")
	if err := os.Rename(root, saved); err != nil {
		t.Fatal(err)
	}
	dirLink(t, outside, root)
	removeOldCopiesAt(r, root, nil, time.Now())
	if _, err := os.Stat(filepath.Join(saved, "dev", "old")); !os.IsNotExist(err) {
		t.Fatalf("old copy not removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "dev", "old")); err != nil {
		t.Fatalf("outside copy removed: %v", err)
	}
}
