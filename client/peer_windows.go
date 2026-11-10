package main

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"
)

// Windows: vsi agenti (claude, codex, grok) tečejo v enem objektu Job routerja. Članstvo podedujejo vsi potomci,
// tudi osiroteli (izstop iz Joba ni dovoljen), zato IsProcessInJob za PID odjemalca zanesljivo pove, ali je agent.
// Job tudi ustavi agente, ko se router konča (KILL_ON_JOB_CLOSE).
var (
	peerK32         = syscall.NewLazyDLL("kernel32.dll")
	pCreateJob      = peerK32.NewProc("CreateJobObjectW")
	pSetJobInfo     = peerK32.NewProc("SetInformationJobObject")
	pAssignJob      = peerK32.NewProc("AssignProcessToJobObject")
	pIsProcessInJob = peerK32.NewProc("IsProcessInJob")
	pTerminateJob   = peerK32.NewProc("TerminateJobObject")
	pNtResume       = syscall.NewLazyDLL("ntdll.dll").NewProc("NtResumeProcess")
	pGetTCPTable    = syscall.NewLazyDLL("iphlpapi.dll").NewProc("GetExtendedTcpTable")

	agentJob     syscall.Handle
	agentJobErr  error
	agentJobOnce sync.Once
)

type jobBasicLimits struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type jobExtendedLimits struct {
	Basic                 jobBasicLimits
	IoCounters            [6]uint64
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

func createAgentJob() { agentJob, agentJobErr = newKillJob() }

// newKillJob: Job z KILL_ON_JOB_CLOSE (zapiranje ročaja ali konec minigw ubije vse njegove procese).
func newKillJob() (syscall.Handle, error) {
	h, _, err := pCreateJob.Call(0, 0)
	if h == 0 {
		return 0, fmt.Errorf("CreateJobObject: %v", err)
	}
	info := jobExtendedLimits{Basic: jobBasicLimits{LimitFlags: 0x2000}} // JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if r, _, err := pSetJobInfo.Call(h, 9 /* JobObjectExtendedLimitInformation */, uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info)); r == 0 {
		syscall.CloseHandle(syscall.Handle(h))
		return 0, fmt.Errorf("SetInformationJobObject: %v", err)
	}
	return syscall.Handle(h), nil
}

// startAgent: proces ustvarjen ustavljen, dodan v Job in šele nato pognan (pred tem ne more ustvariti otrok).
// agentWrap (#68): s škatlo MXC ves CLI v njej (mxcWrap); wxc-exec.exe se pred vsako rabo preveri (zgoščena vrednost).
// Napaka: agent ne teče (nikoli brez škatle, ko je vklopljena). Codex (unboxed) in brez škatle: kot prej.
func agentWrap(exe string, args []string, m agentMounts) (string, []string, func(), error) {
	if m.unboxed || !windowsBox() {
		return exe, args, func() {}, nil
	}
	return boxWrap(exe, args, m)
}

func pinWrap(cfgDir, exe string, args, pins []string, logf func(string, ...any)) (string, []string) {
	return exe, args
}

// claudePinOK: pin (bwrap) je le na Linux.
func claudePinOK(cfgDir string, logf func(string, ...any)) bool { return false }

// agentSandboxed (#68): agenti so v škatli MXC (preizkus ob zagonu uspel); sicer le z izrecnim dovoljenjem.
func agentSandboxed(cfgDir string) bool { return windowsBox() }

func hardenRouter() {}

func agentReaperMain([]string) { os.Exit(2) }

// initReaper: le Linux (PID 1 v vsebniku).
func initReaper() {}

