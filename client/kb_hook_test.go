package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

type hookWorld struct {
	root, cfg, work string
	e               *projectEnv
	out, errOut     *bytes.Buffer
	calls           int
}

// newHookWorld: a temp config folder (like <UserConfigDir>/minigw) and a linked project folder; never the real ones.
func newHookWorld(t *testing.T, stdin string) *hookWorld {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	w := &hookWorld{root: root, cfg: filepath.Join(root, "cfg", "minigw"), work: filepath.Join(root, "work"), out: &bytes.Buffer{}, errOut: &bytes.Buffer{}}
	os.MkdirAll(w.work, 0o700)
	w.e = &projectEnv{
		state: filepath.Join(w.cfg, "projects.json"), cwd: w.work, in: strings.NewReader(stdin), out: w.out, err: w.errOut,
		gateway: func() string { return "gw1" },
		profile: func() string { return "" },
		now:     func() time.Time { return time.Unix(1000000, 0) },
		do: func(connection, string, string, map[string]string, []byte) (int, []byte, http.Header) {
			w.calls++
			t.Error("a hook must not call the gateway")
			return 599, nil, nil
		},
	}
	w.e.hookProfile = func(name string) (string, string) { // only the default profile, the run's own gateway
		if name == "" {
			return w.e.gateway(), ""
		}
		return "", ""
	}
	return w
}

func (w *hookWorld) link(t *testing.T, gw string, project int64, slug string) {
	t.Helper()
	st, _ := loadProjectState(w.e.state)
	st.Links = append(st.Links, projectLink{Gateway: gw, Project: project, Slug: slug, Path: w.work})
	if err := saveProjectState(w.e.state, st); err != nil {
		t.Fatal(err)
	}
}

