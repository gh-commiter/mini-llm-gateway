package main

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// ownerIsUser: elevated processes must create folders owned by the user.
func ownerIsUser() error {
	var tok windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY|windows.TOKEN_ADJUST_DEFAULT, &tok); err != nil {
		return err
	}
	defer tok.Close()
	elevated, err := tokenElevated(tok)
	if err != nil {
		return err
	}
	if !elevated {
		return nil
	}
	tu, err := tok.GetTokenUser()
	if err != nil {
		return err
	}
	owner := struct{ Owner *windows.SID }{tu.User.Sid} // TOKEN_OWNER
	return windows.SetTokenInformation(tok, windows.TokenOwner, (*byte)(unsafe.Pointer(&owner)), uint32(unsafe.Sizeof(owner)))
}

func tokenElevated(tok windows.Token) (bool, error) {
	var elevation uint32
	var size uint32
	err := windows.GetTokenInformation(tok, windows.TokenElevation, (*byte)(unsafe.Pointer(&elevation)), uint32(unsafe.Sizeof(elevation)), &size)
	return elevation != 0, err
}
