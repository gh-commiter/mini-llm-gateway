package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// sshPublicLine, sshSign: ključ in podpis SSHSIG, kot ju naredi ssh-keygen (-Y sign, sha512), za teste.
func sshPublicLine(pub ed25519.PublicKey) string {
	var b bytes.Buffer
	putSSHString(&b, []byte("ssh-ed25519"))
	putSSHString(&b, pub)
	return "ssh-ed25519 " + base64.StdEncoding.EncodeToString(b.Bytes())
}

func sshSign(priv ed25519.PrivateKey, msg []byte, ns string) []byte {
	var pubBlob, sigBlob, signed, out bytes.Buffer
	putSSHString(&pubBlob, []byte("ssh-ed25519"))
	putSSHString(&pubBlob, priv.Public().(ed25519.PublicKey))
	h := sha512.Sum512(msg)
	signed.WriteString("SSHSIG")
	putSSHString(&signed, []byte(ns))
	putSSHString(&signed, nil)
	putSSHString(&signed, []byte("sha512"))
	putSSHString(&signed, h[:])
	putSSHString(&sigBlob, []byte("ssh-ed25519"))
	putSSHString(&sigBlob, ed25519.Sign(priv, signed.Bytes()))
	out.WriteString("SSHSIG")
	binary.Write(&out, binary.BigEndian, uint32(1))
	putSSHString(&out, pubBlob.Bytes())
	putSSHString(&out, []byte(ns))
	putSSHString(&out, nil)
	putSSHString(&out, []byte("sha512"))
	putSSHString(&out, sigBlob.Bytes())
	return []byte("-----BEGIN SSH SIGNATURE-----\n" + base64.StdEncoding.EncodeToString(out.Bytes()) + "\n-----END SSH SIGNATURE-----\n")
}

// fakeRelease: izdaja na GitHubu (TLS) s sredstvom pladnja in SHA256SUMS; sum "" = prava vsota.
func fakeRelease(t *testing.T, sum string, redirectTo string) []byte {
	t.Helper()
	asset := "minigw-tray-windows-" + runtime.GOARCH + ".exe"
	body := []byte("fake tray " + runtime.GOARCH)
	if sum == "" {
		h := sha256.Sum256(body)
		sum = hex.EncodeToString(h[:])
	}
	sums := fmt.Sprintf("%s  minigw-windows-amd64.exe\n%s  %s\n", strings.Repeat("0", 64), sum, asset)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader) // #8: podpis s testnim ključem, pripetim samo v testu
	oldKeys := releaseKeys
	t.Cleanup(func() { releaseKeys = oldKeys })
	releaseKeys = []string{sshPublicLine(pub)}
	sig := sshSign(priv, []byte(sums), releaseNamespace)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/SHA256SUMS.sig"):
			w.Write(sig)
		case strings.HasSuffix(r.URL.Path, "/SHA256SUMS"):
			io.WriteString(w, sums)
		case redirectTo != "":
			http.Redirect(w, r, redirectTo, http.StatusFound)
		case strings.HasSuffix(r.URL.Path, "/"+asset):
			w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	oldBase, oldHosts, oldHTTP := releaseBase, releaseHosts, releaseHTTP
	t.Cleanup(func() { releaseBase, releaseHosts, releaseHTTP = oldBase, oldHosts, oldHTTP })
	releaseBase = srv.URL + "/gh-commiter/mini-llm-gateway/releases/download/v" + version
	releaseHosts = map[string]bool{u.Hostname(): true, "objects.githubusercontent.com": true}
	releaseHTTP = srv.Client()
	releaseHTTP.CheckRedirect = releaseRedirect
	return body
}

func TestReleaseDownloadVerified(t *testing.T) {
	asset := "minigw-tray-windows-" + runtime.GOARCH + ".exe"
	dst := filepath.Join(t.TempDir(), "minigw-tray.exe")
	body := fakeRelease(t, "", "")
	if err := downloadVerified(asset, dst); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(dst); string(got) != string(body) {
		t.Fatalf("downloaded: %q", got)
	}
	fakeRelease(t, strings.Repeat("a", 64), "") // napačna vsota: nič ne ostane
	os.Remove(dst)
	if err := downloadVerified(asset, dst); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("bad sum: %v", err)
	}
	if _, err := os.Stat(dst); err == nil {
		t.Fatal("file kept after a bad sum")
	}
	fakeRelease(t, "", "https://evil.example/tray.exe") // preusmeritev na tuj gostitelj
	if err := downloadVerified(asset, dst); err == nil || !strings.Contains(err.Error(), "refused redirect") {
		t.Fatalf("foreign redirect: %v", err)
	}
	releaseBase = "http://127.0.0.1:1/x" // le https
	if err := downloadVerified(asset, dst); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("plain http: %v", err)
	}
}

