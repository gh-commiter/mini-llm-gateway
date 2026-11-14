package main

import (
	"bufio"
	"context"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// coreFx is the fixture of section 9 behind a real gateway: projects fakture, eureko (structure grant from fakture),
// import (structure grant from eureko); users ana, bor, fani (admin of fakture), eva (reader), cene (eureko), dana
// (import), one device each, ana has a second one. Requests go through real Noise tunnels with a device proof.
type coreFx struct {
	t   *testing.T
	s   *server
	key *ecdh.PrivateKey
	dev map[string]*device
	uid map[string]int64
	pid map[string]int64
	cfg string
}

var t0 = context.Background()

const noAccessBody = `{"error":{"message":"No project access","type":"no_project_access"}}`

func devID(d *device) string { return base64url(d.key.Public().(ed25519.PublicKey)) }

func newCoreFx(t *testing.T, mod func(*serveConfig)) *coreFx {
	t.Helper()
	f := &coreFx{t: t, dev: map[string]*device{}, uid: map[string]int64{}, pid: map[string]int64{}}
	var list []serveDevice
	for _, n := range []string{"ana", "ana2", "bor", "fani", "eva", "cene", "dana"} {
		f.dev[n] = newDevice(n)
		list = append(list, serveDevice{ID: devID(f.dev[n]), Name: n})
	}
	f.s, f.key = testServer(t, func(c *serveConfig) {
		c.Devices = list
		if mod != nil {
			mod(c)
		}
	})
	f.cfg = f.s.cfgPath
	f.s.openCore()
	t.Cleanup(f.s.closeCore)
	if f.s.core == nil {
		t.Fatal(f.s.coreErr)
	}
	f.setup()
	return f
}

func (f *coreFx) ok(err error) {
	f.t.Helper()
	if err != nil {
		f.t.Fatal(err)
	}
}

func (f *coreFx) setup() {
	c, ctx := f.s.core, t0
	for _, n := range []string{"ana", "bor", "fani", "eva", "cene", "dana"} {
		u, err := c.CreateUser(ctx, n)
		f.ok(err)
		f.uid[n] = u.ID
		f.ok(c.SetBinding(ctx, devID(f.dev[n]), u.ID))
	}
	f.ok(c.SetBinding(ctx, devID(f.dev["ana2"]), f.uid["ana"]))
	for _, n := range []string{"fakture", "eureko", "import"} {
		p, err := c.CreateProject(ctx, n, n)
		f.ok(err)
		f.pid[n] = p.ID
	}
	for u, m := range map[string]struct {
		p string
		r core.Role
	}{"ana": {"fakture", core.Developer}, "bor": {"fakture", core.Developer}, "fani": {"fakture", core.Admin},
		"eva": {"fakture", core.Reader}, "cene": {"eureko", core.Developer}, "dana": {"import", core.Developer}} {
		f.ok(c.SetMember(ctx, f.pid[m.p], f.uid[u], m.r))
	}
	f.ok(c.AddGrant(ctx, f.pid["fakture"], f.pid["eureko"], core.Structure))
	f.ok(c.AddGrant(ctx, f.pid["eureko"], f.pid["import"], core.Structure))
}

type resp struct {
	status int
	body   string
	h      http.Header
}

// raw sends one HTTP request through a Noise tunnel; dev nil = no device proof; token "" = none.
func (f *coreFx) raw(dev *device, token, method, path, body string, hdr ...string) resp {
	f.t.Helper()
	req := method + " " + path + " HTTP/1.1\r\nHost: x\r\n"
	if token != "" {
		req += "Authorization: Bearer " + token + "\r\n"
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req += hdr[i] + ": " + hdr[i+1] + "\r\n"
	}
	req += fmt.Sprintf("Content-Length: %d\r\n\r\n%s", len(body), body)
	out := pipeRequest(f.t, f.s, f.key.PublicKey().Bytes(), dev, req)
	r, err := http.ReadResponse(bufio.NewReader(strings.NewReader(out)), nil)
	if err != nil {
		f.t.Fatalf("%s %s: %q: %v", method, path, out, err)
	}
	b, _ := io.ReadAll(r.Body)
	return resp{r.StatusCode, string(b), r.Header}
}

func (f *coreFx) call(user, method, path, body string, hdr ...string) resp {
	return f.raw(f.dev[user], "tok", method, path, body, hdr...)
}

var seq int

func (f *coreFx) create(user, project, class string) (id int64, rev int64) {
	f.t.Helper()
	seq++
	r := f.call(user, "POST", fmt.Sprintf("%s/%d/notes", projectsPrefix, f.pid[project]), fmt.Sprintf(`{"class":%q,"title":"t","body":"b"}`, class),
		"Idempotency-Key", fmt.Sprint("k", seq))
	if r.status != 201 {
		f.t.Fatalf("create %s %s %s: %d %s", user, project, class, r.status, r.body)
	}
	var n struct{ ID, Revision int64 }
	json.Unmarshal([]byte(r.body), &n)
	return n.ID, n.Revision
}

func (f *coreFx) notesURL(project string) string {
	return fmt.Sprintf("%s/%d/notes", projectsPrefix, f.pid[project])
}

func (f *coreFx) listIDs(user, project, query string) map[int64]bool {
	f.t.Helper()
	r := f.call(user, "GET", f.notesURL(project)+query, "")
	if r.status != 200 {
		f.t.Fatalf("list %s %s: %d %s", user, project, r.status, r.body)
	}
	var l struct {
		Notes []struct{ ID int64 }
		Next  string
	}
	json.Unmarshal([]byte(r.body), &l)
	m := map[int64]bool{}
	for _, n := range l.Notes {
		m[n.ID] = true
	}
	return m
}

func status(t *testing.T, r resp, want int) {
	t.Helper()
	if r.status != want {
		t.Fatalf("status %d, want %d: %s", r.status, want, r.body)
	}
}

func errType2(r resp) string {
	var e struct{ Error struct{ Type string } }
	json.Unmarshal([]byte(r.body), &e)
	return e.Error.Type
}

func TestCoreCase1AnaWritesBorReads(t *testing.T) {
	f := newCoreFx(t, nil)
	s, _ := f.create("ana", "fakture", "structure")
	d, _ := f.create("ana", "fakture", "data")
	for _, id := range []int64{s, d} {
		r := f.call("bor", "GET", fmt.Sprintf("%s/%d", f.notesURL("fakture"), id), "")
		status(t, r, 200)
		if !strings.Contains(r.body, `"body":"b"`) || !strings.Contains(r.body, `"author":"ana"`) {
			t.Fatal(r.body)
		}
	}
	r := f.call("ana", "GET", projectsPrefix, "") // a second device of ana is the same person
	status(t, r, 200)
	r2 := f.raw(f.dev["ana2"], "tok", "GET", projectsPrefix, "")
	if r.body != r2.body {
		t.Fatalf("ana's devices differ: %s / %s", r.body, r2.body)
	}
}

func TestCoreCase3And4GrantsAndStrangers(t *testing.T) {
	f := newCoreFx(t, nil)
	s, _ := f.create("ana", "fakture", "structure")
	d, _ := f.create("ana", "fakture", "data")
	got := f.listIDs("cene", "fakture", "")
	if !got[s] || got[d] || len(got) != 1 {
		t.Fatalf("cene lists %v", got)
	}
	status(t, f.call("cene", "GET", fmt.Sprintf("%s/%d", f.notesURL("fakture"), d), ""), 404)
	r := f.call("cene", "GET", projectsPrefix+"/fakture", "")
	status(t, r, 200)
	if !strings.Contains(r.body, `"granted":["structure"]`) || strings.Contains(r.body, `"role"`) {
		t.Fatal(r.body)
	}
	var page map[string]any
	json.Unmarshal([]byte(f.call("cene", "GET", f.notesURL("fakture"), "").body), &page)
	if _, has := page["total"]; has || len(page) > 2 { // no totals
		t.Fatalf("keys %v", page)
	}
	// dana (import) has no way to fakture: everything is 404 with the same body, also for things that do not exist
	var bodies []string
	for _, path := range []string{
		projectsPrefix + "/fakture", fmt.Sprintf("%s/%d", projectsPrefix, f.pid["fakture"]), f.notesURL("fakture"),
		fmt.Sprintf("%s/%d", f.notesURL("fakture"), s), fmt.Sprintf("%s/%d", f.notesURL("fakture"), 999999),
		projectsPrefix + "/no-such-project", fmt.Sprintf("%s/%d/notes", projectsPrefix, 999999),
	} {
		r := f.call("dana", "GET", path, "")
		status(t, r, 404)
		bodies = append(bodies, r.body)
	}
	for _, b := range bodies {
		if b != bodies[0] {
			t.Fatalf("404 bodies differ: %q / %q", b, bodies[0])
		}
	}
	// no chaining: dana reads eureko's structure through her own grant, not fakture's through eureko's
	status(t, f.call("dana", "GET", f.notesURL("eureko"), ""), 200)
	// missing note and note of a visible project that is not visible to the caller look the same
	mine, _ := f.create("ana", "fakture", "private")
	a := f.call("bor", "GET", fmt.Sprintf("%s/%d", f.notesURL("fakture"), mine), "")
	b := f.call("bor", "GET", fmt.Sprintf("%s/%d", f.notesURL("fakture"), 999999), "")
	if a.status != 404 || a.body != b.body {
		t.Fatalf("private vs missing: %s / %s", a.body, b.body)
	}
}

func TestCoreCase5Private(t *testing.T) {
	f := newCoreFx(t, nil)
	id, _ := f.create("ana", "fakture", "private")
	for _, u := range []string{"bor", "cene"} {
		status(t, f.call(u, "GET", fmt.Sprintf("%s/%d", f.notesURL("fakture"), id), ""), 404)
	}
	status(t, f.call("fani", "GET", fmt.Sprintf("%s/%d", f.notesURL("fakture"), id), ""), 200)
	if !f.listIDs("fani", "fakture", "")[id] || f.listIDs("bor", "fakture", "")[id] {
		t.Fatal("list visibility")
	}
}

func TestCoreCase8Revisions(t *testing.T) {
	f := newCoreFx(t, nil)
	url := func(id int64) string { return fmt.Sprintf("%s/%d", f.notesURL("fakture"), id) }
	mine, rev := f.create("bor", "fakture", "data")
	put := func(user string, id, rev int64) resp {
		return f.call(user, "PUT", url(id), fmt.Sprintf(`{"revision":%d,"class":"data","title":"n","body":"b"}`, rev))
	}
	status(t, put("bor", mine, rev), 200)
	r := put("bor", mine, rev) // stale
	status(t, r, 409)
	var e struct {
		Error struct {
			Type     string
			Revision int64
		}
	}
	json.Unmarshal([]byte(r.body), &e)
	if e.Error.Type != "revision_mismatch" || e.Error.Revision != 2 {
		t.Fatal(r.body)
	}
	status(t, f.call("bor", "DELETE", url(mine)+"?revision=1", ""), 409)
	theirs, _ := f.create("ana", "fakture", "data")
	for _, rv := range []int64{1, 2, 77} { // any revision: 403 with no revision in the body
		r := put("bor", theirs, rv)
		status(t, r, 403)
		if strings.Contains(r.body, "revision") && errType2(r) != "forbidden" || strings.Contains(r.body, `"revision":`) {
			t.Fatal("revision leaked: " + r.body)
		}
		status(t, put("dana", theirs, rv), 404)
	}
	status(t, f.call("bor", "DELETE", url(mine)+"?revision=2", ""), 204)
	status(t, f.call("bor", "DELETE", url(theirs)+"?revision=x", ""), 403) // may not delete it: 403 before the revision is looked at
}

func TestCoreCase9Identity(t *testing.T) {
	f := newCoreFx(t, nil)
	models := func(dev *device) resp { return f.raw(dev, "tok", "GET", "/v1/models", "") }
	denied := func(name string, r resp) {
		t.Helper()
		if r.status != 403 || r.body != noAccessBody {
			t.Errorf("%s: %d %s", name, r.status, r.body)
		}
	}
	ana := f.dev["ana"]
	status(t, f.raw(ana, "tok", "GET", projectsPrefix, ""), 200)

	// a device on the list without a binding
	stranger := newDevice("stranger")
	f.s.update(func(c *serveConfig) error {
		c.Devices = append(c.Devices, serveDevice{ID: devID(stranger), Name: "stranger"})
		return nil
	})
	denied("unbound", f.raw(stranger, "tok", "GET", projectsPrefix, ""))
	status(t, models(stranger), 200) // models still answer

	// deactivated user
	f.ok(f.s.core.SetUserActive(t0, f.uid["eva"], false))
	denied("deactivated", f.call("eva", "GET", projectsPrefix, ""))
	status(t, models(f.dev["eva"]), 200)

	// no token, wrong token
	denied("no token", f.raw(ana, "", "GET", projectsPrefix, ""))
	denied("wrong token", f.raw(ana, "nope", "GET", projectsPrefix, ""))
	// a tunnel without a device proof
	denied("no device", f.raw(nil, "tok", "GET", projectsPrefix, ""))

	// removed from the approved list: the bound device is refused, models too (the usual device refusal)
	if st, _ := f.s.adminDo("DELETE", "/devices/"+devID(f.dev["bor"]), nil); st != 200 {
		t.Fatal(st)
	}
	denied("removed", f.call("bor", "GET", projectsPrefix, ""))
	// a re-approved device is still bound and works again (old runs stay dead through the epoch)
	f.s.adminDo("POST", "/devices", []byte(fmt.Sprintf(`{"id":%q,"name":"bor"}`, devID(f.dev["bor"]))))
	status(t, f.call("bor", "GET", projectsPrefix, ""), 200)

	// loopback without a device, plain HTTP: no principal. The model route answers with the token.
	c := startLoopback(t, f.s)
	get := func(path string, hdr ...string) resp {
		rq, _ := http.NewRequest("GET", c.URL+path, nil)
		for i := 0; i+1 < len(hdr); i += 2 {
			rq.Header.Set(hdr[i], hdr[i+1])
		}
		r, err := http.DefaultClient.Do(rq)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		return resp{r.StatusCode, string(b), r.Header}
	}
	denied("loopback, token, no device", get(projectsPrefix, "Authorization", "Bearer tok"))
	status(t, get("/v1/models", "Authorization", "Bearer tok"), 200)
	// a local app without a token: allowed for models, never for project routes
	f.s.update(func(c *serveConfig) error { yes := true; c.AllowLocalApps = &yes; return nil })
	localPeer = func(_, _ *net.TCPAddr) bool { return true }
	defer func() { localPeer = sameUserPeer }()
	status(t, get("/v1/models", "anthropic-version", "2023-06-01"), 200)
	denied("local app", get(projectsPrefix, "anthropic-version", "2023-06-01"))
}

func TestCoreCase9AllowAnyDevice(t *testing.T) {
	f := newCoreFx(t, func(c *serveConfig) { c.Devices = nil; c.AllowAnyDevice = true })
	any := newDevice("any")
	f.ok(f.s.core.SetBinding(t0, devID(any), f.uid["ana"])) // bound, but not in the (empty) approved list
	r := f.raw(any, "tok", "GET", projectsPrefix, "")
	if r.status != 403 || r.body != noAccessBody {
		t.Fatalf("%d %s", r.status, r.body)
	}
	status(t, f.raw(any, "tok", "GET", "/v1/models", ""), 200) // the model route accepts it
}

func TestCoreRevocationBarrier(t *testing.T) { // case 10
	// An operation is paused after the gateway admitted it (principal derived, device approved) and before the core
	// authorizes it. The revocation commits and its admin call answers. After the release:
	//   device removed: the operation was admitted before, it finishes (200);
	//   binding, user, membership, grant: the core authorizes from the state of its own transaction, so it fails;
	// and every request that starts after the admin call has answered fails.
	cases := []struct {
		name       string
		user, path string
		revoke     func(f *coreFx)
		paused     int // status of the paused operation
		after      int // status of a request started after the revocation
	}{
		{"device removed", "bor", "fakture", func(f *coreFx) { f.s.adminDo("DELETE", "/devices/"+devID(f.dev["bor"]), nil) }, 200, 403},
		{"binding changed", "bor", "fakture", func(f *coreFx) {
			st, _ := f.s.adminDo("PUT", "/bindings/"+devID(f.dev["bor"]), []byte(fmt.Sprintf(`{"user":%d}`, f.uid["dana"])))
			if st != 200 {
				t.Fatal(st)
			}
		}, 403, 404}, // after: the device is now dana's, who has no access to fakture
		{"user deactivated", "bor", "fakture", func(f *coreFx) {
			f.s.adminDo("PATCH", fmt.Sprintf("/users/%d", f.uid["bor"]), []byte(`{"active":false}`))
		}, 403, 403},
		{"membership removed", "bor", "fakture", func(f *coreFx) {
			f.s.adminDo("DELETE", fmt.Sprintf("/projects/%d/members/%d", f.pid["fakture"], f.uid["bor"]), nil)
		}, 404, 404},
		{"grant removed", "cene", "fakture", func(f *coreFx) {
			f.s.adminDo("DELETE", fmt.Sprintf("/grants?source=%d&reader=%d&class=structure", f.pid["fakture"], f.pid["eureko"]), nil)
		}, 404, 404},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newCoreFx(t, nil)
			f.create("ana", "fakture", "structure")
			paused, release := make(chan struct{}), make(chan struct{})
			var once sync.Once
			f.s.coreHook = func(r *req, point string) {
				if r.h("X-Test-Pause") == "1" {
					once.Do(func() { close(paused) })
					<-release
				}
			}
			done := make(chan resp)
			go func() { done <- f.call(c.user, "GET", f.notesURL(c.path), "", "X-Test-Pause", "1") }()
			<-paused
			c.revoke(f) // the admin call has answered when this returns
			close(release)
			select {
			case r := <-done:
				if r.status != c.paused {
					t.Errorf("paused operation: %d %s, want %d", r.status, r.body, c.paused)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("timeout")
			}
			if r := f.call(c.user, "GET", f.notesURL(c.path), ""); r.status != c.after {
				t.Errorf("started after: %d %s, want %d", r.status, r.body, c.after)
			}
		})
	}
}

