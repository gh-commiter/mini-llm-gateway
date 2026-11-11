package main

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// #82 (vmesno): GET in PROPFIND, ki dirkata z zamenjavo nadrejene mape pod izključnim zaklepom (kot MOVE, sync ali
// druga seja na isti mapi), nikoli ne bereta zunaj korena: kanarček v mapi zunaj se ne vrne nikoli.
func TestFolderLockReadNeverRacesParentSwap(t *testing.T) {
	root, out := t.TempDir(), t.TempDir()
	os.MkdirAll(filepath.Join(root, "a"), 0o755)
	os.WriteFile(filepath.Join(root, "a", "f.txt"), []byte("inside"), 0o644)
	os.WriteFile(filepath.Join(out, "f.txt"), []byte("CANARY"), 0o644)
	os.WriteFile(filepath.Join(out, "CANARY-name"), nil, 0o644)
	if err := os.Symlink(out, filepath.Join(root, "link")); err != nil {
		t.Skip("no symlinks here: " + err.Error())
	}
	s, other := &shareClient{root: root}, &shareClient{root: root} // other: druga seja (svoja datoteka zaklepa)
	p := func(n string) string { return filepath.Join(root, n) }
	var stop atomic.Bool
	var swaps atomic.Int32
	swapped := make(chan struct{})
	go func() { // a ↔ povezava ven; ob odklepu vedno vrnjeno
		defer close(swapped)
		for !stop.Load() {
			if unlock := other.lockFolder(true, folderLockWait); unlock != nil {
				os.Rename(p("a"), p("tmp"))
				os.Rename(p("link"), p("a"))
				time.Sleep(50 * time.Microsecond)
				os.Rename(p("a"), p("link"))
				os.Rename(p("tmp"), p("a"))
				unlock()
				swaps.Add(1)
			}
		}
	}()
	var leaks atomic.Int32
	var readers sync.WaitGroup
	for g := 0; g < 4; g++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for i := 0; i < 300; i++ {
				for _, j := range []davJob{{Method: "GET", Path: "/s/a/f.txt", Prefix: "/s"},
					{Method: "PROPFIND", Path: "/s/a", Prefix: "/s", Headers: map[string]string{"depth": "1"}}} {
					r := s.handle(j, "k")
					if strings.Contains(string(r.Body), "CANARY") {
						leaks.Add(1)
					} else if r.Status >= 300 {
						t.Errorf("%s: %d", j.Method, r.Status)
					}
				}
			}
		}()
	}
	readers.Wait()
	stop.Store(true)
	<-swapped
	if n := leaks.Load(); n > 0 {
		t.Fatalf("%d reads returned the canary outside the shared folder", n)
	} else if swaps.Load() == 0 {
		t.Fatal("the folder was never swapped during the reads")
	}
}

// Bralci se ne čakajo med sabo; pisanje čaka bralca.
func TestFolderLockSharedAndExclusive(t *testing.T) {
	root := t.TempDir()
	a, b, w := &shareClient{root: root}, &shareClient{root: root}, &shareClient{root: root}
	ua := a.lockFolder(false, folderLockWait)
	ub := b.lockFolder(false, folderLockWait)
	if ua == nil || ub == nil {
		t.Fatal("two readers must hold the lock together")
	}
	defer func(w time.Duration) { folderLockWait = w }(folderLockWait)
	folderLockWait = 100 * time.Millisecond
	if u := w.lockFolder(true, folderLockWait); u != nil {
		t.Fatal("a writer must wait for the readers")
	}
	ua()
	ub()
	if u := w.lockFolder(true, folderLockWait); u == nil {
		t.Fatal("the lock was not released")
	} else {
		u()
	}
}

// Dve seji v dveh procesih na isti mapi: dokler drug proces drži izključni zaklep, GET po folderLockWait vrne 503 z
// besedilom; ko proces konča (tudi ubit: zaklep sprosti jedro), GET spet dela.
func TestFolderLockAcrossProcesses(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "f.txt"), []byte("x"), 0o644)
	cmd := exec.Command(os.Args[0], "-test.run=^TestFolderLockHelper$")
	cmd.Env = append(os.Environ(), "MINIGW_TEST_LOCK_ROOT="+root, "MINIGW_TEST_LOCK_DIR="+folderLockDir)
	stdout, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	if line, _ := bufio.NewReader(stdout).ReadString('\n'); line != "locked\n" {
		t.Fatalf("helper: %q", line)
	}
	defer func(w time.Duration) { folderLockWait = w }(folderLockWait)
	folderLockWait = 300 * time.Millisecond
	s := &shareClient{root: root}
	get := davJob{Method: "GET", Path: "/s/f.txt", Prefix: "/s"}
	start := time.Now()
	r := s.handle(get, "k")
	if r.Status != 503 || !strings.Contains(string(r.Body), "busy") || time.Since(start) < folderLockWait {
		t.Fatalf("while another process writes: %d %q after %v", r.Status, r.Body, time.Since(start))
	}
	cmd.Process.Kill() // sesutje: jedro sprosti zaklep
	cmd.Wait()
	if r := s.handle(get, "k"); r.Status != 200 {
		t.Fatalf("after the other process ended: %d", r.Status)
	}
}

