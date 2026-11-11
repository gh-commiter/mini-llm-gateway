package main

import (
	"errors"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// followStops (L-b): claude se ustavi sam (Ctrl+Z v načinu raw: SIGSTOP sebi); minigw se ustavi za njim, da lupina
// dobi terminal nazaj, po fg (SIGCONT skupini) še claude dobi SIGCONT. waitid z WNOWAIT pusti izhod za cmd.Wait,
// WSTOPPED brez WEXITED porabi le ustavitev.
func followStops(pid int) {
	const cldStopped = 5 // CLD_STOPPED
	for {
		var info unix.Siginfo
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WSTOPPED|unix.WEXITED|unix.WNOWAIT, nil)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil || info.Code != cldStopped {
			return
		}
		unix.Waitid(unix.P_PID, pid, &info, unix.WSTOPPED|unix.WNOHANG, nil)
		syscall.Kill(os.Getpid(), syscall.SIGSTOP)
		syscall.Kill(pid, syscall.SIGCONT)
	}
}
