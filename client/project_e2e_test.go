package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"minigw/core"
)

// clientFx: the in-process gateway of the core tests with real TCP, and project/note commands run as a member's
// client would (Noise tunnel with the member's device key, a temporary state file and folder).
type clientFx struct {
	t     *testing.T
	f     *coreFx
	base  string
	state string
}

func newClientFx(t *testing.T) *clientFx {
	t.Helper()
	s, _, base := coreLiveServer(t)
	f := &coreFx{t: t, s: s, key: s.key, dev: map[string]*device{}, uid: map[string]int64{}, pid: map[string]int64{}}
	for _, n := range []string{"ana", "ana2", "bor", "fani", "eva", "cene", "dana"} {
		f.dev[n] = newDevice(n)
		d := serveDevice{ID: devID(f.dev[n]), Name: n}
		s.update(func(c *serveConfig) error { c.Devices = append(c.Devices, d); return nil })
	}
	// coreLiveServer's owner row is user 1; setup creates the fixture's users after it
	f.setup()
	return &clientFx{t: t, f: f, base: base, state: filepath.Join(t.TempDir(), "projects.json")}
}

type out struct{ bytes.Buffer }

// env builds the command environment of one user in one folder.
func (c *clientFx) env(user, cwd, stdin string) (*projectEnv, *out, *out) {
	dev := c.f.dev[user]
	np, err := newNoiseProxy(c.base, c.f.s.key.PublicKey().Bytes(), dev)
	if err == nil {
		err = np.start()
	}
	if err != nil {
		c.t.Fatal(err)
	}
	c.t.Cleanup(np.stop)
	conn := connection{URL: np.localURL(), Token: "tok"}
	o, e := &out{}, &out{}
	return &projectEnv{
		connect: func() (connection, string) { return conn, "gw-test" },
		state:   c.state, cwd: cwd, in: strings.NewReader(stdin), out: o, err: e, do: realDo, sleep: func(time.Duration) {},
	}, o, e
}

func (c *clientFx) run(user, cwd, stdin string, args ...string) (string, error) {
	c.t.Helper()
	e, o, _ := c.env(user, cwd, stdin)
	var err error
	switch args[0] {
	case "note":
		err = e.note(args[1:])
	case "usage":
		err = e.usage(args[1:])
	default:
		err = e.project(args[1:])
	}
	return o.String(), err
}

func (c *clientFx) must(user, cwd, stdin string, args ...string) string {
	c.t.Helper()
	o, err := c.run(user, cwd, stdin, args...)
	if err != nil {
		c.t.Fatalf("%v: %v", args, err)
	}
	return o
}

func wantErr(t *testing.T, err error, contains string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), contains) {
		t.Fatalf("error %v, want one with %q", err, contains)
	}
}

