package main

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
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
			if unlock := other.lockFolder(true); unlock != nil {
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
	ua := a.lockFolder(false)
	ub := b.lockFolder(false)
	if ua == nil || ub == nil {
		t.Fatal("two readers must hold the lock together")
	}
	defer func(w time.Duration) { folderLockWait = w }(folderLockWait)
	folderLockWait = 100 * time.Millisecond
	if u := w.lockFolder(true); u != nil {
		t.Fatal("a writer must wait for the readers")
	}
	ua()
	ub()
	if u := w.lockFolder(true); u == nil {
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
	if (&shareClient{root: root}).lockFolder(true) == nil {
		os.Exit(1)
	}
	os.Stdout.WriteString("locked\n")
	time.Sleep(time.Minute)
}
