package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// fakeWxcMain: lažni wxc-exec (testni program z MINIGW_TEST_FAKE_WXC=1): zahtevo shrani (MINIGW_TEST_WXC_LOG), nato
// commandLine požene brez peskovnika z dano mapo in okoljem; stdin, stdout in izhod gredo skozi.
func fakeWxcMain() {
	b, err := os.ReadFile(os.Args[1])
	if err != nil {
		os.Exit(90)
	}
	if p := os.Getenv("MINIGW_TEST_WXC_LOG"); p != "" {
		os.WriteFile(p, b, 0o600)
	}
	var req mxcRequest
	if json.Unmarshal(b, &req) != nil {
		os.Exit(91)
	}
	argv := winSplit(req.Process.CommandLine)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = req.Process.Cwd
	cmd.Env = slices.DeleteFunc(slices.Clone(req.Process.Env), func(kv string) bool { return strings.HasPrefix(kv, "MINIGW_TEST_FAKE_WXC=") })
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		if e, ok := err.(*exec.ExitError); ok {
			os.Exit(e.ExitCode())
		}
		os.Exit(92)
	}
	os.Exit(0)
}

// winSplit: obratno od winCmdLine (CommandLineToArgvW za argumente, ki jih ta naredi).
func winSplit(s string) []string {
	var out []string
	var cur strings.Builder
	in, any, slashes := false, false, 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\':
			slashes++
			continue
		case c == '"':
			cur.WriteString(strings.Repeat(`\`, slashes/2))
			if slashes%2 == 1 {
				cur.WriteByte('"')
			} else {
				in, any = !in, true
			}
		case c == ' ' && !in:
			cur.WriteString(strings.Repeat(`\`, slashes))
			if cur.Len() > 0 || any {
				out = append(out, cur.String())
			}
			cur.Reset()
			any = false
		default:
			cur.WriteString(strings.Repeat(`\`, slashes))
			cur.WriteByte(c)
		}
		slashes = 0
	}
	cur.WriteString(strings.Repeat(`\`, slashes))
	if cur.Len() > 0 || any {
		out = append(out, cur.String())
	}
	return out
}

func TestWinCmdLine(t *testing.T) {
	for _, argv := range [][]string{{"a", "b c", ""}, {`C:\Program Files\x.exe`, `a\"b`, `tail\`, `x\\`, `"q"`}, {"-c", `sandbox_mode="read-only"`}} {
		line := winCmdLine(argv)
		if got := winSplit(line); !slices.Equal(got, argv) {
			t.Errorf("%q → %s → %q", argv, line, got)
		}
	}
	if got := winCmdLine([]string{`C:\a b\c.exe`, "-p"}); got != `"C:\a b\c.exe" -p` {
		t.Fatal(got)
	}
}

// Gradnik zahteve: dovoljenja, predniki, mapa nastavitev le v deny, omrežje in vrata izvajanja.
func TestMXCRequestFor(t *testing.T) {
	cfg := filepath.FromSlash("/u/AppData/Roaming/minigw")
	base := mxcSpec{argv: []string{"claude", "-p"}, cwd: "/s/w/c", env: []string{"A=1"}, deny: []string{cfg},
		rw: []string{"/s/w/c", "/s/w/c.deps", "/s/w/c"}, ro: []string{"/p/bin", "/s/w/c", filepath.Join(cfg, "x")}}
	for _, tc := range []struct {
		name               string
		egressDeny         bool
		proxy, wantEgress  string
		wantRuntime, wantI bool
	}{
		{"allow", false, "", "allow", false, false},
		{"deny with proxy", true, "127.0.0.1:5000", "deny", true, true},
		{"allow with ask port", false, "127.0.0.1:5000", "allow", false, true},
		{"deny without port", true, "", "deny", false, false},
	} {
		sp := base
		sp.egressDeny, sp.proxy = tc.egressDeny, tc.proxy
		r := mxcRequestFor(sp)
		fs := r.Filesystem
		if r.Version != "1.0.0" || r.Containment != "processcontainer" || r.UI.Disable || r.Process.Timeout <= 90000 || r.Process.InheritDefaultEnv {
			t.Fatalf("%s: header %+v", tc.name, r)
		}
		if !slices.Equal(fs.ReadWrite, []string{"/s/w/c", "/s/w/c.deps"}) || !slices.Equal(fs.ReadOnly, []string{"/p/bin"}) || !slices.Equal(fs.Denied, []string{cfg}) {
			t.Fatalf("%s: grants %+v", tc.name, fs)
		}
		if want := []string{"/", "/p", "/s", "/s/w"}; !slices.Equal(r.ProcessContainer.Filesystem.Enumerate, want) {
			t.Fatalf("%s: enumerate %v", tc.name, r.ProcessContainer.Filesystem.Enumerate)
		}
		if r.Network.Egress.Default != tc.wantEgress || (r.RuntimeConfig != nil) != tc.wantRuntime || (r.Network.Ingress != nil) != tc.wantI {
			t.Fatalf("%s: network %+v %+v", tc.name, r.Network, r.RuntimeConfig)
		}
		if tc.wantRuntime && r.RuntimeConfig.NetworkProxy != "http://"+tc.proxy {
			t.Fatalf("%s: proxy %v", tc.name, r.RuntimeConfig)
		}
		b, _ := json.Marshal(r)
		for _, k := range []string{`"readwritePaths"`, `"enumeratePaths"`, `"commandLine":"claude -p"`, `"ui":{"disable":false}`} {
			if !strings.Contains(string(b), k) {
				t.Fatalf("%s: %s missing in %s", tc.name, k, b)
			}
		}
		if strings.Count(string(b), "minigw") != 1 { // mapa nastavitev le v deniedPaths
			t.Fatalf("%s: config folder outside deny: %s", tc.name, b)
		}
	}
}

// PATH v škatli: brez MSYS2, brez map pod Scratch, nastavitvami ali nad domačo mapo; keep ostane.
func TestMXCPathDirs(t *testing.T) {
	root := t.TempDir()
	mk := func(p ...string) string {
		d := filepath.Join(append([]string{root}, p...)...)
		os.MkdirAll(d, 0o700)
		return d
	}
	tools, msys, scratch, cfg, home := mk("tools"), mk("Git", "usr", "bin"), mk("scratch"), mk("cfg"), mk("users", "me")
	os.WriteFile(filepath.Join(msys, "msys-2.0.dll"), nil, 0o600)
	wrap, inScratch, users, inHome := mk("scratch", "gitexec", "win-1", "bin"), mk("scratch", "x"), filepath.Join(root, "users"), mk("users", "me", "bin")
	got := mxcPathDirs([]string{wrap, tools, msys, inScratch, cfg, users, inHome, "rel", filepath.Join(root, "missing"), tools}, []string{wrap}, home, scratch, cfg)
	if !slices.Equal(got, []string{wrap, tools, inHome}) {
		t.Fatalf("path: %v", got)
	}
}

// useFakeWxc: lažni wxc-exec (ta testni program) z dovoljeno zgoščeno vrednostjo; vrne datoteko z zadnjo zahtevo.
func useFakeWxc(t *testing.T) (string, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("fake wxc-exec runs /bin/sh")
	}
	exe, _ := os.Executable()
	b, _ := os.ReadFile(exe)
	sum := sha256.Sum256(b)
	old := wxcHashes[runtime.GOARCH]
	wxcHashes[runtime.GOARCH] = hex.EncodeToString(sum[:])
	t.Cleanup(func() { wxcHashes[runtime.GOARCH] = old })
	log := filepath.Join(t.TempDir(), "req.json")
	t.Setenv("MINIGW_TEST_FAKE_WXC", "1")
	t.Setenv("MINIGW_TEST_WXC_LOG", log)
	return exe, log
}

