package core

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

// manifestFx: fakture (team acme, depends on eureko) with notes of every class and kind, a pending submission, a team segment note of every
// class, and eureko notes. Users: ana, bor (developers of fakture; ana on acme's roster, bor not), eva (reader of fakture, on the roster),
// cene (developer of eureko only), dana (nothing there).
func manifestFx(t *testing.T) (*segTeams, []int64) {
	f := newSeg(t)
	f.ok(f.AddDepends(ctx, f.pid["fakture"], f.pid["eureko"]))
	var all []int64
	add := func(n Note) { all = append(all, n.ID) }
	for _, c := range []Class{Structure, Data, Private} {
		f.clock = f.clock.Add(time.Second)
		keySeq++
		n, _, e := f.CreateNote(ctx, f.p("ana"), f.pid["fakture"], fmt.Sprint("mk", keySeq), NoteInput{Class: c, Title: "TITLE-ana-" + string(c), Body: "x"})
		f.ok(e)
		add(n)
	}
	f.clock = f.clock.Add(time.Second)
	keySeq++
	n, _, e := f.CreateNote(ctx, f.p("bor"), f.pid["fakture"], fmt.Sprint("mk", keySeq), NoteInput{Class: Structure, Title: "TITLE-faq", Body: "a", EntryKind: KindFAQ})
	f.ok(e)
	add(n)
	keySeq++
	n, _, e = f.CreateNote(ctx, f.p("bor"), f.pid["fakture"], fmt.Sprint("mk", keySeq), NoteInput{Class: Private, Title: "TITLE-bor-private", Body: "x"})
	f.ok(e)
	add(n)
	for _, c := range []Class{Structure, Data} { // a team segment holds no private notes
		add(f.put("fani", c, "TITLE-seg-"+string(c)))
	}
	for _, c := range []Class{Structure, Data, Private} {
		f.clock = f.clock.Add(time.Second)
		keySeq++
		n, _, e := f.CreateNote(ctx, f.p("cene"), f.pid["eureko"], fmt.Sprint("mk", keySeq), NoteInput{Class: c, Title: "TITLE-eureko-" + string(c), Body: "x"})
		f.ok(e)
		add(n)
	}
	// a pending submission: not a note
	_, _, e = f.Submit(ctx, f.p("ana"), f.pid["fakture"], "mk-sub", SubmissionInput{NoteInput: NoteInput{Class: Structure, Title: "TITLE-pending", Body: "x"}})
	f.ok(e)
	return f, all
}

// The manifest lists exactly what the caller could GET, minus private: for every user and every note id that exists, listed <=> GET works
// (home project and its sources) and the class is not private.
func TestManifestIsExactlyWhatTheCallerCouldGet(t *testing.T) {
	f, all := manifestFx(t)
	fk := f.pid["fakture"]
	for _, u := range []string{"ana", "bor", "eva", "fani"} {
		m, err := f.Manifest(ctx, f.p(u), fk)
		f.ok(err)
		listed := map[int64]ManifestNote{}
		for _, n := range m.Notes {
			listed[n.ID] = n
		}
		if len(listed) != len(m.Notes) || m.V != 1 || m.Truncated {
			t.Fatalf("%s: %+v", u, m)
		}
		for _, id := range all {
			_, gerr := f.GetNote(ctx, f.p(u), fk, id)
			n, in := listed[id]
			if in && gerr != nil {
				t.Errorf("%s: note %d is listed but GET says %v", u, id, gerr)
			}
			if in && n.Class == Private {
				t.Errorf("%s: private note %d listed", u, id)
			}
			if !in && gerr == nil {
				// readable but not listed: only the caller's OWN private notes may be missing
				nn, _ := f.GetNote(ctx, f.p(u), fk, id)
				if nn.Class != Private {
					t.Errorf("%s: note %d (%s) is readable but not listed", u, id, nn.Class)
				}
			}
		}
		for _, s := range m.Sources {
			if s.Label == "" || !strings.HasPrefix(s.Label, "project:") && !strings.HasPrefix(s.Label, "team:") {
				t.Errorf("%s: source %+v", u, s)
			}
		}
	}
}

func manifestIDs(m Manifest) (out []string) {
	for _, n := range m.Notes {
		out = append(out, fmt.Sprintf("%d/%s/%s", n.Source, n.Class, n.Kind))
	}
	sort.Strings(out)
	return
}

