package coreapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"minigw/core"
)

// The owner's read routes of the knowledge base (GET /kb/notes, /kb/notes/{id}, /kb/faq) and the names the review queue
// carries: admin channel only, every class for the owner, team notes under the team's name and never the hidden id.
func TestOwnerKBReads(t *testing.T) {
	f := newFixture(t)
	admin := Context{Kind: KindAdmin}
	n := 0
	call := func(c Context, method, path, body string, want int) Response {
		t.Helper()
		p, query, _ := strings.Cut(path, "?")
		n++
		r := f.api.Handle(bg, c, Request{Method: method, Path: p, RawQuery: query, Body: []byte(body), Header: http.Header{"Idempotency-Key": {fmt.Sprint("k", n)}}})
		if r.Status != want {
			t.Fatalf("%s %s: %d %s, want %d", method, path, r.Status, r.Body, want)
		}
		return r
	}
	fk := f.pid["fakture"]
	proj := fmt.Sprintf("/minigw/v1/projects/%d", fk)

	// the private note is readable by the owner, in the list and by id, with its body
	list := string(call(admin, "GET", fmt.Sprintf("/kb/notes?project=%d", fk), "", 200).Body)
	for _, title := range []string{"t-structure", "t-data", "t-private"} {
		if !strings.Contains(list, title) {
			t.Fatalf("owner list lacks %s: %s", title, list)
		}
	}
	if only := string(call(admin, "GET", fmt.Sprintf("/kb/notes?project=%d&class=private", fk), "", 200).Body); !strings.Contains(only, "t-private") || strings.Contains(only, "t-data") {
		t.Fatalf("class filter: %s", only)
	}
	var one NoteJSON
	json.Unmarshal(call(admin, "GET", fmt.Sprintf("/kb/notes/%d", f.nid["private"]), "", 200).Body, &one)
	if one.Body == nil || *one.Body != "body" || one.Class != core.Private || one.Provenance == nil {
		t.Fatalf("note by id: %+v", one)
	}

	// a team's own notes: segment id as the project, the team's name, no hidden id anywhere
	r := call(admin, "POST", "/teams", `{"name":"acme"}`, 200)
	var created struct{ Team TeamJSON }
	json.Unmarshal(r.Body, &created)
	team := created.Team
	call(admin, "PUT", fmt.Sprintf("/teams/%d/projects/%d", team.ID, fk), "", 200)
	call(admin, "PUT", fmt.Sprintf("/teams/%d/members/%d", team.ID, f.uid["fani"]), "", 200)
	seg := fmt.Sprint(core.TeamSegment(team.ID))
	base := "/minigw/v1/projects/" + seg
	call(member("dev-fani"), "POST", base+"/notes", `{"class":"data","title":"deploy order","body":"first migrate"}`, 201)
	call(member("dev-fani"), "POST", base+"/kb/submissions", `{"class":"data","title":"Where is it?","body":"Here.","entryKind":"faq"}`, 201)
	teamList := string(call(admin, "GET", "/kb/notes?project="+seg, "", 200).Body)
	if !strings.Contains(teamList, "deploy order") || !strings.Contains(teamList, `"team":"acme"`) || strings.Contains(teamList, seg) {
		t.Fatalf("team list: %s", teamList)
	}
	if strings.Contains(list, "deploy order") {
		t.Fatal("the project list shows the team's notes")
	}
	// a pending submission is not a note
	if strings.Contains(teamList, "Where is it?") || strings.Contains(string(call(admin, "GET", "/kb/faq?project="+seg, "", 200).Body), "Where is it?") {
		t.Fatal("a pending submission is listed as a note")
	}

	// the review queue carries names; a team's row has scopeKind team and its name, a project's the project's
	call(member("dev-ana"), "POST", proj+"/kb/submissions", `{"class":"data","title":"Q?","body":"A.","entryKind":"faq"}`, 201)
	var q struct {
		Submissions []adminSubmissionJSON
	}
	json.Unmarshal(call(admin, "GET", "/kb/submissions", "", 200).Body, &q)
	if len(q.Submissions) != 2 {
		t.Fatalf("%+v", q)
	}
	for _, s := range q.Submissions {
		switch s.Title {
		case "Q?":
			if s.AuthorName != "ana" || s.ScopeKind != "project" || s.ScopeName != "fakture" || s.Received.IsZero() || s.IsReplacement {
				t.Errorf("project row: %+v", s)
			}
		case "Where is it?":
			if s.AuthorName != "fani" || s.ScopeKind != "team" || s.ScopeName != "acme" {
				t.Errorf("team row: %+v", s)
			}
		}
	}
	// a replacement is marked, and its detail carries the body and the id of the note it replaces
	note := f.nid["data"]
	cur, _ := f.st.OwnerGetNote(bg, note)
	call(member("dev-ana"), "POST", proj+"/kb/submissions", fmt.Sprintf(`{"class":"data","title":"t-data","body":"new text","replaces":%d,"revision":%d}`, note, cur.Revision), 201)
	json.Unmarshal(call(admin, "GET", "/kb/submissions", "", 200).Body, &q)
	var rep adminSubmissionJSON
	for _, s := range q.Submissions {
		if s.Replaces == note {
			json.Unmarshal(call(admin, "GET", fmt.Sprintf("/kb/submissions/%d", s.ID), "", 200).Body, &rep)
		}
	}
	if !rep.IsReplacement || rep.Body == nil || *rep.Body != "new text" || rep.Replaces != note {
		t.Fatalf("replacement detail: %+v", rep)
	}
	// accept the project faq: it now lists as an faq note, and the kind filter finds it
	for _, s := range q.Submissions {
		if s.Title == "Q?" {
			call(admin, "POST", fmt.Sprintf("/kb/submissions/%d/accept", s.ID), "{}", 200)
		}
	}
	if faq := string(call(admin, "GET", fmt.Sprintf("/kb/faq?project=%d", fk), "", 200).Body); !strings.Contains(faq, `"answer":"A."`) {
		t.Fatalf("faq: %s", faq)
	}
	if kinds := string(call(admin, "GET", fmt.Sprintf("/kb/notes?project=%d&kind=faq", fk), "", 200).Body); !strings.Contains(kinds, "Q?") || strings.Contains(kinds, "t-data") {
		t.Fatalf("kind filter: %s", kinds)
	}

	// paging: limit 1 gives a cursor that continues; a cursor of another project is refused
	pg := call(admin, "GET", fmt.Sprintf("/kb/notes?project=%d&limit=1", fk), "", 200)
	var first struct {
		Notes []NoteJSON
		Next  string
	}
	json.Unmarshal(pg.Body, &first)
	if len(first.Notes) != 1 || first.Next == "" {
		t.Fatalf("page: %s", pg.Body)
	}
	call(admin, "GET", fmt.Sprintf("/kb/notes?project=%d&limit=1&after=%s", fk, first.Next), "", 200)
	call(admin, "GET", fmt.Sprintf("/kb/notes?project=%d&after=%s", f.pid["eureko"], first.Next), "", 400)

	// bad requests
	call(admin, "GET", "/kb/notes", "", 400)
	call(admin, "GET", "/kb/notes?project=x", "", 400)
	call(admin, "GET", "/kb/notes?project=999999", "", 404)
	call(admin, "GET", fmt.Sprintf("/kb/notes?project=%d&class=nope", fk), "", 400)
	call(admin, "GET", fmt.Sprintf("/kb/notes?project=%d&kind=nope", fk), "", 400)
	call(admin, "GET", fmt.Sprintf("/kb/notes?project=%d&limit=1000", fk), "", 400)
	call(admin, "GET", "/kb/notes/x", "", 404)
	call(admin, "GET", "/kb/notes/999999", "", 404)
	call(admin, "GET", "/kb/faq", "", 400)
	call(admin, "GET", fmt.Sprintf("/kb/faq/1?project=%d", fk), "", 404)
	call(admin, "POST", "/kb/notes", "{}", 404)
	call(admin, "DELETE", fmt.Sprintf("/kb/notes/%d", f.nid["data"]), "", 404)
}

