package main

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"minigw/core"
	"minigw/coreapi"
)

// upFx: two real gateways of this binary on loopback (their real member and admin routes, real encrypted connections): the local
// one, whose owner runs the upstream commands, and the "remote" one (the upstream) with project shop and a user gw-mini.
type upFx struct {
	t        *testing.T
	local    *server
	remote   *server
	keys     map[string]string
	addr     string // the upstream's address as the owner types it
	token    string // the upstream's Copy Token
	rproj    int64
	ruser    int64
	adm      *adminClient
	localDev string
}

func newUpFx(t *testing.T, role core.Role) *upFx {
	t.Helper()
	f := &upFx{t: t, keys: fakeKeys(t)}
	f.local, _, _ = coreLiveServer(t)
	var rbase string
	f.remote, _, rbase = coreLiveServer(t)
	f.addr = strings.TrimPrefix(rbase, "http://")
	f.token = connectToken(f.remote.cfg)
	rc := f.remote.core
	u, err := rc.CreateUser(t0, "gw-mini")
	f.must(err)
	f.ruser = u.ID
	p, err := rc.CreateProject(t0, "shop", "Shop")
	f.must(err)
	f.rproj = p.ID
	f.must(rc.SetMember(t0, p.ID, u.ID, role))
	f.adm = &adminClient{off: f.local}
	return f
}

func (f *upFx) must(err error) {
	f.t.Helper()
	if err != nil {
		f.t.Fatal(err)
	}
}

// cmd runs `minigw gw upstream ...` as the owner and returns its output; a failing command is an error with the printed text.
func (f *upFx) cmd(o coreOpts, args ...string) (string, error) {
	out, errText, exit := captured(func() { coreCommand(f.adm, append([]string{"upstream"}, args...), o) })
	if exit != 0 {
		return out, fmt.Errorf("%s", strings.TrimSpace(errText))
	}
	return out, nil
}

func (f *upFx) add(name string) string {
	f.t.Helper()
	out, err := f.cmd(coreOpts{in: strings.NewReader(f.token + "\n")}, "add", name, f.addr)
	f.must(err)
	f.localDev = upstreamHost{f.local}.Info(name).Device
	if f.localDev == "" {
		f.t.Fatal("no device id", out)
	}
	return out
}

// allow: the upstream's owner allows the device and binds it to gw-mini.
func (f *upFx) allow() {
	f.t.Helper()
	f.must(f.remote.update(func(c *serveConfig) error {
		c.Devices = append(c.Devices, serveDevice{ID: f.localDev, Name: "local"})
		return nil
	}))
	f.must(f.remote.core.SetBinding(t0, f.localDev, f.ruser))
}

// seed: the local project shop with ana and notes of three classes.
func (f *upFx) seed() (p core.Principal, project int64, notes map[string]core.Note) {
	c := f.local.core
	u, err := c.CreateUser(t0, "ana")
	f.must(err)
	f.must(c.SetBinding(t0, "dev-ana", u.ID))
	pr, err := c.CreateProject(t0, "shop", "Shop")
	f.must(err)
	f.must(c.SetMember(t0, pr.ID, u.ID, core.Developer))
	pr2, err := c.CreateProject(t0, "other", "Other")
	f.must(err)
	f.must(c.SetMember(t0, pr2.ID, u.ID, core.Developer))
	p, err = c.Principal(t0, "dev-ana", true)
	f.must(err)
	notes = map[string]core.Note{}
	for i, x := range []struct {
		title string
		class core.Class
		proj  int64
	}{{"Invoice numbering", core.Structure, pr.ID}, {"Reset steps", core.Data, pr.ID}, {"My private note", core.Private, pr.ID}, {"Other project note", core.Structure, pr2.ID}} {
		n, _, err := c.CreateNote(t0, p, x.proj, fmt.Sprint("seed-", i), core.NoteInput{Class: x.class, Title: x.title, Body: "text of " + x.title})
		f.must(err)
		notes[x.title] = n
	}
	return p, pr.ID, notes
}

func (f *upFx) remoteSubmissions() (n int, titles string) {
	f.t.Helper()
	f.must(f.remote.core.DB().QueryRow(`SELECT count(*), coalesce(group_concat(title,'|'),'') FROM kb_submissions`).Scan(&n, &titles))
	return
}

