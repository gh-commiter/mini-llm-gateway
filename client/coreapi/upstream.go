package coreapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strconv"
	"strings"
	"time"

	"minigw/core"
)

// UpstreamHost is what only the host process can do for an upstream: keep its secret (the credential store, never the core database)
// and talk to it as the gateway's own device. A host without it (the Mac app's core helper, for now) answers the routes with 501.
type UpstreamHost interface {
	core.UpstreamTransport
	// Connect stores the upstream's token line ("Copy Token": <token>#<gateway key>) and makes the device key of this gateway for it.
	// It refuses a token of this same gateway.
	// deviceName: how this gateway shows up on the upstream's device list (chosen by the owner or a default; never the computer's name).
	Connect(name, address, token, deviceName string) (UpstreamInfo, error)
	Info(name string) UpstreamInfo
	// Forget removes the secret; an error says it could not be removed (it must then be reported, not ignored).
	Forget(name string) error
	// Available: false when the host cannot do upstream work in this process (the Mac helper without a connected app): the routes answer 501.
	Available() bool
}

type upstreamCtxKey struct{}

// WithUpstream gives a request the host that may serve it: a helper has one per CONNECTION (the one whose app answers callouts), so an
// upstream route that arrives on another connection finds none and answers 501 instead of using someone else's.
func WithUpstream(ctx context.Context, h UpstreamHost) context.Context {
	return context.WithValue(ctx, upstreamCtxKey{}, h)
}

func (a *API) upstreamHost(ctx context.Context) UpstreamHost {
	if h, ok := ctx.Value(upstreamCtxKey{}).(UpstreamHost); ok {
		return h
	}
	return a.o.Upstream
}

// UpstreamInfo is what an answer may say about an upstream's credentials: the device ID (a public key, what the upstream's owner has
// to allow) and where the secret is kept. Never the token or the private key.
type UpstreamInfo struct {
	Device string `json:"device"`
	Where  string `json:"where"`
}

type upstreamJSON struct {
	Name    string `json:"name"`
	Address string `json:"address"`
	Device  string `json:"device"`
}

