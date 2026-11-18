package coreapi

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"minigw/core"
)

// The member route: any bound member of the project, by their own rights; a stranger and a bad id are refused; only GET.
func TestManifestMemberRoute(t *testing.T) {
	f := newFixture(t)
	path := fmt.Sprintf("/minigw/v1/projects/%d/kb/manifest", f.pid["fakture"])
	get := func(dev, p string) Response { return f.api.Handle(bg, member(dev), Request{Method: "GET", Path: p}) }
	ids := func(dev string) (out []int64) {
		r := get(dev, path)
		if r.Status != 200 {
			t.Fatalf("%s: %d %s", dev, r.Status, r.Body)
		}
		var m core.Manifest
		if err := json.Unmarshal(r.Body, &m); err != nil || m.V != 1 {
			t.Fatalf("%s: %v %s", dev, err, r.Body)
		}
		for _, n := range m.Notes {
			out = append(out, n.ID)
		}
		if strings.Contains(string(r.Body), "t-structure") || strings.Contains(string(r.Body), "body") {
			t.Fatalf("title or body in %s", r.Body)
		}
		return
	}
	// developer: structure and data, never her own private note; reader: structure only
	if got := ids("dev-ana"); len(got) != 2 || got[0] != f.nid["structure"] || got[1] != f.nid["data"] {
		t.Fatalf("developer: %v", got)
	}
	if got := ids("dev-eva"); len(got) != 1 || got[0] != f.nid["structure"] {
		t.Fatalf("reader: %v", got)
	}
	// a stranger to the project (only a grant the other way round) and an unbound device are refused like any member route
	if r := get("dev-cene", path); r.Status != 200 { // eureko's developer reads fakture structure through the grant: same rights as GET
		t.Fatalf("grant reader: %d %s", r.Status, r.Body)
	}
	if r := get("dev-nobody", path); r.Status == 200 {
		t.Fatalf("an unbound device: %d", r.Status)
	}
	for _, bad := range []string{"/minigw/v1/projects/0/kb/manifest", "/minigw/v1/projects/-1/kb/manifest", "/minigw/v1/projects/x/kb/manifest", "/minigw/v1/projects/999999/kb/manifest"} {
		if r := get("dev-ana", bad); r.Status != 400 && r.Status != 404 {
			t.Errorf("%s: %d", bad, r.Status)
		}
	}
	for _, m := range []string{"POST", "PUT", "DELETE", "PATCH"} {
		if r := f.api.Handle(bg, member("dev-ana"), Request{Method: m, Path: path, Body: []byte(`{}`)}); r.Status == 200 || r.Status == 201 {
			t.Errorf("%s: %d", m, r.Status)
		}
	}
}

// The admin route for the dry run is the owner's alone (no member route, and not reachable with another kind), needs a host, and refuses a real pull.
func TestUpstreamPullIsTheOwnersAndDryOnly(t *testing.T) {
	f := newFixture(t)
	admin := Context{Kind: KindAdmin}
	if r := f.api.Handle(bg, admin, Request{Method: "POST", Path: "/upstream-pull", Body: []byte(`{"dryRun":true}`)}); r.Status != 501 {
		t.Errorf("without a host: %d %s", r.Status, r.Body)
	}
	for _, mp := range []string{"/minigw/v1/upstream-pull", "/minigw/v1/projects/1/upstream-pull", "/upstream-pull"} {
		if r := f.api.Handle(bg, member("dev-fani"), Request{Method: "POST", Path: mp, Body: []byte(`{"dryRun":true}`)}); r.Status != 404 {
			t.Errorf("member %s: %d %s", mp, r.Status, r.Body)
		}
	}
	api := New(Options{Store: func() *core.Store { return f.st }, Upstream: noHost{}})
	for _, c := range []struct {
		method, body string
		want         int
	}{
		{"GET", ``, 404},
		{"POST", `{"dryRun":true,"x":1}`, 400},
		{"POST", `{"dryRun":true,"project":1,"team":1}`, 400},
		{"POST", `{"dryRun":true,"upstream":"Bad_Name"}`, 400},
		{"POST", `{"project":1}`, 400},               // a real pull does not exist yet
		{"POST", `{"dryRun":true,"project":1}`, 400}, // no link
	} {
		if r := api.Handle(bg, admin, Request{Method: c.method, Path: "/upstream-pull", Body: []byte(c.body)}); r.Status != c.want {
			t.Errorf("%s %s: %d %s, want %d", c.method, c.body, r.Status, r.Body, c.want)
		}
	}
}