func TestUpstreamEndToEnd(t *testing.T) {
	f := newUpFx(t, core.Developer)
	out := f.add("hub")
	for _, want := range []string{"upstream hub: " + f.addr, "device " + f.localDev, "credentials stored in test", "waiting for approval on the upstream as gateway "} {
		if !strings.Contains(out, want) {
			t.Fatalf("add: %q lacks %q", out, want)
		}
	}
	p, project, notes := f.seed()
	if out, err := f.cmd(coreOpts{}, "list"); err != nil || !strings.Contains(out, "hub  "+f.addr+"  device "+f.localDev) {
		t.Fatalf("%q %v", out, err)
	}
	if _, err := f.cmd(coreOpts{}, "link", "other", "nope"); err == nil {
		t.Fatal("a link to an upstream that does not exist")
	}
	if _, err := f.cmd(coreOpts{}, "link", "shop", "hub"); err != nil {
		t.Fatal(err)
	}
	if out, err := f.cmd(coreOpts{}, "status"); err != nil || !strings.Contains(out, "shop -> hub (shop): 2 of 2 notes would go, never pushed, classes structure,data") {
		t.Fatalf("%q %v", out, err)
	}
	// the device is not allowed on the upstream yet: a refusal with the upstream's text, nothing sent, remembered in status
	out, err := f.cmd(coreOpts{}, "push", "shop")
	if err != nil || !strings.Contains(out, "stopped: ") || !strings.Contains(out, "created 0") {
		t.Fatalf("%q %v", out, err)
	}
	if out, _ = f.cmd(coreOpts{}, "status", "shop"); !strings.Contains(out, "last refusal: ") {
		t.Fatalf("%q", out)
	}
	f.allow()
	// dry run: asks, writes nothing here or there
	out, err = f.cmd(coreOpts{dryRun: true}, "push", "shop")
	if err != nil || !strings.Contains(out, "would create") || !strings.Contains(out, "would be created 2") {
		t.Fatalf("%q %v", out, err)
	}
	if n, _ := f.remoteSubmissions(); n != 0 {
		t.Fatal("a dry run sent", n)
	}
	if out, _ = f.cmd(coreOpts{}, "status", "shop"); !strings.Contains(out, "2 of 2 notes would go, never pushed") {
		t.Fatalf("a dry run changed the link: %q", out)
	}
	// the push: two submissions, the published public ones of the linked project only
	out, err = f.cmd(coreOpts{}, "push", "shop")
	if err != nil || !strings.Contains(out, "created 2, replaced 0, skipped 0, refused 0") {
		t.Fatalf("%q %v", out, err)
	}
	if n, titles := f.remoteSubmissions(); n != 2 || titles != "Invoice numbering|Reset steps" {
		t.Fatal(n, titles)
	}
	var body, state string
	f.must(f.remote.core.DB().QueryRow(`SELECT body,state FROM kb_submissions ORDER BY id LIMIT 1`).Scan(&body, &state))
	if state != "pending" || !strings.Contains(body, "text of Invoice numbering") || !strings.Contains(body, "minigw upstream ref ") || !strings.Contains(body, "author ana") {
		t.Fatalf("%s %q", state, body)
	}
	// the submission arrived with the rights of the bound user there, as that user
	var uid int64
	f.must(f.remote.core.DB().QueryRow(`SELECT user_id FROM kb_submissions ORDER BY id LIMIT 1`).Scan(&uid))
	if uid != f.ruser {
		t.Fatal("the submission is not the bound user's", uid)
	}
	// again: nothing new
	out, _ = f.cmd(coreOpts{}, "push", "shop")
	if !strings.Contains(out, "created 0, replaced 0, skipped 2, refused 0") {
		t.Fatal(out)
	}
	// a changed note: still pending there, so a new submission that names the old one; after acceptance, a replacement of the note
	n := notes["Invoice numbering"]
	_, err = f.local.core.UpdateNote(t0, p, project, n.ID, n.Revision, core.NoteInput{Class: core.Structure, Title: "Invoice numbering", Body: "second text"})
	f.must(err)
	out, _ = f.cmd(coreOpts{}, "push", "shop")
	if !strings.Contains(out, "replace "+fmt.Sprint(n.ID)) || !strings.Contains(out, "still pending") || !strings.Contains(out, "replaced 1") {
		t.Fatal(out)
	}
	// the reviewers there accept both submissions of that note; the next change replaces the accepted note
	for _, id := range []int{1, 3} {
		if _, err := f.remote.core.ReviewSubmission(t0, int64(id), true, ""); err != nil {
			t.Fatal(id, err)
		}
	}
	n, err = f.local.core.GetNote(t0, p, project, n.ID)
	f.must(err)
	_, err = f.local.core.UpdateNote(t0, p, project, n.ID, n.Revision, core.NoteInput{Class: core.Structure, Title: "Invoice numbering", Body: "third text"})
	f.must(err)
	out, _ = f.cmd(coreOpts{}, "push", "shop")
	if !strings.Contains(out, "replaces the accepted note") || !strings.Contains(out, "replaced 1") {
		t.Fatal(out)
	}
	var rep int64
	var rbody string
	f.must(f.remote.core.DB().QueryRow(`SELECT replaces,body FROM kb_submissions ORDER BY id DESC LIMIT 1`).Scan(&rep, &rbody))
	if rep == 0 || !strings.Contains(rbody, "third text") {
		t.Fatal(rep, rbody)
	}
	// unlink: nothing is sent for it any more
	if _, err = f.cmd(coreOpts{}, "unlink", "shop", "hub"); err != nil {
		t.Fatal(err)
	}
	if _, err = f.cmd(coreOpts{}, "push", "shop"); err == nil || !strings.Contains(err.Error(), "no upstream link") {
		t.Fatal(err)
	}
}

