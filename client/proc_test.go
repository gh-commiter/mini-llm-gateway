package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"
)

// Noben zagon procesa mimo proc.go: na Windows bi otrok pladnja sicer odprl vidno okno konzole.
func TestProcessStartsGoThroughProcGo(t *testing.T) {
	banned := map[string]bool{"exec.Command": true, "exec.CommandContext": true, "os.StartProcess": true,
		"syscall.StartProcess": true, "syscall.CreateProcess": true, "windows.CreateProcess": true}
	files, _ := filepath.Glob("*.go")
	fset := token.NewFileSet()
	for _, f := range files {
		if f == "proc.go" || strings.HasSuffix(f, "_test.go") {
			continue
		}
		af, err := parser.ParseFile(fset, f, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(af, func(n ast.Node) bool {
			if s, ok := n.(*ast.SelectorExpr); ok {
				if x, ok := s.X.(*ast.Ident); ok && banned[x.Name+"."+s.Sel.Name] {
					t.Errorf("%s: %s.%s: use command/commandContext from proc.go (hidden console on Windows)", fset.Position(s.Pos()), x.Name, s.Sel.Name)
				}
			}
			return true
		})
	}
}

func TestMergePath(t *testing.T) {
	got := mergePath(";", `C:\Windows;C:\Tools\`, `C:\Users\u\.local\bin;c:\windows\`, `C:\tools;D:\x`, `C:\Users\u\.local\bin`)
	if want := `C:\Windows;C:\Tools\;C:\Users\u\.local\bin;D:\x`; got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
