//go:build !darwin

package main

import (
	"errors"
	"net"
)

// The helper is a macOS program (the Mac gateway); elsewhere it builds for vet but serves nobody.
func peerIsParent(net.Conn, int) error {
	return errors.New("the peer check is implemented for macOS only")
}
