package coreapi

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"minigw/core"
)

func call(t *testing.T, f *fixture, c Context, method, path, body string, want int) Response {
	t.Helper()
	r := f.api.Handle(bg, c, Request{Method: method, Path: path, Body: []byte(body)})
	if r.Status != want {
		t.Fatalf("%s %s: %d %s, want %d", method, path, r.Status, r.Body, want)
	}
	return r
}

func remotesOf(t *testing.T, r Response) []string {
	t.Helper()
	var out struct{ Remotes []string }
	if err := json.Unmarshal(r.Body, &out); err != nil {
		t.Fatal(err)
	}
	return out.Remotes
}

func TestRemoteRoutesAuthorization(t *testing.T) {
	f := newFixture(t)
	fak := fmt.Sprintf("/minigw/v1/projects/%d/remotes", f.pid["fakture"])
	eur := fmt.Sprintf("/minigw/v1/projects/%d/remotes", f.pid["eureko"])
	add := `{"add":["git@host.example:eureko/fakture.git"]}`
	call(t, f, member("dev-eva"), "POST", fak, add, 403)  // reader
	call(t, f, member("dev-ana"), "POST", fak, add, 403)  // developer
	call(t, f, member("dev-cene"), "POST", fak, add, 403) // grant only
	call(t, f, member("dev-ana"), "POST", eur, add, 404)  // cannot see the project
	call(t, f, Context{Kind: KindMember, DeviceID: "dev-fani", Approved: true, AgentPeer: true}, "POST", fak, add, 403)
	if list, _ := f.st.AdminProjectRemotes(bg, f.pid["fakture"]); len(list) != 0 {
		t.Fatal("a refused write stored", list)
	}
	r := call(t, f, member("dev-fani"), "POST", fak, add, 200)
	if got := remotesOf(t, r); len(got) != 1 || got[0] != "host.example/eureko/fakture" {
		t.Fatal(got)
	}
	call(t, f, member("dev-fani"), "POST", fak, `{"add":["/local/path"]}`, 400)
	call(t, f, member("dev-fani"), "POST", fak, `{"add":["host.example:a/b"],"nope":1}`, 400)
	for _, dev := range []string{"dev-eva", "dev-ana", "dev-fani"} { // any direct member reads
		if got := remotesOf(t, call(t, f, member(dev), "GET", fak, "", 200)); len(got) != 1 {
			t.Fatal(dev, got)
		}
	}
	call(t, f, member("dev-cene"), "GET", fak, "", 403)
	r = call(t, f, member("dev-ana"), "GET", eur, "", 404)
	if strings.Contains(string(r.Body), "host.example") {
		t.Fatal("identity leaked", r.Body)
	}
	r = call(t, f, member("dev-fani"), "POST", fak, `{"remove":["https://tok@HOST.example/Eureko/Fakture"]}`, 200)
	if got := remotesOf(t, r); len(got) != 0 {
		t.Fatal(got)
	}
	// Admin channel: the owner edits too; an archived project is refused.
	adm := Context{Kind: KindAdmin}
	path := fmt.Sprintf("/projects/%d/remotes", f.pid["fakture"])
	if got := remotesOf(t, call(t, f, adm, "POST", path, add, 200)); len(got) != 1 {
		t.Fatal(got)
	}
	if got := remotesOf(t, call(t, f, adm, "GET", path, "", 200)); len(got) != 1 {
		t.Fatal(got)
	}
	call(t, f, adm, "GET", "/projects/999/remotes", "", 404)
	call(t, f, adm, "PATCH", fmt.Sprintf("/projects/%d", f.pid["fakture"]), `{"archived":true}`, 200)
	call(t, f, adm, "POST", path, `{"remove":["https://host.example/a/b"]}`, 403)
	call(t, f, adm, "POST", path, add, 403)
	r = call(t, f, member("dev-fani"), "POST", fak, add, 403)
	if !strings.Contains(string(r.Body), "archived") {
		t.Fatal(string(r.Body))
	}
}

