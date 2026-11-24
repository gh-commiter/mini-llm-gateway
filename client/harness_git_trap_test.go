package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// #187: in direct and sync mode the agent harness (Claude CLI with local tools, Codex, Grok) runs git itself, outside its sandbox, in the client's folder,
// whose .git/config, .git/info/attributes, .gitattributes and .gitmodules the client writes. A scratch repository carries a trap in every place git could run a
// command from; a trap only creates a marker file in the test's temp folder. The harness environment comes from the function the gateway uses at launch.
// The same repository as HarnessGitTrapTests.swift (TrapRepo.setup).

const trapSetup = `set -e
g() { git -c core.hooksPath=/dev/null "$@"; }
g init -q . && g config user.name t && g config user.email t@example.com
printf 'a\n' > a.txt; printf 'b\n' > b.dat; printf 'c\n' > c.inc; printf 'd\n' > d.txt
g add . && g commit -qm one
printf 'A\n' > a.txt; printf 'B\n' > b.dat; printf '*.dat filter=p diff=y\n*.inc filter=z diff=y\n' > .gitattributes
g add . && g commit -qm two
signed=$(g cat-file commit HEAD | awk '{print} /^committer /{print "gpgsig -----BEGIN PGP SIGNATURE-----"; print " x"; print " -----END PGP SIGNATURE-----"}' | g hash-object -t commit -w --stdin)
g update-ref HEAD "$signed"
printf 'C\n' > c.inc && g add c.inc
printf 'Z\n' > a.txt; printf 'Y\n' > b.dat; touch -t 200001010000 a.txt b.dat
printf '[submodule "s"]\n\tpath = s\n\turl = ./nowhere\n\tupdate = !touch "%s/gitmodules.update"\n' "$M" > .gitmodules
mkdir -p .git/info && printf '*.txt filter=x diff=x\n' > .git/info/attributes
for h in post-index-change pre-commit pre-push post-checkout reference-transaction; do printf '#!/bin/sh\ntouch "%s/hook.%s"\n' "$M" "$h" > "$H/$h"; chmod +x "$H/$h"; done
printf '#!/bin/sh\ntouch "%s/fsmonitor"\n' "$M" > "$H/fsmonitor"; chmod +x "$H/fsmonitor"
c() { git config "$1" "$2"; }
c filter.x.clean "touch '$M/filter.clean'; cat"
c filter.x.smudge "touch '$M/filter.smudge'; cat"
c filter.p.process "touch '$M/filter.process'; exit 1"
c diff.external "touch '$M/diff.external'; true"
c diff.x.textconv "touch '$M/diff.textconv'; cat"
c core.fsmonitor "$H/fsmonitor"
c core.hooksPath "$H"
c core.pager "touch '$M/core.pager'; cat"
c core.sshCommand "touch '$M/core.sshCommand'; false"
c gpg.program "$H/gpg"; printf '#!/bin/sh\ntouch "%s/gpg.program"\n' "$M" > "$H/gpg"; chmod +x "$H/gpg"
c log.showSignature true
c core.alternateRefsCommand "touch '$M/core.alternateRefsCommand'; true"
c include.path included.cfg
printf '[diff "y"]\n\ttextconv = touch \\"%s/include.textconv\\"; cat\n[filter "z"]\n\tclean = touch \\"%s/include.filter.clean\\"; cat\n' "$M" "$M" > .git/included.cfg
`

var trapCommands = [][]string{{"status"}, {"diff"}, {"diff", "--cached"}, {"log", "-p", "-1"}, {"show"}, {"add", "-n", "."}, {"stash", "list"}}

type trapRepo struct{ base, repo, markers string }

