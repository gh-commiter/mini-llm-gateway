package coreapi

import (
	"encoding/json"
	"fmt"
	"minigw/core"
	"strings"
	"testing"
)

func TestNoteHistoryRoutes(t *testing.T) {
	f := newFixture(t)
	pid := f.pid["fakture"]
	id := f.nid["private"]
	p := core.Principal{UserID: f.uid["fani"], DeviceID: "dev-fani"}
	_, e := f.st.UpdateNote(bg, p, pid, id, 1, core.NoteInput{Class: core.Data, Title: "current", Body: "current"})
	if e != nil {
		t.Fatal(e)
	}
	memberPath := fmt.Sprintf("/minigw/v1/projects/%d/notes/%d", pid, id)
	ownerPath := fmt.Sprintf("/minigw/admin/v1/kb/notes/%d", id)
	reads := func() int {
		events, e := f.st.AuditEvents(bg, 0, 500)
		if e != nil {
			t.Fatal(e)
		}
		n := 0
		for _, v := range events {
			if v.Action == "kb.owner.read" {
				n++
			}
		}
		return n
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
	}{{member("dev-fani"), memberPath}, {Context{Kind: KindAdmin}, ownerPath}} {
		r := call(tc.c, "GET", tc.path+"/history", "", 200)
		var h core.NoteHistory
		if e = json.Unmarshal(r.Body, &h); e != nil {
			t.Fatal(e)
		}
		if len(h.History) != 1 || h.History[0].Revision != 1 || h.History[0].Body != nil || h.Note.Revision != 2 {
			t.Fatal(h)
		}
		if reads() != 0 {
			t.Fatal("list audited")
		}
		for _, bad := range []string{`{}`, `null`, `[]`, `{"revision":1,"expectedRevision":2,"Class":"data"}`, `{"revision":null,"expectedRevision":2,"class":"data"}`, `{"revision":1,"expectedRevision":2,"class":"data","extra":0}`, `{"revision":1,"revision":1,"expectedRevision":2,"class":"data"}`} {
			call(tc.c, "POST", tc.path+"/restore", bad, 400)
		}
	}
	for i := 1; i <= 2; i++ {
		call(Context{Kind: KindAdmin}, "GET", ownerPath+"/history/1", "", 200)
		if reads() != i {
			t.Fatal("owner row not audited once")
		}
	}
	call(member("dev-fani"), "GET", memberPath+"/history/1", "", 200)
	if reads() != 2 {
		t.Fatal("member audited as owner")
	}
	if e = f.st.AddGrant(bg, pid, f.pid["eureko"], core.Data); e != nil {
		t.Fatal(e)
	}
	r := call(member("dev-cene"), "GET", memberPath+"/history", "", 200)
	if strings.Contains(string(r.Body), "t-private") {
		t.Fatal("private history leaked")
	}
	call(member("dev-cene"), "GET", memberPath+"/history/1", "", 404)
	for _, path := range []string{memberPath, ownerPath} {
		call(Context{Kind: KindInternal}, "GET", path+"/history", "", 404)
	}
	cap := core.RunCap{RunID: "r", UserID: p.UserID, DeviceID: p.DeviceID, ProjectID: pid, Epoch: 1}
	for _, method := range []string{"GET", "POST"} {
		for _, tail := range []string{"history", "history/1", "restore"} {
			call(Context{Kind: KindRun, Run: &cap, Approved: true}, method, "/minigw/v1/notes/1/"+tail, "", 404)
		}
	}
	for _, who := range []string{"ana", "cene"} {
		call(member("dev-"+who), "POST", memberPath+"/restore", `{"revision":1,"expectedRevision":2,"class":"data"}`, 403)
	}
	call(member("dev-fani"), "POST", memberPath+"/restore", `{"revision":1,"expectedRevision":2,"class":"structure"}`, 400)
	call(member("dev-fani"), "POST", memberPath+"/restore", `{"revision":1,"expectedRevision":2,"class":"data"}`, 200)
	call(member("dev-fani"), "POST", memberPath+"/restore", `{"revision":1,"expectedRevision":2,"class":"data"}`, 409)
	call(Context{Kind: KindAdmin}, "POST", ownerPath+"/restore", `{"revision":2,"expectedRevision":3,"class":"data"}`, 200)
	call(Context{Kind: KindAdmin}, "GET", ownerPath+"/history/999", "", 404)
	// The current class also makes a shared saved row a private owner read.
	_, e = f.st.UpdateNote(bg, p, pid, id, 4, core.NoteInput{Class: core.Private, Title: "private now", Body: "private now"})
	if e != nil {
		t.Fatal(e)
	}
	before := reads()
	call(Context{Kind: KindAdmin}, "GET", ownerPath+"/history", "", 200)
	if reads() != before {
		t.Fatal("private current list audited")
	}
	call(Context{Kind: KindAdmin}, "GET", ownerPath+"/history/2", "", 200)
	if reads() != before+1 {
		t.Fatal("private current read not audited")
	}
	call(member("dev-cene"), "GET", memberPath+"/history", "", 404)
	for _, tail := range []string{"history/0", "history/1/extra", "restore/1", "unknown"} {
		call(member("dev-fani"), "GET", memberPath+"/"+tail, "", 404)
	}
}