func TestManifestByRole(t *testing.T) {
	f, _ := manifestFx(t)
	fk, eu := f.pid["fakture"], f.pid["eureko"]
	count := func(m Manifest, src int64, c Class) (n int) {
		for _, x := range m.Notes {
			if x.Source == src && x.Class == c {
				n++
			}
		}
		return
	}
	labels := func(m Manifest) (l []string) {
		for _, s := range m.Sources {
			l = append(l, s.Label)
		}
		return
	}
	// developer on the roster: own project structure+data, the team segment, nothing of eureko (not a member there, no grant to read it)
	m, err := f.Manifest(ctx, f.p("ana"), fk)
	f.ok(err)
	if count(m, fk, Structure) != 2 || count(m, fk, Data) != 1 || count(m, f.seg, Structure) != 1 || count(m, f.seg, Data) != 1 || count(m, eu, Structure) != 0 {
		t.Fatalf("ana: %v %v", manifestIDs(m), labels(m))
	}
	if got := labels(m); strings.Join(got, ",") != "project:fakture,team:acme" {
		t.Fatalf("ana sources %v", got)
	}
	// reader on the roster: fakture structure only; the team segment is read by its roster as a whole (the invariant test above proves each is GET-able)
	m, err = f.Manifest(ctx, f.p("eva"), fk)
	f.ok(err)
	if count(m, fk, Structure) != 2 || count(m, fk, Data) != 0 || count(m, f.seg, Data) != 1 || count(m, f.seg, Structure) != 1 {
		t.Fatalf("eva: %v", manifestIDs(m))
	}
	// the source says which classes the caller reads in full: a reader's home project lists structure only (its data notes are not in the answer)
	if m.Sources[0].Full == nil || len(m.Sources[0].Full) != 1 || m.Sources[0].Full[0] != Structure {
		t.Fatalf("eva full: %v", m.Sources[0].Full)
	}
	if ma, e := f.Manifest(ctx, f.p("ana"), fk); e != nil || len(ma.Sources[0].Full) != 2 {
		t.Fatalf("ana full: %v %v", ma.Sources[0].Full, e)
	}
	// developer of fakture off the team roster: no segment source at all
	m, err = f.Manifest(ctx, f.p("bor"), fk)
	f.ok(err)
	if count(m, f.seg, Structure)+count(m, f.seg, Data) != 0 || strings.Contains(strings.Join(labels(m), ","), "team:") {
		t.Fatalf("bor: %v %v", manifestIDs(m), labels(m))
	}
	// cene reads fakture's structure only through a grant (eureko -> fakture), plus eureko itself (a dependency of fakture, his own project:
	// structure and data, never private); dana has no way in at all: 404
	m, err = f.Manifest(ctx, f.p("cene"), fk)
	f.ok(err)
	if count(m, fk, Structure) != 2 || count(m, fk, Data) != 0 || len(m.Sources) != 2 || count(m, eu, Structure) != 1 || count(m, eu, Data) != 1 || count(m, eu, Private) != 0 {
		t.Fatalf("cene: %v %v", manifestIDs(m), labels(m))
	}
	_, err = f.Manifest(ctx, f.p("dana"), fk)
	is(t, err, ErrNotFound)
	// depends on: eureko's notes appear only for a user who could read them directly (eva made a reader there)
	f.ok(f.SetMember(ctx, eu, f.uid["eva"], Reader))
	m, err = f.Manifest(ctx, f.p("eva"), fk)
	f.ok(err)
	if count(m, eu, Structure) != 1 || count(m, eu, Data) != 0 || count(m, eu, Private) != 0 || !strings.Contains(strings.Join(labels(m), ","), "project:eureko") {
		t.Fatalf("eva with eureko: %v %v", manifestIDs(m), labels(m))
	}
	// a team link's own manifest: the segment as the project asked about
	m, err = f.Manifest(ctx, f.p("ana"), f.seg)
	f.ok(err)
	if count(m, f.seg, Structure) != 1 || count(m, fk, Structure) != 0 || len(m.Sources) != 1 {
		t.Fatalf("segment: %v", manifestIDs(m))
	}
	_, err = f.Manifest(ctx, f.p("bor"), f.seg)
	is(t, err, ErrNotFound)
}