// mxcWrap z lažnim wxc-exec: stdin in izhod skozi, okolje pod Scratch, zahteva in začasna mapa po teku pobrisani,
// HTTPS_PROXY le brez omrežja z vrati izvajanja.
func TestMXCWrapRun(t *testing.T) {
	wxc, log := useFakeWxc(t)
	scratch, cfg, work := t.TempDir(), t.TempDir(), t.TempDir()
	for _, noNet := range []bool{false, true} {
		m := agentMounts{cfgDir: cfg, hide: scratch, rw: []string{work}, cwd: work, noNet: noNet, proxy: "127.0.0.1:4999",
			cmdEnv: append(agentEnv(os.Environ()), "GIT_DIR=/nope", "PATH=/usr/bin:/bin")}
		name, args, done, err := mxcWrap(wxc, "/bin/sh", []string{"-c", `read x; echo "in=$x"; echo "tmp=$TEMP"; echo "home=$USERPROFILE"; echo "proxy=$HTTPS_PROXY"; echo "box=$MINIGW_SANDBOXED"; exit 3`}, m)
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(name, args...)
		cmd.Env = m.cmdEnv
		cmd.Stdin = strings.NewReader("hello\n")
		out, err := cmd.Output()
		if e, ok := err.(*exec.ExitError); !ok || e.ExitCode() != 3 {
			t.Fatalf("exit: %v %s", err, out)
		}
		got := string(out)
		var tmp string
		for _, l := range strings.Split(got, "\n") {
			if v, ok := strings.CutPrefix(l, "tmp="); ok {
				tmp = v
			}
		}
		if !strings.Contains(got, "in=hello\n") || !under(tmp, scratch) || !strings.Contains(got, "home="+filepath.Join(tmp, "home")+"\n") ||
			!strings.Contains(got, "box=1\n") || strings.Contains(got, "proxy=http") != noNet {
			t.Fatalf("output: %s", got)
		}
		var req mxcRequest
		b, _ := os.ReadFile(log)
		json.Unmarshal(b, &req)
		if !slices.Contains(req.Filesystem.ReadWrite, work) || !slices.Equal(req.Filesystem.Denied, []string{cfg}) || (req.Network.Egress.Default == "deny") != noNet {
			t.Fatalf("request: %s", b)
		}
		if _, err := os.Stat(args[0]); err != nil || !under(args[0], filepath.Join(scratch, "tmp")) {
			t.Fatalf("request file: %v", err)
		}
		done()
		if _, err := os.Stat(args[0]); err == nil {
			t.Fatal("request file kept")
		}
		if _, err := os.Stat(tmp); err == nil {
			t.Fatal("run temp kept")
		}
	}
}

