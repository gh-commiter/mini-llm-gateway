package main

// Parity of the project client with the Mac client (Swift, Sources/RouterCore/Project*.swift): the same fixtures in
// client/testdata/project-*.json are asserted here and in Tests/RouterCoreTests/ProjectTests.swift. A change of
// behaviour changes a fixture, and then both clients must agree again. MGW_UPDATE_FIXTURES=1 rewrites the expected
// values (want, projects.golden.json) from this implementation: review the diff before committing it.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func updateFixtures() bool { return os.Getenv("MGW_UPDATE_FIXTURES") != "" }

func readFixture(t *testing.T, name string, v any) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(name, err)
	}
}

func writeFixture(t *testing.T, name string, v any) {
	t.Helper()
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", " ")
	if err := enc.Encode(v); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join("testdata", name), b.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestProjectParityState(t *testing.T) {
	var fx struct {
		GoldenLinks []projectLink `json:"golden_links"`
		LoadOK      []struct {
			Name  string
			Text  string
			Links int
		} `json:"load_ok"`
		LoadErr []struct{ Name, Text string } `json:"load_err"`
	}
	readFixture(t, "project-state.json", &fx)
	dir := t.TempDir()
	p := filepath.Join(dir, "projects.json")
	if err := saveProjectState(p, projectState{Links: fx.GoldenLinks}); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(p)
	golden := filepath.Join("testdata", "projects.golden.json")
	if updateFixtures() {
		os.WriteFile(golden, got, 0o644)
	}
	want, err := os.ReadFile(golden)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("link file differs from %s:\n got %q\nwant %q (%v)", golden, got, want, err)
	}
	if st, err := loadProjectState(golden); err != nil || len(st.Links) != 2 || st.Links[1] != fx.GoldenLinks[1] || st.Version != 1 {
		t.Fatalf("golden does not load back: %+v %v", st, err)
	}
	empty := filepath.Join(dir, "empty.json")
	saveProjectState(empty, projectState{})
	if b, _ := os.ReadFile(empty); string(b) != "{\n  \"version\": 1,\n  \"links\": null\n}\n" {
		t.Fatalf("empty state: %q", b)
	}
	for _, c := range fx.LoadOK {
		os.WriteFile(p, []byte(c.Text), 0o600)
		if st, err := loadProjectState(p); err != nil || len(st.Links) != c.Links {
			t.Errorf("%s: %+v %v", c.Name, st, err)
		}
	}
	for _, c := range fx.LoadErr {
		os.WriteFile(p, []byte(c.Text), 0o600)
		if _, err := loadProjectState(p); err == nil || !strings.HasPrefix(err.Error(), "cannot read "+p+": ") {
			t.Errorf("%s: error %v", c.Name, err)
		}
	}
}

type resCase struct {
	Name     string
	Cwd      string
	Dirs     []string
	Git      []string
	Hints    map[string]string
	Links    []projectLink
	Gateway  string
	Agent    bool
	Symlinks map[string]string
	Want     struct {
		State, Slug, Hint, Header string
		Error                     bool
	}
}

func TestProjectParityResolutionAndHeader(t *testing.T) {
	var fx struct {
		Gateway    string
		Resolution []resCase
	}
	readFixture(t, "project-header.json", &fx)
	for _, c := range fx.Resolution {
		t.Run(c.Name, func(t *testing.T) {
			root := folder(t, t.TempDir(), "root")
			for _, d := range c.Dirs {
				folder(t, root, d)
			}
			for _, d := range c.Git {
				folder(t, root, d, ".git")
			}
			for d, h := range c.Hints {
				os.WriteFile(filepath.Join(root, d, hintFile), []byte(h), 0o644)
			}
			for from, to := range c.Symlinks {
				if err := os.Symlink(filepath.Join(root, to), filepath.Join(root, from)); err != nil {
					t.Fatal(err)
				}
			}
			st := projectState{}
			for _, l := range c.Links {
				l.Path = filepath.Join(root, l.Path)
				st.Links = append(st.Links, l)
			}
			statePath := filepath.Join(t.TempDir(), "projects.json")
			if err := saveProjectState(statePath, st); err != nil {
				t.Fatal(err)
			}
			if c.Agent {
				tok := filepath.Join(t.TempDir(), "tok")
				os.WriteFile(tok, []byte("run-token\n"), 0o600)
				t.Setenv("MINIGW_AGENT_URL", "http://127.0.0.1:9")
				t.Setenv("MINIGW_AGENT_AUTH_FILE", tok)
			}
			cwd := filepath.Join(root, c.Cwd)
			res, err := projectFor(st, c.Gateway, cwd)
			if err != nil {
				t.Fatal(err)
			}
			state := map[projectState3]string{noLink: "none", linkOK: "ok", linkHintDiff: "hintDiff"}[res.State]
			slug := ""
			if res.State != noLink {
				slug = res.Link.Slug
			}
			if state != c.Want.State || slug != c.Want.Slug || res.Hint != c.Want.Hint {
				t.Fatalf("state %s slug %q hint %q, want %+v", state, slug, res.Hint, c.Want)
			}
			key := c.Gateway
			header, herr := projectHeaderFor(cwd, connection{Key: &key}, statePath)
			if header != c.Want.Header || (herr != nil) != c.Want.Error {
				t.Fatalf("header %q err %v, want %+v", header, herr, c.Want)
			}
		})
	}
}