// N1: vsak tek ima še svoj Job (gnezden v Jobu agentov): ko neposredni proces konča (tudi ubit ob ctx Done), Job teka
// ubije še vse potomce (kot pomočnik na Linuxu), zato noben vnuk ne drži stdout in branje doseže EOF.
func startAgent(cmd *exec.Cmd) error {
	agentJobOnce.Do(createAgentJob)
	if agentJobErr != nil {
		return agentJobErr // brez Joba ga peer check ne bi prepoznal: agent ne teče
	}
	run, err := newKillJob()
	if err != nil {
		return err
	}
	h, err := startInJobs(cmd, agentJob, run)
	if err != nil {
		syscall.CloseHandle(run)
		return err
	}
	go func() {
		syscall.WaitForSingleObject(h, syscall.INFINITE)
		pTerminateJob.Call(uintptr(run), 1)
		syscall.CloseHandle(run)
		syscall.CloseHandle(h)
	}()
	return nil
}

// startClient: claude odjemalca (runClaude) v Jobu, ki ga zapre šele konec minigw: z minigw (tudi ubitim) umre vse drevo.
// Brez Joba (napaka) claude vseeno teče, z opozorilom.
func startClient(cmd *exec.Cmd) error {
	job, err := newKillJob()
	if err != nil {
		fmt.Fprintln(os.Stderr, "minigw: warning: no Job for claude ("+err.Error()+"); its processes may outlive minigw")
		return cmd.Start()
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= 0x4 // CREATE_SUSPENDED
	if err := cmd.Start(); err != nil {
		syscall.CloseHandle(job)
		return err
	}
	const access = 0x0100 | 0x0001 | 0x0800 // PROCESS_SET_QUOTA | PROCESS_TERMINATE | PROCESS_SUSPEND_RESUME
	h, err := syscall.OpenProcess(access, false, uint32(cmd.Process.Pid))
	if err != nil {
		cmd.Process.Kill() // brez ročaja ga ni mogoče pognati
		cmd.Wait()
		syscall.CloseHandle(job)
		return err
	}
	defer syscall.CloseHandle(h)
	if r, _, e := pAssignJob.Call(uintptr(job), uintptr(h)); r == 0 { // ročaj Joba sicer namenoma ostane odprt do konca procesa
		syscall.CloseHandle(job)
		fmt.Fprintf(os.Stderr, "minigw: warning: no Job for claude (AssignProcessToJobObject: %v); its processes may outlive minigw\n", e)
	}
	if st, _, _ := pNtResume.Call(uintptr(h)); st != 0 {
		cmd.Process.Kill()
		cmd.Wait()
		return fmt.Errorf("NtResumeProcess: 0x%x", st)
	}
	return nil
}

// startInJobs: proces ustvarjen ustavljen, dodan v Jobe (po vrsti: drugi je gnezden v prvem) in šele nato pognan (pred
// tem ne more ustvariti otrok). Vrne ročaj procesa (SYNCHRONIZE), ki ga zapre klicatelj.
func startInJobs(cmd *exec.Cmd, jobs ...syscall.Handle) (syscall.Handle, error) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= 0x4 // CREATE_SUSPENDED
	if err := cmd.Start(); err != nil {
		return 0, err
	}
	const access = 0x0100 | 0x0001 | 0x0800 | 0x00100000 // PROCESS_SET_QUOTA | PROCESS_TERMINATE | PROCESS_SUSPEND_RESUME | SYNCHRONIZE
	h, err := syscall.OpenProcess(access, false, uint32(cmd.Process.Pid))
	if err == nil {
		for _, j := range jobs {
			if r, _, e := pAssignJob.Call(uintptr(j), uintptr(h)); r == 0 {
				err = fmt.Errorf("AssignProcessToJobObject: %v", e)
				break
			}
		}
		if err == nil {
			if st, _, _ := pNtResume.Call(uintptr(h)); st != 0 {
				err = fmt.Errorf("NtResumeProcess: 0x%x", st)
			}
		}
		if err != nil {
			syscall.CloseHandle(h)
		}
	}
	if err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return 0, err
	}
	return h, nil
}