// Preizkus ob zagonu z lažnim wxc-exec: minigw (ta program) teče kot __mxc-probe; spremenjen wxc-exec ni v redu.
func TestMXCProbe(t *testing.T) {
	wxc, _ := useFakeWxc(t)
	t.Setenv("MINIGW_TEST_MAIN", "1")
	scratch, cfg := t.TempDir(), t.TempDir()
	if why := mxcProbe(wxc, wxc, scratch, cfg); why != "" {
		t.Fatalf("probe: %s", why)
	}
	os.WriteFile(filepath.Join(cfg, "admin.secret"), []byte("x"), 0o600) // brez peskovnika berljiva: preizkus to vidi
	if why := mxcProbe(wxc, wxc, scratch, cfg); why != "the gateway's config was readable inside the sandbox" {
		t.Fatalf("probe deny: %q", why)
	}
	wxcHashes[runtime.GOARCH] = strings.Repeat("0", 64)
	if why := mxcProbe(wxc, wxc, scratch, cfg); !strings.Contains(why, "wxc-exec.exe changed") {
		t.Fatalf("probe hash: %q", why)
	}
}

func TestMXCProbeVerdict(t *testing.T) {
	ok := "nonce:n\nwrite:ok\ndeny:ok\nloopback:ok\nfinal:\n"
	for _, tc := range []struct {
		out      string
		code     int
		loopback bool
		want     string
	}{
		{ok, 7, true, ""},
		{strings.ReplaceAll(ok, "\n", "\r\n"), 7, true, ""},
		{ok, 0, true, "wxc-exec did not pass the prompt and the exit code through (exit 0)"},
		{"", 7, true, "wxc-exec did not pass the prompt and the exit code through (exit 7)"},
		{strings.Replace(ok, "write:ok", "write:fail x", 1), 7, true, "a folder granted for writing could not be written"},
		{strings.Replace(ok, "deny:ok", "deny:fail", 1), 7, true, "the gateway's config was readable inside the sandbox"},
		{strings.Replace(ok, "loopback:ok", "loopback:fail refused", 1), 7, true, "the sandbox cannot reach the gateway on 127.0.0.1 (needs Windows 11 build 26100.9550 / 26200.9550 or later)"},
		{ok, 7, false, "the sandbox cannot reach the gateway on 127.0.0.1 (needs Windows 11 build 26100.9550 / 26200.9550 or later)"},
	} {
		if got := mxcProbeVerdict(tc.out, tc.code, "n", tc.loopback); got != tc.want {
			t.Errorf("%q %d: %q", tc.out, tc.code, got)
		}
	}
}

