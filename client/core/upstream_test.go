package core

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
)

type upCall struct {
	method, path string
	body         map[string]any
	key          string
}

func TestResolveUpstreamRefusesAmbiguousNote(t *testing.T) {
	for _, ours := range []bool{false, true} {
		t.Run(fmt.Sprint(ours), func(t *testing.T) {
			f, notes := upFx(t)
			fk := f.pid["fakture"]
			n := notes["Structure one"]
			f.ok(f.AddUpstream(ctx, "other", "127.0.0.1:8766"))
			yes := true
			for _, up := range []string{"hub", "other"} {
				f.ok(f.LinkUpstreamWith(ctx, LinkSpec{Project: fk, Upstream: up, Remote: "shop", Mode: "publish", Auto: &yes}))
				f.ok(f.upstreamRecordPublished(ctx, fk, up, UpstreamNote{ID: n.ID, Hash: upstreamHash(n), Revision: n.Revision}, 55, 1))
				f.ok(f.upstreamRecordConflict(ctx, fk, up, n.ID, 2))
			}
			f.ok(f.write(ctx, func(tx *sql.Tx) error {
				_, e := tx.Exec(`UPDATE upstream_links SET dirty_since=0 WHERE project_id=?`, fk)
				return e
			}))
			e := f.ResolveUpstream(ctx, fk, 0, n.ID, ours)
			if e == nil || !strings.Contains(e.Error(), "hub") || !strings.Contains(e.Error(), "other") {
				t.Fatalf("ambiguous resolve: %v", e)
			}
			ls, e := f.UpstreamLinks(ctx, fk, 0)
			f.ok(e)
			for _, l := range ls {
				if len(l.Conflicts) != 1 {
					t.Fatalf("resolved %s", l.Upstream)
				}
			}
			f.ok(f.ResolveUpstream(ctx, fk, 0, n.ID, ours, "hub"))
			ls, e = f.UpstreamLinks(ctx, fk, 0)
			f.ok(e)
			for _, l := range ls {
				if l.Upstream == "other" {
					if len(l.Conflicts) != 1 || l.DirtySince != nil {
						t.Fatalf("changed other link: %+v", l)
					}
				} else {
					if len(l.Conflicts) != 0 || (l.DirtySince != nil) != ours {
						t.Fatalf("selected link: %+v", l)
					}
				}
			}
		})
	}
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
		case c.method == "GET" && strings.HasPrefix(c.path, "/minigw/v1/projects/7/kb/submissions/"): // an earlier submission was rejected: sent again
			return 200, `{"state":"rejected","note":0}`, 0
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

func noSleep(time.Duration) bool { return true }

func TestUpstreamAddressAndName(t *testing.T) {
	// the same fixture is asserted by the Mac app (Tests/RouterCoreTests/UpstreamTests.swift): the two normalizers cannot drift
	raw, err := os.ReadFile("../testdata/upstream-addresses.json")
	if err != nil {
		t.Skip("fixture not here:", err)
	}
	var fx struct {
		Valid   map[string]string
		Invalid []string
	}
	if json.Unmarshal(raw, &fx) != nil || len(fx.Valid) < 5 || len(fx.Invalid) < 15 {
		t.Fatal("bad fixture")
	}
	for in, want := range fx.Valid {
		if got, e := NormalizeUpstreamAddress(in); e != nil || got != want {
			t.Errorf("%q: %q %v, want %q", in, got, e, want)
		}
	}
	for _, in := range fx.Invalid {
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
	if _, e = f.PushUpstream(ctx, up, fk, 0, "", 0, false, noSleep); e == nil || len(up.calls) != 0 {
		t.Fatal("a push without a link", e, up.calls)
	}
}

func TestUpstreamPushCreateSkipReplace(t *testing.T) {
	f, notes := upFx(t)
	fk := f.pid["fakture"]
	f.ok(f.LinkUpstream(ctx, fk, 0, "hub", "shop", "", false))
	up := okUp()
	res, e := f.PushUpstream(ctx, up, fk, 0, "", 0, false, noSleep)
	f.ok(e)
	if len(res) != 1 || res[0].Created != 2 || res[0].Refused != 0 || up.count("POST") != 2 {
		t.Fatalf("%+v %v", res, up.calls)
	}
	titles := ""
	for _, c := range up.calls {
		if c.method == "POST" {
			titles += c.body["title"].(string) + "|"
			body := c.body["body"].(string)
			// minimal: an opaque reference and the author's display name; never the gateway id, the note id, the revision or a host name
			if !strings.Contains(body, "<!-- minigw upstream ref ") || !strings.Contains(body, ", author ana -->") || strings.Contains(body, f.GatewayID()) || strings.Contains(body, "revision") || strings.Contains(body, "local note") {
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
	res, e = f.PushUpstream(ctx, up2, fk, 0, "", 0, false, noSleep)
	f.ok(e)
	if res[0].Skipped != 2 || res[0].Created != 0 || up2.count("POST") != 0 {
		t.Fatalf("%+v", res)
	}
	// a changed note, the first submission still pending: nothing more is sent for it (a second submission could be accepted too: two notes)
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
	res, e = f.PushUpstream(ctx, up3, fk, 0, "", 0, false, noSleep)
	f.ok(e)
	if res[0].Replaced != 0 || res[0].Created != 0 || res[0].Waiting != 1 || res[0].Refused != 0 || res[0].Skipped != 1 || up3.count("POST") != 0 || !strings.Contains(res[0].Lines[0], "still pending") {
		t.Fatalf("%+v", res)
	}
	var post upCall
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
		case c.method == "GET" && c.path == "/minigw/v1/projects/7/kb/submissions/1":
			return 200, `{"id":1,"state":"accepted","note":55}`, 0
		case c.method == "GET" && c.path == "/minigw/v1/projects/7/notes/55":
			return 200, `{"id":55,"revision":3}`, 0
		case c.method == "POST":
			return 201, `{"id":12,"state":"pending"}`, 0
		}
		return 404, `{}`, 0
	}}
	res, e = f.PushUpstream(ctx, up4, fk, 0, "", 0, false, noSleep)
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
	res, e := f.PushUpstream(ctx, up, fk, 0, "", 0, true, noSleep)
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
	res, e = f.PushUpstream(ctx, bad, fk, 0, "", 0, true, noSleep)
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
	// the bound user lost its role there (the hub's explicit link-wide answer, no_project_access; a plain forbidden concerns one note only); the text is the upstream's, as one clean line
	up := &fakeUp{handler: func(c upCall) (int, string, time.Duration) {
		if c.method == "GET" {
			return 200, `{"id":7,"slug":"shop"}`, 0
		}
		return 403, `{"error":{"type":"no_project_access","message":"not allowed:\u001b[31m your role\n in this project  does not permit it"}}`, 0
	}}
	res, e := f.PushUpstream(ctx, up, fk, 0, "", 0, false, noSleep)
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
	if _, e = f.PushUpstream(ctx, okUp(), fk, 0, "", 0, false, noSleep); e != nil {
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
		res, e := f.PushUpstream(ctx, &fakeUp{handler: h}, fk, 0, "", 0, false, noSleep)
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
	res, e := f.PushUpstream(ctx, up, fk, 0, "", 0, false, noSleep)
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
	res, e := f.PushUpstream(ctx, up, fk, 0, "", 0, false, func(d time.Duration) bool { waited = append(waited, d); return true })
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
	res, e = f.PushUpstream(ctx, forever, fk, 0, "", 0, false, func(d time.Duration) bool { waited = append(waited, d); return true })
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
	res, e := f.PushUpstream(ctx, up, 0, f.acme, "", 0, false, noSleep)
	f.ok(e)
	if len(res) != 1 || res[0].Created != 1 || res[0].Label != "team acme" {
		t.Fatalf("%+v %v", res, up.calls)
	}
	// not on that team there: stops with a clear text
	none := &fakeUp{handler: func(c upCall) (int, string, time.Duration) { return 200, `[]`, 0 }}
	f.ok(f.UnlinkUpstream(ctx, 0, f.acme, "hub"))
	f.ok(f.LinkUpstream(ctx, 0, f.acme, "hub", "acme-hub", "", false))
	if res, e = f.PushUpstream(ctx, none, 0, f.acme, "", 0, false, noSleep); e != nil || !strings.Contains(res[0].Stopped, "not on team") {
		t.Fatal(res, e)
	}
}

func TestUpstreamTablesCarryNoSecretAndRemoveCleans(t *testing.T) {
	f, _ := upFx(t)
	fk := f.pid["fakture"]
	f.ok(f.LinkUpstream(ctx, fk, 0, "hub", "shop", "", false))
	_, e := f.PushUpstream(ctx, okUp(), fk, 0, "", 0, false, noSleep)
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

// A call with a time budget (the Mac helper answers one request within 30 seconds) ends with More, not with a fault, and the next call
// goes on where it stopped: what was sent is remembered, nothing is sent twice.
func TestUpstreamPushInSteps(t *testing.T) {
	f, _ := upFx(t)
	fk := f.pid["fakture"]
	f.ok(f.LinkUpstream(ctx, fk, 0, "hub", "shop", "", false))
	// the limit cannot be waited out within this call
	limited := &fakeUp{handler: func(c upCall) (int, string, time.Duration) {
		if c.method == "GET" {
			return 200, `{"id":7,"slug":"shop"}`, 0
		}
		return 429, `{}`, 30 * time.Second
	}}
	res, e := f.PushUpstream(ctx, limited, fk, 0, "", 0, false, func(time.Duration) bool { return false })
	f.ok(e)
	if !res[0].More || res[0].Wait != 30 || res[0].Refused != 0 || res[0].Created != 0 {
		t.Fatalf("%+v", res[0])
	}
	if ls, _ := f.UpstreamLinks(ctx, fk, 0); ls[0].LastError != "" || ls[0].Pending != 2 {
		t.Fatalf("a limit that has to be waited for is not a refusal: %+v", ls[0])
	}
	// the budget ends after the first note: More; the next call sends the second and skips the first
	bctx, cancel := context.WithCancel(ctx)
	posts := 0
	up := &fakeUp{handler: func(c upCall) (int, string, time.Duration) {
		switch {
		case c.method == "GET":
			return 200, `{"id":7,"slug":"shop"}`, 0
		default:
			posts++
			cancel()
			return 201, fmt.Sprintf(`{"id":%d}`, posts), 0
		}
	}}
	res, e = f.PushUpstream(bctx, up, fk, 0, "", 0, false, noSleep)
	f.ok(e)
	if !res[0].More || res[0].Created != 1 || posts != 1 {
		t.Fatalf("%+v", res[0])
	}
	res, e = f.PushUpstream(ctx, okUp(), fk, 0, "", 0, false, noSleep)
	f.ok(e)
	if res[0].More || res[0].Created != 1 || res[0].Skipped != 1 {
		t.Fatalf("%+v", res[0])
	}
}

// A link with more notes than one run sends: each call goes further (the continuation is the last note dealt with, also when a note was
// refused), the end state has a receipt for every accepted note, and a dry run reports all of them.
func TestUpstreamPushOfManyNotesGetsPastTheLimit(t *testing.T) {
	t.Run("with the continuation", func(t *testing.T) { pushManyNotes(t, true) })
	t.Run("a plain push again each time", func(t *testing.T) { pushManyNotes(t, false) }) // unchanged notes are passed over, not counted
}

func pushManyNotes(t *testing.T, cursor bool) {
	f := newFx(t)
	f.ok(f.AddUpstream(ctx, "hub", "hub.example"))
	fk := f.pid["fakture"]
	const total = 2500
	f.ok(f.write(ctx, func(tx *sql.Tx) error { // the notes are written directly: 2500 API calls would test nothing more
		for i := 1; i <= total; i++ {
			if _, e := tx.Exec(`INSERT INTO notes(project_id,class,title,body,bytes,author_id,created,updated,revision,imported,entry_kind) VALUES(?,?,?,?,?,?,?,?,1,0,'finding')`,
				fk, "data", fmt.Sprintf("n%04d", i), "body", 4, f.uid["ana"], 1, 1); e != nil {
				return e
			}
		}
		return nil
	}))
	f.ok(f.LinkUpstream(ctx, fk, 0, "hub", "shop", "", false))
	// a dry run reports all 2500 in one call
	res, e := f.PushUpstream(ctx, okUp(), fk, 0, "", 0, true, noSleep)
	f.ok(e)
	if res[0].More || res[0].Created != total || len(res[0].Lines) != maxReportLines+1 { // the report is bounded, the totals are not
		t.Fatalf("dry run: more=%v created=%d lines=%d", res[0].More, res[0].Created, len(res[0].Lines))
	}
	// the upstream refuses the first note for good (it stays unsent) and takes the rest
	next := 0
	up := &fakeUp{handler: func(c upCall) (int, string, time.Duration) {
		switch {
		case c.method == "GET":
			return 200, `{"id":7,"slug":"shop"}`, 0
		case c.body["title"] == "n0001":
			return 400, `{"error":{"message":"title refused"}}`, 0
		}
		next++
		return 201, fmt.Sprintf(`{"id":%d}`, next), 0
	}}
	var after int64
	sent, calls := 0, 0
	for {
		calls++
		res, e = f.PushUpstream(ctx, up, fk, 0, "hub", after, false, noSleep)
		f.ok(e)
		sent += res[0].Created
		if !res[0].More {
			break
		}
		if cursor && res[0].Next <= after {
			t.Fatalf("call %d did not get further: next %d after %d", calls, res[0].Next, after)
		}
		if cursor {
			after = res[0].Next
		}
		if calls > 5 {
			t.Fatal("too many calls")
		}
	}
	if calls != 3 || sent != total-1 {
		t.Fatalf("%d calls, %d sent", calls, sent)
	}
	var n int
	f.ok(f.read(ctx, func(tx *sql.Tx) error { return tx.QueryRow(`SELECT count(*) FROM upstream_sent`).Scan(&n) }))
	if n != total-1 {
		t.Fatalf("%d receipts", n)
	}
	// a continuation must name its link
	if _, e = f.PushUpstream(ctx, up, 0, 0, "", 5, false, noSleep); e == nil {
		t.Fatal("a bare continuation was accepted")
	}
	if _, e = f.PushUpstream(ctx, up, fk, 0, "hub", -1, false, noSleep); e == nil {
		t.Fatal("a negative continuation was accepted")
	}
}

// The report of a big link is bounded: totals count everything, the lines stop at maxReportLines with "... and K more". A dry run is never
// continued (no more), also not when its time budget ends.
func TestUpstreamBigReportsAreBounded(t *testing.T) {
	f := newFx(t)
	f.ok(f.AddUpstream(ctx, "hub", "hub.example"))
	fk := f.pid["fakture"]
	f.ok(f.write(ctx, func(tx *sql.Tx) error {
		for i := 1; i <= 450; i++ {
			if _, e := tx.Exec(`INSERT INTO notes(project_id,class,title,body,bytes,author_id,created,updated,revision,imported,entry_kind) VALUES(?,?,?,?,?,?,?,?,1,0,'finding')`,
				fk, "data", fmt.Sprintf("n%04d", i), "body", 4, f.uid["ana"], 1, 1); e != nil {
				return e
			}
		}
		return nil
	}))
	f.ok(f.LinkUpstream(ctx, fk, 0, "hub", "shop", "", false))
	res, e := f.PushUpstream(ctx, okUp(), fk, 0, "", 0, true, noSleep)
	f.ok(e)
	r := res[0]
	if r.Created != 450 || r.More || len(r.Lines) != maxReportLines+1 || r.Lines[maxReportLines] != "... and 250 more" {
		t.Fatalf("created %d more %v lines %d last %q", r.Created, r.More, len(r.Lines), r.Lines[len(r.Lines)-1])
	}
	// a dry run whose budget is used up says where it stopped; it does not ask to be continued
	dctx, cancel := context.WithCancel(ctx)
	budget := &fakeUp{handler: func(c upCall) (int, string, time.Duration) {
		cancel() // the budget of the call ends while it talks to the upstream
		return 200, `{"id":7,"slug":"shop"}`, 0
	}}
	res, e = f.PushUpstream(dctx, budget, fk, 0, "", 0, true, noSleep)
	f.ok(e)
	if res[0].More || res[0].Next != 0 || res[0].Stopped == "" {
		t.Fatalf("%+v", res[0])
	}
	// the sends per run are bounded by maxPushNotes, counted in notes SENT: the unchanged ones passed over do not count
	old := maxPushNotes
	maxPushNotes = 100
	defer func() { maxPushNotes = old }()
	calls := 0
	var after int64
	for {
		calls++
		res, e = f.PushUpstream(ctx, okUp(), fk, 0, "hub", after, false, noSleep)
		f.ok(e)
		if res[0].Created > 100 {
			t.Fatalf("%d sent in one run", res[0].Created)
		}
		if !res[0].More {
			break
		}
		after = res[0].Next
		if calls > 10 {
			t.Fatal("no end")
		}
	}
	if calls != 5 {
		t.Fatalf("%d calls", calls)
	}
}

// Archived projects and teams: not linked, and nothing is sent for one that was archived after it was linked.
func TestUpstreamArchivedIsRefused(t *testing.T) {
	f, _ := upFx(t)
	fk := f.pid["fakture"]
	f.ok(f.LinkUpstream(ctx, fk, 0, "hub", "shop", "", false))
	f.ok(f.SetProjectArchived(ctx, fk, true))
	up := okUp()
	res, e := f.PushUpstream(ctx, up, fk, 0, "", 0, false, noSleep)
	f.ok(e)
	if res[0].Stopped == "" || !strings.Contains(res[0].Stopped, "archived") || len(up.calls) != 0 {
		t.Fatalf("%+v %v", res[0], up.calls)
	}
	var ref ErrRefusal
	if e = f.LinkUpstream(ctx, f.pid["import"], 0, "hub", "xx", "", false); e != nil {
		t.Fatal(e)
	}
	f.ok(f.SetProjectArchived(ctx, f.pid["import"], true))
	if e = f.LinkUpstream(ctx, f.pid["import"], 0, "hub", "xx", "", false); !errors.As(e, &ref) || !strings.Contains(ref.Msg, "archived") {
		t.Fatal(e)
	}
	g := newSeg(t)
	g.ok(g.AddUpstream(ctx, "hub", "hub.example"))
	g.ok(g.SetTeamArchived(ctx, g.acme, true))
	if e = g.LinkUpstream(ctx, 0, g.acme, "hub", "x", "", false); !errors.As(e, &ref) || !strings.Contains(ref.Msg, "archived") {
		t.Fatal(e)
	}
}

// What leaves is not a hash of guessable numbers: the idempotency key and the reference are HMACs under this gateway's own secret, so an
// upstream cannot recover project or note ids from them, and two gateways send different keys for the same ids.
func TestUpstreamKeysDoNotRevealIDs(t *testing.T) {
	keys := func() (string, string) {
		f, notes := upFx(t)
		fk := f.pid["fakture"]
		f.ok(f.LinkUpstream(ctx, fk, 0, "hub", "shop", "", false))
		up := okUp()
		_, e := f.PushUpstream(ctx, up, fk, 0, "", 0, false, noSleep)
		f.ok(e)
		var key, ref string
		for _, c := range up.calls {
			if c.method == "POST" && c.body["title"] == "Structure one" {
				key = c.key
				body := c.body["body"].(string)
				ref = body[strings.Index(body, "ref ")+4 : strings.Index(body, ", author")]
			}
		}
		// the old, guessable form: sha256 over the plain numbers
		n := notes["Structure one"]
		h := sha256.Sum256([]byte(fmt.Sprint("hub", "|", fk, "|", n.ID, "|", upstreamHash(n), "|", 0)))
		if key == "" || key == "up-"+hex.EncodeToString(h[:16]) {
			t.Fatalf("key %q is the plain hash", key)
		}
		// a second push of the same note is the same key (a repeat must be recognised)
		up2 := okUp()
		f.clock = f.clock.Add(time.Second)
		f.ok(f.UnlinkUpstream(ctx, fk, 0, "hub"))
		f.ok(f.LinkUpstream(ctx, fk, 0, "hub", "shop", "", false))
		_, e = f.PushUpstream(ctx, up2, fk, 0, "", 0, false, noSleep)
		f.ok(e)
		for _, c := range up2.calls {
			if c.method == "POST" && c.body["title"] == "Structure one" && c.key != key {
				t.Fatalf("the key of the same note changed: %q %q", key, c.key)
			}
		}
		return key, ref
	}
	k1, r1 := keys()
	k2, r2 := keys() // another gateway, the same ids
	if k1 == k2 || r1 == r2 {
		t.Fatalf("two gateways sent the same key or reference: %q %q", k1, k2)
	}
}

// The paths the core sends are exactly the ones the Mac app admits (client/testdata/upstream-requests.json, asserted by both).
func TestUpstreamRequestsMatchTheSharedFixture(t *testing.T) {
	raw, err := os.ReadFile("../testdata/upstream-requests.json")
	if err != nil {
		t.Skip("fixture not here:", err)
	}
	var fx struct {
		Patterns []struct{ Method, Pattern string }
		Refused  []struct{ Method, Path string }
	}
	if json.Unmarshal(raw, &fx) != nil || len(fx.Patterns) < 5 {
		t.Fatal("bad fixture")
	}
	admitted := func(method, path string) bool {
		for _, p := range fx.Patterns {
			if p.Method == method && regexp.MustCompile(p.Pattern).MatchString(path) {
				return true
			}
		}
		return false
	}
	for _, r := range fx.Refused {
		if admitted(r.Method, r.Path) {
			t.Errorf("%s %s is admitted by the fixture's patterns", r.Method, r.Path)
		}
	}
	var seen []upCall
	rec := func(h func(c upCall) (int, string, time.Duration)) *fakeUp {
		return &fakeUp{handler: func(c upCall) (int, string, time.Duration) { seen = append(seen, c); return h(c) }}
	}
	// a project push, a replacement of an accepted note, a team push, a probe-like listing
	f, notes := upFx(t)
	fk := f.pid["fakture"]
	f.ok(f.LinkUpstream(ctx, fk, 0, "hub", "shop", "", false))
	ok := okUp().handler
	_, e := f.PushUpstream(ctx, rec(ok), fk, 0, "", 0, false, noSleep)
	f.ok(e)
	n := notes["Structure one"]
	f.clock = f.clock.Add(time.Second)
	_, e = f.UpdateNote(ctx, f.p("ana"), fk, n.ID, n.Revision, NoteInput{Class: Structure, Title: "Structure one", Body: "again"})
	f.ok(e)
	_, e = f.PushUpstream(ctx, rec(func(c upCall) (int, string, time.Duration) {
		switch {
		case c.path == "/minigw/v1/projects/shop":
			return 200, `{"id":7,"slug":"shop"}`, 0
		case c.method == "GET" && strings.Contains(c.path, "/kb/submissions/"):
			return 200, `{"id":1,"state":"accepted","note":55}`, 0
		case c.method == "GET":
			return 200, `{"id":55,"revision":3}`, 0
		}
		return 201, `{"id":9}`, 0
	}), fk, 0, "", 0, false, noSleep)
	f.ok(e)
	// publish mode: a note is created, then updated in place
	pf, pnotes := upFx(t)
	pk := pf.pid["fakture"]
	pf.ok(pf.LinkUpstreamWith(ctx, LinkSpec{Project: pk, Upstream: "hub", Remote: "shop", Mode: "publish"}))
	pub := func(c upCall) (int, string, time.Duration) {
		switch {
		case c.path == "/minigw/v1/projects/shop":
			return 200, `{"id":7,"slug":"shop"}`, 0
		case c.method == "POST":
			return 201, `{"id":55,"revision":1}`, 0
		case c.method == "PUT":
			return 200, `{"id":55,"revision":2}`, 0
		}
		return 200, `{"id":55,"revision":1}`, 0
	}
	_, e = pf.PushUpstream(ctx, rec(pub), pk, 0, "", 0, false, noSleep)
	pf.ok(e)
	pn := pnotes["Structure one"]
	pf.clock = pf.clock.Add(time.Second)
	_, e = pf.UpdateNote(ctx, pf.p("ana"), pk, pn.ID, pn.Revision, NoteInput{Class: Structure, Title: "Structure one", Body: "again"})
	pf.ok(e)
	_, e = pf.PushUpstream(ctx, rec(pub), pk, 0, "", 0, false, noSleep)
	pf.ok(e)
	g := newSeg(t)
	g.ok(g.AddUpstream(ctx, "hub", "hub.example"))
	g.put("fani", Structure, "Team rollout")
	g.ok(g.LinkUpstream(ctx, 0, g.acme, "hub", "acme-hub", "", false))
	_, e = g.PushUpstream(ctx, rec(func(c upCall) (int, string, time.Duration) {
		if c.method == "GET" {
			return 200, fmt.Sprintf(`[{"id":4,"name":"Acme-Hub","segment":%d}]`, segmentID(4)), 0
		}
		return 201, `{"id":1}`, 0
	}), 0, g.acme, "", 0, false, noSleep)
	g.ok(e)
	if len(seen) < 6 {
		t.Fatalf("only %d requests seen", len(seen))
	}
	var posted, put bool
	for _, c := range seen {
		posted = posted || (c.method == "POST" && strings.HasSuffix(c.path, "/notes"))
		put = put || c.method == "PUT"
	}
	if !posted || !put {
		t.Fatalf("publish mode sent no note create (%v) or update (%v)", posted, put)
	}
	for _, c := range seen {
		if !admitted(c.method, c.path) {
			t.Errorf("the core sent %s %s, which the fixture (and so the app) does not admit", c.method, c.path)
		}
	}
	// a project link names a short name as the gateways make them: anything else could not be a path the app admits
	for _, bad := range []string{"Shop", "a", "sh op", "12345", "../teams", "shop?x=1"} {
		if e := f.LinkUpstream(ctx, f.pid["eureko"], 0, "hub", bad, "", false); e == nil {
			t.Errorf("remote project %q accepted", bad)
		}
	}
}

// after "resolve --ours" the note is tracked as a submission pending review: a later local edit in publish mode sends another submission
// (never a PUT, never a second direct POST of the note), whether the upstream note changed or was gone.
func TestResolveOursThenEditStaysOnTheSubmissionPath(t *testing.T) {
	for _, gone := range []bool{false, true} {
		f, notes := upFx(t)
		fk := f.pid["fakture"]
		f.ok(f.LinkUpstreamWith(ctx, LinkSpec{Project: fk, Upstream: "hub", Remote: "shop", Mode: "publish"}))
		remote := func(c upCall) (int, string, time.Duration) { // the upstream note: moved on, or gone
			if gone {
				return 404, `{"error":{"message":"gone"}}`, 0
			}
			return 200, `{"id":55,"revision":9}`, 0
		}
		first := true
		up := &fakeUp{handler: func(c upCall) (int, string, time.Duration) {
			switch {
			case c.path == "/minigw/v1/projects/shop":
				return 200, `{"id":7,"slug":"shop"}`, 0
			case c.method == "POST" && strings.HasSuffix(c.path, "/kb/submissions"):
				return 201, `{"id":31,"state":"pending"}`, 0
			case c.method == "POST":
				return 201, `{"id":55,"revision":1}`, 0
			case strings.Contains(c.path, "/kb/submissions/"):
				return 200, `{"id":31,"state":"rejected"}`, 0
			case c.method == "GET" && !first:
				return remote(c)
			}
			return 200, `{"id":55,"revision":1}`, 0
		}}
		_, e := f.PushUpstream(ctx, up, fk, 0, "", 0, false, noSleep)
		f.ok(e)
		first = false
		n := notes["Structure one"]
		edit := func(body string) {
			f.clock = f.clock.Add(time.Second)
			cur, e := f.GetNote(ctx, f.p("ana"), fk, n.ID)
			f.ok(e)
			_, e = f.UpdateNote(ctx, f.p("ana"), fk, n.ID, cur.Revision, NoteInput{Class: Structure, Title: "Structure one", Body: body})
			f.ok(e)
		}
		edit("second")
		_, e = f.PushUpstream(ctx, up, fk, 0, "", 0, false, noSleep) // conflict
		f.ok(e)
		f.ok(f.ResolveUpstream(ctx, fk, 0, n.ID, true))
		_, e = f.PushUpstream(ctx, up, fk, 0, "", 0, false, noSleep) // the "ours" submission
		f.ok(e)
		before := len(up.calls)
		edit("third")
		r, e := f.PushUpstream(ctx, up, fk, 0, "", 0, false, noSleep)
		f.ok(e)
		if r[0].Conflicts != 0 || r[0].Replaced != 1 {
			t.Fatalf("gone=%v: %+v", gone, r[0])
		}
		for _, c := range up.calls[before:] {
			if c.method == "PUT" || (c.method == "POST" && strings.HasSuffix(c.path, "/notes")) {
				t.Fatalf("gone=%v: a direct write after resolve --ours: %s %s", gone, c.method, c.path)
			}
		}
	}
}
