package main

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

// "no answer from the gateway" names its cause in a few fixed words; the Swift client (ProjectHTTP.cause) uses the same words:
// connection refused | timed out after N s | connection reset | could not resolve the host | network unreachable |
// TLS: certificate not trusted | TLS: handshake failed | bad gateway address.
func TestTransportCauseWords(t *testing.T) {
	op := func(e error) error {
		return &url.Error{Op: "Get", URL: "http://secret-host/x?token=abc", Err: &net.OpError{Op: "dial", Err: os.NewSyscallError("connect", e)}}
	}
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"refused", op(syscall.ECONNREFUSED), "connection refused"},
		{"windows refused", op(syscall.Errno(10061)), "connection refused"},
		{"reset", op(syscall.ECONNRESET), "connection reset"},
		{"eof", &url.Error{Op: "Get", Err: io.EOF}, "connection reset"},
		{"unreachable", op(syscall.ENETUNREACH), "network unreachable"},
		{"host unreachable", op(syscall.EHOSTUNREACH), "network unreachable"},
		{"dns", &url.Error{Err: &net.DNSError{Err: "no such host", Name: "secret-host"}}, "could not resolve the host"},
		{"tls unknown authority", &url.Error{Err: x509.UnknownAuthorityError{}}, "TLS: certificate not trusted"},
		{"tls hostname", &url.Error{Err: x509.HostnameError{Host: "secret-host"}}, "TLS: certificate not trusted"},
		{"timeout", &url.Error{Err: context.DeadlineExceeded}, "timed out after 30 s"},
		{"other", errors.New("something odd with http://secret-host/?token=abc"), ""},
	}
	for _, c := range cases {
		got := transportCause(c.err, 30*time.Second)
		if got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
		if strings.Contains(got, "secret-host") || strings.Contains(got, "token") {
			t.Errorf("%s: the cause names the URL: %q", c.name, got)
		}
	}
}

func TestNoAnswerNamesTheCauseOnARealConnection(t *testing.T) {
	// refused: a loopback port nobody listens on
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()
	st, data, _ := realDo(connection{URL: "http://" + addr, Token: "tok"}, "GET", "/minigw/v1/projects", nil, nil)
	if st != 0 || string(data) != "connection refused" {
		t.Errorf("refused: %d %q", st, data)
	}
	if got := gatewayAnswer(st, data, http.Header{}).Error(); got != "no answer from the gateway (connection refused)" {
		t.Errorf("sentence: %q", got)
	}
	// reset: a server that accepts and drops the connection with a reset
	l, err = net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.(*net.TCPConn).SetLinger(0)
			c.Close()
		}
	}()
	st, data, _ = realDo(connection{URL: "http://" + l.Addr().String(), Token: "tok"}, "GET", "/minigw/v1/projects", nil, nil)
	if st != 0 || string(data) != "connection reset" {
		t.Errorf("reset: %d %q", st, data)
	}
	// a bad address
	if st, data, _ = realDo(connection{URL: "http://bad host\x7f", Token: "tok"}, "GET", "/", nil, nil); st != 0 || string(data) != "bad gateway address" {
		t.Errorf("bad address: %d %q", st, data)
	}
}

func TestNoAnswerSentenceWithoutAndWithACause(t *testing.T) {
	if got := gatewayAnswer(0, nil, nil).Error(); got != "no answer from the gateway" {
		t.Errorf("%q", got)
	}
	if got := gatewayAnswer(0, []byte("timed out after 30 s\x1b[31m\nx"), nil).Error(); strings.ContainsAny(got, "\x1b\n") || !strings.HasPrefix(got, "no answer from the gateway (timed out after 30 s") {
		t.Errorf("%q", got)
	}
}
