package main

import (
	"os"
	"path/filepath"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

func TestOwnerIsUser(t *testing.T) {
	var tok windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY|windows.TOKEN_ADJUST_DEFAULT, &tok); err != nil {
		t.Fatal(err)
	}
	defer tok.Close()
	if tok.IsElevated() {
		admin, err := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)
		if err != nil {
			t.Fatal(err)
		}
		owner := struct{ Owner *windows.SID }{admin}
		if err := windows.SetTokenInformation(tok, windows.TokenOwner, (*byte)(unsafe.Pointer(&owner)), uint32(unsafe.Sizeof(owner))); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := ownerIsUser(); err != nil {
				t.Error(err)
			}
		})
	}
	if err := ownerIsUser(); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(t.TempDir(), "owned")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if !ownedByMe(dir) {
		t.Fatal("new folder is not owned by the user")
	}
}
