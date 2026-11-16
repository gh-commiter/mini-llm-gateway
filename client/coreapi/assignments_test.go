package coreapi

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestAssignmentRoutes(t *testing.T) {
	f := newFixture(t)
	call := func(c Context, method, path, body string, want int) Response {
		t.Helper()
		r := f.api.Handle(bg, c, Request{Method: method, Path: path, Body: []byte(body)})
		if r.Status != want {
			t.Fatalf("%s %s: %d %s", method, path, r.Status, r.Body)
		}
		if err := f.st.CheckMemberships(bg); err != nil {
			t.Fatal(err)
		}
		return r
	}
	owner := Context{Kind: KindAdmin}
	r := call(owner, "POST", "/teams", `{"name":"Engineering"}`, 200)
	var created struct{ Team TeamJSON }
	if e := json.Unmarshal(r.Body, &created); e != nil {
		t.Fatal(e)
	}
	path := fmt.Sprintf("/teams/%d", created.Team.ID)
	p, u := f.pid["eureko"], f.uid["ana"]
	body := fmt.Sprintf(`{"project":%d,"user":%d,"role":"admin"}`, p, u)
	call(owner, "PUT", path+"/assignments", body, 400)
	call(owner, "PUT", fmt.Sprintf("%s/members/%d", path, u), "", 200)
	call(owner, "PUT", fmt.Sprintf("%s/projects/%d", path, p), "", 200)
	call(owner, "PUT", path+"/assignments", body, 200)
	call(owner, "PUT", path+"/assignments", `{"project":1,"user":1,"role":"reader","actor":"owner"}`, 400)
	r = call(owner, "GET", path+"/assignments", "", 200)
	var assignments struct {
		Assignments []struct {
			Project, User int64
			Role          string
		}
	}
	if e := json.Unmarshal(r.Body, &assignments); e != nil || len(assignments.Assignments) != 1 {
		t.Fatal(string(r.Body), e)
	}
	r = call(member("dev-ana"), "GET", "/minigw/v1/projects", "", 200)
	var projects []ProjectJSON
	if e := json.Unmarshal(r.Body, &projects); e != nil {
		t.Fatal(e)
	}
	found := false
	for _, pr := range projects {
		if pr.ID == p {
			found = true
			if pr.Role != "admin" || len(pr.Origins) != 1 || pr.Origins[0].Name != "Engineering" {
				t.Fatal(pr)
			}
		}
	}
	if !found {
		t.Fatal("assignment not materialized")
	}
	call(member("dev-ana"), "GET", fmt.Sprintf("/minigw/v1/teams/%d/assignments", created.Team.ID), "", 404)
	direct := fmt.Sprintf("/projects/%d/members/%d", p, u)
	r = call(owner, "PUT", direct, `{"role":"reader"}`, 200)
	var result struct {
		Role    string
		Origins []json.RawMessage
	}
	json.Unmarshal(r.Body, &result)
	if result.Role != "admin" || len(result.Origins) != 2 {
		t.Fatal(string(r.Body))
	}
	call(owner, "DELETE", path+"/assignments", fmt.Sprintf(`{"project":%d,"user":%d}`, p, u), 200)
	r = call(member("dev-ana"), "GET", "/minigw/v1/projects", "", 200)
	json.Unmarshal(r.Body, &projects)
	for _, pr := range projects {
		if pr.ID == p && pr.Role != "reader" {
			t.Fatal(pr)
		}
	}
	call(owner, "DELETE", path+"/assignments", fmt.Sprintf(`{"project":%d,"user":%d}`, p, u), 404)
}
