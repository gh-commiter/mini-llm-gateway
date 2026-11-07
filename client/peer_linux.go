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
	"sync/atomic"
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

// agentReaperMain: pomočnik (glej startAgent): args = program in argumenti agenta.
func agentReaperMain(args []string) {
	if len(args) == 0 {
		os.Exit(2)
	}
	syscall.RawSyscall(syscall.SYS_PRCTL, 36 /* PR_SET_CHILD_SUBREAPER */, 1, 0)
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGINT)
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
	var stop atomic.Bool
	go func() {
		<-sigs
		stop.Store(true)
		syscall.Kill(-pid, syscall.SIGKILL)
		killChildren()
	}()
	code := 0
	for {
		var ws syscall.WaitStatus
		p, err := syscall.Wait4(-1, &ws, 0, nil)
		if err == syscall.EINTR {
			continue
		}
		if err != nil { // ECHILD: nobenega potomca več
			break
		}
		if p == pid {
			stop.Store(true)
			if code = ws.ExitStatus(); ws.Signaled() {
				code = 128 + int(ws.Signal())
			}
			syscall.Kill(-pid, syscall.SIGKILL)
		}
		if stop.Load() {
			killChildren() // osiroteli so zdaj otroci pomočnika (prevesijo se ob izhodu starša, pred žetvijo)
		}
	}
	os.Exit(code)
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
	bwrapOnce sync.Once
	bwrapPath string // "" = bwrap ni nameščen ali ne deluje (npr. brez uporabniških imenskih prostorov)
)

func bwrapReady(cfgDir string) string {
	bwrapOnce.Do(func() {
		p, err := exec.LookPath("bwrap")
		t, err2 := exec.LookPath("true")
		home, _ := os.UserHomeDir()
		if err == nil && err2 == nil && home != "" &&
			exec.Command(p, append(bwrapArgs(home, cfgDir, nil, nil, nil, nil), "--", t)...).Run() == nil {
			bwrapPath = p
		}
	})
	return bwrapPath
}

// agentSandboxed: Codex in Grok sta zaprta pred skrivnostmi routerja le pod bwrap; sicer le z izrecnim dovoljenjem.
func agentSandboxed(cfgDir string) bool { return bwrapReady(cfgDir) != "" }

// agentWrap: Codex ali Grok pod bwrap (R3): domača mapa in mapa nastavitev sta prazna, nazaj pridejo le mape s
// programi (samo za branje), mapa CLI (~/.codex, ~/.grok: prijava, seje), kopija mape in dovoljene mape. Skrita sta
// tudi D-Bus seje (Secret Service s ključi prehodov) in /tmp/dbus-*. Poti brez simbolnih povezav (R5).
func agentWrap(exe string, args []string, m agentMounts) (string, []string) {
	bw := bwrapReady(m.cfgDir)
	if bw == "" {
		return exe, args
	}
	real := func(p string) string {
		if r, err := filepath.EvalSymlinks(p); err == nil {
			return r
		}
		return p
	}
	home, _ := os.UserHomeDir()
	home = real(home)
	var ro []string
	top := func(p string) { // mapa s programom pod domačo: njena mapa prve ravni (npr. ~/.nvm, ~/.local)
		if rel, err := filepath.Rel(home, real(p)); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
			if d := filepath.Join(home, strings.Split(rel, string(filepath.Separator))[0]); !slices.Contains(ro, d) {
				ro = append(ro, d)
			}
		}
	}
	top(exe)
	for _, d := range filepath.SplitList(os.Getenv("PATH")) {
		if _, err := os.Stat(d); err == nil {
			top(d)
		}
	}
	var rw []string
	if m.state != "" {
		os.MkdirAll(m.state, 0o700)
		rw = append(rw, real(m.state))
	}
	for _, d := range m.rw {
		if _, err := os.Stat(d); err == nil {
			rw = append(rw, real(d))
		}
	}
	ro = slices.DeleteFunc(ro, func(d string) bool { return slices.Contains(rw, d) })
	var hideFiles, hideDirs []string
	if rt := os.Getenv("XDG_RUNTIME_DIR"); rt != "" {
		if _, err := os.Stat(rt + "/bus"); err == nil {
			hideFiles = append(hideFiles, real(rt+"/bus"))
		}
	}
	bus, _ := filepath.Glob("/tmp/dbus-*")
	for _, b := range bus {
		if fi, err := os.Lstat(b); err == nil && fi.IsDir() {
			hideDirs = append(hideDirs, b)
		} else if err == nil && fi.Mode()&os.ModeSymlink == 0 {
			hideFiles = append(hideFiles, b)
		}
	}
	return bw, append(append(bwrapArgs(home, real(m.cfgDir), ro, rw, hideFiles, hideDirs), "--", exe), args...)
}

func peerCheck(ours, peer *net.TCPAddr) error {
	inode := ""
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		if b, err := os.ReadFile(f); err == nil {
			if inode = procSocketInode(b, peer, ours); inode != "" {
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
