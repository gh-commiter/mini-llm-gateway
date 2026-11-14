package coreapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"minigw/core"
)

// The internal calls: what a gateway needs from the core that is not an HTTP route of the API. Each is
// POST /internal/<name> with a JSON body and a JSON answer; the structs below are the wire shapes. The adapter may
// call them in-process (API.Call) or over the helper socket; the result is the same.

// --- admit: a request names a project (agent request with the project header) ---

// AdmitRequest: the project of an agent request. Project is the client's header value ("" = none).
type AdmitRequest struct {
	Device   string `json:"device"`   // the proven device ("" = none)
	Approved bool   `json:"approved"` // on the approved list now
	Project  string `json:"project,omitempty"`
}

// AdmitReply: OK false carries the refusal (Status, Type, Message, RetryAfter) the adapter shows in its own error
// shape; the request is NEVER run without the project. OK true: User is the user of the principal (with a project) and
// RunID/Project/Epoch are the notes capability (Epoch read before the membership check).
type AdmitReply struct {
	OK         bool   `json:"ok"`
	Status     int    `json:"status,omitempty"`
	Type       string `json:"type,omitempty"`
	Message    string `json:"message,omitempty"`
	RetryAfter int    `json:"retryAfter,omitempty"`
	User       int64  `json:"user,omitempty"`
	Project    int64  `json:"project,omitempty"`
	RunID      string `json:"runId,omitempty"`
	Epoch      int64  `json:"epoch,omitempty"`
}

func (a *API) admit(ctx context.Context, in AdmitRequest) AdmitReply {
	if in.Project == "" {
		return AdmitReply{OK: true}
	}
	deny := AdmitReply{Status: 403, Type: "no_project_access", Message: "No project access"}
	st := a.store()
	if st == nil {
		return AdmitReply{Status: 503, Type: "core_unavailable", Message: "The project database is not available"}
	}
	id, err := strconv.ParseInt(in.Project, 10, 64)
	if err != nil || id < 1 || in.Device == "" {
		return deny
	}
	epoch, _ := st.Epoch(ctx)
	p, err := st.Principal(ctx, in.Device, in.Approved)
	if err == nil {
		_, err = st.ResolveForLink(ctx, p, strconv.FormatInt(id, 10)) // a DIRECT role, as link-check
	}
	if err != nil {
		var lim core.ErrLimit
		if errors.As(err, &lim) {
			return AdmitReply{Status: 429, Type: "limit", Message: "Too many requests", RetryAfter: RetryAfterSeconds(lim.RetryAfter)}
		}
		a.o.Log("project %s refused: %v", in.Project, err)
		return deny
	}
	return AdmitReply{OK: true, User: p.UserID, Project: id, RunID: randToken()[:16], Epoch: epoch}
}

// --- scope: the conversation key of a request (section 5) ---

// ScopeRequest: Site names the keying site ("agent" for Codex, Grok and Copilot: kind, owner, cwd; "cli" for Claude CLI:
// session, dir); Fields are its parts in order. Project nil = no project.
type ScopeRequest struct {
	Site    string      `json:"site"`
	Fields  []string    `json:"fields"`
	Project *ScopeParts `json:"project,omitempty"`
}

type ScopeParts struct {
	User    int64 `json:"user"`
	Project int64 `json:"project"`
	Epoch   int64 `json:"epoch"`
}

type ScopeReply struct {
	Key string `json:"key"`
}

// ConversationScope: without a project the 0.5.26 key byte for byte (the fields joined by "|"); with a project its own
// namespace: a NUL prefix (an HTTP header value cannot hold a NUL, so no 0.5.26 key starts with it) and a JSON array,
// whose encoding of the fields is unambiguous.
func ConversationScope(site string, fields []string, p *ScopeParts) string {
	if p == nil {
		return strings.Join(fields, "|")
	}
	parts := []any{site}
	for _, f := range fields {
		parts = append(parts, f)
	}
	b, _ := json.Marshal(append(parts, p.User, p.Project, p.Epoch))
	return "\x00project\x00" + string(b)
}

