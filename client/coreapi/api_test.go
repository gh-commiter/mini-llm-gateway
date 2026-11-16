package coreapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"minigw/core"
)

var bg = context.Background()

type fixture struct {
	st  *core.Store
	api *API
	uid map[string]int64
	pid map[string]int64
	nid map[string]int64
}

func newFixture(t *testing.T) *fixture { return newFixtureIn(t, t.TempDir()) }

// newFixtureIn seeds the store in dir.
func newFixtureIn(t *testing.T, dir string) *fixture {
	t.Helper()
	st, err := core.Open(dir, core.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	f := &fixture{st: st, uid: map[string]int64{}, pid: map[string]int64{}, nid: map[string]int64{}}
	f.api = New(Options{Store: func() *core.Store { return st }, Dir: t.TempDir()})
	ok := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range []string{"ana", "fani", "cene", "eva"} {
		u, err := st.CreateUser(bg, n)
		ok(err)
		f.uid[n] = u.ID
		ok(st.SetBinding(bg, "dev-"+n, u.ID))
	}
	for _, n := range []string{"fakture", "eureko"} {
		p, err := st.CreateProject(bg, n, n)
		ok(err)
		f.pid[n] = p.ID
	}
	ok(st.SetMember(bg, f.pid["fakture"], f.uid["ana"], core.Developer))
	ok(st.SetMember(bg, f.pid["fakture"], f.uid["fani"], core.Admin))
	ok(st.SetMember(bg, f.pid["fakture"], f.uid["eva"], core.Reader))
	ok(st.SetMember(bg, f.pid["eureko"], f.uid["cene"], core.Developer))
	ok(st.AddGrant(bg, f.pid["fakture"], f.pid["eureko"], core.Structure))
	p := core.Principal{UserID: f.uid["ana"], DeviceID: "dev-ana"}
	for _, c := range []core.Class{core.Structure, core.Data, core.Private} {
		n, _, err := st.CreateNote(bg, p, f.pid["fakture"], "k-"+string(c), core.NoteInput{Class: c, Title: "t-" + string(c), Body: "body"})
		ok(err)
		f.nid[string(c)] = n.ID
	}
	return f
}

func member(dev string) Context { return Context{Kind: KindMember, DeviceID: dev, Approved: true} }
func get(path string) Request   { return Request{Method: "GET", Path: path} }

// cases covers every kind. Only requests whose answer does not depend on the clock or on random ids, so that two
// calls (in-process and over the wire) must give byte-identical responses.
func (f *fixture) cases() []struct {
	name   string
	c      Context
	r      Request
	status int
} {
	notes := fmt.Sprintf("/minigw/v1/projects/%d/notes", f.pid["fakture"])
	cap := core.RunCap{RunID: "r1", UserID: f.uid["cene"], DeviceID: "dev-cene", ProjectID: f.pid["eureko"]}
	cap.Epoch, _ = f.st.Epoch(bg)
	stale := cap
	stale.Epoch--
	type tc = struct {
		name   string
		c      Context
		r      Request
		status int
	}
	return []tc{
		{"member: own submissions", member("dev-ana"), get(fmt.Sprintf("/minigw/v1/projects/%d/kb/submissions", f.pid["fakture"])), 200},
		{"admin: pending submissions", Context{Kind: KindAdmin}, get("/kb/submissions"), 200},
		{"run: no submissions", Context{Kind: KindRun, Run: &cap, Approved: true}, get("/minigw/v1/projects/1/kb/submissions"), 404},
		{"member: project list", member("dev-ana"), get("/minigw/v1/projects"), 200},
		{"member: project by slug", member("dev-ana"), get("/minigw/v1/projects/fakture"), 200},
		{"member: grant-only project", member("dev-cene"), get("/minigw/v1/projects/fakture"), 200},
		{"member: link-check with a role", member("dev-ana"), Request{Method: "POST", Path: "/minigw/v1/projects/fakture/link-check"}, 200},
		{"member: link-check grant only", member("dev-cene"), Request{Method: "POST", Path: "/minigw/v1/projects/fakture/link-check"}, 403},
		{"member: stranger sees nothing", member("dev-cene"), get("/minigw/v1/projects/nope"), 404},
		{"member: note list", member("dev-ana"), get(notes), 200},
		{"member: note list as a grantee", member("dev-cene"), get(notes), 200},
		{"member: note read", member("dev-ana"), get(fmt.Sprintf("%s/%d", notes, f.nid["data"])), 200},
		{"member: private note not visible", member("dev-cene"), get(fmt.Sprintf("%s/%d", notes, f.nid["private"])), 404},
		{"member: bad limit", member("dev-ana"), Request{Method: "GET", Path: notes, RawQuery: "limit=x"}, 400},
		{"member: too large body on an invisible note is a 404", member("dev-eva"), Request{Method: "PUT", Path: fmt.Sprintf("%s/%d", notes, f.nid["private"]), BodyTooLarge: true}, 404},
		{"member: too large body on a visible note it may not change", member("dev-eva"), Request{Method: "PUT", Path: fmt.Sprintf("%s/%d", notes, f.nid["structure"]), BodyTooLarge: true}, 403},
		{"member: too large body from the author", member("dev-ana"), Request{Method: "PUT", Path: fmt.Sprintf("%s/%d", notes, f.nid["data"]), BodyTooLarge: true}, 413},
		{"member: usage", member("dev-ana"), get("/minigw/v1/usage"), 200},
		{"member: usage of a project as a reader", member("dev-eva"), Request{Method: "GET", Path: "/minigw/v1/usage", RawQuery: "project=fakture"}, 404},
		{"member: no device", Context{Kind: KindMember}, get("/minigw/v1/projects"), 403},
		{"member: device not approved", Context{Kind: KindMember, DeviceID: "dev-ana"}, get("/minigw/v1/projects"), 403},
		{"member: unbound device", member("dev-x"), get("/minigw/v1/projects"), 403},
		{"member: agent peer", Context{Kind: KindMember, DeviceID: "dev-ana", Approved: true, AgentPeer: true}, get("/minigw/v1/projects"), 403},
		{"member: unknown route", member("dev-ana"), get("/minigw/v1/projects/fakture/bogus"), 404},
		{"admin: users", Context{Kind: KindAdmin}, get("/users"), 200},
		{"admin: projects", Context{Kind: KindAdmin}, get("/projects"), 200},
		{"admin: members", Context{Kind: KindAdmin}, get(fmt.Sprintf("/projects/%d/members", f.pid["fakture"])), 200},
		{"admin: bad body", Context{Kind: KindAdmin}, Request{Method: "POST", Path: "/users", Body: []byte(`{"nope":1}`)}, 400},
		{"admin: usage events", Context{Kind: KindAdmin}, Request{Method: "GET", Path: "/usage/events", RawQuery: "days=7"}, 200},
		{"admin: not a core route", Context{Kind: KindAdmin}, get("/devices"), 0},
		{"admin: 0.5.x usage is not ours", Context{Kind: KindAdmin}, get("/usage"), 0},
		{"run: notes", Context{Kind: KindRun, Run: &cap, Approved: true}, get("/minigw/v1/notes"), 200},
		{"run: note by id", Context{Kind: KindRun, Run: &cap, Approved: true}, get(fmt.Sprintf("/minigw/v1/notes/%d", f.nid["structure"])), 200},
		{"run: private never", Context{Kind: KindRun, Run: &cap, Approved: true}, get(fmt.Sprintf("/minigw/v1/notes/%d", f.nid["private"])), 404},
		{"run: epoch changed", Context{Kind: KindRun, Run: &stale, Approved: true}, get("/minigw/v1/notes"), 403},
		{"run: device not approved now", Context{Kind: KindRun, Run: &cap}, get("/minigw/v1/notes"), 403},
		{"run: forged project in the query is ignored", Context{Kind: KindRun, Run: &cap, Approved: true},
			Request{Method: "GET", Path: "/minigw/v1/notes", RawQuery: fmt.Sprintf("project=%d", f.pid["fakture"])}, 200},
		{"run: writes are not routes", Context{Kind: KindRun, Run: &cap, Approved: true}, Request{Method: "POST", Path: "/minigw/v1/notes"}, 404},
		{"internal: epoch", Context{Kind: KindInternal}, Request{Method: "POST", Path: "/internal/epoch", Body: []byte(`{}`)}, 200},
		{"internal: admit without a project", Context{Kind: KindInternal}, in("admit", `{"device":"dev-ana","approved":true}`), 200},
		{"internal: admit refused (grant only)", Context{Kind: KindInternal}, in("admit", fmt.Sprintf(`{"device":"dev-cene","approved":true,"project":"%d"}`, f.pid["fakture"])), 200},
		{"internal: scope, no project", Context{Kind: KindInternal}, in("scope", `{"site":"cli","fields":["sid","/dir"]}`), 200},
		{"internal: scope, project", Context{Kind: KindInternal}, in("scope", `{"site":"cli","fields":["sid","/dir|u1|p2|e3"],"project":{"user":1,"project":2,"epoch":3}}`), 200},
		{"internal: agent-home", Context{Kind: KindInternal}, in("agent-home", `{"kind":"codex","user":1,"project":2,"epoch":3}`), 200},
		{"internal: agent-home with a path in the kind", Context{Kind: KindInternal}, in("agent-home", `{"kind":"../x","user":1,"project":2,"epoch":3}`), 400},
		{"internal: bound-user", Context{Kind: KindInternal}, in("bound-user", `{"device":"dev-fani"}`), 200},
		{"internal: unknown call", Context{Kind: KindInternal}, in("nope", `{}`), 404},
		{"internal: not a POST", Context{Kind: KindInternal}, get("/internal/epoch"), 404},
	}
}

func in(name, body string) Request {
	return Request{Method: "POST", Path: "/internal/" + name, Body: []byte(body)}
}

func TestEveryKindIsServedAsDocumented(t *testing.T) {
	f := newFixture(t)
	for _, c := range f.cases() {
		got := f.api.Handle(bg, c.c, c.r)
		if got.Status != c.status {
			t.Errorf("%s: %d %s, want %d", c.name, got.Status, got.Body, c.status)
		}
	}
}

// The fixed no_project_access body is the same bytes whichever check refuses, and so is the unavailable body.
func TestFixedBodies(t *testing.T) {
	f := newFixture(t)
	want := `{"error":{"message":"No project access","type":"no_project_access"}}`
	for _, c := range f.cases() {
		if strings.HasPrefix(c.name, "member: no device") || strings.HasPrefix(c.name, "member: agent") || strings.HasPrefix(c.name, "member: unbound") ||
			strings.HasPrefix(c.name, "member: device not approved") {
			if got := string(f.api.Handle(bg, c.c, c.r).Body); got != want {
				t.Errorf("%s: %s", c.name, got)
			}
		}
	}
	none := New(Options{})
	if r := none.Handle(bg, member("dev-ana"), get("/minigw/v1/projects")); r.Status != 503 || !strings.Contains(string(r.Body), "core_unavailable") {
		t.Fatalf("%d %s", r.Status, r.Body)
	}
	if r := none.Handle(bg, Context{Kind: KindAdmin}, get("/users")); r.Status != 503 {
		t.Fatalf("admin without a store: %d", r.Status)
	}
	// 404 for missing and for not visible: the same bytes
	a := f.api.Handle(bg, member("dev-cene"), get(fmt.Sprintf("/minigw/v1/projects/%d/notes/%d", f.pid["fakture"], f.nid["private"])))
	b := f.api.Handle(bg, member("dev-cene"), get(fmt.Sprintf("/minigw/v1/projects/%d/notes/999999", f.pid["fakture"])))
	if a.Status != 404 || !bytes.Equal(a.Body, b.Body) {
		t.Fatalf("%s / %s", a.Body, b.Body)
	}
}

func TestRetryAfterAndEnvelope(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 700; i++ {
		f.api.Handle(bg, member("dev-eva"), get("/minigw/v1/projects"))
	}
	r := f.api.Handle(bg, member("dev-eva"), get("/minigw/v1/projects"))
	if r.Status != 429 || r.Header.Get("Retry-After") == "" || !strings.Contains(string(r.Body), `"limit"`) {
		t.Fatalf("%d %v %s", r.Status, r.Header, r.Body)
	}
	if RetryAfterSeconds(0) != 1 || RetryAfterSeconds(1500e6) != 2 {
		t.Fatal("rounding")
	}
}

// The loopback test: an API behind an in-memory pipe answers with the same bytes as the in-process call.
func TestWireGivesTheSameBytesAsTheInProcessCall(t *testing.T) {
	f := newFixture(t)
	cli, srv := net.Pipe()
	defer cli.Close()
	go f.api.Serve(bg, srv)
	client := NewClient(cli)
	for _, c := range f.cases() {
		want := f.api.Handle(bg, c.c, c.r)
		got, err := client.Do(bg, c.c, c.r)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got.Status != want.Status || !bytes.Equal(got.Body, want.Body) || !reflect.DeepEqual(got.Header, want.Header) {
			t.Errorf("%s:\n wire       %d %v %s\n in-process %d %v %s", c.name, got.Status, got.Header, got.Body, want.Status, want.Header, want.Body)
		}
	}
	// a write (not byte-comparable by itself): the replay over the wire returns the first result of the in-process call
	create := Request{Method: "POST", Path: fmt.Sprintf("/minigw/v1/projects/%d/notes", f.pid["fakture"]), Body: []byte(`{"class":"data","title":"once","body":"b"}`),
		Header: map[string][]string{"Idempotency-Key": {"wire-1"}}}
	first := f.api.Handle(bg, member("dev-ana"), create)
	again, err := client.Do(bg, member("dev-ana"), create)
	if err != nil || first.Status != 201 || again.Status != 201 || !bytes.Equal(first.Body, again.Body) || again.Header.Get("Idempotent-Replay") != "true" {
		t.Fatalf("%v %d %d\n%s\n%s", err, first.Status, again.Status, first.Body, again.Body)
	}
}

func TestWireProtocolGuards(t *testing.T) {
	f := newFixture(t)
	// a frame over MaxFrame is refused before its payload is read
	cli, srv := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- f.api.Serve(bg, srv) }()
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], MaxFrame+1)
	go cli.Write(hdr[:]) // no payload follows
	if _, _, err := ReadResponse(cli); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("oversized: %v", err)
	}
	if err := <-done; !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("serve: %v", err)
	}
	cli.Close()
	// a wrong protocol version
	var buf bytes.Buffer
	b, _ := json.Marshal(wireRequest{V: ProtocolVersion + 1, ID: 1, Context: member("dev-ana"), Request: get("/minigw/v1/projects")})
	binary.Write(&buf, binary.BigEndian, uint32(len(b)))
	buf.Write(b)
	if _, _, _, err := ReadRequest(&buf); !errors.Is(err, ErrVersion) {
		t.Fatalf("version: %v", err)
	}
	// the biggest legal body fits in a frame
	var big bytes.Buffer
	if err := WriteRequest(&big, 1, member("dev-ana"), Request{Method: "PUT", Path: "/x", Body: bytes.Repeat([]byte("x"), MaxBody)}); err != nil {
		t.Fatalf("a body of MaxBody does not fit a frame: %v", err)
	}
	if _, _, r, err := ReadRequest(&big); err != nil || len(r.Body) != MaxBody {
		t.Fatalf("%v %d", err, len(r.Body))
	}
	// a response is matched to its request by id
	c1, s1 := net.Pipe()
	go func() { ReadRequest(s1); WriteResponse(s1, 99, Response{Status: 200}, "") }()
	if _, err := NewClient(c1).Do(bg, member("x"), get("/y")); err == nil || !strings.Contains(err.Error(), "answer 99") {
		t.Fatalf("id mismatch: %v", err)
	}
}

