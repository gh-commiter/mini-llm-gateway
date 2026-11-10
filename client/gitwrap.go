package main

// Ovoj git v škatli MXC na Windows (#68): minigw.exe, kopiran v <Scratch>\gitexec\win-<zgoščena>\bin\git.exe, je prvi na
// PATH agenta. Pravilo 1 iz pathWrapScript (pathWrapDeps): git init in git clone le pod $MINIGW_DEPS, oddaljen clone
// ne, ko je omrežje gradenj izklopljeno; vse drugo gre nespremenjeno pravemu gitu (MINIGW_REAL_GIT). Pravili 2 in 3
// (GIT_DIR klona) na Windows nista potrebni: ves agent je v škatli. Kot pri Codex na Macu je ovoj navodilo
// peskovniškemu klicatelju (pravi git po polni poti ga obide); repozitorij v klonu vseeno ujame foreignGit/nestedProblem.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
)

// gitWrapRefusal: razlog zavrnitve (brez »minigw: «) ali "" = git sme teči. d: trenutna mapa, args: argumenti gita.
func gitWrapRefusal(d string, args []string, env func(string) string) string {
	i := 0
	for ; i < len(args) && strings.HasPrefix(args[i], "-"); i++ { // podukaz za globalnimi možnostmi
		switch a := args[i]; {
		case a == "-C" && i+1 < len(args):
			i++
			p := args[i]
			if !filepath.IsAbs(p) {
				p = filepath.Join(d, p)
			}
			d = filepath.Clean(p)
		case slices.Contains([]string{"-c", "--git-dir", "--work-tree", "--namespace", "--config-env"}, a):
			i++
		}
	}
	if i >= len(args) || args[i] != "init" && args[i] != "clone" {
		return ""
	}
	c := args[i]
	deps := env("MINIGW_DEPS")
	if deps == "" {
		return "this run cannot write (read-only)"
	}
	var u, t string
	sep := false
	for i++; i < len(args); i++ {
		a := args[i]
		switch {
		case strings.HasPrefix(a, "--separate-git-dir"):
			sep = true
		case slices.Contains([]string{"-b", "-c", "-j", "-o", "-u", "--revision", "--template", "--initial-branch", "--object-format", "--ref-format",
			"--origin", "--branch", "--upload-pack", "--reference", "--reference-if-able", "--depth", "--shallow-since", "--shallow-exclude",
			"--config", "--server-option", "--jobs", "--filter", "--bundle-uri"}, a):
			i++
		case strings.HasPrefix(a, "-"):
		case u == "":
			u = a
		case t == "":
			t = a
		}
	}
	if c == "init" {
		if t = u; t == "" {
			t = "."
		}
	} else if t == "" { // ime iz URL kot git: zadnji del brez .git
		t = strings.TrimSuffix(strings.TrimSuffix(strings.TrimRight(u, `/\`), "/.git"), `\.git`)
		if k := strings.LastIndexAny(t, `/\:`); k >= 0 {
			t = t[k+1:]
		}
		t = strings.TrimSuffix(t, ".git")
	}
	if !filepath.IsAbs(t) {
		t = filepath.Join(d, t)
	}
	if slices.Contains(strings.FieldsFunc(t, func(r rune) bool { return r == '/' || r == '\\' }), "..") {
		sep = true
	}
	p := filepath.Clean(t)
	for {
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			break
		}
		up := filepath.Dir(p)
		if up == p {
			break
		}
		p = up
	}
	rd, err1 := filepath.EvalSymlinks(deps)
	rp, err2 := filepath.EvalSymlinks(p)
	if sep || err1 != nil || err2 != nil || !under(rp, rd) {
		return "create repositories only under $MINIGW_DEPS (" + deps + "), e.g. cmake -DFETCHCONTENT_BASE_DIR=\"$MINIGW_DEPS/cmake\", swift build --scratch-path \"$MINIGW_DEPS/swiftpm\""
	}
	if env("MINIGW_SANDBOXED") != "1" && env("SANDBOX_RUNTIME") != "1" {
		return "git init and git clone need the agent sandbox, which this run does not have"
	}
	if off := env("MINIGW_NETWORK_OFF"); c == "clone" && off != "" && remoteGitURL(u) {
		return "network for this agent's builds is off (" + off + ")"
	}
	return ""
}

// remoteGitURL: URL z »://« (razen file://) ali oblika scp »gostitelj:pot«; lokalne poti (tudi C:\…) ne.
func remoteGitURL(u string) bool {
	switch {
	case strings.HasPrefix(u, "file://"), strings.HasPrefix(u, "/"), strings.HasPrefix(u, "."), strings.HasPrefix(u, `\`):
		return false
	case strings.Contains(u, "://"):
		return true
	case len(u) >= 2 && u[1] == ':' && (len(u) == 2 || u[2] == '\\' || u[2] == '/'): // pogon Windows
		return false
	}
	h, _, ok := strings.Cut(u, ":")
	return ok && !strings.ContainsAny(h, `/\`)
}

// gitWrapMain: minigw.exe, zagnan kot git.exe (main): pravilo, nato pravi git z istimi argumenti, stdin/stdout in izhodom.
func gitWrapMain(args []string) {
	d, _ := os.Getwd()
	if why := gitWrapRefusal(d, args, os.Getenv); why != "" {
		fmt.Fprintln(os.Stderr, "minigw: "+why)
		os.Exit(128)
	}
	real := os.Getenv("MINIGW_REAL_GIT")
	if real == "" {
		fmt.Fprintln(os.Stderr, "minigw: MINIGW_REAL_GIT is not set (git wrapper of the gateway)")
		os.Exit(128)
	}
	cmd := command(real, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		var e *exec.ExitError
		if errors.As(err, &e) {
			os.Exit(e.ExitCode())
		}
		fmt.Fprintln(os.Stderr, "minigw: "+err.Error())
		os.Exit(128)
	}
	os.Exit(0)
}

// isGitWrap: ta program teče kot ovoj git (ime git.exe).
func isGitWrap() bool {
	exe, err := os.Executable()
	return err == nil && strings.EqualFold(filepath.Base(exe), "git.exe")
}

var selfHash = sync.OnceValue(func() string {
	f, err := os.Open(minigwExe())
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
})

// gitWrapBin: mapa z git.exe (kopija minigw.exe) v gw, po zgoščeni vrednosti programa (tekoči agent drži staro kopijo
// odprto; nova različica minigw dobi svojo mapo). "" = ni mogoče.
func gitWrapBin(gw string) string {
	gitWrapMu.Lock()
	defer gitWrapMu.Unlock()
	h := selfHash()
	if h == "" {
		return ""
	}
	bin := filepath.Join(gw, "win-"+h, "bin")
	dst := filepath.Join(bin, "git.exe")
	if st, err := os.Lstat(dst); err == nil && st.Mode().IsRegular() {
		return bin
	}
	in, err := os.Open(minigwExe())
	if err != nil || os.MkdirAll(bin, 0o700) != nil {
		return ""
	}
	defer in.Close()
	tmp, err := os.CreateTemp(bin, ".git-*")
	if err != nil {
		return ""
	}
	_, err = io.Copy(tmp, in)
	err = errors.Join(err, tmp.Close())
	if err == nil {
		err = os.Rename(tmp.Name(), dst)
	}
	if err != nil {
		os.Remove(tmp.Name())
		return ""
	}
	return bin
}
