package main

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestPeerTables(t *testing.T) {
	ours := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 8765}
	peer := &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 50000}
	// /proc/net/tcp: lokalno = odjemalec (C350 = 50000), oddaljeno = router (223D = 8765).
	tcp := "  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n" +
		"   0: 0100007F:223D 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 111 1\n" +
		"   1: 0100007F:C350 0100007F:223D 01 00000000:00000000 00:00000000 00000000  1000        0 4242 1\n"
	if in, uid := procSocketInode([]byte(tcp), peer, ours); in != "4242" || uid != 1000 {
		t.Fatalf("tcp inode %q uid %d", in, uid)
	}
	tcp6 := "   0: 0000000000000000FFFF00000100007F:C350 0000000000000000FFFF00000100007F:223D 01 0 0 0 1000 0 777 1\n"
	if in, _ := procSocketInode([]byte(tcp6), peer, ours); in != "777" {
		t.Fatalf("tcp6 (v4-mapped) inode %q", in)
	}
	if in, _ := procSocketInode([]byte(tcp), ours, peer); in != "" {
		t.Fatal("router side matched as client")
	}

	row := func(lip net.IP, lp int, rip net.IP, rp int, pid uint32) []byte {
		r := make([]byte, 24)
		copy(r[4:8], lip.To4())
		r[8], r[9] = byte(lp>>8), byte(lp)
		copy(r[12:16], rip.To4())
		r[16], r[17] = byte(rp>>8), byte(rp)
		binary.LittleEndian.PutUint32(r[20:], pid)
		return r
	}
	tab := binary.LittleEndian.AppendUint32(nil, 2)
	tab = append(tab, row(ours.IP, 8765, ours.IP, 50000, 1)...) // router
	tab = append(tab, row(peer.IP, 50000, ours.IP, 8765, 99)...)
	if pid := tcpTablePID(tab, false, peer, ours); pid != 99 {
		t.Fatalf("v4 pid %d", pid)
	}
	r6 := make([]byte, 56)
	copy(r6[0:16], net.IPv6loopback)
	r6[20], r6[21] = byte(50000>>8), byte(50000&0xff)
	copy(r6[24:40], net.IPv6loopback)
	r6[44], r6[45] = byte(8765>>8), byte(8765&0xff)
	binary.LittleEndian.PutUint32(r6[52:], 7)
	if pid := tcpTablePID(append(binary.LittleEndian.AppendUint32(nil, 1), r6...), true,
		&net.TCPAddr{IP: net.IPv6loopback, Port: 50000}, &net.TCPAddr{IP: net.IPv6loopback, Port: 8765}); pid != 7 {
		t.Fatalf("v6 pid %d", pid)
	}
}

// Testni program je tudi pomočnik agentov (startAgent na Linuxu požene /proc/self/exe __minigw-agent …).
func TestMain(m *testing.M) {
	if strings.HasPrefix(strings.ToLower(filepath.Base(os.Args[0])), "claude") { // #79: lažni claude (TestClaudeAncestorRefusesBeforeClaude)
		fakeClaudeMain()
	}
	if os.Getenv("MINIGW_TEST_FAKE_WXC") == "1" { // #68: lažni wxc-exec (mxc_test.go)
		fakeWxcMain()
	}
	if isGitWrap() { // #68: kopija testnega programa kot git.exe (TestGitWrapExec)
		gitWrapMain(os.Args[1:])
	}
	if len(os.Args) > 1 && os.Args[1] == agentReaperCmd {
		agentReaperMain(os.Args[2:])
	}
	if os.Getenv("MINIGW_TEST_INIT") == "1" { // #52: PID 1 žanje (initReaper), otrok pusti osirotelega vnuka
		initReaper()
		if _, err := os.ReadFile("/proc/1/environ"); err == nil && os.Getuid() != 0 { // M1: PID 1 ni dumpable (root ga bere vseeno)
			fmt.Println("/proc/1/environ is readable")
			os.Exit(4)
		}
		exec.Command("sh", "-c", "sleep 0.2 &").Run()
		time.Sleep(time.Second)
		ents, _ := os.ReadDir("/proc")
		for _, e := range ents {
			if b, err := os.ReadFile("/proc/" + e.Name() + "/stat"); err == nil && strings.Contains(string(b), ") Z ") {
				fmt.Println("zombie: " + string(b))
				os.Exit(3)
			}
		}
		os.Exit(0)
	}
	if f := os.Getenv("MINIGW_TEST_FDS"); f != "" { // #80: datoteka, odprta pri zaprtem fd 2, ne dobi diagnostike (fds_unix_test.go)
		h, _ := os.Create(f)
		fmt.Fprintln(os.Stderr, "minigw: diagnostic")
		h.Close()
		os.Exit(0)
	}
	if os.Getenv("MINIGW_TEST_MAIN") == "1" { // testi poženejo sam ukaz minigw
		os.Args = append([]string{"minigw"}, os.Args[1:]...)
		main()
		os.Exit(0)
	}
	os.Exit(runIsolated(m))
}