// The bound user's rights are what counts: a reader there cannot submit, whatever the local owner does.
func TestUpstreamBoundUserWithoutWriteRole(t *testing.T) {
	f := newUpFx(t, core.Reader)
	f.add("hub")
	f.seed()
	f.allow()
	if _, err := f.cmd(coreOpts{}, "link", "shop", "hub"); err != nil {
		t.Fatal(err)
	}
	out, err := f.cmd(coreOpts{}, "push", "shop")
	if err != nil || !strings.Contains(out, "refused 1") || !strings.Contains(out, "stopped: the upstream says:") {
		t.Fatalf("%q %v", out, err)
	}
	if n, _ := f.remoteSubmissions(); n != 0 {
		t.Fatal("a reader's submission arrived", n)
	}
}

func TestUpstreamAddRefusals(t *testing.T) {
	f := newUpFx(t, core.Developer)
	// this same gateway: by its token and by its address
	own := connectToken(f.local.cfg)
	if _, err := f.cmd(coreOpts{in: strings.NewReader(own)}, "add", "me", f.addr); err == nil || !strings.Contains(err.Error(), "this same gateway") {
		t.Fatal(err)
	}
	if _, err := f.cmd(coreOpts{in: strings.NewReader(f.token)}, "add", "me", fmt.Sprintf("127.0.0.1:%d", f.local.cfg.Port)); err == nil || !strings.Contains(err.Error(), "this same gateway") {
		t.Fatal(err)
	}
	if _, err := f.cmd(coreOpts{in: strings.NewReader(f.token)}, "add", "bad", "https://hub.example"); err == nil {
		t.Fatal("an address with a scheme was accepted")
	}
	if _, err := f.cmd(coreOpts{in: strings.NewReader("just-a-token")}, "add", "bad", f.addr); err == nil || !strings.Contains(err.Error(), "Copy Token") {
		t.Fatal(err)
	}
	if out, _ := f.cmd(coreOpts{}, "list"); !strings.Contains(out, "no upstreams") {
		t.Fatalf("a refused add left something behind: %q", out)
	}
	if len(f.keys) != 0 {
		t.Fatalf("a refused add left credentials behind: %v", f.keys)
	}
	f.add("hub")
	if _, err := f.cmd(coreOpts{in: strings.NewReader(f.token)}, "add", "hub", f.addr); err == nil || !strings.Contains(err.Error(), "exists") {
		t.Fatal(err)
	}
	if _, err := f.cmd(coreOpts{}, "remove", "hub"); err != nil {
		t.Fatal(err)
	}
	if len(f.keys) != 0 {
		t.Fatalf("remove left credentials behind: %v", f.keys)
	}
}

// The token and the device key leave only in the handshake with the configured upstream: not in an API answer, a backup, the log, the
// audit, the status or the push report.
func TestUpstreamSecretsStayInTheCredentialStore(t *testing.T) {
	f := newUpFx(t, core.Developer)
	var logs bytes.Buffer
	f.local.log.SetOutput(&logs)
	f.add("hub")
	f.allow()
	f.seed()
	f.must(f.local.core.LinkUpstream(t0, f.projectID("shop"), 0, "hub", "shop", "", false))
	f.cmd(coreOpts{}, "push", "shop")
	var sec upstreamSecret
	if err := json.Unmarshal([]byte(f.keys["upstream:hub"]), &sec); err != nil || sec.Token == "" || sec.Device == "" || sec.Key == "" {
		t.Fatalf("the secret is not in the credential store: %v %q", err, f.keys["upstream:hub"])
	}
	secrets := []string{sec.Token, sec.Device}
	if sec.Token == "tok" { // the test gateway's token is short: make sure the check can see it
		secrets = []string{"\"tok\"", sec.Device}
	}
	var seen strings.Builder
	for _, path := range []string{"/upstreams", "/upstreams?project=1"} {
		_, obj := f.local.adminDo("GET", path, nil)
		b, _ := json.Marshal(obj)
		seen.Write(b)
	}
	cli, _ := f.cmd(coreOpts{}, "list")
	st, _ := f.cmd(coreOpts{}, "status")
	push, _ := f.cmd(coreOpts{}, "push", "shop")
	seen.WriteString(cli + st + push + logs.String())
	bak := t.TempDir() + "/backup.db"
	f.must(f.local.core.Backup(t0, bak))
	raw, err := os.ReadFile(bak)
	f.must(err)
	_, audit := f.local.adminDo("GET", "/audit", nil)
	ab, _ := json.Marshal(audit)
	seen.Write(ab)
	for _, s := range secrets {
		if strings.Contains(seen.String(), s) || bytes.Contains(raw, []byte(s)) {
			t.Fatalf("the secret %q... is in an answer, the log, the audit or a backup", s[:3])
		}
	}
	if !strings.Contains(cli, f.localDev) {
		t.Fatal("the device id (public) should be shown")
	}
}

