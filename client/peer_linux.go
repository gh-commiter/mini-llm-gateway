package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Linux: vsak agent (claude, codex, grok) teče pod pomočnikom »minigw __minigw-agent«, ki je subreaper
// (PR_SET_CHILD_SUBREAPER): osirotel potomec (dvojni fork) se prevesi nanj, ne na init, zato veriga staršev do
// routerja ostane (R1). Pomočnik požanje vse svoje potomce (Wait4(-1) v routerju bi kradel stanja procesov os/exec)
// in ob koncu agenta ali ob SIGTERM ubije ostale, zato agent ne pusti procesov za sabo.
func startAgent(cmd *exec.Cmd) error {
	cmd.Args = append([]string{"/proc/self/exe", agentReaperCmd, cmd.Path}, cmd.Args[1:]...) // tudi, ko je minigw že zamenjan
	cmd.Path = "/proc/self/exe"
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM} // router umre: pomočnik pospravi
	if cmd.Cancel != nil {                                             // CommandContext: SIGTERM, da pomočnik pospravi
		cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
		cmd.WaitDelay = 10 * time.Second
	}
	return cmd.Start()
}

// stopAgentTree: pomočnik sam pospravi drevo, ko agent konča.
func stopAgentTree(*exec.Cmd) error { return nil }

// agentReaperMain: pomočnik (glej startAgent): args = program in argumenti agenta. Žetev in pobijanje sta v isti
// zanki (N5): otrok, ki ga ubijemo, še ni požet, zato njegov PID ne more pripadati drugemu procesu.
func agentReaperMain(args []string) {
	if len(args) == 0 {
		os.Exit(2)
	}
	syscall.RawSyscall(syscall.SYS_PRCTL, 36 /* PR_SET_CHILD_SUBREAPER */, 1, 0)
	sigs := make(chan os.Signal, 8)
	signal.Notify(sigs, syscall.SIGCHLD, syscall.SIGTERM, syscall.SIGINT) // pred zagonom: noben SIGCHLD ne uide
	exe, err := exec.LookPath(args[0])
	var pid int
	if err == nil {
		pid, err = syscall.ForkExec(exe, args, &syscall.ProcAttr{Env: os.Environ(), Files: []uintptr{0, 1, 2},
			Sys: &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}})
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "minigw: cannot start "+args[0]+": "+err.Error())
		os.Exit(127)
	}
	code, mainDone, stop := 0, false, false
	for {
		for { // požanji vse končane (WNOHANG); ECHILD = nobenega potomca več
			var ws syscall.WaitStatus
			p, err := syscall.Wait4(-1, &ws, syscall.WNOHANG, nil)
			if err == syscall.ECHILD {
				os.Exit(code)
			}
			if err != nil || p <= 0 {
				break
			}
			if p == pid {
				mainDone = true
				if code = ws.ExitStatus(); ws.Signaled() {
					code = 128 + int(ws.Signal())
				}
			}
		}
		if stop && !mainDone {
			syscall.Kill(-pid, syscall.SIGKILL) // skupina agenta; vodja še ni požet
		}
		if stop || mainDone {
			killChildren() // osiroteli so otroci pomočnika (prevesijo se ob izhodu starša, pred žetvijo)
		}
		if sig := <-sigs; sig != syscall.SIGCHLD {
			stop = true
		}
	}
}

