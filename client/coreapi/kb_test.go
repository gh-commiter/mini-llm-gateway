package coreapi

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"minigw/core"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
)

func TestKBAdminPreservesIntegerIDs(t *testing.T) {
	dir := t.TempDir()
	f := newFixtureIn(t, dir)
	v, _, err := f.st.Submit(bg, core.Principal{UserID: f.uid["ana"], DeviceID: "dev-ana"}, f.pid["fakture"], "large", core.SubmissionInput{NoteInput: core.NoteInput{Class: core.Data, Title: "Proposal", Body: "body"}})
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "core.db"))
	if err != nil {
		t.Fatal(err)
	}
	const id int64 = 9007199254740993
	_, err = db.Exec(`UPDATE kb_submissions SET id=? WHERE id=?`, id, v.ID)
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	r := f.api.Handle(bg, Context{Kind: KindAdmin}, get(fmt.Sprintf("/kb/submissions/%d", id)))
	if r.Status != 200 {
		t.Fatal(r.Status, string(r.Body))
	}
	if err = json.Unmarshal(r.Body, &v); err != nil {
		t.Fatal(err)
	}
	if v.ID != id {
		t.Fatal("submission ID rounded", string(r.Body))
	}
}

func TestKBIdentityRoutesAndReplay(t *testing.T) {
	f := newFixture(t)
	path := fmt.Sprintf("/minigw/v1/projects/%d/kb/submissions", f.pid["fakture"])
	payload := `{"class":"structure","title":"Proposal","body":"pending body"}`
	req := Request{Method: "POST", Path: path, Header: http.Header{"Idempotency-Key": {"k"}}, Body: []byte(payload)}
	for _, field := range []string{`"author":"fani"`, `"user":999`, `"device":"dev-fani"`, `"project":999`, `"provenance":{"actorKind":"gateway service"}`} {
		for _, suffix := range []string{"/kb/submissions", "/notes"} {
			spoof := req
			spoof.Path = fmt.Sprintf("/minigw/v1/projects/%d%s", f.pid["fakture"], suffix)
			spoof.Body = []byte(payload[:len(payload)-1] + "," + field + "}")
			r := f.api.Handle(bg, member("dev-ana"), spoof)
			if r.Status != 400 {
				t.Fatalf("spoof %s: %d %s", field, r.Status, r.Body)
			}
		}
	}
	for _, c := range []Context{member("dev-eva"), member("dev-cene"), {Kind: KindMember, DeviceID: "dev-ana", Approved: true, AgentPeer: true}} {
		r := f.api.Handle(bg, c, req)
		if r.Status != 403 {
			t.Fatal(r.Status, string(r.Body))
		}
	}
	r := f.api.Handle(bg, member("dev-ana"), req)
	if r.Status != 201 {
		t.Fatal(r.Status, string(r.Body))
	}
	var v core.Submission
	if e := json.Unmarshal(r.Body, &v); e != nil {
		t.Fatal(e)
	}
	if v.User != f.uid["ana"] || v.Provenance.Device != "dev-ana" || v.Project != f.pid["fakture"] {
		t.Fatal(v)
	}
	r = f.api.Handle(bg, member("dev-ana"), req)
	if r.Status != 201 || r.Header.Get("Idempotent-Replay") != "true" {
		t.Fatal(r)
	}
	own := get(fmt.Sprintf("%s/%d", path, v.ID))
	if r = f.api.Handle(bg, member("dev-fani"), own); r.Status != 404 {
		t.Fatal("project admin saw pending", r)
	}
	if r = f.api.Handle(bg, member("dev-ana"), own); r.Status != 200 {
		t.Fatal(r)
	}
	for _, agent := range []Context{{Kind: KindRun}, {Kind: KindMember, Approved: true, DeviceID: "dev-ana", AgentPeer: true}} {
		r = f.api.Handle(bg, agent, get(path))
		if r.Status != 403 && r.Status != 404 {
			t.Fatal("agent access", r)
		}
	}
	admin := Context{Kind: KindAdmin}
	r = f.api.Handle(bg, admin, Request{Method: "GET", Path: "/kb/submissions", RawQuery: "project=0"})
	if r.Status != 200 {
		t.Fatal(r.Status, string(r.Body))
	}
	action := Request{Method: "POST", Path: fmt.Sprintf("/kb/submissions/%d/accept", v.ID), Body: []byte(`{}`)}
	for i := 0; i < 2; i++ {
		r = f.api.Handle(bg, admin, action)
		if r.Status != 200 {
			t.Fatal(r.Status, string(r.Body))
		}
	}
	r = f.api.Handle(bg, member("dev-ana"), get(fmt.Sprintf("/minigw/v1/projects/%d/notes/%d", v.Project, 4)))
	if r.Status != 200 {
		t.Fatal(r.Status, string(r.Body))
	}
	var n NoteJSON
	if e := json.Unmarshal(r.Body, &n); e != nil {
		t.Fatal(e)
	}
	if n.Provenance == nil || n.Provenance.User != v.User || n.Provenance.Reviewer != "owner" {
		t.Fatal(string(r.Body))
	}
}

