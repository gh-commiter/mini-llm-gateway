package coreapi

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// The wire form between a gateway adapter and a helper that hosts the API: length-prefixed JSON frames, in order, one
// request at a time per connection. A frame is a 4-byte big-endian length (of the JSON that follows) and the JSON.
// Every frame carries the protocol version; a frame longer than MaxFrame is refused before its payload is read.

// MaxFrame is the largest frame: a request body of MaxBody (base64 inside JSON) fits with room to spare.
const MaxFrame = 4 << 20

type wireRequest struct {
	V       int     `json:"v"`
	ID      uint64  `json:"id"`
	Context Context `json:"context"`
	Request Request `json:"request"`
}

type wireResponse struct {
	V        int      `json:"v"`
	ID       uint64   `json:"id"`
	Response Response `json:"response"`
	Error    string   `json:"error,omitempty"` // a protocol-level failure (the API itself answers in Response)
}

var (
	ErrFrameTooLarge = errors.New("coreapi: frame too large")
	ErrVersion       = errors.New("coreapi: unsupported protocol version")
)

func writeFrame(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(b) > MaxFrame {
		return ErrFrameTooLarge
	}
	buf := make([]byte, 4, 4+len(b))
	binary.BigEndian.PutUint32(buf, uint32(len(b)))
	_, err = w.Write(append(buf, b...))
	return err
}

func readFrame(r io.Reader, v any) error {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > MaxFrame {
		return ErrFrameTooLarge
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(r, b); err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// WriteRequest and ReadRequest, WriteResponse and ReadResponse are the two directions.
func WriteRequest(w io.Writer, id uint64, c Context, r Request) error {
	return writeFrame(w, wireRequest{V: ProtocolVersion, ID: id, Context: c, Request: r})
}

func ReadRequest(rd io.Reader) (id uint64, c Context, r Request, err error) {
	for {
		var f struct {
			wireRequest
			Result *CalloutResult `json:"callout_result"`
		}
		if err = readFrame(rd, &f); err != nil {
			return
		}
		if f.V != ProtocolVersion {
			return 0, Context{}, Request{}, fmt.Errorf("%w: %d", ErrVersion, f.V)
		}
		if f.Result != nil { // the answer to a callout that the helper stopped waiting for: it is dropped, never taken for a request
			continue
		}
		return f.ID, f.Context, f.Request, nil
	}
}

func WriteResponse(w io.Writer, id uint64, r Response, protoErr string) error {
	return writeFrame(w, wireResponse{V: ProtocolVersion, ID: id, Response: r, Error: protoErr})
}

func ReadResponse(rd io.Reader) (id uint64, r Response, err error) {
	var f wireResponse
	if err = readFrame(rd, &f); err != nil {
		return
	}
	if f.V != ProtocolVersion {
		return 0, Response{}, fmt.Errorf("%w: %d", ErrVersion, f.V)
	}
	if f.Error != "" {
		return f.ID, Response{}, errors.New(f.Error)
	}
	return f.ID, f.Response, nil
}

// Serve answers requests from one connection until it closes. A request that cannot be decoded ends the connection
// (the peer is not speaking this protocol); an oversized or wrong-version frame is answered once, then the connection ends.
func (a *API) Serve(ctx context.Context, conn io.ReadWriter) error {
	for {
		id, c, r, err := ReadRequest(conn)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			if errors.Is(err, ErrFrameTooLarge) || errors.Is(err, ErrVersion) {
				WriteResponse(conn, id, Response{}, err.Error())
			}
			return err
		}
		if err := WriteResponse(conn, id, a.Handle(ctx, c, r), ""); err != nil {
			return err
		}
	}
}

// Client sends requests over a connection to a helper that runs Serve; one at a time.
type Client struct {
	mu   sync.Mutex
	conn io.ReadWriter
	next uint64
	// OnCallout answers the callouts of the helper (callout.go) while a request is handled; nil: the helper gets no answer and the
	// request fails.
	OnCallout func(CalloutRequest) CalloutResult
}

func NewClient(conn io.ReadWriter) *Client { return &Client{conn: conn} }

// Do is Handle over the wire.
func (c *Client) Do(ctx context.Context, cx Context, r Request) (Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.next++
	if err := WriteRequest(c.conn, c.next, cx, r); err != nil {
		return Response{}, err
	}
	for {
		var f struct {
			wireResponse
			Callout *CalloutRequest `json:"callout"`
		}
		if err := readFrame(c.conn, &f); err != nil {
			return Response{}, err
		}
		if f.V != ProtocolVersion {
			return Response{}, fmt.Errorf("%w: %d", ErrVersion, f.V)
		}
		if f.Callout != nil {
			if c.OnCallout == nil || f.ID != c.next {
				return Response{}, errors.New("coreapi: unexpected callout")
			}
			res := c.OnCallout(*f.Callout)
			res.Seq = f.Callout.Seq
			if err := WriteCalloutResult(c.conn, f.ID, res); err != nil {
				return Response{}, err
			}
			continue
		}
		if f.Error != "" {
			return Response{}, errors.New(f.Error)
		}
		if f.ID != c.next {
			return Response{}, fmt.Errorf("coreapi: answer %d to request %d", f.ID, c.next)
		}
		return f.Response, nil
	}
}
