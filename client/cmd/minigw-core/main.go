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
// `minigw-core restore -store <folder> [-yes] <file|name>` is the offline restore (minigw gw core restore on the Mac): it takes
// core.lock exclusively, so it fails while a helper has the store open, and never starts a helper (restore.go).
//
// Exit codes: 0 EOF (the app quit), 1 refused (peer is not the parent, bad start) or a failed connection, 2 usage,
// 3 the parent died. A store that cannot be opened (newer schema, locked, damaged) is NOT an exit: the process stays up
// and every request is answered with coreapi's 503 core_unavailable (an admin call carries the reason), so the app
// shows why.
package main

import (
	"context"
	"flag"
	"fmt"
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
	laneFD   = 5 // optional second connection (-lane): the upstream routes, which may wait for the app, so that they never hold up the first

	exitRefused = 1
	exitUsage   = 2
	exitOrphan  = 3
)

// slowOpen is nil in every shipped build; the test build (slowupgrade_test_build.go) sets it to stand in for a long upgrade.
var slowOpen func(store string, announce func(from, to int))

func main() {
	if len(os.Args) > 1 && os.Args[1] == "restore" { // `minigw gw core restore` on the Mac: offline, no socket, no helper
		os.Exit(restoreMain(os.Args[2:], os.Stdin, os.Stdout, os.Stderr))
	}
	store := flag.String("store", "", "folder of the core store (private, given by the app)")
	lane := flag.Bool("lane", false, "a second connection on fd 5 serves the upstream routes (they may wait for the app)")
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
	var laneConn net.Conn
	if *lane {
		lf := os.NewFile(laneFD, "lane")
		laneConn, err = net.FileConn(lf)
		lf.Close()
		if err != nil {
			log.Printf("no connection on fd %d: %v", laneFD, err)
			os.Exit(exitRefused)
		}
		if err := peerIsParent(laneConn, os.Getppid()); err != nil {
			log.Printf("refusing the second connection: %v", err)
			os.Exit(exitRefused)
		}
	}
	go watchParent(os.Getppid(), 200*time.Millisecond, func() { os.Exit(exitOrphan) })

	announce := func(from, to int) { // before the snapshot and the migrations: the app then waits for the long step
		detail := fmt.Sprintf("upgrading the database (schema %d to %d)", from, to)
		log.Print(detail)
		writeMsg(conn, msg{Type: "state", Code: "upgrading", Detail: detail})
	}
	if slowOpen != nil { // only in the test build (-tags slowupgrade)
		slowOpen(*store, announce)
	}
	st, openErr := core.Open(*store, core.Options{OnUpgrade: announce})
	if openErr != nil {
		log.Printf("WARNING: the core store cannot be used, every request answers 503 core_unavailable: %v", openErr)
		st = nil
	} else {
		if n := st.SentinelMarked(); n > 0 {
			log.Printf("restore pending file found: %d upstream links marked restored", n)
		}
		if n, e := st.MarkedLinks(context.Background()); e == nil && n > 0 {
			log.Printf("%d upstream links are marked restored; automatic pushes to them are paused; see gw upstream status", n)
		}
	}
	host := &coreapi.CalloutHost{} // the app does the upstream work (network, credential store) for the helper: callouts
	api := coreapi.New(coreapi.Options{
		// one call of the helper has 30 seconds: a push ends with "more" well before and the caller asks again
		UpstreamBudget: 10 * time.Second,
		Store:          func() *core.Store { return st },
		Unavailable: func() string {
			if openErr != nil {
				return openErr.Error()
			}
			return ""
		},
		Dir: *store,
		Log: log.Printf, // reasons only; never a body
		// the Mac runs model labels with Apple Foundation Models only (the app's label worker)
		LabelBackends: []string{"apple-fm"},
	})
	if openErr == nil { // as in minigw serve: no recorder and no spool file while the core is not open (nothing is written next to protected data)
		api.StartRecorder() // usage events (the app sends them): stored in the core; spooled (bounded) in the store folder if a write fails
	}
	mainHost := host
	if laneConn != nil {
		// The upstream routes may wait for the app (the network, the Keychain) and have their own connection: a slow upstream can never
		// hold up a member read or a device revocation on the first one. Only this connection gets callouts.
		mainHost = nil
		go func() {
			if err := serve(context.Background(), laneConn, secret, api, host, requestTimeout); err != nil {
				log.Printf("upstream connection ended: %v", err)
			}
		}()
	}
	err = serve(context.Background(), conn, secret, api, mainHost, requestTimeout)
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
