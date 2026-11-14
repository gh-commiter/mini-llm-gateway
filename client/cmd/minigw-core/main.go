// minigw-core: the helper process of the Mac gateway. It hosts the 0.6.0 core (package coreapi over the store of
// package core) behind ONE inherited socket; the Mac app forwards requests to it with a trusted context. It has no
// network listener, no HTTP server and no gateway or agent code. See docs/core-adapter.md and docs/gateway.md.
//
// Contract with the parent (the app); descriptors are fixed:
//
//	fd 3  one end of a socketpair (AF_UNIX, SOCK_STREAM): the only connection this process serves
//	fd 4  read end of a pipe with the 32-byte per-start secret (read once, then closed); never argv, environment or log
//
// Only the parent is served: the peer of fd 3 must be the parent process (LOCAL_PEERPID) of the same user
// (LOCAL_PEERCRED), and must prove the secret in a mutual HMAC handshake before any request is read. The process exits
// when its parent changes or the app closes the socket.
//
// Exit codes: 0 EOF (the app quit), 1 refused (peer is not the parent, bad start) or a failed connection, 2 usage,
// 3 the parent died. A store that cannot be opened (newer schema, locked, damaged) is NOT an exit: the process stays up
// and every request is answered with coreapi's 503 core_unavailable (an admin call carries the reason), so the app
// shows why.
package main

import (
	"context"
	"flag"
	"io"
	"log"
	"net"
	"os"
	"time"

	"minigw/core"
	"minigw/coreapi"
)

const (
	connFD   = 3
	secretFD = 4

	exitRefused = 1
	exitUsage   = 2
	exitOrphan  = 3
)

func main() {
	store := flag.String("store", "", "folder of the core store (private, given by the app)")
	flag.Parse()
	log.SetFlags(0)
	log.SetPrefix("minigw-core: ")
	if *store == "" || flag.NArg() != 0 {
		log.Print("usage: minigw-core -store <folder> (fd 3: socket, fd 4: secret)")
		os.Exit(exitUsage)
	}
	secret := make([]byte, secretLen)
	pipe := os.NewFile(secretFD, "secret")
	if _, err := io.ReadFull(pipe, secret); err != nil { // blocks until the app writes it and closes its end
		log.Printf("no secret: %v", err)
		os.Exit(exitRefused)
	}
	pipe.Close()
	f := os.NewFile(connFD, "conn")
	conn, err := net.FileConn(f)
	f.Close()
	if err != nil {
		log.Printf("no connection on fd %d: %v", connFD, err)
		os.Exit(exitRefused)
	}
	if err := peerIsParent(conn, os.Getppid()); err != nil {
		log.Printf("refusing: %v", err)
		os.Exit(exitRefused)
	}
	go watchParent(os.Getppid(), 200*time.Millisecond, func() { os.Exit(exitOrphan) })

	st, openErr := core.Open(*store, core.Options{})
	if openErr != nil {
		log.Printf("WARNING: the core store cannot be used, every request answers 503 core_unavailable: %v", openErr)
		st = nil
	}
	api := coreapi.New(coreapi.Options{
		Store: func() *core.Store { return st },
		Unavailable: func() string {
			if openErr != nil {
				return openErr.Error()
			}
			return ""
		},
		Dir: *store,
		Log: log.Printf, // reasons only; never a body
	})
	if openErr == nil { // as in minigw serve: no recorder and no spool file while the core is not open (nothing is written next to protected data)
		api.StartRecorder() // usage events (the app sends them): stored in the core; spooled (bounded) in the store folder if a write fails
	}
	err = serve(context.Background(), conn, secret, api, requestTimeout)
	api.StopRecorder() // drain the queue while the core is still open
	if st != nil {
		st.Close()
	}
	if err != nil {
		log.Printf("connection ended: %v", err)
		os.Exit(exitRefused)
	}
}

// watchParent calls exit when the parent process changes (the app died: the process was handed to launchd).
func watchParent(ppid int, every time.Duration, exit func()) {
	for os.Getppid() == ppid {
		time.Sleep(every)
	}
	exit()
}
