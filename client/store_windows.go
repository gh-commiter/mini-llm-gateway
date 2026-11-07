//go:build windows

package main

// Povezava (naslov, žeton, ključ routerja, ključ naprave) v Windows Credential Manager (Upravitelj poverilnic),
// šifrirano z uporabnikovo prijavo. Brez zunanjih knjižnic: advapi32 neposredno.

import (
	"bufio"
	"errors"
	"os"
	"syscall"
	"unsafe"
)

const credTarget = "minigw:router"

type credential struct {
	Flags              uint32
	Type               uint32
	TargetName         *uint16
	Comment            *uint16
	LastWritten        syscall.Filetime
	CredentialBlobSize uint32
	CredentialBlob     *byte
	Persist            uint32
	AttributeCount     uint32
	Attributes         uintptr
	TargetAlias        *uint16
	UserName           *uint16
}

var (
	advapi32    = syscall.NewLazyDLL("advapi32.dll")
	procWrite   = advapi32.NewProc("CredWriteW")
	procRead    = advapi32.NewProc("CredReadW")
	procDelete  = advapi32.NewProc("CredDeleteW")
	procFree    = advapi32.NewProc("CredFree")
	errNotFound = errors.New("not found")
)

const (
	credTypeGeneric         = 1
	credPersistLocalMachine = 2
)

func storeLoad() ([]byte, error)  { return credLoad(credTarget) }
func storeSave(data []byte) error { return credSave(credTarget, data) }
func storeDelete()                { credDelete(credTarget) }

func credLoad(name string) ([]byte, error) {
	target, _ := syscall.UTF16PtrFromString(name)
	var c *credential
	r, _, err := procRead.Call(uintptr(unsafe.Pointer(target)), credTypeGeneric, 0, uintptr(unsafe.Pointer(&c)))
	if r == 0 {
		if err == syscall.Errno(1168) { // ERROR_NOT_FOUND
			return nil, errNotFound
		}
		return nil, err
	}
	defer procFree.Call(uintptr(unsafe.Pointer(c)))
	return append([]byte{}, unsafe.Slice(c.CredentialBlob, c.CredentialBlobSize)...), nil
}

func credSave(name string, data []byte) error {
	target, _ := syscall.UTF16PtrFromString(name)
	user, _ := syscall.UTF16PtrFromString("minigw")
	c := credential{Type: credTypeGeneric, TargetName: target, UserName: user, Persist: credPersistLocalMachine,
		CredentialBlobSize: uint32(len(data))}
	if len(data) > 0 {
		c.CredentialBlob = &data[0]
	}
	if r, _, err := procWrite.Call(uintptr(unsafe.Pointer(&c)), 0); r == 0 {
		return err
	}
	return nil
}

func credDelete(name string) {
	target, _ := syscall.UTF16PtrFromString(name)
	procDelete.Call(uintptr(unsafe.Pointer(target)), credTypeGeneric, 0)
}

// Ključi prehodov (minigw serve gateway): generična poverilnica minigw/gateway/<ime>.
func osKeyGet(_, name string) (string, string) {
	b, _ := credLoad("minigw/gateway/" + name)
	return string(b), storeName
}

func osKeySet(_, name, key string) (string, error) {
	return storeName, credSave("minigw/gateway/"+name, []byte(key))
}

func osKeyDel(_, name string) { credDelete("minigw/gateway/" + name) }

// readHidden: vrstica s konzole brez odmeva (ENABLE_ECHO_INPUT izklopljen).
func readHidden(f *os.File) (string, error) {
	k := syscall.NewLazyDLL("kernel32.dll")
	get, set := k.NewProc("GetConsoleMode"), k.NewProc("SetConsoleMode")
	var mode uint32
	if r, _, _ := get.Call(f.Fd(), uintptr(unsafe.Pointer(&mode))); r != 0 {
		set.Call(f.Fd(), uintptr(mode&^0x4))
		defer set.Call(f.Fd(), uintptr(mode))
	}
	return bufio.NewReader(f).ReadString('\n')
}

const storeName = "Windows Credential Manager"

