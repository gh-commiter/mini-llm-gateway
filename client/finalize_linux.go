package main

import (
	"os"
	"os/exec"
	"sync"
)

// finalizeBwrap (N1): bwrap za git, ki ob finalize bere ugnezden klon; "" = ga ni ali ne deluje (ugnezdeni tedaj ne gredo nazaj).
var finalizeBwrap = sync.OnceValue(func() string {
	p, err := exec.LookPath("bwrap")
	t, err2 := exec.LookPath("true")
	if err != nil || err2 != nil || command(p, "--ro-bind", "/", "/", "--unshare-pid", "--die-with-parent", "--", t).Run() != nil {
		return ""
	}
	return p
})

// boxGit (N1): git pod bwrap, kjer je delovno drevo ugnezdenega klona pripeto iz odprte mape (fd 3, le za branje) na
// mount: zamenjava poti s povezavo po odprtju ne preusmeri ničesar. Pisljivi le zrcalo in začasna mapa; svoj imenski
// prostor PID z --die-with-parent.
func boxGit(dir *os.File, mount, repo, tmp, exe string, args []string) (string, []string, []*os.File, bool) {
	bw := finalizeBwrap()
	if bw == "" {
		return "", nil, nil, false
	}
	a := []string{"--ro-bind", "/", "/", "--dev", "/dev", "--proc", "/proc", "--bind", repo, repo, "--bind", tmp, tmp,
		"--ro-bind-fd", "3", mount, "--unshare-pid", "--die-with-parent", "--", exe}
	return bw, append(a, args...), []*os.File{dir}, true
}
