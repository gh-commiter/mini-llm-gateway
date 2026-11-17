package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

type upCall struct {
	method, path string
	body         map[string]any
	key          string
}

// fakeUp is a scripted upstream: handler answers (status, body, retry-after). It records what was asked.
type fakeUp struct {
	calls   []upCall
	handler func(c upCall) (int, string, time.Duration)
}

func (u *fakeUp) Do(_ context.Context, _, method, path string, h map[string]string, body []byte) (int, []byte, time.Duration, error) {
	c := upCall{method: method, path: path, key: h["Idempotency-Key"]}
	if len(body) > 0 {
		json.Unmarshal(body, &c.body)
	}
	u.calls = append(u.calls, c)
	st, b, w := u.handler(c)
	return st, []byte(b), w, nil
}

func (u *fakeUp) count(method string) (n int) {
	for _, c := range u.calls {
		if c.method == method {
			n++
		}
	}
	return
}

// okUp: project shop is id 7; every submission becomes the next id; nothing else is known.
func okUp() *fakeUp {
	next := 0
	return &fakeUp{handler: func(c upCall) (int, string, time.Duration) {
		switch {
		case c.method == "GET" && c.path == "/minigw/v1/projects/shop":
			return 200, `{"id":7,"slug":"shop"}`, 0
		case c.method == "POST" && c.path == "/minigw/v1/projects/7/kb/submissions":
			next++
			return 201, fmt.Sprintf(`{"id":%d,"state":"pending"}`, next), 0
		}
		return 404, `{"error":{"message":"not found"}}`, 0
	}}
}

func upFx(t *testing.T) (*fx, map[string]Note) {
	f := newFx(t)
	f.ok(f.AddUpstream(ctx, "hub", "hub.example:8765"))
	notes := map[string]Note{}
	for _, c := range []struct {
		title string
		class Class
	}{{"Structure one", Structure}, {"Data two", Data}, {"Private three", Private}} {
		f.clock = f.clock.Add(time.Second)
		keySeq++
		n, _, e := f.CreateNote(ctx, f.p("ana"), f.pid["fakture"], fmt.Sprint("uk", keySeq), NoteInput{Class: c.class, Title: c.title, Body: "body of " + c.title})
		f.ok(e)
		notes[c.title] = n
	}
	// another project's note: it must never be sent
	keySeq++
	_, _, e := f.CreateNote(ctx, f.p("cene"), f.pid["eureko"], fmt.Sprint("uk", keySeq), NoteInput{Class: Structure, Title: "Elsewhere", Body: "other project"})
	f.ok(e)
	return f, notes
}

func noSleep(time.Duration) {}

func TestUpstreamAddressAndName(t *testing.T) {
	for in, want := range map[string]string{"mini.local": "mini.local:8765", "Hub.Example:9000": "hub.example:9000", "10.0.0.5": "10.0.0.5:8765", "[::1]:8765": "[::1]:8765"} {
		if got, e := NormalizeUpstreamAddress(in); e != nil || got != want {
			t.Errorf("%q: %q %v, want %q", in, got, e, want)
		}
	}
	for _, in := range []string{"", "https://hub.example", "http://hub.example:8765", "user@hub.example", "hub.example/path", "hub.example:0", "hub.example:99999", "hub.example:x", "0.0.0" + ".0:8765", "169.254.169.254", "[fe80::1]:8765", "a b", "bad_host"} {
		if got, e := NormalizeUpstreamAddress(in); e == nil {
			t.Errorf("%q accepted as %q", in, got)
		}
	}
	for _, n := range []string{"", "Hub", "-x", "a_b", strings.Repeat("a", 33)} {
		if ValidUpstreamName(n) {
			t.Errorf("name %q accepted", n)
		}
	}
	f := newFx(t)
	f.ok(f.AddUpstream(ctx, "hub", "hub.example"))
	var ref ErrRefusal
	if e := f.AddUpstream(ctx, "hub", "other.example"); !errors.As(e, &ref) || !strings.Contains(ref.Msg, "exists") {
		t.Fatal(e)
	}
	if u, e := f.GetUpstream(ctx, "hub"); e != nil || u.Address != "hub.example:8765" {
		t.Fatal(u, e)
	}
}

