package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"
)

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
		{"minigw.exe", []string{"status"}, true},
		{"minigw.exe", []string{"models"}, true},
		{"minigw.exe", nil, true},
		{"minigw.exe", []string{"-p", "hello"}, true},
		{"minigw.exe", []string{"--model", "example"}, true},
		{"minigw.exe", []string{"--minigw-profile", "work"}, true},
		{"minigw.exe", []string{"--help"}, false},
		{"minigw.exe", []string{"--minigw-help"}, false},
		{"minigw.exe", []string{"--version"}, false},
		{"minigw.exe", []string{"profiles", "default", "work"}, true},
		{"minigw.exe", []string{"profiles", "--minigw-profile", "work", "default", "work"}, true},
		{"minigw.exe", []string{"profiles"}, true},
		{"minigw.exe", []string{"profiles", "list"}, true},
		{"minigw.exe", []string{"project", "link", "example"}, true},
		{"minigw.exe", []string{"project", "unlink"}, true},
		{"minigw.exe", []string{"project", "list"}, true},
		{"minigw.exe", []string{"project", "show"}, true},
		{"minigw.exe", []string{"reconnect"}, true},
		{"minigw.exe", []string{"plugin", "install"}, true},
		{"minigw.exe", []string{"plugin", "status"}, true},
		{"minigw.exe", []string{"note", "edit", "example"}, true},
		{"minigw.exe", []string{"note", "list"}, true},
		{"minigw.exe", []string{"note", "read", "example"}, true},
		{"minigw.exe", []string{"usage"}, true},
		{"minigw.exe", []string{"gw", "token"}, true},
		{"minigw.exe", []string{"gw"}, false},
		{"minigw.exe", []string{"gw", "help"}, false},
		{"minigw.exe", []string{"router"}, false},
		{"minigw.exe", []string{"router", "help"}, false},
		{"minigw.exe", []string{"share"}, false},
		{"minigw.exe", []string{"share", "list"}, false},
		{"minigw.exe", []string{"gw", "status"}, true},
		{"minigw.exe", []string{"gw", "help", "--config", "other"}, true},
		{"minigw.exe", []string{"share", "list", "extra"}, true},
		{"minigw.exe", []string{"--help", "-p", "hello"}, true},
		{"minigw.exe", []string{"--version", "extra"}, true},
		{"minigw.exe", []string{"--minigw-profile", "work", "gw", "help"}, false},
		{"minigw.exe", []string{"share", "--minigw-profile=work", "list"}, false},
		{"minigw.exe", []string{"--help", "--minigw-profile"}, true},
		{"minigw-tray.exe", []string{"--help"}, true},
		{"minigw.exe", []string{"new-command"}, true},
	}
	for _, c := range cases {
		if got := createsState(c.arg0, c.args); got != c.want {
			t.Errorf("createsState(%q, %v) = %v, want %v", c.arg0, c.args, got, c.want)
		}
	}
}

// Keep this table exhaustive against main's dispatch, including internal helpers.
// A new command remains refused by default and requires an explicit test expectation.
func TestOwnerPolicyCoversDispatch(t *testing.T) {
	commands := map[string]bool{
		"verify-sums": true, mxcProbeCmd: true, agentReaperCmd: true,
		"serve": true, "gw": false, "router": false, "tray": true,
		"share": false, "project": true, "note": true, "usage": true,
		"plugin": true, "uninstall": true, "run": true, "connect": true,
		"reconnect": true, "disconnect": true, "profiles": true,
		"status": true, "models": true,
		"--minigw-help": false, "-h": false, "--help": false,
		"-v": false, "--version": false,
	}
	file, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "main" {
			continue
		}
		for _, stmt := range fn.Body.List {
			sw, ok := stmt.(*ast.SwitchStmt)
			if !ok {
				continue
			}
			id, ok := sw.Tag.(*ast.Ident)
			if !ok || id.Name != "first" {
				continue
			}
			for _, stmt := range sw.Body.List {
				for _, expr := range stmt.(*ast.CaseClause).List {
					var name string
					switch v := expr.(type) {
					case *ast.BasicLit:
						name, err = strconv.Unquote(v.Value)
						if err != nil {
							t.Fatal(err)
						}
					case *ast.Ident:
						switch v.Name {
						case "mxcProbeCmd":
							name = mxcProbeCmd
						case "agentReaperCmd":
							name = agentReaperCmd
						default:
							t.Fatalf("unhandled dispatch constant %s", v.Name)
						}
					default:
						t.Fatalf("unhandled dispatch expression %T", expr)
					}
					seen[name] = true
					want, ok := commands[name]
					if !ok {
						t.Errorf("dispatch command %q needs an owner-policy expectation", name)
						continue
					}
					if got := createsState("minigw.exe", []string{name}); got != want {
						t.Errorf("createsState(%q) = %v, want %v", name, got, want)
					}
				}
			}
		}
	}
	for name := range commands {
		if !seen[name] {
			t.Errorf("command %q is absent from dispatch", name)
		}
	}
}
