package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"minigw/core"
	"net/http"
	"reflect"
	"regexp"
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
		Review []struct {
			Args              []string
			Out, Error, Input string
			TTY               bool
			Responses         []cmdResp
			Requests          [][]string
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
	for _, c := range f.Review { // a project admin's review: no idempotency key
		var out bytes.Buffer
		var requests [][]string
		tty := c.TTY
		env := &projectEnv{out: &out, in: strings.NewReader(c.Input), tty: func() bool { return tty }, connect: func() (connection, string) { return connection{}, "" }, do: func(_ connection, method, path string, _ map[string]string, b []byte) (int, []byte, http.Header) {
			requests = append(requests, []string{method, path, string(b)})
			for _, r := range c.Responses {
				if r.Match == method+" "+path {
					return r.Status, []byte(r.Body), nil
				}
			}
			t.Fatalf("unexpected %s %s", method, path)
			return 599, nil, nil
		}}
		if e := env.kb(c.Args); c.Error == "" && e != nil || c.Error != "" && (e == nil || e.Error() != c.Error) {
			t.Fatalf("%v: %v, want %q", c.Args, e, c.Error)
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
	review := stdout(t, func() { kbAdmin(owner, []string{"kb", "review"}, coreOpts{}) })
	if !strings.Contains(review, "Reviewed knowledge") {
		t.Fatal(review)
	}
	accepted := stdout(t, func() { kbAdmin(owner, []string{"kb", "accept", "1"}, coreOpts{version: versionIn(t, review)}) })
	if !strings.HasSuffix(accepted, "Reviewed knowledge\nsubmission 1 accepted\n") { // the accept shows what it publishes first
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
	if err = e.kb([]string{"search", "--team", "acme", "migrate"}); err != nil || !strings.Contains(o.String(), "Deploy order  team acme") || strings.Contains(o.String(), "from") {
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
	// No listing of the command line shows the team's hidden project row (its id or slug).
	for _, args := range [][]string{{"project", "list"}, {"project", "list", "--archived"}, {"project", "list", "--json"}, {"project", "list", "--path", "acme"}, {"team", "show", "acme"}} {
		var listing string
		var lerr error
		if args[0] == "team" {
			e, o, _ := c.env("fani", dir, "")
			lerr = e.team(args[1:])
			listing = o.String()
		} else {
			listing, lerr = c.run("fani", dir, "", args...)
		}
		if strings.Contains(listing, "@team") || strings.Contains(listing, "1099511627") {
			t.Fatalf("%v shows a segment: %v %s", args, lerr, listing)
		}
	}
}

// No line-producing function of the Go client prints the hidden id of a team's own notes as a project: every output
// below is made from a gateway answer that carries that id, and must show the team name instead.
func TestNoHiddenIDInOutput(t *testing.T) {
	const seg = int64(1)<<40 + 7
	bigNumber := regexp.MustCompile(`\d{13,}`)
	clean := func(what, out string) {
		t.Helper()
		if bigNumber.MatchString(out) || strings.Contains(out, "@team-") {
			t.Fatalf("%s prints a hidden id: %q", what, out)
		}
	}
	hit := core.SearchHit{ID: 3, Title: "t", Project: seg, Class: core.Structure, Team: "acme", From: "team:acme"}
	clean("searchLine", searchLine(hit))
	hit.Team = "" // an older gateway sends no name: still no id
	clean("searchLine without a name", searchLine(hit))
	sub := core.Submission{ID: 1, Project: seg, State: "pending", Class: core.Data, Title: "t"}
	clean("submissionLine", submissionLine(sub, map[int64]string{7: "acme"}))
	clean("submissionLine without a name", submissionLine(sub, nil))
	if !strings.Contains(submissionLine(sub, map[int64]string{7: "acme"}), "team acme") {
		t.Fatal("team name missing")
	}
	// The commands, against answers that carry the hidden id.
	var out bytes.Buffer
	e := &projectEnv{out: &out, connect: func() (connection, string) { return connection{}, "" }, do: func(_ connection, m, p string, _ map[string]string, _ []byte) (int, []byte, http.Header) {
		switch {
		case strings.HasPrefix(p, "/minigw/v1/teams/"):
			return 200, []byte(fmt.Sprintf(`{"id":7,"name":"acme","segment":%d}`, seg)), nil
		case strings.HasSuffix(p, "/notes"):
			return 200, []byte(fmt.Sprintf(`{"notes":[{"id":5,"project":%d,"team":"acme","class":"data","title":"T","author":"a","updated":"2026-11-17T10:00:00Z","revision":1}]}`, seg)), nil
		case strings.Contains(p, "/notes/"):
			return 200, []byte(fmt.Sprintf(`{"id":5,"project":%d,"team":"acme","class":"data","title":"T","body":"b","author":"a","updated":"2026-11-17T10:00:00Z","revision":1}`, seg)), nil
		case strings.HasSuffix(p, "/kb/faq"):
			return 200, []byte(`{"entries":[]}`), nil
		case strings.Contains(p, "/kb/submissions"):
			return 200, []byte(fmt.Sprintf(`{"submissions":[{"id":8,"project":%d,"class":"data","title":"P","state":"pending"}]}`, seg)), nil
		case strings.Contains(p, "/kb/search"):
			return 200, []byte(fmt.Sprintf(`{"hits":[{"id":5,"title":"T","project":%d,"class":"data","snippet":"s","updated":"2026-11-17T10:00:00Z","team":"acme"}]}`, seg)), nil
		}
		t.Fatal("unexpected", m, p)
		return 599, nil, nil
	}}
	for _, args := range [][]string{{"note", "list", "--team", "acme"}, {"note", "read", "5", "--team", "acme"}, {"kb", "faq", "--team", "acme"}, {"kb", "submissions", "--team", "acme"}, {"kb", "search", "--team", "acme", "x"}} {
		out.Reset()
		var err error
		if args[0] == "note" {
			err = e.note(args[1:])
		} else {
			err = e.kb(args[1:])
		}
		if err != nil {
			t.Fatal(args, err)
		}
		clean(strings.Join(args, " "), out.String())
	}
	// MCP: the model sees the team name; note_read still takes the id it was given.
	out.Reset()
	var asked []string
	e.in = strings.NewReader(mcpInput(t,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"kb_search","arguments":{"query":"x"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"note_read","arguments":{"id":5}}}`))
	inner := e.do
	e.do = func(c connection, m, p string, h map[string]string, b []byte) (int, []byte, http.Header) {
		if strings.Contains(p, "/notes/") || strings.Contains(p, "/kb/search") {
			asked = append(asked, p)
		}
		if p == "/minigw/v1/projects/demo" {
			return 200, []byte(`{"id":1}`), nil
		}
		return inner(c, m, p, h, b)
	}
	if err := e.kbMCP([]string{"--project", "demo"}); err != nil {
		t.Fatal(err)
	}
	clean("kb mcp", out.String())
	if !strings.Contains(out.String(), "acme") || len(asked) != 2 || !strings.HasSuffix(asked[1], "/notes/5") {
		t.Fatal(out.String(), asked)
	}
}

// The scope hint is added to the answer of a note outside the linked project, not when --team or a run already named the scope, and
// the gateway's own answer (and its status, for the profile hint) stays as it was.
func TestNoteScopeHint(t *testing.T) {
	nf := gatewayError(404, nil, nil)
	got := noteScopeHint(nf, false)
	var g gwStatusError
	if !errors.As(got, &g) || g.status != 404 || got.Error() != "not found or not visible\n"+noteScopeText {
		t.Fatal(got)
	}
	if noteScopeHint(nf, true) != nf {
		t.Fatal("hint added although the scope was named")
	}
	other := gatewayError(403, nil, nil)
	if noteScopeHint(other, false) != other {
		t.Fatal("hint added to another answer")
	}
}

// A project admin reviews another member's submission over a real gateway; members without the role, and the admin's own
// submission, are refused.
func TestKBMemberReviewEndToEnd(t *testing.T) {
	c := newClientFx(t)
	dir := t.TempDir()
	ana, _, _ := c.env("ana", dir, "Reviewed by an admin")
	if err := ana.kb([]string{"submit", "fakture", "--key", "p1", "--title", "Proposal", "-"}); err != nil {
		t.Fatal(err)
	}
	if out, err := c.run("ana", dir, "", "kb", "review", "fakture"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatal("a developer reviews", out, err)
	}
	if out, err := c.run("ana", dir, "", "kb", "accept", "1"); err == nil {
		t.Fatal("a developer accepted", out)
	}
	if out := c.must("fani", dir, "", "project", "list"); !strings.Contains(out, "1 submission waits for review") {
		t.Fatal(out)
	}
	review := c.must("fani", dir, "", "kb", "review")
	if !strings.Contains(review, "Reviewed by an admin") || !strings.Contains(review, "writer ana") {
		t.Fatal(review)
	}
	if out, err := c.run("fani", dir, "", "kb", "accept", "1"); err == nil || !strings.Contains(err.Error(), "--version") { // no terminal here
		t.Fatal("accepted without naming the version", out, err)
	}
	if out := c.must("fani", dir, "", "kb", "accept", "1", "--version", versionIn(t, review)); !strings.HasSuffix(out, "Reviewed by an admin\nsubmission 1 accepted\n") {
		t.Fatal(out)
	}
	fani, _, _ := c.env("fani", dir, "Mine")
	if err := fani.kb([]string{"submit", "fakture", "--key", "p2", "-"}); err != nil {
		t.Fatal(err)
	}
	if out, err := c.run("fani", dir, "", "kb", "reject", "2", "--reason", "x"); err == nil || !strings.Contains(err.Error(), "your own submission") {
		t.Fatal(out, err)
	}
}

// versionIn is the whole version line kb review printed (the one submission of the listing).
func versionIn(t *testing.T, review string) string {
	t.Helper()
	m := regexp.MustCompile(`(?m)^version ([0-9a-f]{64})$`).FindStringSubmatch(review)
	if m == nil {
		t.Fatal("no version in", review)
	}
	return m[1]
}

// M1': an accept names the WHOLE version: a 12-hex tag is refused outright, also when it is the prefix of the text that is
// there now (two texts the author made to share a prefix), and a different whole hash is "changed".
func TestConfirmAcceptNeedsTheWholeVersion(t *testing.T) {
	a := "e01aad4208f5d17a" + strings.Repeat("0", 48)
	b := "e01aad4208f5d2f8" + strings.Repeat("1", 48) // the same 12-hex prefix as a
	var out bytes.Buffer
	if err := confirmAccept(b, a[:12], false, strings.NewReader(""), &out); err == nil || err.Error() != versionWhole {
		t.Fatal(err)
	}
	if err := confirmAccept(b, a, false, strings.NewReader(""), &out); err == nil || err.Error() != changedSinceRead {
		t.Fatal(err)
	}
	if err := confirmAccept(a, a, false, strings.NewReader(""), &out); err != nil {
		t.Fatal(err)
	}
}

// C3: what members and runs see of another gateway's mirror, as text and JSON (client/testdata/kb-search-upstream.json, shared with
// the Swift client): every hub hit and entry names its origin; a read's text starts with the marker line.
func TestKBUpstreamMirrorOutput(t *testing.T) {
	saved := time.Local
	time.Local = time.UTC
	defer func() { time.Local = saved }()
	var f struct {
		Marker, LineRule string
		Member           []struct {
			Args      []string
			Out       string
			Responses []cmdResp
			Requests  [][]string
		}
		Read, ReadJSON struct {
			Args            []string
			Path, Body, Out string
		}
	}
	if e := json.Unmarshal(srcFile(t, "testdata/kb-search-upstream.json"), &f); e != nil {
		t.Fatal(e)
	}
	for _, c := range f.Member {
		var out bytes.Buffer
		var requests [][]string
		env := &projectEnv{out: &out, connect: func() (connection, string) { return connection{}, "" }, do: func(_ connection, method, path string, _ map[string]string, b []byte) (int, []byte, http.Header) {
			requests = append(requests, []string{method, path, string(b)})
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
		upstreamLines(t, f.LineRule, out.String())
	}
	for _, c := range []struct {
		Args            []string
		Path, Body, Out string
	}{f.Read, f.ReadJSON} {
		var out bytes.Buffer
		e := &projectEnv{out: &out, agent: true, runConn: &connection{}, do: func(_ connection, m, p string, _ map[string]string, _ []byte) (int, []byte, http.Header) {
			if m != "GET" || p != c.Path {
				t.Fatal(m, p)
			}
			return 200, []byte(c.Body), nil
		}}
		if err := e.note(c.Args); err != nil {
			t.Fatal(err)
		}
		if out.String() != c.Out || !strings.Contains(out.String(), f.Marker) {
			t.Fatalf("%v: %q", c.Args, out.String())
		}
		upstreamLines(t, f.LineRule, out.String())
	}
}

// upstreamLines: every line of a text output carries the origin of its hit or note, or is quoted, or is the gateway's own frame or hint.
func upstreamLines(t *testing.T, rule, out string) {
	t.Helper()
	re := regexp.MustCompile(rule)
	for _, l := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		if l != "" && !re.MatchString(l) {
			t.Fatalf("a line of hub text passes for something else: %q in\n%s", l, out)
		}
	}
}

// A tab in a submission is shown to the reviewer where it is: the text on screen is the text that is published.
func TestKBReviewShowsATab(t *testing.T) {
	c := newClientFx(t)
	dir := t.TempDir()
	c.must("ana", dir, "a\tb\n", "kb", "submit", "fakture", "--key", "p1", "--title", "Proposal", "-")
	if review := c.must("fani", dir, "", "kb", "review"); !strings.Contains(review, "a\tb") {
		t.Fatalf("%q", review)
	}
}

// A submit with hidden characters is refused with their place, and the writer sees the whole message; an emoji text goes through.
func TestKBSubmitHiddenRefusedEmojiAccepted(t *testing.T) {
	c := newClientFx(t)
	dir := t.TempDir()
	_, err := c.run("ana", dir, "one\nab​c\n", "kb", "submit", "fakture", "--key", "h1", "--title", "T", "-")
	if err == nil || !strings.Contains(err.Error(), "the text has 1 hidden characters (line 2 column 3: U+200B ZERO WIDTH SPACE): remove them and submit again") {
		t.Fatal(err)
	}
	c.must("ana", dir, "⚠️ Pitfall\nbody\n", "kb", "submit", "fakture", "--key", "e1", "--title", "T", "-")
}

// A new client against an older gateway: the kb and team commands say which version has them instead of "not found or not visible".
func TestOlderGatewayNamesTheVersionForKBAndTeam(t *testing.T) {
	for _, tc := range []struct{ version, want string }{
		{"0.7.5", "this gateway is 0.7.5; kb needs 0.7.10 or newer: update the gateway (/minigw:gw update there)"},
		{"0.7.10", "not found or not visible"},
	} {
		asked := 0
		env := &projectEnv{out: new(bytes.Buffer), connect: func() (connection, string) { return connection{}, "" },
			do: olderRouteHint(func(_ connection, m, p string, _ map[string]string, _ []byte) (int, []byte, http.Header) {
				switch p {
				case "/health/ready":
					asked++
					return 200, []byte(`{"version":"` + tc.version + `"}`), nil
				case apiProjects + "/shop":
					return 200, []byte(`{"id":7}`), nil
				}
				return 404, []byte(`{"error":{"message":"not found"}}`), nil
			})}
		err := env.kb([]string{"search", "shop", "x"})
		if err == nil || err.Error() != tc.want {
			t.Fatalf("%s: %v", tc.version, err)
		}
	}
	env := &projectEnv{out: new(bytes.Buffer), connect: func() (connection, string) { return connection{}, "" },
		do: olderRouteHint(func(_ connection, m, p string, _ map[string]string, _ []byte) (int, []byte, http.Header) {
			if p == "/health/ready" {
				return 200, []byte(`{"version":"0.7.0"}`), nil
			}
			return 404, []byte(`{}`), nil
		})}
	if err := env.team([]string{"list"}); err == nil || !strings.Contains(err.Error(), "this gateway is 0.7.0; team needs 0.7.1 or newer") {
		t.Fatal(err)
	}
}