func (f *upFx) projectID(slug string) int64 {
	ps, err := f.local.core.AdminListProjects(t0)
	f.must(err)
	for _, p := range ps {
		if p.Slug == slug {
			return p.ID
		}
	}
	f.t.Fatal("no project", slug)
	return 0
}

// A hostile upstream that answers with a redirect: it is not followed (and nothing could carry it to another host: the connection is
// made in memory to the configured address only).
func TestUpstreamRedirectIsNotFollowed(t *testing.T) {
	keys := fakeKeys(t)
	local, _, _ := coreLiveServer(t)
	key, _ := ecdh.X25519().GenerateKey(rand.Reader)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var accepted atomic.Int32
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted.Add(1)
			go func() {
				defer c.Close()
				hs := make([]byte, 4+handshakeLen)
				c.SetDeadline(time.Now().Add(5 * time.Second))
				if _, err := readFull(c, hs); err != nil {
					return
				}
				reply, sess, err := respond(hs[4:], key)
				if err != nil {
					return
				}
				c.Write(reply)
				var plain, pending []byte
				buf := make([]byte, 64<<10)
				for !bytes.Contains(plain, []byte("\r\n\r\n")) {
					n, err := c.Read(buf)
					if n > 0 {
						var p []byte
						p, pending, _ = unframe(append(pending, buf[:n]...), sess.receive)
						plain = append(plain, p...)
					}
					if err != nil {
						return
					}
				}
				c.Write(frames([]byte("HTTP/1.1 302 Found\r\nLocation: http://127.0.0.1:1/elsewhere\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"), sess.send))
			}()
		}
	}()
	tok := "tok#" + base64url(key.PublicKey().Bytes())
	h := upstreamHost{local}
	if _, err := h.Connect("evil", ln.Addr().String(), tok, "test device"); err != nil {
		t.Fatal(err)
	}
	f := &upFx{t: t, local: local, keys: keys, adm: &adminClient{off: local}}
	f.must(local.core.AddUpstream(t0, "evil", ln.Addr().String()))
	st, _, _, err := h.Do(context.Background(), "evil", "GET", "/minigw/v1/projects/shop", nil, nil)
	if err != nil || st != 302 {
		t.Fatalf("status %d, err %v: the redirect must come back as it is", st, err)
	}
	if accepted.Load() != 1 {
		t.Fatalf("%d connections: the redirect was followed", accepted.Load())
	}
}

func readFull(c net.Conn, b []byte) (int, error) {
	n := 0
	for n < len(b) {
		m, err := c.Read(b[n:])
		n += m
		if err != nil {
			return n, err
		}
	}
	return n, nil
}

// A push that the gateway ends with "more" (a helper answers a call within 30 seconds) is asked for again, after the wait it names.
func TestUpstreamPushAsksAgainWhileTheGatewaySaysMore(t *testing.T) {
	calls := 0
	var bodies []string
	a := &adminClient{fake: func(method, path string, b []byte) (int, []byte) {
		if path == "/projects" {
			return 200, []byte(`{"projects":[{"ID":3,"Slug":"shop","Name":"Shop"}]}`)
		}
		calls++
		bodies = append(bodies, string(b))
		more := ""
		if calls < 3 {
			more = fmt.Sprintf(`,"more":true,"wait":7,"next":%d,"stopped":"the upstream's write limit: push again in 7 seconds"`, calls*100)
		}
		return 200, []byte(fmt.Sprintf(`{"pushes":[{"label":"shop","upstream":"hub","remote":"shop","project":3,"lines":["create %d n"],"created":1%s}]}`, calls, more))
	}}
	var waits []time.Duration
	out, _, exit := captured(func() {
		coreCommand(a, []string{"upstream", "push", "shop"}, coreOpts{pause: func(d time.Duration) { waits = append(waits, d) }})
	})
	if exit != 0 || calls != 3 || len(waits) != 2 || waits[0] != 7*time.Second || strings.Count(out, "create ") != 3 {
		t.Fatalf("%d calls, waits %v, exit %d: %s", calls, waits, exit, out)
	}
	// the continuation names the link and where the last call ended
	if bodies[1] != `{"after":100,"dryRun":false,"project":3,"upstream":"hub"}` || bodies[2] != `{"after":200,"dryRun":false,"project":3,"upstream":"hub"}` {
		t.Fatalf("%v", bodies)
	}
	// a gateway that says "more" without getting further is not asked for ever
	calls = 0
	stuck := &adminClient{fake: func(method, path string, b []byte) (int, []byte) {
		if path == "/projects" {
			return 200, []byte(`{"projects":[{"ID":3,"Slug":"shop","Name":"Shop"}]}`)
		}
		calls++
		return 200, []byte(`{"pushes":[{"label":"shop","upstream":"hub","remote":"shop","project":3,"lines":[],"more":true,"next":50}]}`)
	}}
	out, _, _ = captured(func() {
		coreCommand(stuck, []string{"upstream", "push", "shop"}, coreOpts{pause: func(time.Duration) {}})
	})
	if calls != 2 || !strings.Contains(out, "no progress") {
		t.Fatalf("%d calls: %s", calls, out)
	}
}

