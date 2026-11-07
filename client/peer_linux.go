package main

import (
	"errors"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// startAgent: na Linuxu brez posebnosti (procesi agentov so potomci routerja; peerCheck hodi po verigi staršev).
func startAgent(cmd *exec.Cmd) error { return cmd.Start() }

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
