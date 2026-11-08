package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestNativeSMBPath(t *testing.T) {
	for _, c := range []struct{ src, mp, cwd, share, rel string }{
		{"//user@server._smb._tcp.local/Java", "/mnt/java", "/mnt/java/projects/apps", "Java", "projects/apps"},
		{"//h/Jane%20Public/sub", "/mnt/p", "/mnt/p", "Jane Public", "sub"},
		{`\\mini\Java`, "/mnt/j", "/mnt/j/x", "Java", "x"},
	} {
		if s, r, ok := smbPath(c.src, c.mp, c.cwd); !ok || s != c.share || r != c.rel {
			t.Errorf("smbPath(%q, %q, %q) = %q %q %v", c.src, c.mp, c.cwd, s, r, ok)
		}
	}
	if _, _, ok := smbPath("//h/Java", "/mnt/java", "/mnt/javax/p"); ok {
		t.Error("folder outside the mount")
	}
	if _, _, ok := smbPath("/dev/sda1", "/", "/home/x"); ok {
		t.Error("local disk")
	}
	info := "36 25 0:44 / /mnt/java rw - cifs //mini/Java rw\n" +
		"37 25 0:45 / /mnt/java/sub\\040dir rw shared:1 - smb3 //mini/Other rw\n" +
		"38 25 8:1 / / rw - ext4 /dev/sda1 rw\n"
	if src, mp, ok := smbFromMountinfo(info, "/mnt/java/sub dir/p"); !ok || src != "//mini/Other" || mp != "/mnt/java/sub dir" {
		t.Errorf("mountinfo: %q %q %v", src, mp, ok)
	}
	if _, _, ok := smbFromMountinfo(info, "/home/x"); ok {
		t.Error("ext4 is not SMB")
	}
}

var nativeDiscards atomic.Int32 // DELETE /minigw/native v testih

// fakeNativeRouter: kot router Mac: označba v `routerDir` za shemo Java/projects, odgovor `file` (lažni router).
func fakeNativeRouter(t *testing.T, routerDir string, status int, file string) (connection, *string) {
	var marker string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" { // odjemalec označbe ni videl: izziv ven
			nativeDiscards.Add(1)
			w.WriteHeader(204)
			return
		}
		var b struct{ Share, Path string }
		json.NewDecoder(r.Body).Decode(&b)
		if status != 200 {
			w.WriteHeader(status)
			w.Write([]byte(`{"error":{"message":"not in the allowed folders"}}`))
			return
		}
		if b.Share != "Java" || b.Path != "projects" {
			t.Errorf("claim %q %q", b.Share, b.Path)
		}
		id := "0123456789abcdef0123456789abcdef"
		marker = "nonce-" + id
		os.WriteFile(filepath.Join(routerDir, ".minigw-native-"+id), []byte(marker), 0o644)
		if file == "" {
			file = ".minigw-native-" + id
		}
		json.NewEncoder(w).Encode(map[string]string{"id": id, "file": file})
	}))
	t.Cleanup(srv.Close)
	return connection{URL: srv.URL, Token: "tok"}, &marker
}

func TestNativeProof(t *testing.T) {
	markerTries = 1
	root := t.TempDir()
	share := filepath.Join(root, "Java")
	project := filepath.Join(share, "projects")
	os.MkdirAll(project, 0o755)

	// Prava mapa (»priklop« je ista mapa): dokaz je vsebina označbe.
	t.Setenv("MINIGW_NATIVE_MOUNT", "Java="+share)
	c, marker := fakeNativeRouter(t, project, 200, "")
	if p, why := nativeProof(c, project); p == nil || p["x-native-proof"] != *marker {
		t.Fatalf("true positive: %v %s", p, why)
	}

	// Ista shema in pot, druga mapa: označbe tukaj ni → brez dokaza.
	spoof := filepath.Join(root, "local", "Java")
	os.MkdirAll(filepath.Join(spoof, "projects"), 0o755)
	t.Setenv("MINIGW_NATIVE_MOUNT", "Java="+spoof)
	c, _ = fakeNativeRouter(t, project, 200, "")
	before := nativeDiscards.Load()
	if p, why := nativeProof(c, filepath.Join(spoof, "projects")); p != nil || !strings.Contains(why, "check file is not visible") || nativeDiscards.Load() != before+1 {
		t.Fatalf("spoof: %v %s", p, why)
	}

	// Mapa zunaj dovoljenih map routerja: router zavrne, minigw ne bere ničesar.
	t.Setenv("MINIGW_NATIVE_MOUNT", "Java="+share)
	c, _ = fakeNativeRouter(t, project, 403, "")
	if p, why := nativeProof(c, project); p != nil || why != "not in the allowed folders" {
		t.Fatalf("allow-list: %v %s", p, why)
	}

	// Lažni router, ki bi rad prebral drugo datoteko: ime ne ustreza → nič.
	os.WriteFile(filepath.Join(project, "secret"), []byte("s"), 0o600)
	c, _ = fakeNativeRouter(t, project, 200, "secret")
	if p, _ := nativeProof(c, project); p != nil {
		t.Fatal("read a file the router named")
	}

	t.Setenv("MINIGW_NATIVE_MOUNT", "")
	if p, why := nativeProof(c, project); p != nil || !strings.Contains(why, "not on an SMB share") {
		t.Fatalf("local folder: %s", why)
	}
}

