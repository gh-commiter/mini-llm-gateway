package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// 0.6.0 task 7a (#106), Linux: a real process under the agent's bwrap cannot read or write the core folder (the
// database, its sidecars, backups, the usage spool): not by its path, not through a link in an allowed folder, not
// when the allowed folder is the folder around the config folder. Runs where bwrap works (the Docker release gate).
func TestBwrapHidesTheCoreFolder(t *testing.T) {
	s, _ := testServer(t, nil)
	if bwrapReady(s.cfgDir) == "" {
		t.Skip("bwrap not available here; the bwrap step of the release gate (privileged container with bubblewrap) fails if this is skipped")
	}
	core := filepath.Join(s.cfgDir, coreDirName)
	if err := os.MkdirAll(filepath.Join(core, "backups"), 0o700); err != nil {
		t.Fatal(err)
	}
	files := []string{"core.db", "core.db-wal", "core.db-shm", "core.state", "core.audit.log", "usage.spool", "backups/core-1.db"}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(core, f), []byte("SECRET-NOTES"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	base := realOf(t, t.TempDir())
	work := filepath.Join(base, "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "f.txt"), []byte("ALLOWED"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(work, "lnk")
	if err := os.Symlink(core, link); err != nil {
		t.Fatal(err)
	}
	rootLink := filepath.Join(work, "cfg")
	if err := os.Symlink(s.cfgDir, rootLink); err != nil {
		t.Fatal(err)
	}
	// the folder around the config folder is allowed as well (rw): the config folder stays masked inside it
	parent := realOf(t, filepath.Dir(s.cfgDir))
	script := `cat "$W/f.txt"; echo; for f in ` + strings.Join(files, " ") + `; do
  for d in "$C" "$W/lnk" "$W/cfg/core"; do cat "$d/$f" 2>/dev/null && echo "LEAK $d/$f"; done
done
ls "$C" 2>/dev/null | grep -q . && echo "LEAK listing"
echo x > "$C/new" 2>/dev/null && echo "LEAK write"
mv "$C/core.db" "$W/stolen" 2>/dev/null && echo "LEAK move"
echo end`
	for _, rw := range [][]string{{work}, {work, parent}} {
		name, args, done, err := agentWrap("/bin/sh", []string{"-c", script}, agentMounts{cfgDir: s.cfgDir, rw: rw, cwd: work})
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(name, args...)
		cmd.Env = append(os.Environ(), "C="+core, "W="+work)
		out, err := cmd.CombinedOutput()
		done()
		if err != nil || !strings.HasPrefix(string(out), "ALLOWED") || !strings.HasSuffix(strings.TrimSpace(string(out)), "end") || strings.Contains(string(out), "LEAK") || strings.Contains(string(out), "SECRET") {
			t.Fatalf("rw %v: %v\n%s", rw, err, out)
		}
		if _, err := os.Stat(filepath.Join(core, "new")); err == nil {
			t.Fatal("the agent wrote into the core folder")
		}
		if b, _ := os.ReadFile(filepath.Join(core, "core.db")); string(b) != "SECRET-NOTES" {
			t.Fatalf("core.db changed: %q", b)
		}
	}
}

// A hard link from an allowed folder to core.db: bwrap masks the config folder by path, the alias is the same inode
// in a folder that is bound, so THE SANDBOX ALONE DOES NOT STOP IT (asserted below as the control). What stops it is the
// core: it refuses to open while a core file has a name outside the core folder, and every launch is refused.
func TestBwrapAloneDoesNotStopAHardLinkSoTheCoreRefuses(t *testing.T) {
	s, _ := testServer(t, func(c *serveConfig) { c.Codex.Enabled = true })
	if bwrapReady(s.cfgDir) == "" {
		t.Skip("bwrap not available here; the bwrap step of the release gate (privileged container with bubblewrap) fails if this is skipped")
	}
	core := filepath.Join(s.cfgDir, coreDirName)
	if err := os.MkdirAll(core, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(core, "core.db"), []byte("SECRET-NOTES"), 0o600); err != nil {
		t.Fatal(err)
	}
	work := realOf(t, t.TempDir())
	if err := os.Link(filepath.Join(core, "core.db"), filepath.Join(work, "alias.db")); err != nil {
		t.Fatal(err)
	}
	name, args, done, err := agentWrap("/bin/sh", []string{"-c", `cat "$W/alias.db"`}, agentMounts{cfgDir: s.cfgDir, rw: []string{work}, cwd: work})
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	cmd := exec.Command(name, args...)
	cmd.Env = append(os.Environ(), "W="+work)
	out, _ := cmd.CombinedOutput()
	if !strings.Contains(string(out), "SECRET-NOTES") {
		t.Logf("bwrap stopped the alias by itself (more than expected): %q", out) // not a failure: the core still refuses
	}
	s.openCore()
	if s.core != nil {
		s.closeCore()
		t.Fatal("the core opened although core.db has a second name")
	}
	if why := s.coreRefusal(s.conf(), "codex"); !strings.Contains(why, "remove the other link") {
		t.Fatalf("launch: %q", why)
	}
}
