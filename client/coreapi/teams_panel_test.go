package coreapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"minigw/core"
)

// What the Teams and projects panel reads and sends: the extra fields of the owner's answers (counts, names, where a role comes
// from), the single-remote body, the DELETE of an assignment by query, and that a team's hidden segment never shows up as a project.
func TestTeamsAndProjectsPanelAnswers(t *testing.T) {
	f := newFixture(t)
	admin := Context{Kind: KindAdmin}
	call := func(method, path, body string, want int) []byte {
		t.Helper()
		p, q, _ := strings.Cut(path, "?")
		r := f.api.Handle(bg, admin, Request{Method: method, Path: p, RawQuery: q, Body: []byte(body), Header: http.Header{}})
		if r.Status != want {
			t.Fatalf("%s %s: %d %s, want %d", method, path, r.Status, r.Body, want)
		}
		return r.Body
	}
	var created struct{ Team TeamJSON }
	json.Unmarshal(call("POST", "/teams", `{"name":"acme"}`, 200), &created)
	team, fk, ana := created.Team.ID, f.pid["fakture"], f.uid["ana"]
	call("PUT", fmt.Sprintf("/teams/%d/projects/%d", team, fk), "", 200)
	call("PUT", fmt.Sprintf("/teams/%d/members/%d", team, ana), "", 200)
	call("PUT", fmt.Sprintf("/teams/%d/assignments", team), fmt.Sprintf(`{"project":%d,"user":%d,"role":"reader"}`, fk, ana), 200)
	seg := fmt.Sprint(core.TeamSegment(team))

	var teams struct{ Teams []adminTeamJSON }
	json.Unmarshal(call("GET", "/teams", "", 200), &teams)
	if len(teams.Teams) != 1 || teams.Teams[0].MemberCount != 1 || teams.Teams[0].ProjectCount != 1 || teams.Teams[0].Name != "acme" {
		t.Fatalf("%+v", teams)
	}
	// the project list: the team's name and the number of people; a team's hidden notes segment is never a project
	list := call("GET", "/projects", "", 200)
	var projects struct{ Projects []adminProjectJSON }
	json.Unmarshal(list, &projects)
	if strings.Contains(string(list), seg) || len(projects.Projects) != 2 {
		t.Fatalf("the project list shows a segment or misses a project: %s", list)
	}
	for _, p := range projects.Projects {
		if p.ID == fk && (p.TeamName != "acme" || p.MemberCount != 3) { // ana (direct developer + the team), fani admin, eva reader
			t.Errorf("fakture: %+v", p)
		}
		if p.ID == f.pid["eureko"] && p.TeamName != "" {
			t.Errorf("eureko: %+v", p)
		}
	}
	// where a role comes from: ana has a direct developer role and reader through acme
	var members struct{ Members []map[string]any }
	json.Unmarshal(call("GET", fmt.Sprintf("/projects/%d/members", fk), "", 200), &members)
	found := false
	for _, m := range members.Members {
		if m["name"] == "ana" {
			found = true
			if m["directRole"] != "developer" || m["fromTeams"] != "acme (reader)" || m["role"] != "developer" {
				t.Errorf("ana: %v", m)
			}
		}
	}
	if !found {
		t.Fatal("ana is not listed")
	}
	// a person with a role only through a team has directRole "none"
	cene := f.uid["cene"]
	call("PUT", fmt.Sprintf("/teams/%d/members/%d", team, cene), "", 200)
	call("PUT", fmt.Sprintf("/teams/%d/assignments", team), fmt.Sprintf(`{"project":%d,"user":%d,"role":"developer"}`, fk, cene), 200)
	json.Unmarshal(call("GET", fmt.Sprintf("/projects/%d/members", fk), "", 200), &members)
	for _, m := range members.Members {
		if m["name"] == "cene" && (m["directRole"] != "none" || m["fromTeams"] != "acme (developer)") {
			t.Errorf("cene: %v", m)
		}
	}
	// assignments carry an id and names; DELETE takes the person and project as a query (a panel sends no body with a DELETE)
	var as struct{ Assignments []adminAssignmentJSON }
	json.Unmarshal(call("GET", fmt.Sprintf("/teams/%d/assignments", team), "", 200), &as)
	if len(as.Assignments) != 2 || as.Assignments[0].UserName == "" || as.Assignments[0].ProjectName != "fakture" || as.Assignments[0].ID != fmt.Sprintf("%d-%d", fk, ana) {
		t.Fatalf("%+v", as)
	}
	call("DELETE", fmt.Sprintf("/teams/%d/assignments?project=%d&user=%d", team, fk, cene), "", 200)
	call("DELETE", fmt.Sprintf("/teams/%d/assignments?project=%d&user=%d", team, fk, cene), "", 404) // gone
	call("DELETE", fmt.Sprintf("/teams/%d/assignments?project=x&user=1", team), "", 400)
	call("DELETE", fmt.Sprintf("/teams/%d/assignments", team), "", 400)
	// grants carry an id and the project names
	call("POST", "/grants", fmt.Sprintf(`{"source":%d,"reader":%d,"class":"structure"}`, fk, f.pid["eureko"]), 200)
	g := string(call("GET", "/grants", "", 200))
	if !strings.Contains(g, `"sourceName":"fakture"`) || !strings.Contains(g, `"readerName":"eureko"`) || !strings.Contains(g, fmt.Sprintf(`"id":"%d-%d-structure"`, fk, f.pid["eureko"])) {
		t.Fatalf("grants: %s", g)
	}
	call("DELETE", fmt.Sprintf("/grants?source=%d&reader=%d&class=structure", fk, f.pid["eureko"]), "", 200)
	// one remote at a time, as a form sends it; the list also comes as rows
	var rem struct {
		Remotes []string
		Entries []map[string]string
	}
	json.Unmarshal(call("POST", fmt.Sprintf("/projects/%d/remotes", fk), `{"addRemote":"git@host.example:a/b.git"}`, 200), &rem)
	if len(rem.Remotes) != 1 || len(rem.Entries) != 1 || rem.Entries[0]["remote"] != rem.Remotes[0] {
		t.Fatalf("%+v", rem)
	}
	json.Unmarshal(call("GET", fmt.Sprintf("/projects/%d/remotes", fk), "", 200), &rem)
	if len(rem.Entries) != 1 {
		t.Fatalf("%+v", rem)
	}
	json.Unmarshal(call("POST", fmt.Sprintf("/projects/%d/remotes", fk), fmt.Sprintf(`{"removeRemote":%q}`, rem.Remotes[0]), 200), &rem)
	if len(rem.Entries) != 0 {
		t.Fatalf("%+v", rem)
	}
	call("POST", fmt.Sprintf("/projects/%d/remotes", fk), `{"addRemote":"git@host.example:a/b.git","nope":1}`, 400)
	// the member route (not the owner's) does not take the single-remote keys
	r := f.api.Handle(bg, member("dev-fani"), Request{Method: "POST", Path: fmt.Sprintf("/minigw/v1/projects/%d/remotes", fk), Body: []byte(`{"addRemote":"git@host.example:c/d.git"}`)})
	if r.Status != 400 {
		t.Fatalf("member remotes took addRemote: %d %s", r.Status, r.Body)
	}
}
