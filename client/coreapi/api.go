// Package coreapi is the one place where "a request and who is asking" becomes "status, headers, body" for the
// 0.6.0 core: member routes, core admin routes, the run's notes routes and the internal calls a gateway makes.
// minigw serve calls it in-process; the Mac helper calls it over a socket (wire.go). It does not authenticate
// anybody: the caller (the gateway adapter) proves the device, checks the approved list, authenticates the admin and
// the run port, and passes the result in a Context. See docs/core-adapter.md.
package coreapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"minigw/core"
)

// ProtocolVersion of the wire form and of the internal calls.
const ProtocolVersion = 1

// MaxBody is the largest request body the API takes; the adapter reads at most this many bytes (64 KiB of body can
// grow to 384 KiB of \u escapes) and sets BodyTooLarge for more.
const MaxBody = 512 << 10

// Kind says what the adapter has authenticated.
type Kind string

const (
	KindMember   Kind = "member"   // a member's device (proven in its tunnel)
	KindAdmin    Kind = "admin"    // the owner (admin authentication passed, no agent peer)
	KindRun      Kind = "run"      // an agent run's notes capability (the run port's token and peer checks passed)
	KindInternal Kind = "internal" // a call of the gateway itself (/internal/<name>)
)

// Context is what the adapter vouches for.
type Context struct {
	Kind      Kind         `json:"kind"`
	DeviceID  string       `json:"device,omitempty"`    // member: the device proven in this tunnel ("" = none)
	Approved  bool         `json:"approved,omitempty"`  // member, run: the device is on the approved list NOW
	AgentPeer bool         `json:"agentPeer,omitempty"` // member: the caller is a process of one of the gateway's agents
	Run       *core.RunCap `json:"run,omitempty"`       // run: the capability
}

// Request: only what the API reads. Header carries Idempotency-Key (and whatever the adapter chooses to pass to
// the AfterAdmission test hook).
type Request struct {
	Method         string      `json:"method"`
	Path           string      `json:"path"` // admin: the escaped path below the admin prefix
	RawQuery       string      `json:"query,omitempty"`
	Header         http.Header `json:"header,omitempty"`
	Body           []byte      `json:"body,omitempty"`
	BodyTooLarge   bool        `json:"bodyTooLarge,omitempty"`   // the adapter stopped reading at MaxBody
	BodyUnreadable bool        `json:"bodyUnreadable,omitempty"` // a read error or a deadline
}

// Response: Status 0 from an admin call means "not a core route" (the adapter goes on).
type Response struct {
	Status int         `json:"status"`
	Header http.Header `json:"header,omitempty"`
	Body   []byte      `json:"body,omitempty"`
}

// Options of an API.
type Options struct {
	Store       func() *core.Store // the store, nil while it is not usable
	Unavailable func() string      // why it is not usable (for the admin 503 text)
	Dir         string             // private folder for the usage spool
	Log         func(format string, args ...any)
	// AfterAdmission is a test hook: called after a member request has its principal and before the route runs.
	AfterAdmission func(ctx context.Context, c Context, r Request)
	// PanelProviders are the panel providers besides the built-in one (the id "builtin" is reserved).
	PanelProviders []PanelProvider
}

// API is the core's request handler.
type API struct {
	o   Options
	rec *Recorder
}

// New makes an API. Start the usage recorder with StartRecorder when the process runs long (not for tests of routes).
func New(o Options) *API {
	if o.Log == nil {
		o.Log = func(string, ...any) {}
	}
	if o.Unavailable == nil {
		o.Unavailable = func() string { return "" }
	}
	return &API{o: o}
}

func (a *API) store() *core.Store {
	if a.o.Store == nil {
		return nil
	}
	return a.o.Store()
}

