package main

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// openMarker: točka ponovne razčlenitve (povezava, spoj) se odpre sama, ne cilj; taka datoteka je zavrnjena.
func openMarker(p string) (*os.File, error) {
	name, err := syscall.UTF16PtrFromString(p)
	if err != nil {
		return nil, err
	}
	const openReparsePoint = 0x00200000 // FILE_FLAG_OPEN_REPARSE_POINT
	h, err := syscall.CreateFile(name, syscall.GENERIC_READ, syscall.FILE_SHARE_READ, nil, syscall.OPEN_EXISTING,
		syscall.FILE_ATTRIBUTE_NORMAL|openReparsePoint, 0)
	if err != nil {
		return nil, err
	}
	var info syscall.ByHandleFileInformation
	if syscall.GetFileInformationByHandle(h, &info) != nil || info.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0 ||
		info.FileAttributes&syscall.FILE_ATTRIBUTE_DIRECTORY != 0 {
		syscall.CloseHandle(h)
		return nil, errors.New("not a regular file")
	}
	return os.NewFile(uintptr(h), p), nil
}

func openRootNote(root *os.Root, name string) (*os.File, error) {
	parent, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer parent.Close()
	if !filepath.IsLocal(name) {
		return nil, errors.New("invalid import path")
	}
	n, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return nil, err
	}
	const dontReparse = 0x1000
	oa := windows.OBJECT_ATTRIBUTES{Length: uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})), RootDirectory: windows.Handle(parent.Fd()), ObjectName: n, Attributes: windows.OBJ_CASE_INSENSITIVE | dontReparse}
	var h windows.Handle
	var ios windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(&h, windows.FILE_GENERIC_READ|windows.SYNCHRONIZE, &oa, &ios, nil, 0,
		windows.FILE_SHARE_READ, windows.FILE_OPEN, windows.FILE_NON_DIRECTORY_FILE|windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_REPARSE_POINT, 0, 0)
	if err != nil {
		if status, ok := err.(windows.NTStatus); ok {
			return nil, status.Errno()
		}
		return nil, err
	}
	var info windows.ByHandleFileInformation
	if err = windows.GetFileInformationByHandle(h, &info); err != nil || info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		windows.CloseHandle(h)
		return nil, errors.New("not a regular file")
	}
	return os.NewFile(uintptr(h), name), nil
}

// protectExported: Windows has no handle-based DACL call here, so the DACL is set by path (the folder is still held open).
func protectExported(p string) error { return protectDir(p) }
