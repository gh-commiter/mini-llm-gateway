package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func writeOwner(t *testing.T, dir, id string) {
	t.Helper()
	os.MkdirAll(dir, 0o700)
	if err := os.WriteFile(filepath.Join(dir, scratchOwnerFile), []byte(id+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// N4, odločitev 2: prva raba zapiše lastnika; tuj lastnik z zaklepom = in-use, brez zaklepa = stale.
func TestClaimScratchOwner(t *testing.T) {
	a := filepath.Join(t.TempDir(), "a")
	if err := protectScratch(a); err != nil { // Windows: ročaj je zaklep
		t.Fatal(err)
	}
	if st := claimScratch(a, "gw-a"); st != "" || scratchOwner(a) != "gw-a" {
		t.Fatalf("first use: %q %q", st, scratchOwner(a))
	}
	if st := claimScratch(a, "gw-a"); st != "" {
		t.Fatalf("own folder again: %q", st)
	}
	if st := scratchState(a, "gw-b"); st != scratchInUse { // gw-a teče (ta proces drži zaklep)
		t.Fatalf("held by the other: %q", st)
	}
	b := filepath.Join(t.TempDir(), "b")
	t.Cleanup(releaseScratch) // Windows: ročaj bi preprečil brisanje TempDir
	protectScratch(b)
	claimScratch(b, "gw-a") // zaklep gre na drugo mapo: a ostane brez
	if st := claimScratch(a, "gw-b"); st != scratchStale || scratchOwner(a) != "gw-a" {
		t.Fatalf("stale owner: %q, owner %q", st, scratchOwner(a))
	}
}

// Kaj šteje kot pogovori: datoteka kjerkoli razen v predpomnilnikih, gitexec, tmp, mxc.
func TestScratchHasConversations(t *testing.T) {
	d := t.TempDir()
	writeOwner(t, d, "gw-a")
	for _, p := range []string{"tmp/mxc/x.json", "gitexec/win-1/git.exe", "caches/a", "mxc/b"} {
		os.MkdirAll(filepath.Join(d, filepath.Dir(p)), 0o700)
		os.WriteFile(filepath.Join(d, p), []byte("x"), 0o600)
	}
	os.MkdirAll(filepath.Join(d, "sync", "runs"), 0o700) // prazne mape ne štejejo
	if scratchHasConversations(d) {
		t.Fatal("caches counted")
	}
	os.WriteFile(filepath.Join(d, "sync", "runs", "f"), []byte("x"), 0o600)
	if !scratchHasConversations(d) {
		t.Fatal("clone not counted")
	}
}

// Prevzem = preimenovanje: stara mapa dobi ime in lastnika te instance; prazna lastna mapa izbrisana, s pogovori ob strani.
func TestTakeScratch(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	setup := func(t *testing.T, own string) (old, dst string) {
		base := t.TempDir()
		old, dst = filepath.Join(base, "ana"), filepath.Join(base, "gw-new")
		writeOwner(t, old, "gw-old")
		os.MkdirAll(filepath.Join(old, "worktrees", "dev"), 0o700)
		os.WriteFile(filepath.Join(old, "worktrees", "dev", "f"), []byte("old"), 0o600)
		if own != "" {
			writeOwner(t, dst, "gw-new")
			os.MkdirAll(filepath.Join(dst, filepath.Dir(own)), 0o700)
			os.WriteFile(filepath.Join(dst, own), []byte("new"), 0o600)
		}
		return
	}
	check := func(t *testing.T, old, dst string) {
		t.Helper()
		if _, err := os.Stat(old); !os.IsNotExist(err) {
			t.Fatal("old name still there")
		}
		if b, _ := os.ReadFile(filepath.Join(dst, "worktrees", "dev", "f")); string(b) != "old" || scratchOwner(dst) != "gw-new" {
			t.Fatalf("not renamed: %q %q", b, scratchOwner(dst))
		}
	}
	t.Run("no folder", func(t *testing.T) {
		old, want := setup(t, "")
		dst, aside, err := takeScratch(old, "gw-new", now)
		if err != nil || dst != want || aside != "" {
			t.Fatal(dst, aside, err)
		}
		check(t, old, dst)
	})
	t.Run("own folder without conversations", func(t *testing.T) {
		old, want := setup(t, "tmp/x")
		dst, aside, err := takeScratch(old, "gw-new", now)
		if err != nil || dst != want || aside != "" {
			t.Fatal(dst, aside, err)
		}
		check(t, old, dst)
		if _, err := os.Stat(filepath.Join(dst, "tmp", "x")); !os.IsNotExist(err) {
			t.Fatal("merged")
		}
	})
	t.Run("own folder with conversations", func(t *testing.T) {
		old, want := setup(t, "state/s.json")
		dst, aside, err := takeScratch(old, "gw-new", now)
		if err != nil || dst != want || aside != want+".old-2026-11-10" {
			t.Fatal(dst, aside, err)
		}
		check(t, old, dst)
		if b, _ := os.ReadFile(filepath.Join(aside, "state", "s.json")); string(b) != "new" {
			t.Fatal("not kept aside")
		}
		if _, err := os.Stat(filepath.Join(dst, "state")); !os.IsNotExist(err) {
			t.Fatal("merged")
		}
	})
	t.Run("lock held", func(t *testing.T) {
		if runtime.GOOS == "windows" {
			t.Skip("Windows: the other gateway's handle (HPX360)")
		}
		old, _ := setup(t, "")
		lockScratch(old) // drug prehod teče
		defer lockScratch(t.TempDir())
		if _, _, err := takeScratch(old, "gw-new", now); err == nil || !strings.Contains(err.Error(), "running") {
			t.Fatalf("taken while held: %v", err)
		}
		if scratchOwner(old) != "gw-old" {
			t.Fatal("owner changed")
		}
	})
}

// Strežnik: mapa drugega prehoda = brez agentov (503), stanje v GET /general, Take over prek API.
func TestScratchForeignServer(t *testing.T) {
	s, cfg, _ := liveServer(t, nil)
	old := s.conf().Scratch
	lockScratch(t.TempDir()) // naš zaklep stran: mapa je »stale«
	writeOwner(t, old, "gw-other")
	releaseScratch()
	s.claimScratch()
	if !s.scratchOther.Load() || s.apiGeneral(false).ScratchState != scratchStale {
		t.Fatalf("foreign: %v %q", s.scratchOther.Load(), s.apiGeneral(false).ScratchState)
	}
	out := stdout(t, func() { routerCommand([]string{"scratch", "--config", cfg}, strings.NewReader("")) })
	if !strings.Contains(out, "stale owner") {
		t.Fatalf("cli: %q", out)
	}
	st, obj := s.adminDo("POST", "/general/scratch/take", nil)
	g, _ := obj.(apiGeneral)
	want := filepath.Join(filepath.Dir(old), s.conf().Instance)
	if st != 200 || g.Scratch != want || s.scratchOther.Load() || scratchOwner(want) != s.conf().Instance {
		t.Fatalf("take: %d %v", st, obj)
	}
	if c, err := loadServeConfig(cfg); err != nil || c.Scratch != want {
		t.Fatalf("serve.json: %q %v", c.Scratch, err)
	}
	if st, _ := s.adminDo("POST", "/general/scratch/take", nil); st != 409 {
		t.Fatalf("own folder taken again: %d", st)
	}
}

func TestScratchForeignRefusesRequests(t *testing.T) {
	mac := newDevice("Mac")
	s, k := testServer(t, func(c *serveConfig) { c.AllowAnyDevice = true })
	s.scratchOther.Store(true)
	get := "GET /v1/models HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer tok\r\n\r\n"
	if r := pipeRequest(t, s, k.PublicKey().Bytes(), mac, get); !strings.HasPrefix(r, "HTTP/1.1 503") || !strings.Contains(r, "Scratch folder") {
		t.Fatalf("foreign scratch: %q", r)
	}
}
