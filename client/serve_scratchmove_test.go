package main

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// scratchFixture: zrcalo, klon, odvisnosti, stanje in povezava (ne sledi se ji).
func scratchFixture(t *testing.T, dir string) {
	t.Helper()
	for p, body := range map[string]string{"repos/dev/a.git/HEAD": "ref: x\n", "worktrees/dev/a/main.go": "package main\n",
		"deps/cmake/big.bin": strings.Repeat("z", 1<<20), "state/abc/claude/s.json": "{}"} {
		f := filepath.Join(dir, p)
		os.MkdirAll(filepath.Dir(f), 0o700)
		if err := os.WriteFile(f, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-48 * time.Hour).Truncate(time.Second)
	os.Chtimes(filepath.Join(dir, "deps/cmake/big.bin"), old, old)
	if runtime.GOOS != "windows" {
		os.Symlink("/nonexistent/target", filepath.Join(dir, "worktrees/dev/a/link"))
	}
}

func checkFixture(t *testing.T, dir string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "deps/cmake/big.bin"))
	if err != nil || len(b) != 1<<20 {
		t.Fatalf("deps not moved: %v", err)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "deps/cmake/big.bin")); time.Since(fi.ModTime()) < 47*time.Hour {
		t.Fatalf("mtime not kept: %v", fi.ModTime())
	}
	for _, p := range []string{"repos/dev/a.git/HEAD", "worktrees/dev/a/main.go", "state/abc/claude/s.json"} {
		if _, err := os.Stat(filepath.Join(dir, p)); err != nil {
			t.Fatal(err)
		}
	}
	if runtime.GOOS != "windows" {
		if l, err := os.Readlink(filepath.Join(dir, "worktrees/dev/a/link")); err != nil || l != "/nonexistent/target" {
			t.Fatalf("link: %q %v", l, err)
		}
	}
}

func TestMoveScratchRenameSameVolume(t *testing.T) {
	old, dst := filepath.Join(t.TempDir(), "old"), filepath.Join(t.TempDir(), "new", "scratch")
	scratchFixture(t, old)
	renamed, err := moveScratchDir(old, dst)
	if err != nil || !renamed {
		t.Fatalf("renamed=%v err=%v", renamed, err)
	}
	checkFixture(t, dst)
	if _, err := os.Stat(old); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("old folder left")
	}
}

// Drug nosilec: scratchCopy neposredno (rename v testu vedno uspe), nato preverjanje in povezave.
func TestMoveScratchCopyCrossVolume(t *testing.T) {
	old, dst := filepath.Join(t.TempDir(), "old"), filepath.Join(t.TempDir(), "new")
	scratchFixture(t, old)
	if err := protectTarget(dst); err != nil {
		t.Fatal(err)
	}
	if err := scratchCopy(old, dst); err != nil {
		t.Fatal(err)
	}
	checkFixture(t, dst)
	checkFixture(t, old) // kopija: original ostane do zamenjave nastavitev
}

func TestMoveScratchRefusals(t *testing.T) {
	old := filepath.Join(t.TempDir(), "old")
	scratchFixture(t, old)
	if _, err := moveScratchDir(old, filepath.Join(old, "inner")); err == nil {
		t.Fatal("moved into itself")
	}
	full := t.TempDir()
	os.WriteFile(filepath.Join(full, "x"), nil, 0o600)
	if _, err := moveScratchDir(old, full); err == nil || !strings.Contains(err.Error(), "already contains") {
		t.Fatalf("non-empty target: %v", err)
	}
	checkFixture(t, old)
}

// Napaka DACL (zaščite) na cilju: stara mapa ostane, cilja ni.
func TestMoveScratchProtectFailureRollsBack(t *testing.T) {
	old, dst := filepath.Join(t.TempDir(), "old"), filepath.Join(t.TempDir(), "new")
	scratchFixture(t, old)
	var calls []string
	protectTarget = func(d string) error { calls = append(calls, d); return errors.New("no DACL") }
	t.Cleanup(func() { protectTarget = protectScratch })
	if _, err := moveScratchDir(old, dst); err == nil {
		t.Fatal("no error")
	}
	if len(calls) == 0 || calls[0] != dst {
		t.Fatalf("protect not called on the target: %v", calls)
	}
	checkFixture(t, old)
	if _, err := os.Stat(dst); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("target left: %v", err)
	}
}

