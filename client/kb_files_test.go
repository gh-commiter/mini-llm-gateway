package main

import (
	"context"
	"encoding/json"
	"fmt"
	"minigw/core"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMarkdownClientGateway(t *testing.T) {
	c := newClientFx(t)
	s := c.f.s.core
	ana := core.Principal{UserID: c.f.uid["ana"], DeviceID: devID(c.f.dev["ana"])}
	pid := c.f.pid["fakture"]
	for _, cl := range []core.Class{core.Structure, core.Data, core.Private} {
		if _, _, e := s.CreateNote(context.Background(), ana, pid, "export-"+string(cl), core.NoteInput{Class: cl, Title: string(cl), Body: "body\r\n---\n\x00"}); e != nil {
			t.Fatal(e)
		}
	}
	if _, _, e := s.Submit(context.Background(), ana, pid, "pending-export", core.SubmissionInput{NoteInput: core.NoteInput{Class: core.Data, Title: "pending", Body: "absent"}}); e != nil {
		t.Fatal(e)
	}
	for _, tc := range []struct {
		user string
		want int
	}{{"ana", 3}, {"eva", 1}, {"bor", 2}} {
		dir := t.TempDir()
		env, _, _ := c.env(tc.user, dir, "")
		if e := env.kb([]string{"export", "fakture", dir}); e != nil {
			t.Fatal(e)
		}
		entries, e := os.ReadDir(dir)
		if e != nil {
			t.Fatal(e)
		}
		if len(entries) != tc.want+1 {
			t.Fatal(tc.user, entries)
		}
		for _, entry := range entries {
			if entry.Name() == "INDEX.md" {
				continue
			}
			data, e := os.ReadFile(filepath.Join(dir, entry.Name()))
			if e != nil {
				t.Fatal(e)
			}
			if !strings.HasPrefix(string(data), "---\nid: ") || !strings.HasSuffix(string(data), "body\r\n---\n\x00") {
				t.Fatalf("%q", data)
			}
		}
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "new.md")
	if e := os.WriteFile(file, []byte("one"), 0600); e != nil {
		t.Fatal(e)
	}
	env, out, _ := c.env("ana", dir, "")
	for i := 0; i < 2; i++ {
		if e := env.kb([]string{"import", "fakture", file}); e != nil {
			t.Fatal(e)
		}
	}
	if !strings.Contains(out.String(), "create path:new.md") || !strings.Contains(out.String(), "skip path:new.md") {
		t.Fatal(out.String())
	}
	if e := os.WriteFile(file, []byte("two"), 0600); e != nil {
		t.Fatal(e)
	}
	if e := env.kb([]string{"import", "fakture", dir}); e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(out.String(), "merge path:new.md") {
		t.Fatal(out.String())
	}
	reader, _, _ := c.env("eva", dir, "")
	if e := reader.kb([]string{"import", "fakture", file, "--class", "structure"}); e == nil || !strings.Contains(e.Error(), "not allowed") {
		t.Fatal(e)
	}
	for _, cmd := range []string{"export", "import"} {
		agent := *env
		agent.agent = true
		if e := agent.kb([]string{cmd, "fakture", dir}); e == nil || e.Error() != agentRefusal {
			t.Fatal(e)
		}
	}
}

func TestMarkdownClientPagedExport(t *testing.T) {
	dir := t.TempDir()
	var slept time.Duration
	var limited bool
	var calls []string
	env := &projectEnv{sleep: func(d time.Duration) { slept += d }, out: os.Stdout, connect: func() (connection, string) { return connection{}, "" }, do: func(_ connection, m, p string, _ map[string]string, _ []byte) (int, []byte, http.Header) {
		calls = append(calls, p)
		if !limited {
			limited = true
			return 429, nil, http.Header{"Retry-After": []string{"1"}}
		}
		switch p {
		case apiProjects + "/demo":
			return 200, []byte(`{"id":1}`), nil
		case apiProjects + "/1/notes?limit=100&after=":
			return 200, []byte(`{"notes":[{"id":1}],"next":"cursor"}`), nil
		case apiProjects + "/1/notes?limit=100&after=cursor":
			return 200, []byte(`{"notes":[{"id":2}]}`), nil
		default:
			id := 1
			if strings.HasSuffix(p, "/2") {
				id = 2
			}
			n := core.Note{ID: int64(id), Title: fmt.Sprint(id), Body: "text", Class: core.Data}
			b, _ := json.Marshal(n)
			return 200, b, nil
		}
	}}
	if e := env.kb([]string{"export", "demo", dir}); e != nil {
		t.Fatal(e)
	}
	if len(calls) != 6 || slept != time.Second {
		t.Fatal(calls)
	}
	if e := env.kb([]string{"export", "demo", dir}); e == nil || !strings.Contains(e.Error(), "empty") {
		t.Fatal(e)
	}
}

func TestMarkdownSharedFixtureAndBundleFlags(t *testing.T) {
	var fixture struct {
		Note           core.Note
		Markdown       string
		OriginNote     core.Note
		OriginMarkdown string
	}
	if e := json.Unmarshal(srcFile(t, "testdata/markdown-copy.json"), &fixture); e != nil {
		t.Fatal(e)
	}
	if string(core.Markdown(fixture.Note, "")) != fixture.Markdown {
		t.Fatalf("%q", core.Markdown(fixture.Note, ""))
	}
	if got := string(core.Markdown(fixture.OriginNote, "")); got != fixture.OriginMarkdown || fixture.OriginNote.Origin != "gw1:1" { // the export names its gateway and project
		t.Fatalf("%q", got)
	}
	_, route, body, e := bundleRequest([]string{"export", "--team", "Team", "--project", "one", "--project=two", "--with-private", "bundle.zip"})
	if e != nil {
		t.Fatal(e)
	}
	export, ok := body.(core.BundleExport)
	if !ok || route != "/export" || len(export.Projects) != 2 || export.Projects[1] != "two" || !export.WithPrivate || !filepath.IsAbs(export.Path) {
		t.Fatal(body, route)
	}
	_, route, body, e = bundleRequest([]string{"import", "bundle.zip", "--source", "remotesrc", "--dry-run", "--map", "ana=Ana", "--map=bor=Bor"})
	if e != nil {
		t.Fatal(e)
	}
	input, ok := body.(core.BundleImport)
	if !ok || route != "/import" || !input.DryRun || input.Source != "remotesrc" || len(input.Map) != 2 || input.Map["ana"] != "Ana" {
		t.Fatal(body, route)
	}
	for _, args := range [][]string{{"export", "x.zip"}, {"export", "--team", "T", "x.zip", "--map", "a=b"}, {"export", "--team", "T", "x.zip", "--source", "s"}, {"import", "x.zip"}, {"import", "x.zip", "--with-private"}, {"import", "x.zip", "--source", "s", "--map", "a=b", "--map", "a=c"}, {"import", "x.zip", "--source", "s", "--map", "bad"}, {"import", "x.zip", "--source", "s", "--project", "one"}} {
		if _, _, _, e := bundleRequest(args); e == nil {
			t.Fatal(args)
		}
	}
}

func TestMarkdownClientImportWaits(t *testing.T) {
	file := filepath.Join(t.TempDir(), "note.md")
	if e := os.WriteFile(file, []byte("body"), 0600); e != nil {
		t.Fatal(e)
	}
	var slept time.Duration
	var calls int
	env := &projectEnv{out: os.Stdout, sleep: func(d time.Duration) { slept += d }, connect: func() (connection, string) { return connection{}, "" },
		do: func(_ connection, m, p string, _ map[string]string, _ []byte) (int, []byte, http.Header) {
			if m == "GET" {
				return 200, []byte(`{"id":1}`), nil
			}
			calls++
			if calls == 1 {
				return 429, nil, http.Header{"Retry-After": []string{"1"}}
			}
			return 200, []byte(`{"action":"create","key":"path:note.md","note":1}`), nil
		}}
	if e := env.kb([]string{"import", "demo", file}); e != nil {
		t.Fatal(e)
	}
	if calls != 2 || slept != time.Second {
		t.Fatal(calls, slept)
	}
}

// A gateway that always answers 429 must end the command after a bounded number of attempts, not
// spin for ever with a sleep the tests replace by a no-op.
func TestMarkdownClient429IsBounded(t *testing.T) {
	file := filepath.Join(t.TempDir(), "note.md")
	if e := os.WriteFile(file, []byte("body"), 0600); e != nil {
		t.Fatal(e)
	}
	var waits []time.Duration
	calls := 0
	env := &projectEnv{out: os.Stdout, sleep: func(d time.Duration) { waits = append(waits, d) }, connect: func() (connection, string) { return connection{}, "" },
		do: func(_ connection, _, _ string, _ map[string]string, _ []byte) (int, []byte, http.Header) {
			calls++
			return 429, nil, http.Header{"Retry-After": []string{"1"}}
		}}
	e := env.kb([]string{"import", "demo", file})
	if e == nil || e.Error() != "the gateway keeps answering 429: try again later" {
		t.Fatal(e)
	}
	if calls != kbRetryAttempts || len(waits) != kbRetryAttempts-1 {
		t.Fatalf("%d calls, %d waits", calls, len(waits))
	}
	for _, d := range waits {
		if d != time.Second {
			t.Fatalf("waits %v", waits)
		}
	}
}

// A file this gateway exported from this project is the note it came from: exporting and importing again is a no-op, and an
// edit of the file updates that note (never a second copy). A file from another project does not claim the note.
func TestKBExportThenImportNeverDuplicates(t *testing.T) {
	c := newClientFx(t)
	s := c.f.s.core
	ana := core.Principal{UserID: c.f.uid["ana"], DeviceID: devID(c.f.dev["ana"])}
	pid := c.f.pid["fakture"]
	n, _, e := s.CreateNote(context.Background(), ana, pid, "k1", core.NoteInput{Class: core.Data, Title: "Deploy", Body: "migrate first"})
	if e != nil {
		t.Fatal(e)
	}
	dir := t.TempDir()
	env, out, _ := c.env("ana", dir, "")
	exp := filepath.Join(dir, "out")
	if e := env.kb([]string{"export", "fakture", exp}); e != nil {
		t.Fatal(e)
	}
	count := func(project int64) int {
		pg, e := s.ListNotes(context.Background(), ana, project, "", "", 100)
		if e != nil {
			t.Fatal(e)
		}
		return len(pg.Notes)
	}
	out.Reset()
	if e := env.kb([]string{"import", "fakture", exp}); e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(out.String(), fmt.Sprintf("skip self:%d", n.ID)) || count(pid) != 1 {
		t.Fatalf("%q, %d notes", out.String(), count(pid))
	}
	file := filepath.Join(exp, fmt.Sprintf("%d.md", n.ID))
	b, _ := os.ReadFile(file)
	if e := os.WriteFile(file, []byte(strings.Replace(string(b), "migrate first", "migrate, then deploy", 1)), 0600); e != nil {
		t.Fatal(e)
	}
	out.Reset()
	if e := env.kb([]string{"import", "fakture", exp}); e != nil {
		t.Fatal(e)
	}
	g, _ := s.GetNote(context.Background(), ana, pid, n.ID)
	if !strings.Contains(out.String(), fmt.Sprintf("merge self:%d", n.ID)) || count(pid) != 1 || g.Body != "migrate, then deploy" {
		t.Fatalf("%q, %d notes, %q", out.String(), count(pid), g.Body)
	}
	// the same file into another project is a new note there, and leaves this one alone
	cene := core.Principal{UserID: c.f.uid["cene"], DeviceID: devID(c.f.dev["cene"])}
	cenv, _, _ := c.env("cene", dir, "")
	if e := cenv.kb([]string{"import", "eureko", exp}); e != nil {
		t.Fatal(e)
	}
	if pg, _ := s.ListNotes(context.Background(), cene, c.f.pid["eureko"], "", "", 100); len(pg.Notes) != 1 {
		t.Fatalf("%d notes in eureko", len(pg.Notes))
	}
	if g2, _ := s.GetNote(context.Background(), ana, pid, n.ID); g2.Revision != g.Revision {
		t.Fatal("the note of another project was changed by a file of this one")
	}
}
