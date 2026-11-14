package main

// 0.6.0 sec-fix F1/F2: the local proxy closes a connection whose head it cannot rewrite safely; the gateway refuses
// several X-Minigw-Project values. The rewrite table is the shared fixture (TestProjectParityRewriteHead).

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestProxyClosesOnUnsafeHeads(t *testing.T) {
	c := newClientFx(t)
	np, err := newNoiseProxy(c.base, c.f.s.key.PublicKey().Bytes(), c.f.dev["ana"])
	if err == nil {
		err = np.start()
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(np.stop)
	talk := func(parts ...string) string { // each part is a separate write; the reply (or "" when closed with nothing)
		conn, err := net.Dial("tcp", np.ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(10 * time.Second))
		for _, p := range parts {
			conn.Write([]byte(p))
			time.Sleep(30 * time.Millisecond)
		}
		b, _ := io.ReadAll(conn)
		return string(b)
	}
	if r := talk("GET /health/live HTTP/1.1\r\nHost: x\r\n", "x-minigw-project: 9\r\n", "\r\n"); !strings.HasPrefix(r, "HTTP/1.1 200") { // control: split head, forged header dropped
		t.Fatalf("valid head: %q", r)
	}
	for name, parts := range map[string][]string{
		"late obs-fold forged header":    {"GET /health/live HTTP/1.1\r\nHost: x\r\n", " X-Minigw-Project: 7\r\n", "\r\n"},
		"late space before colon":        {"GET /health/live HTTP/1.1\r\nHost: x\r\n", "X-Minigw-Project : 7\r\n", "\r\n"},
		"no end of head in 64 KiB":       {"GET /health/live HTTP/1.1\r\nHost: x\r\n", strings.Repeat("X-A: 1\r\n", 9000), "X-Minigw-Project: 7\r\n", "\r\n"},
		"forged header after 64 KiB too": {"GET /health/live HTTP/1.1\r\n" + strings.Repeat("a", 70<<10), "\r\nX-Minigw-Project: 7\r\n\r\n"},
	} {
		if r := talk(parts...); r != "" {
			t.Errorf("%s: answered %.60q", name, r)
		}
	}
}

func TestGatewayRefusesSeveralProjectHeaders(t *testing.T) {
	s, _ := testServer(t, nil)
	hr := httptest.NewRequest("POST", "/v1/messages", nil)
	hr.Header.Add("X-Minigw-Project", "1")
	hr.Header.Add("x-minigw-project", "2")
	w := httptest.NewRecorder()
	if st := s.attachProject(w, &req{Request: hr, conn: &serveConn{}, rid: "r"}); st != 400 && w.Code != 400 {
		t.Fatalf("status %d %d: %s", st, w.Code, w.Body)
	}
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "More than one") {
		t.Fatalf("not refused: %d %s", w.Code, w.Body)
	}
}
