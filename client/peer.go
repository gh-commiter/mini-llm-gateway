package main

// Skrbniški API in procesi agentov (varnostni pregled H1, kot PeerProcess na Macu): agent (claude, codex, grok) z
// omrežjem bi lahko pognal »minigw router …« in prevzel router. Zahtevo MGW-Admin zavrnemo, če jo pošlje proces, ki
// izhaja iz tega routerja: Linux prek /proc (vtičnica → proces → veriga staršev, in cgroup enote systemd), Windows
// prek tabele TCP (PID) in objekta Job, v katerem tečejo vsi agenti. Kar ni jasno, zavrnemo (fail closed).

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// agentReaperCmd: skriti ukaz minigw za pomočnika, pod katerim teče agent (Linux, startAgent).
const agentReaperCmd = "__minigw-agent"

// agentMounts: kaj Codex ali Grok pod bwrap še vidita (Linux): mapa nastavitev je vedno skrita.
type agentMounts struct {
	cfgDir string
	state  string   // stanje CLI te naprave (<scratch>/state/<naprava>/codex): branje in pisanje
	envVar string   // CODEX_HOME ali GROK_HOME (kaže na state)
	files  []string // prave datoteke prijave (~/.codex/auth.json …), na katere kaže stanje: branje in pisanje
	rw     []string // kopija mape, začasna mapa zahteve, dovoljene mape
	ro     []string // dodatno le za branje (Copilot: razširjen paket v ~/.cache/copilot)
	hide   string   // mapa, skrita tudi zunaj domače mape (Scratch: kopije drugih naprav, stanje agentov; I3)
	env    []string // dodatne spremenljivke (Copilot: žeton prijave pod bwrap)
}

// packageRoot: mapa, ki jo CLI pod domačo mapo potrebuje: različica nvm (~/.nvm/versions/node/<v>), sicer najbližja
// mapa s package.json nad programom (paket npm), sicer mapa programa.
func packageRoot(home, exe string) string {
	if rel, err := filepath.Rel(home, exe); err == nil {
		if p := strings.Split(filepath.ToSlash(rel), "/"); len(p) > 4 && p[0] == ".nvm" && p[1] == "versions" && p[2] == "node" {
			return filepath.Join(home, ".nvm", "versions", "node", p[3])
		}
	}
	for d := filepath.Dir(exe); d != home && strings.HasPrefix(d, home+string(filepath.Separator)); d = filepath.Dir(d) {
		if _, err := os.Stat(filepath.Join(d, "package.json")); err == nil {
			return d
		}
	}
	return filepath.Dir(exe)
}

// bwrapArgs: vse kot je (/), nato prazna domača mapa in mapa nastavitev, potrebne mape nazaj, skrite vtičnice.
// hidden: prazne mape takoj za domačo (Scratch zunaj nje); mape agenta v njih se nato vežejo nazaj.
func bwrapArgs(home, cfgDir string, hidden, ro, rw, hideFiles, hideDirs []string) []string {
	// --unshare-pid in svoj /proc: ukazi agenta ne vidijo drugih procesov (okolje copilota z žetonom v /proc/<pid>/environ).
	a := []string{"--dev-bind", "/", "/", "--die-with-parent", "--unshare-pid", "--proc", "/proc", "--tmpfs", home}
	for _, d := range hidden {
		a = append(a, "--tmpfs", d)
	}
	for _, d := range ro {
		a = append(a, "--ro-bind", d, d)
	}
	for _, d := range rw { // mape ali posamezne datoteke (prijava CLI)
		a = append(a, "--bind", d, d)
	}
	a = append(a, "--tmpfs", cfgDir)
	for _, f := range hideFiles {
		a = append(a, "--ro-bind", "/dev/null", f)
	}
	for _, d := range hideDirs {
		a = append(a, "--tmpfs", d)
	}
	return a
}

var errAgentPeer = errors.New("this request comes from an agent process of the gateway: the admin API refuses it")

// adminPeerCheck: nil = odjemalec ni agent tega routerja.
func adminPeerCheck(c net.Conn) error {
	ours, ok1 := c.LocalAddr().(*net.TCPAddr)
	peer, ok2 := c.RemoteAddr().(*net.TCPAddr)
	if !ok1 || !ok2 {
		return errors.New("not a TCP connection")
	}
	return peerCheck(ours, peer)
}

