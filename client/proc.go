package main

import (
	"context"
	"os/exec"
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
	hideConsole(cmd)
	return cmd
}

// consoleCommand: interaktivni otrok v terminalu (runClaude) deli konzolo starša, zato ostane brez hideConsole.
func consoleCommand(name string, args ...string) *exec.Cmd { return exec.Command(name, args...) }
