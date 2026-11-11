package main

import (
	"errors"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// extendedPath (M1): pot za surovi CreateFile tudi nad 260 znaki brez LongPathsEnabled (privzeto 0): »\\?\C:\…«,
// »\\?\UNC\strežnik\…«; že razširjena (»\\?\«) ali naprava (»\\.\«) ostane. »\\?\« izklopi normalizacijo, zato Clean.
func extendedPath(p string) string {
	if strings.HasPrefix(p, `\\?\`) || strings.HasPrefix(p, `\\.\`) {
		return p
	}
	if a, err := filepath.Abs(p); err == nil {
		p = a // Abs tudi počisti (»/« → »\«, ».«, »..«)
	}
	if r, ok := strings.CutPrefix(p, `\\`); ok {
		return `\\?\UNC\` + r
	}
	if filepath.IsAbs(p) {
		return `\\?\` + p
	}
	return p
}

// codexFinalPath (#59): končna pot mape, kot jo vidi Codex (Rust canonicalize sledi tudi stičiščem in priklopnim
// točkam, filepath.EvalSymlinks od Go 1.23 ne): GetFinalPathNameByHandle(FILE_NAME_NORMALIZED|VOLUME_NAME_DOS = 0),
// ob napaki enkrat FILE_NAME_OPENED (0x8; mnogi strežniki SMB na NAS normalizirane ne vrnejo).
func codexFinalPath(p string) string {
	u, err := windows.UTF16PtrFromString(extendedPath(p))
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
	for _, flags := range []uint32{0, 0x8} { // FILE_NAME_NORMALIZED, FILE_NAME_OPENED
		if n, err := windows.GetFinalPathNameByHandle(h, &buf[0], uint32(len(buf)), flags); err == nil && n > 0 && int(n) < len(buf) {
			f := trimFinalPrefix(windows.UTF16ToString(buf[:n]))
			if flags != 0 && hasShortName(f) { // L-a: odprto ime ni razširjeno; kratko ime 8.3 je druga pot do iste mape
				return ""
			}
			return f
		}
	}
	return ""
}

// hasShortName: katera sestavina poti je kratko ime 8.3 (shortName, sync.go).
func hasShortName(p string) bool {
	for _, c := range strings.Split(p, `\`) {
		if shortName.MatchString(c) {
			return true
		}
	}
	return false
}

// finalPath: kanonična pot za preverbo »ostane v korenu« (davResolve, protectedCanonical). filepath.EvalSymlinks od
// Go 1.23 stičišč (mklink /J, brez pravic) ne razreši; pot skoznje bi ušla iz korena. Ne da se odpreti = napaka.
func finalPath(p string) (string, error) {
	if f := codexFinalPath(p); f != "" {
		return f, nil
	}
	return "", errors.New("final path of " + p)
}
