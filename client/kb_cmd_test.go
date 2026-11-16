package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func TestKBCommandParity(t *testing.T) {
	var f struct {
		Admin  []*admCase
		Member []struct {
			Args       []string
			Input, Out string
			Responses  []cmdResp
			Requests   [][]string
		}
		NoteJSON string
	}
	if e := json.Unmarshal(srcFile(t, "testdata/teams-kb-submissions.json"), &f); e != nil {
		t.Fatal(e)
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
	if out.String() != f.NoteJSON+"\n" {
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
	review := stdout(t, func() { kbAdmin(owner, []string{"kb", "review"}, "") })
	if !strings.Contains(review, "Reviewed knowledge") {
		t.Fatal(review)
	}
	accepted := stdout(t, func() { kbAdmin(owner, []string{"kb", "accept", "1"}, "") })
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
}
