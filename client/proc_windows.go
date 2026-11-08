package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	err := syscall.OpenProcessToken(syscall.Handle(^uintptr(0)) /* trenutni proces */, syscall.TOKEN_QUERY|syscall.TOKEN_DUPLICATE, &tok)
	var env []string
	if err == nil {
		env, err = userEnv(tok)
		tok.Close()
	}
	envSource = "user"
	if err != nil { // rezerva: Path iz registra (sistemski in uporabnikov), razširjen z okoljem tega procesa
		envSource = "registry (" + err.Error() + ")"
		env = []string{"PATH=" + regString(0x80000002, `SYSTEM\CurrentControlSet\Control\Session Manager\Environment`) + ";" + regString(0x80000001, `Environment`)}
	}
	for _, kv := range env {
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
	home, _ := os.UserHomeDir()
	os.Setenv("PATH", mergePath(";", os.Getenv("PATH"), filepath.Join(home, ".local", "bin")))
}

// envSource: od kod je okolje (vrstica zagona v dnevniku): »user« ali »registry (napaka)«.
var envSource string

// userEnv: CreateEnvironmentBlock za žeton (brez podedovanja okolja tega procesa); nil ob napaki.
func userEnv(tok syscall.Token) ([]string, error) {
	userenv := syscall.NewLazyDLL("userenv.dll")
	var block *uint16
	if r, _, e := userenv.NewProc("CreateEnvironmentBlock").Call(uintptr(unsafe.Pointer(&block)), uintptr(tok), 0); r == 0 || block == nil {
		return nil, fmt.Errorf("CreateEnvironmentBlock: %v", e)
	}
	defer userenv.NewProc("DestroyEnvironmentBlock").Call(uintptr(unsafe.Pointer(block)))
	var out []string
	for p := unsafe.Pointer(block); ; {
		n := 0
		for *(*uint16)(unsafe.Add(p, n*2)) != 0 {
			n++
		}
		if n == 0 {
			return out, nil
		}
		out = append(out, syscall.UTF16ToString(unsafe.Slice((*uint16)(p), n)))
		p = unsafe.Add(p, (n+1)*2)
	}
}

// regString: vrednost Path ključa, razširjena (%USERPROFILE% …); "" ob napaki.
func regString(root uintptr, key string) string {
	get := syscall.NewLazyDLL("advapi32.dll").NewProc("RegGetValueW")
	const flags = 0x00000002 | 0x00000004 | 0x10000000 // RRF_RT_REG_SZ | RRF_RT_REG_EXPAND_SZ | RRF_NOEXPAND
	var size uint32
	k, v := u16(key), u16("Path")
	if r, _, _ := get.Call(root, uintptr(unsafe.Pointer(k)), uintptr(unsafe.Pointer(v)), flags, 0, 0, uintptr(unsafe.Pointer(&size))); r != 0 || size == 0 {
		return ""
	}
	buf := make([]uint16, size/2+1)
	if r, _, _ := get.Call(root, uintptr(unsafe.Pointer(k)), uintptr(unsafe.Pointer(v)), flags, 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size))); r != 0 {
		return ""
	}
	raw := syscall.UTF16ToString(buf)
	exp := syscall.NewLazyDLL("kernel32.dll").NewProc("ExpandEnvironmentStringsW")
	out := make([]uint16, 32768)
	if n, _, _ := exp.Call(uintptr(unsafe.Pointer(u16(raw))), uintptr(unsafe.Pointer(&out[0])), uintptr(len(out))); n == 0 || int(n) > len(out) {
		return raw
	}
	return syscall.UTF16ToString(out)
}