// Prijave v škatli: le kopija noter (vsakič znova), nikoli nazaj; le navadne datoteke do meje; manjkajoča prava
// odstrani kopijo.
func TestAuthCopyIn(t *testing.T) {
	d := t.TempDir()
	real, dst := filepath.Join(d, "real", "auth.json"), filepath.Join(d, "state", "auth.json")
	os.MkdirAll(filepath.Dir(real), 0o700)
	read := func(p string) string { b, _ := os.ReadFile(p); return string(b) }
	os.WriteFile(real, []byte("A"), 0o600)
	if authCopyIn(real, dst) != nil || read(dst) != "A" {
		t.Fatal("copy in")
	}
	os.WriteFile(dst, []byte("B"), 0o600) // agent je kopijo spremenil: nazaj nikoli, naslednji tek dobi pravo
	if authCopyIn(real, dst) != nil || read(dst) != "A" || read(real) != "A" {
		t.Fatal("copy changed")
	}
	os.WriteFile(filepath.Join(d, "big"), make([]byte, 2<<20), 0o600)
	if authCopyIn(filepath.Join(d, "big"), dst) == nil {
		t.Fatal("oversized file copied")
	}
	if runtime.GOOS != "windows" {
		os.Symlink(real, filepath.Join(d, "link"))
		if authCopyIn(filepath.Join(d, "link"), dst) == nil {
			t.Fatal("link followed")
		}
	}
	os.Remove(real)
	if authCopyIn(real, dst) != nil {
		t.Fatal("missing real")
	}
	if _, err := os.Stat(dst); err == nil {
		t.Fatal("stale copy kept")
	}
	src, out := filepath.Join(d, "tree"), filepath.Join(d, "out")
	os.MkdirAll(filepath.Join(src, "p"), 0o700)
	os.WriteFile(filepath.Join(src, "p", "s.jsonl"), []byte("x"), 0o600)
	copyRegularTree(src, out)
	if read(filepath.Join(out, "p", "s.jsonl")) != "x" {
		t.Fatal("tree")
	}
}

func TestTomlElevated(t *testing.T) {
	for in, want := range map[string]bool{
		"[windows]\nsandbox = \"elevated\"\n":                      true,
		"model = \"x\"\n[windows]\n  sandbox='elevated' # setup\n": true,
		"windows.sandbox = \"elevated\"":                           true,
		"[windows]\nsandbox = \"unelevated\"":                      false,
		"[features]\nsandbox = \"elevated\"":                       false,
		"# [windows]\n# sandbox = \"elevated\"":                    false,
		"":                                                         false,
	} {
		if got := tomlElevated(in); got != want {
			t.Errorf("%q: %v", in, got)
		}
	}
}

// Ovoj git (minigw kot git.exe), pravilo 1 kot TestPathWrapDeps: init/clone le pod $MINIGW_DEPS, oznaka peskovnika,
// omrežje izklopljeno; drugi ukazi gredo skozi.
func TestGitWrapRefusal(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	wt := filepath.Join(root, "w", "c")
	deps := depsDir(wt)
	os.MkdirAll(filepath.Join(wt, "build"), 0o700)
	os.MkdirAll(deps, 0o700)
	hint := "create repositories only under $MINIGW_DEPS (" + deps + ")"
	env := func(kv ...string) func(string) string {
		m := map[string]string{"MINIGW_DEPS": deps, "MINIGW_SANDBOXED": "1"}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return func(k string) string { return m[k] }
	}
	for _, tc := range []struct {
		dir  string
		args []string
		env  func(string) string
		want string
	}{
		{wt, []string{"status"}, env(), ""},
		{wt, []string{"-C", "x", "commit", "-m", "init"}, env(), ""},
		{wt, []string{"init", "-q", filepath.Join(deps, "x")}, env(), ""},
		{deps, []string{"-c", "a=b", "clone", "-q", "--depth", "1", "https://h/r.git"}, env(), ""},
		{deps, []string{"init", "-q"}, env(), ""},
		{wt, []string{"-C", deps, "clone", "../c", "y"}, env(), ""},
		{wt, []string{"init", "-q", "build/_deps/foo"}, env(), hint},
		{wt, []string{"clone", "-q", "src", "build/_deps/bar"}, env(), hint},
		{wt, []string{"init", "-q", filepath.Join(root, "z")}, env(), hint},
		{wt, []string{"init", "-q", filepath.Join(deps, "n", "..", "..", "esc")}, env(), hint},
		{wt, []string{"clone", "--separate-git-dir", filepath.Join(wt, "g"), "src", filepath.Join(deps, "s")}, env(), hint},
		{wt, []string{"clone", "https://h/r.git"}, env(), hint}, // cilj r v klonu
		{deps, []string{"init", "h"}, env("MINIGW_SANDBOXED", ""), "git init and git clone need the agent sandbox, which this run does not have"},
		{deps, []string{"init", "h"}, env("MINIGW_DEPS", ""), "this run cannot write (read-only)"},
		{deps, []string{"clone", "https://example.invalid/a.git"}, env("MINIGW_NETWORK_OFF", "Network for builds"), "network for this agent's builds is off (Network for builds)"},
		{deps, []string{"clone", "git@host:a.git"}, env("MINIGW_NETWORK_OFF", "N"), "network for this agent's builds is off (N)"},
		{deps, []string{"clone", wt, "w"}, env("MINIGW_NETWORK_OFF", "N"), ""},
	} {
		if got := gitWrapRefusal(tc.dir, tc.args, tc.env); !strings.HasPrefix(got, tc.want) || (tc.want == "") != (got == "") {
			t.Errorf("%s git %v: %q", tc.dir, tc.args, got)
		}
	}
	for u, want := range map[string]bool{"https://a/b": true, "git@h:r": true, "file:///x": false, "/x": false, "./x": false,
		`C:\repo`: false, "C:/repo": false, `\\srv\share`: false, "ssh://h/r": true, "r": false} {
		if remoteGitURL(u) != want {
			t.Errorf("remote %q", u)
		}
	}
}