// The owner routes are admin-only: no member, run, or internal context reaches them, under either spelling.
func TestOwnerKBReadsAreNotOnOtherChannels(t *testing.T) {
	f := newFixture(t)
	fk := f.pid["fakture"]
	cap := core.RunCap{RunID: "r", UserID: f.uid["ana"], DeviceID: "dev-ana", ProjectID: fk, Epoch: 1}
	for _, c := range []Context{member("dev-ana"), member("dev-fani"), {Kind: KindRun, Run: &cap, Approved: true}, {Kind: KindInternal}} {
		for _, p := range []string{"/kb/notes", "/kb/notes/1", "/kb/faq", "/minigw/v1/kb/notes", "/minigw/v1/kb/faq", "/minigw/v1/kb/notes/1", "/minigw/admin/v1/kb/notes"} {
			r := f.api.Handle(bg, c, Request{Method: "GET", Path: p, RawQuery: fmt.Sprintf("project=%d", fk)})
			if r.Status == 200 || strings.Contains(string(r.Body), "t-private") {
				t.Errorf("%s GET %s: %d %s", c.Kind, p, r.Status, r.Body)
			}
		}
	}
	if IsMemberPath("/minigw/v1/kb/notes") || IsMemberPath("/minigw/v1/kb/faq") {
		t.Fatal("a member path")
	}
}