// --- agent-home: the key of an agent's home for a run with a project (review M2) ---

type AgentHomeRequest struct {
	Kind    string `json:"kind"`
	User    int64  `json:"user"`
	Project int64  `json:"project"`
	Epoch   int64  `json:"epoch"`
}

// AgentHomeReply: the home is <device state>/proj/<DirName>/<kind>. Homes whose name starts with Prefix and carries a
// lower epoch are dead and may be removed; at most MaxHomes are kept per device.
type AgentHomeReply struct {
	DirName  string `json:"dirName"`
	Relative string `json:"relative"`
	Prefix   string `json:"prefix"`
	Epoch    int64  `json:"epoch"`
	MaxHomes int    `json:"maxHomes"`
}

func AgentHome(in AgentHomeRequest) AgentHomeReply {
	name := fmt.Sprintf("%d-%d-e%d", in.User, in.Project, in.Epoch)
	return AgentHomeReply{DirName: name, Relative: "proj/" + name + "/" + in.Kind, Prefix: fmt.Sprintf("%d-%d-e", in.User, in.Project), Epoch: in.Epoch, MaxHomes: 20}
}

// --- the rest ---

type DeviceRequest struct {
	Device string `json:"device"`
}
type DevicesRequest struct {
	Devices []string `json:"devices"`
}
type DevicesReply struct {
	Done []string `json:"done"` // the devices whose epoch bump is done, in order; the first failure stops the list
}
type EpochReply struct {
	Epoch int64 `json:"epoch"`
}
type OwnerRequest struct {
	Name string `json:"name"` // the OS account name; "" = "owner"
}
type BoundReply struct {
	User int64 `json:"user"`
}
type EventRequest struct {
	Event core.UsageEvent `json:"event"`
}

// LostRequest: events the adapter could not deliver (its own queue overflowed while the helper was down); the core shows
// them as a gap, as for events its own queue drops.
type LostRequest struct {
	Count int64     `json:"count"`
	From  time.Time `json:"from"`
	To    time.Time `json:"to"`
}

