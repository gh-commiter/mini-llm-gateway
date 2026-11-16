package coreapi

import (
	"fmt"
	"minigw/core"
	"strings"
	"testing"
)

func TestSearchRoutesAndCapability(t *testing.T) {
	f := newFixture(t)
	project := f.pid["fakture"]
	path := fmt.Sprintf("/minigw/v1/projects/%d/kb/search", project)
	req := Request{Method: "GET", Path: path, RawQuery: "q=uniquecomet"}
	call := func(c Context, r Request, want int) Response {
		t.Helper()
		v := f.api.Handle(bg, c, r)
		if v.Status != want {
			t.Fatalf("%+v: %d %s", r, v.Status, v.Body)
		}
		return v
	}
	before := call(member("dev-eva"), req, 200)
	p := core.Principal{UserID: f.uid["ana"], DeviceID: "dev-ana"}
	_, _, e := f.st.CreateNote(bg, p, project, "search-hidden", core.NoteInput{Class: core.Private, Title: "uniquecomet hidden title", Body: "hidden body"})
	if e != nil {
		t.Fatal(e)
	}
	after := call(member("dev-eva"), req, 200)
	if string(before.Body) != string(after.Body) || strings.Contains(string(after.Body), "hidden") {
		t.Fatal(string(after.Body))
	}
	call(Context{Kind: KindMember}, req, 403)
	peer := member("dev-ana")
	peer.AgentPeer = true
	call(peer, req, 403)
	bad := req
	bad.RawQuery = "q="
	call(member("dev-ana"), bad, 400)
	epoch, e := f.st.Epoch(bg)
	if e != nil {
		t.Fatal(e)
	}
	cap := core.RunCap{UserID: p.UserID, DeviceID: p.DeviceID, ProjectID: project, Epoch: epoch}
	run := Context{Kind: KindRun, Run: &cap, Approved: true}
	rr := Request{Method: "GET", Path: "/minigw/v1/kb/search", RawQuery: "q=uniquecomet&project=1&user=1&class=private"}
	result := call(run, rr, 200)
	if string(result.Body) != `{"hits":[]}` {
		t.Fatal(string(result.Body))
	}
	call(run, req, 404)
	call(member("dev-ana"), rr, 404)
	rebuild := Request{Method: "POST", Path: "/kb/search/rebuild"}
	call(run, rebuild, 404)
	call(member("dev-ana"), rebuild, 404)
	call(Context{Kind: KindAdmin}, rebuild, 200)
	if e = f.st.RemoveMember(bg, project, p.UserID); e != nil {
		t.Fatal(e)
	}
	call(run, rr, 403)
}

func TestSearchAuthorizationChanges(t *testing.T) {
	for _, change := range []string{"approval", "binding", "epoch", "membership", "grant", "missing project"} {
		t.Run(change, func(t *testing.T) {
			f := newFixture(t)
			epoch, _ := f.st.Epoch(bg)
			cap := core.RunCap{UserID: f.uid["cene"], DeviceID: "dev-cene", ProjectID: f.pid["eureko"], Epoch: epoch}
			run := Context{Kind: KindRun, Run: &cap, Approved: true}
			req := Request{Method: "GET", Path: "/minigw/v1/kb/search", RawQuery: "q=body&project=999&class=private"}
			before := f.api.Handle(bg, run, req)
			if before.Status != 200 || !strings.Contains(string(before.Body), "t-structure") || strings.Contains(string(before.Body), "t-private") || strings.Contains(string(before.Body), "t-data") {
				t.Fatalf("%d %s", before.Status, before.Body)
			}
			var err error
			switch change {
			case "approval":
				run.Approved = false
			case "binding":
				err = f.st.SetBinding(bg, "dev-cene", f.uid["ana"])
			case "epoch":
				_, err = f.st.BumpEpoch(bg, "test")
			case "membership":
				err = f.st.RemoveMember(bg, f.pid["eureko"], f.uid["cene"])
			case "grant":
				err = f.st.RemoveGrant(bg, f.pid["fakture"], f.pid["eureko"], core.Structure)
			case "missing project":
				cap.ProjectID = 0
			}
			if err != nil {
				t.Fatal(err)
			}
			after := f.api.Handle(bg, run, req)
			if after.Status != 403 {
				t.Fatalf("%d %s", after.Status, after.Body)
			}
			if strings.Contains(string(after.Body), "t-structure") {
				t.Fatal("metadata leaked")
			}
		})
	}
}
