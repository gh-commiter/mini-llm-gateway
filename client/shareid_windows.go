package main

import (
	"os"
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

// volumeOf (#84): GUID nosilca, ki hrani p (\\?\Volume{…}\), in ali je p sam točka priklopa (koren pogona ali
// nosilec, priklopljen v mapo). extendedPath tudi nad 260 znaki; predpona se pred primerjavo odstrani.
func volumeOf(p string) (guid string, root bool, err error) {
	u, err := windows.UTF16PtrFromString(extendedPath(p))
	if err != nil {
		return "", false, err
	}
	buf := make([]uint16, windows.MAX_LONG_PATH)
	if err := windows.GetVolumePathName(u, &buf[0], uint32(len(buf))); err != nil {
		return "", false, err
	}
	vp := windows.UTF16ToString(buf)
	trim := func(p string) string { return strings.TrimRight(filepath.Clean(trimFinalPrefix(p)), `\`) }
	root = strings.EqualFold(trim(vp), trim(extendedPath(p)))
	g := make([]uint16, 64)
	for _, m := range []string{vp, trimFinalPrefix(vp)} { // nekatere različice ne sprejmejo »\\?\«
		v, err2 := windows.UTF16PtrFromString(strings.TrimRight(m, `\`) + `\`)
		if err = err2; err == nil {
			if err = windows.GetVolumeNameForVolumeMountPoint(v, &g[0], uint32(len(g))); err == nil {
				return windows.UTF16ToString(g), root, nil
			}
		}
	}
	return "", false, err
}

// mountRoot (#84): dir je nosilec, priklopljen v mapo, in to je nosilec %SystemDrive% ali doma (korene pogonov zavrne
// že shareRefusal). Nosilec z drugimi podatki, priklopljen v mapo, je dovoljen.
func mountRoot(dir string, _ fileID, home string) (bool, error) {
	g, root, err := volumeOf(dir)
	if err != nil || !root {
		return false, err
	}
	for _, p := range []string{os.Getenv("SystemDrive") + `\`, home} {
		o, _, err := volumeOf(p)
		if err != nil {
			return false, err
		}
		if strings.EqualFold(o, g) {
			return true, nil
		}
	}
	return false, nil
}
