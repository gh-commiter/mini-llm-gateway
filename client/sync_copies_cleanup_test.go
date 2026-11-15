package main

import (
	"crypto/sha256"
	"fmt"
	"os/exec"
	"strings"
	"testing"
)

func TestSyncCopiesE2ECleanup(t *testing.T) {
	shell, err := exec.LookPath("zsh")
	if err != nil {
		t.Skip("requires zsh for the profile e2e scripts")
	}
	data := srcFile(t, "../scripts/sync-copies-cleanup.zsh")
	script := string(data)
	cwd := "/private/tmp/profile cleanup/proj"
	sum := sha256.Sum256([]byte(cwd))
	owned := fmt.Sprintf("%x", sum)[:16]
	for _, tc := range []struct{ name, before string }{
		{"existing", "0000000000000000"},
		{"empty", "no synced copies"},
		{"existing-read-only", owned + "-ro"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := "4"
			expected := "removed " + owned + "-ro\nremoved " + owned + "\nremoved " + owned + "-ro"
			if tc.name == "existing-read-only" {
				calls = "2"
				expected = "removed " + owned
			}
			fresh := owned + "\n" + owned + "-ro\n2222222222222222"
			if tc.name == "existing" {
				fresh = tc.before + "\n" + fresh
			}
			cmd := exec.Command(shell, "-c", script+`
set -eu
OLD_COPIES=$1
NEW_COPIES=$2
OWNED=$3
CWD=$4
EXPECTED_CALLS=$5
phase=before
remove_calls=0
sync_admin() {
 [[ $1 == sync && $* == *'--device test-device' ]] || exit 2
 if [[ $2 == list ]]; then
  if [[ $phase == before ]]; then print -r -- "$OLD_COPIES"; else print -r -- "$NEW_COPIES"; fi
 else
  [[ $3 == "$OWNED" || $3 == "$OWNED-ro" ]] || exit 3
  (( ++remove_calls ))
  (( remove_calls > 1 )) || return 1
  print "removed $3"
 fi
}
sleep() { :; }
sync_copies_begin test-device "$CWD"
phase=after
sync_copies_cleanup
[[ -z $SYNC_COPIES_DEVICE && $remove_calls == $EXPECTED_CALLS ]]
sync_copies_cleanup
`, "cleanup", tc.before, fresh, owned, cwd, calls)
			out, err := cmd.CombinedOutput()
			if err != nil || strings.TrimSpace(string(out)) != expected {
				t.Fatalf("cleanup: %v %s", err, out)
			}
		})
	}
}

func TestSyncCopiesE2ECleanupFailure(t *testing.T) {
	shell, err := exec.LookPath("zsh")
	if err != nil {
		t.Skip("requires zsh for the profile e2e scripts")
	}
	data := srcFile(t, "../scripts/sync-copies-cleanup.zsh")
	cmd := exec.Command(shell, "-c", string(data)+`
set -eu
SYNC_COPIES_DEVICE=test-device
SYNC_COPIES_ID=1111111111111111
sync_admin() {
 if [[ $2 == list ]]; then print 1111111111111111; else return 1; fi
}
sleep() { :; }
if sync_copies_cleanup; then exit 2; fi
[[ $SYNC_COPIES_DEVICE == test-device ]]
`)
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "gateway synced-copy cleanup failed") {
		t.Fatalf("cleanup failure: %v %s", err, out)
	}
}

// Check the actual entry points as well as the shared helper: dropping either
// setup or cleanup from a profile script must break the regression coverage.
func TestProfileScriptsWireSyncCopiesCleanup(t *testing.T) {
	for _, tc := range []struct{ file, source, begin, session string }{
		{"profiles-e2e.sh", "source scripts/sync-copies-cleanup.zsh", `sync_copies_begin "$DEVICE" "$H/proj"`, "(cd $H/proj &&"},
		{"profiles-swift-e2e.sh", `source "$W/scripts/sync-copies-cleanup.zsh"`, `sync_copies_begin local "$T/proj"`, "(cd $T/proj &&"},
	} {
		t.Run(tc.file, func(t *testing.T) {
			b := srcFile(t, "../scripts/"+tc.file)
			s := string(b)
			source, begin, session := strings.Index(s, tc.source), strings.Index(s, tc.begin), strings.Index(s, tc.session)
			if source < 0 || begin <= source || session <= begin {
				t.Fatal("missing cleanup setup before session")
			}
			if !strings.Contains(s[:begin], "sync_copies_cleanup ||") || !strings.Contains(s[session:], "\nsync_copies_cleanup ||") {
				t.Fatal("missing exit or post-session cleanup")
			}
		})
	}
}
