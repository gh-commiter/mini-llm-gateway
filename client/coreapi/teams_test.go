package coreapi

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestTeamRoutes(t *testing.T) {
	f := newFixture(t)
	admin := Context{Kind: KindAdmin}
	call := func(c Context, method, path, body string, want int) Response {
		t.Helper()
		r := f.api.Handle(bg, c, Request{Method: method, Path: path, Body: []byte(body)})
		if r.Status != want {
			t.Fatalf("%s %s: %d %s, want %d", method, path, r.Status, r.Body, want)
		}
		return r
	}
	create := func(name string) int64 {
		t.Helper()
		r := call(admin, "POST", "/teams", fmt.Sprintf(`{"name":%q}`, name), 200)
		var out struct{ Team TeamJSON }
		if err := json.Unmarshal(r.Body, &out); err != nil {
			t.Fatal(err)
		}
		return out.Team.ID
	}
	id := create("Shared")
	other := create("Hidden team")
	path := fmt.Sprintf("/teams/%d", id)
	memberPath := fmt.Sprintf("/minigw/v1/teams/%d", id)
	for _, uid := range []int64{f.uid["ana"], f.uid["cene"]} {
		call(admin, "PUT", fmt.Sprintf("%s/members/%d", path, uid), "", 200)
	}
	call(admin, "PUT", fmt.Sprintf("/teams/%d/members/%d", other, f.uid["ana"]), "", 200)
	for _, pid := range []int64{f.pid["fakture"], f.pid["eureko"]} {
		call(admin, "PUT", fmt.Sprintf("%s/projects/%d", path, pid), "", 200)
	}
	r := call(member("dev-ana"), "GET", "/minigw/v1/teams", "", 200)
	var teams []TeamJSON
	if err := json.Unmarshal(r.Body, &teams); err != nil || len(teams) != 2 {
		t.Fatalf("multi-team: %s %v", r.Body, err)
	}
	r = call(member("dev-ana"), "GET", memberPath, "", 200)
	var detail TeamDetailJSON
	if err := json.Unmarshal(r.Body, &detail); err != nil {
		t.Fatal(err)
	}
	if len(detail.Members) != 2 || len(detail.Projects) != 1 || detail.Projects[0].Slug != "fakture" || strings.Contains(string(r.Body), "eureko") {
		t.Fatalf("visible directory: %s", r.Body)
	}
	// A grantee already reads the source project, so it remains visible in this directory.
	r = call(member("dev-cene"), "GET", memberPath, "", 200)
	if err := json.Unmarshal(r.Body, &detail); err != nil || len(detail.Projects) != 2 {
		t.Fatalf("grant visibility: %s %v", r.Body, err)
	}
	hidden := call(member("dev-cene"), "GET", fmt.Sprintf("/minigw/v1/teams/%d", other), "", 404)
	missing := call(member("dev-cene"), "GET", "/minigw/v1/teams/999999", "", 404)
	if !bytes.Equal(hidden.Body, missing.Body) {
		t.Fatalf("hidden team differs: %s %s", hidden.Body, missing.Body)
	}
	// A request without a device, and an agent peer, keep the fixed body.
	for _, c := range []Context{{Kind: KindMember}, {Kind: KindMember, DeviceID: "dev-ana", Approved: true, AgentPeer: true}} {
		r = call(c, "GET", "/minigw/v1/teams", "", 403)
		if !bytes.Equal(r.Body, NoProjectAccess().Body) {
			t.Fatalf("identity envelope: %s", r.Body)
		}
	}
	// When the device itself is the reason, the refusal names the missing condition and the owner's command,
	// with the caller's own device id.
	for _, c := range []struct {
		ctx    Context
		format string
	}{
		{member("unbound"), "no project access: this device is not bound to a user (the owner runs: minigw gw bind %s USER)"},
		{{Kind: KindMember, DeviceID: "dev-ana"}, "no project access: this device is not on the gateway's device list (the owner runs: minigw gw devices allow %s)"},
	} {
		r = call(c.ctx, "GET", "/minigw/v1/teams", "", 403)
		want := fmt.Sprintf(c.format, c.ctx.DeviceID)
		var b struct {
			Error struct{ Message, Type string }
		}
		if err := json.Unmarshal(r.Body, &b); err != nil || b.Error.Type != "no_project_access" || b.Error.Message != want {
			t.Fatalf("device refusal for %q: %s (want %q)", c.ctx.DeviceID, r.Body, want)
		}
	}
	call(admin, "PATCH", path, `{"name":"Renamed"}`, 200)
	call(admin, "PATCH", path, `{"archived":true}`, 200)
	call(admin, "GET", path, "", 200)
	call(member("dev-ana"), "GET", memberPath, "", 200)
	call(admin, "PUT", fmt.Sprintf("%s/members/%d", path, f.uid["eva"]), "", 400)
	call(admin, "DELETE", fmt.Sprintf("%s/projects/%d", path, f.pid["fakture"]), "", 200)
	r = call(member("dev-ana"), "GET", memberPath, "", 200)
	if err := json.Unmarshal(r.Body, &detail); err != nil || len(detail.Projects) != 0 {
		t.Fatalf("project removal: %s %v", r.Body, err)
	}
	call(admin, "DELETE", fmt.Sprintf("%s/members/%d", path, f.uid["ana"]), "", 200)
	call(member("dev-ana"), "GET", memberPath, "", 404)
	call(admin, "PATCH", path, `{"name":"x","archived":false}`, 400)
	call(admin, "GET", path+"/members/1/extra", "", 404)
	call(admin, "POST", path, "", 404)
	if err := f.st.SetUserActive(bg, f.uid["ana"], false); err != nil {
		t.Fatal(err)
	}
	call(member("dev-ana"), "GET", "/minigw/v1/teams", "", 403)
}

func TestTeamPrincipalRecheckedAfterAdmission(t *testing.T) {
	f := newFixture(t)
	team, err := f.st.CreateTeam(bg, "Team")
	if err != nil {
		t.Fatal(err)
	}
	if err = f.st.AddTeamMember(bg, team.ID, f.uid["ana"]); err != nil {
		t.Fatal(err)
	}
	f.api.o.AfterAdmission = func(_ context.Context, _ Context, _ Request) {
		if err := f.st.SetBinding(bg, "dev-ana", f.uid["eva"]); err != nil {
			t.Fatal(err)
		}
	}
	r := f.api.Handle(bg, member("dev-ana"), get("/minigw/v1/teams"))
	if r.Status != 403 || !bytes.Equal(r.Body, NoProjectAccess().Body) {
		t.Fatalf("stale principal: %d %s", r.Status, r.Body)
	}
}