func TestClientProjectAndNoteFlow(t *testing.T) {
	c := newClientFx(t)
	dir := folder(t, t.TempDir(), "fakture-checkout")
	sub := folder(t, dir, "src")

	// before a link: the note commands say what to do; a hint turns into a suggestion
	_, err := c.run("ana", dir, "", "note", "list")
	wantErr(t, err, "not linked")
	hint(t, dir, "fakture")
	_, err = c.run("ana", sub, "", "note", "list")
	wantErr(t, err, "suggests project fakture")
	if _, err := os.Stat(c.state); err == nil {
		t.Fatal("a hint created a link")
	}

	o := c.must("ana", dir, "", "project", "list")
	if !strings.Contains(o, "fakture") || !strings.Contains(o, "developer") {
		t.Fatalf("list: %q", o)
	}
	if o = c.must("ana", dir, "", "project", "link", "fakture"); !strings.Contains(o, "linked "+dir) {
		t.Fatalf("link: %q", o)
	}
	_, err = c.run("ana", dir, "", "project", "link", "fakture")
	wantErr(t, err, "already linked")
	if o = c.must("ana", sub, "", "project", "show"); !strings.Contains(o, "fakture (id") || !strings.Contains(o, "agrees") {
		t.Fatalf("show: %q", o)
	}
	if o = c.must("ana", sub, "", "project", "list"); !strings.Contains(o, "(linked here)") {
		t.Fatalf("list: %q", o)
	}

	// push, list, read
	o = c.must("ana", sub, "# Invoice numbering\nOne counter per year.\n", "note", "push", "--class", "structure", "-")
	if !strings.Contains(o, "created (structure, revision 1)") {
		t.Fatalf("push: %q", o)
	}
	var id int64
	fmt.Sscanf(o, "note %d", &id)
	// the same machine state serves every user of the test; bor reads through the same link
	if o = c.must("bor", dir, "", "note", "list"); !strings.Contains(o, "Invoice numbering") {
		t.Fatalf("list: %q", o)
	}
	if o = c.must("bor", dir, "", "note", "list", "--class", "data"); !strings.Contains(o, "no notes") {
		t.Fatalf("list data: %q", o)
	}
	o = c.must("bor", dir, "", "note", "read", fmt.Sprint(id))
	if !strings.Contains(o, "One counter per year.") || !strings.Contains(o, "by ana") || !strings.Contains(o, "revision 1") {
		t.Fatalf("read: %q", o)
	}
	var j struct {
		Notes []struct{ ID int64 }
	}
	if err := json.Unmarshal([]byte(c.must("bor", dir, "", "note", "list", "--json")), &j); err != nil || len(j.Notes) != 1 || j.Notes[0].ID != id {
		t.Fatalf("--json: %+v %v", j, err)
	}

	// edit: a stale revision shows the current one; without --revision the command reads it first
	c.must("ana", dir, "", "note", "edit", fmt.Sprint(id), "--title", "Numbering")
	_, err = c.run("ana", dir, "new body\n", "note", "edit", fmt.Sprint(id), "--revision", "1", "-")
	wantErr(t, err, "current revision is 2")
	if o = c.must("ana", dir, "new body\n", "note", "edit", fmt.Sprint(id), "-"); !strings.Contains(o, "revision 3") {
		t.Fatalf("edit: %q", o)
	}
	if o = c.must("ana", dir, "", "note", "read", fmt.Sprint(id)); !strings.Contains(o, "Numbering") || !strings.Contains(o, "new body") {
		t.Fatalf("read after edit: %q", o)
	}
	// a reader may read but not change; a stranger finds nothing
	_, err = c.run("eva", dir, "x", "note", "edit", fmt.Sprint(id), "-")
	wantErr(t, err, "not allowed")
	_, err = c.run("dana", dir, "", "note", "read", fmt.Sprint(id))
	wantErr(t, err, "not found or not visible")

	// rm
	if o = c.must("ana", dir, "", "note", "rm", fmt.Sprint(id)); !strings.Contains(o, "deleted") {
		t.Fatalf("rm: %q", o)
	}
	_, err = c.run("ana", dir, "", "note", "read", fmt.Sprint(id))
	wantErr(t, err, "not found or not visible")

	// unlink
	if o = c.must("ana", sub, "", "project", "unlink"); !strings.Contains(o, "unlinked "+dir) {
		t.Fatalf("unlink: %q", o)
	}
	_, err = c.run("ana", dir, "", "note", "list")
	wantErr(t, err, "suggests project fakture") // the hint is still there, the link is gone
}

func TestClientPrivateNoteMessageAndDefaults(t *testing.T) {
	c := newClientFx(t)
	dir := folder(t, t.TempDir(), "p")
	c.must("ana", dir, "", "project", "link", "fakture")
	o := c.must("ana", dir, "secret text\nmore\n", "note", "push", "--class", "private", "-")
	if strings.Count(o, privateNote) != 1 {
		t.Fatalf("private message: %q", o)
	}
	if o = c.must("ana", dir, "plain\n", "note", "push", "-"); strings.Contains(o, privateNote) || !strings.Contains(o, "(data,") {
		t.Fatalf("default class: %q", o)
	}
	// the title is the first line; bor does not see the private note
	if o = c.must("ana", dir, "", "note", "list"); !strings.Contains(o, "secret text") {
		t.Fatal(o)
	}
	if o = c.must("bor", dir, "", "note", "list"); strings.Contains(o, "secret text") {
		t.Fatal("bor sees ana's private note")
	}
	// --json: the answer is JSON, the privacy line goes to stderr
	e, so, se := c.env("ana", dir, "x\n")
	if err := e.note([]string{"push", "--class", "private", "--json", "-"}); err != nil {
		t.Fatal(err)
	}
	var n struct{ Class string }
	if json.Unmarshal(so.Bytes(), &n) != nil || n.Class != "private" || !strings.Contains(se.String(), privateNote) {
		t.Fatalf("stdout %q stderr %q", so.String(), se.String())
	}
}