func TestInstallFileAndPath(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	os.WriteFile(src, []byte("program"), 0o755)
	dst := filepath.Join(dir, "sub", "minigw.exe")
	if err := installFile(src, dst); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(dst); string(b) != "program" {
		t.Fatalf("copy: %q", b)
	}
	if err := installFile(dst, dst); err != nil { // že teče od tam: brez kopije
		t.Fatal(err)
	}
	if err := installData([]byte("x"), dst, strings.Repeat("0", 64), 1); err == nil {
		t.Fatal("bad sum accepted")
	}

	p, add := pathWith(`C:\Windows;C:\Tools\`, `C:\Users\u\AppData\Local\Programs\Mini Gateway`)
	if !add || p != `C:\Windows;C:\Tools\;C:\Users\u\AppData\Local\Programs\Mini Gateway` {
		t.Fatalf("append: %q", p)
	}
	if again, add := pathWith(p, `c:\users\u\appdata\local\programs\mini gateway\`); add || again != p { // idempotentno
		t.Fatalf("second append: %q %v", again, add)
	}
	if empty, _ := pathWith("", `C:\MG`); empty != `C:\MG` {
		t.Fatalf("empty PATH: %q", empty)
	}
	if out := pathWithout(p, `C:\Users\u\AppData\Local\Programs\Mini Gateway`); out != `C:\Windows;C:\Tools\` {
		t.Fatalf("remove: %q", out)
	}
}

func TestServeInstallFiles(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	cfg := filepath.Join(home, "my cfg", "serve.json")
	saveServeConfig(cfg, serveConfig{Token: "tok"})
	exe, _ := os.Executable()
	var calls, started []string
	reg := map[string]string{}
	envPath := `C:\Windows`
	o1, o2, o3, o4, o5, o6, o7, o8, o9, o10 := runCmd, runValueSet, runValueGet, runValueDel, userHomeFn, userProgramsDir, trayClose, startDetached, envPathGet, envPathSet
	t.Cleanup(func() {
		runCmd, runValueSet, runValueGet, runValueDel, userHomeFn, userProgramsDir, trayClose, startDetached, envPathGet, envPathSet = o1, o2, o3, o4, o5, o6, o7, o8, o9, o10
	})
	noLinger := false // loginctl in sudo -n zavrneta linger
	runCmd = func(name string, args ...string) error {
		calls = append(calls, name+" "+strings.Join(args, " "))
		if noLinger && slices.Contains(args, "enable-linger") {
			return errors.New("denied")
		}
		return nil
	}
	ol := lingerDir
	lingerDir = filepath.Join(home, "linger")
	t.Cleanup(func() { lingerDir = ol })
	t.Setenv("USER", "ana")
	runValueSet = func(n, v string) error { reg[n] = v; return nil }
	runValueGet = func(n string) (string, error) { return reg[n], nil }
	runValueDel = func(n string) { delete(reg, n) }
	userHomeFn = func() string { return home }
	userProgramsDir = func() string { return filepath.Join(home, "Programs") }
	closed := 0
	trayClose = func() error { closed++; return nil }
	startDetached = func(e string, a ...string) error { started = append(started, e+" "+strings.Join(a, " ")); return nil }
	envPathGet = func() (string, error) { return envPath, nil }
	envPathSet = func(v string) error { envPath = v; return nil }
	tray := fakeRelease(t, "", "") // pladnja ni ob exe: prenos iz izdaje

	bin := filepath.Join(home, ".local", "bin", "minigw")
	// F4: na macOS je router aplikacija.
	if _, err := serviceCommand("install", "darwin", cfg); err == nil || !strings.Contains(err.Error(), "install the Mini Gateway app") || serviceInstalled("darwin") {
		t.Fatalf("darwin install: %v", err)
	}
	for _, goos := range []string{"linux", "windows"} {
		calls = nil
		msg, err := serviceCommand("install", goos, cfg)
		if err != nil {
			t.Fatalf("%s install: %v", goos, err)
		}
		if !serviceInstalled(goos) {
			t.Fatalf("%s: not installed", goos)
		}
		switch goos {
		case "linux":
			data, _ := os.ReadFile(servicePath(goos))
			if !strings.Contains(string(data), `ExecStart="`+bin+`" serve --config "`+cfg+`"`) ||
				strings.Join(calls, ";") != "systemctl --user daemon-reload;systemctl --user enable minigw-serve.service;systemctl --user restart minigw-serve.service;loginctl enable-linger ana" ||
				!strings.Contains(msg, "at boot") {
				t.Fatalf("unit: %s calls %v msg %q", data, calls, msg)
			}
			calls, noLinger = nil, true
			if m, _ := serviceCommand("install", goos, cfg); !strings.HasSuffix(m, "run: sudo loginctl enable-linger ana") || !slices.Contains(calls, "sudo -n loginctl enable-linger ana") || autostart(goos) != "sign-in" {
				t.Fatalf("no linger: %q %v", m, calls)
			}
			os.MkdirAll(lingerDir, 0o755)
			os.WriteFile(filepath.Join(lingerDir, "ana"), nil, 0o644)
			if calls = nil; autostart(goos) != "boot" {
				t.Fatal("autostart with linger")
			}
			if m, _ := serviceCommand("install", goos, cfg); slices.Contains(calls, "loginctl enable-linger ana") || !strings.Contains(m, "at boot") {
				t.Fatalf("linger already on: %q %v", m, calls)
			}
			if a, _, _ := fileSHA256(bin); a == "" {
				t.Fatal("minigw not copied to ~/.local/bin")
			}
		case "windows":
			dir := filepath.Join(home, "Programs", "Mini Gateway")
			want := `"` + filepath.Join(dir, "minigw-tray.exe") + `" tray --config "` + cfg + `"`
			gotTray, _ := os.ReadFile(filepath.Join(dir, "minigw-tray.exe"))
			a, _, _ := fileSHA256(exe)
			b, _, _ := fileSHA256(filepath.Join(dir, "minigw.exe"))
			if reg[runValueName] != want || string(gotTray) != string(tray) || a != b || closed != 1 ||
				!strings.HasSuffix(envPath, ";"+dir) || len(started) != 1 || !strings.Contains(msg, "notification area") {
				t.Fatalf("windows: run %q path %q started %v closed %d msg %q", reg[runValueName], envPath, started, closed, msg)
			}
			if _, err := serviceCommand("install", goos, cfg); err != nil || strings.Count(envPath, dir) != 1 { // posodobitev: PATH enkrat
				t.Fatalf("reinstall: %v %q", err, envPath)
			}
			before := envPath // L1: PATH, ki ga ni mogoče prebrati, ostane, kot je
			envPathGet = func() (string, error) { return "", errors.New("registry busy") }
			if _, err := serviceCommand("install", goos, cfg); err == nil || envPath != before {
				t.Fatalf("PATH read error: %v %q", err, envPath)
			}
			envPathGet = func() (string, error) { return envPath, nil }
		}
		msg, err = serviceCommand("uninstall", goos, cfg)
		if err != nil || serviceInstalled(goos) {
			t.Fatalf("%s uninstall: %v", goos, err)
		}
		if goos == "windows" {
			dir := filepath.Join(home, "Programs", "Mini Gateway")
			if envPath != `C:\Windows` || !strings.Contains(msg, filepath.Dir(cfg)) {
				t.Fatalf("uninstall: path %q msg %q", envPath, msg)
			}
			if _, err := os.Stat(filepath.Join(dir, "minigw.exe")); err == nil {
				t.Fatal("program files kept")
			}
		}
	}
	if q := systemdQuote(`C:\a "b" 100%`); q != `"C:\\a \"b\" 100%%"` {
		t.Fatalf("systemd quote: %s", q)
	}
}

// Vrstica 13 revizije: namestitev z MSI posodobi novi MSI (msiexec), odstrani se v Aplikacijah; datotek ne prepiše.
func TestServeInstallWithMSI(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "serve.json")
	saveServeConfig(cfg, serveConfig{Token: "tok"})
	var calls, got []string
	o1, o2, o3, o4 := msiDir, fetchRelease, runCmd, installedVersion
	t.Cleanup(func() { msiDir, fetchRelease, runCmd, installedVersion = o1, o2, o3, o4 })
	installedVersion = func(string) string { return "0.0.1" }
	msiDir = func() string { return `C:\Users\ana\AppData\Local\Programs\Mini Gateway\` }
	fetchRelease = func(asset, dst string) error {
		got = append(got, asset)
		return os.WriteFile(dst, []byte("msi"), 0o600)
	}
	runCmd = func(name string, args ...string) error {
		calls = append(calls, name+" "+args[0]+" "+strings.Join(args[2:], " "))
		if b, _ := os.ReadFile(args[1]); string(b) != "msi" {
			t.Errorf("msiexec got %s", args[1])
		}
		return nil
	}
	msg, err := installWindows(os.Args[0], cfg)
	if err != nil || !slices.Equal(got, []string{"minigw-windows-" + runtime.GOARCH + ".msi"}) || !slices.Equal(calls, []string{"msiexec /i /passive /norestart"}) ||
		!strings.Contains(msg, "updated with the MSI") {
		t.Fatalf("install: %v %q %v %v", err, msg, got, calls)
	}
	// Vrstica 9: ista različica → nič; novejša nameščena → ne nazaj (brez prenosa in msiexec).
	calls, got = nil, nil
	for v, want := range map[string]string{version: "already up to date", "99.0.0": "is newer than this minigw"} {
		installedVersion = func(string) string { return v }
		if msg, err := installWindows(os.Args[0], cfg); err != nil || !strings.Contains(msg, want) || len(calls) != 0 || len(got) != 0 {
			t.Fatalf("%s: %v %q %v %v", v, err, msg, calls, got)
		}
	}
	if !versionLess("0.5.9", "0.5.10") || versionLess("0.5.10", "0.5.9") || versionLess("1.0", "1.0") {
		t.Fatal("versionLess")
	}
	installedVersion = func(string) string { return "" }
	fetchRelease = func(string, string) error { return errors.New("not signed") }
	if _, err := installWindows(os.Args[0], cfg); err == nil || !strings.Contains(err.Error(), "installed with the MSI") {
		t.Fatalf("download error: %v", err)
	}
	if _, err := uninstallWindows(cfg); err == nil || !strings.Contains(err.Error(), "Settings → Apps") {
		t.Fatalf("uninstall: %v", err)
	}
}

// Meni pladnja: le vrednost Run (celotna namestitev bi zaprla pladenj, ki jo je sprožil).
func TestTrayStartupOnlyRunValue(t *testing.T) {
	reg := map[string]string{}
	o1, o2, o3 := runValueSet, runValueDel, trayClose
	t.Cleanup(func() { runValueSet, runValueDel, trayClose = o1, o2, o3 })
	runValueSet = func(n, v string) error { reg[n] = v; return nil }
	runValueDel = func(n string) { delete(reg, n) }
	trayClose = func() error { t.Fatal("the tray menu must not close the tray"); return nil }
	cfg := filepath.Join(t.TempDir(), "serve.json")
	if err := trayStartup(true, cfg); err != nil || !strings.Contains(reg[runValueName], cfg) {
		t.Fatalf("on: %v %q", err, reg)
	}
	if trayStartup(false, cfg); reg[runValueName] != "" {
		t.Fatal("off")
	}
}

// #8: brez podpisa ali s tujim podpisom se pladenj ne namesti.
func TestReleaseDownloadNeedsSignature(t *testing.T) {
	fakeRelease(t, "", "")
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	releaseKeys = []string{sshPublicLine(other.Public().(ed25519.PublicKey))} // drug ključ kot ta, ki je podpisal
	dst := filepath.Join(t.TempDir(), "tray.exe")
	if err := downloadVerified("minigw-tray-windows-"+runtime.GOARCH+".exe", dst); err == nil || !strings.Contains(err.Error(), "not signed by Mini Gateway") {
		t.Fatalf("foreign signature: %v", err)
	}
	if _, err := os.Stat(dst); err == nil {
		t.Fatal("installed without a good signature")
	}
}

// #25: brez vračanja na starejšo izdajo (serve install) in podpisana VERSION v SHA256SUMS mora biti ta.
func TestServeInstallRefusesDowngrade(t *testing.T) {
	old, oldAllow := installedVersion, allowDowngrade
	t.Cleanup(func() { installedVersion, allowDowngrade = old, oldAllow })
	target := filepath.Join(t.TempDir(), "minigw")
	os.WriteFile(target, []byte("x"), 0o755)
	installedVersion = func(string) string { return "99.0.0" }
	if err := refuseDowngrade(target); err == nil || !strings.Contains(err.Error(), "--allow-downgrade") {
		t.Fatalf("newer installed: %v", err)
	}
	allowDowngrade = true
	if err := refuseDowngrade(target); err != nil {
		t.Fatalf("--allow-downgrade: %v", err)
	}
	allowDowngrade = false
	installedVersion = func(string) string { return "0.0.1" }
	if err := refuseDowngrade(target); err != nil {
		t.Fatalf("older installed: %v", err)
	}
	sum := strings.Repeat("a", 64)
	if w, err := sumsWant([]byte("VERSION "+version+"\n"+sum+"  minigw-linux-arm64\n"), "minigw-linux-arm64"); err != nil || w != sum {
		t.Fatalf("sums: %q %v", w, err)
	}
	if _, err := sumsWant([]byte("VERSION 0.0.1\n"+sum+"  minigw-linux-arm64\n"), "minigw-linux-arm64"); err == nil {
		t.Fatal("VERSION of another release accepted")
	}
	if w, err := sumsWant([]byte(sum+"  minigw-linux-arm64\n"), "minigw-linux-arm64"); err != nil || w != sum { // starejša izdaja brez VERSION
		t.Fatalf("old sums: %q %v", w, err)
	}
}
