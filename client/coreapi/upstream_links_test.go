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
