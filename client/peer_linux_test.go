package main

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// N5: pomočnik ob koncu agenta ubije, kar je ostalo (tudi osirotelo), in se konča takoj.
func TestAgentReaperKillsLeftovers(t *testing.T) {
	pidFile := t.TempDir() + "/pid"
	cmd := exec.Command("/bin/sh", "-c", "sh -c 'sleep 60 & echo $! > "+pidFile+"' ; exit 3")
	start := time.Now()
	if err := startAgent(cmd); err != nil {
		t.Fatal(err)
	}
	err := cmd.Wait()
	if time.Since(start) > 5*time.Second || cmd.ProcessState.ExitCode() != 3 {
		t.Fatalf("helper: %v after %v", err, time.Since(start))
	}
	b, _ := os.ReadFile(pidFile)
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if pid == 0 || syscall.Kill(pid, 0) == nil {
		t.Fatalf("orphan %d still runs", pid)
	}
}

// N3: pod pinWrap .git ni mogoče preimenovati (točka priklopa), drugo v mapi pa ja.
func TestPinWrapGitCannotBeRenamed(t *testing.T) {
	if bwrapReady(t.TempDir()) == "" {
		t.Skip("bwrap not available")
	}
	wt := t.TempDir()
	os.Mkdir(wt+"/.git", 0o755)
	os.WriteFile(wt+"/f", []byte("x"), 0o644)
	name, args := pinWrap(t.TempDir(), "/bin/sh", []string{"-c", "mv f g && ! mv .git .g 2>/dev/null"}, []string{wt + "/.git"}, t.Logf)
	if !nestedBwrap {
		t.Skip("nested bwrap not available: pinWrap is off")
	}
	cmd := exec.Command(name, args...)
	cmd.Dir = wt
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if _, err := os.Stat(wt + "/.git"); err != nil {
		t.Fatal(".git was renamed")
	}
}

// pinWrap: kjer bwrap v bwrap ne deluje (Ubuntu 24.04 AppArmor), Claude teče brez pinWrap in to se zapiše enkrat na minuto.
// Lažni bwrap izvede ukaz za »--«; z nested=false odpove, ko teče znotraj sebe.
func TestPinWrapOffWithoutNestedBwrap(t *testing.T) {
	defer func() { bwrapOnce, bwrapPath, nestedAt, nestedBwrap = sync.Once{}, "", time.Time{}, false }()
	for _, nested := range []bool{false, true} {
		dir := t.TempDir()
		guard := `[ -n "$FAKE_BWRAP" ] && exit 1` + "\n"
		if nested {
			guard = ""
		}
		os.WriteFile(dir+"/bwrap", []byte("#!/bin/sh\n"+guard+`while [ "$1" != "--" ]; do shift; done; shift; FAKE_BWRAP=1 exec "$@"`+"\n"), 0o755)
		t.Setenv("PATH", dir+":/usr/bin:/bin")
		bwrapOnce, bwrapPath, nestedAt, nestedBwrap = sync.Once{}, "", time.Time{}, false
		logs := 0
		for range 2 {
			name, _ := pinWrap(t.TempDir(), "/bin/claude", []string{"-p"}, []string{dir}, func(string, ...any) { logs++ })
			if want := map[bool]string{false: "/bin/claude", true: dir + "/bwrap"}[nested]; name != want {
				t.Fatalf("nested=%v: %s, want %s", nested, name, want)
			}
		}
		if want := map[bool]int{false: 1, true: 0}[nested]; logs != want {
			t.Fatalf("nested=%v: logged %d times", nested, logs)
		}
		if !nested { // N3b: neuspeli preizkus se po minuti ponovi (ne sync.Once za ves proces)
			nestedAt = nestedAt.Add(-nestedRetry)
			pinWrap(t.TempDir(), "/bin/claude", []string{"-p"}, []string{dir}, func(string, ...any) { logs++ })
			if logs != 2 {
				t.Fatalf("probe not retried after a minute: logged %d times", logs)
			}
		}
	}
}

