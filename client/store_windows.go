//go:build windows

package main

// Povezava (naslov, žeton, ključ routerja, ključ naprave) v Windows Credential Manager (Upravitelj poverilnic),
// šifrirano z uporabnikovo prijavo. Brez zunanjih knjižnic: advapi32 neposredno.

import (
	"errors"
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

func storeLoad() ([]byte, error) {
	target, _ := syscall.UTF16PtrFromString(credTarget)
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

func storeSave(data []byte) error {
	target, _ := syscall.UTF16PtrFromString(credTarget)
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

func storeDelete() {
	target, _ := syscall.UTF16PtrFromString(credTarget)
	procDelete.Call(uintptr(unsafe.Pointer(target)), credTypeGeneric, 0)
}

const storeName = "Windows Credential Manager"