func TestCoreCase18Api(t *testing.T) {
	f := newCoreFx(t, nil)
	n := f.notesURL("fakture")
	post := func(key, body string) resp {
		return f.call("ana", "POST", n, body, "Idempotency-Key", key)
	}
	for name, r := range map[string]resp{
		"invalid JSON":     post("k1", `{"class":`),
		"unknown field":    post("k2", `{"class":"data","title":"t","bogus":1}`),
		"invalid class":    post("k3", `{"class":"nope","title":"t"}`),
		"no key":           f.call("ana", "POST", n, `{"class":"data","title":"t"}`),
		"key too long":     post(strings.Repeat("k", 65), `{"class":"data","title":"t"}`),
		"empty title":      post("k4", `{"class":"data","title":" "}`),
		"bad cursor":       f.call("ana", "GET", n+"?after=zzz", ""),
		"bad limit":        f.call("ana", "GET", n+"?limit=1000", ""),
		"limit not int":    f.call("ana", "GET", n+"?limit=x", ""),
		"bad class query":  f.call("ana", "GET", n+"?class=x", ""),
		"missing revision": f.call("ana", "PUT", fmt.Sprintf("%s/%d", n, noteOf(f, "ana")), `{"class":"data","title":"t"}`),
	} {
		if r.status != 400 || errType2(r) != "invalid" {
			t.Errorf("%s: %d %s", name, r.status, r.body)
		}
	}
	// idempotency
	r1 := post("same", `{"class":"data","title":"t","body":"b"}`)
	r2 := post("same", `{"class":"data","title":"t","body":"b"}`)
	status(t, r1, 201)
	status(t, r2, 201)
	var a, b struct{ ID int64 }
	json.Unmarshal([]byte(r1.body), &a)
	json.Unmarshal([]byte(r2.body), &b)
	if a.ID != b.ID || r2.h.Get("Idempotent-Replay") != "true" || r1.h.Get("Idempotent-Replay") != "" {
		t.Fatalf("replay: %s / %s", r1.body, r2.body)
	}
	r3 := post("same", `{"class":"data","title":"other"}`)
	if r3.status != 409 || errType2(r3) != "idempotency_conflict" {
		t.Fatalf("%d %s", r3.status, r3.body)
	}
	// sizes: 65 KiB body is refused by the core; a body over the request limit before it is parsed
	r4 := post("big", `{"class":"data","title":"t","body":"`+strings.Repeat("x", 65<<10)+`"}`)
	if r4.status != 413 || errType2(r4) != "too_large" {
		t.Fatalf("%d %s", r4.status, r4.body[:min(80, len(r4.body))])
	}
	r5 := post("huge", `{"class":"data","title":"t","body":"`+strings.Repeat("x", 600<<10)+`"}`)
	if r5.status != 413 || errType2(r5) != "too_large" {
		t.Fatalf("%d %s", r5.status, r5.body[:min(80, len(r5.body))])
	}
	// the write limit: 60 a minute; the answer carries Retry-After
	var limited resp
	for i := 0; i < 70 && limited.status == 0; i++ {
		if r := post(fmt.Sprint("rl", i), `{"class":"data","title":"t"}`); r.status == 429 {
			limited = r
		}
	}
	if limited.status != 429 || errType2(limited) != "limit" || limited.h.Get("Retry-After") == "" {
		t.Fatalf("limit: %d %v %s", limited.status, limited.h, limited.body)
	}
	// a repeated listing across pages
	for i := 0; i < 3; i++ {
		f.create("bor", "fakture", "data")
	}
	r := f.call("bor", "GET", n+"?limit=2", "")
	var pg struct {
		Notes []struct{ ID int64 }
		Next  string
	}
	json.Unmarshal([]byte(r.body), &pg)
	if r.status != 200 || len(pg.Notes) != 2 || pg.Next == "" {
		t.Fatalf("%d %s", r.status, r.body)
	}
	if r := f.call("ana", "GET", n+"?limit=2&after="+pg.Next, ""); r.status != 400 { // another user's cursor
		t.Fatalf("foreign cursor: %d", r.status)
	}
}

