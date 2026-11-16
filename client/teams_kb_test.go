package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// The Mac router runs the same sequence through its supervised helper.
func TestTeamsKBNoteRetrievalParity(t *testing.T) {
	var cases []struct {
		User, Method, Path, Body, Key string
		Want                          int
	}
	if err := json.Unmarshal(srcFile(t, "testdata/teams-kb-retrieval.json"), &cases); err != nil {
		t.Fatal(err)
	}
	f := newCoreFx(t, nil)
	var id int64
	var invisible string
	for i, c := range cases {
		path := f.notesURL("fakture") + strings.ReplaceAll(c.Path, "{note}", fmt.Sprint(id))
		r := f.call(c.User, c.Method, path, c.Body, "Idempotency-Key", c.Key)
		if r.status != c.Want {
			t.Fatalf("case %d: %d %s, want %d", i, r.status, r.body, c.Want)
		}
		if r.status == 201 {
			var n struct{ ID int64 }
			if err := json.Unmarshal([]byte(r.body), &n); err != nil {
				t.Fatal(err)
			}
			id = n.ID
		}
		if r.status == 404 {
			if invisible == "" {
				invisible = r.body
			}
			if r.body != invisible {
				t.Fatalf("case %d: missing/invisible differ: %s", i, r.body)
			}
		}
	}
}
