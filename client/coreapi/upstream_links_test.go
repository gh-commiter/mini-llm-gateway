package coreapi

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"minigw/core"
)

// methodHost answers an upstream by method and path.
type methodHost struct {
	secretHost
	answer func(method, path string) (int, string)
}

func (h *methodHost) Info(string) UpstreamInfo { return UpstreamInfo{Device: "DEV", Where: "test"} }
func (h *methodHost) Do(_ context.Context, _ string, method string, path string, _ map[string]string, _ []byte) (int, []byte, time.Duration, error) {
	st, body := h.answer(method, path)
	return st, []byte(body), 0, nil
}

func TestResolveSelectsOnlyTheNamedUpstream(t *testing.T) {
	for _, ours := range []bool{false, true} {
		t.Run(fmt.Sprint(ours), func(t *testing.T) {
			f := newFixture(t)
			changed := false
			h := &methodHost{secretHost: secretHost{secrets: map[string]string{}}}
			h.answer = func(method, path string) (int, string) {
				if path == "/minigw/v1/projects/shop" {
					return 200, `{"id":7,"slug":"shop"}`
				}
				if method == "POST" {
					return 201, `{"id":55,"revision":1}`
				}
				if changed {
					return 200, `{"revision":9}`
				}
				return 200, `{"revision":1}`
			}
			api := New(Options{Store: func() *core.Store { return f.st }, Upstream: h})
			call := func(method, path, body string, want int) []byte {
				t.Helper()
				r := api.Handle(bg, Context{Kind: KindAdmin}, Request{Method: method, Path: path, Body: []byte(body)})
				if r.Status != want {
					t.Fatalf("%s %s: %d %s", method, path, r.Status, r.Body)
				}
				return r.Body
			}
			fk := f.pid["fakture"]
			for _, up := range []string{"hub", "other"} {
				call("PUT", "/upstreams/"+up, `{"address":"127.0.0.1:8766","token":"tok#key"}`, 200)
				call("PUT", fmt.Sprintf("/upstreams/%s/links/%d", up, fk), `{"remote":"shop","mode":"publish","auto":true}`, 200)
			}
			call("POST", "/upstream-push", fmt.Sprintf(`{"project":%d}`, fk), 200)
			p := core.Principal{UserID: f.uid["ana"], DeviceID: "dev-ana"}
			n, e := f.st.GetNote(bg, p, fk, f.nid["structure"])
			if e != nil {
				t.Fatal(e)
			}
			_, e = f.st.UpdateNote(bg, p, fk, n.ID, n.Revision, core.NoteInput{Class: n.Class, Title: n.Title, Body: "edited"})
			if e != nil {
				t.Fatal(e)
			}
			changed = true
			call("POST", "/upstream-push", fmt.Sprintf(`{"project":%d}`, fk), 200)
			b := call("POST", "/upstream-resolve", fmt.Sprintf(`{"project":%d,"note":%d,"ours":%t}`, fk, n.ID, ours), 400)
			if !strings.Contains(string(b), "hub") || !strings.Contains(string(b), "other") {
				t.Fatal(string(b))
			}
			var detail map[string]any
			json.Unmarshal(call("GET", fmt.Sprintf("/upstream-links/project-%d-hub", fk), "", 200), &detail)
			rows := detail["conflictRows"].([]any)
			if rows[0].(map[string]any)["upstream"] != "hub" {
				t.Fatal("conflict row does not name its upstream")
			}
			call("POST", "/upstream-resolve", fmt.Sprintf(`{"project":%d,"note":%d,"ours":%t,"upstream":"hub"}`, fk, n.ID, ours), 200)
			ls, e := f.st.UpstreamLinks(bg, fk, 0)
			if e != nil {
				t.Fatal(e)
			}
			for _, l := range ls {
				want := 0
				if l.Upstream == "other" {
					want = 1
				}
				if len(l.Conflicts) != want {
					t.Fatalf("%s: %+v", l.Upstream, l.Conflicts)
				}
			}
			// The old request remains valid once only one conflict is left.
			call("POST", "/upstream-resolve", fmt.Sprintf(`{"project":%d,"note":%d,"ours":false}`, fk, n.ID), 200)
		})
	}
}