// H1, R1, R4: proces, ki ga je pognal router (agent), tudi vnuk po dvojnem forku, ne pride skozi skrbniški API
// (s pravo skrivnostjo) in ne do prijave strani z nastavitvami.
func TestAdminRefusesAgentProcess(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "windows" {
		t.Skip("peer check: Linux and Windows (the Mac app has PeerProcess)")
	}
	s, _, base := liveServer(t, nil)
	env := "MINIGW_PEER_HELPER=" + base + "|" + s.admin + "|" + hex.EncodeToString(s.key.PublicKey().Bytes())
	run := func(cmd *exec.Cmd, start func(*exec.Cmd) error) string {
		cmd.Env = append(os.Environ(), env, "T="+os.Args[0])
		var out strings.Builder
		cmd.Stdout = &out
		if err := start(cmd); err != nil {
			t.Fatal(err)
		}
		cmd.Wait()
		return out.String()
	}
	child := func(start func(*exec.Cmd) error, mode string) string {
		return run(exec.Command(os.Args[0], "-test.run=^TestPeerHelper$", mode), start)
	}
	plain := func(c *exec.Cmd) error { return c.Start() }
	if out := child(startAgent, ""); !strings.Contains(out, "status 403") || !strings.Contains(out, "agent process") {
		t.Fatalf("agent got through: %q", out)
	}
	if out := child(startAgent, "login"); !strings.Contains(out, "status 403") { // R4: pred preverjanjem povezave
		t.Fatalf("agent reached the settings login: %q", out)
	}
	if runtime.GOOS == "linux" {
		// R1: srednji proces takoj konča; vnuk se prevesi na pomočnika (subreaper), ne na init.
		fork := `sh -c '(sleep 0.5; exec "$T" -test.run=^TestPeerHelper$) & exit 0'; sleep 3`
		if out := run(exec.Command("/bin/sh", "-c", fork), startAgent); !strings.Contains(out, "status 403") {
			t.Fatalf("double-forked agent got through: %q", out)
		}
		// Kontrola: brez pomočnika veriga razpade (zato je pomočnik potreben, in test bi to opazil).
		if out := run(exec.Command("/bin/sh", "-c", fork), plain); !strings.Contains(out, "status 200") {
			t.Fatalf("control: %q", out)
		}
	}
	if runtime.GOOS == "windows" { // Windows: zunaj Joba agentov je odjemalec dovoljen
		if out := child(plain, ""); !strings.Contains(out, "status 200") {
			t.Fatalf("plain process refused: %q", out)
		}
	}
	if st, _ := adminNoise(t, s, base, s.admin, "GET", adminPrefix+"/status", ""); st != 200 { // ta proces sam
		t.Fatalf("self: %d", st)
	}
}