// Handle is the one entry point.
func (a *API) Handle(ctx context.Context, c Context, r Request) Response {
	switch c.Kind {
	case KindMember:
		return a.member(ctx, c, r)
	case KindAdmin:
		return a.admin(ctx, r)
	case KindRun:
		return a.run(ctx, c, r)
	case KindInternal:
		return a.internal(ctx, r)
	}
	return jsonErr(500, "internal", "Internal error")
}

// --- responses and errors ---

func resp(status int, v any, hdr ...string) Response {
	b, _ := json.Marshal(v)
	h := http.Header{"Content-Type": {"application/json"}}
	for i := 0; i+1 < len(hdr); i += 2 {
		h.Set(hdr[i], hdr[i+1])
	}
	return Response{Status: status, Header: h, Body: b}
}

// jsonErr is the member envelope {"error":{"type","message"}}.
func jsonErr(status int, typ, msg string, extra ...map[string]any) Response {
	e := map[string]any{"type": typ, "message": msg}
	for _, m := range extra {
		for k, v := range m {
			e[k] = v
		}
	}
	return resp(status, map[string]any{"error": e})
}

// errResponse maps a core error to status and envelope in ONE place. An unknown error is a 500 with a fixed message;
// the detail goes only to the log.
func (a *API) errResponse(err error) Response {
	var rev core.ErrRevision
	var lim core.ErrLimit
	var inv core.ErrInvalid
	switch {
	case errors.Is(err, core.ErrNoProjectAccess):
		return jsonErr(403, "no_project_access", "No project access")
	case errors.Is(err, core.ErrNotFound):
		return jsonErr(404, "not_found", "Not found")
	case errors.Is(err, core.ErrForbidden):
		return jsonErr(403, "forbidden", "Not allowed")
	case errors.Is(err, core.ErrArchived):
		return jsonErr(403, "archived", "The project is archived")
	case errors.As(err, &rev):
		return jsonErr(409, "revision_mismatch", "The note has changed", map[string]any{"revision": rev.Current})
	case errors.Is(err, core.ErrIdempotency):
		return jsonErr(409, "idempotency_conflict", "The Idempotency-Key was used with another payload")
	case errors.Is(err, core.ErrPendingLimit):
		return jsonErr(413, "pending_limit", core.ErrPendingLimit.Error())
	case errors.Is(err, core.ErrTooLarge):
		return jsonErr(413, "too_large", "Too large")
	case errors.As(err, &lim):
		r := jsonErr(429, "limit", "Too many requests")
		r.Header.Set("Retry-After", strconv.Itoa(RetryAfterSeconds(lim.RetryAfter)))
		return r
	case errors.As(err, &inv):
		return jsonErr(400, "invalid", inv.Error())
	}
	a.o.Log("core error: %v", err)
	return jsonErr(500, "internal", "Internal error")
}

// RetryAfterSeconds rounds a wait up to whole seconds, at least one.
func RetryAfterSeconds(d time.Duration) int { return max(1, int((d+time.Second-1)/time.Second)) }

func noAccess() Response { return jsonErr(403, "no_project_access", "No project access") }

// NoProjectAccess is the fixed 403 an adapter answers itself (a request that fails the gateway token or comes through a
// run port). testdata/no_project_access.json holds its exact body; the Go gateway's own copy and the Swift one are
// asserted against it.
func NoProjectAccess() Response { return noAccess() }

func unavailable() Response {
	return jsonErr(503, "core_unavailable", "The project database is not available")
}

// Unavailable is the 503 an adapter answers itself when the core cannot be asked at all (the Mac helper is down).
// testdata/unavailable.json holds its exact body: the Go and Swift tests both assert it, so the two cannot drift.
func Unavailable() Response { return unavailable() }

// decodeStrict: one JSON object, unknown fields refused.
func decodeStrict(body []byte, v any) error {
	d := json.NewDecoder(strings.NewReader(string(body)))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	if d.Decode(&struct{}{}) != io.EOF {
		return errors.New("data after the JSON object")
	}
	return nil
}