// GET /upstream-links/{id}: one link with its conflicts as rows (the note's title and what happened there), what the panel's detail
// page lists and resolves; the count is also in the list.
func TestUpstreamLinkDetailListsConflicts(t *testing.T) {
	f := newFixture(t)
	first := true
	h := &methodHost{secretHost: secretHost{secrets: map[string]string{}}}
	h.answer = func(method, path string) (int, string) {
		switch {
		case path == "/minigw/v1/projects/shop":
			return 200, `{"id":7,"slug":"shop"}`
		case method == "POST":
			return 201, `{"id":55,"revision":1}`
		case method == "GET" && !first:
			return 200, `{"id":55,"revision":9}` // the upstream note moved on
		}
		return 200, `{"id":55,"revision":1}`
	}
	api := New(Options{Store: func() *core.Store { return f.st }, Upstream: h, UpstreamSleep: func(time.Duration) bool { return true }})
	call := func(method, path, body string, want int) []byte {
		t.Helper()
		r := api.Handle(bg, Context{Kind: KindAdmin}, Request{Method: method, Path: path, Body: []byte(body)})
		if r.Status != want {
			t.Fatalf("%s %s: %d %s, want %d", method, path, r.Status, r.Body, want)
		}
		return r.Body
	}
	call("PUT", "/upstreams/hub", `{"address":"hub.example","token":"tok#key"}`, 200)
	fk := f.pid["fakture"]
	id := fmt.Sprintf("project-%d-hub", fk)
	call("PUT", fmt.Sprintf("/upstreams/hub/links/%d", fk), `{"remote":"shop","mode":"publish"}`, 200)
	call("POST", "/upstream-push", fmt.Sprintf(`{"project":%d}`, fk), 200)
	first = false
	p := core.Principal{UserID: f.uid["ana"], DeviceID: "dev-ana"}
	cur, err := f.st.GetNote(bg, p, fk, f.nid["structure"])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.UpdateNote(bg, p, fk, cur.ID, cur.Revision, core.NoteInput{Class: core.Structure, Title: cur.Title, Body: "changed here"}); err != nil {
		t.Fatal(err)
	}
	call("POST", "/upstream-push", fmt.Sprintf(`{"project":%d}`, fk), 200)

	var list struct{ Links []adminLinkJSON }
	json.Unmarshal(call("GET", "/upstreams", "", 200), &list)
	if len(list.Links) != 1 || list.Links[0].ConflictCount != 1 || list.Links[0].Mode != "publish" {
		t.Fatalf("%+v", list.Links)
	}
	var d adminLinkDetailJSON
	json.Unmarshal(call("GET", "/upstream-links/"+id, "", 200), &d)
	if d.ID != id || d.ConflictCount != 1 || len(d.ConflictRows) != 1 {
		t.Fatalf("%+v", d)
	}
	if c := d.ConflictRows[0]; c.Note != f.nid["structure"] || c.Title != "t-structure" || c.What != "changed" || c.ProjectID != fk || c.TeamID != 0 {
		t.Fatalf("%+v", c)
	}
	// resolving takes the row's own numbers; the detail then lists none
	call("POST", "/upstream-resolve", fmt.Sprintf(`{"project":%d,"team":0,"note":%d,"ours":false}`, fk, f.nid["structure"]), 200)
	json.Unmarshal(call("GET", "/upstream-links/"+id, "", 200), &d)
	if d.ConflictCount != 0 || d.ConflictRows == nil || len(d.ConflictRows) != 0 {
		t.Fatalf("%+v", d)
	}
	// anything that is not a link of this gateway
	for path, want := range map[string]int{
		"/upstream-links/project-" + fmt.Sprint(fk) + "-nope": 404, "/upstream-links/project-9999-hub": 404,
		"/upstream-links/x": 400, "/upstream-links/team-0-hub": 400, "/upstream-links/shop-1-hub": 400, "/upstream-links/project-1": 400, "/upstream-links/project-a-hub": 400,
	} {
		call("GET", path, "", want)
	}
	call("POST", "/upstream-links/"+id, "{}", 404)
	if r := api.Handle(bg, Context{Kind: KindMember, DeviceID: "dev-ana", Approved: true}, Request{Method: "GET", Path: "/upstream-links/" + id}); r.Status == 200 || strings.Contains(string(r.Body), "t-structure") {
		t.Fatalf("a member read the owner's link: %d %s", r.Status, r.Body)
	}
}

