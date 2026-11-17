package coreapi

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"minigw/core"
)

// Callouts: the helper has no network and no credential store; the app that started it does the upstream work for it. While the helper
// handles ONE request it may send the app a callout frame on the same connection and wait for the answer frame (the connection is one
// request at a time, so nothing else is in flight). Both frames carry the id of the request being handled.
//
//	helper -> app   {"v":1,"id":N,"callout":{op, upstream, address, token, method, path, header, body}}
//	app -> helper   {"v":1,"id":N,"callout_result":{status, body, retryAfter, error, device, where}}
//
// Only an app that said "callouts" in its hello gets them; for any other the upstream routes answer 501. The token travels once, in a
// connect callout, over this private connection; the helper does not keep it, log it or answer with it.
const (
	CalloutDo      = "do"
	CalloutConnect = "connect"
	CalloutInfo    = "info"
	CalloutForget  = "forget"

	// MaxCalloutBody: the most of an upstream's answer that is passed back (the core parses at most 1 MiB of it).
	MaxCalloutBody = (1 << 20) + 1
	calloutTimeout = 25 * time.Second
)

type CalloutRequest struct {
	Op       string            `json:"op"`
	Upstream string            `json:"upstream"`
	Address  string            `json:"address,omitempty"`
	Token    string            `json:"token,omitempty"`
	Method   string            `json:"method,omitempty"`
	Path     string            `json:"path,omitempty"`
	Header   map[string]string `json:"header,omitempty"`
	Body     []byte            `json:"body,omitempty"`
}

type CalloutResult struct {
	Status     int    `json:"status"`
	Body       []byte `json:"body,omitempty"`
	RetryAfter int    `json:"retryAfter,omitempty"` // seconds
	Error      string `json:"error,omitempty"`
	Device     string `json:"device,omitempty"`
	Where      string `json:"where,omitempty"`
}

type wireCallout struct {
	V       int             `json:"v"`
	ID      uint64          `json:"id"`
	Callout *CalloutRequest `json:"callout,omitempty"`
}

type wireCalloutResult struct {
	V      int            `json:"v"`
	ID     uint64         `json:"id"`
	Result *CalloutResult `json:"callout_result,omitempty"`
}

// WriteCalloutResult is what the app sends back.
func WriteCalloutResult(w io.Writer, id uint64, r CalloutResult) error {
	return writeFrame(w, wireCalloutResult{V: ProtocolVersion, ID: id, Result: &r})
}

// CalloutHost is the UpstreamHost of a helper: every call is a callout to the app on the connection that the helper is serving.
type CalloutHost struct {
	mu      sync.Mutex
	conn    io.ReadWriter
	id      uint64
	enabled bool
}

// Attach is called by the serve loop: the connection, and whether the app can answer callouts.
func (h *CalloutHost) Attach(conn io.ReadWriter, enabled bool) {
	h.mu.Lock()
	h.conn, h.enabled = conn, enabled
	h.mu.Unlock()
}

// Begin names the request that is being handled (the callouts of it carry its id).
func (h *CalloutHost) Begin(id uint64) {
	h.mu.Lock()
	h.id = id
	h.mu.Unlock()
}

func (h *CalloutHost) Available() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.enabled && h.conn != nil
}

func (h *CalloutHost) call(req CalloutRequest) (CalloutResult, error) {
	h.mu.Lock()
	conn, id := h.conn, h.id
	h.mu.Unlock()
	if conn == nil {
		return CalloutResult{}, errors.New("no app to do this")
	}
	if c, ok := conn.(net.Conn); ok {
		c.SetDeadline(time.Now().Add(calloutTimeout))
		defer c.SetDeadline(time.Time{})
	}
	if err := writeFrame(conn, wireCallout{V: ProtocolVersion, ID: id, Callout: &req}); err != nil {
		return CalloutResult{}, err
	}
	var f wireCalloutResult
	if err := readFrame(conn, &f); err != nil {
		return CalloutResult{}, err
	}
	if f.V != ProtocolVersion || f.ID != id || f.Result == nil {
		return CalloutResult{}, errors.New("the app answered a callout out of step")
	}
	if len(f.Result.Body) > MaxCalloutBody {
		return CalloutResult{}, errors.New("an answer of the upstream is too large")
	}
	return *f.Result, nil
}

func (h *CalloutHost) Do(_ context.Context, upstream, method, path string, header map[string]string, body []byte) (int, []byte, time.Duration, error) {
	r, err := h.call(CalloutRequest{Op: CalloutDo, Upstream: upstream, Method: method, Path: path, Header: header, Body: body})
	if err != nil {
		return 0, nil, 0, err
	}
	if r.Error != "" {
		return 0, nil, 0, errors.New(r.Error)
	}
	return r.Status, r.Body, time.Duration(r.RetryAfter) * time.Second, nil
}

func (h *CalloutHost) Connect(name, address, token string) (UpstreamInfo, error) {
	r, err := h.call(CalloutRequest{Op: CalloutConnect, Upstream: name, Address: address, Token: token})
	if err != nil {
		return UpstreamInfo{}, err
	}
	if r.Error != "" {
		return UpstreamInfo{}, errors.New(r.Error)
	}
	return UpstreamInfo{Device: r.Device, Where: r.Where}, nil
}

func (h *CalloutHost) Info(name string) UpstreamInfo {
	r, err := h.call(CalloutRequest{Op: CalloutInfo, Upstream: name})
	if err != nil || r.Error != "" {
		return UpstreamInfo{}
	}
	return UpstreamInfo{Device: r.Device}
}

func (h *CalloutHost) Forget(name string) { h.call(CalloutRequest{Op: CalloutForget, Upstream: name}) }

var _ core.UpstreamTransport = (*CalloutHost)(nil)
