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
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

func updateFixtures() bool { return os.Getenv("MGW_UPDATE_FIXTURES") != "" }

var rootPath = regexp.MustCompile(`\{root\}[^\s"]*`)

// portable: the fixture is written with "/" and the Unix error text; the program prints native separators and the OS's
// own words (Windows: "\" in paths, "The system cannot find the file specified."), which are correct there.
func portable(s string) string {
	s = rootPath.ReplaceAllStringFunc(s, func(m string) string { return strings.ReplaceAll(m, `\`, "/") })
	return strings.ReplaceAll(s, "The system cannot find the file specified.", "no such file or directory")
}

func TestPortableNormalisesWindowsOutput(t *testing.T) {
	got := portable(`linked {root}\proj\src to x; open {root}\nope.txt: The system cannot find the file specified.`)
	if want := `linked {root}/proj/src to x; open {root}/nope.txt: no such file or directory`; got != want {
		t.Fatalf("%q", got)
	}
	if s := `a\b {root}/x`; portable(s) != s {
		t.Fatal("a backslash outside a root path must stay")
	}
}

func readFixture(t *testing.T, name string, v any) {
	t.Helper()
	b := srcFile(t, "testdata/"+name)
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
	want := srcFile(t, "testdata/projects.golden.json") // embedded copy
	if updateFixtures() {
		want = got
	}
	golden = filepath.Join(dir, "golden.json") // load back from a real file: the tree may be absent
	os.WriteFile(golden, want, 0o644)
	if !bytes.Equal(got, want) {
		t.Fatalf("link file differs from %s:\n got %q\nwant %q", golden, got, want)
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
				dirLink(t, filepath.Join(root, to), filepath.Join(root, from)) // a junction on Windows without the symlink privilege
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
		Rewrite []struct {
			Name, Head, Project, Want string
			Reject                    bool // F1: the connection is closed, nothing is passed on
		}
	}
	readFixture(t, "project-header.json", &fx)
	for _, c := range fx.Rewrite {
		got, ok := rewriteHead([]byte(c.Head), c.Project)
		if ok == c.Reject || string(got) != c.Want {
			t.Errorf("%s:\n got %q ok=%v\nwant %q reject=%v", c.Name, got, ok, c.Want, c.Reject)
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
					rel = filepath.ToSlash(rel)
					got.Links = append(got.Links, l.Slug+":"+strconv.FormatInt(l.Project, 10)+"@"+rel)
				}
			}
			unsub := func(s string) string { return portable(strings.ReplaceAll(s, root, "{root}")) }
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

// R4: the 64 KiB limit is on the head's length (before CRLFCRLF), not on how much has arrived: rows of the shared fixture.
func TestProjectParityHeadLimit(t *testing.T) {
	var fx struct {
		Limit []struct {
			Name    string
			HeadLen int
			Cut     int
			Accept  bool
		}
	}
	readFixture(t, "project-header.json", &fx)
	if len(fx.Limit) < 6 {
		t.Fatal("rows missing")
	}
	for _, c := range fx.Limit {
		pre := "GET / HTTP/1.1\r\nX-Pad: "
		req := []byte(pre + strings.Repeat("a", c.HeadLen-len(pre)) + "\r\n\r\n")
		h := &headRewriter{project: "7"}
		parts := [][]byte{req}
		if c.Cut > 0 {
			parts = [][]byte{req[:c.Cut], req[c.Cut:]}
		}
		ok := true
		var sent []byte
		for _, p := range parts {
			out, k := h.feed(p)
			ok = ok && k
			sent = append(sent, out...)
		}
		if ok != c.Accept || ok && !bytes.HasPrefix(sent, []byte("GET / HTTP/1.1\r\nX-Minigw-Project: 7\r\n")) {
			t.Errorf("%s: accepted=%v want %v", c.Name, ok, c.Accept)
		}
	}
}
