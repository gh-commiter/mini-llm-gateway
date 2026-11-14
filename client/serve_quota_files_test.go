package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

const planted = `{"payload":{"rate_limits":{"primary":{"used_percent":5,"window_minutes":300,"resets_at":4102444800}}}}` + "\n"

// Q3: the session log of a project CODEX_HOME is a place an agent run has touched: links in it lead nowhere.
func TestCodexLogIsNotReadThroughLinks(t *testing.T) {
	home := t.TempDir()
	real := t.TempDir() // another place, with a log that matches
	os.MkdirAll(filepath.Join(real, "2026", "11", "14"), 0o755)
	os.WriteFile(filepath.Join(real, "2026", "11", "14", "rollout-x-thread-a.jsonl"), []byte(planted), 0o644)
	os.MkdirAll(filepath.Join(home, "sessions"), 0o755)
	if err := os.Symlink(filepath.Join(real, "2026"), filepath.Join(home, "sessions", "2026")); err != nil {
		t.Skip("no symlinks here:", err)
	}
	if got := codexSessionFile(home, "thread-a"); got != "" {
		t.Fatalf("entered a linked year folder: %s", got)
	}
	// a linked day folder, and a linked log
	day := filepath.Join(home, "sessions", "2027", "01", "02")
	os.MkdirAll(filepath.Join(home, "sessions", "2027", "01"), 0o755)
	os.Symlink(filepath.Join(real, "2026", "11", "14"), day)
	if got := codexSessionFile(home, "thread-a"); got != "" {
		t.Fatalf("entered a linked day folder: %s", got)
	}
	os.Remove(day)
	os.MkdirAll(day, 0o755)
	os.Symlink(filepath.Join(real, "2026", "11", "14", "rollout-x-thread-a.jsonl"), filepath.Join(day, "rollout-y-thread-a.jsonl"))
	if got := codexSessionFile(home, "thread-a"); got != "" {
		t.Fatalf("took a linked log: %s", got)
	}
	// the same file, directly: no link, a log
	good := filepath.Join(day, "rollout-z-thread-a.jsonl")
	os.WriteFile(good, []byte(planted), 0o644)
	if got := codexSessionFile(home, "thread-a"); got != good {
		t.Fatalf("the real log was not found: %q", got)
	}
	// a link given as the path itself is refused by the reader, also when it points at a real log
	if v := lastRateLimits(filepath.Join(day, "rollout-y-thread-a.jsonl"), time.Now()); v != nil {
		t.Fatalf("read through a link: %+v", v)
	}
	if v := lastRateLimits(good, time.Now()); len(v) != 1 {
		t.Fatalf("%+v", v)
	}
	if runtime.GOOS != "windows" { // a link to a device is no log and is never read
		l := filepath.Join(day, "rollout-dev-thread-b.jsonl")
		if os.Symlink("/dev/zero", l) == nil {
			if got := codexSessionFile(home, "thread-b"); got != "" {
				t.Fatalf("took a link to /dev/zero: %s", got)
			}
			if v := lastRateLimits(l, time.Now()); v != nil {
				t.Fatalf("%+v", v)
			}
		}
	}
}
