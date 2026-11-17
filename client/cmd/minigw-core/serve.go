package main

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"minigw/coreapi"
)

// The handshake precedes the coreapi frames on the same connection. Frames are the same form (4-byte big-endian length,
// JSON) but at most maxHandshake long, and the whole handshake has one deadline:
//
//	app    hello    {type, min, max, nonce}            the protocol versions it speaks, its nonce
//	helper hello_ok {type, version, nonce, mac}        mac proves the secret to the app first (label "srv")
//	app    auth     {type, mac}                        label "cli"
//	helper ready    {type}                             coreapi frames follow
//
// mac = hex(HMAC-SHA256(secret, label|version|first nonce|second nonce)); the secret itself is never sent. On a version
// without overlap the helper answers {type:"error", code:"version_mismatch", server_min, server_max} and closes.
const (
	secretLen        = 32
	maxHandshake     = 1 << 10
	handshakeTimeout = 5 * time.Second
	writeTimeout     = 10 * time.Second
	requestTimeout   = 30 * time.Second // one request, from decoded frame to answer; longer = 503
)

type msg struct {
	Type      string `json:"type,omitempty"`
	Min       int    `json:"min,omitempty"`
	Max       int    `json:"max,omitempty"`
	Nonce     string `json:"nonce,omitempty"`
	Version   int    `json:"version,omitempty"`
	MAC       string `json:"mac,omitempty"`
	Code      string `json:"code,omitempty"`
	ServerMin int    `json:"server_min,omitempty"`
	ServerMax int    `json:"server_max,omitempty"`
	Callouts  bool   `json:"callouts,omitempty"` // hello: the app can do upstream work for the helper (callouts, coreapi/callout.go)
}

func mac(secret []byte, label string, version int, a, b string) string {
	m := hmac.New(sha256.New, secret)
	fmt.Fprintf(m, "%s|%d|%s|%s", label, version, a, b)
	return fmt.Sprintf("%x", m.Sum(nil))
}

func writeMsg(c net.Conn, m msg) error {
	b, _ := json.Marshal(m)
	c.SetWriteDeadline(time.Now().Add(writeTimeout))
	_, err := c.Write(binary.BigEndian.AppendUint32(nil, uint32(len(b))))
	if err == nil {
		_, err = c.Write(b)
	}
	return err
}

var errHandshakeFrame = errors.New("handshake frame too large")

func readMsg(c net.Conn) (m msg, err error) {
	var h [4]byte
	if _, err = io.ReadFull(c, h[:]); err != nil {
		return
	}
	n := binary.BigEndian.Uint32(h[:])
	if n > maxHandshake { // refused before the payload is read
		return m, errHandshakeFrame
	}
	b := make([]byte, n)
	if _, err = io.ReadFull(c, b); err != nil {
		return
	}
	return m, json.Unmarshal(b, &m)
}

func handshake(c net.Conn, secret []byte) (callouts bool, err error) {
	c.SetDeadline(time.Now().Add(handshakeTimeout))
	defer c.SetDeadline(time.Time{})
	h, err := readMsg(c)
	if err != nil || h.Type != "hello" || h.Nonce == "" || len(h.Nonce) > 128 {
		return false, errors.New("bad hello")
	}
	v := coreapi.ProtocolVersion
	if h.Min > v || h.Max < v {
		writeMsg(c, msg{Type: "error", Code: "version_mismatch", ServerMin: v, ServerMax: v})
		return false, fmt.Errorf("version mismatch: the app speaks %d..%d, the helper %d", h.Min, h.Max, v)
	}
	nb := make([]byte, 16)
	rand.Read(nb)
	sn := fmt.Sprintf("%x", nb)
	if err := writeMsg(c, msg{Type: "hello_ok", Version: v, Nonce: sn, MAC: mac(secret, "srv", v, h.Nonce, sn)}); err != nil {
		return false, err
	}
	a, err := readMsg(c)
	if err != nil || a.Type != "auth" || !hmac.Equal([]byte(a.MAC), []byte(mac(secret, "cli", v, sn, h.Nonce))) {
		writeMsg(c, msg{Type: "error", Code: "auth_failed"})
		return false, errors.New("auth failed (wrong or stale secret)")
	}
	return h.Callouts, writeMsg(c, msg{Type: "ready"})
}

// serve: handshake, then coreapi frames one at a time until the app closes the connection (nil). A frame over
// coreapi.MaxFrame is refused before its payload is read (coreapi.ReadRequest); a request is answered within timeout or
// with coreapi's 503. ponytail: a peer that stalls inside a frame is not timed out; the peer is the authenticated parent
// and its own deadline kills this process.
func serve(ctx context.Context, c net.Conn, secret []byte, api *coreapi.API, host *coreapi.CalloutHost, timeout time.Duration) error {
	defer c.Close()
	callouts, err := handshake(c, secret)
	if err != nil {
		return err
	}
	if host != nil {
		host.Attach(c, callouts)
		ctx = coreapi.WithUpstream(ctx, host) // this connection's host, for the requests that arrive on it only
	}
	for {
		id, cx, rq, err := coreapi.ReadRequest(c)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			if errors.Is(err, coreapi.ErrFrameTooLarge) || errors.Is(err, coreapi.ErrVersion) {
				coreapi.WriteResponse(c, id, coreapi.Response{}, err.Error())
			}
			return err
		}
		rctx, cancel := context.WithTimeout(ctx, timeout)
		if host != nil {
			host.Begin(id)
		}
		resp := api.Handle(rctx, cx, rq)
		if rctx.Err() != nil {
			resp = coreapi.Unavailable() // the outcome of a write is unknown to the caller: it must not assume
		}
		cancel()
		c.SetWriteDeadline(time.Now().Add(writeTimeout))
		if err := coreapi.WriteResponse(c, id, resp, ""); err != nil {
			return err
		}
	}
}
