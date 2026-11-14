package core

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// RestoreOutcome says what really happened (review): not applied (the previous database is in place), applied with a
// cleanup left for the next start, or applied. stopHint tells how to stop the gateway ("stop minigw serve first").
func RestoreOutcome(err error, stopHint string) (msg string, failed bool) {
	switch {
	case err == nil:
		return "", false
	case errors.Is(err, ErrLocked):
		return "the gateway (or another minigw) has the core database open: " + stopHint + ", then run the restore again. Nothing was changed.", true
	case errors.Is(err, ErrRestoreApplied):
		return "restore APPLIED: the backup is the database now (the previous one is kept as a snapshot next to it), but a cleanup step failed and is finished when the gateway starts: " + err.Error(), false
	}
	return "restore NOT applied, the previous database is in place: " + err.Error(), true
}

// RestoreCLI is `minigw gw core restore <file|name>` on the store folder, with no gateway running (the store's lock tells):
// the text, the question and the exit code are the same for minigw serve (Go) and the Mac's minigw-core restore. A name
// without a path is looked up in the store's backups folder. Returns the exit code.
func RestoreCLI(store, file string, yes bool, in io.Reader, out, errw io.Writer, stopHint string) int {
	fail := func(s string) int { fmt.Fprintln(errw, "minigw: "+s); return 1 }
	if !strings.ContainsAny(file, `/\`) { // a backup's name: it is in the protected backups folder
		if fi, err := os.Stat(filepath.Join(store, BackupFolder, file)); err == nil && fi.Mode().IsRegular() {
			file = filepath.Join(store, BackupFolder, file)
		}
	}
	fmt.Fprintln(out, "Restoring "+file+" brings back the users, bindings, memberships and grants of that backup, also ones revoked since.")
	fmt.Fprintln(out, "The approved device list (serve.json) is not part of the backup and stays as it is. Every run and conversation ends (the epoch grows).")
	if !yes {
		fmt.Fprint(out, "Continue? [y/N] ")
		line, _ := bufio.NewReader(in).ReadString('\n')
		if l := strings.ToLower(strings.TrimSpace(line)); l != "y" && l != "yes" {
			return fail("cancelled")
		}
	}
	err := Restore(store, file)
	if msg, failed := RestoreOutcome(err, stopHint); failed {
		return fail(msg)
	} else if err != nil {
		fmt.Fprintln(out, msg)
		return 0
	}
	fmt.Fprintln(out, "restored; the previous database is kept next to it as core-<schema>-<time>.db")
	if _, err := os.Stat(store); err != nil {
		return fail(err.Error())
	}
	return 0
}
