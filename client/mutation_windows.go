package main

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

func mutationHandlePath(f *os.File) (string, error) {
	b := make([]uint16, windows.MAX_LONG_PATH)
	n, err := windows.GetFinalPathNameByHandle(windows.Handle(f.Fd()), &b[0], uint32(len(b)), 0)
	if err != nil {
		return "", err
	}
	if n == 0 || int(n) >= len(b) {
		return "", fmt.Errorf("invalid handle path")
	}
	return trimFinalPrefix(windows.UTF16ToString(b[:n])), nil
}
func mutationOpenFile(r *os.Root, n string, flags int, mode os.FileMode) (*os.File, error) {
	// All data writes create a new file; never follow an existing reparse point.
	return r.OpenFile(n, flags|os.O_CREATE|os.O_EXCL, mode)
}
func mutationRename(src *os.File, sn string, dst *os.File, dn string) error {
	name, err := windows.NewNTUnicodeString(sn)
	if err != nil {
		return err
	}
	oa := windows.OBJECT_ATTRIBUTES{Length: uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})), RootDirectory: windows.Handle(src.Fd()), ObjectName: name, Attributes: windows.OBJ_CASE_INSENSITIVE}
	var h windows.Handle
	var ios windows.IO_STATUS_BLOCK
	err = windows.NtCreateFile(&h, windows.DELETE|windows.SYNCHRONIZE, &oa, &ios, nil, 0,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_OPEN,
		windows.FILE_OPEN_REPARSE_POINT|windows.FILE_OPEN_FOR_BACKUP_INTENT|windows.FILE_SYNCHRONOUS_IO_NONALERT, 0, 0)
	if err != nil {
		if status, ok := err.(windows.NTStatus); ok {
			return status.Errno()
		}
		return err
	}
	defer windows.CloseHandle(h)
	target, err := windows.UTF16FromString(dn)
	if err != nil {
		return err
	}
	type renameInfo struct {
		Replace uint32
		Root    windows.Handle
		Length  uint32
		Name    [1]uint16
	}
	var info renameInfo
	size := max(int(unsafe.Sizeof(info)), int(unsafe.Offsetof(info.Name))+(len(target)-1)*2)
	buf := make([]byte, size)
	ri := (*renameInfo)(unsafe.Pointer(&buf[0]))
	ri.Replace = windows.FILE_RENAME_REPLACE_IF_EXISTS
	ri.Root = windows.Handle(dst.Fd())
	ri.Length = uint32((len(target) - 1) * 2)
	copy(unsafe.Slice(&ri.Name[0], len(target)-1), target)
	// Match Root.Rename: try POSIX semantics, then the older filesystem API.
	ri.Replace |= windows.FILE_RENAME_POSIX_SEMANTICS
	const fileRenameInformationEx = 65
	err = windows.NtSetInformationFile(h, &ios, &buf[0], uint32(size), fileRenameInformationEx)
	if err == nil {
		return nil
	}
	ri.Replace = windows.FILE_RENAME_REPLACE_IF_EXISTS
	err = windows.NtSetInformationFile(h, &ios, &buf[0], uint32(size), windows.FileRenameInformation)
	if status, ok := err.(windows.NTStatus); ok {
		return status.Errno()
	}
	return err
}