// The Go side reads an upstream's answer with net/http (not a reader of its own): redirects off, a total timeout, and at most 1 MiB and one
// byte of body kept (what the core accepts). A table of hostile answers, through the real host over a real encrypted connection: none may
// panic, hang or return more than the cap.
func TestUpstreamHostSurvivesHostileAnswers(t *testing.T) {
	keys := fakeKeys(t)
	local, _, _ := coreLiveServer(t)
	_ = keys
	big := bytes.Repeat([]byte("a"), 3<<20)
	chunked := func(sizeLine string) []byte {
		return []byte("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n" + sizeLine + "\r\nabcde\r\n0\r\n\r\n")
	}
	table := map[string][]byte{
		"empty size line":      chunked(""),
		"extension only":       chunked(";x"),
		"plus sign":            chunked("+5"),
		"minus":                chunked("-1"),
		"not hex":              chunked("zz"),
		"huge hex":             chunked("ffffffffffffffffffff"),
		"huge decimal length":  []byte("HTTP/1.1 200 OK\r\nContent-Length: 99999999999999999999\r\n\r\nx"),
		"length above the cap": append([]byte("HTTP/1.1 200 OK\r\nContent-Length: 3145728\r\n\r\n"), big...),
		"close-delimited big":  append([]byte("HTTP/1.1 200 OK\r\nConnection: close\r\n\r\n"), big...),
		"both length and TE":   []byte("HTTP/1.1 200 OK\r\nContent-Length: 1\r\nTransfer-Encoding: chunked\r\n\r\n1\r\nx\r\n0\r\n\r\n"),
		"endless interim":      bytes.Repeat([]byte("HTTP/1.1 100 Continue\r\n\r\n"), 5000),
		"garbage":              []byte("\x00\xff\xfe not http at all\r\n\r\n"),
		"truncated":            []byte("HTTP/1.1 200 OK\r\nContent-Length: 10\r\n\r\nabc"),
		"no head end":          []byte("HTTP/1.1 200 OK\r\nX: y"),
		"many tiny chunks":     append([]byte("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n"), bytes.Repeat([]byte("1\r\nx\r\n"), 100000)...),
	}
	for name, answer := range table {
		t.Run(name, func(t *testing.T) {
			key, _ := ecdh.X25519().GenerateKey(rand.Reader)
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			go func() {
				for {
					c, err := ln.Accept()
					if err != nil {
						return
					}
					go func() {
						defer c.Close()
						hs := make([]byte, 4+handshakeLen)
						c.SetDeadline(time.Now().Add(20 * time.Second))
						if _, err := readFull(c, hs); err != nil {
							return
						}
						reply, sess, err := respond(hs[4:], key)
						if err != nil {
							return
						}
						c.Write(reply)
						var plain, pending []byte
						buf := make([]byte, 64<<10)
						for !bytes.Contains(plain, []byte("\r\n\r\n")) {
							n, err := c.Read(buf)
							if n > 0 {
								var p []byte
								p, pending, _ = unframe(append(pending, buf[:n]...), sess.receive)
								plain = append(plain, p...)
							}
							if err != nil {
								return
							}
						}
						for off := 0; off < len(answer); off += maxPlain {
							if _, err := c.Write(frames(answer[off:min(off+maxPlain, len(answer))], sess.send)); err != nil {
								return
							}
						}
					}()
				}
			}()
			h := upstreamHost{local}
			if _, err := h.Connect("evil", ln.Addr().String(), "tok#"+base64url(key.PublicKey().Bytes()), "test device"); err != nil {
				t.Fatal(err)
			}
			local.core.AddUpstream(t0, "evil", ln.Addr().String())
			defer local.core.RemoveUpstream(t0, "evil")
			start := time.Now()
			st, body, _, err := h.Do(context.Background(), "evil", "GET", "/minigw/v1/projects/shop", nil, nil)
			if len(body) > coreapi.MaxCalloutBody {
				t.Fatalf("%d bytes of body kept", len(body))
			}
			_ = st // a body of more than 1 MiB reaches the core as one byte too many, which it refuses (TestUpstreamAnswersAreData)
			if time.Since(start) > 30*time.Second {
				t.Fatal("took", time.Since(start))
			}
		})
	}
}

