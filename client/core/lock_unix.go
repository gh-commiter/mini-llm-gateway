//go:build !windows

package core

import (
	"os"
	"syscall"
)

// tryLock takes a non-blocking exclusive flock; busy = another open file description holds it.
func tryLock(f *os.File) (busy bool, err error) {
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	return err == syscall.EWOULDBLOCK, err
}