// Ovoj git zares: kopija testnega programa kot git.exe požene pravi git (init v $MINIGW_DEPS) ali zavrne z izhodom 128.
func TestGitWrapExec(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil || runtime.GOOS == "windows" {
		t.Skip("needs git; Windows: covered on HPX360")
	}
	root, _ := filepath.EvalSymlinks(t.TempDir())
	deps := filepath.Join(root, "c.deps")
	os.MkdirAll(deps, 0o700)
	exe, _ := os.Executable()
	wrap := filepath.Join(root, "bin", "git.exe")
	os.MkdirAll(filepath.Dir(wrap), 0o700)
	b, _ := os.ReadFile(exe)
	os.WriteFile(wrap, b, 0o700)
	run := func(args ...string) (string, int) {
		cmd := exec.Command(wrap, args...)
		cmd.Dir = deps
		cmd.Env = append(os.Environ(), "MINIGW_REAL_GIT="+git, "MINIGW_DEPS="+deps, "MINIGW_SANDBOXED=1")
		out, err := cmd.CombinedOutput()
		if e, ok := err.(*exec.ExitError); ok {
			return string(out), e.ExitCode()
		}
		return string(out), 0
	}
	if out, c := run("init", "-q", "x"); c != 0 {
		t.Fatalf("init in deps: %d %s", c, out)
	}
	if _, err := os.Stat(filepath.Join(deps, "x", ".git")); err != nil {
		t.Fatal("no repository")
	}
	if out, c := run("init", "-q", filepath.Join(root, "out")); c != 128 || !strings.HasPrefix(out, "minigw: create repositories only under $MINIGW_DEPS") {
		t.Fatalf("outside: %d %s", c, out)
	}
	if out, c := run("-C", filepath.Join(deps, "x"), "rev-parse", "--is-inside-work-tree"); c != 0 || strings.TrimSpace(out) != "true" {
		t.Fatalf("pass through: %d %s", c, out)
	}
}