// #52: minigw serve kot PID 1 (bwrap --as-pid-1, kot vsebnik brez --init) požanje osirotele procese.
func TestInitReaperReapsOrphans(t *testing.T) {
	bw, err := exec.LookPath("bwrap")
	if err != nil || exec.Command(bw, "--unshare-pid", "--as-pid-1", "--dev-bind", "/", "/", "--proc", "/proc", "true").Run() != nil {
		t.Skip("no usable bwrap (Linux with user namespaces)")
	}
	exe, _ := os.Executable()
	cmd := exec.Command(bw, "--unshare-pid", "--as-pid-1", "--dev-bind", "/", "/", "--proc", "/proc", exe)
	cmd.Env = append(os.Environ(), "MINIGW_TEST_INIT=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
}

func TestAgentReaperPassesDirectoryFD(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(dir+"/checked", []byte("checked"), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cmd := exec.Command("/bin/cat", "/proc/self/fd/3/checked")
	cmd.ExtraFiles = []*os.File{f}
	var out strings.Builder
	cmd.Stdout = &out
	if err := startAgent(cmd); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if out.String() != "checked" {
		t.Fatalf("descriptor lost: %q", out.String())
	}
}

func TestAllowedFolderBwrapUsesCheckedFD(t *testing.T) {
	forceBwrapFD(t, true)
	base := realOf(t, t.TempDir())
	first, second, alias := base+"/first", base+"/second", base+"/alias"
	for _, p := range []string{first, second} {
		if err := os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	dirLink(t, first, alias)
	s, _ := testServer(t, nil)
	c := s.conf()
	c.AllowedFolders = []string{alias}
	if _, err := checkAgent(&c, s.cfgDir, []string{alias}); err != nil {
		t.Fatal(err)
	}
	checked := s.checkedAgentFolders(c, base)
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	dirLink(t, second, alias)
	held, err := checked.open()
	if err != nil {
		t.Fatal(err)
	}
	defer closeFolders(held)
	for _, mode := range []string{"--bind", "--ro-bind"} {
		args, files := pinAllowedBinds("", []string{mode, first, first, "--", "agent", mode, first, first}, checked, held)
		want := []string{mode + "-fd", "3", first, "--", "agent", mode, first, first}
		if len(files) != 1 || files[0] != held[0] {
			t.Fatalf("bind handles: %v", files)
		}
		if strings.Join(args, " ") != strings.Join(want, " ") {
			t.Fatalf("bind: %v", args)
		}
	}
	repeated, files := pinAllowedBinds("", []string{"--bind", first, first, "--ro-bind", first, first}, checked, held)
	if len(files) != 2 || repeated[1] != "3" || repeated[4] != "4" {
		t.Fatalf("reused consumed fd: %v %v", repeated, files)
	}
	bw := bwrapReady(s.cfgDir)
	if bw == "" {
		t.Skip("bwrap not available; descriptor arguments checked")
	}
	name, args, done, err := agentWrap("/bin/true", nil, agentMounts{cfgDir: s.cfgDir, checked: checked, rw: checked.paths()})
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	args, files = pinAllowedBinds(s.cfgDir, args, checked, held)
	if strings.Contains(strings.Join(args, " "), alias) || strings.Contains(strings.Join(args, " "), second) {
		t.Fatalf("retargeted bind: %v", args)
	}
	cmd := exec.Command(name, args...)
	cmd.ExtraFiles = files
	if err := startAgent(cmd); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
}

func forceBwrapFD(t *testing.T, on bool) {
	previous := bwrapFDMode
	bwrapFDMode = func(string) bool { return on }
	t.Cleanup(func() { bwrapFDMode = previous })
}

// Without --bind-fd (bubblewrap before 0.10.0): path binds of the checked real path, identity checked before launch.
func TestAllowedFolderBwrapPathMode(t *testing.T) {
	forceBwrapFD(t, false)
	base := realOf(t, t.TempDir())
	first, second, alias := base+"/first", base+"/second", base+"/alias"
	for _, p := range []string{first, second} {
		if err := os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	dirLink(t, first, alias)
	s, _ := testServer(t, nil)
	c := s.conf()
	c.AllowedFolders = []string{alias}
	if _, err := checkAgent(&c, s.cfgDir, []string{alias}); err != nil {
		t.Fatal(err)
	}
	checked := s.checkedAgentFolders(c, base)
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	dirLink(t, second, alias)
	in := []string{"--bind", first, first, "--", "agent"}
	args, files := pinAllowedBinds(s.cfgDir, in, checked, nil)
	if len(files) != 0 || strings.Join(args, " ") != strings.Join(in, " ") || strings.Contains(strings.Join(args, " "), alias) {
		t.Fatalf("path mode: %v %v", args, files)
	}
	previous := bwrapPath
	bwrapPath = "/test/bwrap"
	defer func() { bwrapPath = previous }()
	name, cargs, files := allowedClaudeWrap(s.cfgDir, "claude", []string{"--", "x"}, checked, nil, false, t.Logf)
	if name != "claude" || len(files) != 0 || strings.Join(cargs, " ") != "-- x" {
		t.Fatalf("Claude path mode: %s %v %v", name, cargs, files)
	}
	// The link target changed after validation: the checked real path itself is replaced, the launch refuses.
	if err := os.Rename(first, first+"-old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(first, 0700); err != nil {
		t.Fatal(err)
	}
	if err := checked.verify(); err == nil {
		t.Fatal("launch not refused after the checked folder changed")
	}
}

func TestAllowedClaudeWrapPinsSnapshot(t *testing.T) {
	forceBwrapFD(t, true)
	path := realOf(t, t.TempDir())
	id, err := fileIDOf(path)
	if err != nil {
		t.Fatal(err)
	}
	checked := checkedFolders{{path, id}}
	held, err := checked.open()
	if err != nil {
		t.Fatal(err)
	}
	defer closeFolders(held)
	previous := bwrapPath
	bwrapPath = "/test/bwrap"
	defer func() { bwrapPath = previous }()
	for _, readOnly := range []bool{false, true} {
		name, args, files := allowedClaudeWrap("", bwrapPath, []string{"--dev-bind", "/", "/", "--", "agent", "--bind", "untrusted", "untrusted"}, checked, held, readOnly, t.Logf)
		mode := "--bind-fd"
		if readOnly {
			mode = "--ro-bind-fd"
		}
		want := "--dev-bind / / " + mode + " 3 " + path + " -- agent --bind untrusted untrusted"
		if name != bwrapPath || strings.Join(args, " ") != want || len(files) != 1 || files[0] != held[0] {
			t.Fatalf("Claude pin: %s %v %v", name, args, files)
		}
	}
}

// bwrap descriptors: the agent must not hold the descriptors of the allowed folders (a read-only run could write
// through one). Starts the real wrap; the agent lists every descriptor above 2 it holds.
func TestAllowedFolderDescriptorsNotInherited(t *testing.T) {
	probe := bwrapFDMode("")
	forceBwrapFD(t, true)
	base := realOf(t, t.TempDir())
	rw, ro := base+"/rw", base+"/ro"
	for _, p := range []string{rw, ro} {
		if err := os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	s, _ := testServer(t, nil)
	if bwrapReady(s.cfgDir) == "" {
		t.Skip("bwrap not available")
	}
	c := s.conf()
	c.AllowedFolders = []string{rw, ro}
	if _, err := checkAgent(&c, s.cfgDir, nil); err != nil {
		t.Fatal(err)
	}
	checked := s.checkedAgentFolders(c, base)
	held, err := checked.open()
	if err != nil {
		t.Fatal(err)
	}
	defer closeFolders(held)
	// Control: without bwrap the same listing shows an inherited descriptor, so the check below can fail.
	list := `for f in /proc/$$/fd/*; do n=${f##*/}; [ "$n" -gt 2 ] && echo "$n $(readlink $f)"; done`
	plain, err := os.Open(rw)
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	control := exec.Command("/bin/sh", "-c", list)
	control.ExtraFiles = []*os.File{plain}
	if b, err := control.Output(); err != nil || !strings.Contains(string(b), rw) {
		t.Fatalf("the listing does not show an inherited descriptor: %q %v", b, err)
	}
	script := list + `; ls ` + rw + ` >/dev/null && echo bound`
	name, args, done, err := agentWrap("/bin/sh", []string{"-c", script}, agentMounts{cfgDir: s.cfgDir, checked: checked, rw: checked.paths()})
	if err != nil {
		t.Fatal(err)
	}
	defer done()
	args, files := pinAllowedBinds(s.cfgDir, args, checked, held)
	if len(files) != 2 {
		t.Fatalf("descriptor binds not used: %v", args)
	}
	cmd := exec.Command(name, args...)
	cmd.ExtraFiles = files
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := startAgent(cmd); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("%v: %s", err, out.String())
	}
	if !strings.Contains(out.String(), "bound") {
		t.Fatalf("allowed folder not bound: %s", out.String())
	}
	for _, line := range strings.Split(out.String(), "\n") {
		if strings.Contains(line, rw) || strings.Contains(line, ro) {
			t.Fatalf("the agent holds a descriptor of an allowed folder: %s", out.String())
		}
	}
	t.Logf("descriptors the agent holds above 2: %q; the start probe accepts this bwrap: %v", out.String(), probe)
}
