package main

import (
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// fileIDOf (#84): istovetnost mape (serijska številka nosilca in indeks datoteke; stičišča in imena 8.3 vodijo do
// istega ročaja), napaka = ni je mogoče ugotoviti.
func fileIDOf(p string) (fileID, error) {
	u, err := windows.UTF16PtrFromString(extendedPath(p))
	if err != nil {
		return fileID{}, err
	}
	h, err := windows.CreateFile(u, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0) // BACKUP_SEMANTICS: ročaj mape
	if err != nil {
		return fileID{}, err
	}
	defer windows.CloseHandle(h)
	var fi windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &fi); err != nil {
		return fileID{}, err
	}
	return fileID{uint64(fi.VolumeSerialNumber), uint64(fi.FileIndexHigh)<<32 | uint64(fi.FileIndexLow)}, nil
}

// mountRoot (#84): dir je koren nosilca (pogon ali nosilec, priklopljen v mapo; GetVolumePathName).
func mountRoot(dir string, _ fileID) (bool, error) {
	u, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return false, err
	}
	buf := make([]uint16, windows.MAX_LONG_PATH)
	if err := windows.GetVolumePathName(u, &buf[0], uint32(len(buf))); err != nil {
		return false, err
	}
	trim := func(p string) string { return strings.TrimRight(filepath.Clean(p), `\`) }
	return strings.EqualFold(trim(windows.UTF16ToString(buf)), trim(dir)), nil
}