// initReaper (#52): PID 1 (vsebnik brez docker run --init) dobi vse osirotele procese (npr. ostanke bwrap --unshare-pid);
// nepožeti zombiji obdržijo svoje imenske prostore. Wait4(-1) v routerju bi kradel stanja procesov os/exec, zato PID 1
// zažene isti ukaz kot otroka (prehod), sam pa le žanje vse, posreduje signale in konča z izhodom otroka (kot tini).
func initReaper() {
	if os.Getpid() != 1 {
		return
	}
	hardenRouter() // ima vse okolje (žetoni): /proc/1/environ ni berljiv agentom; otrok (exec) se zaščiti sam
	sigs := make(chan os.Signal, 8)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT) // le ta dva posreduje
	signal.Ignore(syscall.SIGHUP, syscall.SIGQUIT)       // kot prej: PID 1 jih ne konča
	// vedno sam sebe; brez /proc ForkExec ne uspe (agenti /proc tako ali tako rabijo)
	pid, err := syscall.ForkExec("/proc/self/exe", os.Args, &syscall.ProcAttr{Env: os.Environ(), Files: []uintptr{0, 1, 2}})
	if err != nil {
		fmt.Fprintln(os.Stderr, "minigw: cannot start the gateway under PID 1: "+err.Error())
		os.Exit(1)
	}
	go func() {
		for s := range sigs {
			syscall.Kill(pid, s.(syscall.Signal))
		}
	}()
	for {
		var ws syscall.WaitStatus
		p, err := syscall.Wait4(-1, &ws, 0, nil)
		if err == syscall.EINTR {
			continue
		}
		if err != nil {
			os.Exit(1)
		}
		if p == pid {
			code := ws.ExitStatus()
			if ws.Signaled() {
				code = 128 + int(ws.Signal())
			}
			os.Exit(code)
		}
	}
}