func peerCheck(ours, peer *net.TCPAddr) error {
	pid, err := tcpOwnerPID(ours, peer)
	if err != nil {
		return err
	}
	if pid == uint32(os.Getpid()) {
		return nil // ta proces (testi)
	}
	agentJobOnce.Do(createAgentJob)
	if agentJobErr != nil {
		return agentJobErr
	}
	h, err := syscall.OpenProcess(0x1000 /* PROCESS_QUERY_LIMITED_INFORMATION */, false, pid)
	if err != nil {
		return fmt.Errorf("cannot open the client process %d: %v", pid, err)
	}
	defer syscall.CloseHandle(h)
	var in int32
	if r, _, e := pIsProcessInJob.Call(uintptr(h), uintptr(agentJob), uintptr(unsafe.Pointer(&in))); r == 0 {
		return fmt.Errorf("IsProcessInJob: %v", e)
	}
	if in != 0 {
		return errAgentPeer
	}
	return nil
}

// sameUserPeer: proces odjemalca ima isti uporabniški SID kot prehod in ni agent prehoda.
func sameUserPeer(ours, peer *net.TCPAddr) bool {
	pid, err := tcpOwnerPID(ours, peer)
	if err != nil {
		return false
	}
	self, err1 := processSID(uint32(os.Getpid()))
	sid, err2 := processSID(pid)
	return err1 == nil && err2 == nil && sid == self && !confinedApp(int(pid)) && peerCheck(ours, peer) == nil
}

// confinedApp (L1): proces v AppContainer (TokenIsAppContainer), kot App Sandbox na Macu; napaka šteje kot da (fail closed).
func confinedApp(pid int) bool {
	h, err := syscall.OpenProcess(0x1000 /* PROCESS_QUERY_LIMITED_INFORMATION */, false, uint32(pid))
	if err != nil {
		return true
	}
	defer syscall.CloseHandle(h)
	var tok syscall.Token
	if syscall.OpenProcessToken(h, syscall.TOKEN_QUERY, &tok) != nil {
		return true
	}
	defer tok.Close()
	var v, n uint32
	if syscall.GetTokenInformation(tok, 29 /* TokenIsAppContainer */, (*byte)(unsafe.Pointer(&v)), 4, &n) != nil {
		return true
	}
	return v != 0
}

// peerInTree (M2): proces odjemalca je root ali njegov potomec (starši iz posnetka procesov).
func peerInTree(ours, peer *net.TCPAddr, root int) bool {
	pid, err := tcpOwnerPID(ours, peer)
	if err != nil {
		return false
	}
	snap, err := syscall.CreateToolhelp32Snapshot(syscall.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(snap)
	parent := map[uint32]uint32{}
	e := syscall.ProcessEntry32{Size: uint32(unsafe.Sizeof(syscall.ProcessEntry32{}))}
	for err = syscall.Process32First(snap, &e); err == nil; err = syscall.Process32Next(snap, &e) {
		parent[e.ProcessID] = e.ParentProcessID
	}
	for q, n := pid, 0; n < 4096; q, n = parent[q], n+1 {
		if q == uint32(root) {
			return true
		}
		// L4: PID starša je lahko ponovno uporabljen (sirota mrtvega starša): starš mora biti ustvarjen pred otrokom.
		p := parent[q]
		if q == 0 || p == q || !createdBefore(p, q) {
			return false
		}
	}
	return false
}

// createdBefore: proces a je bil ustvarjen pred procesom b (GetProcessTimes); ne da se preveriti = ne.
func createdBefore(a, b uint32) bool {
	created := func(pid uint32) (int64, bool) {
		h, err := syscall.OpenProcess(0x1000 /* PROCESS_QUERY_LIMITED_INFORMATION */, false, pid)
		if err != nil {
			return 0, false
		}
		defer syscall.CloseHandle(h)
		var c, e, k, u syscall.Filetime
		if syscall.GetProcessTimes(h, &c, &e, &k, &u) != nil {
			return 0, false
		}
		return c.Nanoseconds(), true
	}
	ta, ok1 := created(a)
	tb, ok2 := created(b)
	return ok1 && ok2 && ta < tb
}

// tcpOwnerPID: PID procesa z odjemalčevo stranjo povezave (GetExtendedTcpTable, TCP_TABLE_OWNER_PID_ALL).
func tcpOwnerPID(ours, peer *net.TCPAddr) (uint32, error) {
	for _, v6 := range []bool{false, true} {
		af := uintptr(2) // AF_INET
		if v6 {
			af = 23 // AF_INET6
		}
		size := uint32(64 << 10)
		for range 5 {
			buf := make([]byte, size)
			r, _, _ := pGetTCPTable.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)), 0, af, 5, 0)
			if r == 122 { // ERROR_INSUFFICIENT_BUFFER: size je zdaj potrebna velikost
				size += 4 << 10
				continue
			}
			if r != 0 {
				return 0, fmt.Errorf("GetExtendedTcpTable: error %d", r)
			}
			if pid := tcpTablePID(buf[:size], v6, peer, ours); pid != 0 {
				return pid, nil
			}
			break
		}
	}
	return 0, errors.New("cannot find the client's connection in the TCP table")
}

