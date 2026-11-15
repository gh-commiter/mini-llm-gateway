package main

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

type gitNoteCases struct {
	Cases []struct {
		Mode            string
		Shell, ReadOnly bool
		Note            string
	}
	Hints [][2]string
}

func TestGitNoteMatchesTheSharedTable(t *testing.T) {
	var c gitNoteCases
	if err := json.Unmarshal(srcFile(t, "testdata/git-notes.json"), &c); err != nil || len(c.Cases) == 0 {
		t.Fatal(err)
	}
	for _, k := range c.Cases {
		if got := gitNote(k.Mode, k.Shell, k.ReadOnly); got != k.Note {
			t.Errorf("%s shell=%v ro=%v:\n got %q\nwant %q", k.Mode, k.Shell, k.ReadOnly, got, k.Note)
		}
		if len(k.Note) > 750 {
			t.Errorf("%s: note is %d characters", k.Mode, len(k.Note))
		}
	}
	for _, h := range c.Hints {
		if got := gitRefusalHint(h[0]); got != h[1] {
			t.Errorf("hint %q: %q, want %q", h[0], got, h[1])
		}
	}
}

// A command the note says works must not write a path the rules refuse, and one it says is not possible must not be writable: checked against the rules directly.
func TestGitNoteAgreesWithTheRules(t *testing.T) {
	note := gitNote("direct", true, false)
	_, afterWorks, _ := strings.Cut(note, "commit in this folder: ")
	said, rest, _ := strings.Cut(afterWorks, ". Not possible in this folder: ")
	refusedPart, _, _ := strings.Cut(rest, ". Use merge")
	works, refused := strings.Split(said, ", "), strings.Split(refusedPart, ", ")
	for _, op := range gitOps {
		allowed := 0
		for _, p := range op.paths {
			if !protectedPathFor(".git/"+p, true) {
				allowed++
			}
		}
		if slices.Contains(works, op.name) && allowed != len(op.paths) {
			t.Errorf("the note says %q works but the rules refuse one of its paths", op.name)
		}
		if slices.Contains(refused, op.name) && allowed != 0 {
			t.Errorf("the note says %q is not possible but the rules allow one of its paths", op.name)
		}
	}
}
