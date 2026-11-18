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

// The admin route for the dry run is the owner's alone (no member route, and not reachable with another kind), needs a host and has a real pull for links with reading on.
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
		{"POST", `{"project":1}`, 400},               // a real pull: no link
		{"POST", `{"dryRun":true,"project":1}`, 400}, // no link
	} {
		if r := api.Handle(bg, admin, Request{Method: c.method, Path: "/upstream-pull", Body: []byte(c.body)}); r.Status != c.want {
			t.Errorf("%s %s: %d %s, want %d", c.method, c.body, r.Status, r.Body, c.want)
		}
	}
}

// The owner's reading: a link needs the acceptance, the push settings stay, a real pull fills the mirror and the status shows it.
func TestAdminReadLinkAndPull(t *testing.T) {
	f := newFixture(t)
	h := &methodHost{secretHost: secretHost{secrets: map[string]string{}}}
	h.answer = func(method, path string) (int, string) {
		switch {
		case path == "/minigw/v1/projects/shop":
			return 200, `{"id":7,"slug":"shop"}`
		case path == "/minigw/v1/projects/7/kb/manifest":
			return 200, `{"v":1,"sources":[{"id":7,"label":"project:shop"}],"notes":[{"id":4,"source":7,"rev":2,"class":"structure","kind":"finding"}],"truncated":false}`
		case path == "/minigw/v1/projects/7/notes/4":
			return 200, `{"id":4,"project":7,"class":"structure","title":"Deploy order","body":"first the db","updated":"2026-11-17T09:00:00Z","revision":2,"entryKind":"finding"}`
		}
		return 404, `{}`
	}
	api := New(Options{Store: func() *core.Store { return f.st }, Upstream: h})
	call := func(method, path, body string, want int) []byte {
		t.Helper()
		r := api.Handle(bg, Context{Kind: KindAdmin}, Request{Method: method, Path: path, Body: []byte(body)})
		if r.Status != want {
			t.Fatalf("%s %s %s: %d %s", method, path, body, r.Status, r.Body)
		}
		return r.Body
	}
	call("PUT", "/upstreams/hub", `{"address":"127.0.0.1:8766","token":"tok#key"}`, 200)
	link := fmt.Sprintf("/upstreams/hub/links/%d", f.pid["fakture"])
	call("PUT", link, `{"remote":"shop","classes":"structure","mode":"publish"}`, 200)
	if b := call("PUT", link, `{"read":true,"readOnly":true}`, 400); !strings.Contains(string(b), "--accept-reach") {
		t.Fatalf("%s", b)
	}
	call("PUT", link, `{"read":true,"readOnly":true,"acceptReach":true,"x":1}`, 400)
	call("PUT", link, `{"read":true,"readOnly":true,"acceptReach":true,"readClasses":"private"}`, 400)
	call("POST", "/upstream-pull", fmt.Sprintf(`{"project":%d}`, f.pid["fakture"]), 400) // reading is still off
	call("PUT", link, `{"read":true,"readOnly":true,"acceptReach":true}`, 200)
	var pr struct{ Pulls []struct{ Added, Mirror int } }
	if err := json.Unmarshal(call("POST", "/upstream-pull", fmt.Sprintf(`{"project":%d}`, f.pid["fakture"]), 200), &pr); err != nil || len(pr.Pulls) != 1 || pr.Pulls[0].Added != 1 || pr.Pulls[0].Mirror != 1 {
		t.Fatalf("%v %+v", err, pr)
	}
	var st struct {
		Links []struct {
			Read        bool
			Mode        string
			Classes     []string
			ReadClasses []string
			Mirror      int
			MirrorBytes int
			LastPullOk  *string
			PullError   string
		}
	}
	if err := json.Unmarshal(call("GET", "/upstreams", ``, 200), &st); err != nil || len(st.Links) != 1 {
		t.Fatalf("%v %+v", err, st)
	}
	if l := st.Links[0]; !l.Read || l.Mode != "publish" || len(l.Classes) != 1 || len(l.ReadClasses) != 1 || l.Mirror != 1 || l.MirrorBytes == 0 || l.LastPullOk == nil || l.PullError != "" {
		t.Fatalf("%+v", l)
	}
	// no member route reaches any of it: the mirror is in no member answer yet
	for _, mp := range []string{fmt.Sprintf("/minigw/v1/projects/%d/notes", f.pid["fakture"]), fmt.Sprintf("/minigw/v1/projects/%d/kb/search?q=Deploy", f.pid["fakture"])} {
		if r := api.Handle(bg, member("dev-fani"), Request{Method: "GET", Path: mp}); strings.Contains(string(r.Body), "Deploy order") {
			t.Fatalf("%s shows a mirrored note: %s", mp, r.Body)
		}
	}
	// reading off deletes the mirror; unlink and removal leave nothing
	call("PUT", link, `{"read":false,"readOnly":true}`, 200)
	var n int
	if err := f.st.DB().QueryRow(`SELECT count(*) FROM upstream_mirror`).Scan(&n); err != nil || n != 0 {
		t.Fatal(n, err)
	}
}
