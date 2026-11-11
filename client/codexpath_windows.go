package main

import (
	"errors"

	"golang.org/x/sys/windows"
)

// codexFinalPath (#59): končna pot mape, kot jo vidi Codex (Rust canonicalize sledi tudi stičiščem in priklopnim
// točkam, filepath.EvalSymlinks od Go 1.23 ne): GetFinalPathNameByHandle(FILE_NAME_NORMALIZED|VOLUME_NAME_DOS = 0).
func codexFinalPath(p string) string {
	u, err := windows.UTF16PtrFromString(p)
	if err != nil {
		return ""
	}
	h, err := windows.CreateFile(u, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0) // BACKUP_SEMANTICS: ročaj mape
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), 0)
	if err != nil || n == 0 || int(n) >= len(buf) {
		return ""
	}
	return trimFinalPrefix(windows.UTF16ToString(buf[:n]))
}

// finalPath: kanonična pot za preverbo »ostane v korenu« (davResolve, protectedCanonical). filepath.EvalSymlinks od
// Go 1.23 stičišč (mklink /J, brez pravic) ne razreši; pot skoznje bi ušla iz korena. Ne da se odpreti = napaka.
func finalPath(p string) (string, error) {
	if f := codexFinalPath(p); f != "" {
		return f, nil
	}
	return "", errors.New("final path of " + p)
}
