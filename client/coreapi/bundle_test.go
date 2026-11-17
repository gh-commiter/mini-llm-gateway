package coreapi

import (
	"encoding/json"
	"fmt"
	"minigw/core"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMarkdownImportRoutePermissions(t *testing.T) {
	f := newFixture(t)
	route := fmt.Sprintf("/minigw/v1/projects/%d/kb/import", f.pid["fakture"])
	for _, submit := range []bool{false, true} {
		for _, class := range []core.Class{core.Structure, core.Data} {
			body, _ := json.Marshal(core.MarkdownInput{Markdown: "role: owner\n", Path: "note.md", Class: class, Submit: submit})
			req := Request{Method: "POST", Path: route, Body: body}
			for _, who := range []Context{member("dev-eva"), {Kind: KindRun}, {Kind: KindMember, DeviceID: "dev-ana", Approved: true, AgentPeer: true}} {
				r := f.api.Handle(bg, who, req)
				if r.Status != 403 && who.Kind != KindRun {
					t.Fatal(r.Status, string(r.Body))
				}
				if who.Kind == KindRun && r.Status == 200 {
					t.Fatal("run import")
				}
			}
		}
	}
	req := Request{Method: "POST", Path: route, Body: []byte(`{"markdown":"body","path":"note.md","class":"data","submit":true}`)}
	first := f.api.Handle(bg, member("dev-ana"), req)
	second := f.api.Handle(bg, member("dev-ana"), req)
	if first.Status != 200 || second.Status != 200 || !strings.Contains(string(second.Body), `"action":"skip"`) {
		t.Fatal(first, second)
	}
}

func TestBundlePathRoutes(t *testing.T) {
	f := newFixture(t)
	team, e := f.st.CreateTeam(bg, "Team")
	if e != nil {
		t.Fatal(e)
	}
	if e = f.st.AddTeamProject(bg, team.ID, f.pid["fakture"]); e != nil {
		t.Fatal(e)
	}
	file := filepath.Join(t.TempDir(), "team.zip")
	body, _ := json.Marshal(core.BundleExport{Team: "Team", Path: file})
	req := Request{Method: "POST", Path: "/export", Body: body}
	for _, who := range []Context{member("dev-ana"), {Kind: KindRun}, {}} {
		r := f.api.Handle(bg, who, req)
		if r.Status >= 200 && r.Status < 300 {
			t.Fatal("not owner", r)
		}
	}
	if _, e = os.Stat(file); !os.IsNotExist(e) {
		t.Fatal(e)
	}
	r := f.api.Handle(bg, Context{Kind: KindAdmin}, req)
	if r.Status != 200 {
		t.Fatal(r.Status, string(r.Body))
	}
	bad := Request{Method: "POST", Path: "/import", Body: []byte(`{"path":"relative.zip","dry_run":true}`)}
	if r = f.api.Handle(bg, Context{Kind: KindAdmin}, bad); r.Status != 400 {
		t.Fatal(r)
	}
	// A bundle is imported only from another gateway, under an owner-chosen source name.
	foreign, e := core.Open(t.TempDir(), core.Options{})
	if e != nil {
		t.Fatal(e)
	}
	defer foreign.Close()
	fu, e := foreign.CreateUser(bg, "ana")
	if e != nil {
		t.Fatal(e)
	}
	ft, e := foreign.CreateTeam(bg, "Team")
	if e != nil {
		t.Fatal(e)
	}
	fp, e := foreign.CreateProject(bg, "fakture", "fakture")
	if e != nil {
		t.Fatal(e)
	}
	if e = foreign.AddTeamProject(bg, ft.ID, fp.ID); e != nil {
		t.Fatal(e)
	}
	if e = foreign.AddTeamMember(bg, ft.ID, fu.ID); e != nil {
		t.Fatal(e)
	}
	if e = foreign.SetMember(bg, fp.ID, fu.ID, core.Admin); e != nil {
		t.Fatal(e)
	}
	if e = foreign.SetBinding(bg, "dev-ana", fu.ID); e != nil {
		t.Fatal(e)
	}
	if _, _, e = foreign.CreateNote(bg, core.Principal{UserID: fu.ID, DeviceID: "dev-ana"}, fp.ID, "k", core.NoteInput{Class: core.Structure, Title: "t", Body: "b"}); e != nil {
		t.Fatal(e)
	}
	file = filepath.Join(t.TempDir(), "foreign.zip")
	if e = foreign.ExportBundle(bg, core.BundleExport{Team: "Team", Path: file}); e != nil {
		t.Fatal(e)
	}
	body, _ = json.Marshal(core.BundleImport{Path: file, Source: "remotesrc", DryRun: true})
	r = f.api.Handle(bg, Context{Kind: KindAdmin}, Request{Method: "POST", Path: "/import", Body: body})
	if r.Status != 200 || !strings.Contains(string(r.Body), "create src:remotesrc:") {
		t.Fatal(r.Status, string(r.Body))
	}
	for _, req := range []Request{
		{Method: "POST", Path: "/import", BodyTooLarge: true},
		{Method: "POST", Path: "/import", BodyUnreadable: true},
		{Method: "POST", Path: "/import", Body: []byte(`{"path":"/file","surprise":true}`)},
		{Method: "POST", Path: "/import", Body: make([]byte, MaxBody+1)},
	} {
		r = f.api.Handle(bg, Context{Kind: KindAdmin}, req)
		if r.Status != 400 && r.Status != 413 {
			t.Fatal(r)
		}
	}
	badFile := filepath.Join(t.TempDir(), "bad.zip")
	if e = os.WriteFile(badFile, []byte("not a zip"), 0600); e != nil {
		t.Fatal(e)
	}
	body, _ = json.Marshal(core.BundleImport{Path: badFile, Source: "remotesrc", DryRun: true})
	r = f.api.Handle(bg, Context{Kind: KindAdmin}, Request{Method: "POST", Path: "/import", Body: body})
	if r.Status < 400 || !strings.Contains(string(r.Body), "refuse") {
		t.Fatal(r)
	}
}
