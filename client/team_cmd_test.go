package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"testing"
)

func TestTeamCommands(t *testing.T) {
	var f struct {
		Projects []struct{ Body, Out string }
		Admin    []*admCase
		Member   []struct {
			Name                           string
			Args                           []string
			Body, Out, Error, Path, Lookup string
			Status                         int
		}
	}
	readFixture(t, "teams-kb-commands.json", &f)
	for _, c := range f.Admin {
		t.Run(c.Name, func(t *testing.T) {
			got := runAdminCase(c)
			g, _ := json.Marshal(got)
			w, _ := json.Marshal(c.Want)
			if !bytes.Equal(g, w) {
				t.Fatalf("got %s want %s", g, w)
			}
		})
	}
	for _, c := range f.Projects {
		var out bytes.Buffer
		e := &projectEnv{out: &out, connect: func() (connection, string) { return connection{}, "" }, do: func(_ connection, method, path string, _ map[string]string, _ []byte) (int, []byte, http.Header) {
			if method != "GET" || path != "/minigw/v1/projects" {
				t.Fatalf("%s %s", method, path)
			}
			return 200, []byte(c.Body), nil
		}}
		if err := e.project([]string{"list"}); err != nil {
			t.Fatal(err)
		}
		if out.String() != c.Out {
			t.Fatalf("project origins: got %q want %q", out.String(), c.Out)
		}
	}
	for _, c := range f.Member {
		t.Run(c.Name, func(t *testing.T) {
			var out bytes.Buffer
			calls := 0
			e := &projectEnv{out: &out, connect: func() (connection, string) { return connection{}, "" }, do: func(_ connection, method, path string, _ map[string]string, _ []byte) (int, []byte, http.Header) {
				calls++
				if c.Lookup != "" && calls == 1 {
					if method != "GET" || path != "/minigw/v1/teams?archived=1" {
						t.Fatalf("%s %s", method, path)
					}
					return 200, []byte(c.Lookup), nil
				}
				if method != "GET" || path != c.Path {
					t.Fatalf("%s %s", method, path)
				}
				return c.Status, []byte(c.Body), nil
			}}
			err := e.team(c.Args)
			msg := ""
			if err != nil {
				msg = err.Error()
			}
			wantCalls := 1
			if c.Lookup != "" {
				wantCalls++
			}
			if out.String() != c.Out || msg != c.Error || calls != wantCalls {
				t.Fatalf("out %q err %q calls %d", out.String(), msg, calls)
			}
		})
	}
}

func TestTeamCommandRefusesAgentAndBadArguments(t *testing.T) {
	e := &projectEnv{agent: true}
	if err := e.team([]string{"list"}); err == nil || err.Error() != agentRefusal {
		t.Fatal(err)
	}
	e.agent = false
	for _, args := range [][]string{nil, {"show"}, {"list", "extra"}, {"unknown"}, {"list", "--bad"}} {
		if err := e.team(args); err == nil || err.Error() != "usage:\n"+teamHelp {
			t.Fatal(args, err)
		}
	}
}
