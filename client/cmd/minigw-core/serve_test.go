package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"minigw/core"
	"minigw/coreapi"
)

var testSecret = []byte("0123456789abcdef0123456789abcdef")

func testAPI(t *testing.T) *coreapi.API {
	t.Helper()
	st, err := core.Open(t.TempDir(), core.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return coreapi.New(coreapi.Options{Store: func() *core.Store { return st }})
}

// start serves one end of an in-memory connection and returns the other end and the result of serve.
func start(t *testing.T, api *coreapi.API, timeout time.Duration) (net.Conn, chan error) {
	t.Helper()
	return startWith(t, api, nil, timeout)
}

func startWith(t *testing.T, api *coreapi.API, host *coreapi.CalloutHost, timeout time.Duration) (net.Conn, chan error) {
	t.Helper()
	a, b := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- serve(context.Background(), b, testSecret, api, host, timeout) }()
	t.Cleanup(func() { a.Close() })
	return a, done
}

func send(t *testing.T, c net.Conn, m msg) {
	t.Helper()
	if err := writeMsg(c, m); err != nil {
		t.Fatal(err)
	}
}

// clientHandshake is the app's side; secret lets a test use a wrong one.
func clientHandshake(t *testing.T, c net.Conn, secret []byte, lo, hi int) (reply msg) {
	t.Helper()
	return clientHandshakeWith(t, c, secret, lo, hi, false)
}

func clientHandshakeWith(t *testing.T, c net.Conn, secret []byte, lo, hi int, callouts bool) (reply msg) {
	t.Helper()
	send(t, c, msg{Type: "hello", Min: lo, Max: hi, Nonce: "cn", Callouts: callouts})
	r, err := readMsg(c)
	if err != nil {
		t.Fatalf("hello answer: %v", err)
	}
	if r.Type != "hello_ok" {
		return r
	}
	if r.MAC != mac(secret, "srv", r.Version, "cn", r.Nonce) {
		return msg{Type: "bad_server_mac"}
	}
	send(t, c, msg{Type: "auth", MAC: mac(secret, "cli", r.Version, r.Nonce, "cn")})
	r, err = readMsg(c)
	if err != nil {
		return msg{Type: "closed"}
	}
	return r
}

func TestHandshakeThenRequests(t *testing.T) {
	c, done := start(t, testAPI(t), time.Second)
	if r := clientHandshake(t, c, testSecret, 1, 1); r.Type != "ready" {
		t.Fatalf("handshake: %+v", r)
	}
	cl := coreapi.NewClient(c)
	r, err := cl.Do(context.Background(), coreapi.Context{Kind: coreapi.KindInternal},
		coreapi.Request{Method: "POST", Path: "/internal/epoch", Body: []byte(`{}`)})
	if err != nil || r.Status != 200 {
		t.Fatalf("epoch: %v %+v", err, r)
	}
	r, err = cl.Do(context.Background(), coreapi.Context{Kind: coreapi.KindMember}, coreapi.Request{Method: "GET", Path: "/minigw/v1/projects"})
	if err != nil || r.Status != 403 || !strings.Contains(string(r.Body), "no_project_access") {
		t.Fatalf("no device: %v %+v", err, r)
	}
	c.Close() // the app quits: EOF is a clean end
	if err := <-done; err != nil {
		t.Fatalf("EOF ended with %v", err)
	}
}

func TestStaleSecretServesNothing(t *testing.T) {
	c, done := start(t, testAPI(t), time.Second)
	// the app with an OLD secret does not even accept the helper's MAC, so it sends no auth: nothing is served either way
	if r := clientHandshake(t, c, []byte("an old secret of another start!!"), 1, 1); r.Type != "bad_server_mac" {
		t.Fatalf("got %+v", r)
	}
	c.Close()
	<-done

	c, done = start(t, testAPI(t), time.Second)
	// a peer that answers with a wrong auth is refused and no request is read
	send(t, c, msg{Type: "hello", Min: 1, Max: 1, Nonce: "cn"})
	r, _ := readMsg(c)
	send(t, c, msg{Type: "auth", MAC: mac([]byte("wrong"), "cli", 1, r.Nonce, "cn")})
	if r, _ = readMsg(c); r.Type != "error" || r.Code != "auth_failed" {
		t.Fatalf("got %+v", r)
	}
	if err := <-done; err == nil || !strings.Contains(err.Error(), "auth failed") {
		t.Fatalf("serve: %v", err)
	}
	// a request frame written now is not answered (the connection is closed)
	if err := coreapi.WriteRequest(c, 1, coreapi.Context{Kind: coreapi.KindInternal}, coreapi.Request{Method: "POST", Path: "/internal/epoch", Body: []byte(`{}`)}); err == nil {
		if _, _, err := coreapi.ReadResponse(c); err == nil {
			t.Fatal("a refused peer was served")
		}
	}
}

