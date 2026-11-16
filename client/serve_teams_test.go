package main

import (
	"fmt"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGatewayTeamRoutes(t *testing.T) {
	f := newCoreFx(t, nil)
	team, err := f.s.core.CreateTeam(t0, "Directory")
	f.ok(err)
	f.ok(f.s.core.AddTeamMember(t0, team.ID, f.uid["ana"]))
	f.ok(f.s.core.AddTeamProject(t0, team.ID, f.pid["eureko"]))
	path := fmt.Sprintf("/minigw/v1/teams/%d", team.ID)
	r := f.call("ana", "GET", path, "")
	status(t, r, 200)
	if !strings.Contains(r.body, "Directory") || strings.Contains(r.body, "eureko") {
		t.Fatalf("directory leaks unreadable project: %s", r.body)
	}
	status(t, f.call("cene", "GET", path, ""), 404)
	adminPeerCheckFn = func(net.Conn) error { return errAgentPeer }
	defer func() { adminPeerCheckFn = adminPeerCheck }()
	rq := httptest.NewRequest("GET", path, nil)
	rq.Header.Set("Authorization", "Bearer tok")
	w := httptest.NewRecorder()
	f.s.projectHTTP(w, &req{Request: rq, conn: &serveConn{peer: "127.0.0.1", device: &serveDevice{ID: devID(f.dev["ana"]), Name: "ana"}}})
	if w.Code != 403 || w.Body.String() != noAccessBody {
		t.Fatalf("agent peer: %d %s", w.Code, w.Body.String())
	}
	adminPeerCheckFn = adminPeerCheck
	stranger := newDevice("unbound")
	f.s.update(func(c *serveConfig) error {
		c.Devices = append(c.Devices, serveDevice{ID: devID(stranger), Name: "unbound"})
		return nil
	})
	for _, r := range []resp{f.raw(stranger, "tok", "GET", "/minigw/v1/teams", ""), f.raw(nil, "tok", "GET", path, ""), f.raw(f.dev["ana"], "", "GET", path, "")} {
		if r.status != 403 || r.body != noAccessBody {
			t.Fatalf("team identity refusal: %d %s", r.status, r.body)
		}
	}
	f.ok(f.s.core.SetUserActive(t0, f.uid["ana"], false))
	r = f.call("ana", "GET", path, "")
	if r.status != 403 || r.body != noAccessBody {
		t.Fatalf("inactive user: %d %s", r.status, r.body)
	}
}