// listenerIsOurs (F2): vsak proces, ki posluša na vratih, ima isti uporabniški SID kot ta proces.
func listenerIsOurs(port int) bool {
	self, err := processSID(uint32(os.Getpid()))
	if err != nil {
		return false
	}
	var pids []uint32
	for _, v6 := range []bool{false, true} {
		af := uintptr(2)
		if v6 {
			af = 23
		}
		size := uint32(64 << 10)
		for range 5 {
			buf := make([]byte, size)
			r, _, _ := pGetTCPTable.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)), 0, af, 5, 0)
			if r == 122 {
				size += 4 << 10
				continue
			}
			if r == 0 {
				pids = append(pids, tcpListenPIDs(buf, v6, port)...)
			}
			break
		}
	}
	for _, pid := range pids {
		if sid, err := processSID(pid); err != nil || sid != self {
			return false
		}
	}
	return len(pids) > 0
}

// listenerExes (L3): programi procesov, ki poslušajo na vratih (QueryFullProcessImageNameW).
func listenerExes(port int) ([]string, bool) {
	var exes []string
	q := syscall.NewLazyDLL("kernel32.dll").NewProc("QueryFullProcessImageNameW")
	for _, v6 := range []bool{false, true} {
		af := uintptr(2)
		if v6 {
			af = 23
		}
		size := uint32(64 << 10)
		for range 5 {
			buf := make([]byte, size)
			r, _, _ := pGetTCPTable.Call(uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)), 0, af, 5, 0)
			if r == 122 {
				size += 4 << 10
				continue
			}
			if r == 0 {
				for _, pid := range tcpListenPIDs(buf, v6, port) {
					h, err := syscall.OpenProcess(0x1000 /* PROCESS_QUERY_LIMITED_INFORMATION */, false, pid)
					if err != nil {
						continue
					}
					name := make([]uint16, 1024)
					n := uint32(len(name))
					if ok, _, _ := q.Call(uintptr(h), 0, uintptr(unsafe.Pointer(&name[0])), uintptr(unsafe.Pointer(&n))); ok != 0 {
						exes = append(exes, syscall.UTF16ToString(name[:n]))
					}
					syscall.CloseHandle(h)
				}
			}
			break
		}
	}
	return exes, true
}

// processSID: uporabniški SID procesa (žeton, TokenUser).
func processSID(pid uint32) (string, error) {
	h, err := syscall.OpenProcess(0x1000 /* PROCESS_QUERY_LIMITED_INFORMATION */, false, pid)
	if err != nil {
		return "", err
	}
	defer syscall.CloseHandle(h)
	var tok syscall.Token
	if err := syscall.OpenProcessToken(h, syscall.TOKEN_QUERY, &tok); err != nil {
		return "", err
	}
	defer tok.Close()
	u, err := tok.GetTokenUser()
	if err != nil {
		return "", err
	}
	return u.User.Sid.String()
}

// bwrapReady: bwrap je le na Linuxu.
func bwrapReady(string) string { return "" }

// watchGitMoves (N3c): le Linux (inotify); drugje runCLI preverja klon vsako sekundo (macOS serve je za razvoj, na
// Windows teče Claude v worktree le z »Allow commands without a sandbox«).
func watchGitMoves(dirs []string, moved func()) (stop func(), ok bool) { return nil, false }
