package main

// Lokalni proxy: Claude Code in minigw govorita navaden HTTP na 127.0.0.1, proxy vsako povezavo prenese
// šifrirano (Noise) do routerja. Ena lokalna povezava = ena šifrirana povezava (kot RouterCore/NoiseProxy.swift).

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

type device struct {
	key  ed25519.PrivateKey
	name string
}

// proxyProject: the project id the next proxy sends with its requests (set by main before route).
var proxyProject string

type noiseProxy struct {
	router    string // host:port
	routerKey []byte
	dev       *device
	ln        net.Listener
	project   atomic.Value // string: the project id minigw resolved for this run ("" = none); see rewriteHead
}

func newNoiseProxy(routerURL string, routerKey []byte, dev *device) (*noiseProxy, error) {
	u, err := url.Parse(routerURL)
	if err != nil || u.Hostname() == "" || len(routerKey) != 32 {
		return nil, fmt.Errorf("invalid gateway %q or key", routerURL)
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

// rewriteHead (L1): the request line and headers (without the final blank line) with every X-Minigw-Project header
// removed, whatever its case, and, when the run has a project, the one minigw resolved. The agent CLI's own settings
// (ANTHROPIC_CUSTOM_HEADERS from a repository) can therefore never choose the project of a request.
// 0.6.0 sec-fix F1, fail closed: ok=false (close the connection) when the head has a line another parser could read
// differently: a line starting with SP/HT (obs-fold), a bare CR or LF inside a line, a line without a colon or a header
// name that is not an RFC 7230 token (`X-Minigw-Project : 7`, a no-break space). The name is compared exactly.
func rewriteHead(head []byte, project string) (out []byte, ok bool) {
	lines := strings.Split(string(head), "\r\n")
	kept := make([]string, 0, len(lines)+1)
	for i, l := range lines {
		if strings.ContainsAny(l, "\r\n") {
			return nil, false
		}
		if i > 0 {
			name, _, found := strings.Cut(l, ":")
			if !found || name == "" || strings.IndexFunc(name, func(r rune) bool { return r > 0x7f || !isTokenByte(byte(r)) }) >= 0 {
				return nil, false
			}
			if strings.EqualFold(name, "x-minigw-project") {
				continue
			}
		}
		kept = append(kept, l)
		if i == 0 && project != "" {
			kept = append(kept, "X-Minigw-Project: "+project)
		}
	}
	return []byte(strings.Join(kept, "\r\n")), true
}

// isTokenByte: RFC 7230 tchar.
func isTokenByte(b byte) bool {
	return b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || strings.IndexByte("!#$%&'*+-.^_`|~", b) >= 0
}

// headRewriter: the first request's header block of one local connection until it is complete, rewritten with rewriteHead;
// everything after it passes unchanged. ok=false: close the connection. R4: the 64 KiB limit is on the offset of the
// terminator, whatever the chunking (as Projects.HeadRewriter on the Mac).
type headRewriter struct {
	project string
	head    []byte
	done    bool
}

const maxHead = 64 << 10

func (h *headRewriter) feed(chunk []byte) (out []byte, ok bool) {
	if h.done {
		return chunk, true
	}
	h.head = append(h.head, chunk...)
	i := bytes.Index(h.head, []byte("\r\n\r\n"))
	if i < 0 {
		return nil, len(h.head) < maxHead+4
	}
	if i > maxHead {
		return nil, false
	}
	rewritten, ok := rewriteHead(h.head[:i], h.project)
	if !ok {
		return nil, false
	}
	h.done = true
	out = append(rewritten, h.head[i:]...)
	h.head = nil
	return out, true
}

func (p *noiseProxy) localURL() string { return "http://" + p.ln.Addr().String() }

func (p *noiseProxy) stop() {
	if p.ln != nil {
		p.ln.Close()
	}
}

func (p *noiseProxy) serve(local net.Conn) {
	defer local.Close()
	project, _ := p.project.Load().(string) // read once per connection
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
		hr := &headRewriter{project: project} // the first request's header block, until it is complete (the gateway serves one request per connection)
		for {
			n, err := local.Read(buf)
			if n > 0 {
				chunk, ok := hr.feed(buf[:n])
				if !ok { // F1/F2/R4: a head that cannot be rewritten safely, or is longer than 64 KiB: close, pass nothing on
					local.Close()
					remote.Close()
					return
				}
				for off := 0; off < len(chunk); off += maxPlain {
					if _, werr := remote.Write(frames(chunk[off:min(off+maxPlain, len(chunk))], session.send)); werr != nil {
						return
					}
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