func TestUpstreamLinkRules(t *testing.T) {
	f, _ := upFx(t)
	fk := f.pid["fakture"]
	is(t, f.LinkUpstream(ctx, fk, 0, "nope", "shop", "", false), ErrNotFound)
	is(t, f.LinkUpstream(ctx, 999, 0, "hub", "shop", "", false), ErrNotFound) // a project the owner did not name does not exist
	is(t, f.LinkUpstream(ctx, 0, 999, "hub", "shop", "", false), ErrNotFound)
	for name, c := range map[string]struct {
		classes string
		private bool
	}{"private without the switch": {"structure,private", false}, "switch without private": {"structure", true}, "unknown class": {"structure,x", false}, "twice": {"data,data", false}} {
		var ref ErrRefusal
		if e := f.LinkUpstream(ctx, fk, 0, "hub", "shop", c.classes, c.private); !errors.As(e, &ref) {
			t.Errorf("%s: %v", name, e)
		}
	}
	f.ok(f.LinkUpstream(ctx, fk, 0, "hub", "shop", "", false))
	ls, e := f.UpstreamLinks(ctx, 0, 0)
	f.ok(e)
	if len(ls) != 1 || ls[0].Label != "fakture" || ls[0].Pending != 2 || ls[0].Total != 2 || ls[0].LastPush != nil { // the private note is not covered
		t.Fatalf("%+v", ls)
	}
	f.ok(f.LinkUpstream(ctx, fk, 0, "hub", "shop", "structure,data,private", true))
	if ls, _ = f.UpstreamLinks(ctx, fk, 0); ls[0].Total != 3 {
		t.Fatalf("%+v", ls)
	}
	f.ok(f.UnlinkUpstream(ctx, fk, 0, "hub"))
	is(t, f.UnlinkUpstream(ctx, fk, 0, "hub"), ErrNotFound)
	// a project without a link sends nothing: there is nothing to push
	up := okUp()
	if _, e = f.PushUpstream(ctx, up, fk, 0, false, noSleep); e == nil || len(up.calls) != 0 {
		t.Fatal("a push without a link", e, up.calls)
	}
}

