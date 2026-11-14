package main

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestAllowedFolderReplacedBetweenRuns(t *testing.T) {
	for _, parent := range []bool{false, true} {
		t.Run(map[bool]string{false: "folder", true: "parent"}[parent], func(t *testing.T) {
			base := realOf(t, t.TempDir())
			root := filepath.Join(base, "allowed")
			folder := root
			if parent {
				folder = filepath.Join(root, "child")
			}
			if err := os.MkdirAll(folder, 0700); err != nil {
				t.Fatal(err)
			}
			s, _ := testServer(t, func(c *serveConfig) { c.AllowedFolders = []string{folder} })
			var logs bytes.Buffer
			s.log = log.New(&logs, "", 0)
			folders := func() []string { return s.agentFolders(s.conf(), base) }
			if got := folders(); !slices.Equal(got, []string{folder}) {
				t.Fatalf("initial folders: %v", got)
			}
			target := realOf(t, t.TempDir())
			if parent {
				if err := os.Mkdir(filepath.Join(target, "child"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.RemoveAll(root); err != nil {
				t.Fatal(err)
			}
			dirLink(t, target, root)
			for i := 0; i < 2; i++ {
				if got := folders(); len(got) != 0 {
					t.Fatalf("replacement given to agents: %v", got)
				}
			}
			if strings.Count(logs.String(), "its real path changed") != 1 {
				t.Fatalf("refusal log: %s", &logs)
			}
			for _, body := range []string{`{"permissionMode":"acceptEdits"}`, `{"folders":` + strconv.Quote(folder) + `}`} {
				if st, b := s.adminDo("PATCH", "/agents/claude", []byte(body)); st != 200 {
					t.Fatalf("save: %d %v", st, b)
				}
				if got := folders(); len(got) != 0 {
					t.Fatalf("save trusted replacement: %v", got)
				}
			}
			if st, b := s.adminDo("PATCH", "/agents/claude", []byte(`{"folders":""}`)); st != 200 {
				t.Fatalf("remove: %d %v", st, b)
			}
			if st, b := s.adminDo("PATCH", "/agents/claude", []byte(`{"folders":`+strconv.Quote(folder)+`}`)); st != 200 {
				t.Fatalf("add: %d %v", st, b)
			}
			if got := folders(); len(got) != 1 {
				t.Fatalf("trusted replacement refused: %v", got)
			}
		})
	}
}

func TestAllowedFolderNonDirectoryDoesNotMarkMissing(t *testing.T) {
	base := realOf(t, t.TempDir())
	parent := filepath.Join(base, "parent")
	folder := filepath.Join(parent, "child")
	if err := os.MkdirAll(folder, 0700); err != nil {
		t.Fatal(err)
	}
	s, _ := testServer(t, func(c *serveConfig) { c.AllowedFolders = []string{folder} })
	if err := os.RemoveAll(folder); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(folder, []byte("file"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := s.agentFolders(s.conf(), base); len(got) != 0 {
		t.Fatalf("not a directory accepted: %v", got)
	}
	if _, marked := s.wasMissing.Load(folder); marked {
		t.Fatal("non-directory marked missing")
	}
	if err := os.Remove(folder); err != nil {
		t.Fatal(err)
	}
	if got := s.agentFolders(s.conf(), base); len(got) != 0 {
		t.Fatalf("missing directory accepted: %v", got)
	}
	if _, marked := s.wasMissing.Load(folder); !marked {
		t.Fatal("ENOENT did not mark missing")
	}
}

// Configuration keeps its spelling; runs use the checked final path.
func TestAllowedFolderRetainedAlias(t *testing.T) {
	base := realOf(t, t.TempDir())
	first, second, alias := filepath.Join(base, "first"), filepath.Join(base, "second"), filepath.Join(base, "alias")
	for _, p := range []string{first, second} {
		if err := os.Mkdir(p, 0700); err != nil {
			t.Fatal(err)
		}
	}
	dirLink(t, first, alias)
	s, _ := testServer(t, nil)
	c := s.conf()
	c.AllowedFolders = []string{alias}
	if _, err := checkAgent(&c, s.cfgDir, []string{alias}); err != nil {
		t.Fatal(err)
	}
	if c.AllowedFolders[0] != alias || !samePath(c.AllowedRecords[alias].Real, first) {
		t.Fatalf("recorded identity: %v %v", c.AllowedFolders, c.AllowedRecords)
	}
	want := first
	if runtime.GOOS == "windows" { // #93: keeps the drive name, identity checked at launch
		want = alias
	}
	if got := s.agentFolders(c, base); !slices.Equal(got, []string{want}) {
		t.Fatalf("trusted alias refused: %v", got)
	}
	if err := os.Remove(alias); err != nil {
		t.Fatal(err)
	}
	dirLink(t, second, alias)
	if got := s.agentFolders(c, base); len(got) != 0 {
		t.Fatalf("retargeted alias accepted: %v", got)
	}
}