// killChildren: SIGKILL vsem otrokom tega procesa (iz /proc/*/stat).
func killChildren() {
	self := os.Getpid()
	ents, _ := os.ReadDir("/proc")
	for _, e := range ents {
		if pid, err := strconv.Atoi(e.Name()); err == nil && procPPID(pid) == self {
			syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}

// hardenRouter: router ni »dumpable« (R2): drug proces istega uporabnika (agent) ne bere /proc/<router>/mem ali
// environ in se ne more nanj pripeti s ptrace. Branje /proc drugih procesov (peerCheck) ostane.
func hardenRouter() { syscall.RawSyscall(syscall.SYS_PRCTL, 4 /* PR_SET_DUMPABLE */, 0, 0) }

var (
	bwrapOnce   sync.Once
	bwrapPath   string // "" = bwrap ni nameščen ali ne deluje (npr. brez uporabniških imenskih prostorov)
	nestedMu    sync.Mutex
	nestedAt    time.Time // zadnji neuspeli preizkus (N3b: ponovi se po nestedRetry, uspeh velja do konca procesa)
	nestedBwrap bool      // bwrap deluje tudi znotraj bwrap (pinWrap)
	nestedRetry = time.Minute
)

func bwrapReady(cfgDir string) string {
	bwrapOnce.Do(func() {
		p, err := exec.LookPath("bwrap")
		t, err2 := exec.LookPath("true")
		home, _ := os.UserHomeDir()
		if err == nil && err2 == nil && home != "" &&
			command(p, append(bwrapArgs(home, cfgDir, nil, nil, nil, "", nil, nil, nil, nil), "--", t)...).Run() == nil {
			bwrapPath = p
		}
	})
	return bwrapPath
}

// agentSandboxed: Codex in Grok sta zaprta pred skrivnostmi routerja le pod bwrap; sicer le z izrecnim dovoljenjem.
func agentSandboxed(cfgDir string) bool { return bwrapReady(cfgDir) != "" }

// pinWrap: Claude CLI v worktree pod bwrap le s pins (N3): datotečni sistem je kot zunaj (Claude potrebuje domačo
// mapo), le .git klona je točka priklopa, zato ga med tekom ni mogoče preimenovati ali zamenjati (EBUSY). Brez bwrap
// ostane exe, kot je; zamenjavo nato ujame worktreeIntact pred naslednjim zagonom.
// Lastni sandbox Claude Code (bwrap) nato teče gnezden v imenskem prostoru pinWrap; kjer gnezdeni uporabniški imenski
// prostori niso dovoljeni (Ubuntu 24.04, AppArmor bwrap-userns-restrict), bi Bash v Claude odpovedal: takrat pinWrap
// to zapiše in ga izpusti; preizkus ponovi najpogosteje enkrat na minuto (N3b: tudi po prehodni napaki). Brez pin
// runCLI preverja klon med tekom (worktreeIntact) in claude ob zamenjavi .git ustavi.
func pinWrap(cfgDir, exe string, args, pins []string, logf func(string, ...any)) (string, []string) {
	if !claudePinOK(cfgDir, logf) {
		return exe, args
	}
	a := []string{"--dev-bind", "/", "/", "--die-with-parent", "--unshare-pid", "--proc", "/proc"}
	for _, p := range pins {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			a = append(a, "--bind", r, r)
		}
	}
	return bwrapPath, append(append(a, "--", exe), args...)
}

// claudePinOK: pinWrap deluje (bwrap in gnezdeni bwrap v svojem imenskem prostoru PID); preizkus kot prej.
func claudePinOK(cfgDir string, logf func(string, ...any)) bool {
	bw := bwrapReady(cfgDir)
	if bw == "" {
		return false
	}
	nestedMu.Lock()
	if !nestedBwrap && time.Since(nestedAt) >= nestedRetry {
		t, err := exec.LookPath("true")
		d := os.TempDir()
		if nestedBwrap = err == nil && command(bw, "--dev-bind", "/", "/", "--die-with-parent", "--unshare-pid", "--proc", "/proc", "--bind", d, d, "--",
			bw, "--dev-bind", "/", "/", "--", t).Run() == nil; !nestedBwrap {
			nestedAt = time.Now()
			logf("claude: nested bwrap does not work here (user namespaces restricted?), so the .git pin of worktree clones is off; the clone is checked during each run instead")
		}
	}
	ok := nestedBwrap
	nestedMu.Unlock()
	return ok // N1: s pin ima claude svoj imenski prostor PID; ko konča, jedro ustavi vse potomce (tudi setsid)
}

// watchGitMoves (N3c): inotify na mapi .git klona (IN_MOVE_SELF, IN_DELETE_SELF). Dogodek ostane v vrsti jedra, tudi če
// agent .git takoj vrne nazaj, zato ga zamenjava in vrnitev med dvema preverbama ne skrijeta; moved teče ob prvem
// dogodku (tudi IN_IGNORED, prepolna vrsta) in ob vsaki napaki branja razen zaprtja (stop). false: inotify ne deluje.
// dirs: vse mape (pinPaths) z eno instanco inotify (meja instanc na uporabnika je nizka, 128).
func watchGitMoves(dirs []string, moved func()) (stop func(), ok bool) {
	fd, err := syscall.InotifyInit1(syscall.IN_CLOEXEC | syscall.IN_NONBLOCK)
	if err != nil {
		return nil, false
	}
	for _, d := range dirs {
		if _, err := syscall.InotifyAddWatch(fd, d, syscall.IN_MOVE_SELF|syscall.IN_DELETE_SELF|syscall.IN_DONT_FOLLOW); err != nil {
			syscall.Close(fd)
			return nil, false
		}
	}
	f := os.NewFile(uintptr(fd), "inotify") // neblokirajoč: Close prekine Read
	go func() {
		if n, err := f.Read(make([]byte, 4096)); n > 0 || !errors.Is(err, os.ErrClosed) {
			moved()
		}
	}()
	return func() { f.Close() }, true
}

// agentWrap: Codex ali Grok pod bwrap (R3, N1, N2): domača mapa in mapa nastavitev sta prazni; nazaj pridejo le
// točne mape s programi iz PATH in paket CLI (samo za branje), stanje CLI te naprave in njene datoteke prijave,
// kopija mape, začasna mapa zahteve in dovoljene mape. Skriti so XDG_RUNTIME_DIR (D-Bus, keyring, agent gpg),
// /tmp/dbus-*, /tmp/ssh-* in občutljive mape, če bi bile znotraj vezane mape. Poti brez simbolnih povezav (R5).
func agentWrap(exe string, args []string, m agentMounts) (string, []string, func(), error) {
	bw := bwrapReady(m.cfgDir)
	if bw == "" {
		return exe, args, func() {}, nil
	}
	real := func(p string) string {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return r
		}
		return p
	}
	home, _ := os.UserHomeDir()
	home = real(home)
	inside := func(p, d string) bool { return p == d || strings.HasPrefix(p, d+string(filepath.Separator)) }
	exists := func(p string) bool { _, err := os.Stat(p); return err == nil }
	var ro, rw []string
	add := func(list *[]string, p string) {
		if !slices.Contains(*list, p) {
			*list = append(*list, p)
		}
	}
	for _, d := range filepath.SplitList(os.Getenv("PATH")) {
		if r := real(d); inside(r, home) && r != home && exists(r) {
			add(&ro, r)
		}
	}
	if r := real(exe); inside(r, home) {
		add(&ro, packageRoot(home, r))
	}
	for _, p := range m.ro {
		if exists(p) {
			add(&ro, real(p))
		}
	}
	if m.state != "" {
		os.MkdirAll(m.state, 0o700)
		add(&rw, real(m.state))
	}
	for _, p := range append(slices.Clone(m.files), m.rw...) {
		if exists(p) {
			add(&rw, real(p))
		}
	}
	ro = slices.DeleteFunc(ro, func(d string) bool { return slices.Contains(rw, d) })
	var hideFiles, hideDirs []string
	hide := func(p string) {
		if fi, err := os.Lstat(p); err == nil && fi.IsDir() {
			hideDirs = append(hideDirs, p)
		} else if err == nil && fi.Mode()&os.ModeSymlink == 0 {
			hideFiles = append(hideFiles, p)
		}
	}
	for _, p := range []string{".ssh", ".gnupg", ".aws", ".netrc", ".config", ".cargo/credentials", ".cargo/credentials.toml", ".local/share/keyrings"} {
		p = filepath.Join(home, p)
		for _, d := range append(slices.Clone(ro), rw...) {
			if inside(p, d) && p != d {
				hide(p)
				break
			}
		}
	}
	if rt := os.Getenv("XDG_RUNTIME_DIR"); rt != "" && exists(rt) {
		hideDirs = append(hideDirs, real(rt))
	}
	for _, p := range m.tmps { // #53
		if exists(p) {
			hideDirs = append(hideDirs, real(p))
		}
	}
	for _, pat := range []string{"/tmp/dbus-*", "/tmp/ssh-*"} {
		found, _ := filepath.Glob(pat)
		for _, p := range found {
			hide(p)
		}
	}
	var hidden []string
	if m.hide != "" && exists(m.hide) && !inside(real(m.hide), home) { // v domači mapi jo skrije že --tmpfs home
		hidden = []string{real(m.hide)}
	}
	var protect, pins []string
	for _, p := range m.protect {
		if exists(p) {
			protect = append(protect, real(p))
		}
	}
	for _, p := range m.pins {
		if exists(p) {
			pins = append(pins, real(p))
		}
	}
	rehide := "" // H1: pisljiva mapa vsebuje Scratch (tudi v domači mapi, ki jo sicer skrije --tmpfs home)
	if m.hide != "" && exists(m.hide) {
		for _, d := range rw {
			if inside(real(m.hide), d) {
				rehide = real(m.hide)
			}
		}
	}
	a := bwrapArgs(home, real(m.cfgDir), hidden, ro, rw, rehide, pins, protect, hideFiles, hideDirs)
	if m.noNet {
		a = append([]string{"--unshare-net"}, a...)
	}
	return bw, append(append(a, "--", exe), args...), func() {}, nil
}

