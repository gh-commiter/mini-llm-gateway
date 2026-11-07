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