// `upstream add` is a client's first contact: the upstream lists the device as WAITING, with the name the owner chose and the client
// description of a gateway; once allowed, a gateway that only sends notes shows its last contact and the same description.
func TestUpstreamAddMakesTheDeviceWaitingAndItIsDescribed(t *testing.T) {
	f := newUpFx(t, core.Developer)
	out, err := f.cmd(coreOpts{in: strings.NewReader(f.token), deviceName: "Uros mini"}, "add", "hub", f.addr)
	f.must(err)
	f.localDev = upstreamHost{f.local}.Info("hub").Device
	if !strings.Contains(out, "waiting for approval on the upstream as Uros mini ("+f.localDev+"): its owner runs: minigw gw devices allow "+f.localDev) {
		t.Fatalf("%q", out)
	}
	_, obj := f.remote.adminDo("GET", "/devices", nil)
	b, _ := json.Marshal(obj)
	var l struct{ Devices, Pending []apiDevice }
	json.Unmarshal(b, &l)
	if len(l.Pending) != 1 || l.Pending[0].ID != f.localDev || l.Pending[0].Name != "Uros mini" || l.Pending[0].Client != "gateway, minigw "+version {
		t.Fatalf("pending list: %s", b)
	}
	// allowed but not bound: the probe says what is missing; then bound: connected
	f.must(f.remote.update(func(c *serveConfig) error {
		c.Devices = append(c.Devices, serveDevice{ID: f.localDev, Name: "Uros mini"})
		return nil
	}))
	ph := coreapi.ProbeUpstream(t0, upstreamHost{f.local}, "hub", "Uros mini", f.localDev)
	if !strings.Contains(ph, "not bound to a user") || !strings.Contains(ph, "minigw gw bind "+f.localDev) {
		t.Fatalf("%q", ph)
	}
	f.must(f.remote.core.SetBinding(t0, f.localDev, f.ruser))
	if ph = coreapi.ProbeUpstream(t0, upstreamHost{f.local}, "hub", "Uros mini", f.localDev); !strings.Contains(ph, "connected") {
		t.Fatalf("%q", ph)
	}
	// after a push (member routes only) the allowed device shows its client description and last contact
	f.seed()
	f.must(f.local.core.LinkUpstream(t0, f.projectID("shop"), 0, "hub", "shop", "", false))
	if _, err := f.cmd(coreOpts{}, "push", "shop"); err != nil {
		t.Fatal(err)
	}
	_, obj = f.remote.adminDo("GET", "/devices", nil)
	b, _ = json.Marshal(obj)
	json.Unmarshal(b, &l)
	if len(l.Devices) != 1 || l.Devices[0].Client != "gateway, minigw "+version || l.Devices[0].LastSeen == "" {
		t.Fatalf("device list after a push: %s", b)
	}
}