// listenerIsOurs (F2): vrata poslušajo le procesi tega uporabnika (uid iz /proc/net/tcp{,6}); nič najdenega = ne.
func listenerIsOurs(port int) bool {
	var uids []int
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		if b, err := os.ReadFile(f); err == nil {
			uids = append(uids, procListenUIDs(b, port)...)
		}
	}
	for _, u := range uids {
		if u != os.Getuid() {
			return false
		}
	}
	return len(uids) > 0
}

// sameUserPeer: vtičnica odjemalca je tega uporabnika (uid iz /proc/net/tcp{,6}) in odjemalec ni agent prehoda.
func sameUserPeer(ours, peer *net.TCPAddr) bool {
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		if b, err := os.ReadFile(f); err == nil {
			if inode, uid := procSocketInode(b, peer, ours); inode != "" {
				if uid != os.Getuid() || peerCheck(ours, peer) != nil {
					return false
				}
				for _, p := range socketPIDs("socket:[" + inode + "]") {
					if p != os.Getpid() && confinedApp(p) {
						return false
					}
				}
				return true
			}
		}
	}
	return false
}

// confinedApp (L1): Flatpak (/.flatpak-info v korenu procesa) ali snap (cgroup snap.*), kot App Sandbox na Macu.
// Neberljiv koren procesa šteje kot omejen (fail closed).
func confinedApp(pid int) bool {
	root := "/proc/" + strconv.Itoa(pid) + "/root/"
	if _, err := os.Stat(root); err != nil {
		return true
	}
	if _, err := os.Stat(root + ".flatpak-info"); err == nil {
		return true
	}
	return strings.Contains(procCgroup(pid), "/snap.")
}

