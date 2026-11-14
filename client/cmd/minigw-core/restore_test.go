package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"minigw/core"
)

func restoreRun(args ...string) (code int, out, errText string) {
	var o, e bytes.Buffer
	return restoreMain(args, strings.NewReader("y\n"), &o, &e), o.String(), e.String()
}

// users: the names of the users in the store, in order.
func users(t *testing.T, dir string) (names []string) {
	t.Helper()
	s, err := core.Open(dir, core.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	us, err := s.ListUsers(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range us {
		names = append(names, u.Name)
	}
	return
}

func TestRestoreSubcommandBackupChangeRestore(t *testing.T) {
	dir := t.TempDir()
	s, err := core.Open(dir, core.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	s.CreateUser(ctx, "before")
	if _, err := s.BackupNamed(ctx, "b1.db"); err != nil {
		t.Fatal(err)
	}
	s.CreateUser(ctx, "after")

	// the helper (or the gateway) has the store open: refused, nothing changed
	code, _, errText := restoreRun("-store", dir, "-yes", "b1.db")
	if code != 1 || !strings.Contains(errText, "minigw: the gateway (or another minigw) has the core database open: quit the Mini Gateway app first") || !strings.Contains(errText, "Nothing was changed") {
		t.Fatalf("locked: %d %q", code, errText)
	}
	s.Close()
	if u := users(t, dir); len(u) != 2 {
		t.Fatalf("refused restore changed the database: %q", u)
	}

	// without -yes the question is asked, and a no ends it
	var o, e bytes.Buffer
	if c := restoreMain([]string{"-store", dir, "b1.db"}, strings.NewReader("n\n"), &o, &e); c != 1 || e.String() != "minigw: cancelled\n" || !strings.Contains(o.String(), "Continue? [y/N] ") {
		t.Fatalf("cancel: %d %q %q", c, o.String(), e.String())
	}
	if u := users(t, dir); len(u) != 2 {
		t.Fatalf("cancelled restore changed the database: %q", u)
	}

	// the name is looked up in the backups folder
	code, out, errText := restoreRun("-store", dir, "-yes", "b1.db")
	if code != 0 || errText != "" || !strings.Contains(out, "restored; the previous database is kept next to it") {
		t.Fatalf("restore: %d %q %q", code, out, errText)
	}
	if u := users(t, dir); len(u) != 1 || u[0] != "before" {
		t.Fatalf("not restored: %q", u)
	}
}

func TestRestoreSubcommandUsageAndOtherFlags(t *testing.T) {
	for _, args := range [][]string{{}, {"-store", t.TempDir()}, {"x.db"}, {"-store", t.TempDir(), "a", "b"}, {"-socket", "x"}} {
		if code, _, errText := restoreRun(args...); code != exitUsage || errText == "" {
			t.Errorf("%v: %d %q", args, code, errText)
		}
	}
	if code, _, errText := restoreRun("-store", t.TempDir(), "-yes", "missing.db"); code != 1 || !strings.Contains(errText, "restore NOT applied") {
		t.Errorf("missing backup: %d %q", code, errText)
	}
}