// Posrednik CONNECT vrat izvajanja (#68): dovoljen gostitelj → tunel; drug gostitelj ali vrata → 403 z imenom
// nastavitve; brez »May ask other models« navadne zahteve 404 (vrata so le za posrednik).
func TestRunConnectProxy(t *testing.T) {
	s, _ := testServer(t, nil)
	echo, _ := net.Listen("tcp", "127.0.0.1:0")
	defer echo.Close()
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	old, oldR := proxyDial, proxyResolve
	proxyResolve = func(host string) ([]net.IP, error) {
		if host == "api.anthropic.com" {
			return []net.IP{net.ParseIP("10.1.1.1"), net.ParseIP("160.79.104.10")}, nil // privatni se preskoči
		}
		return []net.IP{net.ParseIP("127.0.0.1")}, nil
	}
	proxyDial = func(addr string) (net.Conn, error) {
		if addr != "160.79.104.10:443" {
			t.Errorf("dial %s", addr)
		}
		return net.Dial("tcp", echo.Addr().String())
	}
	defer func() { proxyDial, proxyResolve = old, oldR }()
	parent := httptest.NewRequest("POST", "/v1/messages", nil)
	r := &req{Request: parent, conn: &serveConn{device: &serveDevice{Name: "pc"}, peer: "10.0.0.9"}}
	run, env := s.startRun(r, "claude", "/copy", false, true, "", "", "", "", claudeAPIHosts, claudeNetworkSetting)
	if run == nil || env != nil || run.asks() || run.runAddr() == "" {
		t.Fatalf("run: %v %v", run, env)
	}
	defer run.stop()
	connect := func(host string) (string, net.Conn) {
		c, err := net.Dial("tcp", run.addr)
		if err != nil {
			t.Fatal(err)
		}
		c.Write([]byte("CONNECT " + host + " HTTP/1.1\r\nHost: " + host + "\r\n\r\n"))
		br := bufio.NewReader(c)
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 200 {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			c.Close()
			return resp.Status + " " + string(body), nil
		}
		return resp.Status, c
	}
	if st, _ := connect("api.anthropic.com:443"); st != "403 Forbidden" && !strings.Contains(st, "Not a process") {
		t.Fatalf("before start: %s", st)
	}
	run.started(os.Getpid())
	st, c := connect("api.anthropic.com:443")
	if c == nil {
		t.Fatalf("allowed host: %s", st)
	}
	c.Write([]byte("ping\n"))
	if l, _ := bufio.NewReader(c).ReadString('\n'); l != "ping\n" {
		t.Fatalf("tunnel: %q", l)
	}
	c.Close()
	claudeAPIHosts = append(claudeAPIHosts, "rebind.example") // dovoljen gostitelj, ki se razreši v loopback
	defer func() { claudeAPIHosts = claudeAPIHosts[:len(claudeAPIHosts)-1] }()
	run.allow = claudeAPIHosts
	for _, h := range []string{"github.com:443", "api.anthropic.com:80", "127.0.0.1:443", "rebind.example:443"} {
		if st, c := connect(h); c != nil || !strings.HasPrefix(st, "403") || !strings.Contains(st, "Network for builds") {
			t.Fatalf("%s: %s", h, st)
		}
	}
	// izvajanje z omrežjem (le vprašanja drugim modelom, Codex): posrednika ni
	s2, _ := testServer(t, func(c *serveConfig) { c.Codex.AskOthers = true })
	run2, _ := s2.startRun(r, "codex", "/copy", false, true, "", "", "", "", nil, "")
	defer run2.stop()
	run2.started(os.Getpid())
	c2, _ := net.Dial("tcp", run2.addr)
	c2.Write([]byte("CONNECT api.anthropic.com:443 HTTP/1.1\r\nHost: api.anthropic.com:443\r\n\r\n"))
	if resp, err := http.ReadResponse(bufio.NewReader(c2), nil); err != nil || resp.StatusCode != 403 {
		t.Fatalf("CONNECT without the proxy: %v %v", resp, err)
	}
	c2.Close()
	q, _ := http.NewRequest("GET", "http://"+run.addr+"/v1/models", nil)
	q.Header.Set("Authorization", "Bearer "+run.token)
	if resp, err := http.DefaultClient.Do(q); err != nil || resp.StatusCode != 404 {
		t.Fatalf("ask without the switch: %v %v", resp, err)
	}
}

// Pripeti vrednosti v minigw in v gradnji (scripts/mxc.sha256, fetch-mxc.sh) se ujemata.
func TestWxcHashesMatchBuild(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "scripts", "mxc.sha256"))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, l := range strings.Split(string(b), "\n") {
		if f := strings.Fields(l); len(f) == 2 {
			got[f[1]] = f[0]
		}
	}
	if got["x64/wxc-exec.exe"] != wxcHashes["amd64"] || got["arm64/wxc-exec.exe"] != wxcHashes["arm64"] {
		t.Fatalf("scripts/mxc.sha256 %v, client/mxc.go %v", got, wxcHashes)
	}
}

