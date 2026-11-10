package main

// Peskovnik agentov na Windows (#68, docs/plans/design-0.5.23-windows-sandbox.md): minigw serve zažene ves CLI agenta
// (Claude Code, Copilot, Grok; Codex ne, ta ima svoj »elevated« peskovnik) prek wxc-exec.exe v MXC processcontainer
// (uporabnik z nizko integriteto + AppContainer). Ena zahteva JSON na zagon (pogodba 1.0.0, shema
// schemas/stable/mxc-config.schema.1.0.0.json v microsoft/mxc): pisanje le v klon, $MINIGW_DEPS, predpomnilnike, TEMP
// zagona in dom agenta pod Scratch; branje le dovoljenih map (programi iz PATH, namestitev minigw); mapa nastavitev
// prepovedana. Gradnik zahteve in preizkus sta brez oznake gradnje (testi tečejo tudi na Macu in Linuxu z lažnim
// wxc-exec); izbira, ali se uporabi, je v peer_windows.go.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	mxcRelease  = "1.0.1" // izdaja microsoft/mxc, iz katere je wxc-exec.exe v MSI
	mxcContract = "1.0.0" // različica pogodbe zahteve
	mxcTimeout  = 24 * time.Hour
)

// wxcHashes: SHA-256 wxc-exec.exe iz mxc-release-binaries.zip v1.0.1 (x64, arm64), enako kot scripts/mxc.sha256.
// minigw ga preveri pred vsako rabo: mapa namestitve je uporabnikova in jo je lahko zamenjal prejšnji tek brez peskovnika.
var wxcHashes = map[string]string{
	"amd64": "090f5d1f7bb93104ab08dcd4e8034ef954aeefcdaf6658dde5f7dff4cfc12f3a",
	"arm64": "91aa4171e8809f33f19622c381116188eab7d8c8cffad231230a31b58146ee77",
}

// wxcExe: wxc-exec.exe ob minigw.exe (mxc\wxc-exec.exe, komponenta MSI Sandbox), nikoli iz PATH; testi: lažni.
var wxcExe = func() string { return filepath.Join(filepath.Dir(minigwExe()), "mxc", "wxc-exec.exe") }