func TestMemberReviewRoutes(t *testing.T) {
	f := newFixture(t)
	pid := f.pid["fakture"]
	v, _, err := f.st.Submit(bg, core.Principal{UserID: f.uid["ana"], DeviceID: "dev-ana"}, pid, "k1", core.SubmissionInput{NoteInput: core.NoteInput{Class: core.Data, Title: "Proposal", Body: "body"}})
	if err != nil {
		t.Fatal(err)
	}
	list := get(fmt.Sprintf("/minigw/v1/projects/%d/kb/review", pid))
	one := get(fmt.Sprintf("/minigw/v1/projects/0/kb/review/%d", v.ID))
	accept := Request{Method: "POST", Path: fmt.Sprintf("/minigw/v1/projects/0/kb/review/%d/accept", v.ID), Body: []byte(`{"reason":""}`)}
	for _, dev := range []string{"dev-ana", "dev-eva", "dev-cene"} { // not an admin of the project: as if unseen
		for _, req := range []Request{list, one, accept} {
			if r := f.api.Handle(bg, member(dev), req); r.Status != 404 {
				t.Fatal(dev, req.Path, r.Status, string(r.Body))
			}
		}
	}
	if r := f.api.Handle(bg, Context{Kind: KindAdmin}, accept); r.Status == 200 { // a member path is never an admin route
		t.Fatal("admin context served a member path", r.Status)
	}
	r := f.api.Handle(bg, member("dev-fani"), get("/minigw/v1/projects"))
	if r.Status != 200 || !strings.Contains(string(r.Body), `"pendingReview":1`) {
		t.Fatal(r.Status, string(r.Body))
	}
	if r = f.api.Handle(bg, member("dev-ana"), get("/minigw/v1/projects")); strings.Contains(string(r.Body), "pendingReview") {
		t.Fatal("count shown to a developer", string(r.Body))
	}
	if r = f.api.Handle(bg, member("dev-fani"), list); r.Status != 200 || !strings.Contains(string(r.Body), `"title":"Proposal"`) {
		t.Fatal(r.Status, string(r.Body))
	}
	if r = f.api.Handle(bg, member("dev-fani"), one); r.Status != 200 || !strings.Contains(string(r.Body), `"body":"body"`) {
		t.Fatal(r.Status, string(r.Body))
	}
	if r = f.api.Handle(bg, member("dev-fani"), accept); r.Status != 200 || !strings.Contains(string(r.Body), `"state":"accepted"`) {
		t.Fatal(r.Status, string(r.Body))
	}
	own, _, err := f.st.Submit(bg, core.Principal{UserID: f.uid["fani"], DeviceID: "dev-fani"}, pid, "k2", core.SubmissionInput{NoteInput: core.NoteInput{Class: core.Data, Title: "Mine", Body: "b"}})
	if err != nil {
		t.Fatal(err)
	}
	r = f.api.Handle(bg, member("dev-fani"), Request{Method: "POST", Path: fmt.Sprintf("/minigw/v1/projects/%d/kb/review/%d/reject", pid, own.ID), Body: []byte(`{"reason":"x"}`)})
	if r.Status != 400 || !strings.Contains(string(r.Body), "your own submission") {
		t.Fatal(r.Status, string(r.Body))
	}
}
