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
		{"minigw.exe", nil, false},
	}
	for _, c := range cases {
		if got := createsState(c.arg0, c.args); got != c.want {
			t.Errorf("createsState(%q, %v) = %v, want %v", c.arg0, c.args, got, c.want)
		}
	}
}
