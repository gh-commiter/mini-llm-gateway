package main

import (
	"os"
	"testing"
)

// W2: UNC paths are remote; the system drive is not; the real drive type decides for a drive letter.
func TestIsRemoteVolumeWindows(t *testing.T) {
	for _, p := range []string{`\\server\share\dir`, `\\QNAP\Public\x`} {
		if !isRemoteVolume(p) {
			t.Errorf("%s is not reported remote", p)
		}
	}
	for _, p := range []string{os.Getenv("SystemDrive") + `\Windows`, os.TempDir()} {
		if isRemoteVolume(p) {
			t.Errorf("%s is reported remote", p)
		}
	}
}