// adminUpstreams serves /upstreams[/{name}[/links/{project}]] and POST /upstream-push (the owner's routes).
func (a *API) adminUpstreams(ctx context.Context, st *core.Store, method string, seg []string, q url.Values, body []byte, failErr func(error) (int, any, bool)) (int, any, bool) {
	fail := func(status int, msg string) (int, any, bool) { return status, errorBody(status, msg), true }
	h := a.upstreamHost(ctx)
	if h == nil || !h.Available() {
		return fail(501, "this gateway cannot send to an upstream here")
	}
	num := func(v string) (int64, bool) { n, err := strconv.ParseInt(v, 10, 64); return n, err == nil && n > 0 }
	switch {
	case method == "GET" && len(seg) == 1:
		us, err := st.ListUpstreams(ctx)
		if err != nil {
			return failErr(err)
		}
		var project, team int64
		if v := q.Get("project"); v != "" {
			project, _ = num(v)
		}
		if v := q.Get("team"); v != "" {
			team, _ = num(v)
		}
		ls, err := st.UpstreamLinks(ctx, project, team)
		if err != nil {
			return failErr(err)
		}
		out := []upstreamJSON{}
		for _, u := range us {
			out = append(out, upstreamJSON{u.Name, u.Address, h.Info(u.Name).Device})
		}
		rows := []adminLinkJSON{}
		for _, l := range ls {
			rows = append(rows, toAdminLink(l))
		}
		return 200, map[string]any{"upstreams": out, "links": rows}, true
	case method == "GET" && len(seg) == 3 && seg[2] == "probe":
		// An explicit check by the owner (the page asks when its detail view opens): the first contact of an ordinary client, as `add` makes
		// it. Nothing is written. `state` is a code a panel can name; `probe` is the text of the CLI.
		u, err := st.GetUpstream(ctx, seg[1])
		if err != nil {
			return failErr(err)
		}
		info := h.Info(u.Name)
		text := ProbeUpstream(ctx, h, u.Name, "this gateway", info.Device)
		return 200, map[string]any{"name": u.Name, "address": u.Address, "device": info.Device, "state": probeState(text), "probe": text}, true
	case method == "PUT" && len(seg) == 2:
		var b struct {
			Address    string `json:"address"`
			Token      string `json:"token"`
			DeviceName string `json:"deviceName"`
		}
		if decodeStrict(body, &b) != nil || b.Token == "" {
			return fail(400, "bad JSON body (address, token, deviceName)")
		}
		if !core.ValidUpstreamName(seg[1]) {
			return failErr(core.ErrRefusal{Msg: "upstream name: lowercase letters, digits and -, up to 32"})
		}
		if b.DeviceName == "" {
			b.DeviceName = "gateway " + shortID(st.GatewayID())
		}
		if !core.ValidDeviceName(b.DeviceName) {
			return failErr(core.ErrRefusal{Msg: "device name: printable ASCII, 1 to 64 characters"})
		}
		a.upMu.Lock() // check, connect and add are one step: two adds of the same name cannot delete each other's secret
		defer a.upMu.Unlock()
		if _, err := st.GetUpstream(ctx, seg[1]); err == nil {
			return failErr(core.ErrRefusal{Msg: "an upstream named " + seg[1] + " exists: remove it first (minigw gw upstream remove " + seg[1] + ")"})
		} else if !errors.Is(err, core.ErrNotFound) {
			return failErr(err)
		}
		addr, err := core.NormalizeUpstreamAddress(b.Address)
		if err != nil {
			return failErr(err)
		}
		info, err := h.Connect(seg[1], addr, b.Token, b.DeviceName)
		if err != nil {
			return failErr(core.ErrRefusal{Msg: err.Error()})
		}
		if err := st.AddUpstream(ctx, seg[1], addr); err != nil {
			if ferr := h.Forget(seg[1]); ferr != nil {
				a.o.Log("upstream %s: credentials could not be removed after a failed add: %v", seg[1], ferr)
			}
			return failErr(err)
		}
		// The owner's own command is the reason to contact the upstream: what a client's first contact is, to tell what it still needs from
		// its owner. A model route is what registers an unknown device as waiting for approval (the member routes only refuse it).
		probe := ProbeUpstream(ctx, h, seg[1], b.DeviceName, info.Device)
		return 200, map[string]any{"name": seg[1], "address": addr, "device": info.Device, "where": info.Where, "probe": probe}, true
	case method == "DELETE" && len(seg) == 2:
		a.upMu.Lock()
		defer a.upMu.Unlock()
		err := st.RemoveUpstream(ctx, seg[1])
		// The credentials go whatever the database said (a row that is gone must not leave a secret behind); a failure to remove them is
		// reported, not swallowed.
		if ferr := h.Forget(seg[1]); ferr != nil {
			return fail(500, "upstream "+seg[1]+": the credentials could not be removed ("+ferr.Error()+"): remove them by hand")
		}
		if err != nil {
			return failErr(err)
		}
		return 200, map[string]any{"ok": true}, true
	case (method == "PUT" || method == "DELETE") && len(seg) == 4 && seg[2] == "links":
		id, ok := num(seg[3])
		if !ok {
			return fail(400, "bad project or team id")
		}
		var b struct {
			Team        bool   `json:"team"`
			Remote      string `json:"remote"`
			Classes     string `json:"classes"`
			WithPrivate bool   `json:"withPrivate"`
		}
		if len(body) > 0 && decodeStrict(body, &b) != nil {
			return fail(400, "bad JSON body (team, remote, classes, withPrivate)")
		}
		var project, team int64 = id, 0
		if b.Team {
			project, team = 0, id
		}
		if method == "DELETE" && len(body) == 0 && q.Get("team") == "true" { // a DELETE has no body in a panel: ?team=true names a team's link
			b.Team = true
		}
		if b.Team {
			project, team = 0, id
		}
		if method == "PUT" && b.Remote == "" { // the same name there, as the CLI does it (a form leaves it empty to mean that)
			b.Remote = a.localName(ctx, st, id, b.Team)
		}
		var err error
		if method == "PUT" {
			err = st.LinkUpstream(ctx, project, team, seg[1], b.Remote, b.Classes, b.WithPrivate)
		} else {
			err = st.UnlinkUpstream(ctx, project, team, seg[1])
		}
		if err != nil {
			return failErr(err)
		}
		return 200, map[string]any{"ok": true}, true
	}
	return fail(404, "not found")
}

// adminUpstreamPush serves POST /upstream-push {project, team, dryRun}: the push by hand.
func (a *API) adminUpstreamPush(ctx context.Context, st *core.Store, method string, body []byte, failErr func(error) (int, any, bool)) (int, any, bool) {
	fail := func(status int, msg string) (int, any, bool) { return status, errorBody(status, msg), true }
	h := a.upstreamHost(ctx)
	if h == nil || !h.Available() {
		return fail(501, "this gateway cannot send to an upstream here")
	}
	if method != "POST" {
		return fail(404, "not found")
	}
	var b struct {
		Project  int64  `json:"project"`
		Team     int64  `json:"team"`
		Upstream string `json:"upstream"`
		After    int64  `json:"after"`
		DryRun   bool   `json:"dryRun"`
	}
	if decodeStrict(body, &b) != nil || b.Project < 0 || b.Team < 0 || b.Project > 0 && b.Team > 0 || b.Upstream != "" && !core.ValidUpstreamName(b.Upstream) {
		return fail(400, "bad JSON body (project or team, upstream, after, dryRun)")
	}
	sleep := a.o.UpstreamSleep
	if sleep == nil {
		sleep = func(d time.Duration) bool { time.Sleep(d); return true }
	}
	if a.o.UpstreamBudget > 0 { // one call of a helper has a time limit: the run ends with "more" and the caller asks again
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, a.o.UpstreamBudget)
		defer cancel()
		inner := sleep
		sleep = func(d time.Duration) bool {
			if dl, ok := ctx.Deadline(); ok && time.Until(dl) < d+time.Second {
				return false
			}
			return inner(d)
		}
	}
	res, err := st.PushUpstream(ctx, h, b.Project, b.Team, b.Upstream, b.After, b.DryRun, sleep)
	if err != nil {
		return failErr(err)
	}
	rows := []adminPushJSON{}
	for _, r := range res {
		rows = append(rows, adminPushJSON{r, strings.Join(r.Lines, "\n")})
	}
	return 200, map[string]any{"pushes": rows}, true
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