func (w *hookWorld) write(t *testing.T, rel string, v any) {
	t.Helper()
	p := filepath.Join(w.cfg, rel)
	os.MkdirAll(filepath.Dir(p), 0o700)
	b, _ := json.Marshal(v)
	if err := os.WriteFile(p, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestKBHookFixture(t *testing.T) {
	var f struct {
		Gateway  string
		Project  int64
		Slug     string
		Now      int64
		Profiles []struct{ Name, Gateway, Arg string }
		Cases    []struct {
			Name, Event string
			Hint        string
			LinkSlug    string
			Stdin       map[string]any
			StdinRaw    *string
			StdinPad    int
			Enabled     bool
			Profile     string
			Entries     []hookEnabled
			Links       []projectLink
			Linked      bool
			Marker      *hookMarker
			Submitted   int64
			Git         struct{ Head, Status string }
			Agent       bool
			Want        struct {
				Stdout  string
				Marker  *hookMarker
				Session string
			}
		}
	}
	if err := json.Unmarshal(srcFile(t, "testdata/kb-hook.json"), &f); err != nil {
		t.Fatal(err)
	}
	for _, c := range f.Cases {
		t.Run(c.Name, func(t *testing.T) {
			w := newHookWorld(t, "")
			w.e.now = func() time.Time { return time.Unix(f.Now, 0) }
			w.e.gitState = func(dir string) (string, string) {
				if dir != w.work {
					t.Errorf("git asked in %s, not in the linked folder %s", dir, w.work)
				}
				return c.Git.Head, c.Git.Status
			}
			w.e.agent = c.Agent
			w.e.hookProfile = func(name string) (string, string) {
				for _, p := range f.Profiles {
					if p.Name == name {
						return p.Gateway, p.Arg
					}
				}
				return "", ""
			}
			for _, l := range c.Links {
				w.link(t, l.Gateway, l.Project, l.Slug)
			}
			if c.Linked {
				slug := f.Slug
				if c.LinkSlug != "" {
					slug = c.LinkSlug
				}
				w.link(t, f.Gateway, f.Project, slug)
			}
			if c.Hint != "" {
				os.WriteFile(filepath.Join(w.work, hintFile), []byte(c.Hint+"\n"), 0o644)
			}
			if c.Enabled {
				entries := c.Entries
				if entries == nil {
					entries = []hookEnabled{{f.Gateway, f.Project, c.Profile}}
				}
				w.write(t, "kb-hooks.json", hookSettings{Version: 1, Enabled: entries})
			}
			session := c.Want.Session
			if session == "" {
				session = "s1"
			}
			if c.Marker != nil {
				w.write(t, "kb-hook/"+session+".json", c.Marker)
			}
			if c.Submitted != 0 {
				w.write(t, "kb-submitted.json", hookSubmitted{1, []hookSubmit{{f.Gateway, f.Project, c.Submitted}}})
			}
			in := ""
			switch {
			case c.StdinRaw != nil:
				in = *c.StdinRaw
			default:
				if c.StdinPad > 0 {
					c.Stdin["pad"] = strings.Repeat("x", c.StdinPad)
				}
				c.Stdin["cwd"] = strings.ReplaceAll(c.Stdin["cwd"].(string), "$CWD", w.work)
				b, _ := json.Marshal(c.Stdin)
				in = string(b)
			}
			w.e.in = strings.NewReader(in)
			before := time.Now()
			if err := w.e.kb([]string{"hook", c.Event}); err != nil {
				t.Fatal(err)
			}
			if time.Since(before) > 2*time.Second {
				t.Error("a hook takes more than 2 s")
			}
			if w.out.String() != c.Want.Stdout {
				t.Errorf("stdout\n got %q\nwant %q", w.out.String(), c.Want.Stdout)
			}
			if w.errOut.Len() != 0 {
				t.Errorf("stderr %q", w.errOut.String())
			}
			var got *hookMarker
			if b, err := os.ReadFile(filepath.Join(w.cfg, "kb-hook", session+".json")); err == nil {
				got = &hookMarker{}
				if json.Unmarshal(b, got) != nil {
					t.Fatalf("marker is not JSON: %s", b)
				}
			}
			if !reflect.DeepEqual(got, c.Want.Marker) {
				t.Errorf("marker got %+v want %+v", got, c.Want.Marker)
			}
			// nothing but the state files, markers inside kb-hook/ and the project folder (which stays empty)
			filepath.Walk(w.root, func(p string, fi os.FileInfo, err error) error {
				rel, _ := filepath.Rel(w.root, p)
				rel = filepath.ToSlash(rel)
				if fi.IsDir() || rel == "cfg/minigw/projects.json" || rel == "cfg/minigw/kb-hooks.json" || rel == "cfg/minigw/kb-submitted.json" || rel == "work/"+hintFile ||
					strings.HasPrefix(rel, "cfg/minigw/kb-hook/") && !strings.Contains(strings.TrimPrefix(rel, "cfg/minigw/kb-hook/"), "/") {
					return nil
				}
				t.Errorf("unexpected file %s", rel)
				return nil
			})
			if fi, err := os.Stat(filepath.Join(w.cfg, "kb-hook", session+".json")); err == nil && runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
				t.Errorf("marker mode %v", fi.Mode().Perm())
			}
		})
	}
}

func TestKBHookPrune(t *testing.T) {
	w := newHookWorld(t, "")
	w.link(t, "gw1", 7, "acme-shop")
	w.write(t, "kb-hooks.json", hookSettings{1, []hookEnabled{{"gw1", 7, ""}}})
	w.e.gitState = func(string) (string, string) { return "h", "s" }
	const day, now = 24 * 3600, 5000000
	w.e.now = func() time.Time { return time.Unix(now, 0) }
	w.write(t, "kb-hook/old.json", hookMarker{Gateway: "gw1", Project: 7, Started: now - 15*day})
	w.write(t, "kb-hook/fresh.json", hookMarker{Gateway: "gw1", Project: 7, Started: now - day})
	os.WriteFile(filepath.Join(w.cfg, "kb-hook", "junk.json"), []byte("{"), 0o600)
	for i := 0; i < 230; i++ {
		w.write(t, fmt.Sprintf("kb-hook/m%03d.json", i), hookMarker{Gateway: "gw1", Project: 7, Started: 4000000 + int64(i)})
	}
	w.e.in = strings.NewReader(hookStdin("new", w.work))
	w.e.kb([]string{"hook", "start"})
	entries, _ := os.ReadDir(filepath.Join(w.cfg, "kb-hook"))
	names := map[string]bool{}
	for _, d := range entries {
		names[d.Name()] = true
	}
	if names["old.json"] || names["junk.json"] || !names["fresh.json"] || !names["new.json"] {
		t.Fatalf("prune: %v", names)
	}
	if len(entries) != hookKeepMax { // 200 markers, the new one among them
		t.Fatalf("%d files", len(entries))
	}
	if names["m000.json"] || names["m031.json"] || !names["m032.json"] || !names["m229.json"] { // the oldest go first
		t.Fatalf("the wrong ones were removed: %v", names)
	}
}

func TestKBHooksOnOffStatus(t *testing.T) {
	w := newHookWorld(t, "")
	run := func(args ...string) (string, error) {
		w.out.Reset()
		err := w.e.kb(append([]string{"hooks"}, args...))
		return w.out.String(), err
	}
	if _, err := run("status"); err == nil || !strings.HasPrefix(err.Error(), "this folder is not linked to a project") {
		t.Fatal(err)
	}
	if _, err := run("on"); err == nil || !strings.HasPrefix(err.Error(), "this folder is not linked to a project") {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(w.cfg, "kb-hooks.json")); err == nil {
		t.Fatal("refused, but the file exists")
	}
	if _, err := run("on", "acme-shop"); err == nil || !strings.Contains(err.Error(), "acme-shop") {
		t.Fatal(err)
	}
	w.link(t, "gw1", 7, "acme-shop")
	w.link(t, "gw1", 8, "web")
	st, _ := loadProjectState(w.e.state)
	st.Links[1].Path = filepath.Join(w.root, "elsewhere")
	saveProjectState(w.e.state, st)
	want := func(args []string, out string) {
		t.Helper()
		if got, err := run(args...); err != nil || got != out {
			t.Fatalf("%v: %q %v", args, got, err)
		}
	}
	want([]string{"status"}, "kb hooks: off for project acme-shop\n")
	snapshot := dirDigest(t, w.work)
	want([]string{"on"}, "kb hooks on for project acme-shop\n")
	want([]string{"on"}, "kb hooks on for project acme-shop\n")
	want([]string{"status"}, "kb hooks: on for project acme-shop\n")
	want([]string{"status", "web"}, "kb hooks: off for project web\n") // one project does not switch another
	want([]string{"on", "web"}, "kb hooks on for project web\n")
	want([]string{"off", "acme-shop"}, "kb hooks off for project acme-shop\n")
	want([]string{"status", "web"}, "kb hooks: on for project web\n")
	want([]string{"status"}, "kb hooks: off for project acme-shop\n")
	set := loadHookSettings(filepath.Join(w.cfg, "kb-hooks.json"))
	if !reflect.DeepEqual(set, hookSettings{1, []hookEnabled{{"gw1", 8, ""}}}) {
		t.Fatalf("%+v", set)
	}
	if fi, _ := os.Stat(filepath.Join(w.cfg, "kb-hooks.json")); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
	if dirDigest(t, w.work) != snapshot {
		t.Fatal("the linked folder changed")
	}
	if w.calls != 0 {
		t.Fatal("network call")
	}
	for _, bad := range [][]string{{}, {"maybe"}, {"on", "a", "b"}} {
		if _, err := run(bad...); err == nil || !strings.HasPrefix(err.Error(), "usage:") {
			t.Fatalf("%v: %v", bad, err)
		}
	}
	w.e.gateway = func() string { return "" }
	var shared struct{ NotConnected string }
	json.Unmarshal(srcFile(t, "testdata/kb-hook.json"), &shared)
	if _, err := run("status"); err == nil || err.Error() != shared.NotConnected || shared.NotConnected == "" {
		t.Fatalf("%v, want %q", err, shared.NotConnected)
	}
	w.e.gateway = func() string { return "gw1" }
	w.e.agent = true
	if _, err := run("status"); err == nil || err.Error() != agentRefusal {
		t.Fatal(err)
	}
}

// dirDigest: names, sizes and contents of a folder tree.
func dirDigest(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		b.WriteString(p + "\n")
		if !fi.IsDir() {
			c, _ := os.ReadFile(p)
			b.Write(c)
		}
		return nil
	})
	return b.String()
}