func TestUpstreamPushCreateSkipReplace(t *testing.T) {
	f, notes := upFx(t)
	fk := f.pid["fakture"]
	f.ok(f.LinkUpstream(ctx, fk, 0, "hub", "shop", "", false))
	up := okUp()
	res, e := f.PushUpstream(ctx, up, fk, 0, false, noSleep)
	f.ok(e)
	if len(res) != 1 || res[0].Created != 2 || res[0].Refused != 0 || up.count("POST") != 2 {
		t.Fatalf("%+v %v", res, up.calls)
	}
	titles := ""
	for _, c := range up.calls {
		if c.method == "POST" {
			titles += c.body["title"].(string) + "|"
			body := c.body["body"].(string)
			if !strings.Contains(body, "<!-- sent by gateway "+f.GatewayID()+": local note") || !strings.Contains(body, "author ana") {
				t.Errorf("trailer: %q", body)
			}
			if c.body["replaces"].(float64) != 0 || len(c.key) < 10 || len(c.key) > 64 {
				t.Errorf("%+v", c)
			}
		}
	}
	if titles != "Structure one|Data two|" { // never the private note, never another project's
		t.Fatal(titles)
	}
	ls, _ := f.UpstreamLinks(ctx, fk, 0)
	if ls[0].Pending != 0 || ls[0].LastPush == nil || ls[0].LastError != "" {
		t.Fatalf("%+v", ls[0])
	}
	// the second push sends nothing
	up2 := okUp()
	res, e = f.PushUpstream(ctx, up2, fk, 0, false, noSleep)
	f.ok(e)
	if res[0].Skipped != 2 || res[0].Created != 0 || up2.count("POST") != 0 {
		t.Fatalf("%+v", res)
	}
	// a changed note, earlier submission still pending: sent again as a new submission that names the old one
	n := notes["Structure one"]
	f.clock = f.clock.Add(time.Second)
	_, e = f.UpdateNote(ctx, f.p("ana"), fk, n.ID, n.Revision, NoteInput{Class: Structure, Title: "Structure one", Body: "changed body"})
	f.ok(e)
	up3 := &fakeUp{handler: func(c upCall) (int, string, time.Duration) {
		switch {
		case c.path == "/minigw/v1/projects/shop":
			return 200, `{"id":7,"slug":"shop"}`, 0
		case c.method == "GET" && c.path == "/minigw/v1/projects/7/kb/submissions/1":
			return 200, `{"id":1,"state":"pending","note":0}`, 0
		case c.method == "POST":
			return 201, `{"id":9,"state":"pending"}`, 0
		}
		return 404, `{}`, 0
	}}
	res, e = f.PushUpstream(ctx, up3, fk, 0, false, noSleep)
	f.ok(e)
	var post upCall
	for _, c := range up3.calls {
		if c.method == "POST" {
			post = c
		}
	}
	if res[0].Replaced != 1 || res[0].Skipped != 1 || post.body["replaces"].(float64) != 0 || !strings.Contains(post.body["body"].(string), "supersedes submission 1") || !strings.Contains(res[0].Lines[0], "still pending") {
		t.Fatalf("%+v %+v", res, post)
	}
	// the earlier submission was accepted as remote note 55 (revision 3): the change goes as its replacement
	f.clock = f.clock.Add(time.Second)
	n, e = f.GetNote(ctx, f.p("ana"), fk, n.ID)
	f.ok(e)
	_, e = f.UpdateNote(ctx, f.p("ana"), fk, n.ID, n.Revision, NoteInput{Class: Structure, Title: "Structure one", Body: "changed again"})
	f.ok(e)
	up4 := &fakeUp{handler: func(c upCall) (int, string, time.Duration) {
		switch {
		case c.path == "/minigw/v1/projects/shop":
			return 200, `{"id":7,"slug":"shop"}`, 0
		case c.method == "GET" && c.path == "/minigw/v1/projects/7/kb/submissions/9":
			return 200, `{"id":9,"state":"accepted","note":55}`, 0
		case c.method == "GET" && c.path == "/minigw/v1/projects/7/notes/55":
			return 200, `{"id":55,"revision":3}`, 0
		case c.method == "POST":
			return 201, `{"id":12,"state":"pending"}`, 0
		}
		return 404, `{}`, 0
	}}
	res, e = f.PushUpstream(ctx, up4, fk, 0, false, noSleep)
	f.ok(e)
	post = upCall{}
	for _, c := range up4.calls {
		if c.method == "POST" {
			post = c
		}
	}
	if res[0].Replaced != 1 || post.body["replaces"].(float64) != 55 || post.body["revision"].(float64) != 3 {
		t.Fatalf("%+v %+v", res, post)
	}
}

func TestUpstreamDryRunWritesNothing(t *testing.T) {
	f, _ := upFx(t)
	fk := f.pid["fakture"]
	f.ok(f.LinkUpstream(ctx, fk, 0, "hub", "shop", "", false))
	up := okUp()
	res, e := f.PushUpstream(ctx, up, fk, 0, true, noSleep)
	f.ok(e)
	if up.count("POST") != 0 || res[0].Created != 2 || !res[0].DryRun || !strings.HasPrefix(res[0].Lines[0], "would create") {
		t.Fatalf("%+v %v", res, up.calls)
	}
	ls, _ := f.UpstreamLinks(ctx, fk, 0)
	if ls[0].Pending != 2 || ls[0].LastPush != nil || ls[0].LastError != "" {
		t.Fatalf("a dry run changed the link: %+v", ls[0])
	}
	var n int
	f.ok(f.read(ctx, func(tx *sql.Tx) error { return tx.QueryRow(`SELECT count(*) FROM upstream_sent`).Scan(&n) }))
	if n != 0 {
		t.Fatal("a dry run wrote receipts")
	}
	// a refusal in a dry run is not remembered either
	bad := &fakeUp{handler: func(upCall) (int, string, time.Duration) { return 403, `{"error":{"message":"No project access"}}`, 0 }}
	res, e = f.PushUpstream(ctx, bad, fk, 0, true, noSleep)
	f.ok(e)
	if res[0].Stopped == "" {
		t.Fatal("no refusal shown")
	}
	if ls, _ = f.UpstreamLinks(ctx, fk, 0); ls[0].LastError != "" {
		t.Fatal("a dry run remembered a refusal")
	}
}