// ProbeUpstream is the first contact of an ordinary client: GET /v1/models, which an upstream that does not know the device answers with a
// refusal AND lists the device as waiting for approval (name and ID from the device proof, the client description from the header). Once
// the device is allowed, the member routes tell whether it is bound to a user.
func ProbeUpstream(ctx context.Context, h UpstreamHost, upstream, deviceName, deviceID string) string {
	status, data, _, err := h.Do(ctx, upstream, "GET", "/v1/models", nil, nil)
	var e struct{ Error struct{ Message string } }
	json.Unmarshal(data, &e)
	switch {
	case err != nil || status == 0:
		return "the upstream did not answer: check the address, and that its gateway is running and reachable from here"
	case status == 401:
		return "the upstream refused the token: copy it again (Settings, General, Copy Token on the upstream)"
	case status == 403:
		return "waiting for approval on the upstream as " + deviceName + " (" + deviceID + "): its owner runs: minigw gw devices allow " + deviceID
	case status != 200:
		return "the upstream answered HTTP " + strconv.Itoa(status)
	}
	status, data, _, err = h.Do(ctx, upstream, "GET", "/minigw/v1/projects", nil, nil)
	json.Unmarshal(data, &e)
	switch {
	case err != nil || status == 0:
		return "the upstream did not answer the second request"
	case status == 200:
		return "connected: the device is allowed and bound to a user there"
	case status == 403 && strings.Contains(e.Error.Message, "device list"): // an upstream on this computer lets a model request pass without the list
		return "waiting for approval on the upstream as " + deviceName + " (" + deviceID + "): its owner runs: minigw gw devices allow " + deviceID
	case status == 403 && (e.Error.Message == "" || strings.Contains(e.Error.Message, "bound")):
		return "the device is allowed there, but not bound to a user: its owner runs: minigw gw bind " + deviceID + " <user>"
	case status == 403:
		return "the device is allowed there; the upstream says: " + core.UpText(e.Error.Message, 200)
	}
	return "the upstream answered HTTP " + strconv.Itoa(status)
}

// adminLinkJSON is a link as the owner's list shows it, plus what a panel would otherwise have to derive: a unique id, the kind of place
// (project or team), the id to name the link in a path (a project id, or a TEAM id: not the hidden segment) and the same split as two
// numbers for a push body, and the classes as text. The stored fields stay as they were (the CLI reads them).
type adminLinkJSON struct {
	core.UpstreamLinkInfo
	ID          string `json:"id"`
	Place       string `json:"place"`
	LinkID      int64  `json:"linkId"`
	ProjectID   int64  `json:"projectId"`
	TeamID      int64  `json:"teamId"`
	ClassesText string `json:"classesText"`
}

func toAdminLink(l core.UpstreamLinkInfo) adminLinkJSON {
	j := adminLinkJSON{UpstreamLinkInfo: l, Place: "project", LinkID: l.Project, ProjectID: l.Project}
	if team, ok := core.SegmentTeam(l.Project); ok && l.Team {
		j.Place, j.LinkID, j.ProjectID, j.TeamID = "team", team, 0, team
	}
	j.ID = j.Place + "-" + strconv.FormatInt(j.LinkID, 10) + "-" + l.Upstream
	var cs []string
	for _, c := range l.Classes {
		cs = append(cs, string(c))
	}
	j.ClassesText = strings.Join(cs, ", ")
	return j
}

// adminPushJSON is a push report with its lines as one text (a panel shows a text block, not a list of strings).
type adminPushJSON struct {
	core.UpstreamPush
	LinesText string `json:"linesText"`
}

// probeState names what ProbeUpstream said: unreachable, token (refused), waiting (for approval), notbound, connected or other.
func probeState(text string) string {
	switch {
	case strings.HasPrefix(text, "the upstream did not answer"):
		return "unreachable"
	case strings.HasPrefix(text, "the upstream refused the token"):
		return "token"
	case strings.HasPrefix(text, "waiting for approval"):
		return "waiting"
	case strings.HasPrefix(text, "the device is allowed there, but not bound"):
		return "notbound"
	case strings.HasPrefix(text, "connected:"):
		return "connected"
	}
	return "other"
}

// localName is the short name of a project, or the name of a team: what a link names on the upstream when the owner gives no other.
func (a *API) localName(ctx context.Context, st *core.Store, id int64, team bool) string {
	if team {
		ts, _ := st.AdminListTeams(ctx)
		for _, t := range ts {
			if t.ID == id {
				return t.Name
			}
		}
		return ""
	}
	ps, _ := st.AdminListProjects(ctx)
	for _, p := range ps {
		if p.ID == id {
			return p.Slug
		}
	}
	return ""
}