// Pomočnik TestFolderLockAcrossProcesses: druga seja, ki drži izključni zaklep mape.
func TestFolderLockHelper(t *testing.T) {
	root := os.Getenv("MINIGW_TEST_LOCK_ROOT")
	if root == "" {
		t.Skip("helper process only")
	}
	folderLockDir = os.Getenv("MINIGW_TEST_LOCK_DIR")
	if (&shareClient{root: root}).lockFolder(true, folderLockWait) == nil {
		os.Exit(1)
	}
	os.Stdout.WriteString("locked\n")
	time.Sleep(time.Minute)
}

// L3: tudi zaklep v procesu le do roka (posel istega odjemalca, ki drži izključni zaklep).
func TestFolderLockInProcessTimeout(t *testing.T) {
	s := &shareClient{root: t.TempDir()}
	u := s.lockFolder(true, time.Second)
	if u == nil {
		t.Fatal("no lock")
	}
	defer u()
	defer func(w time.Duration) { folderLockWait = w }(folderLockWait)
	folderLockWait = 100 * time.Millisecond
	start := time.Now()
	if s.lockFolder(false, 100*time.Millisecond) != nil || time.Since(start) < 100*time.Millisecond {
		t.Fatal("a reader must give up after the wait while a writer of this process holds the lock")
	}
	if r := s.handle(davJob{Method: "GET", Path: "/s/x", Prefix: "/s"}, "k"); r.Status != 503 {
		t.Fatalf("GET while held: %d", r.Status)
	}
	// #82 cilj 1: sync in worktree čakajo do roka posla prehoda, ne 5 s (sicer delo agenta ne pride nazaj)
	if folderLockWaitFor("GET") != folderLockWait || folderLockWaitFor("SYNC-PULL") < time.Minute || folderLockWaitFor("WORKTREE-PULL") >= 300*time.Second {
		t.Fatal("sync and worktree jobs wait up to the gateway's job timeout")
	}
}

// L2: ime datoteke zaklepa je enako kot v Swift (FolderLockTests.folderLockNameMatchesGo): st_dev macOS z visokim bitom
// (int32 < 0) kot uint32; drugje cel.
func TestFolderLockNameMatchesSwift(t *testing.T) {
	dev := int32(-5) // Stat_t.Dev na macOS
	id := fileID{uint64(dev), 0x1234}
	if n := folderLockName(id, "darwin"); n != "fffffffb-1234.lock" {
		t.Fatalf("darwin: %s", n)
	}
	if n := folderLockName(fileID{1 << 40, 7}, "linux"); n != "10000000000-7.lock" {
		t.Fatalf("linux: %s", n)
	}
}

// L1: mapa zaklepov, v katero sme pisati kdor koli, ni uporabljena (opozorilo, le zaklep v procesu).
func TestFolderLockRefusesSharedLockDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix mode bits")
	}
	d := filepath.Join(t.TempDir(), "locks")
	os.Mkdir(d, 0o777)
	os.Chmod(d, 0o777)
	defer func(old string) { folderLockDir = old }(folderLockDir)
	folderLockDir = d
	u := (&shareClient{root: t.TempDir()}).lockFolder(true, time.Second)
	if u == nil {
		t.Fatal("must fall back to the in-process lock")
	}
	u()
	if ents, _ := os.ReadDir(d); len(ents) != 0 {
		t.Fatalf("lock file created in a world-writable folder: %v", ents)
	}
}

