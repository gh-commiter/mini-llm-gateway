package coreapi

import (
	"encoding/json"
	"fmt"
	"testing"
)

// A linked gateway reads the hub's generation from the first answer of a push or pull: the project by its slug, or the team list. Other
// answers (the project list, a team's detail) do not carry it.
func TestProjectAndTeamAnswersCarryTheGeneration(t *testing.T) {
	f := newFixture(t)
	gen, err := f.st.HubGeneration(bg)
	if err != nil || len(gen) != 16 {
		t.Fatal(gen, err)
	}
	adm := Context{Kind: KindAdmin}
	var made struct{ Team TeamJSON }
	if err := json.Unmarshal(call(t, f, adm, "POST", "/teams", `{"name":"Mine"}`, 200).Body, &made); err != nil {
		t.Fatal(err)
	}
	call(t, f, adm, "PUT", fmt.Sprintf("/teams/%d/members/%d", made.Team.ID, f.uid["ana"]), "", 200)
	var pr ProjectJSON
	if err := json.Unmarshal(call(t, f, member("dev-ana"), "GET", "/minigw/v1/projects/fakture", "", 200).Body, &pr); err != nil || pr.Generation != gen {
		t.Fatalf("project: %+v %v", pr, err)
	}
	var teams []TeamJSON
	if err := json.Unmarshal(call(t, f, member("dev-ana"), "GET", "/minigw/v1/teams", "", 200).Body, &teams); err != nil || len(teams) != 1 || teams[0].Generation != gen {
		t.Fatalf("teams: %+v %v", teams, err)
	}
	var list []ProjectJSON
	if err := json.Unmarshal(call(t, f, member("dev-ana"), "GET", "/minigw/v1/projects", "", 200).Body, &list); err != nil || len(list) == 0 || list[0].Generation != "" {
		t.Fatalf("list: %+v %v", list, err)
	}
}
