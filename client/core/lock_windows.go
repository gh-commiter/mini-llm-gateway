//go:build windows

package core

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// tryLock locks the first byte exclusively without waiting; busy = another handle holds it.
func tryLock(f *os.File) (busy bool, err error) {
	err = windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, new(windows.Overlapped))
	return errors.Is(err, windows.ERROR_LOCK_VIOLATION), err
}