// procAddr: naslov iz /proc/net/tcp{,6} (»0100007F:1F90«; IP po 32-bitnih besedah v vrstnem redu gostitelja).
func procAddr(s string) (net.IP, int, bool) {
	h, p, ok := strings.Cut(s, ":")
	b, err := hex.DecodeString(h)
	port, err2 := strconv.ParseUint(p, 16, 16)
	if !ok || err != nil || err2 != nil || (len(b) != 4 && len(b) != 16) {
		return nil, 0, false
	}
	for i := 0; i < len(b); i += 4 {
		b[i], b[i+1], b[i+2], b[i+3] = b[i+3], b[i+2], b[i+1], b[i]
	}
	return net.IP(b), int(port), true
}

// procSocketInode: inode vtičnice odjemalca (njen lokalni naslov je naš oddaljeni in obratno); "" = ni najdena.
func procSocketInode(table []byte, peer, ours *net.TCPAddr) string {
	for _, line := range strings.Split(string(table), "\n") {
		f := strings.Fields(line)
		if len(f) < 10 {
			continue
		}
		lip, lport, ok1 := procAddr(f[1])
		rip, rport, ok2 := procAddr(f[2])
		if ok1 && ok2 && lport == peer.Port && rport == ours.Port && lip.Equal(peer.IP) && rip.Equal(ours.IP) {
			return f[9]
		}
	}
	return ""
}

// tcpTablePID: PID iz tabele GetExtendedTcpTable (TCP_TABLE_OWNER_PID_ALL) za povezavo odjemalca; 0 = ni najdena.
// IPv4: vrstice po 24 bajtov (stanje, lokalni naslov, vrata, oddaljeni naslov, vrata, PID); IPv6: 56 bajtov.
func tcpTablePID(table []byte, v6 bool, peer, ours *net.TCPAddr) uint32 {
	if len(table) < 4 {
		return 0
	}
	n := int(binary.LittleEndian.Uint32(table))
	size := 24
	if v6 {
		size = 56
	}
	for i := 0; i < n && 4+(i+1)*size <= len(table); i++ {
		r := table[4+i*size : 4+(i+1)*size]
		var lip, rip net.IP
		var lp, rp, pid int
		if v6 {
			lip, rip = net.IP(r[0:16]), net.IP(r[24:40])
			lp, rp, pid = int(r[20])<<8|int(r[21]), int(r[44])<<8|int(r[45]), int(binary.LittleEndian.Uint32(r[52:]))
		} else {
			lip, rip = net.IP(r[4:8]), net.IP(r[12:16])
			lp, rp, pid = int(r[8])<<8|int(r[9]), int(r[16])<<8|int(r[17]), int(binary.LittleEndian.Uint32(r[20:]))
		}
		if lp == peer.Port && rp == ours.Port && lip.Equal(peer.IP) && rip.Equal(ours.IP) {
			return uint32(pid)
		}
	}
	return 0
}

// procListenUIDs (F2): uid procesov, ki poslušajo na vratih (/proc/net/tcp{,6}: stanje 0A = LISTEN, polje 7 = uid).
func procListenUIDs(table []byte, port int) []int {
	var out []int
	for _, line := range strings.Split(string(table), "\n") {
		f := strings.Fields(line)
		if len(f) < 8 || f[3] != "0A" {
			continue
		}
		if _, p, ok := procAddr(f[1]); ok && p == port {
			if uid, err := strconv.Atoi(f[7]); err == nil {
				out = append(out, uid)
			}
		}
	}
	return out
}

// tcpListenPIDs (F2): PID-ji iz tabele GetExtendedTcpTable, ki poslušajo na vratih (stanje 2 = LISTEN).
func tcpListenPIDs(table []byte, v6 bool, port int) []uint32 {
	if len(table) < 4 {
		return nil
	}
	n := int(binary.LittleEndian.Uint32(table))
	size := 24
	if v6 {
		size = 56
	}
	var out []uint32
	for i := 0; i < n && 4+(i+1)*size <= len(table); i++ {
		r := table[4+i*size : 4+(i+1)*size]
		var state uint32
		var lp int
		var pid uint32
		if v6 {
			lp, state, pid = int(r[20])<<8|int(r[21]), binary.LittleEndian.Uint32(r[48:]), binary.LittleEndian.Uint32(r[52:])
		} else {
			state, lp, pid = binary.LittleEndian.Uint32(r[0:]), int(r[8])<<8|int(r[9]), binary.LittleEndian.Uint32(r[20:])
		}
		if state == 2 && lp == port {
			out = append(out, pid)
		}
	}
	return out
}
