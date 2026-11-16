package main

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// claudeCodeCases: primeri #79, isti za Swift (RouterCoreTests, claudeCodeCases).
type claudeCodeCases struct {
	Procs []struct {
		Exe    string
		Args   []string
		Claude bool
	}
	Chains []struct {
		Name     string
		Terminal bool
		Claude   bool
		Chain    []struct {
			Exe                    string
			Args                   []string
			Unreadable, Loop, Gone bool
		}
	}
	Policy []struct {
		Name                      string
		Marker, Override, Checked bool
		Chain                     int
	}
	Args []struct {
		Args    []string
		Refused bool
	}
	Combined []struct {
		Args []string
		Flag string
	}
	Continue []struct {
		Args, Out []string
		Last      string
		Dropped   bool
	}
}

func loadClaudeCodeCases(t *testing.T) claudeCodeCases {
	var c claudeCodeCases
	if err := json.Unmarshal(srcFile(t, "testdata/claude-code-cases.json"), &c); err != nil || len(c.Chains) == 0 {
		t.Fatal(err)
	}
	return c
}

// fakeChain: lažna veriga prednikov (PID 1000 je starš minigw).
func (c claudeCodeCases) fakeChain(i int) procFunc {
	ch := c.Chains[i].Chain
	return func(pid int) (int, string, []string, error) {
		e := ch[pid-1000]
		switch {
		case e.Unreadable:
			return 0, "", nil, errors.New("unreadable")
		case e.Gone:
			return 0, "", nil, errGone
		case e.Loop:
			return 1000, e.Exe, e.Args, nil
		case pid-1000 == len(ch)-1:
			return 0, e.Exe, e.Args, nil
		}
		return pid + 1, e.Exe, e.Args, nil
	}
}

func TestClaudeCodeDetection(t *testing.T) {
	c := loadClaudeCodeCases(t)
	for _, p := range c.Procs {
		if isClaudeProc(p.Exe, p.Args) != p.Claude {
			t.Errorf("isClaudeProc(%q, %q) != %v", p.Exe, p.Args, p.Claude)
		}
	}
	for i, ch := range c.Chains {
		if claudeCaller(1000, c.fakeChain(i), ch.Terminal) != ch.Claude {
			t.Errorf("%s: claude = %v expected", ch.Name, ch.Claude)
		}
	}
	// prava veriga: ta proces ni claude, branje dela (vsi trije sistemi)
	if _, exe, _, err := procLookup()(os.Getpid()); err != nil || isClaudeProc(exe, nil) {
		t.Errorf("own process: %q %v", exe, err)
	}
}

// Možnosti po kontekstu klicatelja: označen (CLAUDECODE ali claude med predniki, markClaudeCode) in brez
// MINIGW_ALLOW_UNSAFE_FLAGS=1 → seznam claudeSafe; sicer vse (D2: človek v terminalu, skripte).
func TestClaudeCodeArgsPolicy(t *testing.T) {
	c := loadClaudeCodeCases(t)
	for _, p := range c.Policy {
		ch := c.Chains[p.Chain]
		env := map[string]string{}
		if p.Marker || claudeCaller(1000, c.fakeChain(p.Chain), ch.Terminal) {
			env["CLAUDECODE"] = "1"
		}
		if p.Override {
			env["MINIGW_ALLOW_UNSAFE_FLAGS"] = "1"
		}
		get := func(k string) string { return env[k] }
		if checkClaudeArgs(get) != p.Checked {
			t.Errorf("%s: checked != %v", p.Name, p.Checked)
		}
		for _, a := range c.Args {
			if refused := unsafeClaudeFlag(a.Args, get) != ""; refused != (p.Checked && a.Refused) {
				t.Errorf("%s: %q refused = %v", p.Name, a.Args, refused)
			}
		}
	}
}

