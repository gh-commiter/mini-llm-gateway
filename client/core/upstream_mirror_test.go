package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// A scripted hub for the pull tests: its notes, its manifest and every way it can misbehave.
type hubNote struct {
	id, src, rev int64
	class        Class
	kind         string
	title, body  string
}

const hubSeg = int64(1<<40 + 1)

type hub struct {
	notes     []hubNote
	truncated bool
	proj      int                                                     // status of the project lookup (0: 200)
	manifest  func() (int, string)                                    // overrides the manifest answer
	note      func(id int64, def string) (int, string, time.Duration) // overrides a note answer
	asked     []int64                                                 // ids of the note GETs, in order
}

func (h *hub) manifestJSON() string {
	type mn struct {
		ID     int64  `json:"id"`
		Source int64  `json:"source"`
		Rev    int64  `json:"rev"`
		Class  string `json:"class"`
		Kind   string `json:"kind"`
	}
	m := struct {
		V         int              `json:"v"`
		Sources   []map[string]any `json:"sources"`
		Notes     []mn             `json:"notes"`
		Truncated bool             `json:"truncated"`
	}{V: 1, Sources: []map[string]any{{"id": 7, "label": "project:shop"}, {"id": hubSeg, "label": "team:acme"}}, Notes: []mn{}, Truncated: h.truncated}
	for _, n := range h.notes {
		m.Notes = append(m.Notes, mn{n.id, n.src, n.rev, string(n.class), n.kind})
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func (n hubNote) json() string {
	b, _ := json.Marshal(map[string]any{"id": n.id, "project": n.src, "class": n.class, "title": n.title, "body": n.body, "updated": "2026-11-17T09:00:00Z", "revision": n.rev, "entryKind": n.kind, "author": "x", "created": "2026-11-17T09:00:00Z"})
	return string(b)
}

func (h *hub) up() *fakeUp {
	return &fakeUp{handler: func(c upCall) (int, string, time.Duration) {
		switch {
		case c.method == "GET" && c.path == "/minigw/v1/projects/shop":
			if h.proj != 0 {
				return h.proj, `{"error":{"message":"project gone"}}`, 0
			}
			return 200, `{"id":7,"slug":"shop"}`, 0
		case c.method == "GET" && c.path == "/minigw/v1/projects/7/kb/manifest":
			if h.manifest != nil {
				st, b := h.manifest()
				if st == 429 {
					return st, b, 2 * time.Minute
				}
				return st, b, 0
			}
			return 200, h.manifestJSON(), 0
		case c.method == "GET" && strings.HasPrefix(c.path, "/minigw/v1/projects/7/notes/"):
			id, _ := strconv.ParseInt(strings.TrimPrefix(c.path, "/minigw/v1/projects/7/notes/"), 10, 64)
			h.asked = append(h.asked, id)
			def, st := `{"error":{"message":"not found"}}`, 404
			for _, n := range h.notes {
				if n.id == id {
					def, st = n.json(), 200
				}
			}
			if h.note != nil {
				return h.note(id, def)
			}
			return st, def, 0
		}
		return 404, `{"error":{"message":"not found"}}`, 0
	}}
}

func readFx(t *testing.T) (*fx, int64) {
	f, fk := pullFx(t)
	readOn(f, fk, "", "")
	return f, fk
}

func readOn(f *fx, project int64, classes, sources string) {
	f.t.Helper()
	yes := true
	var src *string
	if sources != "" {
		src = &sources
	}
	f.ok(f.LinkUpstreamWith(ctx, LinkSpec{Project: project, Upstream: "hub", Remote: "shop", Read: &yes, ReadClasses: classes, ReadSources: src, AcceptReach: true, ReadOnly: true}))
}

func structNote(id int64, title string) hubNote {
	return hubNote{id: id, src: 7, rev: 1, class: Structure, kind: KindFinding, title: title, body: "body of " + title}
}

// mirror lists the mirror of the link as "note/rev/class/kind/title".
func mirror(f *fx) (out []string) {
	f.t.Helper()
	rows, err := f.DB().Query(`SELECT remote_note,remote_rev,class,kind,title FROM upstream_mirror ORDER BY remote_note`)
	f.ok(err)
	defer rows.Close()
	for rows.Next() {
		var n, r int64
		var c, k, ti string
		f.ok(rows.Scan(&n, &r, &c, &k, &ti))
		out = append(out, fmt.Sprintf("%d/%d/%s/%s/%s", n, r, c, k, ti))
	}
	return
}

func postings(f *fx) (n int) {
	f.t.Helper()
	f.ok(f.DB().QueryRow(`SELECT count(*) FROM upstream_mirror_search`).Scan(&n))
	return
}

// notesState: everything local notes are, as one string: a pull must never change it.
func notesState(f *fx) (s string) {
	f.t.Helper()
	f.ok(f.DB().QueryRow(`SELECT count(*)||'|'||coalesce(sum(revision),0)||'|'||coalesce(group_concat(title||body,'~'),'') FROM notes`).Scan(&s))
	return
}

// countsOutside: the row counts of every table except the ones a pull may write.
func countsOutside(t *testing.T, f *fx) string {
	var keep []string
	for _, c := range strings.Split(tableCounts(t, f), ",") {
		if !strings.HasPrefix(c, "upstream_mirror") && !strings.HasPrefix(c, "audit_events=") && !strings.HasPrefix(c, "sqlite_") {
			keep = append(keep, c)
		}
	}
	return strings.Join(keep, ",")
}

func pull(f *fx, fk int64, up UpstreamTransport) UpstreamPull {
	f.t.Helper()
	res, err := f.PullUpstream(ctx, up, fk, 0, "", false, noSleep)
	f.ok(err)
	if len(res) != 1 {
		f.t.Fatal(res)
	}
	return res[0]
}

func linkOf(f *fx, fk int64) UpstreamLinkInfo {
	f.t.Helper()
	ls, err := f.UpstreamLinks(ctx, fk, 0)
	f.ok(err)
	return ls[0]
}

// ---- schema

func TestMigration23To24KeepsLinksAndSnapshots(t *testing.T) {
	dir := t.TempDir()
	db, err := openDB(filepath.Join(dir, dbFile))
	if err != nil {
		t.Fatal(err)
	}
	tx, _ := db.Begin()
	for _, m := range migrations[:23] {
		if err = m(tx); err != nil {
			t.Fatal(err)
		}
	}
	setMeta(tx, "schema", "23")
	for _, q := range []string{`INSERT INTO upstreams(name,address,created) VALUES('hub','h:8765',1)`, `INSERT INTO projects(id,slug,name) VALUES(1,'shop','Shop')`,
		`INSERT INTO upstream_links(project_id,upstream,remote,remote_team,classes,with_private,mode,auto) VALUES(1,'hub','shop',0,'structure',0,'publish',1)`} {
		if _, err = tx.Exec(q); err != nil {
			t.Fatal(q, err)
		}
	}
	tx.Commit()
	db.Close()
	s := openT(t, dir)
	defer s.Close()
	if snaps, _ := filepath.Glob(filepath.Join(dir, "core-23-*.db")); len(snaps) != 1 {
		t.Fatal("no snapshot of schema 23", snaps)
	}
	if v, _ := schemaVersion(ctx, s.DB()); v != len(migrations) {
		t.Fatal("schema", v)
	}
	var read, auto int
	var classes, mode, rc, rs string
	if err = s.DB().QueryRow(`SELECT read,read_classes,read_sources,classes,mode,auto FROM upstream_links`).Scan(&read, &rc, &rs, &classes, &mode, &auto); err != nil || read != 0 || rc != "structure" || rs != "" || classes != "structure" || mode != "publish" || auto != 1 {
		t.Fatal(read, rc, rs, classes, mode, auto, err) // the old link: reading off, its push settings as they were
	}
	if err = checkSchemaObjects(ctx, s.DB(), len(migrations)); err != nil {
		t.Fatal(err)
	}
	// an older program meeting this schema refuses it (as for every schema; TestNewerSchemaRefused): here the file claims one schema more
	s.Close()
	db, err = openDB(filepath.Join(dir, dbFile))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec(`UPDATE meta SET value=? WHERE key='schema'`, strconv.Itoa(len(migrations)+1)); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err = Open(dir, Options{}); !errors.Is(err, ErrNewerSchema) {
		t.Fatal(err)
	}
}

// ---- the link

func TestReadNeedsAcceptanceAndNeverTouchesPushSettings(t *testing.T) {
	f, fk := pullFx(t)
	yes := true
	for name, sp := range map[string]LinkSpec{
		"read":         {Read: &yes},
		"classes only": {ReadClasses: "data"},
		"sources only": {ReadSources: ptr("team:acme")},
	} {
		sp.Project, sp.Upstream, sp.Remote, sp.ReadOnly = fk, "hub", "shop", true
		e := f.LinkUpstreamWith(ctx, sp)
		if e == nil || !strings.Contains(e.Error(), "--accept-reach") {
			t.Fatalf("%s: %v", name, e)
		}
	}
	if l := linkOf(f, fk); l.Read {
		t.Fatalf("%+v", l)
	}
	// private is never read; odd lists are refused
	for _, c := range []string{"private", "structure,private", "x", "structure,structure"} {
		if e := f.LinkUpstreamWith(ctx, LinkSpec{Project: fk, Upstream: "hub", Remote: "shop", Read: &yes, ReadClasses: c, AcceptReach: true, ReadOnly: true}); e == nil {
			t.Fatalf("class list %q accepted", c)
		}
	}
	for _, sr := range []string{"shop", "project:", "project:a,project:a", "team:x\ny", strings.Repeat("project:a,", 17) + "project:b"} {
		if e := f.LinkUpstreamWith(ctx, LinkSpec{Project: fk, Upstream: "hub", Remote: "shop", Read: &yes, ReadSources: &sr, AcceptReach: true, ReadOnly: true}); e == nil {
			t.Fatalf("source list %q accepted", sr)
		}
	}
	no := false
	if e := f.LinkUpstreamWith(ctx, LinkSpec{Project: fk, Upstream: "hub", Remote: "shop", Read: &no, ReadClasses: "data", ReadOnly: true}); e == nil {
		t.Fatal("--no-read with a class list")
	}
	// push settings of the link: a publish link that pushes automatically, structure only
	auto := true
	f.ok(f.LinkUpstreamWith(ctx, LinkSpec{Project: fk, Upstream: "hub", Remote: "shop", Classes: "structure", Mode: "publish", Auto: &auto}))
	before := linkOf(f, fk)
	f.clock = f.clock.Add(time.Minute)
	f.ok(f.LinkUpstreamWith(ctx, LinkSpec{Project: fk, Upstream: "hub", Remote: "other-name", Classes: "data", Read: &yes, ReadClasses: "structure,data", ReadSources: ptr("team:acme"), AcceptReach: true, ReadOnly: true}))
	after := linkOf(f, fk)
	if !after.Read || after.Remote != "shop" || after.Mode != "publish" || !after.Auto || len(after.Classes) != 1 || after.Classes[0] != Structure || after.WithPrivate != before.WithPrivate ||
		classesCSV(after.ReadClasses) != "structure,data" || strings.Join(after.ReadSources, ",") != "team:acme" || after.ReadAccepted == nil || !after.ReadAccepted.Equal(f.clock) {
		t.Fatalf("%+v", after)
	}
	// a link with push settings AND reading changes both
	f.ok(f.LinkUpstreamWith(ctx, LinkSpec{Project: fk, Upstream: "hub", Remote: "shop", Classes: "structure,data", Read: &yes, AcceptReach: true}))
	if l := linkOf(f, fk); len(l.Classes) != 2 || !l.Read {
		t.Fatalf("%+v", l)
	}
	// a link without any read option keeps reading as it was
	f.ok(f.LinkUpstreamWith(ctx, LinkSpec{Project: fk, Upstream: "hub", Remote: "shop", Classes: "structure"}))
	if l := linkOf(f, fk); !l.Read || classesCSV(l.ReadClasses) != "structure,data" {
		t.Fatalf("%+v", l)
	}
	// a new link made by --read alone has the push defaults and sends nothing by itself
	f2, fk2 := newPullFxNoLink(t)
	readOn(f2, fk2, "", "")
	if l := linkOf(f2, fk2); !l.Read || l.Auto || l.Mode != "submit" {
		t.Fatalf("%+v", l)
	}
}

func newPullFxNoLink(t *testing.T) (*fx, int64) {
	f := newFx(t)
	f.ok(f.AddUpstream(ctx, "hub", "hub.example:8765"))
	return f, f.pid["fakture"]
}

// ---- the pull

func TestPullFillsTheMirrorAndOnlyThere(t *testing.T) {
	f, fk := readFx(t)
	f.mk("ana", "fakture", Structure) // a local note that must stay as it is
	localBefore, outsideBefore := notesState(f), countsOutside(t, f)
	h := &hub{notes: []hubNote{structNote(1, "One"), structNote(2, "Two"), {id: 3, src: 7, rev: 4, class: Data, kind: KindFinding, title: "Data three", body: "d"}, {id: 4, src: hubSeg, rev: 1, class: Structure, kind: KindFAQ, title: "Seg faq", body: "a"}}}
	up := h.up()
	res := pull(f, fk, up)
	if res.Stopped != "" || res.More || res.Added != 1+1 || res.Mirror != 2 || res.Skipped != 0 {
		t.Fatalf("%+v", res) // default: structure of the project itself only
	}
	if got := strings.Join(mirror(f), "|"); got != "1/1/structure/finding/One|2/1/structure/finding/Two" || postings(f) != 2 {
		t.Fatalf("%s %d", got, postings(f))
	}
	l := linkOf(f, fk)
	if l.LastPullOK == nil || l.LastPull == nil || l.PullError != "" || l.Mirror != 2 || l.MirrorBytes == 0 || l.ReadAccepted == nil {
		t.Fatalf("%+v", l)
	}
	if notesState(f) != localBefore || countsOutside(t, f) != outsideBefore {
		t.Fatal("a pull wrote outside the mirror:\n" + outsideBefore + "\n" + countsOutside(t, f))
	}
	// an unchanged hub: nothing is fetched again
	h.asked = nil
	res = pull(f, fk, up)
	if len(h.asked) != 0 || res.Added+res.Updated+res.Removed != 0 || res.Mirror != 2 {
		t.Fatalf("%v %+v", h.asked, res)
	}
	// a changed revision is fetched and replaces the row; a new note arrives; a note gone from the (complete) manifest is deleted
	h.notes[0].rev, h.notes[0].title = 2, "One v2"
	h.notes = append(h.notes[:1], h.notes[2:]...) // note 2 is gone upstream
	h.notes = append(h.notes, structNote(5, "Five"))
	h.asked = nil
	res = pull(f, fk, up)
	if res.Added != 1 || res.Updated != 1 || res.Removed != 1 || fmt.Sprint(h.asked) != "[1 5]" || res.Mirror != 2 {
		t.Fatalf("%v %+v", h.asked, res)
	}
	if got := strings.Join(mirror(f), "|"); got != "1/2/structure/finding/One v2|5/1/structure/finding/Five" || postings(f) != 2 {
		t.Fatalf("%s %d", got, postings(f))
	}
	var hits int
	f.ok(f.DB().QueryRow(`SELECT count(*) FROM upstream_mirror_search WHERE upstream_mirror_search MATCH 'v2'`).Scan(&hits))
	if hits != 1 {
		t.Fatal("the search index does not hold the new title")
	}
	// classes and sources are what the owner accepted: widening them deletes the mirror (re-fetched by the next pull), data and the segment come
	readOn(f, fk, "structure,data", "team:acme")
	if len(mirror(f)) != 0 || postings(f) != 0 {
		t.Fatal("the mirror survived a change of what is read")
	}
	res = pull(f, fk, up)
	if res.Added != 4 || res.Skipped != 0 || strings.Join(mirror(f), "|") != "1/2/structure/finding/One v2|3/4/data/finding/Data three|4/1/structure/faq/Seg faq|5/1/structure/finding/Five" {
		t.Fatalf("%+v %v", res, mirror(f))
	}
	if notesState(f) != localBefore {
		t.Fatal("local notes changed")
	}
	// the requests: only GETs, and only requests the Mac app admits (the shared fixture, which also refuses every other shape of them)
	raw, e := os.ReadFile("../testdata/upstream-requests.json")
	f.ok(e)
	var fixture struct {
		Patterns []struct{ Method, Pattern string }
	}
	f.ok(json.Unmarshal(raw, &fixture))
	for _, c := range up.calls {
		ok := false
		for _, p := range fixture.Patterns {
			ok = ok || p.Method == c.method && regexp.MustCompile(p.Pattern).MatchString(c.path)
		}
		if !ok || c.method != "GET" {
			t.Errorf("%s %s is not admitted by the shared fixture", c.method, c.path)
		}
	}
	// the audit row of a pull has counts, never a title or a body
	evs, err := f.AuditEvents(ctx, 0, 500)
	f.ok(err)
	n := 0
	for _, e := range evs {
		if e.Action == "upstream.pull" {
			n++
			if strings.Contains(string(e.Detail), "One") || strings.Contains(string(e.Detail), "body of") {
				t.Fatal(string(e.Detail))
			}
		}
	}
	if n == 0 {
		t.Fatal("no audit row")
	}
}

func TestPullSanitisesWhatItStores(t *testing.T) {
	f, fk := readFx(t)
	n := structNote(1, "Ti\u001b[31mtle‮\nsecond line")
	n.body = "line1\r\nline\u0007 2​\ttab end\x1b]0;evil\x07"
	h := &hub{notes: []hubNote{n}}
	res := pull(f, fk, h.up())
	if res.Added != 1 {
		t.Fatalf("%+v", res)
	}
	var title, body string
	f.ok(f.DB().QueryRow(`SELECT title,body FROM upstream_mirror`).Scan(&title, &body))
	if title != "Ti [31mtle second line" || body != "line1\nline 2\ttabend]0;evil" {
		t.Fatalf("%q %q", title, body)
	}
}

// Invalid UTF-8 is not repaired: the note is skipped and counted (a lone surrogate escape too, which the decoder would turn into U+FFFD);
// a note that really contains U+FFFD is fine.
func TestPullSkipsNotesThatAreNotUTF8(t *testing.T) {
	for name, repl := range map[string]string{"bytes": "\"title\":\"G\xff\xfeod\"", "lone surrogate": `"title":"G\ud800od"`} {
		f, fk := readFx(t)
		h := &hub{notes: []hubNote{structNote(1, "Good"), structNote(2, "Fine")}}
		h.note = func(id int64, def string) (int, string, time.Duration) {
			if id == 1 {
				return 200, strings.Replace(def, `"title":"Good"`, repl, 1), 0
			}
			return 200, strings.Replace(def, `"title":"Fine"`, `"title":"Fi\ufffdne"`, 1), 0
		}
		res := pull(f, fk, h.up())
		if res.Skipped != 1 || res.Added != 1 || !strings.Contains(strings.Join(res.Lines, "\n"), "not UTF-8") || len(mirror(f)) != 1 {
			t.Fatalf("%s: %+v %v", name, res, mirror(f))
		}
	}
}

func TestPullCursorKeepsOnMoreAndDeletesOnlyFromACompleteManifest(t *testing.T) {
	old := maxPullFetch
	maxPullFetch = 2
	defer func() { maxPullFetch = old }()
	f, fk := readFx(t)
	h := &hub{}
	for i := int64(1); i <= 5; i++ {
		h.notes = append(h.notes, structNote(i, fmt.Sprint("N", i)))
	}
	up := h.up()
	res := pull(f, fk, up)
	if !res.More || res.Next != 2 || res.Added != 2 || linkOf(f, fk).LastPullOK != nil {
		t.Fatalf("%+v", res) // a run that is not through is not "ok"
	}
	var cur int64
	f.ok(f.DB().QueryRow(`SELECT pull_cursor FROM upstream_links`).Scan(&cur))
	if cur != 2 {
		t.Fatal("cursor", cur)
	}
	res = pull(f, fk, up)
	if !res.More || res.Next != 4 || res.Added != 2 {
		t.Fatalf("%+v", res)
	}
	res = pull(f, fk, up)
	if res.More || res.Added != 1 || res.Mirror != 5 || linkOf(f, fk).LastPullOK == nil {
		t.Fatalf("%+v", res)
	}
	f.ok(f.DB().QueryRow(`SELECT pull_cursor FROM upstream_links`).Scan(&cur))
	if cur != 0 {
		t.Fatal("the cursor stays after the last run", cur)
	}
	// a truncated manifest never deletes: note 5 is not in it, note 3 changed
	h.notes = h.notes[:4]
	h.notes[2].rev = 2
	h.truncated = true
	res = pull(f, fk, up)
	if res.Removed != 0 || res.Updated != 1 || res.Mirror != 5 || !res.Truncated || !strings.Contains(strings.Join(res.Lines, "\n"), "nothing is deleted") {
		t.Fatalf("%+v", res)
	}
	h.truncated = false
	res = pull(f, fk, up)
	if res.Removed != 1 || res.Mirror != 4 {
		t.Fatalf("%+v", res)
	}
}

func TestPullPurgesAtOnceWhenTheUpstreamWithdrawsTheRight(t *testing.T) {
	for name, c := range map[string]struct {
		proj     int
		manifest func() (int, string)
		note     func(int64, string) (int, string, time.Duration)
	}{
		"project lookup 401": {proj: 401},
		"project lookup 403": {proj: 403},
		"project gone":       {proj: 404},
		"manifest 401":       {manifest: func() (int, string) { return 401, `{}` }},
		"manifest 403":       {manifest: func() (int, string) { return 403, `{"error":{"message":"device not bound"}}` }},
		"note 403":           {note: func(id int64, def string) (int, string, time.Duration) { return 403, `{}`, 0 }},
	} {
		t.Run(name, func(t *testing.T) {
			f, fk := readFx(t)
			h := &hub{notes: []hubNote{structNote(1, "One"), structNote(2, "Two")}}
			pull(f, fk, h.up())
			if len(mirror(f)) != 2 {
				t.Fatal("setup")
			}
			h.proj, h.manifest, h.note = c.proj, c.manifest, c.note
			h.notes = append(h.notes, structNote(3, "Three")) // so that a note GET is attempted
			res := pull(f, fk, h.up())
			if !res.Purged || res.Stopped == "" || len(mirror(f)) != 0 || postings(f) != 0 {
				t.Fatalf("%+v %v", res, mirror(f))
			}
			l := linkOf(f, fk)
			if l.PullError == "" || l.Mirror != 0 || l.LastPullOK != nil || !l.Read {
				t.Fatalf("%+v", l)
			}
		})
	}
	// a team link: the team is gone from the upstream's list
	f := newFx(t)
	f.ok(f.AddUpstream(ctx, "hub", "hub.example:8765"))
	tm, err := f.CreateTeam(ctx, "acme")
	f.ok(err)
	yes := true
	f.ok(f.LinkUpstreamWith(ctx, LinkSpec{Team: tm.ID, Upstream: "hub", Remote: "acme", Read: &yes, AcceptReach: true}))
	seg := segmentID(tm.ID)
	f.ok(f.write(ctx, func(tx *sql.Tx) error {
		_, e := tx.Exec(`INSERT INTO upstream_mirror(project_id,upstream,source_id,source_label,remote_note,remote_rev,class,kind,title,body,bytes,updated,fetched) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, seg, "hub", 1, "team:acme", 9, 1, "structure", "finding", "t", "b", 2, 1, 1)
		return e
	}))
	up := &fakeUp{handler: func(c upCall) (int, string, time.Duration) {
		return 200, `[{"name":"other","segment":1099511627778}]`, 0
	}}
	res, err := f.PullUpstream(ctx, up, 0, tm.ID, "", false, noSleep)
	f.ok(err)
	var n int
	f.ok(f.DB().QueryRow(`SELECT count(*) FROM upstream_mirror`).Scan(&n))
	if !res[0].Purged || n != 0 {
		t.Fatalf("%+v %d", res[0], n)
	}
}

func TestPullOldHubStoresNothingAndKeepsTheMirror(t *testing.T) {
	f, fk := readFx(t)
	h := &hub{notes: []hubNote{structNote(1, "One")}}
	pull(f, fk, h.up())
	for name, mf := range map[string]func() (int, string){
		"404":       func() (int, string) { return 404, `{}` },
		"501":       func() (int, string) { return 501, `{}` },
		"redirect":  func() (int, string) { return 302, `` },
		"version 2": func() (int, string) { return 200, `{"v":2,"sources":[],"notes":[]}` },
	} {
		h.manifest = mf
		res := pull(f, fk, h.up())
		if res.Purged || res.Stopped == "" || strings.Join(mirror(f), "|") != "1/1/structure/finding/One" {
			t.Fatalf("%s: %+v", name, res)
		}
		if l := linkOf(f, fk); l.PullError == "" || l.Mirror != 1 {
			t.Fatalf("%s: %+v", name, l)
		}
	}
	// a hub that never had one
	f, fk = readFx(t)
	h = &hub{manifest: func() (int, string) { return 404, `{}` }}
	res := pull(f, fk, h.up())
	if !strings.Contains(res.Stopped, "does not support reading") || len(mirror(f)) != 0 {
		t.Fatalf("%+v", res)
	}
}

// Every way a hub can answer a note wrongly takes that note out of the pull (counted, said), and nothing else: the rest of the run is
// stored, the previous row of that note stays, and no answer writes outside the mirror.
func TestPullHostileNoteAnswers(t *testing.T) {
	good := func(id int64) string { return structNote(id, "Good").json() }
	huge := `{"id":1,"class":"structure","title":"t","body":"` + strings.Repeat("a", 1<<20) + `","revision":1,"entryKind":"finding"}`
	big64 := `{"id":1,"class":"structure","title":"t","body":"` + strings.Repeat("a", maxBody+1) + `","revision":1,"entryKind":"finding"}`
	for name, c := range map[string]struct {
		ans  func(def string) (int, string)
		skip string
	}{
		"other id":       {func(string) (int, string) { return 200, strings.Replace(good(1), `"id":1`, `"id":999`, 1) }, "manifest"},
		"other revision": {func(string) (int, string) { return 200, strings.Replace(good(1), `"revision":1`, `"revision":2`, 1) }, "manifest"},
		"other class": {func(string) (int, string) {
			return 200, strings.Replace(good(1), `"class":"structure"`, `"class":"data"`, 1)
		}, "manifest"},
		"private": {func(string) (int, string) {
			return 200, strings.Replace(good(1), `"class":"structure"`, `"class":"private"`, 1)
		}, "manifest"},
		"other kind": {func(string) (int, string) {
			return 200, strings.Replace(good(1), `"entryKind":"finding"`, `"entryKind":"faq"`, 1)
		}, "manifest"},
		"not json":         {func(string) (int, string) { return 200, `<html>` }, "JSON"},
		"trailing data":    {func(def string) (int, string) { return 200, def + `{}` }, "after the JSON"},
		"id as a string":   {func(string) (int, string) { return 200, strings.Replace(good(1), `"id":1`, `"id":"1"`, 1) }, "JSON"},
		"revision float":   {func(string) (int, string) { return 200, strings.Replace(good(1), `"revision":1`, `"revision":1.5`, 1) }, "JSON"},
		"over 1 MiB":       {func(string) (int, string) { return 200, huge }, "size limit"},
		"over 64 KiB body": {func(string) (int, string) { return 200, big64 }, "too large"},
		"title too long": {func(string) (int, string) {
			return 200, strings.Replace(good(1), `"title":"Good"`, `"title":"`+strings.Repeat("t", maxTitle+1)+`"`, 1)
		}, "too large"},
		"empty title": {func(string) (int, string) {
			return 200, strings.Replace(good(1), `"title":"Good"`, `"title":"\u001b"`, 1)
		}, "no title"},
		"gone since": {func(string) (int, string) { return 404, `{}` }, "not served (404)"},
		"redirect":   {func(string) (int, string) { return 302, `` }, "not served (302)"},
		"a 400":      {func(string) (int, string) { return 400, `{"error":{"message":"x"}}` }, "not served (400)"},
	} {
		t.Run(name, func(t *testing.T) {
			f, fk := readFx(t)
			h := &hub{notes: []hubNote{structNote(1, "Bad"), structNote(2, "Fine")}}
			// note 1 was mirrored before: it keeps its old row when the new answer is refused
			pull(f, fk, h.up())
			h.notes[0].rev = 1
			f.ok(f.write(ctx, func(tx *sql.Tx) error {
				_, e := tx.Exec(`UPDATE upstream_mirror SET remote_rev=0 WHERE remote_note=1`)
				return e
			}))
			h.note = func(id int64, def string) (int, string, time.Duration) {
				if id == 1 {
					st, b := c.ans(def)
					return st, b, 0
				}
				return 200, def, 0
			}
			local, outside := notesState(f), countsOutside(t, f)
			res := pull(f, fk, h.up())
			if res.Skipped != 1 || res.Added+res.Updated != 0 || res.Purged || !strings.Contains(strings.Join(res.Lines, "\n"), c.skip) {
				t.Fatalf("%+v", res)
			}
			if got := strings.Join(mirror(f), "|"); got != "1/0/structure/finding/Bad|2/1/structure/finding/Fine" {
				t.Fatal(got)
			}
			if notesState(f) != local || countsOutside(t, f) != outside {
				t.Fatal("wrote outside the mirror")
			}
		})
	}
}

func TestPullHostileManifestsStoreNothingAndKeepThePreviousMirror(t *testing.T) {
	src := `"sources":[{"id":7,"label":"project:shop"}]`
	one := func(note string) string { return `{"v":1,` + src + `,"notes":[` + note + `],"truncated":false}` }
	for name, body := range map[string]string{
		"not json":      `<html>`,
		"private":       one(`{"id":9,"source":7,"rev":1,"class":"private","kind":"finding"}`),
		"negative id":   one(`{"id":-5,"source":7,"rev":1,"class":"structure","kind":"finding"}`),
		"duplicate ids": one(`{"id":9,"source":7,"rev":1,"class":"structure","kind":"finding"},{"id":9,"source":7,"rev":2,"class":"structure","kind":"finding"}`),
		"unknown src":   one(`{"id":9,"source":8,"rev":1,"class":"structure","kind":"finding"}`),
		"over 1 MiB":    `{"v":1,` + src + `,"notes":[],"pad":"` + strings.Repeat("a", 1<<20) + `"}`,
		"too many":      `{"v":1,` + src + `,"notes":[` + strings.Repeat(`{"id":1,"source":7,"rev":1,"class":"structure","kind":"finding"},`, MaxManifest) + `{"id":2,"source":7,"rev":1,"class":"structure","kind":"finding"}],"truncated":false}`,
		"huge id":       one(`{"id":99999999999999999999,"source":7,"rev":1,"class":"structure","kind":"finding"}`),
	} {
		t.Run(name, func(t *testing.T) {
			f, fk := readFx(t)
			h := &hub{notes: []hubNote{structNote(1, "One")}}
			pull(f, fk, h.up())
			h.manifest = func() (int, string) { return 200, body }
			h.asked = nil
			res := pull(f, fk, h.up())
			if res.Stopped == "" || res.Purged || len(h.asked) != 0 || strings.Join(mirror(f), "|") != "1/1/structure/finding/One" {
				t.Fatalf("%+v %v", res, h.asked)
			}
		})
	}
}

// A manifest that lists ids the hub does not serve, and a note that grows with every GET, cost at most the notes of one run, and the run
// still ends normally.
func TestPullManifestOfIdsNotServedAndANoteThatGrows(t *testing.T) {
	f, fk := readFx(t)
	h := &hub{}
	for i := int64(1); i <= 300; i++ {
		h.notes = append(h.notes, structNote(i, fmt.Sprint("N", i)))
	}
	grow := 0
	h.note = func(id int64, def string) (int, string, time.Duration) {
		switch {
		case id <= 100: // listed, never served
			return 404, `{}`, 0
		case id == 101: // grows by 100 KiB on every GET
			grow++
			n := structNote(101, "Grows")
			n.body = strings.Repeat("a", grow*100<<10)
			return 200, n.json(), 0
		}
		return 200, def, 0
	}
	up := h.up()
	res := pull(f, fk, up)
	if len(h.asked) != maxPullFetch || !res.More || res.Skipped != 101 || res.Added != 99 {
		t.Fatalf("%d %+v", len(h.asked), res)
	}
	for res.More {
		res = pull(f, fk, up)
	}
	if got := len(mirror(f)); got != 199 { // 300 listed - 100 not served - the one that is too large
		t.Fatal(got)
	}
	if linkOf(f, fk).LastPullOK == nil || strings.Contains(strings.Join(mirror(f), ""), "Grows") {
		t.Fatal("the traversal did not end, or the oversized note was taken")
	}
}

func TestPullBoundsPerLinkAndPerGateway(t *testing.T) {
	// per gateway: bodies over the cap are not taken ("mirror full"); what fits is
	f, fk := readFx(t)
	h := &hub{}
	body := strings.Repeat("a", maxBody)
	for i := int64(1); i <= 3; i++ {
		n := structNote(i, fmt.Sprint("N", i))
		n.body = body
		h.notes = append(h.notes, n)
	}
	// another link's mirror holds almost the whole budget
	f.ok(f.write(ctx, func(tx *sql.Tx) error {
		_, e := tx.Exec(`INSERT INTO upstream_mirror(project_id,upstream,source_id,source_label,remote_note,remote_rev,class,kind,title,body,bytes,updated,fetched) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, 99, "other", 1, "x", 1, 1, "structure", "finding", "t", "b", MaxMirrorBytes/mirrorCost-maxBody-100, 1, 1)
		return e
	}))
	res := pull(f, fk, h.up())
	if res.Added != 1 || res.Skipped != 2 || !strings.Contains(strings.Join(res.Lines, "\n"), "mirror full") {
		t.Fatalf("%+v", res)
	}
	// per link: the count
	f, fk = readFx(t)
	f.ok(f.write(ctx, func(tx *sql.Tx) error {
		for i := 1; i <= MaxMirrorNotes; i++ {
			if _, e := tx.Exec(`INSERT INTO upstream_mirror(project_id,upstream,source_id,source_label,remote_note,remote_rev,class,kind,title,body,bytes,updated,fetched) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, fk, "hub", 7, "project:shop", 1000+i, 1, "structure", "finding", "t", "b", 2, 1, 1); e != nil {
				return e
			}
		}
		return nil
	}))
	h = &hub{notes: []hubNote{structNote(1, "One")}, truncated: true} // truncated: the rows of the manifest-less ids stay
	res = pull(f, fk, h.up())
	if res.Added != 0 || res.Skipped != 1 || res.Mirror != MaxMirrorNotes {
		t.Fatalf("%+v", res)
	}
}

func TestPullSlowUpstreamEndsTheCallWithoutStoringHalfARun(t *testing.T) {
	oldCall, oldRun := pullCallTimeout, pullRunTimeout
	pullCallTimeout, pullRunTimeout = 50*time.Millisecond, time.Second
	defer func() { pullCallTimeout, pullRunTimeout = oldCall, oldRun }()
	f, fk := readFx(t)
	h := &hub{notes: []hubNote{structNote(1, "One"), structNote(2, "Two"), structNote(3, "Three")}}
	blocking := &slowUp{inner: h.up(), slowPath: "/notes/"}
	start := time.Now()
	res, err := f.PullUpstream(ctx, blocking, fk, 0, "", false, noSleep)
	f.ok(err)
	if took := time.Since(start); took > 2*time.Second {
		t.Fatal("a slow upstream held the call for", took)
	}
	r := res[0]
	if r.Added != 0 || len(mirror(f)) != 0 || r.Stopped == "" || r.More || r.Purged {
		t.Fatalf("%+v", r) // three tries that got no answer end the run: a failure, not "ok"
	}
	if blocking.slowCalls.Load() != maxPullFails {
		t.Fatal("slow calls", blocking.slowCalls.Load())
	}
	if l := linkOf(f, fk); l.LastPullOK != nil || !strings.Contains(l.PullError, "could not be fetched") {
		t.Fatalf("%+v", l)
	}
	// the local gateway is not stalled while a pull waits: another write goes through (the network wait holds no database transaction)
	blocking2 := &slowUp{inner: h.up(), slowPath: "/kb/manifest", hold: make(chan struct{})}
	done := make(chan UpstreamPull, 1)
	go func() {
		r, _ := f.PullUpstream(ctx, blocking2, fk, 0, "", false, noSleep)
		done <- r[0]
	}()
	for blocking2.slowCalls.Load() == 0 { // ponytail: waits for the fake to be asked
		time.Sleep(time.Millisecond)
	}
	f.mk("ana", "fakture", Structure)
	if e := f.LinkUpstream(ctx, f.pid["eureko"], 0, "hub", "shop2", "", false); e != nil {
		t.Fatal(e)
	}
	if _, e := f.PullUpstream(ctx, h.up(), fk, 0, "", false, noSleep); e == nil || !strings.Contains(e.Error(), "running") {
		t.Fatal("a second pull of the link ran over the first:", e)
	}
	close(blocking2.hold)
	if r := <-done; r.Stopped == "" {
		t.Fatalf("%+v", r)
	}
}

// slowUp answers like its inner upstream, except for paths with slowPath: those wait until the request's context ends (or hold is closed).
type slowUp struct {
	inner     *fakeUp
	slowPath  string
	hold      chan struct{}
	slowCalls atomic.Int32
}

func (u *slowUp) Do(c context.Context, up, method, path string, h map[string]string, body []byte) (int, []byte, time.Duration, error) {
	if strings.Contains(path, u.slowPath) {
		u.slowCalls.Add(1)
		select {
		case <-c.Done():
		case <-u.hold:
		}
		return 0, nil, 0, c.Err()
	}
	return u.inner.Do(c, up, method, path, h, body)
}

func TestPullRateLimitedUpstream(t *testing.T) {
	f, fk := readFx(t)
	h := &hub{notes: []hubNote{structNote(1, "One"), structNote(2, "Two")}}
	asks := 0
	h.note = func(id int64, def string) (int, string, time.Duration) {
		asks++
		if id == 2 && asks <= 3 {
			return 429, `{}`, 90 * time.Second
		}
		return 200, def, 0
	}
	var waited []time.Duration
	res, err := f.PullUpstream(ctx, h.up(), fk, 0, "", false, func(d time.Duration) bool { waited = append(waited, d); return true })
	f.ok(err)
	if res[0].More || res[0].Added != 2 || len(waited) != 2 || waited[0] != 90*time.Second {
		t.Fatalf("%+v %v", res[0], waited)
	}
	// the manifest itself rate-limited and not waitable: "more", nothing stored, no failure
	f, fk = readFx(t)
	h = &hub{manifest: func() (int, string) { return 429, `{}` }}
	res, err = f.PullUpstream(ctx, h.up(), fk, 0, "", false, func(time.Duration) bool { return false })
	f.ok(err)
	if !res[0].More || res[0].Wait != 120 || len(mirror(f)) != 0 || linkOf(f, fk).PullError != "" {
		t.Fatalf("%+v", res[0])
	}
	// a wait that cannot be made within this call: "more", the cursor stays, no failure
	f, fk = readFx(t)
	h = &hub{notes: []hubNote{structNote(1, "One"), structNote(2, "Two")}}
	asks = 0
	h.note = func(id int64, def string) (int, string, time.Duration) {
		if id == 2 {
			return 429, `{}`, 150 * time.Second
		}
		return 200, def, 0
	}
	res, err = f.PullUpstream(ctx, h.up(), fk, 0, "", false, func(time.Duration) bool { return false })
	f.ok(err)
	r := res[0]
	if !r.More || r.Wait != 120 || r.Added != 1 || r.Next != 1 || r.Purged || linkOf(f, fk).PullError != "" {
		t.Fatalf("%+v", r) // the wait is capped at maxRateWait
	}
}

// The time budget of a helper call: the run ends with "more" when the context is done, keeps what it has and the cursor.
func TestPullEndsWithMoreWhenTheBudgetOfTheCallIsUsed(t *testing.T) {
	f, fk := readFx(t)
	h := &hub{notes: []hubNote{structNote(1, "One"), structNote(2, "Two"), structNote(3, "Three")}}
	c, cancel := context.WithCancel(ctx)
	h.note = func(id int64, def string) (int, string, time.Duration) {
		if id == 2 {
			cancel() // the budget is gone after the second GET
		}
		return 200, def, 0
	}
	res, err := f.PullUpstream(c, h.up(), fk, 0, "", false, noSleep)
	f.ok(err)
	if !res[0].More || res[0].Added != 2 || res[0].Next != 2 || len(mirror(f)) != 2 {
		t.Fatalf("%+v", res[0])
	}
	var cur int64
	f.ok(f.DB().QueryRow(`SELECT pull_cursor FROM upstream_links`).Scan(&cur))
	if cur != 2 {
		t.Fatal(cur)
	}
}

func TestPullRefusedWithoutReadingOrLinkAndNotWhenBusy(t *testing.T) {
	f, fk := pullFx(t)
	up := (&hub{}).up()
	_, err := f.PullUpstream(ctx, up, fk, 0, "", false, noSleep)
	is(t, err, ErrRefusal{"reading is off for this link: minigw gw upstream link <project> <upstream> --read --accept-reach (look first: pull <project> --dry-run)"})
	_, err = f.PullUpstream(ctx, up, f.pid["eureko"], 0, "", false, noSleep)
	is(t, err, ErrRefusal{"no upstream link for this: minigw gw upstream link <project> <upstream>"})
	if len(up.calls) != 0 {
		t.Fatal("a refused pull contacted the upstream")
	}
	// reading on, but a push of the link is running: refused, nothing contacted
	readOn(f, fk, "", "")
	mu := f.linkLock(fk, "hub")
	mu.Lock()
	_, err = f.PullUpstream(ctx, up, fk, 0, "", false, noSleep)
	mu.Unlock()
	if !errors.As(err, new(errPushRunning)) || len(up.calls) != 0 {
		t.Fatal(err)
	}
	// and the other way round: a pull in progress refuses an unlink and the removal of the upstream
	mu.Lock()
	e1 := f.UnlinkUpstream(ctx, fk, 0, "hub")
	mu.Unlock()
	if !errors.As(e1, new(errPushRunning)) {
		t.Fatal(e1)
	}
}

// ---- what removes the mirror

func TestMirrorIsDeletedOnUnlinkOnRemovalAndWhenReadingIsTurnedOff(t *testing.T) {
	fill := func() (*fx, int64) {
		f, fk := readFx(t)
		h := &hub{notes: []hubNote{structNote(1, "One"), structNote(2, "Two")}}
		pull(f, fk, h.up())
		if len(mirror(f)) != 2 || postings(f) != 2 {
			t.Fatal("setup")
		}
		return f, fk
	}
	empty := func(f *fx) {
		t.Helper()
		if len(mirror(f)) != 0 || postings(f) != 0 {
			t.Fatalf("mirror left: %v %d", mirror(f), postings(f))
		}
		var n int
		f.ok(f.DB().QueryRow(`SELECT count(*) FROM upstream_mirror_search_content`).Scan(&n))
		if n != 0 {
			t.Fatal("search content left", n)
		}
	}
	f, fk := fill()
	f.ok(f.UnlinkUpstream(ctx, fk, 0, "hub"))
	empty(f)
	f, _ = fill()
	f.ok(f.RemoveUpstream(ctx, "hub"))
	empty(f)
	f, fk = fill()
	no := false
	f.ok(f.LinkUpstreamWith(ctx, LinkSpec{Project: fk, Upstream: "hub", Remote: "shop", Read: &no, ReadOnly: true}))
	empty(f)
	if l := linkOf(f, fk); l.Read || l.LastPullOK != nil || l.PullError != "" {
		t.Fatalf("%+v", l)
	}
	// another link's mirror is not touched by any of these
	f, fk = fill()
	f.ok(f.AddUpstream(ctx, "other", "other.example:8765"))
	f.ok(f.write(ctx, func(tx *sql.Tx) error {
		_, e := tx.Exec(`INSERT INTO upstream_mirror(project_id,upstream,source_id,source_label,remote_note,remote_rev,class,kind,title,body,bytes,updated,fetched) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, fk, "other", 1, "x", 1, 1, "structure", "finding", "keep", "b", 2, 1, 1)
		return e
	}))
	f.ok(f.UnlinkUpstream(ctx, fk, 0, "hub"))
	if got := strings.Join(mirror(f), "|"); got != "1/1/structure/finding/keep" {
		t.Fatal(got)
	}
}

func TestMirrorIsNotInExportBundlesOrPushes(t *testing.T) {
	f := newSeg(t)
	fk := f.pid["fakture"]
	f.ok(f.AddUpstream(ctx, "hub", "hub.example:8765"))
	f.ok(f.LinkUpstream(ctx, fk, 0, "hub", "shop", "", false))
	readOn(f.fx, fk, "", "")
	h := &hub{notes: []hubNote{structNote(1, "MIRRORED-TITLE")}}
	h.notes[0].body = "MIRRORED-BODY"
	pull(f.fx, fk, h.up())
	f.mk("ana", "fakture", Structure)
	file := filepath.Join(t.TempDir(), "b.zip")
	f.ok(f.ExportBundle(ctx, BundleExport{Team: "acme", Path: file}))
	b, err := readBundle(file)
	f.ok(err)
	for name, data := range b.files {
		if strings.Contains(string(data), "MIRRORED") {
			t.Fatalf("%s carries a mirrored note", name)
		}
	}
	// a push sends local notes only, and counts none of the mirror
	up := okUp()
	res, err := f.PushUpstream(ctx, up, fk, 0, "", 0, false, noSleep)
	f.ok(err)
	for _, c := range up.calls {
		if strings.Contains(fmt.Sprint(c.body), "MIRRORED") {
			t.Fatal("a push sent a mirrored note")
		}
	}
	if res[0].Created != 1 {
		t.Fatalf("%+v", res[0])
	}
}

func TestPullIsNotConfusedByIdsOfOtherLinksAndSources(t *testing.T) {
	// the same upstream note id in two links (two local projects reading one hub project): each link has its own row
	f, fk := readFx(t)
	f.ok(f.LinkUpstream(ctx, f.pid["eureko"], 0, "hub", "shop", "", false))
	readOn(f, f.pid["eureko"], "", "")
	h := &hub{notes: []hubNote{structNote(1, "One")}}
	pull(f, fk, h.up())
	h.notes[0].title = "One"
	res, err := f.PullUpstream(ctx, h.up(), f.pid["eureko"], 0, "", false, noSleep)
	f.ok(err)
	if res[0].Added != 1 {
		t.Fatalf("%+v", res[0])
	}
	var n int
	f.ok(f.DB().QueryRow(`SELECT count(*) FROM upstream_mirror WHERE remote_note=1`).Scan(&n))
	if n != 2 {
		t.Fatal(n)
	}
	// ids are keys only: a hub that names a huge or reused id cannot reach a row of another link
	h2 := &hub{notes: []hubNote{{id: 1<<50 + 5, src: 7, rev: 1, class: Structure, kind: KindFinding, title: "Huge", body: "b"}}}
	h2.note = func(id int64, def string) (int, string, time.Duration) { return 200, def, 0 }
	up := &fakeUp{handler: func(c upCall) (int, string, time.Duration) {
		if strings.HasPrefix(c.path, "/minigw/v1/projects/7/notes/") {
			return 200, h2.notes[0].json(), 0
		}
		return h2.up().handler(c)
	}}
	if _, err = f.PullUpstream(ctx, up, fk, 0, "", false, noSleep); err != nil {
		t.Fatal(err)
	}
	f.ok(f.DB().QueryRow(`SELECT count(*) FROM upstream_mirror WHERE project_id=? AND remote_note=1`, f.pid["eureko"]).Scan(&n))
	if n != 1 {
		t.Fatal("another link's row was touched", n)
	}
}

func TestStatusShowsReadingTheMirrorAndTheAge(t *testing.T) {
	f, fk := readFx(t)
	if l := linkOf(f, fk); !l.Read || l.Mirror != 0 || l.LastPullOK != nil || l.LastPullAge != 0 {
		t.Fatalf("%+v", l)
	}
	h := &hub{notes: []hubNote{structNote(1, "One")}}
	pull(f, fk, h.up())
	f.clock = f.clock.Add(3 * time.Hour)
	l := linkOf(f, fk)
	if l.LastPullAge != 3*3600 || l.Mirror != 1 || l.MirrorBytes != int64(len("One")+len("body of One")) {
		t.Fatalf("%+v", l)
	}
	h.manifest = func() (int, string) { return 500, `{}` }
	pull(f, fk, h.up())
	if l = linkOf(f, fk); l.PullError == "" || l.LastPull == nil || l.LastPullOK == nil || !l.LastPull.After(*l.LastPullOK) || l.Mirror != 1 {
		t.Fatalf("%+v", l)
	}
}

// ---- the review of C2 (sec-upc2-0728)

// M1: the acceptance covers the exact reach: upstream (the link itself), remote project or team, classes, sources.
func TestAcceptanceIsBoundToTheReach(t *testing.T) {
	filled := func() (*fx, int64) {
		f, fk := readFx(t)
		pull(f, fk, (&hub{notes: []hubNote{structNote(1, "One")}}).up())
		if len(mirror(f)) != 1 || !linkOf(f, fk).Read {
			t.Fatal("setup")
		}
		return f, fk
	}
	yes := true
	// a plain re-link to another remote project (as `link <project> <name> --auto` does by defaulting the remote): reading off, mirror gone
	f, fk := filled()
	auto := true
	off, err := f.LinkUpstreamReport(ctx, LinkSpec{Project: fk, Upstream: "hub", Remote: "fakture", Auto: &auto})
	f.ok(err)
	if l := linkOf(f, fk); !off || l.Read || l.Mirror != 0 || l.ReadAccepted != nil || l.Remote != "fakture" || !l.Auto || len(mirror(f)) != 0 || postings(f) != 0 {
		t.Fatalf("%v %+v", off, l)
	}
	if _, err = f.PullUpstream(ctx, (&hub{}).up(), fk, 0, "", false, noSleep); err == nil {
		t.Fatal("a pull ran on a link whose reach was not accepted")
	}
	// the same re-link to the SAME remote keeps reading and the mirror
	f, fk = filled()
	off, err = f.LinkUpstreamReport(ctx, LinkSpec{Project: fk, Upstream: "hub", Remote: "shop", Auto: &auto})
	f.ok(err)
	if l := linkOf(f, fk); off || !l.Read || l.Mirror != 1 {
		t.Fatalf("%v %+v", off, l)
	}
	// the new reach in the same command, with the acceptance: reading stays on for the NEW remote, the old mirror is gone
	f, fk = filled()
	off, err = f.LinkUpstreamReport(ctx, LinkSpec{Project: fk, Upstream: "hub", Remote: "web", Read: &yes, AcceptReach: true})
	f.ok(err)
	if l := linkOf(f, fk); off || !l.Read || l.Remote != "web" || l.Mirror != 0 || len(mirror(f)) != 0 {
		t.Fatalf("%v %+v", off, l)
	}
	var reach string
	f.ok(f.DB().QueryRow(`SELECT read_reach FROM upstream_links`).Scan(&reach))
	if reach != "hub|web|0|structure|" {
		t.Fatal(reach)
	}
	// a team link: another remote team name is another reach too
	g := newFx(t)
	g.ok(g.AddUpstream(ctx, "hub", "hub.example:8765"))
	tm, e := g.CreateTeam(ctx, "acme")
	g.ok(e)
	g.ok(g.LinkUpstreamWith(ctx, LinkSpec{Team: tm.ID, Upstream: "hub", Remote: "acme", Read: &yes, AcceptReach: true}))
	if off, err = g.LinkUpstreamReport(ctx, LinkSpec{Team: tm.ID, Upstream: "hub", Remote: "acme-eng"}); err != nil || !off {
		t.Fatal(off, err)
	}
	// classes and sources can only change WITH the acceptance: without it the command is refused and the accepted reach stays
	f, fk = filled()
	for _, sp := range []LinkSpec{{ReadClasses: "structure,data"}, {ReadSources: ptr("team:acme")}} {
		sp.Project, sp.Upstream, sp.Remote, sp.ReadOnly = fk, "hub", "shop", true
		if e := f.LinkUpstreamWith(ctx, sp); e == nil {
			t.Fatal("a change of what is read without the acceptance")
		}
	}
	if l := linkOf(f, fk); !l.Read || classesCSV(l.ReadClasses) != "structure" || l.Mirror != 1 {
		t.Fatalf("%+v", l)
	}
	// another upstream is another link: its reading is off until accepted there
	f.ok(f.AddUpstream(ctx, "other", "other.example:8765"))
	f.ok(f.LinkUpstream(ctx, fk, 0, "other", "shop", "", false))
	for _, l := range func() []UpstreamLinkInfo { ls, _ := f.UpstreamLinks(ctx, fk, 0); return ls }() {
		if l.Upstream == "other" && l.Read {
			t.Fatal("reading came with a new link")
		}
	}
	// the audit row names what is in effect, not only the flags that were given
	f.ok(f.LinkUpstreamWith(ctx, LinkSpec{Project: fk, Upstream: "hub", Remote: "shop", Read: &yes, ReadSources: ptr("team:acme"), AcceptReach: true, ReadOnly: true}))
	f.ok(f.LinkUpstreamWith(ctx, LinkSpec{Project: fk, Upstream: "hub", Remote: "shop", Read: &yes, AcceptReach: true, ReadOnly: true})) // no lists given
	evs, e2 := f.AuditEvents(ctx, 0, 500)
	f.ok(e2)
	last := ""
	for _, ev := range evs {
		if ev.Action == "upstream.link" && strings.Contains(string(ev.Detail), "readAccepted") {
			last = string(ev.Detail)
		}
	}
	if !strings.Contains(last, `"reach":"hub|shop|0|structure|team:acme"`) || !strings.Contains(last, `"classes":"structure"`) || !strings.Contains(last, `"sources":"team:acme"`) {
		t.Fatal(last)
	}
}

// A link update waits for a running pull (it is refused, as an unlink is): the run reads the link, its remote and its acceptance together and
// contacts the remote it was accepted for. (sec-upc4 7)
func TestLinkUpdateIsRefusedWhileThePullRuns(t *testing.T) {
	f, fk := readFx(t)
	h := &hub{notes: []hubNote{structNote(1, "One"), structNote(2, "Two")}}
	var refused error
	h.note = func(id int64, def string) (int, string, time.Duration) {
		if id == 1 {
			auto := true
			refused = f.LinkUpstreamWith(ctx, LinkSpec{Project: fk, Upstream: "hub", Remote: "fakture", Auto: &auto})
		}
		return 200, def, 0
	}
	res := pull(f, fk, h.up())
	var busy errPushRunning
	if !errors.As(refused, &busy) || res.Added != 2 {
		t.Fatalf("%v %+v", refused, res)
	}
	if l := linkOf(f, fk); l.Remote != "shop" || !l.Read {
		t.Fatalf("%+v", l)
	}
}

// Whatever got past the lock (a restored or edited database), a run never uses one remote with another remote's acceptance: before the first
// request and again where it writes.
func TestPullRefusesAnAcceptanceThatIsNotForTheLinksReach(t *testing.T) {
	f, fk := readFx(t)
	h := &hub{notes: []hubNote{structNote(1, "One")}}
	f.ok(f.write(ctx, func(tx *sql.Tx) error { _, e := tx.Exec(`UPDATE upstream_links SET remote='elsewhere'`); return e }))
	up := h.up()
	res := pull(f, fk, up)
	if len(up.calls) != 0 || len(mirror(f)) != 0 || !strings.Contains(res.Stopped, "not the one you accepted") {
		t.Fatalf("%+v %v", res, up.calls)
	}
	// the same change while the run is in flight: nothing stored
	f.ok(f.write(ctx, func(tx *sql.Tx) error { _, e := tx.Exec(`UPDATE upstream_links SET remote='shop'`); return e }))
	h.note = func(id int64, def string) (int, string, time.Duration) {
		f.ok(f.write(ctx, func(tx *sql.Tx) error { _, e := tx.Exec(`UPDATE upstream_links SET remote='elsewhere'`); return e }))
		return 200, def, 0
	}
	res = pull(f, fk, h.up())
	if len(mirror(f)) != 0 || res.Added != 0 {
		t.Fatalf("%+v %v", res, mirror(f))
	}
}

// M2 (core): a full mirror ends the traversal; nothing is fetched to be thrown away.
func TestFullMirrorFetchesNothingNew(t *testing.T) {
	f, fk := readFx(t)
	f.ok(f.write(ctx, func(tx *sql.Tx) error {
		for i := 1; i <= MaxMirrorNotes; i++ {
			if _, e := tx.Exec(`INSERT INTO upstream_mirror(project_id,upstream,source_id,source_label,remote_note,remote_rev,class,kind,title,body,bytes,updated,fetched) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, fk, "hub", 7, "project:shop", i+100000, 1, "structure", "finding", "t", "b", 2, 1, 1); e != nil {
				return e
			}
		}
		return nil
	}))
	h := &hub{}
	for i := int64(1); i <= 201; i++ { // the hub offers 201 notes the mirror has no room for
		h.notes = append(h.notes, structNote(i, "N"))
	}
	changed := structNote(100001, "N")
	changed.rev = 2 // one known note changed: that one is fetched
	h.notes = append(h.notes, changed)
	h.truncated = true
	res := pull(f, fk, h.up())
	if len(h.asked) != 1 || h.asked[0] != 100001 || res.More || res.Skipped != 201 || !strings.Contains(strings.Join(res.Lines, "\n"), "mirror full") || res.Mirror != MaxMirrorNotes || res.Updated != 1 {
		t.Fatalf("%v %+v", h.asked, res)
	}
	if l := linkOf(f, fk); l.LastPullOK == nil {
		t.Fatal("a full mirror leaves the traversal unfinished")
	}
}

// The disk cost: 64 MiB of cap counts a note at mirrorCost times its text.
func TestMirrorCapCountsTheDiskCost(t *testing.T) {
	f, fk := readFx(t)
	f.ok(f.write(ctx, func(tx *sql.Tx) error {
		_, e := tx.Exec(`INSERT INTO upstream_mirror(project_id,upstream,source_id,source_label,remote_note,remote_rev,class,kind,title,body,bytes,updated,fetched) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`, 99, "other", 1, "x", 1, 1, "structure", "finding", "t", "b", MaxMirrorBytes/mirrorCost-10, 1, 1)
		return e
	}))
	res := pull(f, fk, (&hub{notes: []hubNote{structNote(1, "One")}}).up())
	if res.Added != 0 || res.Skipped != 1 {
		t.Fatalf("%+v", res) // text alone is below 64 MiB, its cost is not
	}
}

// L1: a wait for the rate limit never outlives the run's own deadline.
func TestPullDoesNotSleepPastItsDeadline(t *testing.T) {
	old := pullRunTimeout
	pullRunTimeout = 3 * time.Second
	defer func() { pullRunTimeout = old }()
	f, fk := readFx(t)
	h := &hub{notes: []hubNote{structNote(1, "One")}, manifest: func() (int, string) { return 429, `{}` }}
	slept := 0
	res, err := f.PullUpstream(ctx, h.up(), fk, 0, "", false, func(time.Duration) bool { slept++; return true })
	f.ok(err)
	if slept != 0 || !res[0].More || res[0].Wait != 120 {
		t.Fatalf("slept %d %+v", slept, res[0]) // the 2 minute wait does not fit in the 3 seconds that are left
	}
}

// L2: a cut manifest keeps what it does not list, but not what this link no longer takes.
func TestTruncatedManifestStillDropsWhatTheLinkNoLongerTakes(t *testing.T) {
	f, fk := readFx(t)
	h := &hub{notes: []hubNote{structNote(1, "One"), structNote(2, "Two"), structNote(3, "Three")}}
	pull(f, fk, h.up())
	f.ok(f.write(ctx, func(tx *sql.Tx) error { // a data row (this link reads structure only) and a row of a source it does not take
		_, e := tx.Exec(`UPDATE upstream_mirror SET class='data' WHERE remote_note=2`)
		if e == nil {
			_, e = tx.Exec(`UPDATE upstream_mirror SET source_id=99 WHERE remote_note=3`)
		}
		return e
	}))
	h.notes = h.notes[:1] // 2 and 3 are not listed any more, and the manifest is cut: unlisted rows stay unless this link does not take them
	h.truncated = true
	h.notes = append(h.notes, hubNote{id: 4, src: 7, rev: 1, class: Data, kind: KindFinding, title: "Four", body: "b"}) // listed, but a class not read
	pull(f, fk, h.up())
	if got := strings.Join(mirror(f), "|"); got != "1/1/structure/finding/One" {
		t.Fatal(got)
	}
	// a note the hub moved to data stays out of the mirror even when the manifest is cut
	f, fk = readFx(t)
	h = &hub{notes: []hubNote{structNote(1, "One"), structNote(2, "Two")}}
	pull(f, fk, h.up())
	h.notes[1].class, h.notes[1].rev, h.truncated = Data, 2, true
	pull(f, fk, h.up())
	if got := strings.Join(mirror(f), "|"); got != "1/1/structure/finding/One" {
		t.Fatal(got)
	}
}

// L4: what a reader cannot see is removed, by one rule.
func TestInvisibleRuneTable(t *testing.T) {
	for r, want := range map[rune]bool{
		'a': false, ' ': false, '\n': true /* a control: mirrorBody keeps it itself */, 0x7f: true, 0x200b: true, 0x202e: true, 0x2066: true, 0xfeff: true, 0xad: true,
		0xfe0f: true, 0xfe00: true, 0xe0100: true, 0xe01ef: true, 0xe0041: true, 0xe0001: true, 0x115f: true, 0x1160: true, 0x3164: true, 0xffa0: true,
		0x034f: true, 0x180b: true, 0x2028: true, 0x2029: true, 'é': false, '日': false, '😀': false, 0xe01f0: false,
	} {
		if invisibleRune(r) != want {
			t.Errorf("U+%04X: %v", r, !want)
		}
	}
	if got := mirrorBody("a\u200bb\ufe0fc\U000e0041d\u3164e\tf\ng"); got != "abcde\tf\ng" {
		t.Fatalf("%q", got)
	}
	if got := upText("x\ufe0f\U000e0100y\u3164z", 40); got != "x y z" {
		t.Fatalf("%q", got)
	}
}

// L8: a restore rebuilds the mirror's search index too (it has its own copy of the text).
func TestRestoreRebuildsTheMirrorSearchIndex(t *testing.T) {
	f, fk := readFx(t)
	pull(f, fk, (&hub{notes: []hubNote{structNote(1, "Alpha title")}}).up())
	bak := filepath.Join(t.TempDir(), "m.db")
	f.ok(f.Backup(ctx, bak))
	damageIndex(t, bak, `UPDATE upstream_mirror_search SET body='zebra'`)
	dir := t.TempDir()
	f.ok(Restore(dir, bak))
	s := openT(t, dir)
	defer s.Close()
	var zebra, alpha int
	s.DB().QueryRow(`SELECT count(*) FROM upstream_mirror_search WHERE upstream_mirror_search MATCH 'zebra'`).Scan(&zebra)
	s.DB().QueryRow(`SELECT count(*) FROM upstream_mirror_search WHERE upstream_mirror_search MATCH 'alpha'`).Scan(&alpha)
	if zebra != 0 || alpha != 1 {
		t.Fatal("the mirror's index was not rebuilt", zebra, alpha)
	}
}

// L9: pulling every link in one call: a link after the used-up budget is "more", the reports of the links done stay.
func TestPullAllLinksKeepsTheReportsWhenTheBudgetIsUsed(t *testing.T) {
	f, _ := readFx(t)
	f.ok(f.LinkUpstream(ctx, f.pid["eureko"], 0, "hub", "shop", "", false))
	readOn(f, f.pid["eureko"], "", "")
	h := &hub{notes: []hubNote{structNote(1, "One"), structNote(2, "Two")}}
	c, cancel := context.WithCancel(ctx)
	h.note = func(id int64, def string) (int, string, time.Duration) {
		if id == 2 {
			cancel() // the budget is gone while the first link is in its last note
		}
		return 200, def, 0
	}
	res, err := f.PullUpstream(c, h.up(), 0, 0, "", false, noSleep)
	f.ok(err)
	if len(res) != 2 || res[0].Added != 2 || !res[1].More || res[1].Wait != 1 {
		t.Fatalf("%+v", res)
	}
}

func ptr(s string) *string { return &s }