// A submission that can no longer be read there is its own kind of conflict row (not "changed"), and a note out of sight is "deleted".
func TestUpstreamLinkDetailNamesTheKindOfConflict(t *testing.T) {
	f := newFixture(t)
	h := &methodHost{secretHost: secretHost{secrets: map[string]string{}}}
	h.answer = func(method, path string) (int, string) {
		switch {
		case path == "/minigw/v1/projects/shop":
			return 200, `{"id":7,"slug":"shop"}`
		case method == "POST":
			return 201, `{"id":5,"state":"pending"}`
		}
		return 404, `{"error":{"message":"gone"}}`
	}
	api := New(Options{Store: func() *core.Store { return f.st }, Upstream: h})
	call := func(method, path, body string) []byte {
		t.Helper()
		r := api.Handle(bg, Context{Kind: KindAdmin}, Request{Method: method, Path: path, Body: []byte(body)})
		if r.Status != 200 {
			t.Fatalf("%s %s: %d %s", method, path, r.Status, r.Body)
		}
		return r.Body
	}
	fk := f.pid["fakture"]
	call("PUT", "/upstreams/hub", `{"address":"127.0.0.1:8766","token":"tok#key"}`)
	call("PUT", fmt.Sprintf("/upstreams/hub/links/%d", fk), `{"remote":"shop"}`)
	call("POST", "/upstream-push", fmt.Sprintf(`{"project":%d}`, fk))
	p := core.Principal{UserID: f.uid["ana"], DeviceID: "dev-ana"}
	n, e := f.st.GetNote(bg, p, fk, f.nid["structure"])
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.st.UpdateNote(bg, p, fk, n.ID, n.Revision, core.NoteInput{Class: n.Class, Title: n.Title, Body: "edited"}); e != nil {
		t.Fatal(e)
	}
	call("POST", "/upstream-push", fmt.Sprintf(`{"project":%d}`, fk)) // the earlier submission cannot be read
	var detail struct {
		ConflictRows []struct {
			Note int64
			What string
		}
	}
	json.Unmarshal(call("GET", fmt.Sprintf("/upstream-links/project-%d-hub", fk), ""), &detail)
	if len(detail.ConflictRows) != 1 || detail.ConflictRows[0].What != "submission" || detail.ConflictRows[0].Note != n.ID {
		t.Fatalf("%+v", detail)
	}
}

// A link that exists keeps its destination when a later `link` does not name one (a mode, class or auto change); only a named different
// destination is refused once notes were sent; a first link without one uses the local name.
func TestRelinkWithoutADestinationKeepsIt(t *testing.T) {
	f := newFixture(t)
	h := &methodHost{secretHost: secretHost{secrets: map[string]string{}}}
	h.answer = func(method, path string) (int, string) {
		switch {
		case strings.HasPrefix(path, "/minigw/v1/projects/") && !strings.Contains(path, "/kb/") && method == "GET":
			return 200, fmt.Sprintf(`{"id":7,"slug":%q}`, strings.TrimPrefix(path, "/minigw/v1/projects/"))
		case method == "POST":
			return 201, `{"id":5,"state":"pending"}`
		}
		return 404, `{}`
	}
	api := New(Options{Store: func() *core.Store { return f.st }, Upstream: h})
	call := func(method, path, body string, want int) []byte {
		t.Helper()
		r := api.Handle(bg, Context{Kind: KindAdmin}, Request{Method: method, Path: path, Body: []byte(body)})
		if r.Status != want {
			t.Fatalf("%s %s: %d %s", method, path, r.Status, r.Body)
		}
		return r.Body
	}
	fk := f.pid["fakture"]
	link := fmt.Sprintf("/upstreams/hub/links/%d", fk)
	call("PUT", "/upstreams/hub", `{"address":"127.0.0.1:8766","token":"tok#key"}`, 200)
	remote := func() string {
		ls, e := f.st.UpstreamLinks(bg, fk, 0)
		if e != nil || len(ls) != 1 {
			t.Fatal(e, ls)
		}
		return ls[0].Remote
	}
	call("PUT", link, `{}`, 200) // a first link without a destination: the local name
	if remote() != "fakture" {
		t.Fatal(remote())
	}
	call("PUT", link, `{"remote":"shared"}`, 200)
	call("POST", "/upstream-push", fmt.Sprintf(`{"project":%d}`, fk), 200) // receipts
	for _, body := range []string{`{"auto":true}`, `{"auto":false}`, `{"classes":"structure"}`, `{"mode":"submit","auto":true}`} {
		var out map[string]any
		json.Unmarshal(call("PUT", link, body, 200), &out)
		if remote() != "shared" || out["remote"] != "shared" {
			t.Fatalf("%s: %q %v", body, remote(), out)
		}
	}
	b := call("PUT", link, `{"remote":"other"}`, 400) // an explicit different destination
	if !strings.Contains(string(b), "unlink it and link it again") || remote() != "shared" {
		t.Fatal(string(b), remote())
	}
	call("PUT", link, `{"remote":"shared","auto":true}`, 200) // the same one named: fine
}