func TestVersionMismatch(t *testing.T) {
	for _, v := range [][2]int{{coreapi.ProtocolVersion + 1, coreapi.ProtocolVersion + 3}, {0, coreapi.ProtocolVersion - 1}} {
		c, done := start(t, testAPI(t), time.Second)
		r := clientHandshake(t, c, testSecret, v[0], v[1])
		if r.Type != "error" || r.Code != "version_mismatch" || r.ServerMin != coreapi.ProtocolVersion || r.ServerMax != coreapi.ProtocolVersion {
			t.Fatalf("%v: %+v", v, r)
		}
		if err := <-done; err == nil || !strings.Contains(err.Error(), "version mismatch") {
			t.Fatalf("serve: %v", err)
		}
	}
	// a request frame of another version after a good handshake is answered once, then the connection ends
	c, done := start(t, testAPI(t), time.Second)
	clientHandshake(t, c, testSecret, 1, 1)
	body, _ := json.Marshal(map[string]any{"v": coreapi.ProtocolVersion + 1, "id": 7, "context": coreapi.Context{Kind: coreapi.KindInternal}, "request": coreapi.Request{}})
	c.Write(binary.BigEndian.AppendUint32(nil, uint32(len(body))))
	c.Write(body)
	if _, _, err := coreapi.ReadResponse(c); err == nil || !strings.Contains(err.Error(), "version") {
		t.Fatalf("answer: %v", err)
	}
	if err := <-done; !errors.Is(err, coreapi.ErrVersion) {
		t.Fatalf("serve: %v", err)
	}
}

func TestFrameBounds(t *testing.T) {
	// a handshake frame over maxHandshake: closed before the payload is read (only the header was sent)
	c, done := start(t, testAPI(t), time.Second)
	c.Write(binary.BigEndian.AppendUint32(nil, maxHandshake+1))
	if err := <-done; err == nil {
		t.Fatal("serve accepted a huge handshake frame")
	}
	// a request frame over coreapi.MaxFrame: answered with a protocol error and closed, the payload never read
	c, done = start(t, testAPI(t), time.Second)
	clientHandshake(t, c, testSecret, 1, 1)
	c.Write(binary.BigEndian.AppendUint32(nil, coreapi.MaxFrame+1))
	if _, _, err := coreapi.ReadResponse(c); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("answer: %v", err)
	}
	if err := <-done; !errors.Is(err, coreapi.ErrFrameTooLarge) {
		t.Fatalf("serve: %v", err)
	}
	// the largest request body the API takes still fits in a frame
	c, done = start(t, testAPI(t), time.Second)
	clientHandshake(t, c, testSecret, 1, 1)
	big := coreapi.Request{Method: "POST", Path: "/internal/scope", Body: []byte(strings.Repeat("a", coreapi.MaxBody))}
	if err := coreapi.WriteRequest(c, 1, coreapi.Context{Kind: coreapi.KindInternal}, big); err != nil {
		t.Fatal(err)
	}
	if _, r, err := coreapi.ReadResponse(c); err != nil || r.Status != 400 {
		t.Fatalf("MaxBody request: %v %+v", err, r)
	}
	c.Close()
	<-done
}