func TestPeerHelper(t *testing.T) {
	v := os.Getenv("MINIGW_PEER_HELPER")
	if v == "" {
		t.Skip("helper for TestAdminRefusesAgentProcess")
	}
	p := strings.SplitN(v, "|", 3)
	if slices.Contains(os.Args, "login") {
		resp, err := http.Post(p[0]+"/minigw/settings/login", "application/json", strings.NewReader(`{"t":"x"}`))
		if err == nil {
			fmt.Printf("status %d\n", resp.StatusCode)
		}
		return
	}
	key, _ := hex.DecodeString(p[2])
	a := &adminClient{base: p[0], secret: p[1], routerKey: key}
	st, b, err := a.http("GET", adminPrefix+"/status", nil, 5*time.Second)
	fmt.Printf("status %d %s %v\n", st, b, err)
}

// R2: router ni dumpable (drug proces ne bere njegovega environ), sam pa še bere /proc drugih procesov.
func TestHardenRouter(t *testing.T) {
	if os.Getenv("MINIGW_DUMP_HELPER") == "1" {
		hardenRouter()
		_, e1 := os.ReadDir(fmt.Sprintf("/proc/%d/fd", os.Getppid()))
		_, e2 := os.ReadDir("/proc/self/fd")
		fmt.Printf("scan %v %v\n", e1, e2)
		time.Sleep(2 * time.Second)
		return
	}
	if runtime.GOOS != "linux" || os.Getuid() == 0 {
		t.Skip("Linux, as a normal user (root may have CAP_SYS_PTRACE)")
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestHardenRouter$")
	cmd.Env = append(os.Environ(), "MINIGW_DUMP_HELPER=1")
	out, _ := cmd.StdoutPipe()
	cmd.Start()
	defer cmd.Wait()
	line, _ := bufio.NewReader(out).ReadString('\n')
	if line != "scan <nil> <nil>\n" {
		t.Fatalf("router cannot read /proc: %q", line)
	}
	if _, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", cmd.Process.Pid)); err == nil {
		t.Fatal("environ of a non-dumpable router is readable")
	}
}

// R3, R5: domača mapa in mapa nastavitev prazni; programi le za branje, mape agenta za pisanje, vtičnice skrite.
func TestBwrapArgs(t *testing.T) {
	a := strings.Join(bwrapArgs("/home/u", "/home/u/.config/minigw", []string{"/srv/scratch"}, []string{"/home/u/.nvm"}, []string{"/home/u/.codex", "/s/copy"}, "", nil, []string{"/s/copy/.git/config"},
		[]string{"/run/user/1/bus"}, []string{"/tmp/dbus-x"}), " ")
	want := "--dev-bind / / --die-with-parent --unshare-pid --proc /proc --tmpfs /home/u --tmpfs /srv/scratch --ro-bind /home/u/.nvm /home/u/.nvm --bind /home/u/.codex /home/u/.codex " +
		"--bind /s/copy /s/copy --ro-bind /s/copy/.git/config /s/copy/.git/config --tmpfs /home/u/.config/minigw --ro-bind /dev/null /run/user/1/bus --tmpfs /tmp/dbus-x"
	if a != want {
		t.Fatalf("bwrap args:\n%s\n%s", a, want)
	}
}

