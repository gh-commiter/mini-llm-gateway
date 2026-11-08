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

func createAgentJob() {
	h, _, err := pCreateJob.Call(0, 0)
	if h == 0 {
		agentJobErr = fmt.Errorf("CreateJobObject: %v", err)
		return
	}
	info := jobExtendedLimits{Basic: jobBasicLimits{LimitFlags: 0x2000}} // JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if r, _, err := pSetJobInfo.Call(h, 9 /* JobObjectExtendedLimitInformation */, uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info)); r == 0 {
		syscall.CloseHandle(syscall.Handle(h))
		agentJobErr = fmt.Errorf("SetInformationJobObject: %v", err)
		return
	}
	agentJob = syscall.Handle(h)
}

// startAgent: proces ustvarjen ustavljen, dodan v Job in šele nato pognan (pred tem ne more ustvariti otrok).
func agentWrap(exe string, args []string, m agentMounts) (string, []string) { return exe, args }

// agentSandboxed: sandbox Codex in Grok na Windows ni preverjen: le z izrecnim dovoljenjem.
func agentSandboxed(cfgDir string) bool { return false }

func hardenRouter() {}

func agentReaperMain([]string) { os.Exit(2) }

func startAgent(cmd *exec.Cmd) error {
	agentJobOnce.Do(createAgentJob)
	if agentJobErr != nil {
		return agentJobErr // brez Joba ga peer check ne bi prepoznal: agent ne teče
	}
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= 0x4 // CREATE_SUSPENDED
	if err := cmd.Start(); err != nil {
		return err
	}
	const access = 0x0100 | 0x0001 | 0x0800 // PROCESS_SET_QUOTA | PROCESS_TERMINATE | PROCESS_SUSPEND_RESUME
	h, err := syscall.OpenProcess(access, false, uint32(cmd.Process.Pid))
	if err == nil {
		defer syscall.CloseHandle(h)
		if r, _, e := pAssignJob.Call(uintptr(agentJob), uintptr(h)); r == 0 {
			err = fmt.Errorf("AssignProcessToJobObject: %v", e)
		} else if st, _, _ := pNtResume.Call(uintptr(h)); st != 0 {
			err = fmt.Errorf("NtResumeProcess: 0x%x", st)
		}
	}
	if err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		return err
	}
	return nil
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
	return err1 == nil && err2 == nil && sid == self && peerCheck(ours, peer) == nil
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
