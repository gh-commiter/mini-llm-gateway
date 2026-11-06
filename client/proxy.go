package main

// Lokalni proxy: Claude Code in minigw govorita navaden HTTP na 127.0.0.1, proxy vsako povezavo prenese
// šifrirano (Noise) do routerja. Ena lokalna povezava = ena šifrirana povezava (kot RouterCore/NoiseProxy.swift).

import (
	"crypto/ed25519"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"net/url"
	"time"
)

type device struct {
	key  ed25519.PrivateKey
	name string
}

type noiseProxy struct {
	router    string // host:port
	routerKey []byte
	dev       *device
	ln        net.Listener
}

func newNoiseProxy(routerURL string, routerKey []byte, dev *device) (*noiseProxy, error) {
	u, err := url.Parse(routerURL)
	if err != nil || u.Hostname() == "" || len(routerKey) != 32 {
		return nil, fmt.Errorf("invalid router %q or key", routerURL)
	}
	port := u.Port()
	if port == "" {
		port = "8765"
	}
	return &noiseProxy{router: net.JoinHostPort(u.Hostname(), port), routerKey: routerKey, dev: dev}, nil
}

// stablePort: isti lokalni port za isti router (Claude Code hrani seznam modelov skupaj z ANTHROPIC_BASE_URL).
func (p *noiseProxy) stablePort() int {
	h, port, _ := net.SplitHostPort(p.router)
	sum := sha256.Sum256([]byte(h + ":" + port))
	return 20000 + (int(sum[0])<<8|int(sum[1]))%10000
}

// start: stalen port; če je zaseden (druga seja minigw), naključen.
func (p *noiseProxy) start() error {
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", p.stablePort()))
	if err != nil {
		if ln, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
			return err
		}
	}
	p.ln = ln
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go p.serve(c)
		}
	}()
	return nil
}

func (p *noiseProxy) localURL() string { return "http://" + p.ln.Addr().String() }

func (p *noiseProxy) stop() {
	if p.ln != nil {
		p.ln.Close()
	}
}

func (p *noiseProxy) serve(local net.Conn) {
	defer local.Close()
	remote, err := net.DialTimeout("tcp", p.router, 15*time.Second)
	if err != nil {
		return
	}
	defer remote.Close()
	hs, err := newInitiator(p.routerKey)
	if err != nil {
		return
	}
	first, err := hs.first()
	if err != nil {
		return
	}
	if _, err := remote.Write(append(append([]byte{}, noiseMagic...), first...)); err != nil {
		return
	}
	reply := make([]byte, handshakeLen)
	remote.SetReadDeadline(time.Now().Add(15 * time.Second))
	if _, err := io.ReadFull(remote, reply); err != nil { // router ni sprejel (napačen ključ?)
		return
	}
	remote.SetReadDeadline(time.Time{})
	session, err := hs.finish(reply)
	if err != nil {
		return
	}
	if p.dev != nil {
		if _, err := remote.Write(frames(deviceProof(p.dev.key, p.dev.name, session.hash), session.send)); err != nil {
			return
		}
	}

	// Lokalno → okvirji → router. Ob koncu lokalnega pošiljanja odgovor še teče; povezavo zapre router.
	go func() {
		buf := make([]byte, maxPlain)
		for {
			n, err := local.Read(buf)
			if n > 0 {
				if _, werr := remote.Write(frames(buf[:n], session.send)); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	// Router → dešifrirano → lokalno; ob koncu zapre obe strani.
	var pending []byte
	buf := make([]byte, 256*1024)
	for {
		n, err := remote.Read(buf)
		if n > 0 {
			var plain []byte
			var ferr error
			plain, pending, ferr = unframe(append(pending, buf[:n]...), session.receive)
			if ferr != nil {
				return
			}
			if len(plain) > 0 {
				if _, werr := local.Write(plain); werr != nil {
					return
				}
			}
		}
		if err != nil {
			return
		}
	}
}