func TestRunOutput(t *testing.T) {
	var out, errOut bytes.Buffer
	body := "o aGVsbG8K\np\ne ZXJyCg==\nx 3\n"
	if code := runOutput(strings.NewReader(body), &out, &errOut); code != 3 || out.String() != "hello\n" || errOut.String() != "err\n" {
		t.Fatalf("code %d out %q err %q", code, out.String(), errOut.String())
	}
	if code := runOutput(strings.NewReader("o aGk=\n"), io.Discard, io.Discard); code != 255 {
		t.Fatalf("cut off: %d", code)
	}
}

// Go router (minigw serve) native še nima: 404, minigw sporoči »does not support native mode«.
func TestServeHasNoNative(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	s, k := testServer(t, func(c *serveConfig) { c.AllowAnyDevice = true })
	dev := &device{key: priv, name: "Air"}
	for _, p := range []string{"/minigw/native", "/minigw/run"} {
		resp := pipeRequest(t, s, k.PublicKey().Bytes(), dev, "POST "+p+" HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer tok\r\nContent-Length: 2\r\n\r\n{}")
		if !strings.HasPrefix(resp, "HTTP/1.1 404") {
			t.Errorf("%s: %.60q", p, resp)
		}
	}
}

func TestSharedFolderModes(t *testing.T) {
	old := parseSharedFolders("/data/a\r\n\n  /data/b  \n") // starejša datoteka: le poti
	if len(old) != 2 || old[0] != (sharedFolder{Path: "/data/a"}) || old[1] != (sharedFolder{Path: "/data/b"}) {
		t.Fatalf("old format: %+v", old)
	}
	list := []sharedFolder{{Path: "/mnt/java/projects", Work: "native"}, {Path: "/mnt/java", Work: "sync", ReadOnly: true}, {Path: "/mnt/java/my folder"}}
	text := formatSharedFolders(list)
	// ista oblika kot SharedFolders.format v minigw za macOS (ista datoteka)
	if text != "/mnt/java/projects\t--work=native\n/mnt/java\t--work=sync --ro\n/mnt/java/my folder\n" {
		t.Fatalf("format: %q", text)
	}
	if got := parseSharedFolders(text); len(got) != 3 || got[0] != list[0] || got[1] != list[1] || got[2] != list[2] {
		t.Fatalf("round trip: %+v", got)
	}
	if got := parseSharedFolders("/p\t--work=bogus --ro"); got[0] != (sharedFolder{Path: "/p", ReadOnly: true}) {
		t.Fatalf("bogus mode: %+v", got)
	}
	apps := lookupSharedFolder(list, "/mnt/java/projects/apps") // najbolj natančna mapa
	if apps == nil || apps.Work != "native" || lookupSharedFolder(list, "/mnt/javax") != nil {
		t.Fatalf("lookup: %+v", apps)
	}
	java := lookupSharedFolder(list, "/mnt/java/x")
	for _, c := range []struct {
		flag  string
		saved *sharedFolder
		work  string
		ro    bool
	}{
		{"", apps, "native", false},
		{"sync", apps, "sync", false}, // zastavica zmaga
		{"", nil, "auto", false},
		{"", java, "sync", true},
		{"native", java, "direct", true}, // M1: shranjen --ro ostane; native samo za branje → direct samo za branje
		{"native", nil, "direct", true},
	} {
		if w, ro := workMode(c.flag, c.saved, c.saved == nil && c.flag == "native"); w != c.work || ro != c.ro {
			t.Errorf("workMode(%q, %+v) = %s %v", c.flag, c.saved, w, ro)
		}
	}
}