func TestKBSubmitRecordsOnlyA201(t *testing.T) {
	for _, tc := range []struct {
		status int
		record bool
	}{{201, true}, {400, false}, {403, false}, {413, false}, {0, false}} {
		w := newHookWorld(t, "text\n")
		w.e.connect = func() (connection, string) { return connection{}, "gw1" }
		w.e.sleep = func(time.Duration) {}
		w.e.do = func(_ connection, method, path string, _ map[string]string, _ []byte) (int, []byte, http.Header) {
			if method == "GET" {
				return 200, []byte(`{"id":7}`), nil
			}
			return tc.status, []byte(`{"id":1,"project":7,"state":"pending","class":"data","title":"t"}`), nil
		}
		w.e.kb([]string{"submit", "acme-shop", "--title", "t", "-"})
		b, err := os.ReadFile(filepath.Join(w.cfg, "kb-submitted.json"))
		if tc.record != (err == nil) {
			t.Fatalf("status %d: record=%v err=%v", tc.status, tc.record, err)
		}
		if tc.record {
			var s hookSubmitted
			json.Unmarshal(b, &s)
			if !reflect.DeepEqual(s, hookSubmitted{1, []hookSubmit{{"gw1", 7, 1000000}}}) {
				t.Fatalf("%+v", s)
			}
			if fi, _ := os.Stat(filepath.Join(w.cfg, "kb-submitted.json")); runtime.GOOS != "windows" && fi.Mode().Perm() != 0o600 {
				t.Fatalf("mode %v", fi.Mode().Perm())
			}
		}
	}
}

func hookRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitIn(t, dir, "init", "-q")
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644)
	gitIn(t, dir, "add", "a.txt")
	gitIn(t, dir, "commit", "-qm", "one")
	return dir
}

func TestHookGitState(t *testing.T) {
	dir := hookRepo(t)
	head, status := hookGitStateWithin(dir, time.Minute)
	want := strings.TrimSpace(gitIn(t, dir, "rev-parse", "HEAD"))
	if head != want || len(status) != 64 {
		t.Fatalf("%q %q", head, status)
	}
	if h, s := hookGitStateWithin(dir, time.Minute); h != head || s != status {
		t.Fatal("not stable")
	}
	os.WriteFile(filepath.Join(dir, "b.txt"), []byte("b"), 0o644)
	if h, s := hookGitStateWithin(dir, time.Minute); h != head || s == status {
		t.Fatalf("an untracked file: %q %q", h, s)
	}
	gitIn(t, dir, "add", "b.txt")
	gitIn(t, dir, "commit", "-qm", "two")
	if h, _ := hookGitStateWithin(dir, time.Minute); h == head {
		t.Fatal("a commit does not change head")
	}
	if h, s := hookGitStateWithin(t.TempDir(), time.Minute); h != "" || s != "" {
		t.Fatalf("not a repository: %q %q", h, s)
	}
}