func waitUntil(t *testing.T, what string, d time.Duration, ok func() bool) {
	t.Helper()
	end := time.Now().Add(d)
	for !ok() {
		if time.Now().After(end) {
			t.Fatal("timed out waiting for", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The automatic push between two real gateways (loopback): a published note reaches the upstream by itself after the debounce, a storm of
// edits is one push, an upstream that refuses is retried with a back-off and the link recovers, and status tells all of it.
func TestUpstreamAutoPushBetweenTwoGateways(t *testing.T) {
	f := newUpFx(t, core.Developer)
	f.add("hub")
	f.allow()
	p, project, notes := f.seed()
	// timing: a quarter of a second quiet, the loop looks every 30 ms
	f.local.core.SetAutoTiming(core.AutoTiming{Debounce: 300 * time.Millisecond, MaxWait: 5 * time.Second, BackoffBase: 250 * time.Millisecond, BackoffMax: time.Second})
	f.local.autoTick.Store(int64(30 * time.Millisecond))
	out, err := f.cmd(coreOpts{auto: true}, "link", "shop", "hub")
	f.must(err)
	if !strings.Contains(out, "changes are sent automatically") {
		t.Fatal(out)
	}
	// the notes that were already there go by themselves (switching auto on marks what is unsent)
	waitUntil(t, "the first automatic push", 10*time.Second, func() bool { n, _ := f.remoteSubmissions(); return n == 2 })
	// the upstream has both submissions a moment before the local run has written its receipts and cleared the dirty mark: wait for the
	// run to end (on Linux the status was once read in between: "1 of 2 notes would go ... dirty since")
	var st string
	waitUntil(t, "the first automatic run to finish", 10*time.Second, func() bool {
		st, _ = f.cmd(coreOpts{}, "status", "shop")
		return strings.Contains(st, "last success") && !strings.Contains(st, "dirty since")
	})
	if !strings.Contains(st, ", auto") {
		t.Fatalf("status after the push: %s", st)
	}
	// a storm of edits (a member edits the same note 40 times in a quick row): ONE more submission, with the last text
	n := notes["Invoice numbering"]
	before, _ := f.remoteSubmissions()
	for i := 0; i < 40; i++ {
		cur, err := f.local.core.GetNote(t0, p, project, n.ID)
		f.must(err)
		_, err = f.local.core.UpdateNote(t0, p, project, n.ID, cur.Revision, core.NoteInput{Class: core.Structure, Title: "Invoice numbering", Body: fmt.Sprint("storm ", i)})
		f.must(err)
	}
	waitUntil(t, "the push after the storm", 10*time.Second, func() bool { n, _ := f.remoteSubmissions(); return n > before })
	time.Sleep(1200 * time.Millisecond) // long enough for a second (wrong) push to show
	after, titles := f.remoteSubmissions()
	if after != before+1 {
		t.Fatalf("a storm of 40 edits made %d pushes (%s)", after-before, titles)
	}
	var body string
	f.must(f.remote.core.DB().QueryRow(`SELECT body FROM kb_submissions ORDER BY id DESC LIMIT 1`).Scan(&body))
	if !strings.Contains(body, "storm 39") {
		t.Fatalf("not the last text: %q", body)
	}
	// the upstream refuses (the device is unbound there): failures with a back-off, shown in status; bound again: it recovers by itself
	f.must(f.remote.core.RemoveBinding(t0, f.localDev))
	cur, err := f.local.core.GetNote(t0, p, project, n.ID)
	f.must(err)
	_, err = f.local.core.UpdateNote(t0, p, project, n.ID, cur.Revision, core.NoteInput{Class: core.Structure, Title: "Invoice numbering", Body: "while it is down"})
	f.must(err)
	waitUntil(t, "a failure in status", 10*time.Second, func() bool {
		s, _ := f.cmd(coreOpts{}, "status", "shop")
		return strings.Contains(s, "last failure") && strings.Contains(s, "next attempt") && strings.Contains(s, "last refusal")
	})
	f.must(f.remote.core.SetBinding(t0, f.localDev, f.ruser))
	waitUntil(t, "the recovery", 15*time.Second, func() bool {
		s, _ := f.cmd(coreOpts{}, "status", "shop")
		return !strings.Contains(s, "dirty since") && strings.Contains(s, "last success")
	})
	if n2, _ := f.remoteSubmissions(); n2 != after+1 {
		t.Fatalf("the change made while it was down arrived %d times", n2-after)
	}
	// an audit row per automatic run, counts only
	var rows int
	var detail string
	f.must(f.local.core.DB().QueryRow(`SELECT count(*), coalesce(max(detail),'') FROM audit_events WHERE action='upstream.auto-push'`).Scan(&rows, &detail))
	if rows < 3 || strings.Contains(detail, "storm") || strings.Contains(detail, "Invoice") {
		t.Fatalf("%d audit rows, %s", rows, detail)
	}
}

func (f *upFx) remoteNote(title string) (id, rev int64, body string) {
	f.t.Helper()
	f.must(f.remote.core.DB().QueryRow(`SELECT id, revision, body FROM notes WHERE title=?`, title).Scan(&id, &rev, &body))
	return
}

// Publish mode between two real gateways: notes are written on the upstream directly and updated in place, a note that changed there is
// never overwritten (a conflict, shown until the owner resolves it), and both resolutions do what they say.
func TestUpstreamPublishModeBetweenTwoGateways(t *testing.T) {
	f := newUpFx(t, core.Developer)
	f.add("hub")
	f.allow()
	p, project, notes := f.seed()
	c := f.local.core
	edit := func(title, body string) {
		t.Helper()
		n := notes[title]
		cur, err := c.GetNote(t0, p, project, n.ID)
		f.must(err)
		_, err = c.UpdateNote(t0, p, project, n.ID, cur.Revision, core.NoteInput{Class: n.Class, Title: title, Body: body})
		f.must(err)
	}
	if out, err := f.cmd(coreOpts{mode: "publish"}, "link", "shop", "hub"); err != nil {
		t.Fatal(out, err)
	}
	out, err := f.cmd(coreOpts{}, "push", "shop")
	if err != nil || !strings.Contains(out, "create "+fmt.Sprint(notes["Invoice numbering"].ID)) || !strings.Contains(out, "created 2") {
		t.Fatalf("%q %v", out, err)
	}
	if n, _ := f.remoteSubmissions(); n != 0 {
		t.Fatalf("publish mode made %d submissions", n)
	}
	id, rev, body := f.remoteNote("Invoice numbering")
	if !strings.HasPrefix(body, "text of Invoice numbering") {
		t.Fatalf("%q", body)
	}
	// a second push with nothing changed sends nothing; a changed note updates the SAME note there
	if out, _ = f.cmd(coreOpts{}, "push", "shop"); !strings.Contains(out, "created 0, replaced 0, skipped 2") {
		t.Fatal(out)
	}
	edit("Invoice numbering", "second text")
	if out, _ = f.cmd(coreOpts{}, "push", "shop"); !strings.Contains(out, "replaced 1") {
		t.Fatal(out)
	}
	id2, rev2, body2 := f.remoteNote("Invoice numbering")
	var count int
	f.must(f.remote.core.DB().QueryRow(`SELECT count(*) FROM notes`).Scan(&count))
	if id2 != id || rev2 <= rev || !strings.HasPrefix(body2, "second text") || count != 2 {
		t.Fatalf("id %d/%d rev %d/%d body %q notes %d", id, id2, rev, rev2, body2, count)
	}
	// somebody changes the note on the upstream; our edit must not overwrite it
	rp, err := f.remote.core.Principal(t0, f.localDev, true)
	f.must(err)
	rproj := f.projectID("shop")
	_, err = f.remote.core.UpdateNote(t0, rp, rproj, id, rev2, core.NoteInput{Class: core.Structure, Title: "Invoice numbering", Body: "THEIR text"})
	f.must(err)
	edit("Invoice numbering", "third text")
	for i := 0; i < 2; i++ { // the second push does not try again
		out, err = f.cmd(coreOpts{}, "push", "shop")
		if err != nil || (i == 0 && !strings.Contains(out, "conflict ")) || !strings.Contains(out, "conflicts 1") {
			t.Fatalf("push %d: %q %v", i, out, err)
		}
		if _, _, b := f.remoteNote("Invoice numbering"); b != "THEIR text" {
			t.Fatalf("overwritten: %q", b)
		}
	}
	if out, _ = f.cmd(coreOpts{}, "status", "shop"); !strings.Contains(out, "CONFLICT: note "+fmt.Sprint(notes["Invoice numbering"].ID)+" (changed there)") {
		t.Fatal(out)
	}
	// resolve with nothing to resolve, or without a choice
	if _, err = f.cmd(coreOpts{ours: true}, "resolve", "shop", fmt.Sprint(notes["Reset steps"].ID)); err == nil {
		t.Fatal("resolved a note without a conflict")
	}
	// theirs: our change is forgotten for this revision; a later edit goes through as an update again
	nid := fmt.Sprint(notes["Invoice numbering"].ID)
	if out, err = f.cmd(coreOpts{theirs: true}, "resolve", "shop", nid); err != nil {
		t.Fatal(out, err)
	}
	if out, _ = f.cmd(coreOpts{}, "status", "shop"); strings.Contains(out, "CONFLICT") {
		t.Fatal(out)
	}
	if out, _ = f.cmd(coreOpts{}, "push", "shop"); !strings.Contains(out, "replaced 0") || !strings.Contains(out, "skipped 2") {
		t.Fatal(out)
	}
	if _, _, b := f.remoteNote("Invoice numbering"); b != "THEIR text" {
		t.Fatalf("theirs overwritten: %q", b)
	}
	edit("Invoice numbering", "fourth text")
	if out, _ = f.cmd(coreOpts{}, "push", "shop"); !strings.Contains(out, "replaced 1") {
		t.Fatal(out)
	}
	if _, _, b := f.remoteNote("Invoice numbering"); !strings.HasPrefix(b, "fourth text") {
		t.Fatalf("%q", b)
	}
	// ours: the upstream changes again, we choose ours: our version arrives as a SUBMISSION, the note there is not touched
	_, rev3, _ := f.remoteNote("Invoice numbering")
	_, err = f.remote.core.UpdateNote(t0, rp, rproj, id, rev3, core.NoteInput{Class: core.Structure, Title: "Invoice numbering", Body: "THEIR second"})
	f.must(err)
	edit("Invoice numbering", "fifth text")
	if out, _ = f.cmd(coreOpts{}, "push", "shop"); !strings.Contains(out, "conflicts 1") {
		t.Fatal(out)
	}
	if out, err = f.cmd(coreOpts{ours: true}, "resolve", "shop", nid); err != nil {
		t.Fatal(out, err)
	}
	out, err = f.cmd(coreOpts{}, "push", "shop")
	if err != nil || !strings.Contains(out, "resolved as ours") {
		t.Fatalf("%q %v", out, err)
	}
	if n, titles := f.remoteSubmissions(); n != 1 || titles != "Invoice numbering" {
		t.Fatalf("%d %s", n, titles)
	}
	if _, _, b := f.remoteNote("Invoice numbering"); b != "THEIR second" {
		t.Fatalf("ours overwrote: %q", b)
	}
	if out, _ = f.cmd(coreOpts{}, "status", "shop"); strings.Contains(out, "CONFLICT") {
		t.Fatal(out)
	}
	// the mode cannot be switched once notes were sent in the other one; the link keeps its mode
	if _, err = f.cmd(coreOpts{mode: "submit"}, "link", "shop", "hub"); err == nil {
		t.Fatal("mode switched after sending")
	}
	if out, _ = f.cmd(coreOpts{}, "status", "shop"); !strings.Contains(out, "auto") && !strings.Contains(out, "publish") {
		t.Fatal(out)
	}
	// a note deleted on the upstream is a conflict, not recreated
	rid, rrev, _ := f.remoteNote("Reset steps")
	f.must(f.remote.core.DeleteNote(t0, rp, rproj, rid, rrev))
	edit("Reset steps", "changed after their delete")
	if out, _ = f.cmd(coreOpts{}, "push", "shop"); !strings.Contains(out, "deleted on the upstream, not recreated") {
		t.Fatal(out)
	}
	if out, _ = f.cmd(coreOpts{}, "status", "shop"); !strings.Contains(out, "(deleted there)") {
		t.Fatal(out)
	}
}