func TestShareAddSavesMode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("AppData", filepath.Join(home, "AppData"))
	proj := filepath.Join(t.TempDir(), "projects")
	os.MkdirAll(proj, 0o755)
	shareCommand([]string{"add", "--work=native", proj})
	shareCommand([]string{"add", proj, "--work=sync", "--ro"}) // znova: zamenja način, ne podvoji
	if got := sharedFolders(); len(got) != 1 || got[0] != (sharedFolder{Path: proj, Work: "sync", ReadOnly: true}) {
		t.Fatalf("saved: %+v", got)
	}
	shareCommand([]string{"remove", proj})
	if got := sharedFolders(); len(got) != 0 {
		t.Fatalf("removed: %+v", got)
	}
}

func TestShareRefusesLineBreaksAndTabs(t *testing.T) { // L6
	if !validSharePath("/mnt/java/a b") || validSharePath("/x\t--work=native") || validSharePath("/x\n/y") || validSharePath("/x\r") {
		t.Fatal("validSharePath")
	}
}

func TestReadMarkerRefusesLinksAndBigFiles(t *testing.T) { // M2
	d := t.TempDir()
	os.WriteFile(filepath.Join(d, "secret"), []byte("s3cret"), 0o600)
	os.Symlink(filepath.Join(d, "secret"), filepath.Join(d, "link"))
	os.WriteFile(filepath.Join(d, "big"), bytes.Repeat([]byte("a"), 129), 0o600)
	os.WriteFile(filepath.Join(d, "ok"), []byte("nonce\n"), 0o600)
	os.Mkdir(filepath.Join(d, "dir"), 0o700)
	if _, ok := readMarker(filepath.Join(d, "link")); ok {
		t.Error("followed a symlink")
	}
	if _, ok := readMarker(filepath.Join(d, "big")); ok {
		t.Error("read more than 128 bytes")
	}
	if _, ok := readMarker(filepath.Join(d, "dir")); ok {
		t.Error("read a folder")
	}
	if v, ok := readMarker(filepath.Join(d, "ok")); !ok || v != "nonce" {
		t.Errorf("regular file: %q %v", v, ok)
	}
}

func TestTerminalSafeKeepsOnlyColours(t *testing.T) { // L1
	in := "\x1b[1;31mred\x1b[0m\r\n\x1b]0;title\x07\x1b]52;c;Zm9v\x1b\\\x1bP1$q\x1b\\\x1b[6n\x1b[2J\x1b[?1049h\x07ok\t\x1b"
	if got := string(terminalSafe([]byte(in))); got != "\x1b[1;31mred\x1b[0m\r\nok\t" {
		t.Fatalf("%q", got)
	}
	if got := string(terminalSafe([]byte("a\u009b2Jb\u009d0;x\u0007č€"))); got != "a2Jb0;xč€" { // N4: C1 v UTF-8
		t.Fatalf("C1: %q", got)
	}
}

func TestMountinfoRoot(t *testing.T) { // I3: priklop podmape (bind) in prefixpath
	info := "40 25 0:44 /proj /mnt/e rw - cifs //mini/Java rw\n41 25 0:45 /p /mnt/p rw - cifs //mini/Java/p rw\n"
	if src, _, _ := smbFromMountinfo(info, "/mnt/e/x"); src != "//mini/Java/proj" {
		t.Errorf("bind: %q", src)
	}
	if src, _, _ := smbFromMountinfo(info, "/mnt/p"); src != "//mini/Java/p" {
		t.Errorf("prefixpath: %q", src)
	}
}

func TestSafeWriterAcrossChunks(t *testing.T) { // P3
	var b bytes.Buffer
	w := &safeWriter{w: &b}
	w.Write([]byte{'a', 0xc2})
	w.Write([]byte{0x9b, '2', 'J', 'b'}) // C1 CSI razdeljen na dva kosa
	w.Write([]byte{0xc4})
	w.Write([]byte{0x8d}) // »č«, razdeljen drugje, ostane cel
	if got := b.String(); got != "a2Jbč" {
		t.Fatalf("%q", got)
	}
	if got := terminalSafe([]byte{'a', 0xc2}); string(got) != "a" {
		t.Fatalf("lone C2: %q", got)
	}
}