func TestProjectParityRewriteHead(t *testing.T) {
	var fx struct {
		Rewrite []struct{ Name, Head, Project, Want string }
	}
	readFixture(t, "project-header.json", &fx)
	for _, c := range fx.Rewrite {
		if got := string(rewriteHead([]byte(c.Head), c.Project)); got != c.Want {
			t.Errorf("%s:\n got %q\nwant %q", c.Name, got, c.Want)
		}
	}
}

type cmdResp struct {
	Match   string            `json:"match"`
	Status  int               `json:"status"`
	Body    string            `json:"body"`
	Headers map[string]string `json:"headers,omitempty"`
}

type cmdCase struct {
	Name      string            `json:"name"`
	Args      []string          `json:"args"`
	Stdin     string            `json:"stdin,omitempty"`
	Link      *cmdLink          `json:"link,omitempty"`
	Hint      string            `json:"hint,omitempty"`
	Agent     string            `json:"agent,omitempty"` // "refuse": inside an agent run without a run port; "run": with it
	Cwd       string            `json:"cwd,omitempty"`
	Files     map[string]string `json:"files,omitempty"`
	Responses []cmdResp         `json:"responses"`
	Want      *cmdWant          `json:"want,omitempty"`
}

type cmdLink struct {
	Slug    string `json:"slug"`
	Project int64  `json:"project"`
	Rel     string `json:"rel"`
}

type cmdWant struct {
	Out      string     `json:"out"`
	Err      string     `json:"err"`
	Error    string     `json:"error"`
	Requests [][]string `json:"requests"` // method, path, body, Idempotency-Key
	Links    []string   `json:"links"`    // slug:project@folder after the command
}

func TestProjectParityCommands(t *testing.T) {
	var fx struct {
		Gateway string     `json:"gateway"`
		Cases   []*cmdCase `json:"cases"`
	}
	readFixture(t, "project-commands.json", &fx)
	saved := time.Local
	time.Local = time.UTC // note times are printed in local time; the fixture is in UTC
	defer func() { time.Local = saved }()
	for _, c := range fx.Cases {
		t.Run(c.Name, func(t *testing.T) {
			root := folder(t, t.TempDir(), "root")
			sub := func(s string) string { return strings.ReplaceAll(s, "{root}", root) }
			for _, d := range []string{"proj", "proj/src"} {
				folder(t, root, d)
			}
			for f, body := range c.Files {
				os.WriteFile(filepath.Join(root, f), []byte(body), 0o644)
			}
			statePath := filepath.Join(t.TempDir(), "projects.json")
			if c.Link != nil {
				saveProjectState(statePath, projectState{Links: []projectLink{{Gateway: fx.Gateway, Project: c.Link.Project, Slug: c.Link.Slug, Path: filepath.Join(root, c.Link.Rel)}}})
			}
			if c.Hint != "" {
				os.WriteFile(filepath.Join(root, "proj", hintFile), []byte(c.Hint+"\n"), 0o644)
			}
			cwd := "proj"
			if c.Cwd != "" {
				cwd = c.Cwd
			}
			var out, errw bytes.Buffer
			conn := connection{URL: "http://gateway.test", Token: "tok"}
			var reqs [][]string
			e := &projectEnv{
				connect: func() (connection, string) { return conn, fx.Gateway },
				state:   statePath, cwd: filepath.Join(root, cwd), in: strings.NewReader(sub(c.Stdin)), out: &out, err: &errw,
				agent: c.Agent != "", sleep: func(time.Duration) {},
			}
			if c.Agent == "run" {
				e.runConn = &conn
			}
			e.do = func(_ connection, method, path string, h map[string]string, body []byte) (int, []byte, http.Header) {
				reqs = append(reqs, []string{method, path, string(body), h["Idempotency-Key"]})
				for _, r := range c.Responses {
					if r.Match == method+" "+path {
						hd := http.Header{}
						for k, v := range r.Headers {
							hd.Set(k, v)
						}
						return r.Status, []byte(r.Body), hd
					}
				}
				return 599, []byte("no such response in the fixture"), http.Header{}
			}
			args := make([]string, len(c.Args))
			for i, a := range c.Args {
				args[i] = sub(a)
			}
			var err error
			switch args[0] {
			case "note":
				err = e.note(args[1:])
			case "usage":
				err = e.usage(args[1:])
			default:
				err = e.project(args[1:])
			}
			got := &cmdWant{Out: out.String(), Err: errw.String(), Requests: reqs}
			if err != nil {
				got.Error = err.Error()
			}
			if st, _ := loadProjectState(statePath); true {
				for _, l := range st.Links {
					rel, _ := filepath.Rel(root, l.Path)
					got.Links = append(got.Links, l.Slug+":"+strconv.FormatInt(l.Project, 10)+"@"+rel)
				}
			}
			unsub := func(s string) string { return strings.ReplaceAll(s, root, "{root}") }
			got.Out, got.Err, got.Error = unsub(got.Out), unsub(got.Err), unsub(got.Error)
			for i := range got.Requests {
				got.Requests[i][2] = unsub(got.Requests[i][2])
			}
			if got.Requests == nil {
				got.Requests = [][]string{}
			}
			if got.Links == nil {
				got.Links = []string{}
			}
			if updateFixtures() {
				c.Want = got
				return
			}
			if c.Want == nil {
				t.Fatal("no expected values in the fixture")
			}
			gj, _ := json.Marshal(got)
			wj, _ := json.Marshal(c.Want)
			if !bytes.Equal(gj, wj) {
				t.Fatalf("\n got %s\nwant %s", gj, wj)
			}
		})
	}
	if updateFixtures() {
		writeFixture(t, "project-commands.json", fx)
	}
}
