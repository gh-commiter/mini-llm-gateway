package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestSyncCopiesE2ECleanup(t *testing.T) {
	shell, err := exec.LookPath("zsh")
	if err != nil {
		t.Skip("requires zsh for the profile e2e scripts")
	}
	data, err := os.ReadFile("../scripts/sync-copies-cleanup.zsh")
	if err != nil {
		t.Fatal(err)
	}
	script := string(data)
	for _, tc := range []struct{ name, before string }{
		{"existing", "0000000000000000"},
		{"empty", "no synced copies"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fresh := "1111111111111111"
			if tc.name == "existing" {
				fresh = tc.before + "\n" + fresh
			}
			cmd := exec.Command(shell, "-c", script+`
set -eu
OLD_COPIES=$1
NEW_COPIES=$2
phase=before
remove_calls=0
sync_admin() {
 [[ $1 == sync && $* == *'--device test-device' ]] || exit 2
 if [[ $2 == list ]]; then
  if [[ $phase == before ]]; then print -r -- "$OLD_COPIES"; else print -r -- "$NEW_COPIES"; fi
 else
  [[ $3 == 1111111111111111 ]] || exit 3
  (( ++remove_calls ))
  (( remove_calls > 1 )) || return 1
  print "removed $3"
 fi
}
sleep() { :; }
sync_copies_begin test-device
phase=after
sync_copies_cleanup
[[ -z $SYNC_COPIES_DEVICE && $remove_calls == 2 ]]
sync_copies_cleanup
`, "cleanup", tc.before, fresh)
			out, err := cmd.CombinedOutput()
			if err != nil || strings.TrimSpace(string(out)) != "removed 1111111111111111" {
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
	data, err := os.ReadFile("../scripts/sync-copies-cleanup.zsh")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(shell, "-c", string(data)+`
set -eu
SYNC_COPIES_DEVICE=test-device
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
