//go:build windows

package core

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// linkInfo: NumberOfLinks of the file (GetFileInformationByHandle, the same call as the file-identity checks of the
// gateway; 0 when it cannot be told) and its identity (volume serial and file index).
func linkInfo(path string, _ os.FileInfo) (uint64, string) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, ""
	}
	h, err := windows.CreateFile(p, windows.FILE_READ_ATTRIBUTES, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return 0, ""
	}
	defer windows.CloseHandle(h)
	var info windows.ByHandleFileInformation
	if windows.GetFileInformationByHandle(h, &info) != nil {
		return 0, ""
	}
	return uint64(info.NumberOfLinks), fmt.Sprint(info.VolumeSerialNumber, ":", info.FileIndexHigh, ":", info.FileIndexLow)
}
