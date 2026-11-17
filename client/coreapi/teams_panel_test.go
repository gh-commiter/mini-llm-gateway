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

// The counts of the owner's lists come from aggregate queries; they must equal what the detailed lists say.
func TestAdminCountsEqualTheDetailedLists(t *testing.T) {
	f := newFixture(t)
	admin := Context{Kind: KindAdmin}
	do := func(method, path, body string) {
		t.Helper()
		if r := f.api.Handle(bg, admin, Request{Method: method, Path: path, Body: []byte(body)}); r.Status != 200 {
			t.Fatalf("%s %s: %d %s", method, path, r.Status, r.Body)
		}
	}
	for i, name := range []string{"acme", "web", "empty"} {
		var created struct{ Team TeamJSON }
		r := f.api.Handle(bg, admin, Request{Method: "POST", Path: "/teams", Body: []byte(fmt.Sprintf(`{"name":%q}`, name))})
		json.Unmarshal(r.Body, &created)
		id := created.Team.ID
		if i < 2 {
			do("PUT", fmt.Sprintf("/teams/%d/projects/%d", id, []int64{f.pid["fakture"], f.pid["eureko"]}[i]), "")
			do("PUT", fmt.Sprintf("/teams/%d/members/%d", id, f.uid["ana"]), "")
			do("PUT", fmt.Sprintf("/teams/%d/members/%d", id, f.uid["cene"]), "")
			do("PUT", fmt.Sprintf("/teams/%d/assignments", id), fmt.Sprintf(`{"project":%d,"user":%d,"role":"reader"}`, []int64{f.pid["fakture"], f.pid["eureko"]}[i], f.uid["cene"]))
		}
	}
	counts, err := f.st.AdminCounts(bg)
	if err != nil {
		t.Fatal(err)
	}
	ts, _ := f.st.AdminListTeams(bg)
	for _, tm := range ts {
		ms, _ := f.st.ListTeamMembers(bg, tm.ID)
		ps, _ := f.st.ListTeamProjects(bg, tm.ID)
		if counts.MembersOfTeam[tm.ID] != len(ms) || counts.ProjectsOfTeam[tm.ID] != len(ps) {
			t.Errorf("team %s: counts %d/%d, lists %d/%d", tm.Name, counts.MembersOfTeam[tm.ID], counts.ProjectsOfTeam[tm.ID], len(ms), len(ps))
		}
		for _, p := range ps {
			if counts.TeamOfProject[p.ID] != tm.Name {
				t.Errorf("project %d: team %q, want %q", p.ID, counts.TeamOfProject[p.ID], tm.Name)
			}
		}
	}
	ps, _ := f.st.AdminListProjects(bg)
	for _, p := range ps {
		ms, _ := f.st.ListMembers(bg, p.ID)
		if counts.MembersOfProject[p.ID] != len(ms) {
			t.Errorf("project %s: %d people, list says %d", p.Slug, counts.MembersOfProject[p.ID], len(ms))
		}
	}
	// the handler: one aggregate call (four queries) plus the plain list, not a query per team, project or person
	if counts.MembersOfTeam[ts[0].ID] != 2 || len(counts.MembersOfProject) < 2 {
		t.Fatalf("%+v", counts)
	}
}

func TestPanelRouteEdgeCases(t *testing.T) {
	f := newFixture(t)
	admin := Context{Kind: KindAdmin}
	call := func(method, path, query, body string, want int) []byte {
		t.Helper()
		r := f.api.Handle(bg, admin, Request{Method: method, Path: path, RawQuery: query, Body: []byte(body)})
		if r.Status != want {
			t.Fatalf("%s %s?%s %s: %d %s, want %d", method, path, query, body, r.Status, r.Body, want)
		}
		return r.Body
	}
	var created struct{ Team TeamJSON }
	json.Unmarshal(call("POST", "/teams", "", `{"name":"acme"}`, 200), &created)
	team, fk := created.Team.ID, f.pid["fakture"]
	call("PUT", fmt.Sprintf("/teams/%d/projects/%d", team, fk), "", "", 200)
	call("PUT", fmt.Sprintf("/teams/%d/members/%d", team, f.uid["ana"]), "", "", 200)
	call("PUT", fmt.Sprintf("/teams/%d/assignments", team), "", fmt.Sprintf(`{"project":%d,"user":%d,"role":"reader"}`, fk, f.uid["ana"]), 200)
	// both a body and a query: refused, nothing deleted
	both := fmt.Sprintf(`{"project":%d,"user":%d}`, fk, f.uid["ana"])
	call("DELETE", fmt.Sprintf("/teams/%d/assignments", team), fmt.Sprintf("project=%d&user=%d", fk, f.uid["ana"]), both, 400)
	var as struct{ Assignments []adminAssignmentJSON }
	json.Unmarshal(call("GET", fmt.Sprintf("/teams/%d/assignments", team), "", "", 200), &as)
	if len(as.Assignments) != 1 {
		t.Fatal("the assignment was deleted by a refused request")
	}
	call("DELETE", fmt.Sprintf("/teams/%d/assignments", team), "", both, 200) // a body alone still works
	// remotes: an empty single value is refused; an unchanged list writes no audit row
	remotes := fmt.Sprintf("/projects/%d/remotes", fk)
	audits := func() (n int) {
		evs, _ := f.st.AuditEvents(bg, 0, 500)
		for _, e := range evs {
			if e.Action == "project.remotes" {
				n++
			}
		}
		return
	}
	call("POST", remotes, "", `{"addRemote":""}`, 400)
	call("POST", remotes, "", `{"removeRemote":"  "}`, 400)
	call("POST", remotes, "", `{}`, 400)
	if audits() != 0 {
		t.Fatal("a refused call wrote an audit row")
	}
	call("POST", remotes, "", `{"addRemote":"git@host.example:a/b.git"}`, 200)
	call("POST", remotes, "", `{"addRemote":"git@host.example:a/b.git"}`, 200) // the same again: nothing changes
	if audits() != 1 {
		t.Fatalf("%d audit rows, want 1 (the second add changed nothing)", audits())
	}
	// where a role comes from also comes as pairs
	call("PUT", fmt.Sprintf("/teams/%d/assignments", team), "", fmt.Sprintf(`{"project":%d,"user":%d,"role":"reader"}`, fk, f.uid["ana"]), 200)
	var members struct {
		Members []struct {
			Name  string
			Teams []struct{ Team, Role string }
		}
	}
	json.Unmarshal(call("GET", fmt.Sprintf("/projects/%d/members", fk), "", "", 200), &members)
	for _, m := range members.Members {
		if m.Name == "ana" && (len(m.Teams) != 1 || m.Teams[0].Team != "acme" || m.Teams[0].Role != "reader") {
			t.Errorf("ana: %+v", m)
		}
	}
}
