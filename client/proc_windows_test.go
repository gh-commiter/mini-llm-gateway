package main

import (
	"context"
	"os/exec"
	"testing"
)

func TestCommandHidesConsole(t *testing.T) {
	for _, cmd := range []*exec.Cmd{command("git", "--version"), commandContext(context.Background(), "git")} {
		if a := cmd.SysProcAttr; a == nil || a.HideWindow || a.CreationFlags&createNoWindow == 0 {
			t.Fatalf("SysProcAttr %+v: want CREATE_NO_WINDOW without HideWindow (GUI children stay visible)", a)
		}
	}
	cmd := command("x")
	cmd.SysProcAttr.CreationFlags |= 0x4 // CREATE_SUSPENDED (startAgent)
	hideConsole(cmd)
	if cmd.SysProcAttr.CreationFlags != createNoWindow|0x4 {
		t.Fatalf("flags 0x%x: existing flags must stay", cmd.SysProcAttr.CreationFlags)
	}
}
