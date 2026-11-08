package main

import (
	"os"
	"strings"
	"os/exec"
	"path/filepath"
	"syscall"
	"unsafe"
)

const createNoWindow = 0x08000000 // CREATE_NO_WINDOW

// hideConsole: otrok dobi skrito konzolo (tudi za svoje otroke, npr. conpty), ne vidnega okna; obstoječe zastavice
// (CREATE_SUSPENDED za Job, CREATE_NEW_PROCESS_GROUP …) ostanejo. Ne HideWindow (SW_HIDE): ta skrije tudi okna
// programov GUI (msiexec /passive, prvo sporočilo pladnja), ki bi nato nevidno čakali.
func hideConsole(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= createNoWindow
}

// refreshPath: okolje uporabnika, kot ga dobi Raziskovalec (CreateEnvironmentBlock: sistemske in uporabnikove spremenljivke
// iz registra, razširjene s pravim profilom), in ~\.local\bin na PATH. Pladenj, ki ga zažene MSI, podeduje okolje
// namestitvenega programa: brez uporabnikovega PATH (claude="") in z napačnim %LOCALAPPDATA%, zato mapa iz uporabnikovega
// PATH kot »%LOCALAPPDATA%\Programs\…\codex.exe« ni bila najdena. Velja za LookPath in za vse otroke.
// tray=false (ukazna vrstica): le manjkajoče mape na konec PATH; spremenljivke, ki jih je nastavila lupina, ostanejo.
func refreshPath(tray bool) {
	var tok syscall.Token
	if syscall.OpenProcessToken(syscall.Handle(^uintptr(0)) /* trenutni proces */, syscall.TOKEN_QUERY|syscall.TOKEN_DUPLICATE, &tok) == nil {
		defer tok.Close()
		for _, kv := range userEnv(tok) {
			k, v, ok := strings.Cut(kv, "=")
			switch {
			case !ok || k == "":
			case strings.EqualFold(k, "PATH") && tray:
				os.Setenv("PATH", mergePath(";", v, os.Getenv("PATH")))
			case strings.EqualFold(k, "PATH"):
				os.Setenv("PATH", mergePath(";", os.Getenv("PATH"), v))
			case tray:
				os.Setenv(k, v)
			}
		}
	}
	home, _ := os.UserHomeDir()
	os.Setenv("PATH", mergePath(";", os.Getenv("PATH"), filepath.Join(home, ".local", "bin")))
}

// userEnv: CreateEnvironmentBlock za žeton (brez podedovanja okolja tega procesa); nil ob napaki.
func userEnv(tok syscall.Token) []string {
	userenv := syscall.NewLazyDLL("userenv.dll")
	var block *uint16
	if r, _, _ := userenv.NewProc("CreateEnvironmentBlock").Call(uintptr(unsafe.Pointer(&block)), uintptr(tok), 0); r == 0 || block == nil {
		return nil
	}
	defer userenv.NewProc("DestroyEnvironmentBlock").Call(uintptr(unsafe.Pointer(block)))
	var out []string
	for p := unsafe.Pointer(block); ; {
		n := 0
		for *(*uint16)(unsafe.Add(p, n*2)) != 0 {
			n++
		}
		if n == 0 {
			return out
		}
		out = append(out, syscall.UTF16ToString(unsafe.Slice((*uint16)(p), n)))
		p = unsafe.Add(p, (n+1)*2)
	}
}