func TestCoreCase23AdminRoutes(t *testing.T) {
	s, _, base := coreLiveServer(t)
	host := strings.TrimPrefix(base, "http://")
	if st, b := adminNoise(t, s, base, s.admin, "GET", adminPrefix+"/users", ""); st != 200 || !strings.Contains(b, `"owner":true`) {
		t.Fatalf("with the proof: %d %s", st, b)
	}
	// no proof, the secret in the clear, the gateway token, a wrong proof
	for name, auth := range map[string]string{"no auth": "", "bearer secret": s.admin, "bearer token": "tok"} {
		if got, _ := adminReq(t, base, "", auth, "GET", adminPrefix+"/users", ""); got != 401 {
			t.Errorf("%s: %d", name, got)
		}
	}
	if st, _ := adminNoise(t, s, base, s.admin+"x", "GET", adminPrefix+"/users", ""); st != 401 {
		t.Errorf("wrong proof: %d", st)
	}
	// an agent descendant of the gateway, with a valid proof
	adminPeerCheckFn = func(net.Conn) error { return errAgentPeer }
	defer func() { adminPeerCheckFn = adminPeerCheck }()
	for _, p := range []struct{ m, path, body string }{{"GET", "/users", ""}, {"POST", "/users", `{"name":"x"}`}, {"POST", "/core/backup", `{"file":"/x"}`}} {
		if st, _ := adminNoise(t, s, base, s.admin, p.m, adminPrefix+p.path, p.body); st != 403 {
			t.Errorf("agent descendant %s %s: %d", p.m, p.path, st)
		}
	}
	adminPeerCheckFn = adminPeerCheck
	// a member's tunnel (a device on another computer), even with the real secret in the clear
	d := newDevice("member")
	r := pipeRequest(t, s, s.key.PublicKey().Bytes(), d, "GET "+adminPrefix+"/users HTTP/1.1\r\nHost: "+host+"\r\nAuthorization: Bearer "+s.admin+"\r\n\r\n")
	if !strings.HasPrefix(r, "HTTP/1.1 403") {
		t.Errorf("member tunnel: %q", r)
	}
	// the member routes give the owner user no power: a bound owner device sees no project it is not a member of
	users, _ := s.core.ListUsers(t0)
	if len(users) != 1 || !users[0].Owner {
		t.Fatalf("owner row: %+v", users)
	}
	s.core.CreateProject(t0, "secret", "secret")
	s.core.SetBinding(t0, devID(d), users[0].ID)
	s.update(func(c *serveConfig) error {
		c.Devices = append(c.Devices, serveDevice{ID: devID(d), Name: "member"})
		return nil
	})
	f := &coreFx{t: t, s: s, key: s.key}
	if r := f.raw(d, "tok", "GET", projectsPrefix, ""); r.status != 200 || r.body != "[]" {
		t.Errorf("owner user's projects: %d %s", r.status, r.body)
	}
	if r := f.raw(d, "tok", "GET", adminPrefix+"/users", ""); r.status != 403 {
		t.Errorf("owner device on the admin route: %d", r.status)
	}
}

