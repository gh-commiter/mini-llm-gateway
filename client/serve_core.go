package main

// The 0.6.0 core on the gateway (docs/plans/contracts-0.6.0.md): opening the store, the member routes under
// /minigw/v1/projects and the error mapping. The policy itself is in package core.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os/user"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"minigw/core"
	"minigw/coreapi"
)

const (
	projectsPrefix = "/minigw/v1/projects"
	usagePath      = "/minigw/v1/usage"
	coreDirName    = "core"
)

// openCore opens <config dir>/core. A gateway that cannot open it still starts: project routes answer 503, the
// models work. Not called for the offline server (minigw gw without a running gateway).
func (s *server) openCore() {
	var st *core.Store
	var err error
	if why := s.coreOpenRefusal(s.conf()); why != "" { // #106: agents that could read the database: not opened unless accepted
		err = errors.New(why)
	} else {
		s.logCoreAccepted(s.conf())
		st, err = core.Open(filepath.Join(s.cfgDir, coreDirName), core.Options{ProtectDir: protectDir})
	}
	if err == nil {
		s.core = st // the API reads the store through s.core; nothing serves yet (openCore runs before serve)
		err = s.ensureOwner(st)
		if err == nil {
			err = s.reconcileRevocations(st) // before the project routes are enabled
		}
		if err != nil {
			s.core = nil
			st.Close()
		}
	}
	if err != nil {
		s.coreErr = err
		s.log.Printf("WARNING: the core database cannot be used, project routes answer 503 core_unavailable (models work): %v", err)
		s.api.StartRecorder() // usage events go to the spool until a start with a usable core
		return
	}
	s.api.StartRecorder()
}

func (s *server) closeCore() {
	s.api.StopRecorder() // drain the usage queue while the core is still open
	if s.core != nil {
		s.core.Close()
	}
}

// ensureOwner: the owner's user row, named after the OS account (the rest is coreapi's ensure-owner).
func (s *server) ensureOwner(st *core.Store) error {
	name := "owner"
	if u, err := user.Current(); err == nil && u.Username != "" {
		name = u.Username[strings.LastIndexAny(u.Username, `\`)+1:]
	}
	return s.api.Call(context.Background(), "ensure-owner", coreapi.OwnerRequest{Name: name}, nil)
}

// --- the adapter to package coreapi: authentication here, policy and routes there ---

// newAPI builds the core's request handler for this server (docs/core-adapter.md).
func (s *server) newAPI() *coreapi.API {
	return coreapi.New(coreapi.Options{
		Store: func() *core.Store { return s.core },
		Unavailable: func() string {
			if s.coreErr != nil {
				return s.coreErr.Error()
			}
			return ""
		},
		Dir: s.cfgDir,
		Log: s.log.Printf,
		AfterAdmission: func(ctx context.Context, _ coreapi.Context, _ coreapi.Request) {
			if rq, _ := ctx.Value(reqCtxKey{}).(*req); rq != nil && s.coreHook != nil {
				s.coreHook(rq, "admitted")
			}
		},
	})
}

type reqCtxKey struct{}

// writeAPI writes a coreapi Response: its headers, the request id and the body.
func writeAPI(w http.ResponseWriter, rid string, r coreapi.Response) int {
	for k, v := range r.Header {
		for _, x := range v {
			w.Header().Add(k, x)
		}
	}
	w.Header().Set("request-id", rid)
	w.WriteHeader(r.Status)
	if len(r.Body) > 0 {
		w.Write(r.Body)
	}
	return r.Status
}

// apiRequest builds the coreapi Request of an incoming request: the body is read here, with the same limit and
// deadline as readBody (the server has no global read timeout), and never more than coreapi.MaxBody.
func apiRequest(w http.ResponseWriter, r *req, escapedPath string) coreapi.Request {
	q := coreapi.Request{Method: r.Method, Path: escapedPath, RawQuery: r.URL.RawQuery, Header: http.Header{}}
	if v := r.Header.Get("Idempotency-Key"); v != "" {
		q.Header.Set("Idempotency-Key", v)
	}
	rc := http.NewResponseController(w)
	rc.SetReadDeadline(time.Now().Add(120 * time.Second))
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, coreapi.MaxBody))
	rc.SetReadDeadline(time.Time{})
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			q.BodyTooLarge = true
		} else {
			q.BodyUnreadable = true
		}
	}
	q.Body = data
	return q
}

// projectHTTP serves the member routes (/minigw/v1/projects…, /minigw/v1/usage). What stays here: the token, the run port
// exclusion, the agent-peer check and the device proven in this Noise tunnel with its approval now (NOT deviceAllowed:
// its loopback and allow-any-device rules do not apply). Everything else is coreapi.
func (s *server) projectHTTP(w http.ResponseWriter, r *req) int {
	if runOf(r) != nil || !s.authorized(r) {
		return writeAPI(w, r.rid, noProjectAccess())
	}
	c := coreapi.Context{Kind: coreapi.KindMember}
	// A process of one of this gateway's own agents may not use the member routes, as for the admin API (L7).
	if r.conn.loopback() {
		if err := adminPeerCheckFn(r.conn); err != nil {
			s.log.Printf("%s project route refused: %v", r.rid, err)
			c.AgentPeer = true
		}
	}
	if dev := r.conn.device; dev != nil { // proven in this Noise tunnel (readDevice); nil for plain loopback and plain remote
		c.DeviceID, c.Approved = dev.ID, s.conf().listed(dev.ID)
	}
	ctx := context.WithValue(r.Context(), reqCtxKey{}, r)
	return writeAPI(w, r.rid, s.api.Handle(ctx, c, apiRequest(w, r, r.URL.Path)))
}

// noProjectAccess: the fixed 403 body (also for a request that fails the token or comes from the run port).
func noProjectAccess() coreapi.Response {
	return coreapi.Response{Status: 403, Header: http.Header{"Content-Type": {"application/json"}},
		Body: []byte(`{"error":{"message":"No project access","type":"no_project_access"}}`)}
}

// clearRevocation removes a device id from the pending list once its epoch bump is done.
func (s *server) clearRevocation(id string) {
	s.update(func(c *serveConfig) error {
		c.PendingRevocations = slices.DeleteFunc(c.PendingRevocations, func(x string) bool { return x == id })
		return nil
	})
}

// reconcileRevocations does the epoch bumps that a device removal could not (A7): offline, core unavailable, failed
// write. The pending list lives in serve.json (the adapter's); the bump is coreapi's reconcile.
func (s *server) reconcileRevocations(st *core.Store) error {
	pending := s.conf().PendingRevocations
	if len(pending) == 0 {
		return nil
	}
	var out coreapi.DevicesReply
	if err := s.api.Call(context.Background(), "reconcile", coreapi.DevicesRequest{Devices: pending}, &out); err != nil {
		return err
	}
	for _, id := range out.Done {
		s.clearRevocation(id)
	}
	if len(out.Done) != len(pending) {
		return errors.New("a pending revocation could not be applied")
	}
	return nil
}
