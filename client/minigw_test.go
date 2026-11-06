package main

import (
	"os"
	"path/filepath"
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
	if shareWanted(sub, false) {
		t.Fatal("folder outside home shared without share add")
	}
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
	shareCommand([]string{"remove", proj})
	if shareWanted(sub, false) {
		t.Fatal("removed folder still shared")
	}
}