// wxcVerify: datoteka ima pripeto zgoščeno vrednost za to arhitekturo.
func wxcVerify(p string) error {
	want := wxcHashes[runtime.GOARCH]
	if want == "" {
		return errors.New("no wxc-exec.exe for " + runtime.GOARCH)
	}
	f, err := os.Open(p)
	if err != nil {
		return errors.New("wxc-exec.exe is missing (" + p + ")")
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if hex.EncodeToString(h.Sum(nil)) != want {
		return errors.New("wxc-exec.exe changed (its SHA-256 is not the one this minigw ships)")
	}
	return nil
}

// mxcRequest: zahteva wxc-exec (OneShotRequest pogodbe 1.0.0).
type mxcRequest struct {
	Version          string      `json:"version"`
	Containment      string      `json:"containment"`
	Process          mxcProcess  `json:"process"`
	Filesystem       mxcFS       `json:"filesystem"`
	ProcessContainer mxcPC       `json:"processContainer"`
	Network          mxcNet      `json:"network"`
	RuntimeConfig    *mxcRuntime `json:"runtimeConfig,omitempty"`
	UI               mxcUI       `json:"ui"`
}

type mxcProcess struct {
	CommandLine       string   `json:"commandLine"`
	Cwd               string   `json:"cwd,omitempty"`
	Env               []string `json:"env"`
	InheritDefaultEnv bool     `json:"inheritDefaultEnv"`
	Timeout           int64    `json:"timeout"` // ms; nikoli privzetih 90 s (W8)
}

type mxcFS struct {
	ReadWrite []string `json:"readwritePaths"`
	ReadOnly  []string `json:"readonlyPaths"`
	Denied    []string `json:"deniedPaths"`
}

type mxcPC struct {
	Filesystem struct {
		Enumerate []string `json:"enumeratePaths"`
	} `json:"filesystem"`
}

type mxcNet struct {
	Egress  mxcDefault  `json:"egress"`
	Ingress *mxcDefault `json:"ingress,omitempty"`
}

type mxcDefault struct {
	Default string `json:"default"`
}

type mxcRuntime struct {
	NetworkProxy string `json:"networkProxy"`
}

type mxcUI struct {
	Disable bool `json:"disable"` // false: sicer vsak konzolni program 0xC0000142 (W2)
}

// mxcSpec: vse, kar določi zahtevo (čista funkcija mxcRequestFor).
type mxcSpec struct {
	argv         []string // program in argumenti
	cwd          string
	env          []string
	rw, ro, deny []string
	egressDeny   bool
	proxy        string // vrata izvajanja (127.0.0.1:port): posrednik CONNECT in vprašanja drugim modelom; "" = brez
}

// mxcRequestFor: zahteva iz mxcSpec. Branje je seznam dovoljenih: kar ni v rw/ro, ni berljivo; enumerate = vsi predniki
// dovoljenih poti (W3: git sicer ne prebere trenutne mape), brez njih samih. Mapa nastavitev le v deny.
func mxcRequestFor(sp mxcSpec) mxcRequest {
	clean := func(in []string) []string {
		var out []string
		for _, p := range in {
			if p = filepath.Clean(p); p != "." && !slices.ContainsFunc(sp.deny, func(d string) bool { return under(p, d) }) && !slices.Contains(out, p) {
				out = append(out, p)
			}
		}
		return out
	}
	rw := clean(sp.rw)
	ro := slices.DeleteFunc(clean(sp.ro), func(p string) bool { return slices.Contains(rw, p) })
	r := mxcRequest{Version: mxcContract, Containment: "processcontainer",
		Process:    mxcProcess{CommandLine: winCmdLine(sp.argv), Cwd: sp.cwd, Env: sp.env, Timeout: mxcTimeout.Milliseconds()},
		Filesystem: mxcFS{ReadWrite: nonNil(rw), ReadOnly: nonNil(ro), Denied: nonNil(slices.Clone(sp.deny))},
		Network:    mxcNet{Egress: mxcDefault{"allow"}}}
	r.ProcessContainer.Filesystem.Enumerate = nonNil(mxcAncestors(append(slices.Clone(rw), ro...), sp.deny))
	if sp.egressDeny {
		r.Network.Egress.Default = "deny"
	}
	if sp.proxy != "" { // W9: posrednik klicatelja; loopback do teh vrat (tudi vprašanja drugim modelom)
		r.RuntimeConfig = &mxcRuntime{NetworkProxy: "http://" + sp.proxy}
		r.Network.Ingress = &mxcDefault{"allow"}
	}
	return r
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// mxcAncestors: vsi predniki poti do korena diska, brez poti samih in brez mape v deny ali pod njo; urejeno.
func mxcAncestors(paths, deny []string) []string {
	var out []string
	for _, p := range paths {
		for d := filepath.Dir(p); ; d = filepath.Dir(d) {
			if !slices.Contains(paths, d) && !slices.Contains(out, d) && !slices.ContainsFunc(deny, func(x string) bool { return under(d, x) }) {
				out = append(out, d)
			}
			if filepath.Dir(d) == d {
				break
			}
		}
	}
	slices.Sort(out)
	return out
}

// winCmdLine: ukazna vrstica Windows (pravila CommandLineToArgvW, kot syscall.EscapeArg, ki je le na Windows).
func winCmdLine(argv []string) string {
	var b strings.Builder
	for i, a := range argv {
		if i > 0 {
			b.WriteByte(' ')
		}
		if a != "" && !strings.ContainsAny(a, " \t\n\v\"") {
			b.WriteString(a)
			continue
		}
		b.WriteByte('"')
		slashes := 0
		for _, c := range []byte(a) {
			switch c {
			case '\\':
				slashes++
			case '"':
				b.WriteString(strings.Repeat(`\`, slashes+1))
				slashes = 0
			default:
				slashes = 0
			}
			b.WriteByte(c)
		}
		b.WriteString(strings.Repeat(`\`, slashes))
		b.WriteByte('"')
	}
	return b.String()
}

// envSet: KEY=v namesto vseh vnosov KEY (Windows: brez razlike v velikih črkah).
func envSet(env []string, key, v string) []string {
	env = slices.DeleteFunc(slices.Clone(env), func(kv string) bool {
		k, _, _ := strings.Cut(kv, "=")
		return strings.EqualFold(k, key)
	})
	return append(env, key+"="+v)
}

// envGet: zadnja vrednost KEY v env.
func envGet(env []string, key string) string {
	v := ""
	for _, kv := range env {
		if k, val, ok := strings.Cut(kv, "="); ok && strings.EqualFold(k, key) {
			v = val
		}
	}
	return v
}

// mxcPathDirs: mape PATH, ki jih škatla bere: obstoječe absolutne, brez MSYS2 (Git\usr\bin: tam programi umrejo, W4),
// ne pod Scratch ali v mapi nastavitev in ne nad domačo mapo, Scratch ali mapo nastavitev. keep: dovoljeno vseeno
// (ovoj git v Scratch\gitexec\bin).
func mxcPathDirs(path []string, keep []string, home, scratch, cfgDir string) []string {
	var out []string
	for _, d := range path {
		if d == "" || !filepath.IsAbs(d) || slices.Contains(out, d) {
			continue
		}
		if slices.Contains(keep, d) {
			out = append(out, d)
			continue
		}
		st, err := os.Stat(d)
		if err != nil || !st.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(d, "msys-2.0.dll")); err == nil {
			continue
		}
		bad := false
		for _, x := range []string{scratch, cfgDir} {
			if x != "" && (under(d, x) || under(x, d)) {
				bad = true
			}
		}
		if home != "" && under(home, d) {
			bad = true
		}
		if !bad {
			out = append(out, d)
		}
	}
	return out
}

// mxcWrap: zahteva za exe z args v škatli (agentMounts) v datoteko <Scratch>\tmp\mxc-*.json (zunaj vseh dovoljenj) in
// ukaz wxc-exec zanjo. cleanup po cmd.Wait pobriše zahtevo in začasno mapo, ki jo je naredil (m.temp prazen).
func mxcWrap(wxc, exe string, args []string, m agentMounts) (string, []string, func(), error) {
	scratch := m.hide
	if scratch == "" {
		return "", nil, nil, errors.New("no scratch folder for the sandbox request")
	}
	var dones []func()
	done := func() {
		for _, d := range slices.Backward(dones) {
			d()
		}
	}
	exists := func(p string) bool { _, err := os.Stat(p); return err == nil }
	var rw, ro []string
	if m.state != "" {
		os.MkdirAll(m.state, 0o700)
		rw = append(rw, m.state)
	}
	for _, p := range m.rw {
		if exists(p) {
			rw = append(rw, p)
		}
	}
	tmpRoot := filepath.Join(scratch, "tmp")
	os.MkdirAll(tmpRoot, 0o700)
	temp := m.temp
	if temp == "" {
		d, err := os.MkdirTemp(tmpRoot, "run-*")
		if err != nil {
			return "", nil, nil, err
		}
		temp = d
		dones = append(dones, func() { os.RemoveAll(d) })
	}
	os.MkdirAll(temp, 0o700)
	home := m.home
	if home == "" {
		home = filepath.Join(temp, "home")
	}
	roaming, local := filepath.Join(home, "AppData", "Roaming"), filepath.Join(home, "AppData", "Local")
	for _, d := range []string{roaming, local} {
		os.MkdirAll(d, 0o700)
	}
	rw = append(rw, temp, home)
	userHome, _ := os.UserHomeDir()
	for _, p := range m.ro {
		if exists(p) {
			ro = append(ro, p)
		}
	}
	ro = append(ro, filepath.Dir(minigwExe()), filepath.Dir(exe))
	if userHome != "" && under(exe, userHome) {
		ro = append(ro, packageRoot(userHome, exe))
	}
	env := slices.Clone(m.cmdEnv)
	for _, k := range []string{"JAVA_HOME", "MAVEN_HOME", "M2_HOME", "DOTNET_ROOT", "GOROOT"} { // orodja zunaj PATH
		if v := envGet(env, k); v != "" && filepath.IsAbs(v) && exists(v) {
			ro = append(ro, v)
		}
	}
	keep := slices.Clone(ro)
	path := mxcPathDirs(filepath.SplitList(envGet(env, "PATH")), keep, userHome, scratch, m.cfgDir)
	for _, d := range path {
		ro = append(ro, d)
		if strings.EqualFold(filepath.Base(d), "cmd") && exists(filepath.Join(d, "git.exe")) { // Git for Windows: mingw64\libexec\git-core
			ro = append(ro, filepath.Dir(d))
		}
	}
	for k, v := range map[string]string{"USERPROFILE": home, "HOME": home, "APPDATA": roaming, "LOCALAPPDATA": local,
		"TEMP": temp, "TMP": temp, "TMPDIR": temp, "PATH": strings.Join(path, string(os.PathListSeparator)), "MINIGW_SANDBOXED": "1"} {
		env = envSet(env, k, v)
	}
	if m.noNet && m.proxy != "" { // omrežje izklopljeno: le gostitelji ponudnika prek posrednika (W8: brez njega Claude visi)
		env = envSet(env, "HTTPS_PROXY", "http://"+m.proxy) // Windows: imena brez razlike v velikih črkah
		env = envSet(env, "NO_PROXY", "127.0.0.1,localhost")
	}
	slices.Sort(env)
	cwd := m.cwd
	var deny []string
	if m.cfgDir != "" {
		deny = []string{m.cfgDir}
	}
	req := mxcRequestFor(mxcSpec{argv: append([]string{exe}, args...), cwd: cwd, env: env, rw: rw, ro: ro,
		deny: deny, egressDeny: m.noNet, proxy: m.proxy})
	b, err := json.MarshalIndent(req, "", " ")
	if err != nil {
		done()
		return "", nil, nil, err
	}
	f, err := os.CreateTemp(tmpRoot, "mxc-*.json")
	if err == nil {
		_, err = f.Write(b)
		err = errors.Join(err, f.Close())
		name := f.Name()
		dones = append(dones, func() { os.Remove(name) })
	}
	if err != nil {
		done()
		return "", nil, nil, err
	}
	return wxc, []string{f.Name()}, done, nil
}

// boxWrap: agentWrap s škatlo (Windows; drugje le testi z lažnim wxc-exec): wxc-exec.exe se pred vsako rabo preveri.
// Napaka: agent ne teče (nikoli brez škatle, ko je vklopljena).
func boxWrap(exe string, args []string, m agentMounts) (string, []string, func(), error) {
	wxc := wxcExe()
	if err := wxcVerify(wxc); err != nil {
		return "", nil, nil, fmt.Errorf("Windows sandbox: %v", err)
	}
	return mxcWrap(wxc, exe, args, m)
}

// --- preizkus ob zagonu (minigw.exe __mxc-probe v škatli) ---

const mxcProbeCmd = "__mxc-probe"

// mxcProbeMain: v škatli: odmev vrstice s stdin, pisanje v dovoljeno mapo, branje prepovedane datoteke (mora pasti),
// povezava na 127.0.0.1 prehoda; izhod 7. Vsak preizkus v svoji vrstici »ime:ok« ali »ime:fail …«.
// args: mapa za pisanje, prepovedana datoteka, naslov poslušalca.
func mxcProbeMain(args []string) {
	if len(args) != 3 {
		os.Exit(2)
	}
	line, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	fmt.Println("nonce:" + strings.TrimSpace(line))
	p := filepath.Join(args[0], "probe.txt")
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		fmt.Println("write:fail " + err.Error())
	} else if b, err := os.ReadFile(p); err != nil || string(b) != "x" {
		fmt.Println("write:fail read back")
	} else {
		fmt.Println("write:ok")
	}
	if _, err := os.ReadFile(args[1]); err != nil {
		fmt.Println("deny:ok")
	} else {
		fmt.Println("deny:fail")
	}
	if c, err := net.DialTimeout("tcp", args[2], 5*time.Second); err != nil {
		fmt.Println("loopback:fail " + err.Error())
	} else {
		c.Write([]byte("ok\n"))
		c.Close()
		fmt.Println("loopback:ok")
	}
	fmt.Println("final:" + codexFinalPath(args[0])) // le za diagnozo (W7)
	os.Exit(7)
}

// mxcProbeVerdict: "" = škatla deluje, sicer razlog (angleško, za stran z nastavitvami in dnevnik).
func mxcProbeVerdict(out string, code int, nonce string, loopback bool) string {
	got := map[string]string{}
	for _, l := range strings.Split(strings.ReplaceAll(out, "\r", ""), "\n") {
		if k, v, ok := strings.Cut(l, ":"); ok {
			got[k] = v
		}
	}
	switch {
	case got["nonce"] != nonce || code != 7:
		return fmt.Sprintf("wxc-exec did not pass the prompt and the exit code through (exit %d)", code)
	case got["write"] != "ok":
		return "a folder granted for writing could not be written"
	case got["deny"] != "ok":
		return "the gateway's config was readable inside the sandbox"
	case got["loopback"] != "ok" || !loopback:
		return "the sandbox cannot reach the gateway on 127.0.0.1 (needs Windows 11 build 26100.9550 / 26200.9550 or later)"
	}
	return ""
}

// mxcProbe: preizkus škatle z wxc (preverjen) in minigw (programom za __mxc-probe). "" = deluje, sicer razlog.
func mxcProbe(wxc, minigw, scratch, cfgDir string) string {
	if err := wxcVerify(wxc); err != nil {
		return err.Error()
	}
	os.MkdirAll(filepath.Join(scratch, "tmp"), 0o700)
	dir, err := os.MkdirTemp(filepath.Join(scratch, "tmp"), "probe-*")
	if err != nil {
		return err.Error()
	}
	defer os.RemoveAll(dir)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err.Error()
	}
	defer ln.Close()
	reached := make(chan bool, 1)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				reached <- false
				return
			}
			c.SetDeadline(time.Now().Add(5 * time.Second))
			l, _ := bufio.NewReader(c).ReadString('\n')
			c.Close()
			if l == "ok\n" {
				reached <- true
				return
			}
		}
	}()
	m := agentMounts{cfgDir: cfgDir, hide: scratch, rw: []string{dir}, temp: dir, cwd: dir, cmdEnv: agentEnv(os.Environ()),
		noNet: true, proxy: ln.Addr().String()}
	name, args, done, err := mxcWrap(wxc, minigw, []string{mxcProbeCmd, dir, filepath.Join(cfgDir, "serve.json"), ln.Addr().String()}, m)
	if err != nil {
		return err.Error()
	}
	defer done()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	nonce := randomToken()
	cmd := commandContext(ctx, name, args...)
	cmd.Env = m.cmdEnv
	cmd.Stdin = strings.NewReader(nonce + "\n")
	var out, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &stderr
	err = cmd.Run()
	code := 0
	if e, ok := err.(*exec.ExitError); ok {
		code = e.ExitCode()
	} else if err != nil {
		return "cannot run wxc-exec.exe: " + err.Error()
	}
	ok := false
	select {
	case ok = <-reached:
	case <-time.After(2 * time.Second):
	}
	why := mxcProbeVerdict(out.String(), code, nonce, ok)
	if why != "" && strings.TrimSpace(stderr.String()) != "" {
		first, _, _ := strings.Cut(strings.TrimSpace(stderr.String()), "\n")
		why += ": " + strings.TrimSpace(first)
	}
	return why
}

// --- prijave agentov: kopija noter, nazaj le ob nespremenjeni pravi (Windows: povezave zahtevajo pravice, trde ne
// prečkajo nosilcev C: → D:) ---

var (
	authMu     sync.Mutex
	authCopied = map[string]string{} // kopija v stanju naprave → SHA-256 prave datoteke ob zadnji kopiji noter
)

func fileSum(p string) (string, []byte) {
	b, err := os.ReadFile(p)
	if err != nil {
		return "", nil
	}
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:]), b
}

// authCopyIn: prava datoteka real → dst. Prijava (config=false) se ne prepiše, če se prava od zadnje kopije ni
// spremenila (dst je lahko novejša, ker jo je CLI osvežil; authCopyBack jo prenese). Nastavitve (config) vedno znova:
// agent jih ne more podtakniti naslednjemu teku.
func authCopyIn(real, dst string, config bool) error {
	authMu.Lock()
	defer authMu.Unlock()
	sum, b := fileSum(real)
	if sum == "" {
		return nil
	}
	if cur, _ := fileSum(dst); !config && cur != "" && authCopied[dst] == sum {
		return nil
	}
	if err := writeFileAtomic(dst, b); err != nil {
		return err
	}
	authCopied[dst] = sum
	return nil
}

// authCopyBack: spremenjena kopija dst gre v real atomarno, le če ima real še vrednost ob kopiji noter; sicer zmaga
// novejša prava (drug tek ali lastnikov CLI), naslednji tek jo kopira noter.
func authCopyBack(real, dst string) bool {
	authMu.Lock()
	defer authMu.Unlock()
	was, ok := authCopied[dst]
	sum, b := fileSum(dst)
	if !ok || sum == "" || sum == was {
		return false
	}
	if cur, _ := fileSum(real); cur != was {
		return false
	}
	if writeFileAtomic(real, b) != nil {
		return false
	}
	authCopied[dst] = sum
	return true
}