// A checkout whose .git/config names a core.fsmonitor program must not get it run (gitGuardEnv, core.fsmonitor=false).
func TestHookGitStateDoesNotRunTheCheckoutsCode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fsmonitor script is a POSIX shell script")
	}
	dir := hookRepo(t)
	proof := filepath.Join(t.TempDir(), "ran")
	script := filepath.Join(t.TempDir(), "fsmonitor.sh")
	os.WriteFile(script, []byte("#!/bin/sh\necho ran >> "+proof+"\nprintf '\\0'\n"), 0o755)
	gitIn(t, dir, "config", "core.fsmonitor", script)
	// positive control: plain git does run it, so the check below proves the guard and not a git that never calls it
	gitIn(t, dir, "status")
	if _, err := os.Stat(proof); err != nil {
		t.Fatal("the control did not run the fsmonitor script: the test proves nothing")
	}
	os.Remove(proof)
	hookGitStateWithin(dir, time.Minute)
	if _, err := os.Stat(proof); err == nil {
		t.Fatal("the checkout's core.fsmonitor program ran")
	}
}

// M1: git status and diff-files run the checkout's content filters (info/attributes + filter.<x>.clean) on a changed file; the
// hook's git state must not.
func TestHookGitStateDoesNotRunContentFilters(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the filter is a POSIX shell command")
	}
	dir := hookRepo(t)
	proof := filepath.Join(t.TempDir(), "ran")
	os.WriteFile(filepath.Join(dir, ".git", "info", "attributes"), []byte("* filter=x\n"), 0o644)
	gitIn(t, dir, "config", "filter.x.clean", "touch '"+proof+"'; cat")
	later := time.Now().Add(time.Hour) // a tracked file whose stat differs from the index, content the same: git compares content, through the filter
	os.Chtimes(filepath.Join(dir, "a.txt"), later, later)
	gitIn(t, dir, "status") // positive control: plain status runs the filter
	if _, err := os.Stat(proof); err != nil {
		t.Fatal("the control did not run the filter: the test proves nothing")
	}
	os.Rename(proof, proof+".1")
	_, before := hookGitStateWithin(dir, time.Minute)
	if _, err := os.Stat(proof); err == nil {
		t.Fatal("the checkout's clean filter ran")
	}
	os.WriteFile(filepath.Join(dir, "a.txt"), []byte("changed again, longer"), 0o644)
	if _, after := hookGitStateWithin(dir, time.Minute); after == before || len(after) != 64 {
		t.Fatal("an edit of a tracked file is not seen")
	}
	gitIn(t, dir, "add", "a.txt") // staged edits are seen too
	if _, staged := hookGitStateWithin(dir, time.Minute); staged == before {
		t.Fatal("a staged file is not seen")
	}
}

// L3: with no kb-hooks.json nothing but that file is read: no profile, no projects.json.
func TestKBHookReadsNothingWhenNotEnabled(t *testing.T) {
	for _, settings := range []*hookSettings{nil, {Version: 1}} {
		w := newHookWorld(t, "")
		w.link(t, "gw1", 7, "acme-shop")
		if settings != nil {
			w.write(t, "kb-hooks.json", settings)
		}
		asked := 0
		w.e.gateway = func() string { asked++; return "gw1" }
		w.e.in = strings.NewReader(hookStdin("s1", w.work))
		if err := w.e.kb([]string{"hook", "start"}); err != nil || asked != 0 || w.out.Len() != 0 {
			t.Fatalf("settings %v: asked %d, out %q, err %v", settings, asked, w.out.String(), err)
		}
	}
}

