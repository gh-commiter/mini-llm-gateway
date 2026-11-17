package coreapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"minigw/core"
)

// Team notes over HTTP: the segment id comes from the team JSON; members of the roster read and search it, everyone
// else gets the usual 404 from every route. Each non-member assertion fails if the roster check is removed.
func TestSegmentRoutes(t *testing.T) {
	f := newFixture(t)
	admin := Context{Kind: KindAdmin}
	keys := 0
	call := func(c Context, method, path, body string, want int) Response {
		t.Helper()
		path, query, _ := strings.Cut(path, "?")
		r := f.api.Handle(bg, c, Request{Method: method, Path: path, RawQuery: query, Body: []byte(body), Header: http.Header{"Idempotency-Key": {fmt.Sprint("k", keys)}}})
		keys++
		path = strings.TrimSuffix(path+"?"+query, "?")
		if r.Status != want {
			t.Fatalf("%s %s: %d %s, want %d", method, path, r.Status, r.Body, want)
		}
		return r
	}
	r := call(admin, "POST", "/teams", `{"name":"acme"}`, 200)
	var created struct{ Team TeamJSON }
	if err := json.Unmarshal(r.Body, &created); err != nil {
		t.Fatal(err)
	}
	team := created.Team
	call(admin, "PUT", fmt.Sprintf("/teams/%d/projects/%d", team.ID, f.pid["fakture"]), "", 200)
	for _, u := range []string{"ana", "fani"} { // eva is a project reader but not on the roster
		call(admin, "PUT", fmt.Sprintf("/teams/%d/members/%d", team.ID, f.uid[u]), "", 200)
	}
	r = call(member("dev-ana"), "GET", fmt.Sprintf("/minigw/v1/teams/%d", team.ID), "", 200)
	var detail TeamDetailJSON
	if err := json.Unmarshal(r.Body, &detail); err != nil || detail.Segment != core.TeamSegment(team.ID) || detail.Segment <= 1<<40 {
		t.Fatalf("segment in team json: %s %v", r.Body, err)
	}
	seg := detail.Segment
	base := fmt.Sprintf("/minigw/v1/projects/%d", seg)
	// fani is admin of fakture: writes. ana reads.
	r = call(member("dev-fani"), "POST", base+"/notes", `{"class":"data","title":"deploy order","body":"first migrate"}`, 201)
	var made NoteJSON
	if err := json.Unmarshal(r.Body, &made); err != nil {
		t.Fatal(err)
	}
	call(member("dev-ana"), "POST", base+"/notes", `{"class":"data","title":"x","body":"y"}`, 403)
	call(member("dev-fani"), "POST", base+"/notes", `{"class":"private","title":"x","body":"y"}`, 403)
	read := fmt.Sprintf("%s/notes/%d", base, made.ID)
	call(member("dev-ana"), "GET", read, "", 200)
	if !strings.Contains(string(call(member("dev-ana"), "GET", base+"/kb/search?q=migrate", "", 200).Body), `"from":"team:acme"`) {
		t.Fatal("segment search has no from")
	}
	// A search of the project covers the segment; the hit opens through the project route.
	proj := fmt.Sprintf("/minigw/v1/projects/%d", f.pid["fakture"])
	hit := call(member("dev-ana"), "GET", proj+"/kb/search?q=migrate", "", 200)
	if !strings.Contains(string(hit.Body), `"from":"team:acme"`) {
		t.Fatalf("%s", hit.Body)
	}
	call(member("dev-ana"), "GET", fmt.Sprintf("%s/notes/%d", proj, made.ID), "", 200)
	call(member("dev-ana"), "GET", proj+"/notes", "", 200) // list: the project only
	if strings.Contains(string(call(member("dev-ana"), "GET", proj+"/notes", "", 200).Body), "deploy order") {
		t.Fatal("project list shows the segment")
	}
	// eva (reader of the team's project, not on the roster) and cene (no relation) get nothing, from any route.
	for _, dev := range []string{"dev-eva", "dev-cene"} {
		for _, p := range []string{base + "/notes", read, base + "/kb/search?q=migrate", base + "/kb/faq", base + "/kb/submissions"} {
			call(member(dev), "GET", p, "", 404)
		}
		call(member(dev), "POST", base+"/notes", `{"class":"data","title":"x","body":"y"}`, 404)
	}
	if strings.Contains(string(call(member("dev-eva"), "GET", proj+"/kb/search?q=migrate", "", 200).Body), "deploy") {
		t.Fatal("non-member search found the segment")
	}
	call(member("dev-eva"), "GET", fmt.Sprintf("%s/notes/%d", proj, made.ID), "", 404)
	// The segment is no project: not in the lists, not by id or slug.
	if strings.Contains(string(call(member("dev-fani"), "GET", "/minigw/v1/projects", "", 200).Body), "@team") {
		t.Fatal("segment in the project list")
	}
	call(member("dev-fani"), "GET", base, "", 404)
	call(member("dev-fani"), "GET", "/minigw/v1/projects/@team-1", "", 404)
	if strings.Contains(string(call(admin, "GET", "/projects", "", 200).Body), "@team") {
		t.Fatal("segment in the admin project list")
	}
	call(admin, "PUT", fmt.Sprintf("/projects/%d/members/%d", seg, f.uid["ana"]), `{"role":"reader"}`, 404)
	// The run channel: eva's run in fakture finds nothing from the team; ana's run does.
	epoch, _ := f.st.Epoch(bg)
	run := func(u, q string) string {
		c := core.RunCap{UserID: f.uid[u], DeviceID: "dev-" + u, ProjectID: f.pid["fakture"], Epoch: epoch}
		return string(call(Context{Kind: KindRun, Run: &c, Approved: true}, "GET", "/minigw/v1/kb/search?q="+q, "", 200).Body)
	}
	if !strings.Contains(run("ana", "migrate"), "team:acme") || strings.Contains(run("eva", "migrate"), "deploy") {
		t.Fatal("run scope")
	}
	// A roster member who is only a reader here but admin of a project of ANOTHER team must not write this segment.
	r = call(admin, "POST", "/teams", `{"name":"other"}`, 200)
	var other struct{ Team TeamJSON }
	if err := json.Unmarshal(r.Body, &other); err != nil {
		t.Fatal(err)
	}
	call(admin, "PUT", fmt.Sprintf("/teams/%d/projects/%d", other.Team.ID, f.pid["eureko"]), "", 200)
	if err := f.st.SetMember(bg, f.pid["eureko"], f.uid["eva"], core.Admin); err != nil {
		t.Fatal(err)
	}
	call(admin, "PUT", fmt.Sprintf("/teams/%d/members/%d", team.ID, f.uid["eva"]), "", 200)
	call(member("dev-eva"), "GET", read, "", 200)
	call(member("dev-eva"), "POST", base+"/notes", `{"class":"data","title":"x","body":"y"}`, 403)
	// Removed from the roster: gone on the next call.
	call(admin, "DELETE", fmt.Sprintf("/teams/%d/members/%d", team.ID, f.uid["ana"]), "", 200)
	call(member("dev-ana"), "GET", read, "", 404)
}

