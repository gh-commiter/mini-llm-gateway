package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestKBCommandParity(t *testing.T) {
	saved := time.Local
	time.Local = time.UTC // note and search times are printed in local time; the fixture is in UTC
	defer func() { time.Local = saved }()
	var f struct {
		Admin  []*admCase
		Member []struct {
			Args       []string
			Input, Out string
			Responses  []cmdResp
			Requests   [][]string
		}
		NoteJSON, NoteJSONOut string
		MemberErrors          []struct {
			Args  []string
			Error string
		}
	}
	if e := json.Unmarshal(srcFile(t, "testdata/teams-kb-submissions.json"), &f); e != nil {
		t.Fatal(e)
	}
	for _, c := range f.MemberErrors { // refused before any request
		env := &projectEnv{out: new(bytes.Buffer), connect: func() (connection, string) { return connection{}, "" }, do: func(_ connection, m, p string, _ map[string]string, _ []byte) (int, []byte, http.Header) {
			t.Fatalf("%v sent %s %s", c.Args, m, p)
			return 599, nil, nil
		}}
		if e := env.kb(c.Args); e == nil || e.Error() != c.Error {
			t.Fatalf("%v: %v, want %s", c.Args, e, c.Error)
		}
	}
	for _, c := range f.Admin {
		got := runAdminCase(c)
		if !reflect.DeepEqual(got, c.Want) {
			t.Fatalf("%s: got %+v want %+v", c.Name, got, c.Want)
		}
	}
	for _, c := range f.Member {
		var out bytes.Buffer
		var requests [][]string
		env := &projectEnv{out: &out, in: strings.NewReader(c.Input), connect: func() (connection, string) { return connection{}, "" }, do: func(_ connection, method, path string, h map[string]string, b []byte) (int, []byte, http.Header) {
			requests = append(requests, []string{method, path, string(b)})
			if method == "POST" && h["Idempotency-Key"] != "fixed" {
				t.Fatal("missing key", h)
			}
			for _, r := range c.Responses {
				if r.Match == method+" "+path {
					return r.Status, []byte(r.Body), nil
				}
			}
			t.Fatalf("unexpected %s %s", method, path)
			return 599, nil, nil
		}}
		if e := env.kb(c.Args); e != nil {
			t.Fatal(e)
		}
		if out.String() != c.Out || !reflect.DeepEqual(requests, c.Requests) {
			t.Fatalf("%v: %q %+v", c.Args, out.String(), requests)
		}
	}
	var out bytes.Buffer
	e := &projectEnv{out: &out, agent: true, runConn: &connection{}, do: func(_ connection, m, p string, _ map[string]string, _ []byte) (int, []byte, http.Header) {
		if m != "GET" || p != "/minigw/v1/notes/7" {
			t.Fatal(m, p)
		}
		return 200, []byte(f.NoteJSON), nil
	}}
	if err := e.note([]string{"read", "7", "--json"}); err != nil {
		t.Fatal(err)
	}
	if out.String() != f.NoteJSONOut+"\n" {
		t.Fatal(out.String())
	}
	if err := e.kb([]string{"submissions"}); err == nil || err.Error() != agentRefusal {
		t.Fatal(err)
	}
}