func TestByRemoteNeverLeaks(t *testing.T) {
	f := newFixture(t)
	for _, p := range []string{"fakture", "eureko"} {
		if _, err := f.st.AdminChangeProjectRemotes(bg, f.pid[p], []string{"git@host.example:eureko/mono.git"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	body := `{"remote":"https://user:pw@Host.example:8443/Eureko/Mono.git"}`
	slugs := func(dev string) []string {
		t.Helper()
		r := call(t, f, member(dev), "POST", "/minigw/v1/projects/by-remote", body, 200)
		var out struct {
			Identity string
			Projects []ProjectJSON
		}
		if err := json.Unmarshal(r.Body, &out); err != nil || out.Identity != "host.example/eureko/mono" {
			t.Fatal(string(r.Body), err)
		}
		var s []string
		for _, p := range out.Projects {
			s = append(s, p.Slug)
		}
		return s
	}
	if got := slugs("dev-ana"); len(got) != 1 || got[0] != "fakture" { // member of fakture only
		t.Fatal(got)
	}
	if got := slugs("dev-cene"); len(got) != 1 || got[0] != "eureko" { // fakture is grant only for cene
		t.Fatal(got)
	}
	if err := f.st.SetMember(bg, f.pid["fakture"], f.uid["cene"], core.Reader); err != nil {
		t.Fatal(err)
	}
	if got := slugs("dev-cene"); len(got) != 2 {
		t.Fatal("ambiguity must show both", got)
	}
	if err := f.st.SetProjectArchived(bg, f.pid["eureko"], true); err != nil {
		t.Fatal(err)
	}
	if got := slugs("dev-cene"); len(got) != 1 || got[0] != "fakture" {
		t.Fatal("archived matched", got)
	}
	call(t, f, member("dev-ana"), "POST", "/minigw/v1/projects/by-remote", `{"remote":"/srv/repo"}`, 400)
	call(t, f, member("dev-ana"), "POST", "/minigw/v1/projects/by-remote", `{"remote":"a/b","x":1}`, 400)
	call(t, f, member("dev-ana"), "GET", "/minigw/v1/projects/by-remote", "", 404)
	call(t, f, Context{Kind: KindMember, DeviceID: "dev-ana", Approved: true, AgentPeer: true}, "POST", "/minigw/v1/projects/by-remote", body, 403)
	call(t, f, Context{Kind: KindMember, DeviceID: "unbound", Approved: true}, "POST", "/minigw/v1/projects/by-remote", body, 403)
}

func TestTeamPathRoutes(t *testing.T) {
	f := newFixture(t)
	adm := Context{Kind: KindAdmin}
	mk := func(name string) int64 {
		r := call(t, f, adm, "POST", "/teams", fmt.Sprintf(`{"name":%q}`, name), 200)
		var out struct{ Team TeamJSON }
		if err := json.Unmarshal(r.Body, &out); err != nil {
			t.Fatal(err)
		}
		return out.Team.ID
	}
	mine, other := mk("Mine"), mk("Other")
	call(t, f, adm, "PUT", fmt.Sprintf("/teams/%d/members/%d", mine, f.uid["ana"]), "", 200)
	call(t, f, adm, "PUT", fmt.Sprintf("/teams/%d/projects/%d", mine, f.pid["fakture"]), "", 200)
	call(t, f, adm, "PUT", fmt.Sprintf("/teams/%d/projects/%d", other, f.pid["eureko"]), "", 200)
	call(t, f, adm, "PUT", fmt.Sprintf("/teams/%d/members/%d", other, f.uid["cene"]), "", 200)
	call(t, f, adm, "PATCH", fmt.Sprintf("/teams/%d", mine), `{"path":"Eureko/Backend"}`, 200)
	call(t, f, adm, "PATCH", fmt.Sprintf("/teams/%d", other), `{"path":"Eureko/Backend"}`, 200)
	call(t, f, adm, "PATCH", fmt.Sprintf("/teams/%d", mine), `{"path":"/x"}`, 400)
	call(t, f, adm, "PATCH", fmt.Sprintf("/teams/%d", mine), `{"path":"x","name":"y"}`, 400)
	call(t, f, adm, "PATCH", fmt.Sprintf("/teams/%d", mine), `{}`, 400)
	call(t, f, adm, "PATCH", "/teams/999", `{"path":"x"}`, 404)
	// A member has no route to set a label (PATCH is not a member route) and sees the label in the team JSON.
	call(t, f, member("dev-ana"), "PATCH", fmt.Sprintf("/minigw/v1/teams/%d", mine), `{"path":"hacked"}`, 404)
	call(t, f, member("dev-ana"), "POST", fmt.Sprintf("/minigw/v1/teams/%d", mine), `{"path":"hacked"}`, 404)
	var detail TeamDetailJSON
	if err := json.Unmarshal(call(t, f, member("dev-ana"), "GET", fmt.Sprintf("/minigw/v1/teams/%d", mine), "", 200).Body, &detail); err != nil || detail.Path != "eureko/backend" {
		t.Fatal(detail, err)
	}
	var teams []TeamJSON
	if err := json.Unmarshal(call(t, f, member("dev-ana"), "GET", "/minigw/v1/teams", "", 200).Body, &teams); err != nil || len(teams) != 1 || teams[0].Path != "eureko/backend" {
		t.Fatal(teams, err)
	}
	list := func(dev, query string) []string {
		t.Helper()
		r := f.api.Handle(bg, member(dev), Request{Method: "GET", Path: "/minigw/v1/projects", RawQuery: query})
		if r.Status != 200 {
			t.Fatal(r.Status, string(r.Body))
		}
		var ps []ProjectJSON
		if err := json.Unmarshal(r.Body, &ps); err != nil {
			t.Fatal(err)
		}
		out := []string{}
		for _, p := range ps {
			out = append(out, p.Slug)
		}
		return out
	}
	// ana belongs to team Mine only; cene (eureko developer, grantee of fakture) to team Other only.
	if got := list("dev-ana", "path=Eureko"); len(got) != 1 || got[0] != "fakture" {
		t.Fatal(got)
	}
	if got := list("dev-cene", "path=eureko/backend"); len(got) != 1 || got[0] != "eureko" {
		t.Fatal("only projects of the caller's own teams", got)
	}
	if got := list("dev-eva", "path=eureko"); len(got) != 0 {
		t.Fatal("label widened visibility", got)
	}
	if got := list("dev-ana", ""); len(got) != 1 {
		t.Fatal(got)
	}
	r := f.api.Handle(bg, member("dev-ana"), Request{Method: "GET", Path: "/minigw/v1/projects", RawQuery: "path=/x"})
	if r.Status != 400 {
		t.Fatal(r.Status)
	}
}

func TestFAQRouteAndKinds(t *testing.T) {
	f := newFixture(t)
	sub := fmt.Sprintf("/minigw/v1/projects/%d/kb/submissions", f.pid["fakture"])
	faq := fmt.Sprintf("/minigw/v1/projects/%d/kb/faq", f.pid["fakture"])
	submit := func(class, title, kind string, want int) core.Submission {
		t.Helper()
		body := fmt.Sprintf(`{"class":%q,"title":%q,"body":"answer to %s","entryKind":%q}`, class, title, title, kind)
		r := f.api.Handle(bg, member("dev-ana"), Request{Method: "POST", Path: sub, Body: []byte(body), Header: map[string][]string{"Idempotency-Key": {title}}})
		if r.Status != want {
			t.Fatalf("%d %s", r.Status, r.Body)
		}
		var v core.Submission
		json.Unmarshal(r.Body, &v)
		return v
	}
	submit("structure", "Shared?", "faq", 201)
	submit("private", "Mine?", "faq", 201)
	submit("data", "x", "rumor", 400)
	if v := submit("data", "Plain", "", 201); v.EntryKind != "finding" {
		t.Fatal(v)
	}
	// Pending submissions never list.
	var page struct {
		Entries []struct {
			ID            int64
			Title, Answer string
			Class         string
		}
		Next string
	}
	if err := json.Unmarshal(call(t, f, member("dev-ana"), "GET", faq, "", 200).Body, &page); err != nil || len(page.Entries) != 0 {
		t.Fatal("pending faq listed", page, err)
	}
	subs, err := f.st.ListSubmissions(bg, core.Principal{}, 0, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range subs {
		if v.EntryKind == "faq" {
			if _, err := f.st.ReviewSubmission(bg, v.ID, true, ""); err != nil {
				t.Fatal(err)
			}
		}
	}
	titles := func(dev string) []string {
		t.Helper()
		page.Entries = nil
		if err := json.Unmarshal(call(t, f, member(dev), "GET", faq, "", 200).Body, &page); err != nil {
			t.Fatal(err)
		}
		out := []string{}
		for _, e := range page.Entries {
			out = append(out, e.Title)
		}
		return out
	}
	if got := titles("dev-ana"); len(got) != 2 {
		t.Fatal(got)
	}
	if got := titles("dev-fani"); len(got) != 2 { // project admin
		t.Fatal(got)
	}
	for _, dev := range []string{"dev-eva", "dev-cene"} {
		if got := titles(dev); len(got) != 1 || got[0] != "Shared?" {
			t.Fatal("private faq visible to", dev, got)
		}
	}
	if page.Next != "" || page.Entries[0].Answer != "answer to Shared?" {
		t.Fatal(page)
	}
	r := f.api.Handle(bg, member("dev-eva"), Request{Method: "GET", Path: fmt.Sprintf("/minigw/v1/projects/%d/kb/search", f.pid["fakture"]), RawQuery: "q=Mine"})
	if r.Status != 200 || strings.Contains(string(r.Body), "Mine") {
		t.Fatal("private faq in search", r.Status, r.Body)
	}
	r = f.api.Handle(bg, member("dev-ana"), Request{Method: "GET", Path: fmt.Sprintf("/minigw/v1/projects/%d/kb/search", f.pid["fakture"]), RawQuery: "q=Shared"})
	if !strings.Contains(string(r.Body), `"entryKind":"faq"`) {
		t.Fatal(string(r.Body))
	}
	call(t, f, member("dev-ana"), "GET", fmt.Sprintf("/minigw/v1/projects/%d/kb/faq", f.pid["eureko"]), "", 404)
	call(t, f, Context{Kind: KindMember, DeviceID: "dev-ana", Approved: true, AgentPeer: true}, "GET", faq, "", 403)
	call(t, f, Context{Kind: KindRun, Run: &core.RunCap{}, Approved: true}, "GET", faq, "", 404) // not on the run channel
}

func TestSpoofedEntryKindOnPushAndEditIsIgnored(t *testing.T) {
	f := newFixture(t)
	notes := fmt.Sprintf("/minigw/v1/projects/%d/notes", f.pid["fakture"])
	r := f.api.Handle(bg, member("dev-ana"), Request{Method: "POST", Path: notes, Header: map[string][]string{"Idempotency-Key": {"spoof"}}, Body: []byte(`{"class":"data","title":"T","body":"B","entryKind":"faq"}`)})
	if r.Status != 201 {
		t.Fatal(r.Status, string(r.Body))
	}
	var n NoteJSON
	if err := json.Unmarshal(r.Body, &n); err != nil || n.EntryKind != "finding" {
		t.Fatal(string(r.Body))
	}
	r = f.api.Handle(bg, member("dev-ana"), Request{Method: "PUT", Path: fmt.Sprintf("%s/%d", notes, n.ID), Body: []byte(fmt.Sprintf(`{"class":"data","title":"T2","body":"B","revision":%d,"entryKind":"faq"}`, n.Revision))})
	if r.Status != 200 {
		t.Fatal(r.Status, string(r.Body))
	}
	if err := json.Unmarshal(r.Body, &n); err != nil || n.EntryKind != "finding" || n.Title != "T2" {
		t.Fatal(string(r.Body))
	}
	var kind string
	if err := f.st.DB().QueryRow(`SELECT entry_kind FROM notes WHERE id=?`, n.ID).Scan(&kind); err != nil || kind != "finding" {
		t.Fatal(kind, err)
	}
}
