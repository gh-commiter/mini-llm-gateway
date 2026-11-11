package main

import (
	"os"
	"testing"

	"golang.org/x/sys/windows"
)

// #84: stičišče in ime 8.3 doma sta dom; C:\Users vsebuje dom; koren pogona je zavrnjen.
func TestShareRefusalWindows(t *testing.T) {
	home, proj := fakeHome(t)
	j := t.TempDir() + `\j`
	junction(t, home, j)
	refused := []string{j, os.Getenv("SystemDrive") + `\Users`, os.Getenv("SystemDrive") + `\`}
	u, _ := windows.UTF16PtrFromString(home)
	buf := make([]uint16, windows.MAX_LONG_PATH)
	if n, err := windows.GetShortPathName(u, &buf[0], uint32(len(buf))); err == nil && n > 0 {
		if short := windows.UTF16ToString(buf[:n]); short != home {
			refused = append(refused, short)
		} else {
			t.Log("8.3 names are off on this volume")
		}
	}
	for _, d := range refused {
		if shareRefusal(d) == "" {
			t.Errorf("%s allowed", d)
		}
	}
	if why := shareRefusal(proj); why != "" {
		t.Errorf("%s refused: %s", proj, why)
	}
}