func TestKBClientGatewayFlow(t *testing.T) {
	c := newClientFx(t)
	dir := t.TempDir()
	e, out, _ := c.env("ana", dir, "Reviewed knowledge")
	if err := e.kb([]string{"submit", "fakture", "--key", "proposal", "--title", "Proposal", "-"}); err != nil {
		t.Fatal(err)
	}
	if out.String() != "submission 1 pending\n" {
		t.Fatal(out.String())
	}
	out.Reset()
	if err := e.project([]string{"link", "fakture"}); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if err := e.note([]string{"list"}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "Proposal") || len(c.f.listIDs("ana", "fakture", "")) != 0 {
		t.Fatal("pending content appeared in notes", out.String())
	}
	reader, _, _ := c.env("eva", dir, "Reader proposal")
	if err := reader.kb([]string{"submit", "fakture", "--key", "reader", "-"}); err == nil {
		t.Fatal("reader submitted")
	}
	owner := &adminClient{off: c.f.s}
	review := stdout(t, func() { kbAdmin(owner, []string{"kb", "review"}, "", "") })
	if !strings.Contains(review, "Reviewed knowledge") {
		t.Fatal(review)
	}
	accepted := stdout(t, func() { kbAdmin(owner, []string{"kb", "accept", "1"}, "", "") })
	if accepted != "submission 1 accepted\n" {
		t.Fatal(accepted)
	}
	out.Reset()
	if err := e.note([]string{"read", "1", "--json"}); err != nil {
		t.Fatal(err)
	}
	var note struct {
		Body       string
		Provenance struct{ Device, Reviewer string }
	}
	if err := json.Unmarshal(out.Bytes(), &note); err != nil {
		t.Fatal(err)
	}
	if note.Body != "Reviewed knowledge" || note.Provenance.Device != devID(c.f.dev["ana"]) || note.Provenance.Reviewer != "owner" {
		t.Fatal(out.String())
	}
	out.Reset()
	if err := e.kb([]string{"search", "knowledge"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Proposal") || !strings.Contains(out.String(), "Reviewed knowledge") {
		t.Fatal(out.String())
	}
}

func TestKBRunSearchParity(t *testing.T) {
	saved := time.Local
	time.Local = time.UTC // search times are printed in local time; the fixture is in UTC
	defer func() { time.Local = saved }()
	var f struct {
		RunSearch []struct {
			Args                   []string
			Path, Body, Out, Error string
		}
	}
	if err := json.Unmarshal(srcFile(t, "testdata/teams-kb-submissions.json"), &f); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.RunSearch {
		var out bytes.Buffer
		calls := 0
		e := &projectEnv{agent: true, runConn: &connection{}, out: &out, do: func(_ connection, m, p string, _ map[string]string, _ []byte) (int, []byte, http.Header) {
			calls++
			if m != "GET" || p != c.Path || !notesPath(m, p[:strings.Index(p, "?")]) {
				t.Fatal(m, p)
			}
			return 200, []byte(c.Body), nil
		}}
		err := e.kb(c.Args)
		if c.Error != "" {
			if err == nil || err.Error() != c.Error || calls != 0 {
				t.Fatal(err, calls)
			}
			continue
		}
		if err != nil || out.String() != c.Out || calls != 1 {
			t.Fatal(err, out.String(), calls)
		}
	}
	e := &projectEnv{agent: true}
	if err := e.kb([]string{"search", "comet"}); err == nil || err.Error() != agentRefusal {
		t.Fatal(err)
	}
}

// --team over a real gateway: the team's own notes are written by a team member who is admin of a team project, read
// by every team member, found by a search of the project, and invisible to anyone off the roster.
func TestKBTeamFlagEndToEnd(t *testing.T) {
	c := newClientFx(t)
	core, ctx := c.f.s.core, t0
	team, err := core.CreateTeam(ctx, "acme")
	if err != nil {
		t.Fatal(err)
	}
	for _, u := range []string{"ana", "fani"} { // eva is a project reader off the roster
		if err = core.AddTeamMember(ctx, team.ID, c.f.uid[u]); err != nil {
			t.Fatal(err)
		}
	}
	if err = core.AddTeamProject(ctx, team.ID, c.f.pid["fakture"]); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	out, err := c.run("fani", dir, "Migrate before deploy.", "note", "push", "--team", "acme", "--title", "Deploy order", "-")
	if err != nil || !strings.Contains(out, "created (data") {
		t.Fatal(out, err)
	}
	if out, err = c.run("ana", dir, "x", "note", "push", "--team", "acme", "-"); err == nil {
		t.Fatal("a roster member who is not a project admin wrote", out)
	}
	if out = c.must("ana", dir, "", "note", "list", "--team", "acme"); !strings.Contains(out, "Deploy order") {
		t.Fatal(out)
	}
	e, o, _ := c.env("ana", dir, "")
	if err = e.kb([]string{"search", "--team", "acme", "migrate"}); err != nil || !strings.Contains(o.String(), "Deploy order") || !strings.Contains(o.String(), "from team:acme") {
		t.Fatal(o.String(), err)
	}
	o.Reset()
	if err = e.kb([]string{"search", "fakture", "migrate"}); err != nil || !strings.Contains(o.String(), "from team:acme") {
		t.Fatal(o.String(), err)
	}
	eva, ev, _ := c.env("eva", dir, "")
	if err = eva.kb([]string{"search", "fakture", "migrate"}); err != nil || strings.Contains(ev.String(), "Deploy") {
		t.Fatal(ev.String(), err)
	}
	if out, err = c.run("eva", dir, "", "note", "list", "--team", "acme"); err == nil || !strings.Contains(err.Error(), "404") && !strings.Contains(err.Error(), "not found") {
		t.Fatal("off the roster", out, err)
	}
}