// listenerExes (L3): programi procesov, ki poslušajo na vratih (inode iz /proc/net/tcp → /proc/<pid>/exe).
func listenerExes(port int) ([]string, bool) {
	var exes []string
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, ino := range procListenInodes(b, port) {
			for _, pid := range socketPIDs("socket:[" + ino + "]") {
				if e, err := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid)); err == nil {
					exes = append(exes, e)
				}
			}
		}
	}
	return exes, true
}

func peerCheck(ours, peer *net.TCPAddr) error {
	inode := ""
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		if b, err := os.ReadFile(f); err == nil {
			if inode, _ = procSocketInode(b, peer, ours); inode != "" {
				break
			}
		}
	}
	if inode == "" {
		return errors.New("cannot find the client's connection in /proc/net/tcp")
	}
	pids := socketPIDs("socket:[" + inode + "]")
	if len(pids) == 0 {
		return errors.New("cannot identify the client process")
	}
	self := os.Getpid()
	mine := procCgroup(self)
	unit := strings.HasSuffix(strings.TrimSpace(mine), "/"+systemdUnit) // router v svoji enoti systemd
	for _, p := range pids {
		if p == self {
			continue // ta proces (testi; pladenj kliče API neposredno)
		}
		// Tudi osirotel potomec (dvojni fork) ostane v cgroup enote routerja.
		if unit && procCgroup(p) == mine {
			return errAgentPeer
		}
		for q, n := p, 0; q > 1; n++ {
			if q = procPPID(q); q < 0 || n > 4096 {
				return errors.New("cannot read the client's parent processes")
			}
			if q == self {
				return errAgentPeer
			}
		}
	}
	return nil
}

// peerInTree (M2): vsi procesi z odjemalčevo vtičnico so root ali njegovi potomci (bwrap: v imenskem prostoru gostitelja).
func peerInTree(ours, peer *net.TCPAddr, root int) bool {
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		inode, _ := procSocketInode(b, peer, ours)
		if inode == "" {
			continue
		}
		pids := socketPIDs("socket:[" + inode + "]")
		for _, p := range pids {
			q, n := p, 0
			for q != root && q > 1 && n < 4096 {
				q, n = procPPID(q), n+1
			}
			if q != root {
				return false
			}
		}
		return len(pids) > 0
	}
	return false
}

// socketPIDs: procesi z odprto vtičnico (opisnik »socket:[inode]«); berljivi so le lastni procesi.
func socketPIDs(target string) []int {
	var out []int
	ents, _ := os.ReadDir("/proc")
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		fds, _ := os.ReadDir("/proc/" + e.Name() + "/fd")
		for _, fd := range fds {
			if l, _ := os.Readlink("/proc/" + e.Name() + "/fd/" + fd.Name()); l == target {
				out = append(out, pid)
				break
			}
		}
	}
	return out
}

// procPPID: starš iz /proc/<pid>/stat (polja za zadnjim »)«: stanje, ppid); -1 = ni berljivo.
func procPPID(pid int) int {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return -1
	}
	s := string(b)
	f := strings.Fields(s[strings.LastIndexByte(s, ')')+1:])
	if len(f) < 2 {
		return -1
	}
	p, err := strconv.Atoi(f[1])
	if err != nil {
		return -1
	}
	return p
}

func procCgroup(pid int) string {
	b, _ := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cgroup")
	return string(b)
}