// internal serves POST /internal/<name>.
func (a *API) internal(ctx context.Context, r Request) Response {
	name, ok := strings.CutPrefix(r.Path, "/internal/")
	if !ok || r.Method != "POST" {
		return jsonErr(404, "not_found", "Not found")
	}
	bad := jsonErr(400, "invalid", "invalid JSON body")
	st := a.store()
	switch name {
	case "epoch":
		if st == nil {
			return unavailable()
		}
		e, _ := st.Epoch(ctx)
		return resp(200, EpochReply{e})
	case "admit":
		var in AdmitRequest
		if decodeStrict(r.Body, &in) != nil {
			return bad
		}
		return resp(200, a.admit(ctx, in))
	case "scope":
		var in ScopeRequest
		if decodeStrict(r.Body, &in) != nil {
			return bad
		}
		return resp(200, ScopeReply{ConversationScope(in.Site, in.Fields, in.Project)})
	case "agent-home":
		var in AgentHomeRequest
		if decodeStrict(r.Body, &in) != nil || in.Kind == "" || strings.ContainsAny(in.Kind, `/\`) {
			return bad
		}
		return resp(200, AgentHome(in))
	case "device-revoked":
		var in DeviceRequest
		if decodeStrict(r.Body, &in) != nil || in.Device == "" {
			return bad
		}
		if st == nil {
			return unavailable()
		}
		if err := st.DeviceRevoked(ctx, in.Device); err != nil {
			a.o.Log("DeviceRevoked %s: %v", in.Device, err)
			return jsonErr(500, "internal", "the epoch could not be raised")
		}
		return resp(200, map[string]any{"ok": true})
	case "reconcile":
		var in DevicesRequest
		if decodeStrict(r.Body, &in) != nil {
			return bad
		}
		if st == nil {
			return unavailable()
		}
		out := DevicesReply{Done: []string{}}
		for _, d := range in.Devices {
			if err := st.DeviceRevoked(ctx, d); err != nil {
				a.o.Log("reconcile %s: %v", d, err)
				break
			}
			out.Done = append(out.Done, d)
		}
		return resp(200, out)
	case "ensure-owner":
		var in OwnerRequest
		if decodeStrict(r.Body, &in) != nil {
			return bad
		}
		if st == nil {
			return unavailable()
		}
		if err := ensureOwner(ctx, st, in.Name); err != nil {
			a.o.Log("ensure-owner: %v", err)
			return jsonErr(500, "internal", "Internal error")
		}
		return resp(200, map[string]any{"ok": true})
	case "bound-user":
		var in DeviceRequest
		if decodeStrict(r.Body, &in) != nil {
			return bad
		}
		return resp(200, BoundReply{a.UserOf(ctx, in.Device)})
	case "event-begin", "event-finish":
		var in EventRequest
		if decodeStrict(r.Body, &in) != nil {
			return bad
		}
		if name == "event-begin" {
			a.EventBegin(in.Event)
		} else {
			a.EventFinish(in.Event)
		}
		return Response{Status: 202, Header: http.Header{"Content-Type": {"application/json"}}, Body: []byte(`{"queued":true}`)}
	case "event-lost":
		var in LostRequest
		if decodeStrict(r.Body, &in) != nil || in.Count < 1 {
			return bad
		}
		if a.rec != nil {
			a.rec.noteLoss(in.Count, in.From, in.To)
			a.rec.kick() // a gap that arrives alone (the app quit with events queued) is stored now
		}
		return Response{Status: 202, Header: http.Header{"Content-Type": {"application/json"}}, Body: []byte(`{"queued":true}`)}
	}
	return jsonErr(404, "not_found", "Not found")
}

// ensureOwner creates the owner's user row once (named after the OS account); the mark gives no power, it only lets the
// owner be a project member.
func ensureOwner(ctx context.Context, st *core.Store, name string) error {
	users, err := st.ListUsers(ctx)
	if err != nil || len(users) > 0 {
		return err
	}
	if name == "" {
		name = "owner"
	}
	u, err := st.CreateUser(ctx, name)
	if err != nil {
		return err
	}
	return st.SetUserOwner(ctx, u.ID, true)
}

// Call makes an internal call in-process through the same path as the wire: it fails for a status of 400 or more.
func (a *API) Call(ctx context.Context, name string, in, out any) error {
	body, _ := json.Marshal(in)
	r := a.internal(ctx, Request{Method: "POST", Path: "/internal/" + name, Body: body})
	if r.Status >= 400 {
		var e struct {
			Error struct{ Type, Message string }
		}
		json.Unmarshal(r.Body, &e)
		return fmt.Errorf("%s: %d %s", name, r.Status, e.Error.Message)
	}
	if out != nil {
		return json.Unmarshal(r.Body, out)
	}
	return nil
}

// --- usage events from the gateway ---

// EventBegin and EventFinish enqueue; they never block (see Recorder). Begin records the event as begun (an
// interrupted run keeps its project), Finish completes it (or, when Begin never reached the database, inserts it whole).
func (a *API) EventBegin(e core.UsageEvent)  { a.rec.enqueue(recMsg{kind: evBegin, ev: e}) }
func (a *API) EventFinish(e core.UsageEvent) { a.rec.enqueue(recMsg{kind: evFinish, ev: e}) }

// UserOf: the user a device is bound to, from the core's in-memory snapshot (no database I/O on the request path); when
// there is no core, the last binding seen for the device (attribution of an event only, never an access decision).
func (a *API) UserOf(ctx context.Context, device string) int64 {
	var cache *Recorder = a.rec
	if st := a.store(); st != nil {
		if id, err := st.BoundUser(ctx, device); err == nil && id != 0 {
			if cache != nil {
				cache.bound.Store(device, id)
			}
			return id
		}
	}
	if cache != nil {
		if v, ok := cache.bound.Load(device); ok {
			return v.(int64)
		}
	}
	return 0
}