func TestUpstreamRefusalStopsAndIsRemembered(t *testing.T) {
	f, _ := upFx(t)
	fk := f.pid["fakture"]
	f.ok(f.LinkUpstream(ctx, fk, 0, "hub", "shop", "", false))
	// the bound user there is only a reader: the upstream refuses; the text is the upstream's, as one clean line
	up := &fakeUp{handler: func(c upCall) (int, string, time.Duration) {
		if c.method == "GET" {
			return 200, `{"id":7,"slug":"shop"}`, 0
		}
		return 403, `{"error":{"type":"forbidden","message":"not allowed:\u001b[31m your role\n in this project  does not permit it"}}`, 0
	}}
	res, e := f.PushUpstream(ctx, up, fk, 0, false, noSleep)
	f.ok(e)
	if res[0].Refused != 1 || res[0].Created != 0 || res[0].Stopped == "" || up.count("POST") != 1 { // the second note is not even tried
		t.Fatalf("%+v", res)
	}
	for _, l := range append(res[0].Lines, res[0].Stopped) {
		if strings.ContainsAny(l, "\x1b\n ") {
			t.Fatalf("control characters of the upstream's text were passed on: %q", l)
		}
	}
	ls, _ := f.UpstreamLinks(ctx, fk, 0)
	if ls[0].LastPush != nil || !strings.Contains(ls[0].LastError, "role") || ls[0].Pending != 2 {
		t.Fatalf("%+v", ls[0])
	}
	// a later success clears the refusal
	if _, e = f.PushUpstream(ctx, okUp(), fk, 0, false, noSleep); e != nil {
		t.Fatal(e)
	}
	if ls, _ = f.UpstreamLinks(ctx, fk, 0); ls[0].LastError != "" || ls[0].LastPush == nil {
		t.Fatalf("%+v", ls[0])
	}
}

func TestUpstreamAnswersAreData(t *testing.T) {
	f, _ := upFx(t)
	fk := f.pid["fakture"]
	f.ok(f.LinkUpstream(ctx, fk, 0, "hub", "shop", "", false))
	count := func() (n int) {
		f.ok(f.read(ctx, func(tx *sql.Tx) error {
			return tx.QueryRow(`SELECT (SELECT count(*) FROM notes)+(SELECT count(*) FROM projects)+(SELECT count(*) FROM upstreams)+(SELECT count(*) FROM upstream_links)`).Scan(&n)
		}))
		return
	}
	before := count()
	// an answer for another project than the one asked for, a huge answer, junk, a negative id: none of it is believed
	for name, h := range map[string]func(upCall) (int, string, time.Duration){
		"other project": func(upCall) (int, string, time.Duration) { return 200, `{"id":7,"slug":"secret"}`, 0 },
		"huge": func(upCall) (int, string, time.Duration) {
			return 200, `{"id":7,"slug":"shop","x":"` + strings.Repeat("a", 2<<20) + `"}`, 0
		},
		"junk":        func(upCall) (int, string, time.Duration) { return 200, `<html>`, 0 },
		"negative id": func(upCall) (int, string, time.Duration) { return 200, `{"id":-5,"slug":"shop"}`, 0 },
	} {
		res, e := f.PushUpstream(ctx, &fakeUp{handler: h}, fk, 0, false, noSleep)
		f.ok(e)
		if res[0].Stopped == "" || res[0].Created != 0 {
			t.Errorf("%s: %+v", name, res[0])
		}
	}
	// a submission answer without an id is a refusal of that note, not a receipt
	up := &fakeUp{handler: func(c upCall) (int, string, time.Duration) {
		if c.method == "GET" {
			return 200, `{"id":7,"slug":"shop"}`, 0
		}
		return 201, `{"state":"pending","id":0,"note":99999}`, 0
	}}
	res, e := f.PushUpstream(ctx, up, fk, 0, false, noSleep)
	f.ok(e)
	if res[0].Refused != 2 || res[0].Created != 0 {
		t.Fatalf("%+v", res[0])
	}
	if count() != before {
		t.Fatal("an answer of the upstream changed a note, project, upstream or link here")
	}
}

