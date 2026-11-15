package main

import "testing"

func TestCommandsThatCreateStateAreRefused(t *testing.T) {
	cases := []struct {
		arg0 string
		args []string
		want bool
	}{
		{"minigw.exe", []string{"serve"}, true},
		{"minigw.exe", []string{"tray"}, true},
		{"minigw.exe", []string{"connect", "host", "--token", "x"}, true},
		{"minigw.exe", []string{"share", "add"}, true},
		{"minigw.exe", []string{"run", "--", "claude"}, true},
		{"minigw.exe", []string{"--minigw-profile", "work", "connect", "host"}, true},
		{"minigw.exe", []string{"--minigw-profile=work", "run"}, true},
		{"minigw-tray.exe", nil, true},
		{"minigw.exe", []string{"status"}, false},
		{"minigw.exe", []string{"models"}, false},
		{"minigw.exe", nil, true},
		{"minigw.exe", []string{"-p", "hello"}, true},
		{"minigw.exe", []string{"--model", "example"}, true},
		{"minigw.exe", []string{"--minigw-profile", "work"}, true},
		{"minigw.exe", []string{"--help"}, false},
		{"minigw.exe", []string{"--minigw-help"}, false},
		{"minigw.exe", []string{"--version"}, false},
		{"minigw.exe", []string{"profiles", "default", "work"}, true},
		{"minigw.exe", []string{"profiles", "--minigw-profile", "work", "default", "work"}, true},
		{"minigw.exe", []string{"profiles"}, false},
		{"minigw.exe", []string{"profiles", "list"}, false},
		{"minigw.exe", []string{"project", "link", "example"}, true},
		{"minigw.exe", []string{"project", "unlink"}, true},
		{"minigw.exe", []string{"project", "list"}, false},
		{"minigw.exe", []string{"project", "show"}, false},
		{"minigw.exe", []string{"reconnect"}, true},
		{"minigw.exe", []string{"plugin", "install"}, true},
		{"minigw.exe", []string{"plugin", "status"}, false},
		{"minigw.exe", []string{"note", "edit", "example"}, true},
		{"minigw.exe", []string{"note", "list"}, false},
		{"minigw.exe", []string{"note", "read", "example"}, false},
		{"minigw.exe", []string{"usage"}, false},
		{"minigw.exe", []string{"gw", "token"}, true},
	}
	for _, c := range cases {
		if got := createsState(c.arg0, c.args); got != c.want {
			t.Errorf("createsState(%q, %v) = %v, want %v", c.arg0, c.args, got, c.want)
		}
	}
}