func TestClientPushRetriesALostAnswerSafely(t *testing.T) {
	c := newClientFx(t)
	dir := folder(t, t.TempDir(), "p")
	c.must("ana", dir, "", "project", "link", "fakture")
	e, o, _ := c.env("ana", dir, "lost answer\n")
	var mu sync.Mutex
	posts, keys := 0, map[string]bool{}
	real := e.do
	e.do = func(cn connection, method, path string, h map[string]string, body []byte) (int, []byte, http.Header) {
		st, data, hd := real(cn, method, path, h, body)
		if method == "POST" {
			mu.Lock()
			posts++
			keys[h["Idempotency-Key"]] = true
			drop := posts <= 2 // the gateway created the note, the answer never arrived (twice)
			mu.Unlock()
			if drop {
				return 0, nil, nil
			}
		}
		return st, data, hd
	}
	if err := e.note([]string{"push", "-"}); err != nil {
		t.Fatal(err)
	}
	if posts != 3 || len(keys) != 1 || !strings.Contains(o.String(), "created") {
		t.Fatalf("posts %d keys %v out %q", posts, keys, o.String())
	}
	if l := c.must("ana", dir, "", "note", "list"); strings.Count(l, "lost answer") != 1 {
		t.Fatalf("duplicates: %q", l)
	}
	// every answer lost: the error names the key, and repeating with it is safe too
	e2, _, _ := c.env("ana", dir, "again\n")
	real2 := e2.do
	var firstKey string
	e2.do = func(cn connection, m, p string, h map[string]string, b []byte) (int, []byte, http.Header) {
		if m == "POST" {
			firstKey = h["Idempotency-Key"]
			real2(cn, m, p, h, b)
			return 0, nil, nil
		}
		return real2(cn, m, p, h, b)
	}
	err := e2.note([]string{"push", "-"})
	wantErr(t, err, "--key "+firstKey)
	c.must("ana", dir, "again\n", "note", "push", "--key", firstKey, "-")
	if l := c.must("ana", dir, "", "note", "list"); strings.Count(l, "again") != 1 {
		t.Fatalf("duplicates after --key: %q", l)
	}
}

func TestClientGrantOnlyUserCannotLink(t *testing.T) {
	c := newClientFx(t)
	dir := folder(t, t.TempDir(), "p")
	_, err := c.run("cene", dir, "", "project", "link", "fakture") // a structure grant, no direct role
	wantErr(t, err, "not allowed")
	wantErr(t, err, "direct role")
	if st, _ := loadProjectState(c.state); len(st.Links) != 0 {
		t.Fatalf("a link was saved: %+v", st)
	}
	_, err = c.run("dana", dir, "", "project", "link", "fakture")
	wantErr(t, err, "not found or not visible")
	c.must("cene", dir, "", "project", "link", "eureko") // a direct member links
	// the grant lets cene read fakture's list entry with what it gives
	if o := c.must("cene", dir, "", "project", "list"); !strings.Contains(o, "reads structure by grant") {
		t.Fatalf("list: %q", o)
	}
}

func TestClientNestedLinksAndProjectFlag(t *testing.T) {
	c := newClientFx(t)
	if err := c.f.s.core.SetMember(t0, c.f.pid["eureko"], c.f.uid["fani"], core.Developer); err != nil {
		t.Fatal(err)
	}
	root := folder(t, t.TempDir(), "mono")
	inner := folder(t, root, "services", "billing")
	c.must("fani", root, "", "project", "link", "fakture")
	c.must("fani", inner, "", "project", "link", "eureko") // deeper folder: allowed, nearest wins
	if o := c.must("fani", folder(t, inner, "x"), "", "project", "show"); !strings.Contains(o, "eureko") {
		t.Fatalf("nested: %q", o)
	}
	if o := c.must("fani", folder(t, root, "services"), "", "project", "show"); !strings.Contains(o, "fakture") {
		t.Fatalf("outer: %q", o)
	}
	// --project only confirms the linked project of this checkout
	c.must("fani", root, "n\n", "note", "push", "--project", "fakture", "-")
	_, err := c.run("fani", root, "n\n", "note", "push", "--project", "eureko", "-")
	wantErr(t, err, "--project only names the linked project")
	// unlink from a subfolder removes the link that applies there
	if o := c.must("fani", folder(t, inner, "x"), "", "project", "unlink"); !strings.Contains(o, "eureko") {
		t.Fatalf("unlink: %q", o)
	}
	if o := c.must("fani", inner, "", "project", "show"); !strings.Contains(o, "fakture") {
		t.Fatalf("after unlink: %q", o)
	}
	// a hint with another slug in the linked root refuses the note commands
	hint(t, root, "eureko")
	_, err = c.run("fani", root, "", "note", "list")
	wantErr(t, err, "says eureko")
	_, err = c.run("fani", root, "", "project", "link", "eureko")
	wantErr(t, err, "already linked")
}