// #82 cilj 1: prevzem, ki ga minigw zavrne s 503 (mapo drži druga seja), konča zahtevo z napako »busy«; spremembe
// ostanejo na prehodu in pridejo z naslednjo zahtevo (sync), veja ostane na prehodu in jo dostavi naslednja zahteva
// (worktree). Prej je bil odgovor 200, delo agenta pa ni prišlo k odjemalcu.
func TestPullRefusedBusyFailsRequest(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake claude is a shell script")
	}
	defer func(a, b time.Duration) { folderSyncWait, folderWorktreePullWait = a, b }(folderSyncWait, folderWorktreePullWait)
	folderSyncWait, folderWorktreePullWait = 300*time.Millisecond, 300*time.Millisecond
	for _, mode := range []string{"sync", "worktree"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			client, marks := filepath.Join(root, "client"), filepath.Join(root, "marks")
			os.MkdirAll(client, 0o755)
			os.MkdirAll(marks, 0o755)
			writeT(t, filepath.Join(client, "a.txt"), "one\n")
			commit := ""
			if mode == "worktree" {
				gitT(t, client, "init", "-q", "-b", "main")
				gitT(t, client, "add", "-A")
				gitT(t, client, "commit", "-qm", "first")
				commit = `git add -A && git -c user.name=a -c user.email=a@x commit -qm "agent $n"`
			}
			fake := filepath.Join(root, "claude")
			writeT(t, fake, `#!/bin/sh
`+cliGuard+`
cat > /dev/null
n=$(ls agent*.txt 2>/dev/null | wc -l | tr -d ' ')
echo "agent $n" > agent$n.txt
`+commit+`
touch `+marks+`/started$n
while [ ! -e `+marks+`/go$n ]; do sleep 0.05; done
echo '{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"done"}}}'
echo '{"type":"result","subtype":"success","usage":{"output_tokens":1}}'
`)
			os.Chmod(fake, 0o755)
			s, _ := testServer(t, func(c *serveConfig) { c.Claude = fake })
			c := startLoopback(t, s)
			sid := "sidbusy-" + mode
			sc := &shareClient{c: c, session: sid, root: client, sync: mode == "sync", worktree: mode == "worktree", top: client,
				synced: map[string]syncEntry{}, reported: map[string]bool{}}
			if !sc.register() {
				t.Fatal("register failed: " + sc.refusal)
			}
			sc.start(2)
			defer sc.unregister()
			h := map[string]string{"x-claude-mini-session": sid, "x-claude-mini-cwd": client, "content-type": "application/json"}
			ask := func(n string, hold bool) (int, string) {
				go func() { // agent teče: druga seja vzame mapo, nato agent konča
					for {
						if _, err := os.Stat(filepath.Join(marks, "started"+n)); err == nil {
							break
						}
						time.Sleep(10 * time.Millisecond)
					}
					if hold {
						other := &shareClient{root: client}
						u := other.lockFolder(true, time.Second)
						defer u()
						os.WriteFile(filepath.Join(marks, "go"+n), nil, 0o644)
						time.Sleep(2 * time.Second) // dlje od roka prevzema
						return
					}
					os.WriteFile(filepath.Join(marks, "go"+n), nil, 0o644)
				}()
				body := `{"model":"claude-cli-haiku","tools":[{"name":"x"}],"messages":[{"role":"user","content":"go` + n + `"}]}`
				st, data := call(c, "POST", "/v1/messages", h, []byte(body), 60*time.Second)
				return st, string(data)
			}
			st, data := ask("0", true)
			if st == 200 || !strings.Contains(data, "the shared folder is busy") ||
				(mode == "sync" && !strings.Contains(data, "kept on the gateway")) || (mode == "worktree" && !strings.Contains(data, "stays on the gateway")) {
				t.Fatalf("pull refused: %d %s", st, data)
			}
			if _, err := os.Stat(filepath.Join(client, "agent0.txt")); mode == "sync" && err == nil {
				t.Fatal("changes copied back although the pull was refused")
			}
			time.Sleep(2 * time.Second) // druga seja spusti mapo
			if st, data := ask("1", false); st != 200 {
				t.Fatalf("next request: %d %s", st, data)
			}
			if mode == "sync" { // obe spremembi (tudi zadržana) pri odjemalcu
				for _, f := range []string{"agent0.txt", "agent1.txt"} {
					if _, err := os.Stat(filepath.Join(client, f)); err != nil {
						t.Fatalf("%s not copied back: %v", f, err)
					}
				}
				return
			}
			branch := "minigw/claude-" + sha256Hex([]byte(sid))[:8] + "-0"
			files := gitT(t, client, "ls-tree", "-r", "--name-only", branch)
			if !strings.Contains(files, "agent0.txt") || !strings.Contains(files, "agent1.txt") {
				t.Fatalf("branch %s after the next request:\n%s", branch, files)
			}
		})
	}
}