func TestMoveScratchAPI(t *testing.T) {
	s, cfg, _ := liveServer(t, nil)
	old := s.conf().Scratch
	scratchFixture(t, old)
	to := filepath.Join(nonTempDir(t), "moved")
	dst := filepath.Join(to, s.conf().Instance) // odločitev 3: "to" je osnova
	api := func(body string) (int, map[string]any) {
		st, obj := s.adminDo("POST", "/general/scratch/move", []byte(body))
		m, _ := obj.(map[string]any)
		if g, ok := obj.(apiGeneral); ok {
			m = map[string]any{"scratch": g.Scratch}
		}
		return st, m
	}
	// zaposlen: tekoča zahteva drži bralno ključavnico, seja v hubu, agent
	s.scratchGate.RLock()
	if st, m := api(`{"to":"` + filepath.ToSlash(to) + `"}`); st != 409 || !strings.Contains(errMsg(m), "sessions or agents are active") {
		t.Fatalf("busy request: %d %v", st, m)
	}
	s.scratchGate.RUnlock()
	s.agentsRunning.Add(1)
	if st, _ := api(`{"to":"` + filepath.ToSlash(to) + `"}`); st != 409 {
		t.Fatalf("busy agent: %d", st)
	}
	s.agentsRunning.Add(-1)
	if st, _ := api(`{"to":"relative"}`); st != 400 {
		t.Fatalf("relative: %d", st)
	}
	if st, m := api(`{"to":"` + filepath.ToSlash(to) + `"}`); st != 200 || m["scratch"] != filepath.Clean(dst) {
		t.Fatalf("move: %d %v", st, m)
	}
	checkFixture(t, dst)
	if c, err := loadServeConfig(cfg); err != nil || c.Scratch != filepath.Clean(dst) {
		t.Fatalf("serve.json: %q %v", c.Scratch, err)
	}
	// CLI: minigw gw scratch [move DIR]
	out := stdout(t, func() { routerCommand([]string{"scratch", "--config", cfg}, strings.NewReader("")) })
	if !strings.Contains(out, "Scratch folder: "+filepath.Clean(dst)) {
		t.Fatalf("cli: %q", out)
	}
	back := filepath.Join(nonTempDir(t), "back")
	out = stdout(t, func() { routerCommand([]string{"scratch", "move", back, "--config", cfg}, strings.NewReader("")) })
	if back = filepath.Join(back, s.conf().Instance); !strings.Contains(out, "Scratch folder: "+back) || s.conf().Scratch != back {
		t.Fatalf("cli move: %q %q", out, s.conf().Scratch)
	}
	checkFixture(t, back)
}

// Med premikom nove zahteve odjemalcev dobijo 503 (ne začnejo seje v stari mapi), potem spet 200.
func TestScratchGateRefusesDuringMove(t *testing.T) {
	mac := newDevice("Mac")
	s, k := testServer(t, func(c *serveConfig) { c.AllowAnyDevice = true })
	get := "GET /v1/models HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer tok\r\n\r\n"
	s.scratchGate.Lock()
	if r := pipeRequest(t, s, k.PublicKey().Bytes(), mac, get); !strings.HasPrefix(r, "HTTP/1.1 503") || !strings.Contains(r, "moving its Scratch") {
		t.Fatalf("during the move: %q", r)
	}
	s.scratchGate.Unlock()
	if r := pipeRequest(t, s, k.PublicKey().Bytes(), mac, get); !strings.HasPrefix(r, "HTTP/1.1 200") {
		t.Fatalf("after the move: %q", r)
	}
}

func errMsg(m map[string]any) string {
	e, _ := m["error"].(map[string]string)
	return e["message"]
}

// serve.json se ne da zamenjati: kopija/preimenovanje nazaj, stara mapa ostane Scratch.
func TestMoveScratchConfigFailureRollsBack(t *testing.T) {
	s, cfg, _ := liveServer(t, nil)
	old := s.conf().Scratch
	scratchFixture(t, old)
	os.Remove(cfg)
	os.Mkdir(cfg, 0o700) // loadServeConfig ne uspe
	dst := filepath.Join(nonTempDir(t), "moved")
	if st, _ := s.adminDo("POST", "/general/scratch/move", []byte(`{"to":"`+filepath.ToSlash(dst)+`"}`)); st != 500 {
		t.Fatalf("status %d", st)
	}
	checkFixture(t, old)
	if s.conf().Scratch != old {
		t.Fatal("scratch switched")
	}
	if _, err := os.Stat(filepath.Join(dst, s.conf().Instance)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("target left: %v", err)
	}
}
