package main

import (
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
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
	if in := procSocketInode([]byte(tcp), peer, ours); in != "4242" {
		t.Fatalf("tcp inode %q", in)
	}
	tcp6 := "   0: 0000000000000000FFFF00000100007F:C350 0000000000000000FFFF00000100007F:223D 01 0 0 0 1000 0 777 1\n"
	if in := procSocketInode([]byte(tcp6), peer, ours); in != "777" {
		t.Fatalf("tcp6 (v4-mapped) inode %q", in)
	}
	if procSocketInode([]byte(tcp), ours, peer) != "" {
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

// H1: proces, ki ga je pognal router (agent), ne pride skozi skrbniški API, tudi s pravo skrivnostjo.
func TestAdminRefusesAgentProcess(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "windows" {
		t.Skip("peer check: Linux and Windows (the Mac app has PeerProcess)")
	}
	s, _, base := liveServer(t, nil)
	child := func(start func(*exec.Cmd) error) string {
		cmd := exec.Command(os.Args[0], "-test.run=^TestPeerHelper$")
		cmd.Env = append(os.Environ(), "MINIGW_PEER_HELPER="+base+"|"+s.admin+"|"+hex.EncodeToString(s.key.PublicKey().Bytes()))
		var out strings.Builder
		cmd.Stdout = &out
		if err := start(cmd); err != nil {
			t.Fatal(err)
		}
		cmd.Wait()
		return out.String()
	}
	if out := child(startAgent); !strings.Contains(out, "status 403") || !strings.Contains(out, "agent process") {
		t.Fatalf("agent got through: %q", out)
	}
	if runtime.GOOS == "windows" { // Windows: zunaj Joba agentov je odjemalec dovoljen
		if out := child(func(c *exec.Cmd) error { return c.Start() }); !strings.Contains(out, "status 200") {
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
	key, _ := hex.DecodeString(p[2])
	a := &adminClient{base: p[0], secret: p[1], routerKey: key}
	st, b, err := a.http("GET", adminPrefix+"/status", nil, 5*time.Second)
	fmt.Printf("status %d %s %v\n", st, b, err)
}

// H1.5: na Linuxu z bwrap Codex in Grok ne vidita mape nastavitev.
func TestAgentWrapHidesConfig(t *testing.T) {
	cfg := t.TempDir()
	os.WriteFile(cfg+"/serve.json", []byte("SECRET"), 0o600)
	name, args := agentWrap("/bin/sh", []string{"-c", "cat " + cfg + "/serve.json; echo end"}, cfg)
	if name == "/bin/sh" {
		t.Skip("no usable bwrap (Linux with user namespaces)")
	}
	out, _ := exec.Command(name, args...).CombinedOutput()
	if strings.Contains(string(out), "SECRET") || !strings.Contains(string(out), "end") {
		t.Fatalf("config visible under bwrap: %q", out)
	}
}
