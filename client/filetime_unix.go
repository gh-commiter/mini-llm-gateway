//go:build !windows

package main

import (
	"os"
	"time"

	"golang.org/x/sys/unix"
)

func fileChtimes(f *os.File, t time.Time) error {
	tv := unix.NsecToTimeval(t.UnixNano())
	return unix.Futimes(int(f.Fd()), []unix.Timeval{tv, tv})
}
