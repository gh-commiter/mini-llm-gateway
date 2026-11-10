package main

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// Vsak zunanji ukaz se ustvari tu (proc_test.go to preveri): na Windows brez okna konzole (hideConsole), ker
// minigw-tray.exe nima konzole in bi vsak otrok sicer dobil svoje vidno okno.

func command(name string, args ...string) *exec.Cmd {
	cmd := exec.Command(name, args...)
	hideConsole(cmd)
	return cmd
}

func commandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = 5 * time.Second // N1: Output() ne obvisi, ko vnuk drži cev (Linux startAgent: 10 s)
	hideConsole(cmd)
	return cmd
}

// consoleCommand: interaktivni otrok v terminalu (runClaude) deli konzolo starša, zato ostane brez hideConsole.
func consoleCommand(name string, args ...string) *exec.Cmd { return exec.Command(name, args...) }

// mergePath: seznami PATH (ločilo sep) v enega, vsaka mapa enkrat (brez razlike v velikosti črk in končnem »\«).
func mergePath(sep string, lists ...string) string {
	var out []string
	seen := map[string]bool{}
	for _, l := range lists {
		for _, p := range strings.Split(l, sep) {
			k := strings.ToLower(strings.TrimRight(strings.TrimSpace(p), `\/`))
			if k != "" && !seen[k] {
				seen[k] = true
				out = append(out, strings.TrimSpace(p))
			}
		}
	}
	return strings.Join(out, sep)
}

// lookPath: program na PATH ali ""; tudi ob exec.ErrDot (najden v trenutni mapi ali prek relativne poti) "".
func lookPath(name string) string {
	p, err := exec.LookPath(name)
	if err != nil {
		return ""
	}
	return p
}
