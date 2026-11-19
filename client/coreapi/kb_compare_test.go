package coreapi

import (
	"encoding/json"
	"fmt"
	"minigw/core"
	"strings"
	"testing"
)

// The member and owner details of a replacement carry the target's fields, and the accept bodies take currentRevision
// as a number only.
func TestReviewDetailAndAcceptBodyOfAReplacement(t *testing.T) {
	f := newFixture(t)
	pid := f.pid["fakture"]
	ana := core.Principal{UserID: f.uid["ana"], DeviceID: "dev-ana"}
	n, _, e := f.st.CreateNote(bg, ana, pid, "cmp-note", core.NoteInput{Class: core.Structure, Title: "Current", Body: "current body"})
	if e != nil {
		t.Fatal(e)
	}
	v, _, e := f.st.Submit(bg, ana, pid, "cmp", core.SubmissionInput{NoteInput: core.NoteInput{Class: core.Structure, Title: "Proposed", Body: "new body"}, Replaces: n.ID, Revision: n.Revision})
	if e != nil {
		t.Fatal(e)
	}
	call := func(c Context, method, path, body string, want int) Response {
		t.Helper()
		r := f.api.Handle(bg, c, Request{Method: method, Path: path, Body: []byte(body)})
		if r.Status != want {
			t.Fatalf("%s %s: %d %s want %d", method, path, r.Status, r.Body, want)
		}
		return r
	}
	for _, tc := range []struct {
		c    Context
		path string
	}{{member("dev-fani"), fmt.Sprintf("/minigw/v1/projects/%d/kb/review/%d", pid, v.ID)}, {Context{Kind: KindAdmin}, fmt.Sprintf("/kb/submissions/%d", v.ID)}} {
		var d map[string]any
		if e := json.Unmarshal(call(tc.c, "GET", tc.path, "", 200).Body, &d); e != nil {
			t.Fatal(e)
		}
		for k, want := range map[string]any{"targetState": "ok", "currentRevision": float64(1), "currentTitle": "Current", "currentClass": "structure", "currentKind": "finding", "currentReviewCheck": "ok", "currentBody": "current body", "proposalCheck": "ok", "reviewCheck": "ok", "title": "Proposed", "body": "new body"} {
			if d[k] != want {
				t.Errorf("%s: %v, want %v", k, d[k], want)
			}
		}
		hash, _ := d["reviewHash"].(string)
		for _, bad := range []string{`{"reviewHash":"` + hash + `","currentRevision":"1"}`, `{"reviewHash":"` + hash + `","currentRevision":1.5}`, `{"reviewHash":"` + hash + `","currentRevision":-1}`} {
			call(tc.c, "POST", tc.path+"/accept", bad, 400)
		}
	}
	// an old client's body on the MEMBER route: no currentRevision for a replacement is refused with the update message
	memberPath := fmt.Sprintf("/minigw/v1/projects/%d/kb/review/%d", pid, v.ID)
	var md map[string]any
	json.Unmarshal(call(member("dev-fani"), "GET", memberPath, "", 200).Body, &md)
	mh, _ := md["reviewHash"].(string)
	if r := call(member("dev-fani"), "POST", memberPath+"/accept", `{"reviewHash":"`+mh+`"}`, 400); !strings.Contains(string(r.Body), "update minigw") {
		t.Fatal(string(r.Body))
	}
	// no currentRevision: refused with the update message (400, a refusal)
	ownerPath := fmt.Sprintf("/kb/submissions/%d", v.ID)
	var d map[string]any
	json.Unmarshal(call(Context{Kind: KindAdmin}, "GET", ownerPath, "", 200).Body, &d)
	hash, _ := d["reviewHash"].(string)
	r := call(Context{Kind: KindAdmin}, "POST", ownerPath+"/accept", `{"reviewHash":"`+hash+`"}`, 400)
	if !strings.Contains(string(r.Body), "update minigw") {
		t.Fatal(string(r.Body))
	}
	call(Context{Kind: KindAdmin}, "POST", ownerPath+"/accept", `{"reviewHash":"`+hash+`","currentRevision":1}`, 200)
}

// The panel's row action offers Accept for a new note only: the owner list marks a replacement row acceptInPanel false.
func TestKBQueueRowsOfferAcceptOnlyForANewNote(t *testing.T) {
	f := newFixture(t)
	pid := f.pid["fakture"]
	ana := core.Principal{UserID: f.uid["ana"], DeviceID: "dev-ana"}
	n, _, _ := f.st.CreateNote(bg, ana, pid, "q-note", core.NoteInput{Class: core.Structure, Title: "Current", Body: "current body"})
	repl, _, e1 := f.st.Submit(bg, ana, pid, "q-repl", core.SubmissionInput{NoteInput: core.NoteInput{Class: core.Structure, Title: "New", Body: "new"}, Replaces: n.ID, Revision: n.Revision})
	plain, _, e2 := f.st.Submit(bg, ana, pid, "q-plain", core.SubmissionInput{NoteInput: core.NoteInput{Class: core.Structure, Title: "Plain", Body: "plain"}})
	if e1 != nil || e2 != nil {
		t.Fatal(e1, e2)
	}
	r := f.api.Handle(bg, Context{Kind: KindAdmin}, Request{Method: "GET", Path: "/kb/submissions", RawQuery: "project=0&after=0"})
	var pg struct {
		Submissions []struct {
			ID            int64
			AcceptInPanel bool
		}
	}
	if r.Status != 200 || json.Unmarshal(r.Body, &pg) != nil || len(pg.Submissions) != 2 {
		t.Fatal(r.Status, string(r.Body))
	}
	for _, s := range pg.Submissions {
		if want := s.ID == plain.ID; s.AcceptInPanel != want || s.ID != plain.ID && s.ID != repl.ID {
			t.Fatalf("submission %d: acceptInPanel %v", s.ID, s.AcceptInPanel)
		}
	}
}