func TestInternalCallsInProcessAndTheirShapes(t *testing.T) {
	f := newFixture(t)
	var ep EpochReply
	if err := f.api.Call(bg, "epoch", struct{}{}, &ep); err != nil || ep.Epoch < 1 {
		t.Fatalf("%v %+v", err, ep)
	}
	var ok AdmitReply
	f.api.Call(bg, "admit", AdmitRequest{Device: "dev-ana", Approved: true, Project: fmt.Sprint(f.pid["fakture"])}, &ok)
	if !ok.OK || ok.User != f.uid["ana"] || ok.Project != f.pid["fakture"] || ok.RunID == "" || ok.Epoch != ep.Epoch {
		t.Fatalf("%+v", ok)
	}
	for name, in := range map[string]AdmitRequest{
		"grant only":   {Device: "dev-cene", Approved: true, Project: fmt.Sprint(f.pid["fakture"])},
		"not approved": {Device: "dev-ana", Project: fmt.Sprint(f.pid["fakture"])},
		"no device":    {Project: fmt.Sprint(f.pid["fakture"])},
		"bad id":       {Device: "dev-ana", Approved: true, Project: "x"},
		"no project":   {Device: "dev-ana", Approved: true, Project: "9999"},
	} {
		var r AdmitReply
		f.api.Call(bg, "admit", in, &r)
		if r.OK || r.Status != 403 || r.Type != "no_project_access" {
			t.Errorf("%s: %+v", name, r)
		}
	}
	var r AdmitReply // no header: nothing to admit, never a refusal
	f.api.Call(bg, "admit", AdmitRequest{Device: "dev-ana"}, &r)
	if !r.OK || r.Project != 0 {
		t.Fatalf("%+v", r)
	}
	// scope: no project = the 0.5.26 key; a project = its own namespace, no collision with a hostile folder
	var s1, s2, s3 ScopeReply
	f.api.Call(bg, "scope", ScopeRequest{Site: "cli", Fields: []string{"sid", "/w|u1|p2|e3"}}, &s1)
	f.api.Call(bg, "scope", ScopeRequest{Site: "cli", Fields: []string{"sid", "/w"}, Project: &ScopeParts{User: 1, Project: 2, Epoch: 3}}, &s2)
	f.api.Call(bg, "scope", ScopeRequest{Site: "agent", Fields: []string{"codex", "o", "/w"}}, &s3)
	if s1.Key != "sid|/w|u1|p2|e3" || s3.Key != "codex|o|/w" || s2.Key == s1.Key || !strings.HasPrefix(s2.Key, "\x00project\x00") {
		t.Fatalf("%q %q %q", s1.Key, s2.Key, s3.Key)
	}
	var h AgentHomeReply
	f.api.Call(bg, "agent-home", AgentHomeRequest{Kind: "codex", User: 1, Project: 2, Epoch: 3}, &h)
	if h.Relative != "proj/1-2-e3/codex" || h.Prefix != "1-2-e" || h.MaxHomes != 20 {
		t.Fatalf("%+v", h)
	}
	// device-revoked raises the epoch; reconcile does the same for a list
	f.api.Call(bg, "device-revoked", DeviceRequest{Device: "dev-ana"}, nil)
	var ep2 EpochReply
	f.api.Call(bg, "epoch", struct{}{}, &ep2)
	var done DevicesReply
	f.api.Call(bg, "reconcile", DevicesRequest{Devices: []string{"d1", "d2"}}, &done)
	var ep3 EpochReply
	f.api.Call(bg, "epoch", struct{}{}, &ep3)
	if ep2.Epoch != ep.Epoch+1 || len(done.Done) != 2 || ep3.Epoch != ep2.Epoch+2 {
		t.Fatalf("epochs %d %d %d, done %v", ep.Epoch, ep2.Epoch, ep3.Epoch, done.Done)
	}
	// ensure-owner creates the row once
	f2 := newFixture(t)
	f2.st.Close()
	st, _ := core.Open(t.TempDir(), core.Options{})
	defer st.Close()
	api := New(Options{Store: func() *core.Store { return st }})
	for i := 0; i < 2; i++ {
		if err := api.Call(bg, "ensure-owner", OwnerRequest{Name: "uros"}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if us, _ := st.ListUsers(bg); len(us) != 1 || us[0].Name != "uros" || !us[0].Owner {
		t.Fatalf("%+v", us)
	}
}

// Events through the internal calls reach the core; the recorder never blocks the caller.
func TestEventCallsReachTheCore(t *testing.T) {
	f := newFixture(t)
	f.api.StartRecorder()
	defer f.api.StopRecorder()
	ev := core.UsageEvent{ID: "e1", Provider: "p", Model: "m", UserID: f.uid["ana"], DeviceID: "dev-ana", Outcome: "ok", TokensIn: 1, TokensOut: 2, TokenSource: "reported"}
	cost := int64(6000000)
	ev.CostUsdNanos, ev.CostSource, ev.UpstreamModel = &cost, "reported", "resolved"
	ev.Start, ev.End = nowUTC(), nowUTC()
	if err := f.api.Call(bg, "event-begin", EventRequest{Event: ev}, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.api.Call(bg, "event-finish", EventRequest{Event: ev}, nil); err != nil {
		t.Fatal(err)
	}
	f.api.FlushRecorder()
	rep, err := f.st.AdminUsage(bg, 7, core.UsageFilter{})
	if err != nil || len(rep.Rows) != 1 || rep.Rows[0].Events != 1 || rep.Rows[0].TokensOut != 2 || rep.Rows[0].ReportedCost == nil || *rep.Rows[0].ReportedCost != cost || rep.Rows[0].UnknownCost != 0 {
		t.Fatalf("%+v %v", rep, err)
	}
	var upstream string
	if err := f.st.DB().QueryRow(`SELECT upstream_model FROM usage_events WHERE id='e1'`).Scan(&upstream); err != nil || upstream != "resolved" {
		t.Fatalf("upstream %s: %v", upstream, err)
	}
	var b BoundReply
	f.api.Call(bg, "bound-user", DeviceRequest{Device: "dev-ana"}, &b)
	if b.User != f.uid["ana"] {
		t.Fatalf("%+v", b)
	}
}

// The Mac app reports events it could not deliver while the helper was down: they show as a gap, and a bad call is refused.
func TestEventLostShowsAsAGap(t *testing.T) {
	f := newFixture(t)
	f.api.StartRecorder()
	defer f.api.StopRecorder()
	a := nowUTC().Add(-time.Minute)
	if err := f.api.Call(bg, "event-lost", LostRequest{Count: 3, From: a, To: nowUTC()}, nil); err != nil {
		t.Fatal(err)
	}
	if err := f.api.Call(bg, "event-lost", LostRequest{Count: 0}, nil); err == nil {
		t.Fatal("a count of 0 must be refused")
	}
	f.api.FlushRecorder()
	rep, err := f.st.AdminUsage(bg, 7, core.UsageFilter{})
	if err != nil || len(rep.Gaps) != 1 || rep.Gaps[0].Count != 3 {
		t.Fatalf("%+v %v", rep.Gaps, err)
	}
}

// A gap that arrives alone (the app quit with events queued, nothing else follows) is stored at once, not at the next event or tick.
func TestEventLostAloneIsStoredWithoutWaitingForAnEvent(t *testing.T) {
	f := newFixture(t)
	f.api.StartRecorder()
	defer f.api.StopRecorder()
	if err := f.api.Call(bg, "event-lost", LostRequest{Count: 2, From: nowUTC().Add(-time.Minute), To: nowUTC()}, nil); err != nil {
		t.Fatal(err)
	}
	for range 100 { // no FlushRecorder, no event: the 30 s tick is far away
		if rep, err := f.st.AdminUsage(bg, 7, core.UsageFilter{}); err == nil && len(rep.Gaps) == 1 && rep.Gaps[0].Count == 2 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the gap was not stored")
}

func nowUTC() time.Time { return time.Now().UTC() }

// Q5: a gap of the adapter has a stable id; a lost answer and a resend are one gap, and the answer comes only once the gap is durable.
func TestEventLostWithAnIDIsIdempotentAndDurable(t *testing.T) {
	f := newFixture(t)
	f.api.StartRecorder()
	defer f.api.StopRecorder()
	req := LostRequest{ID: "gap_0123456789ab", Count: 3, From: nowUTC().Add(-time.Minute), To: nowUTC()}
	for i := 0; i < 3; i++ { // the answer was lost twice: the app sends the same gap again
		if err := f.api.Call(bg, "event-lost", req, nil); err != nil {
			t.Fatal(err)
		}
	}
	// no FlushRecorder: the answer already meant "stored"
	rep, err := f.st.AdminUsage(bg, 7, core.UsageFilter{})
	if err != nil || len(rep.Gaps) != 1 || rep.Gaps[0].Count != 3 {
		t.Fatalf("%+v %v", rep.Gaps, err)
	}
	other := LostRequest{ID: "gap_ba9876543210", Count: 2, From: req.From, To: req.To}
	if err := f.api.Call(bg, "event-lost", other, nil); err != nil {
		t.Fatal(err)
	}
	var n int64
	rep, _ = f.st.AdminUsage(bg, 7, core.UsageFilter{})
	for _, x := range rep.Gaps {
		n += x.Count
	}
	if n != 5 { // a different id is another gap
		t.Fatalf("counted %d: %+v", n, rep.Gaps)
	}
	for _, bad := range []string{"short", "has space in it", "x/../../y1234", string(make([]byte, 65))} {
		if err := f.api.Call(bg, "event-lost", LostRequest{ID: bad, Count: 1}, nil); err == nil {
			t.Errorf("id %q accepted", bad)
		}
	}
	// no recorder: not durable, so not answered
	g := newFixture(t)
	if err := g.api.Call(bg, "event-lost", req, nil); err == nil {
		t.Fatal("answered OK without a recorder")
	}
}

// The core is not usable when the gap arrives: it is spooled (durable), the answer is OK, and the replay into the core keeps it once,
// also when the app sent it again.
func TestEventLostIsSpooledWhenTheCoreIsDownAndCountedOnce(t *testing.T) {
	u, _ := bareRecorder(t)
	u.api.o.Store = func() *core.Store { return nil } // the core cannot be opened
	g := core.UsageGapRecord{ID: "gap_spooled00001", Count: 4, From: nowUTC().Add(-time.Hour), To: nowUTC()}
	for i := 0; i < 2; i++ { // the second is the resend after a lost answer
		m := recMsg{kind: evGap, gap: g, done: make(chan error, 1)}
		u.handle(m)
		if err := <-m.done; err != nil {
			t.Fatal(err)
		}
	}
	if _, gaps := u.spool.load(); len(gaps) == 0 {
		t.Fatal("not in the spool")
	}
	st, err := core.Open(t.TempDir(), core.Options{}) // the core comes back: the replay is by id
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	u.api.o.Store = func() *core.Store { return st }
	u.replay()
	rep, _ := st.AdminUsage(bg, 7, core.UsageFilter{})
	var n int64
	for _, x := range rep.Gaps {
		n += x.Count
	}
	if n != 4 {
		t.Fatalf("counted %d, want 4 once: %+v", n, rep.Gaps)
	}
}

func TestUsageProviderFilterAndCostJSON(t *testing.T) {
	f := newFixture(t)
	zero := int64(0)
	for _, p := range []string{"OpenRouter", "LiteLLM"} {
		e := core.UsageEvent{ID: p, Start: nowUTC(), End: nowUTC(), Provider: p, Model: "same", Outcome: "ok", TokensIn: 0, TokensOut: 0, TokenSource: "reported", CostUsdNanos: &zero, CostSource: "reported"}
		if err := f.st.FinishEvent(bg, e); err != nil {
			t.Fatal(err)
		}
	}
	response := f.api.Handle(bg, Context{Kind: KindAdmin}, Request{Method: "GET", Path: "/usage/events", RawQuery: "days=7&provider=OpenRouter&model=same"})
	var body struct {
		Days     []UsageRowJSON
		Timezone string
	}
	if err := json.Unmarshal(response.Body, &body); err != nil {
		t.Fatal(err)
	}
	if response.Status != 200 || len(body.Days) != 1 || body.Days[0].Provider != "openrouter" || body.Timezone != "UTC" || body.Days[0].ReportedCost == nil || *body.Days[0].ReportedCost != "0.000000000" || body.Days[0].EstimatedCost != nil || body.Days[0].UnknownCost != 0 {
		t.Fatalf("%d %s", response.Status, response.Body)
	}
}

func TestUsageEstimatedCostJSON(t *testing.T) {
	reported, estimated := int64(1000000), int64(2000000)
	rep := core.UsageReport{Rows: []core.UsageRow{{Provider: "LiteLLM", Model: "m", Events: 4,
		ReportedCost: &reported, EstimatedCost: &estimated, EstimatedEvents: 1, SubscriptionEvents: 1, UnknownBillingEvents: 1, UnknownCost: 2}}}
	b, err := json.Marshal(UsageJSON(rep))
	if err != nil {
		t.Fatal(err)
	}
	var body struct{ Days []UsageRowJSON }
	if err = json.Unmarshal(b, &body); err != nil {
		t.Fatal(err)
	}
	r := body.Days[0]
	if r.Cost == nil || *r.Cost != "0.003000000" || r.ReportedCost == nil || *r.ReportedCost != "0.001000000" || r.EstimatedCost == nil || *r.EstimatedCost != "0.002000000" || r.EstimatedEvents != 1 || r.SubscriptionEvents != 1 || r.UnknownBillingEvents != 1 || r.UnknownCost != 2 {
		t.Fatal(string(b))
	}
}