func TestContinueSession(t *testing.T) {
	c := loadClaudeCodeCases(t)
	for _, k := range c.Combined {
		if f := combinedSessionFlag(k.Args); f != k.Flag {
			t.Errorf("combinedSessionFlag(%q) = %q", k.Args, f)
		}
	}
	for _, k := range c.Continue {
		out, dropped := continueSession(slices.Clone(k.Args), k.Last)
		if !slices.Equal(out, k.Out) || dropped != k.Dropped {
			t.Errorf("%q, %q: %q %v", k.Args, k.Last, out, dropped)
		}
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("APPDATA", filepath.Join(home, "AppData"))
	a, b, id := filepath.Join(home, "a"), filepath.Join(home, "b"), "0c3b5a52-4f0e-4c4b-9d7e-2b1f1a6e9c01"
	if lastSession(a) != "" {
		t.Fatal("session without a record")
	}
	saveLastSession(a, id)
	saveLastSession(b, "--dangerously-skip-permissions") // ni ID seje: ne zapiše
	if lastSession(a) != id || lastSession(b) != "" {
		t.Fatalf("records: %q %q", lastSession(a), lastSession(b))
	}
	os.Mkdir(a, 0o700) // L3: zapis mape, ki je ni več, gre ob naslednjem zapisu
	os.Mkdir(b, 0o700)
	saveLastSession(b, id)
	saveLastSession(filepath.Join(home, "gone"), id)
	os.Remove(filepath.Join(home, "gone"))
	saveLastSession(b, "9d1e0f3a-1111-4222-8333-444455556666")
	if m := loadLastSessions(); len(m) != 2 || m[a] != id || m[b] != "9d1e0f3a-1111-4222-8333-444455556666" {
		t.Fatalf("prune: %v", m)
	}
	if fi, err := os.Stat(lastSessionFile()); err != nil || runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("file mode: %v %v", fi, err)
	}
	os.WriteFile(lastSessionFile(), []byte(`{"`+strings.ReplaceAll(b, `\`, `\\`)+`":"-x"}`), 0o600)
	if lastSession(b) != "" {
		t.Fatal("invalid ID in the file passed on")
	}
}

// fakeClaudeMain: testni program kot »claude« (kopija z imenom claude): »--parent prog args…« požene minigw kot otrok
// claude (prednik, ne CLAUDECODE); sicer je to claude, ki ga zažene minigw: zapiše argumente v MINIGW_TEST_CLAUDE_LOG.
func fakeClaudeMain() {
	if len(os.Args) > 2 && os.Args[1] == "--parent" {
		cmd := exec.Command(os.Args[2], os.Args[3:]...)
		cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
		err := cmd.Run()
		if e, ok := err.(*exec.ExitError); ok {
			os.Exit(e.ExitCode())
		}
		if err != nil {
			os.Exit(125)
		}
		os.Exit(0)
	}
	os.WriteFile(os.Getenv("MINIGW_TEST_CLAUDE_LOG"), []byte(strings.Join(os.Args[1:], " ")), 0o600)
	if s := os.Getenv("MINIGW_TEST_CLAUDE_SIGNAL"); s != "" { // L-b: claude se konča s signalom (TERM) ali se ustavi (STOP)
		p, _ := os.FindProcess(os.Getpid())
		if s == "STOP" && fakeStop != nil {
			p.Signal(fakeStop)
			os.Exit(0) // po SIGCONT
		}
		p.Signal(syscall.SIGTERM)
		time.Sleep(5 * time.Second)
	}
	os.Exit(0)
}

// fakeStop: SIGSTOP, kjer ga test rabi (claudewait_linux_test.go).
var fakeStop os.Signal

// fakeClaudeDir: mapa z lažnim claude (kopija testnega programa) in dnevnik njegovih argumentov.
func fakeClaudeDir(t *testing.T) (dir, log string) {
	dir = t.TempDir()
	fake := filepath.Join(dir, "claude")
	if runtime.GOOS == "windows" {
		fake += ".exe"
	}
	b, err := os.ReadFile(os.Args[0])
	if err != nil || os.WriteFile(fake, b, 0o755) != nil {
		t.Fatal(err)
	}
	return dir, filepath.Join(dir, "claude.log")
}

// minigwCmd: »minigw args…« (testni program) z lažnim claude v PATH, brez CLAUDECODE; MINIGW_ALLOW_UNSAFE_FLAGS=1,
// ker test morda teče v Claude Code.
func minigwCmd(dir, log string, env []string, args ...string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(slices.DeleteFunc(os.Environ(), func(kv string) bool {
		return strings.HasPrefix(kv, "CLAUDECODE=") || strings.HasPrefix(strings.ToUpper(kv), "PATH=")
	}), append(env, "MINIGW_TEST_MAIN=1", "MINIGW_ALLOW_UNSAFE_FLAGS=1", "MINIGW_TEST_CLAUDE_LOG="+log, "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))...)
	return cmd
}

// L-b: claude, ki ga konča signal, konča minigw z istim signalom (ne 128+n).
func TestClaudeSignalExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no signals")
	}
	dir, log := fakeClaudeDir(t)
	err := minigwCmd(dir, log, []string{"MINIGW_TEST_CLAUDE_SIGNAL=TERM"}, "-h").Run()
	ee, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("minigw: %v", err)
	}
	if ws := ee.Sys().(syscall.WaitStatus); !ws.Signaled() || ws.Signal() != syscall.SIGTERM {
		t.Fatalf("minigw ended with %v, not SIGTERM", ws)
	}
}

// #79: »env -u CLAUDECODE minigw … --continue« pod pravim procesom claude minigw zavrne, preden zažene claude;
// MINIGW_ALLOW_UNSAFE_FLAGS=1 ga zažene.
func TestClaudeAncestorRefusesBeforeClaude(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "claude")
	if runtime.GOOS == "windows" {
		fake += ".exe"
	}
	b, err := os.ReadFile(os.Args[0])
	if err != nil || os.WriteFile(fake, b, 0o755) != nil {
		t.Fatal(err)
	}
	log := filepath.Join(dir, "claude.log")
	run := func(extra []string, args ...string) (string, bool) {
		cmd := exec.Command(fake, append([]string{"--parent", os.Args[0]}, args...)...)
		if slices.Contains(extra, "direct") { // brez lažnega starša claude; »direct« ni spremenljivka okolja (beseda brez »=« v bloku okolja Windows)
			cmd = exec.Command(os.Args[0], args...)
			extra = slices.DeleteFunc(slices.Clone(extra), func(s string) bool { return s == "direct" })
		}
		cmd.Env = append(slices.DeleteFunc(os.Environ(), func(kv string) bool {
			return strings.HasPrefix(kv, "CLAUDECODE=") || strings.HasPrefix(kv, "MINIGW_ALLOW_UNSAFE_FLAGS=") || strings.HasPrefix(strings.ToUpper(kv), "PATH=")
		}), append(extra, "MINIGW_TEST_MAIN=1", "MINIGW_TEST_CLAUDE_LOG="+log, "PATH="+dir+string(os.PathListSeparator)+os.Getenv("PATH"))...)
		os.Remove(log)
		out, err := cmd.CombinedOutput()
		if err != nil {
			out = append(out, " ("+err.Error()+")"...)
		}
		_, err = os.Stat(log)
		return string(out), err == nil
	}
	for _, a := range [][]string{{"-h", "--continue"}, {"-p", "--continue", "x"}, {"-p", "-c", "x"}, {"-p", "--resume", "0c3b5a52-4f0e-4c4b-9d7e-2b1f1a6e9c01", "x"}} {
		out, spawned := run(nil, a...)
		if spawned || !strings.Contains(out, "is not accepted when minigw runs inside Claude Code") {
			t.Errorf("%q: spawned=%v %s", a, spawned, out)
		}
	}
	if out, spawned := run([]string{"MINIGW_ALLOW_UNSAFE_FLAGS=1"}, "-h", "--continue"); !spawned {
		t.Errorf("override: claude not started: %s", out)
	}
	// kontrola: brez starša claude (test ne teče v Claude Code, npr. CI) minigw claude zažene
	if !claudeCaller(os.Getpid(), procLookup(), false) {
		if out, spawned := run([]string{"direct"}, "-h", "--continue"); !spawned {
			t.Errorf("without a claude parent: claude not started: %s", out)
		}
	}
}

type toolsCases struct {
	Parse []struct {
		Args  []string
		Rest  []string
		Value string
		Error string
	}
	Plan []struct {
		Model, Flag string
		Ro          bool
		Tools       string
		Error       string
	}
	ClaudeArgs []struct {
		Tools string
		Args  []string
		Out   []string
	}
	Line     []struct{ Tools, Line string }
	FolderRo []struct {
		Model, Flag     string
		FlagRo, SavedRo bool
		Tools           string
	}
	Widen []struct {
		Args []string
		Flag string
	}
}

func loadToolsCases(t *testing.T) toolsCases {
	var c toolsCases
	if err := json.Unmarshal(srcFile(t, "testdata/minigw-tools-cases.json"), &c); err != nil || len(c.Plan) == 0 {
		t.Fatal(err)
	}
	return c
}

// #119: zastavica --minigw-tools (oba zapisa, napačna vrednost), privzeto pri --minigw-ro, argumenti claude, vrstica ob zagonu.
func TestMinigwTools(t *testing.T) {
	c := loadToolsCases(t)
	for _, p := range c.Parse {
		rest, value, msg := takeTools(append([]string(nil), p.Args...))
		if !slices.Equal(rest, p.Rest) || value != p.Value || msg != p.Error {
			t.Errorf("parse %v: got %v %q %q", p.Args, rest, value, msg)
		}
	}
	for _, p := range c.Plan {
		if tools, msg := toolsFor(p.Model, p.Flag, p.Ro); tools != p.Tools || msg != p.Error {
			t.Errorf("plan %+v: got %q %q", p, tools, msg)
		}
	}
	for _, p := range c.ClaudeArgs {
		if out := toolsArgs(p.Tools, p.Args); !slices.Equal(out, p.Out) {
			t.Errorf("claude args %+v: got %v", p, out)
		}
	}
	for _, p := range c.Line {
		if l := toolsLine(p.Tools); l != p.Line {
			t.Errorf("line %q", l)
		}
	}
	for _, p := range c.FolderRo { // R3: shranjena nastavitev mape samo za branje šteje kot --minigw-ro (workMode)
		_, ro := workMode("", &sharedFolder{ReadOnly: p.SavedRo}, p.FlagRo)
		if tools, msg := toolsFor(p.Model, p.Flag, ro); tools != p.Tools || msg != "" {
			t.Errorf("folderRo %+v: got %q %q", p, tools, msg)
		}
	}
	for _, p := range c.Widen { // R1: z omejitvijo ostane le to, kar ne širi orodij; tudi zunaj Claude Code
		if f := toolsWiden(p.Args); f != p.Flag {
			t.Errorf("widen %v: got %q, want %q", p.Args, f, p.Flag)
		}
	}
}

// #119: --minigw-tools sme pod Claude Code (le jemlje orodja); --tools in dovoljenja ostanejo zavrnjeni.
func TestMinigwToolsInsideClaudeCode(t *testing.T) {
	inside := func(k string) string { return map[string]string{"CLAUDECODE": "1"}[k] }
	rest, value, msg := takeTools([]string{"-p", "--model", "litellm-gpt", "--minigw-tools=read", "x"})
	if msg != "" || value != "read" || unsafeClaudeFlag(rest, inside) != "" {
		t.Errorf("not accepted inside Claude Code: %v %q %q", rest, value, msg)
	}
	for _, a := range []string{"--tools", "--allowedTools", "--permission-mode", "--dangerously-skip-permissions"} {
		if unsafeClaudeFlag([]string{"-p", a, "x"}, inside) != a {
			t.Errorf("%s no longer refused", a)
		}
	}
	if unsafeClaudeFlag([]string{"-p", "--minigw-tools=read"}, inside) == "" {
		t.Error("the flag must be taken out before the check, not passed to claude")
	}
	if modelArg([]string{"-p", "--model=a", "--model", "b"}) != "b" || modelArg([]string{"--", "--model", "x"}) != "" {
		t.Error("modelArg")
	}
}

// #119: pod Claude Code (CLAUDECODE) minigw sprejme --minigw-tools, zavrne napačno vrednost in model agenta ter še naprej
// --tools; vse pred zagonom claude; temp HOME, brez prave nastavitve.
func TestMinigwToolsRefusals(t *testing.T) {
	dir, log := fakeClaudeDir(t)
	home := t.TempDir()
	run := func(args ...string) string {
		cmd := minigwCmd(dir, log, []string{"CLAUDECODE=1", "HOME=" + home, "XDG_CONFIG_HOME=" + home, "APPDATA=" + home, "USERPROFILE=" + home}, args...)
		cmd.Env = append(slices.DeleteFunc(cmd.Env, func(kv string) bool { return strings.HasPrefix(kv, "MINIGW_ALLOW_UNSAFE_FLAGS=") }), "MINIGW_ALLOW_UNSAFE_FLAGS=0")
		out, _ := cmd.CombinedOutput()
		return string(out)
	}
	for args, want := range map[string]string{
		"-p --model litellm-gpt --minigw-tools=all x":       `--minigw-tools: unknown value "all" (use none, read or edit)`,
		"-p --model claude-cli-opus --minigw-tools none x":  "agents are limited on the gateway, use --minigw-ro",
		"-p --model litellm-gpt --tools Bash --minigw-ro x": "--tools is not accepted when minigw runs inside Claude Code",
		"-p --model litellm-gpt --minigw-tools=read @x":     "paste the text itself into the prompt, or pass it on stdin",
	} {
		if out := run(strings.Fields(args)...); !strings.Contains(out, want) {
			t.Errorf("%s: %s", args, out)
		}
	}
	// The accepted call must not go on to start claude: on Windows the profiles live in Credential Manager, which the temp HOME
	// does not isolate, so a connected machine would run claude here. An unknown profile stops it right after the tools checks.
	if out := run("-p", "--model", "litellm-gpt", "--minigw-tools=read", "--minigw-profile", "no-such-profile", "x"); strings.Contains(out, "not accepted") ||
		strings.Contains(out, "unknown value") || !strings.Contains(out, "no profile no-such-profile") {
		t.Errorf("--minigw-tools refused inside Claude Code (or did not reach the profile lookup): %s", out)
	}
	// R1: tudi zunaj Claude Code (MINIGW_ALLOW_UNSAFE_FLAGS=1) omejitev ne sme dobiti širših možnosti za sabo
	for _, args := range []string{"-p --model litellm-gpt --minigw-tools=read --allowedTools Bash x", "-p --model litellm-gpt --minigw-ro --tools default x",
		"-p --minigw-tools none --mcp-config m.json x"} {
		cmd := minigwCmd(dir, log, []string{"HOME=" + home, "XDG_CONFIG_HOME=" + home, "APPDATA=" + home, "USERPROFILE=" + home}, strings.Fields(args)...)
		if out, _ := cmd.CombinedOutput(); !strings.Contains(string(out), "is not accepted together with --minigw-tools") {
			t.Errorf("%s: %s", args, out)
		}
	}
	if _, err := os.Stat(log); err == nil {
		t.Error("claude was started")
	}
}

// Owner's cap on tools per provider and model: client/testdata/provider-tools.json, asserted by Swift (ProviderToolsTests) too.
func TestProviderToolsCap(t *testing.T) {
	var f struct {
		Caps []struct {
			Tools      string
			ModelTools map[string]string
			Route, Cap string
		}
		Levels []struct{ Level, Cap, Want string }
		Lines  []struct {
			Name, Tools string
			Models      map[string]string
			Model       string
			Want        []string
		}
		Lists []struct {
			List    map[string]any
			Model   string
			Refusal string
		}
	}
	if err := json.Unmarshal(srcFile(t, "testdata/provider-tools.json"), &f); err != nil || len(f.Caps) == 0 {
		t.Fatal(err)
	}
	for _, c := range f.Caps {
		if got := (serveGateway{Tools: c.Tools, ModelTools: c.ModelTools}).toolsCap(c.Route); got != c.Cap {
			t.Errorf("cap %+v: got %q", c, got)
		}
	}
	for _, c := range f.Levels {
		if got := toolsLevelRefusal(c.Level, c.Cap, "xAI", "grok-4"); got != c.Want {
			t.Errorf("level %+v: got %q", c, got)
		}
	}
	for _, c := range f.Lines {
		if got := providerToolsLines(c.Name, c.Tools, c.Models, c.Model); !slices.Equal(got, c.Want) {
			t.Errorf("lines %+v: got %q", c, got)
		}
	}
	for _, c := range f.Lists {
		if got := editRefusal(c.List, c.Model); got != c.Refusal {
			t.Errorf("list %+v: got %q", c, got)
		}
	}
}