// M2: the command in the hand-in request, parsed and run through the real argument parser with the text on stdin.
// #185: through another profile the commands carry --minigw-profile after their words and before the final - (main takes the flag
// out wherever it is, TestHookProfileFlagAfterTheCommand); here it is taken out the same way.
func TestHookRequestCommandsWork(t *testing.T) {
	for _, profile := range []string{"", "p2"} {
		text := hookText(hookRequestText, "acme-shop", profile)
		cmds := regexp.MustCompile("`(minigw kb submit [^`]*)`").FindAllStringSubmatch(text, -1)
		if len(cmds) != 2 {
			t.Fatalf("%d commands in the request", len(cmds))
		}
		for i, kind := range []string{"finding", "faq"} {
			var args []string
			for _, m := range regexp.MustCompile(`"([^"]*)"|(\S+)`).FindAllStringSubmatch(cmds[i][1], -1) {
				args = append(args, m[1]+m[2])
			}
			args = slices.Replace(args, 0, 1) // minigw
			if args[0] != "kb" || !strings.HasPrefix(cmds[i][1], "minigw kb submit acme-shop --kind "+kind) || args[len(args)-1] != "-" {
				t.Fatal(args)
			}
			if profile != "" {
				if !slices.Equal(args[len(args)-3:len(args)-1], []string{"--minigw-profile", profile}) {
					t.Fatalf("the profile is not before the final -: %v", args)
				}
				args = slices.Delete(args, len(args)-3, len(args)-1)
			}
			w := newHookWorld(t, "the text on stdin\n")
			w.e.connect = func() (connection, string) { return connection{}, "gw1" }
			w.e.sleep = func(time.Duration) {}
			var sent map[string]any
			w.e.do = func(_ connection, method, path string, h map[string]string, b []byte) (int, []byte, http.Header) {
				if method == "GET" {
					return 200, []byte(`{"id":7}`), nil
				}
				h["Idempotency-Key"] = ""
				json.Unmarshal(b, &sent)
				return 201, []byte(`{"id":1,"project":7,"state":"pending","class":"data","title":"t"}`), nil
			}
			if err := w.e.kb(args[1:]); err != nil {
				t.Fatalf("%v: %v", args, err)
			}
			if sent["entryKind"] != kind || sent["body"] != "the text on stdin\n" || sent["title"] == "" || w.out.String() != "submission 1 pending\n" {
				t.Fatalf("%v: %v %q", args, sent, w.out.String())
			}
		}
	}
}

// The request puts nothing taken from the repository on a command line: no title placeholder in double quotes (the shell would
// expand $(...) and backticks in it), in fact no --title at all; the title is the first line of the text on stdin.
func TestHookRequestHasNoShellExpandablePlaceholder(t *testing.T) {
	text := strings.ReplaceAll(hookRequestText, "<slug>", "acme-shop")
	for _, bad := range []string{`"<`, `--title`, `"$`} {
		for _, c := range regexp.MustCompile("`(minigw kb submit [^`]*)`").FindAllStringSubmatch(text, -1) {
			if strings.Contains(c[1], bad) {
				t.Fatalf("%q in %q", bad, c[1])
			}
		}
	}
	if strings.Contains(text, `"<`) {
		t.Fatal("double-quoted placeholder in the request")
	}
}

// hookStdin: the hook's JSON with the folder encoded properly (a Windows path has backslashes; pasted into a JSON
// string unescaped it is invalid JSON and the hook rightly ignores it).
func hookStdin(session, cwd string) string {
	b, _ := json.Marshal(map[string]string{"session_id": session, "cwd": cwd})
	return string(b)
}

// #185: kb hooks on records the run's profile; on through another profile (to the same gateway) replaces the entry and says so;
// status names a profile that is not the default one. The steps and the file are shared with the Swift client (testdata/kb-hook.json).
func TestKBHooksOnThroughAnotherProfile(t *testing.T) {
	var f struct {
		OnSequence []struct {
			Profile string
			Args    []string
			Want    string
		}
		OnFile string
	}
	if err := json.Unmarshal(srcFile(t, "testdata/kb-hook.json"), &f); err != nil || len(f.OnSequence) == 0 {
		t.Fatal(err)
	}
	w := newHookWorld(t, "")
	w.link(t, "gw1", 7, "acme-shop")
	for i, s := range f.OnSequence {
		w.e.profile = func() string { return s.Profile }
		w.out.Reset()
		if err := w.e.kb(append([]string{"hooks"}, s.Args...)); err != nil || w.out.String() != s.Want+"\n" {
			t.Fatalf("step %d %v as %q: %q %v", i, s.Args, s.Profile, w.out.String(), err)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(w.cfg, "kb-hooks.json")); string(b) != f.OnFile {
		t.Fatalf("kb-hooks.json:\n%s", b)
	}
}