func TestCoreUnavailableStillStarts(t *testing.T) {
	f := &coreFx{t: t, dev: map[string]*device{}}
	f.dev["a"] = newDevice("a")
	f.s, f.key = testServer(t, func(c *serveConfig) { c.Devices = []serveDevice{{ID: devID(f.dev["a"]), Name: "a"}} })
	var logs strings.Builder
	f.s.log.SetOutput(&logs)
	// the core folder cannot be created: a file is in its place
	if err := os.WriteFile(filepath.Join(f.s.cfgDir, coreDirName), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	f.s.openCore()
	if f.s.core != nil || f.s.coreErr == nil || strings.Count(logs.String(), "WARNING") != 1 {
		t.Fatalf("core %v err %v log %q", f.s.core, f.s.coreErr, logs.String())
	}
	r := f.raw(f.dev["a"], "tok", "GET", projectsPrefix, "")
	if r.status != 503 || errType2(r) != "core_unavailable" {
		t.Fatalf("%d %s", r.status, r.body)
	}
	status(t, f.raw(f.dev["a"], "tok", "GET", "/v1/models", ""), 200)
	if st, _ := f.s.adminDo("GET", "/users", nil); st != 503 {
		t.Fatalf("admin: %d", st)
	}
	// the offline server (minigw gw without a gateway) never opens the store
	off, err := offlineServer(f.s.cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if off.core != nil {
		t.Fatal("offline server has a core")
	}
	if st, _ := off.adminDo("GET", "/users", nil); st != 503 {
		t.Fatalf("offline admin: %d", st)
	}
}

func TestCoreNewerSchemaStillStarts(t *testing.T) {
	// a database from a newer program: refused, the gateway starts, project routes are 503
	f := newCoreFx(t, nil)
	dir := filepath.Join(f.s.cfgDir, coreDirName)
	f.s.closeCore()
	db := filepath.Join(dir, "core.db")
	if _, err := os.Stat(db); err != nil {
		t.Fatal(err)
	}
	raw, err := sql.Open("sqlite", db) // the driver the core package registered
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`UPDATE meta SET value='999' WHERE key='schema'`); err != nil {
		t.Fatal(err)
	}
	raw.Close()
	s2, _ := testServer(t, nil)
	s2.cfgDir = f.s.cfgDir
	s2.openCore()
	if s2.core != nil || !errors.Is(s2.coreErr, core.ErrNewerSchema) {
		t.Fatalf("err %v", s2.coreErr)
	}
}

func TestCoreCLI(t *testing.T) {
	s, cfg, _ := coreLiveServer(t)
	run := func(args ...string) string {
		return stdout(t, func() { routerCommand(append(args, "--config", cfg), strings.NewReader("")) })
	}
	run("user", "add", "ana")
	run("project", "add", "fakture", "Fakture")
	run("member", "fakture", "set", "ana", "developer")
	id := devID(newDevice("x"))
	run("bind", id, "ana")
	for _, want := range [][]string{{"user", "list"}, {"project", "list"}, {"member", "fakture", "list"}, {"bind", "list"}} {
		out := run(want...)
		if !strings.Contains(out, "ana") && !strings.Contains(out, "fakture") && !strings.Contains(out, id) {
			t.Errorf("%v: %q", want, out)
		}
	}
	run("user", "deactivate", "ana")
	if us, _ := s.core.ListUsers(t0); !(len(us) == 2 && !us[1].Active) {
		t.Fatalf("%+v", us)
	}
	if out := run("core", "backup", "cli.db"); !strings.Contains(out, "backup written") {
		t.Fatal(out)
	}
	if out := run("core", "audit"); !strings.Contains(out, "user.deactivate") && !strings.Contains(out, "user.active") {
		t.Fatalf("audit: %q", out)
	}
}

func TestCoreRestoreNeedsTheGatewayStopped(t *testing.T) {
	s, cfg, _ := coreLiveServer(t)
	run := func(args ...string) string {
		return stdout(t, func() { routerCommand(append(args, "--config", cfg), strings.NewReader("")) })
	}
	run("user", "add", "ana")
	if out := run("core", "backup", "b.db"); !strings.Contains(out, filepath.Join(s.cfgDir, coreDirName, "backups", "b.db")) {
		t.Fatalf("backup: %q", out)
	}
	dest := "b.db" // restore finds a name in the protected folder
	run("user", "add", "bor")
	if err := core.Restore(filepath.Join(s.cfgDir, coreDirName), filepath.Join(s.cfgDir, coreDirName, "backups", dest)); !errors.Is(err, core.ErrLocked) {
		t.Fatalf("restore with the gateway running: %v", err)
	}
	s.closeCore() // the gateway stops
	if out := run("core", "restore", dest, "--yes"); !strings.Contains(out, "also ones revoked since") || !strings.Contains(out, "restored") {
		t.Fatalf("restore: %q", out)
	}
	st, err := core.Open(filepath.Join(s.cfgDir, coreDirName), core.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	us, _ := st.ListUsers(t0)
	if len(us) != 2 { // owner and ana: bor was added after the backup
		t.Fatalf("%+v", us)
	}
}

func TestCoreDeviceRevokedBumpsEpoch(t *testing.T) {
	f := newCoreFx(t, nil)
	before, _ := f.s.core.Epoch(t0)
	if st, _ := f.s.adminDo("DELETE", "/devices/"+devID(f.dev["eva"]), nil); st != 200 {
		t.Fatal(st)
	}
	after, _ := f.s.core.Epoch(t0)
	if after <= before {
		t.Fatal("epoch did not grow")
	}
	bs, _ := f.s.core.ListBindings(t0)
	found := false
	for _, b := range bs {
		found = found || b.DeviceID == devID(f.dev["eva"]) // the binding stays
	}
	if !found {
		t.Fatal("binding deleted with the device")
	}
	// a pending (never approved) device refused through the same route does not bump
	stranger := newDevice("s")
	f.s.addPending(serveDevice{ID: devID(stranger), Name: "s"}, "1.2.3.4", "")
	e := after
	f.s.adminDo("DELETE", "/devices/"+devID(stranger), nil)
	if again, _ := f.s.core.Epoch(t0); again != e {
		t.Fatal("pending refusal bumped the epoch")
	}
}

func coreLiveServer(t *testing.T) (*server, string, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	k, _ := ecdh.X25519().GenerateKey(rand.Reader)
	c := serveConfig{Token: "tok", Key: base64url(k.Bytes()), Port: ln.Addr().(*net.TCPAddr).Port, Tools: defaultTools,
		PermissionMode: "dontAsk", Scratch: t.TempDir(), Claude: anyClaude(t), Devices: []serveDevice{}}
	t.Cleanup(releaseScratch)
	cfg := filepath.Join(t.TempDir(), "serve.json")
	saveServeConfig(cfg, c)
	s, err := newServer(c, cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.log.SetOutput(io.Discard)
	s.openCore()
	t.Cleanup(s.closeCore)
	go s.serve(ln)
	return s, cfg, fmt.Sprintf("http://127.0.0.1:%d", c.Port)
}

func noteOf(f *coreFx, user string) int64 {
	id, _ := f.create(user, "fakture", "data")
	return id
}

func TestCoreVisibilityBeforePayloadChecks(t *testing.T) { // review A6
	f := newCoreFx(t, nil)
	mine, _ := f.create("ana", "fakture", "data")
	n := func(id int64) string { return fmt.Sprintf("%s/%d", f.notesURL("fakture"), id) }
	huge := `{"revision":1,"class":"data","title":"t","body":"` + strings.Repeat("x", 600<<10) + `"}`
	bad := map[string]string{"too large": huge, "malformed": `{"revision":`, "no revision": `{"class":"data","title":"t"}`, "bad class": `{"revision":1,"class":"nope","title":"t"}`,
		"unknown field": `{"revision":1,"class":"data","title":"t","x":1}`}
	for name, body := range bad {
		if r := f.call("dana", "PUT", n(mine), body); r.status != 404 { // dana may not see it
			t.Errorf("%s as a stranger: %d %s", name, r.status, r.body)
		}
		if r := f.call("dana", "PUT", n(999999), body); r.status != 404 { // a note that does not exist
			t.Errorf("%s on a missing note: %d", name, r.status)
		}
		if r := f.call("bor", "PUT", n(mine), body); r.status != 403 { // visible, may not change it
			t.Errorf("%s as a reader of it: %d %s", name, r.status, r.body)
		}
		if r := f.call("ana", "PUT", n(mine), body); r.status != 400 && r.status != 413 { // the author: the payload error
			t.Errorf("%s as the author: %d %s", name, r.status, r.body)
		}
	}
	for _, p := range []string{"?revision=x", "", "?revision="} {
		if r := f.call("dana", "DELETE", n(mine)+p, ""); r.status != 404 {
			t.Errorf("DELETE %q as a stranger: %d", p, r.status)
		}
		if r := f.call("ana", "DELETE", n(mine)+p, ""); r.status != 400 {
			t.Errorf("DELETE %q as the author: %d", p, r.status)
		}
	}
}

// Review A7: a device removal is durable (a pending mark next to the device list) whatever the core is doing, and the
// next core open does the epoch bump before the project routes are enabled.
func TestDeviceRemovalIsDurableAndReconciledAtTheNextOpen(t *testing.T) {
	dev := newDevice("gone")
	id := devID(dev)
	newSrv := func(t *testing.T) *server {
		s, _ := testServer(t, func(c *serveConfig) { c.Devices = []serveDevice{{ID: id, Name: "gone"}} })
		return s
	}
	restart := func(t *testing.T, path string) *server {
		c, err := loadServeConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		s, err := newServer(c, path)
		if err != nil {
			t.Fatal(err)
		}
		s.log.SetOutput(io.Discard)
		s.openCore()
		t.Cleanup(s.closeCore)
		if s.core == nil {
			t.Fatal(s.coreErr)
		}
		return s
	}
	pending := func(t *testing.T, path string) []string {
		c, err := loadServeConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		return c.PendingRevocations
	}
	for _, name := range []string{"offline (minigw gw without a gateway)", "core unavailable", "epoch write fails"} {
		t.Run(name, func(t *testing.T) {
			s := newSrv(t)
			path := s.cfgPath
			base := restart(t, path) // creates the core; its epoch is the one to beat
			e0, _ := base.core.Epoch(t0)
			var st int
			var body string
			del := func(x *server) {
				code, obj := x.adminDo("DELETE", "/devices/"+id, nil)
				st, body = code, fmt.Sprint(obj)
			}
			switch name {
			case "core unavailable": // another process holds the core: this gateway runs without it
				s.openCore()
				if s.core != nil {
					t.Fatal("the core opened")
				}
				del(s)
				base.closeCore()
			case "epoch write fails":
				base.closeCore()
				s.openCore()
				if s.core == nil {
					t.Fatal(s.coreErr)
				}
				s.core.Close() // every write now fails
				del(s)
			default:
				base.closeCore()
				off, err := offlineServer(path)
				if err != nil {
					t.Fatal(err)
				}
				del(off)
			}
			if want := map[bool]int{true: 500, false: 200}[name == "epoch write fails"]; st != want {
				t.Fatalf("%d %s", st, body)
			}
			if got := pending(t, path); len(got) != 1 || got[0] != id { // durable before the call answered
				t.Fatalf("pending revocations: %v", got)
			}
			if c, _ := loadServeConfig(path); len(c.Devices) != 0 {
				t.Fatal("the device is still listed")
			}
			s.api.StopRecorder()
			s2 := restart(t, path)
			e1, _ := s2.core.Epoch(t0)
			if e1 <= e0 || len(pending(t, path)) != 0 {
				t.Fatalf("epoch %d -> %d, pending %v", e0, e1, pending(t, path))
			}
		})
	}
}

func TestRestoreOutcomeTellsTheRealState(t *testing.T) { // review (should-fix)
	if m, failed := restoreOutcome(fmt.Errorf("x: %w", core.ErrRestoreApplied)); failed || !strings.Contains(m, "APPLIED") || strings.Contains(m, "previous database is in place") {
		t.Fatalf("applied: %q %v", m, failed)
	}
	if m, failed := restoreOutcome(errors.New("not a core database")); !failed || !strings.Contains(m, "NOT applied") || !strings.Contains(m, "previous database is in place") {
		t.Fatalf("not applied: %q %v", m, failed)
	}
	if m, failed := restoreOutcome(core.ErrLocked); !failed || !strings.Contains(m, "Nothing was changed") {
		t.Fatalf("locked: %q", m)
	}
	if m, failed := restoreOutcome(nil); failed || m != "" {
		t.Fatal("success")
	}
}

// The gateway's own fixed 403 is the same body as coreapi's (and as the Mac app's constant): one fixture.
func TestNoProjectAccessBodyMatchesTheFixture(t *testing.T) {
	want, err := os.ReadFile("coreapi/testdata/no_project_access.json")
	if err != nil {
		t.Fatal(err)
	}
	if got := noProjectAccess().Body; string(got) != string(want) {
		t.Fatalf("noProjectAccess body %s, fixture %s", got, want)
	}
}