// Pending submissions and private notes leave no trace: the answer is the same with or without them, and carries no title.
func TestManifestLeaksNothingAboutPrivateOrPending(t *testing.T) {
	f := newSeg(t)
	fk := f.pid["fakture"]
	snap := func() string {
		m, err := f.Manifest(ctx, f.p("eva"), fk)
		f.ok(err)
		b, _ := json.Marshal(m)
		return string(b)
	}
	f.mk("ana", "fakture", Structure)
	before := snap()
	for i := 0; i < 3; i++ {
		f.mk("ana", "fakture", Private)
	}
	f.mk("ana", "fakture", Data) // eva cannot read it
	_, _, e := f.Submit(ctx, f.p("ana"), fk, "pend", SubmissionInput{NoteInput: NoteInput{Class: Structure, Title: "TITLE-pending", Body: "x"}})
	f.ok(e)
	if after := snap(); after != before {
		t.Fatalf("the answer changed with notes the caller cannot read:\n%s\n%s", before, after)
	}
	var shape map[string]any
	json.Unmarshal([]byte(before), &shape)
	keys := []string{}
	for k := range shape {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if strings.Join(keys, ",") != "notes,sources,truncated,v" {
		t.Fatal(keys)
	}
	for _, n := range shape["notes"].([]any) {
		ks := []string{}
		for k := range n.(map[string]any) {
			ks = append(ks, k)
		}
		sort.Strings(ks)
		if strings.Join(ks, ",") != "class,id,kind,rev,source" {
			t.Fatal(ks)
		}
	}
	if strings.Contains(before, "TITLE") || strings.Contains(before, "deploy order") {
		t.Fatal("a title or body in the manifest")
	}
}

func TestManifestCapAndRate(t *testing.T) {
	f := newSeg(t)
	fk := f.pid["fakture"]
	f.ok(f.write(ctx, func(tx *sql.Tx) error {
		for i := 0; i < MaxManifest+1; i++ {
			if _, err := tx.Exec(`INSERT INTO notes(project_id,class,title,body,bytes,author_id,created,updated) VALUES(?,?,?,?,?,?,?,?)`, fk, Structure, "s", "b", 2, f.uid["ana"], 1, 1); err != nil {
				return err
			}
		}
		return nil
	}))
	m, err := f.Manifest(ctx, f.p("ana"), fk)
	f.ok(err)
	if len(m.Notes) != MaxManifest || !m.Truncated {
		t.Fatalf("%d truncated=%v", len(m.Notes), m.Truncated)
	}
	if b, _ := json.Marshal(m); len(b) > 1<<20 {
		t.Fatalf("answer of %d bytes", len(b))
	}
	// exactly the cap is complete
	f.ok(f.write(ctx, func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM notes WHERE id=(SELECT max(id) FROM notes)`)
		return err
	}))
	if m, err = f.Manifest(ctx, f.p("ana"), fk); err != nil || len(m.Notes) != MaxManifest || m.Truncated {
		t.Fatalf("%v %d %v", err, len(m.Notes), m.Truncated)
	}
	// the notes list's limit: refused requests count, after the access checks (a stranger still gets 404, an exhausted member 429)
	readsPerMin = 2
	f.rl = rateLimits{w: map[int64]*window{}}
	_, e1 := f.Manifest(ctx, f.p("eva"), fk)
	_, e2 := f.Manifest(ctx, f.p("eva"), fk)
	_, e3 := f.Manifest(ctx, f.p("eva"), fk)
	f.ok(e1)
	f.ok(e2)
	if _, ok := e3.(ErrLimit); !ok {
		t.Fatalf("third answer: %v", e3)
	}
	for i := 0; i < 4; i++ {
		_, e := f.Manifest(ctx, f.p("dana"), fk)
		is(t, e, ErrNotFound)
	}
}

// ---- the local side: the dry run

func pullFx(t *testing.T) (*fx, int64) {
	f := newFx(t)
	f.ok(f.AddUpstream(ctx, "hub", "hub.example:8765"))
	fk := f.pid["fakture"]
	f.ok(f.LinkUpstream(ctx, fk, 0, "hub", "shop", "", false))
	return f, fk
}

func manifestUp(manifest string, status int) *fakeUp {
	return &fakeUp{handler: func(c upCall) (int, string, time.Duration) {
		switch {
		case c.method == "GET" && c.path == "/minigw/v1/projects/shop":
			return 200, `{"id":7,"slug":"shop"}`, 0
		case c.method == "GET" && c.path == "/minigw/v1/projects/7/kb/manifest":
			return status, manifest, 0
		}
		return 404, `{"error":{"message":"not found"}}`, 0
	}}
}

func tableCounts(t *testing.T, f *fx) string {
	var names []string
	rows, err := f.DB().Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE '%\_fts%' ESCAPE '\' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	f.ok(err)
	for rows.Next() {
		var n string
		f.ok(rows.Scan(&n))
		names = append(names, n)
	}
	rows.Close()
	var out []string
	for _, n := range names {
		var c int
		if err := f.DB().QueryRow(`SELECT count(*) FROM "` + n + `"`).Scan(&c); err != nil {
			continue // a virtual table of the search index
		}
		out = append(out, fmt.Sprint(n, "=", c))
	}
	return strings.Join(out, ",")
}

func TestPullDryRunReportsAndStoresNothing(t *testing.T) {
	f, fk := pullFx(t)
	before := tableCounts(t, f)
	up := manifestUp(`{"v":1,"sources":[{"id":7,"label":"project:shop"},{"id":1099511627777,"label":"team:acme"}],"notes":[
		{"id":1,"source":7,"rev":1,"class":"structure","kind":"finding"},{"id":2,"source":7,"rev":4,"class":"structure","kind":"faq"},
		{"id":3,"source":7,"rev":1,"class":"data","kind":"finding"},{"id":9,"source":1099511627777,"rev":2,"class":"structure","kind":"finding"}],"truncated":false,"future":"ignored"}`, 200)
	res, err := f.PullUpstream(ctx, up, fk, 0, "", true, noSleep)
	f.ok(err)
	if len(res) != 1 || res[0].Stopped != "" || res[0].Total != 4 || res[0].Truncated {
		t.Fatalf("%+v", res)
	}
	want := []string{"source project:shop: 3 notes (structure faq 1, structure finding 1, data finding 1)", "source team:acme: 1 notes (structure finding 1)"}
	if strings.Join(res[0].Lines, "|") != strings.Join(want, "|") {
		t.Fatalf("%q", res[0].Lines)
	}
	if after := tableCounts(t, f); after != before {
		t.Fatalf("a dry run wrote:\n%s\n%s", before, after)
	}
	// only GETs, and only requests the Mac app admits (the shared fixture)
	raw, e := os.ReadFile("../testdata/upstream-requests.json")
	f.ok(e)
	var fx struct {
		Patterns []struct{ Method, Pattern string }
	}
	f.ok(json.Unmarshal(raw, &fx))
	for _, c := range up.calls {
		ok := false
		for _, p := range fx.Patterns {
			ok = ok || p.Method == c.method && regexp.MustCompile(p.Pattern).MatchString(c.path)
		}
		if !ok {
			t.Errorf("%s %s is not admitted by the shared fixture", c.method, c.path)
		}
	}
	if up.count("GET") != len(up.calls) || len(up.calls) != 2 {
		t.Fatalf("%+v", up.calls)
	}
	// a real pull needs reading turned on for the link (TestPullRefusedWithoutReadingOrLinkAndNotWhenBusy): nothing is contacted without it
	_, err = f.PullUpstream(ctx, up, fk, 0, "", false, noSleep)
	is(t, err, ErrRefusal{"reading is off for this link: minigw gw upstream link <project> <upstream> --read --accept-reach (look first: pull <project> --dry-run)"})
	if len(up.calls) != 2 {
		t.Fatal("a refused pull contacted the upstream")
	}
	_, err = f.PullUpstream(ctx, up, f.pid["eureko"], 0, "", true, noSleep)
	is(t, err, ErrRefusal{"no upstream link for this: minigw gw upstream link <project> <upstream>"})
}

// A hostile or broken upstream's manifest is data: every one of these ends the dry run with a "stopped" line and takes nothing.
func TestPullDryRunHostileManifests(t *testing.T) {
	huge := `{"v":1,"sources":[{"id":7,"label":"x"}],"notes":[],"pad":"` + strings.Repeat("a", 1<<20) + `"}`
	many := func(n int) string {
		var b strings.Builder
		b.WriteString(`{"v":1,"sources":[{"id":7,"label":"p"}],"notes":[`)
		for i := 1; i <= n; i++ {
			if i > 1 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, `{"id":%d,"source":7,"rev":1,"class":"structure","kind":"finding"}`, i)
		}
		return b.String() + `],"truncated":false}`
	}
	src := `"sources":[{"id":7,"label":"p"}]`
	one := func(note string) string { return `{"v":1,` + src + `,"notes":[` + note + `],"truncated":false}` }
	for name, c := range map[string]struct {
		body   string
		status int
		stop   string
	}{
		"not json":       {`<html>`, 200, "not usable"},
		"trailing data":  {one(``) + `{}`, 200, "not usable"},
		"version 2":      {`{"v":2,"sources":[],"notes":[]}`, 200, "does not support reading"},
		"no version":     {`{"sources":[],"notes":[]}`, 200, "does not support reading"},
		"over the size":  {huge, 200, "no answer"},
		"too many notes": {many(MaxManifest + 1), 200, "not usable"},
		"private class":  {one(`{"id":1,"source":7,"rev":1,"class":"private","kind":"finding"}`), 200, "not usable"},
		"odd class":      {one(`{"id":1,"source":7,"rev":1,"class":"x","kind":"finding"}`), 200, "not usable"},
		"odd kind":       {one(`{"id":1,"source":7,"rev":1,"class":"data","kind":"x"}`), 200, "not usable"},
		"negative id":    {one(`{"id":-1,"source":7,"rev":1,"class":"data","kind":"faq"}`), 200, "not usable"},
		"zero revision":  {one(`{"id":1,"source":7,"rev":0,"class":"data","kind":"faq"}`), 200, "not usable"},
		"duplicate id":   {one(`{"id":1,"source":7,"rev":1,"class":"data","kind":"faq"},{"id":1,"source":7,"rev":2,"class":"data","kind":"faq"}`), 200, "not usable"},
		"unknown source": {one(`{"id":1,"source":8,"rev":1,"class":"data","kind":"faq"}`), 200, "not usable"},
		"id is a string": {one(`{"id":"1","source":7,"rev":1,"class":"data","kind":"faq"}`), 200, "not usable"},
		"id too big":     {one(`{"id":99999999999999999999,"source":7,"rev":1,"class":"data","kind":"faq"}`), 200, "not usable"},
		"duplicate src":  {`{"v":1,"sources":[{"id":7,"label":"p"},{"id":7,"label":"q"}],"notes":[]}`, 200, "not usable"},
		"null":           {`null`, 200, "does not support reading"},
		"old hub 404":    {`{"error":{"message":"not found"}}`, 404, "does not support reading"},
		"old hub 501":    {`{}`, 501, "does not support reading"},
		"refused 403":    {`{"error":{"message":"no role"}}`, 403, "the upstream says: no role"},
		"unauthorised":   {`{}`, 401, "refuses this gateway"},
		"server error":   {`{}`, 500, "HTTP 500"},
	} {
		t.Run(name, func(t *testing.T) {
			f, fk := pullFx(t)
			before := tableCounts(t, f)
			res, err := f.PullUpstream(ctx, manifestUp(c.body, c.status), fk, 0, "", true, noSleep)
			f.ok(err)
			if !strings.Contains(res[0].Stopped, c.stop) || res[0].Total != 0 || len(res[0].Lines) != 0 {
				t.Fatalf("%+v", res[0])
			}
			if tableCounts(t, f) != before {
				t.Fatal("the database changed")
			}
		})
	}
	// text from the upstream is one printable line: control characters in a label are cleaned
	f, fk := pullFx(t)
	res, err := f.PullUpstream(ctx, manifestUp(`{"v":1,"sources":[{"id":7,"label":"p\u001b[31m\nx"}],"notes":[],"truncated":false}`, 200), fk, 0, "", true, noSleep)
	f.ok(err)
	if strings.ContainsAny(strings.Join(res[0].Lines, ""), "\x1b\n") {
		t.Fatalf("%q", res[0].Lines)
	}
}

func TestPullDryRunTruncatedAndRateLimited(t *testing.T) {
	f, fk := pullFx(t)
	res, err := f.PullUpstream(ctx, manifestUp(`{"v":1,"sources":[{"id":7,"label":"p"}],"notes":[{"id":1,"source":7,"rev":1,"class":"data","kind":"faq"}],"truncated":true}`, 200), fk, 0, "", true, noSleep)
	f.ok(err)
	if !res[0].Truncated || res[0].Stopped != "" || !strings.Contains(strings.Join(res[0].Lines, "\n"), "delete nothing") {
		t.Fatalf("%+v", res[0])
	}
	// 429 that cannot be waited out ends the run with a line, within the call; one that can is waited and retried a bounded number of times
	calls := 0
	up := &fakeUp{handler: func(c upCall) (int, string, time.Duration) {
		if c.path == "/minigw/v1/projects/shop" {
			return 200, `{"id":7,"slug":"shop"}`, 0
		}
		calls++
		return 429, `{}`, time.Second
	}}
	res, err = f.PullUpstream(ctx, up, fk, 0, "", true, func(time.Duration) bool { return false })
	f.ok(err)
	if calls != 1 || !strings.Contains(res[0].Stopped, "read limit") {
		t.Fatalf("%d %+v", calls, res[0])
	}
	calls = 0
	res, err = f.PullUpstream(ctx, up, fk, 0, "", true, noSleep)
	f.ok(err)
	if calls != maxRateWaits+1 || res[0].Stopped == "" {
		t.Fatalf("%d %+v", calls, res[0])
	}
}
