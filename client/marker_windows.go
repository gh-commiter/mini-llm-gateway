package main

import (
	"errors"
	"os"
	"syscall"
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
