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
	"testing"
)

func TestNativeSMBPath(t *testing.T) {
	for _, c := range []struct{ src, mp, cwd, share, rel string }{
		{"//uros@mini._smb._tcp.local/Java", "/mnt/java", "/mnt/java/eureko-modules/apps", "Java", "eureko-modules/apps"},
		{"//h/Uros%20Public/sub", "/mnt/p", "/mnt/p", "Uros Public", "sub"},
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

// fakeNativeRouter: kot router Mac: označba v `routerDir` za shemo Java/eureko-modules, odgovor `file` (lažni router).
func fakeNativeRouter(t *testing.T, routerDir string, status int, file string) (connection, *string) {
	var marker string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b struct{ Share, Path string }
		json.NewDecoder(r.Body).Decode(&b)
		if status != 200 {
			w.WriteHeader(status)
			w.Write([]byte(`{"error":{"message":"not in the allowed folders"}}`))
			return
		}
		if b.Share != "Java" || b.Path != "eureko-modules" {
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
	project := filepath.Join(share, "eureko-modules")
	os.MkdirAll(project, 0o755)

	// Prava mapa (»priklop« je ista mapa): dokaz je vsebina označbe.
	t.Setenv("MINIGW_NATIVE_MOUNT", "Java="+share)
	c, marker := fakeNativeRouter(t, project, 200, "")
	if p, why := nativeProof(c, project); p == nil || p["x-native-proof"] != *marker {
		t.Fatalf("true positive: %v %s", p, why)
	}

	// Ista shema in pot, druga mapa: označbe tukaj ni → brez dokaza.
	spoof := filepath.Join(root, "local", "Java")
	os.MkdirAll(filepath.Join(spoof, "eureko-modules"), 0o755)
	t.Setenv("MINIGW_NATIVE_MOUNT", "Java="+spoof)
	c, _ = fakeNativeRouter(t, project, 200, "")
	if p, why := nativeProof(c, filepath.Join(spoof, "eureko-modules")); p != nil || !strings.Contains(why, "check file is not visible") {
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
