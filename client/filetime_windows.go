package main

import (
	"os"
	"time"

	"golang.org/x/sys/windows"
)

func fileChtimes(f *os.File, t time.Time) error {
	ft := windows.NsecToFiletime(t.UnixNano())
	return windows.SetFileTime(windows.Handle(f.Fd()), nil, &ft, &ft)
}