// depends over HTTP: only the owner changes the relation; a member sees only dependencies it can read.
func TestDependsRoutes(t *testing.T) {
	f := newFixture(t)
	admin := Context{Kind: KindAdmin}
	call := func(c Context, method, path string, want int) Response {
		t.Helper()
		r := f.api.Handle(bg, c, Request{Method: method, Path: path})
		if r.Status != want {
			t.Fatalf("%s %s: %d %s, want %d", method, path, r.Status, r.Body, want)
		}
		return r
	}
	fk, eu := f.pid["fakture"], f.pid["eureko"]
	call(admin, "PUT", fmt.Sprintf("/projects/%d/depends/%d", fk, eu), 200)
	if !strings.Contains(string(call(admin, "GET", fmt.Sprintf("/projects/%d/depends", fk), 200).Body), `"slug":"eureko"`) {
		t.Fatal("admin list")
	}
	if !strings.Contains(string(call(admin, "GET", fmt.Sprintf("/projects/%d/depends", eu), 200).Body), `"usedBy":[{"id":`+fmt.Sprint(fk)) {
		t.Fatal("usedBy")
	}
	call(admin, "PUT", fmt.Sprintf("/projects/%d/depends/%d", fk, fk), 400)
	call(admin, "PUT", fmt.Sprintf("/projects/%d/depends/%d", fk, 1<<40+1), 404)
	// eva reads fakture but not eureko: the list hides it. cene reads eureko (member) and fakture (grant).
	if strings.Contains(string(call(member("dev-eva"), "GET", fmt.Sprintf("/minigw/v1/projects/%d/depends", fk), 200).Body), "eureko") {
		t.Fatal("unreadable dependency listed")
	}
	if !strings.Contains(string(call(member("dev-cene"), "GET", fmt.Sprintf("/minigw/v1/projects/%d/depends", fk), 200).Body), "eureko") {
		t.Fatal("readable dependency missing")
	}
	// No member route changes it.
	for _, m := range []string{"PUT", "POST", "DELETE", "PATCH"} {
		call(member("dev-fani"), m, fmt.Sprintf("/minigw/v1/projects/%d/depends/%d", fk, eu), 404)
		call(member("dev-fani"), m, fmt.Sprintf("/minigw/v1/projects/%d/depends", fk), 404)
	}
	call(admin, "DELETE", fmt.Sprintf("/projects/%d/depends/%d", fk, eu), 200)
	call(admin, "DELETE", fmt.Sprintf("/projects/%d/depends/%d", fk, eu), 404)
}

func TestDependsxIsA404(t *testing.T) {
	f := newFixture(t)
	r := f.api.Handle(bg, Context{Kind: KindAdmin}, Request{Method: "GET", Path: "/projects/3/dependsx"})
	if r.Status != 404 && r.Status != 400 {
		t.Fatal(r.Status, r.Body)
	}
}
