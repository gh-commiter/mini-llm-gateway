package main

import (
	"errors"
	"net"
	"os"

	"golang.org/x/sys/unix"
)

// peerOf returns pid and uid of the process at the other end of a unix connection (LOCAL_PEERPID, LOCAL_PEERCRED; no cgo).
// On a socketpair the pid is that of the process that created it: the app.
func peerOf(c net.Conn) (pid int, uid uint32, err error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return 0, 0, errors.New("not a unix connection")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, 0, err
	}
	var e1, e2 error
	err = raw.Control(func(fd uintptr) {
		pid, e1 = unix.GetsockoptInt(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERPID)
		var x *unix.Xucred
		x, e2 = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
		if x != nil {
			uid = x.Uid
		}
	})
	return pid, uid, errors.Join(err, e1, e2)
}

// peerIsParent: the only peer served is the process that started this one (ppid captured at start), same user.
func peerIsParent(c net.Conn, ppid int) error {
	pid, uid, err := peerOf(c)
	if err != nil {
		return err
	}
	if pid != ppid || int(uid) != os.Geteuid() {
		return errors.New("peer is not the parent app")
	}
	return nil
}