func newTrapRepo(t *testing.T) trapRepo {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the Windows gateway: see the note in agentHarnessGitEnv")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	base, _ := filepath.EvalSymlinks(t.TempDir())
	tr := trapRepo{base, filepath.Join(base, "r"), filepath.Join(base, "m")}
	for _, d := range []string{tr.repo, tr.markers, filepath.Join(base, "home"), filepath.Join(base, "hooks")} {
		os.MkdirAll(d, 0o700)
	}
	cmd := exec.Command("/bin/sh", "-c", trapSetup)
	cmd.Dir, cmd.Env = tr.repo, append(tr.baseEnv(), "M="+tr.markers, "H="+filepath.Join(base, "hooks"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("setup: %v %s", err, out)
	}
	return tr
}

func (tr trapRepo) baseEnv() []string {
	return []string{"PATH=" + os.Getenv("PATH"), "HOME=" + filepath.Join(tr.base, "home"), "GIT_CONFIG_NOSYSTEM=1"}
}

// fired runs every command with env over the base environment and gives, per command, the traps that fired.
func (tr trapRepo) fired(env []string) map[string][]string {
	out := map[string][]string{}
	for _, c := range trapCommands {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		cmd := exec.CommandContext(ctx, "git", c...)
		cmd.Dir, cmd.Env = tr.repo, append(slices.Clone(tr.baseEnv()), env...)
		cmd.Run()
		cancel()
		ents, _ := os.ReadDir(tr.markers)
		var hit []string
		for _, e := range ents {
			hit = append(hit, e.Name())
			os.Remove(filepath.Join(tr.markers, e.Name()))
		}
		if len(hit) > 0 {
			out[strings.Join(c, " ")] = hit
		}
	}
	return out
}

// The traps work (control): git with no protection runs them.
func TestGitTrapsFireWithoutProtection(t *testing.T) {
	tr := newTrapRepo(t)
	f := tr.fired(nil)
	all := map[string]bool{}
	for _, h := range f {
		for _, x := range h {
			all[x] = true
		}
	}
	for _, trap := range []string{"filter.clean", "diff.external", "diff.textconv", "gpg.program", "include.filter.clean"} /* include.textconv fires with the Mac's git, not with the Linux image's */ {
		if !all[trap] {
			t.Errorf("%s never fired: %v", trap, f)
		}
	}
}

// #187: the harness of Claude CLI (direct and sync with local tools), Codex and Grok (direct and sync) runs nothing from the client's repository.
func TestAgentHarnessRunsNothingFromTheClientsRepository(t *testing.T) {
	tr := newTrapRepo(t)
	for name, env := range map[string][]string{
		"claude":           claudeHarnessGitEnv(tr.repo, filepath.Join(tr.base, "env.sh")),
		"claude, no tools": claudeHarnessGitEnv(tr.repo, ""), // M1
		"codex":            agentHarnessGitEnv(tr.repo),
		"grok":             agentHarnessGitEnv(tr.repo),
	} {
		if f := tr.fired(env); len(f) > 0 {
			t.Errorf("%s: traps fired: %v", name, f)
		}
	}
}

// #187: the agent's Bash (Claude Code runs CLAUDE_ENV_FILE ahead of each command) and Codex's commands (shell_environment_policy) find the folder's repository;
// only the harness has none. Grok's commands are told to run `env -u GIT_DIR git` (grokGitNote).
func TestAgentCommandsKeepTheFoldersRepository(t *testing.T) {
	tr := newTrapRepo(t)
	file := filepath.Join(tr.base, "env.sh")
	os.WriteFile(file, []byte(bashGitEnvScript), 0o600)
	env := append(tr.baseEnv(), claudeHarnessGitEnv(tr.repo, file)...)
	run := func(argv ...string) (string, error) {
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Dir, cmd.Env = tr.repo, env
		out, err := cmd.CombinedOutput()
		return strings.TrimSpace(string(out)), err
	}
	if out, err := run("git", "rev-parse", "--show-toplevel"); err == nil {
		t.Errorf("the harness found a repository: %s", out)
	}
	for _, argv := range [][]string{{"/bin/sh", "-c", `. "$CLAUDE_ENV_FILE"; git rev-parse --show-toplevel`}, {"env", "-u", "GIT_DIR", "git", "rev-parse", "--show-toplevel"}} {
		if out, err := run(argv...); err != nil || out != tr.repo {
			t.Errorf("%v: %v %s", argv, err, out)
		}
	}
	if !strings.Contains(grokGitNote, "env -u GIT_DIR git") || !strings.Contains(strings.Join(codexHarnessExclude, " "), `["GIT_DIR","GIT_CEILING_DIRECTORIES"]`) {
		t.Error("the commands' way back to the repository changed")
	}
}

var fakeHarnessGitEnv = fake("harness-git-env", func(p fakeParams, args []string) {
	fakeWrite(p["env"], strings.Join(os.Environ(), "\n")+"\n")
	b, _ := os.ReadFile(os.Getenv("CLAUDE_ENV_FILE"))
	fakeWrite(p["env"]+".script", string(b))
	fakeGuard(args)
	fakeStdin()
	fakeClaudeDone(1)
})

// #187: the launch of a sync-mode Claude CLI run (fake claude): the harness gets GIT_DIR=/dev/null; with local tools a CLAUDE_ENV_FILE gives its Bash the
// folder's repository back and is gone after the run; without local tools (M1: no Bash) the harness has no repository all the same and gets no file.
func TestServeSyncClaudeHarnessHasNoRepository(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the Windows gateway: see the note in agentHarnessGitEnv")
	}
	for _, localTools := range []bool{true, false} {
		t.Run(fmt.Sprintf("localTools=%v", localTools), func(t *testing.T) {
			tmp := t.TempDir()
			envFile := filepath.Join(tmp, "env")
			fake := fakeCLI(t, tmp, "claude", fakeHarnessGitEnv, fakeParams{"env": envFile})
			s, _ := testServer(t, func(c *serveConfig) { c.Claude, c.AllowLocalTools = fake, localTools })
			c := startLoopback(t, s)
			dir := t.TempDir()
			os.WriteFile(filepath.Join(dir, "src.txt"), []byte("x\n"), 0o644)
			sc := &shareClient{c: c, session: "sid1", root: dir, sync: true, synced: map[string]syncEntry{}, reported: map[string]bool{}}
			if !sc.register() {
				t.Fatal("register failed: " + sc.refusal)
			}
			sc.start(2)
			defer sc.unregister()
			h := map[string]string{"x-claude-mini-session": "sid1", "x-claude-mini-cwd": dir, "content-type": "application/json"}
			if st, data := call(c, "POST", "/v1/messages", h, []byte(`{"model":"claude-cli-haiku","tools":[{"name":"x"}],"messages":[{"role":"user","content":"go"}]}`), 30*time.Second); st != 200 {
				t.Fatalf("messages: %d %s", st, data)
			}
			env, _ := os.ReadFile(envFile)
			lines := strings.Split(string(env), "\n")
			if !slices.Contains(lines, "GIT_DIR="+os.DevNull) || !slices.ContainsFunc(lines, func(l string) bool { return strings.HasPrefix(l, "GIT_CEILING_DIRECTORIES=/") }) {
				t.Fatalf("the harness has a repository: %q", env)
			}
			want := ""
			if localTools {
				want = bashGitEnvScript
			}
			if script, _ := os.ReadFile(envFile + ".script"); string(script) != want {
				t.Fatalf("CLAUDE_ENV_FILE: %q", script)
			}
			hasFile := false
			for _, l := range lines {
				if f, ok := strings.CutPrefix(l, "CLAUDE_ENV_FILE="); ok {
					hasFile = true
					if _, err := os.Stat(f); err == nil {
						t.Errorf("%s stayed after the run", f)
					}
				}
			}
			if hasFile != localTools {
				t.Errorf("CLAUDE_ENV_FILE given: %v", hasFile)
			}
		})
	}
}