// H1.5: na Linuxu z bwrap Codex in Grok ne vidita mape nastavitev.
func TestAgentWrapHidesConfig(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("bwrap: Linux only (/bin/sh, ls, cat)")
	}
	home, _ := os.UserHomeDir()
	// N1: v PATH je točna mapa ~/.mgwtest/bin; ostalo v ~/.mgwtest in ~/.mgwtest/bin/.ssh ostane skrito.
	os.MkdirAll(home+"/.mgwtest/bin/.ssh", 0o700)
	defer os.RemoveAll(home + "/.mgwtest")
	os.WriteFile(home+"/.mgwtest/bin/tool", []byte("TOOL"), 0o600)
	os.WriteFile(home+"/.mgwtest/other", []byte("OTHER"), 0o600)
	t.Setenv("PATH", os.Getenv("PATH")+":"+home+"/.mgwtest/bin")
	cfg, _ := os.MkdirTemp(home, "cfg") // pod domačo, kot privzeta mapa nastavitev
	defer os.RemoveAll(cfg)
	os.WriteFile(cfg+"/serve.json", []byte("SECRET"), 0o600)
	os.WriteFile(home+"/.minigw-test-private", []byte("PRIVATE"), 0o600)
	defer os.Remove(home + "/.minigw-test-private")
	copyDir := t.TempDir()
	os.WriteFile(copyDir+"/src.txt", []byte("COPY"), 0o600)
	name, args, _, _ := agentWrap("/bin/sh", []string{"-c", "cat " + cfg + "/serve.json " + home + "/.minigw-test-private " + copyDir + "/src.txt " +
		home + "/.mgwtest/bin/tool " + home + "/.mgwtest/other; ls -A " + home + "/.mgwtest/bin/.ssh; echo end"},
		agentMounts{cfgDir: cfg, rw: []string{copyDir}})
	if name == "/bin/sh" {
		t.Skip("no usable bwrap (Linux with user namespaces)")
	}
	out, _ := exec.Command(name, args...).CombinedOutput()
	if strings.Contains(string(out), "SECRET") || strings.Contains(string(out), "PRIVATE") || !strings.Contains(string(out), "COPY") || !strings.Contains(string(out), "end") ||
		!strings.Contains(string(out), "TOOL") || strings.Contains(string(out), "OTHER") {
		t.Fatalf("home or config visible under bwrap: %q", out)
	}
}

// N1: paket CLI pod domačo mapo: različica nvm, sicer mapa s package.json, sicer mapa programa.
func TestPackageRoot(t *testing.T) {
	home := t.TempDir()
	p := filepath.FromSlash // packageRoot vrača očiščene poti (na Windows z \)
	if r := packageRoot(home, p(home+"/.nvm/versions/node/v22.1.0/lib/node_modules/@openai/codex/bin/codex.js")); r != p(home+"/.nvm/versions/node/v22.1.0") {
		t.Fatalf("nvm: %s", r)
	}
	pkg := p(home + "/.npm-global/lib/node_modules/@openai/codex")
	os.MkdirAll(pkg+p("/bin"), 0o755)
	os.WriteFile(pkg+p("/package.json"), []byte("{}"), 0o644)
	if r := packageRoot(home, pkg+p("/bin/codex.js")); r != pkg {
		t.Fatalf("npm: %s", r)
	}
	if r := packageRoot(home, p(home+"/.grok/bin/grok")); r != p(home+"/.grok/bin") {
		t.Fatalf("plain: %s", r)
	}
}

// H1 (2. plast), L1: dovoljena mapa okoli Scratch: po rw znova tmpfs na Scratch, nato klon in objekti; .git klona je
// točka priklopa pred zaščitenimi datotekami.
func TestBwrapRehidesScratch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bwrap: Linux only (paths with /)")
	}
	a := strings.Join(bwrapArgs("/home/u", "/home/u/.config/minigw", nil, []string{"/s/repos/d/r.git/objects"},
		[]string{"/s/worktrees/d/r/claude-x-0", "/srv"}, "/srv/s", []string{"/s/worktrees/d/r/claude-x-0/.git"},
		[]string{"/s/worktrees/d/r/claude-x-0/.git/config"}, nil, nil), " ")
	want := "--bind /srv /srv --tmpfs /srv/s --bind /s/worktrees/d/r/claude-x-0/.git /s/worktrees/d/r/claude-x-0/.git " +
		"--ro-bind /s/worktrees/d/r/claude-x-0/.git/config /s/worktrees/d/r/claude-x-0/.git/config"
	if !strings.Contains(a, want) {
		t.Fatalf("bwrap args:\n%s\nwant …%s…", a, want)
	}
	b := strings.Join(bwrapArgs("/home/u", "/c", nil, []string{"/srv/s/repos/d/r.git/objects"}, []string{"/srv", "/srv/s/worktrees/d/r/x"}, "/srv/s", nil, nil, nil, nil), " ")
	if !strings.Contains(b, "--tmpfs /srv/s --ro-bind /srv/s/repos/d/r.git/objects /srv/s/repos/d/r.git/objects --bind /srv/s/worktrees/d/r/x /srv/s/worktrees/d/r/x") {
		t.Fatalf("rebinds inside Scratch: %s", b)
	}
}