func TestUpstreamWaitsOutTheRateLimit(t *testing.T) {
	f, _ := upFx(t)
	fk := f.pid["fakture"]
	f.ok(f.LinkUpstream(ctx, fk, 0, "hub", "shop", "", false))
	limited, next := 0, 0
	up := &fakeUp{handler: func(c upCall) (int, string, time.Duration) {
		switch {
		case c.method == "GET":
			return 200, `{"id":7,"slug":"shop"}`, 0
		case limited < 2:
			limited++
			return 429, `{"error":{"message":"slow down"}}`, 3 * time.Second
		}
		next++
		return 201, fmt.Sprintf(`{"id":%d}`, next), 0
	}}
	var waited []time.Duration
	res, e := f.PushUpstream(ctx, up, fk, 0, false, func(d time.Duration) { waited = append(waited, d) })
	f.ok(e)
	if res[0].Created != 2 || len(waited) != 2 || waited[0] != 3*time.Second {
		t.Fatalf("%+v %v", res[0], waited)
	}
	// a limit that never ends is not waited out for ever
	forever := &fakeUp{handler: func(c upCall) (int, string, time.Duration) {
		if c.method == "GET" {
			return 200, `{"id":7,"slug":"shop"}`, 0
		}
		return 429, `{}`, time.Hour
	}}
	f.ok(f.UnlinkUpstream(ctx, fk, 0, "hub"))
	f.ok(f.LinkUpstream(ctx, fk, 0, "hub", "shop", "", false))
	waited = nil
	res, e = f.PushUpstream(ctx, forever, fk, 0, false, func(d time.Duration) { waited = append(waited, d) })
	f.ok(e)
	if res[0].Stopped == "" || len(waited) != maxRateWaits || waited[0] > maxRateWait {
		t.Fatalf("%+v %v", res[0], waited)
	}
}

func TestUpstreamPushOfATeamsNotes(t *testing.T) {
	f := newSeg(t)
	f.ok(f.AddUpstream(ctx, "hub", "hub.example"))
	f.put("fani", Structure, "Team rollout")
	f.ok(f.LinkUpstream(ctx, 0, f.acme, "hub", "acme-hub", "", false))
	up := &fakeUp{handler: func(c upCall) (int, string, time.Duration) {
		switch {
		case c.method == "GET" && c.path == "/minigw/v1/teams":
			return 200, fmt.Sprintf(`[{"id":4,"name":"Acme-Hub","segment":%d}]`, segmentID(4)), 0
		case c.method == "POST" && c.path == fmt.Sprintf("/minigw/v1/projects/%d/kb/submissions", segmentID(4)):
			return 201, `{"id":1}`, 0
		}
		return 404, `{}`, 0
	}}
	res, e := f.PushUpstream(ctx, up, 0, f.acme, false, noSleep)
	f.ok(e)
	if len(res) != 1 || res[0].Created != 1 || res[0].Label != "team acme" {
		t.Fatalf("%+v %v", res, up.calls)
	}
	// not on that team there: stops with a clear text
	none := &fakeUp{handler: func(c upCall) (int, string, time.Duration) { return 200, `[]`, 0 }}
	f.ok(f.UnlinkUpstream(ctx, 0, f.acme, "hub"))
	f.ok(f.LinkUpstream(ctx, 0, f.acme, "hub", "acme-hub", "", false))
	if res, e = f.PushUpstream(ctx, none, 0, f.acme, false, noSleep); e != nil || !strings.Contains(res[0].Stopped, "not on team") {
		t.Fatal(res, e)
	}
}

func TestUpstreamTablesCarryNoSecretAndRemoveCleans(t *testing.T) {
	f, _ := upFx(t)
	fk := f.pid["fakture"]
	f.ok(f.LinkUpstream(ctx, fk, 0, "hub", "shop", "", false))
	_, e := f.PushUpstream(ctx, okUp(), fk, 0, false, noSleep)
	f.ok(e)
	f.ok(f.RemoveUpstream(ctx, "hub"))
	var n int
	f.ok(f.read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT (SELECT count(*) FROM upstreams)+(SELECT count(*) FROM upstream_links)+(SELECT count(*) FROM upstream_sent)`).Scan(&n)
	}))
	if n != 0 {
		t.Fatal("links or receipts stayed behind", n)
	}
	is(t, f.RemoveUpstream(ctx, "hub"), ErrNotFound)
}