func TestClientErrorsAsSentences(t *testing.T) {
	c := newClientFx(t)
	dir := folder(t, t.TempDir(), "p")
	c.must("ana", dir, "", "project", "link", "fakture")
	_, err := c.run("ana", dir, "", "note", "read", "999999")
	wantErr(t, err, "not found or not visible")
	_, err = c.run("ana", dir, strings.Repeat("x", 65<<10), "note", "push", "-")
	wantErr(t, err, "too large")
	_, err = c.run("ana", dir, "x", "note", "push", "--class", "nope", "-")
	wantErr(t, err, "rejected: invalid class")
	_, err = c.run("ana", dir, "x", "note", "read", "abc")
	wantErr(t, err, "not a note id")
	// 429: the write limit (60 a minute) says when to retry
	var last error
	for i := 0; i < 62 && last == nil; i++ {
		_, last = c.run("ana", dir, fmt.Sprintf("n%d\n", i), "note", "push", "-")
	}
	wantErr(t, last, "try again in")
	// the gateway's project store goes away: 503 in plain words
	c.f.s.stopUsage() // the usage writer reads s.core: stop it before the test takes the core away
	c.f.s.core = nil
	_, err = c.run("bor", dir, "", "note", "list")
	wantErr(t, err, "the gateway's project store is not available")
}

func TestClientRefusedInsideAnAgentRun(t *testing.T) {
	c := newClientFx(t)
	dir := folder(t, t.TempDir(), "p")
	for _, args := range [][]string{{"project", "link", "fakture"}, {"project", "unlink"}, {"project", "list"}, {"note", "push", "-"},
		{"note", "edit", "1", "-"}, {"note", "rm", "1"}, {"note", "list"}} {
		e, _, _ := c.env("ana", dir, "x")
		e.agent = true
		var err error
		if args[0] == "note" {
			err = e.note(args[1:])
		} else {
			err = e.project(args[1:])
		}
		wantErr(t, err, "inside an agent run only")
		if strings.Contains(err.Error(), "\n") {
			t.Fatalf("not one line: %q", err)
		}
	}
	if _, err := os.Stat(c.state); err == nil {
		t.Fatal("state written inside an agent run")
	}
}

func TestProxyRewritesTheProjectHeader(t *testing.T) { // review L1
	cases := []struct{ name, in, project, want string }{
		{"forged header dropped", "POST /v1/messages HTTP/1.1\r\nHost: x\r\nX-Minigw-Project: 9\r\nContent-Length: 0", "", "POST /v1/messages HTTP/1.1\r\nHost: x\r\nContent-Length: 0"},
		{"any case, replaced by ours", "POST /v1/messages HTTP/1.1\r\nhost: x\r\nx-MINIGW-project : 9\r\nX-MINIGW-PROJECT: 8", "42", "POST /v1/messages HTTP/1.1\r\nX-Minigw-Project: 42\r\nhost: x"},
		{"folded continuation dropped", "GET / HTTP/1.1\r\nX-Minigw-Project: 9\r\n 10\r\nHost: x", "", "GET / HTTP/1.1\r\nHost: x"},
		{"similar names stay", "GET / HTTP/1.1\r\nX-Minigw-Project-Other: 9\r\nX-Minigw-Projects: 1", "7", "GET / HTTP/1.1\r\nX-Minigw-Project: 7\r\nX-Minigw-Project-Other: 9\r\nX-Minigw-Projects: 1"},
		{"no project, no header", "GET / HTTP/1.1\r\nHost: x", "", "GET / HTTP/1.1\r\nHost: x"},
	}
	for _, c := range cases {
		if got := string(rewriteHead([]byte(c.in), c.project)); got != c.want {
			t.Errorf("%s:\n got %q\nwant %q", c.name, got, c.want)
		}
	}
}

func TestMemberRoutesRefuseAnAgentDescendant(t *testing.T) { // review L7
	c := newClientFx(t)
	dir := folder(t, t.TempDir(), "p")
	c.must("ana", dir, "", "project", "list")
	adminPeerCheckFn = func(net.Conn) error { return errAgentPeer }
	defer func() { adminPeerCheckFn = adminPeerCheck }()
	_, err := c.run("ana", dir, "", "project", "list")
	wantErr(t, err, "no project access")
	_, err = c.run("ana", dir, "", "usage")
	wantErr(t, err, "no project access")
}