func TestRequestDeadlineIsA503(t *testing.T) {
	st, err := core.Open(t.TempDir(), core.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	released := make(chan struct{})
	api := coreapi.New(coreapi.Options{Store: func() *core.Store { return st },
		AfterAdmission: func(ctx context.Context, _ coreapi.Context, _ coreapi.Request) { <-ctx.Done(); close(released) }})
	if _, err := st.CreateUser(context.Background(), "ana"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetBinding(context.Background(), "dev", 1); err != nil {
		t.Fatal(err)
	}
	c, _ := start(t, api, 100*time.Millisecond)
	clientHandshake(t, c, testSecret, 1, 1)
	began := time.Now()
	_, r, err := func() (uint64, coreapi.Response, error) {
		coreapi.WriteRequest(c, 1, coreapi.Context{Kind: coreapi.KindMember, DeviceID: "dev", Approved: true}, coreapi.Request{Method: "GET", Path: "/minigw/v1/projects"})
		return coreapi.ReadResponse(c)
	}()
	if err != nil || r.Status != 503 || !strings.Contains(string(r.Body), "core_unavailable") {
		t.Fatalf("deadline answer: %v %+v", err, r)
	}
	<-released
	if d := time.Since(began); d > 2*time.Second {
		t.Fatalf("answered after %v", d)
	}
}

func TestWatchParentCallsExitWhenItChanges(t *testing.T) {
	called := make(chan struct{})
	go watchParent(-1, 5*time.Millisecond, func() { close(called) }) // no process has this ppid
	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("watchParent did not fire")
	}
}

// The upstream routes of the helper: the app does the work through callouts on the connection. Without "callouts" in its hello the
// routes answer 501; with it, the token goes to the app once (connect) and never comes back in an answer.
func TestUpstreamCalloutsThroughTheHelper(t *testing.T) {
	newAPI := func(t *testing.T, host *coreapi.CalloutHost) *coreapi.API {
		st, err := core.Open(t.TempDir(), core.Options{})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { st.Close() })
		return coreapi.New(coreapi.Options{Store: func() *core.Store { return st }, Upstream: host, UpstreamBudget: 5 * time.Second})
	}
	admin := coreapi.Context{Kind: coreapi.KindAdmin}
	put := coreapi.Request{Method: "PUT", Path: "/upstreams/hub", Body: []byte(`{"address":"hub.example","token":"SECRET-TOKEN#KEY"}`)}

	// an app without callouts
	host := &coreapi.CalloutHost{}
	c, _ := startWith(t, newAPI(t, host), host, time.Second)
	if r := clientHandshakeWith(t, c, testSecret, 1, 1, false); r.Type != "ready" {
		t.Fatal(r)
	}
	if r, err := coreapi.NewClient(c).Do(context.Background(), admin, put); err != nil || r.Status != 501 {
		t.Fatalf("%v %+v", err, r)
	}

	// an app with callouts
	host = &coreapi.CalloutHost{}
	c, _ = startWith(t, newAPI(t, host), host, 5*time.Second)
	if r := clientHandshakeWith(t, c, testSecret, 1, 1, true); r.Type != "ready" {
		t.Fatal(r)
	}
	var seen []coreapi.CalloutRequest
	cl := coreapi.NewClient(c)
	cl.OnCallout = func(q coreapi.CalloutRequest) coreapi.CalloutResult {
		seen = append(seen, q)
		switch q.Op {
		case coreapi.CalloutConnect:
			return coreapi.CalloutResult{Device: "DEVICEID", Where: "the Keychain"}
		case coreapi.CalloutDo:
			return coreapi.CalloutResult{Status: 403, Body: []byte(`{"error":{"message":"No project access"}}`)}
		case coreapi.CalloutInfo:
			return coreapi.CalloutResult{Device: "DEVICEID"}
		}
		return coreapi.CalloutResult{}
	}
	r, err := cl.Do(context.Background(), admin, put)
	if err != nil || r.Status != 200 {
		t.Fatalf("%v %+v", err, r)
	}
	if strings.Contains(string(r.Body), "SECRET-TOKEN") || !strings.Contains(string(r.Body), "DEVICEID") || !strings.Contains(string(r.Body), "No project access") {
		t.Fatalf("answer: %s", r.Body)
	}
	if len(seen) != 2 || seen[0].Op != coreapi.CalloutConnect || seen[0].Token != "SECRET-TOKEN#KEY" || seen[0].Address != "hub.example:8765" || seen[1].Op != coreapi.CalloutDo || seen[1].Token != "" {
		t.Fatalf("%+v", seen)
	}
	if r, err = cl.Do(context.Background(), admin, coreapi.Request{Method: "GET", Path: "/upstreams"}); err != nil || r.Status != 200 || strings.Contains(string(r.Body), "SECRET") || !strings.Contains(string(r.Body), "hub.example:8765") {
		t.Fatalf("%v %s", err, r.Body)
	}
}