// Vrednost HKCU\Software\Microsoft\Windows\CurrentVersion\Run: zagon ob prijavi brez skrbniških pravic.
const runKey = `Software\Microsoft\Windows\CurrentVersion\Run`

var (
	procRegCreate = advapi32.NewProc("RegCreateKeyExW")
	procRegSet    = advapi32.NewProc("RegSetValueExW")
	procRegQuery  = advapi32.NewProc("RegQueryValueExW")
	procRegDelete = advapi32.NewProc("RegDeleteValueW")
	procRegClose  = advapi32.NewProc("RegCloseKey")
)

func runKeyOpen() (uintptr, error) {
	const hkcu, access = 0x80000001, 0x0002 | 0x0001 // KEY_SET_VALUE | KEY_QUERY_VALUE
	sub, _ := syscall.UTF16PtrFromString(runKey)
	var h uintptr
	if r, _, _ := procRegCreate.Call(hkcu, uintptr(unsafe.Pointer(sub)), 0, 0, 0, access, 0, uintptr(unsafe.Pointer(&h)), 0); r != 0 {
		return 0, syscall.Errno(r)
	}
	return h, nil
}

func regRunSet(name, value string) error {
	h, err := runKeyOpen()
	if err != nil {
		return err
	}
	defer procRegClose.Call(h)
	n, _ := syscall.UTF16PtrFromString(name)
	v, _ := syscall.UTF16FromString(value)
	if r, _, _ := procRegSet.Call(h, uintptr(unsafe.Pointer(n)), 0, 1 /* REG_SZ */, uintptr(unsafe.Pointer(&v[0])), uintptr(len(v)*2)); r != 0 {
		return syscall.Errno(r)
	}
	return nil
}

func regRunGet(name string) (string, error) {
	h, err := runKeyOpen()
	if err != nil {
		return "", err
	}
	defer procRegClose.Call(h)
	n, _ := syscall.UTF16PtrFromString(name)
	buf := make([]uint16, 2048)
	size := uint32(len(buf) * 2)
	if r, _, _ := procRegQuery.Call(h, uintptr(unsafe.Pointer(n)), 0, 0, uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size))); r != 0 {
		return "", syscall.Errno(r)
	}
	return syscall.UTF16ToString(buf[:size/2]), nil
}

func regRunDel(name string) {
	h, err := runKeyOpen()
	if err != nil {
		return
	}
	defer procRegClose.Call(h)
	n, _ := syscall.UTF16PtrFromString(name)
	procRegDelete.Call(h, uintptr(unsafe.Pointer(n)))
}

// protectDir: zaščiten DACL samo za lastnika in SYSTEM (mapa nastavitev zunaj %AppData%, kjer ni privzeto zasebna).
func protectDir(dir string) error {
	var (
		pConvert = advapi32.NewProc("ConvertStringSecurityDescriptorToSecurityDescriptorW")
		pGetDacl = advapi32.NewProc("GetSecurityDescriptorDacl")
		pSetSec  = advapi32.NewProc("SetNamedSecurityInfoW")
		pFree    = syscall.NewLazyDLL("kernel32.dll").NewProc("LocalFree")
	)
	var sd uintptr
	sddl, _ := syscall.UTF16PtrFromString("D:P(A;OICI;FA;;;OW)(A;OICI;FA;;;SY)")
	if r, _, err := pConvert.Call(uintptr(unsafe.Pointer(sddl)), 1 /* SDDL_REVISION_1 */, uintptr(unsafe.Pointer(&sd)), 0); r == 0 {
		return err
	}
	defer pFree.Call(sd)
	var present, defaulted int32
	var dacl uintptr
	if r, _, err := pGetDacl.Call(sd, uintptr(unsafe.Pointer(&present)), uintptr(unsafe.Pointer(&dacl)), uintptr(unsafe.Pointer(&defaulted))); r == 0 {
		return err
	}
	name, _ := syscall.UTF16PtrFromString(dir)
	const seFileObject, daclInfo, protectedDacl = 1, 0x4, 0x80000000
	if r, _, _ := pSetSec.Call(uintptr(unsafe.Pointer(name)), seFileObject, daclInfo|protectedDacl, 0, 0, dacl, 0); r != 0 {
		return syscall.Errno(r)
	}
	return nil
}