// Vrstica Login v skrbniškem API (#68): žeton Claude in ključ API se shranita, odgovor ima le stanje; napačne vrste
// in oblike so 400; podedovani ključi API ne pridejo do agenta.
func TestAgentLoginAPI(t *testing.T) {
	keys := map[string]string{}
	og, os2, od := keyGet, keySet, keyDel
	keyGet = func(_, n string) (string, string) { return keys[n], "" }
	keySet = func(_, n, v string) (string, error) { keys[n] = v; return "", nil }
	keyDel = func(_, n string) { delete(keys, n) }
	defer func() { keyGet, keySet, keyDel = og, os2, od }()
	s, _, base := liveServer(t, nil)
	tok := "sk-ant-oat01-secret-value-0123456789"
	for _, tc := range []struct {
		path, body string
		want       int
	}{
		{"/agents/claude", `{"token":"` + tok + `"}`, 200},
		{"/agents/claude", `{"token":"github_pat_x"}`, 400},
		{"/agents/codex", `{"apiKey":"sk-proj-0123456789abcdefghij"}`, 200},
		{"/agents/codex", `{"token":"x"}`, 400},
		{"/agents/copilot", `{"apiKey":"0123456789abcdefghijk"}`, 400},
		{"/agents/grok", `{"apiKey":"bad key with spaces 12345"}`, 400},
	} {
		if st, b := adminNoise(t, s, base, s.admin, "PATCH", adminPrefix+tc.path, tc.body); st != tc.want || strings.Contains(b, tok) {
			t.Fatalf("%s %s: %d %s", tc.path, tc.body, st, b)
		}
	}
	if keys[agentLoginKey("claude")] != tok || keys[agentAPIKeyName("codex")] == "" {
		t.Fatalf("keys: %v", keys)
	}
	_, b := adminNoise(t, s, base, s.admin, "GET", adminPrefix+"/agents/claude", "")
	if !strings.Contains(b, `"state":"set"`) || !strings.Contains(b, "claude setup-token") || strings.Contains(b, tok) {
		t.Fatalf("claude login: %s", b)
	}
	if _, b := adminNoise(t, s, base, s.admin, "GET", adminPrefix+"/agents/codex", ""); !strings.Contains(b, "billed through the provider's API") || strings.Contains(b, "sk-proj") {
		t.Fatalf("codex login: %s", b)
	}
	if got := agentKeyEnv(s.cfgDir, "codex"); len(got) != 1 || !strings.HasPrefix(got[0], "CODEX_API_KEY=sk-proj") {
		t.Fatalf("codex env: %v", got)
	}
	for _, k := range []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "XAI_API_KEY", "CODEX_API_KEY"} {
		if slices.ContainsFunc(agentEnv([]string{k + "=inherited"}), func(kv string) bool { return strings.HasPrefix(kv, k) }) {
			t.Fatalf("%s inherited", k)
		}
	}
}

// Gradnja pred preizkusom (UBR) in podedovani ključi *_API_KEY.
func TestBuildTooOldAndKeyStrip(t *testing.T) {
	for b, want := range map[string]bool{"26200.9711": false, "26200.9549": true, "26100.9550": false, "22631.5000": true, "28000.100": false, "": false, "x": false} {
		if buildTooOld(b) != want {
			t.Errorf("%q", b)
		}
	}
	if got := agentEnv([]string{"SOME_VENDOR_API_KEY=x", "Mistral_Api_Key=y", "PATH=/bin"}); !slices.Equal(got, []string{"PATH=/bin"}) {
		t.Fatalf("env: %v", got)
	}
}

// Vsako stanje vrstice Login (agentlogin.go) ima slovenski prevod na strani z nastavitvami (LOGIN_STATES).
func TestLoginStatesTranslated(t *testing.T) {
	src, _ := os.ReadFile("agentlogin.go")
	page, _ := os.ReadFile("settings.html")
	i := strings.Index(string(page), "const LOGIN_STATES = {")
	block := string(page)[i : i+strings.Index(string(page)[i:], "};")]
	found := regexp.MustCompile(`(?:return|o\["state"\] =) "((?:logged in|not logged in|unknown|API key \()[^"]*)"`).FindAllStringSubmatch(string(src), -1)
	if len(found) < 7 {
		t.Fatalf("state strings: %v", found)
	}
	for _, m := range found {
		if !strings.Contains(block, `"`+m[1]+`":`) {
			t.Errorf("no Slovenian text for %q", m[1])
		}
	}
	for _, s := range []string{"set", "not set"} {
		if !strings.Contains(block, `"`+s+`":`) {
			t.Errorf("%q", s)
		}
	}
}
