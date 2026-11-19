package main

import (
	"bufio"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"minigw/core"
)

// kbRemote: a remote settings listener in front of a real core store with one user (ana, a writer in fakture).
type kbRemote struct {
	t     *testing.T
	f     *remoteTransportFixture
	cf    *coreFx
	ana   core.Principal
	pid   int64
	clock time.Time
	n     int
}

func newKBRemote(t *testing.T) *kbRemote {
	t.Helper()
	f := remoteTransport(t)
	cf := newCoreFx(t, func(c *serveConfig) { c.Token = "gw-token-Zq7xK2" })
	cf.s.cfg.RemoteSettings, cf.s.remoteAuth = f.c, f.s.remoteAuth
	f.s = cf.s
	k := &kbRemote{t: t, f: f, cf: cf, pid: cf.pid["fakture"], clock: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	k.ana = core.Principal{UserID: cf.uid["ana"], DeviceID: devID(cf.dev["ana"])}
	cf.s.remoteAuth.now = func() time.Time { return k.clock }
	return k
}

// submit: a pending submission by ana (the core refuses hidden characters here; k.setBody puts them in afterwards).
func (k *kbRemote) submit(title, body string) int64 {
	k.t.Helper()
	k.n++
	v, _, err := k.cf.s.core.Submit(t0, k.ana, k.pid, fmt.Sprintf("remote-kb-%d", k.n), core.SubmissionInput{NoteInput: core.NoteInput{Class: core.Data, Title: title, Body: body}})
	if err != nil {
		k.t.Fatal(err)
	}
	return v.ID
}

func (k *kbRemote) exec(query string, args ...any) {
	k.t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(k.cf.s.cfgDir, "core", "core.db"))
	if err != nil {
		k.t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec(query, args...); err != nil {
		k.t.Fatal(err)
	}
}

func (k *kbRemote) count(query string, args ...any) (n int) {
	k.t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(k.cf.s.cfgDir, "core", "core.db")+"?mode=ro")
	if err != nil {
		k.t.Fatal(err)
	}
	defer db.Close()
	if err = db.QueryRow(query, args...).Scan(&n); err != nil {
		k.t.Fatal(err)
	}
	return
}

func (k *kbRemote) do(method, path, body string) *httptest.ResponseRecorder {
	return k.f.run(k.f.request(method, adminPrefix+path, body))
}

type kbRead struct {
	ID         int64  `json:"id"`
	State      string `json:"state"`
	Title      string `json:"title"`
	Body       string `json:"body"`
	ReviewHash string `json:"reviewHash"`
	Check      string `json:"reviewCheck"`
	Hidden     int    `json:"hidden"`
	Note       int64  `json:"note"`
	Author     string `json:"authorName"`
	Scope      string `json:"scopeName"`
	Reason     string `json:"reason"`
	// the replaced note, as the page receives it
	TargetState     string `json:"targetState"`
	CurrentRevision int64  `json:"currentRevision"`
	CurrentTitle    string `json:"currentTitle"`
	CurrentBody     string `json:"currentBody"`
	CurrentHidden   int    `json:"currentHidden"`
}

func (k *kbRemote) read(id int64) kbRead {
	k.t.Helper()
	w := k.do("GET", fmt.Sprintf("/kb/submissions/%d", id), "")
	var v kbRead
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &v) != nil {
		k.t.Fatal("read", id, w.Code, w.Body.String())
	}
	return v
}

func (k *kbRemote) accept(id int64, hash string) *httptest.ResponseRecorder {
	return k.do("POST", fmt.Sprintf("/kb/submissions/%d/accept", id), `{"reviewHash":"`+hash+`"}`)
}

func (k *kbRemote) acceptAt(id int64, hash string, current int64) *httptest.ResponseRecorder {
	return k.do("POST", fmt.Sprintf("/kb/submissions/%d/accept", id), fmt.Sprintf(`{"reviewHash":"%s","currentRevision":%d}`, hash, current))
}

func (k *kbRemote) reject(id int64, reason string) *httptest.ResponseRecorder {
	b, _ := json.Marshal(map[string]string{"reason": reason})
	return k.do("POST", fmt.Sprintf("/kb/submissions/%d/reject", id), string(b))
}

func (k *kbRemote) remoteAudit() (rows []map[string]string) {
	k.t.Helper()
	f, err := os.Open(filepath.Join(filepath.Dir(k.f.config), "remote-settings.audit.jsonl"))
	if err != nil {
		k.t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var m map[string]string
		if json.Unmarshal(sc.Bytes(), &m) != nil {
			k.t.Fatal(sc.Text())
		}
		rows = append(rows, m)
	}
	return
}

// Reading, accepting and rejecting remotely: the exact version read is published, a changed or hidden one is not, a body
// that is not exactly one of the two shapes never reaches the core.
func TestRemoteKBReviewEndToEnd(t *testing.T) {
	k := newKBRemote(t)
	notes := func() int { return k.count(`SELECT COUNT(*) FROM notes`) }
	pending := func(id int64) int {
		return k.count(`SELECT COUNT(*) FROM kb_submissions WHERE id=? AND state='pending'`, id)
	}
	secrets := []string{"gw-token-Zq7xK2", remoteTestPassword, k.f.token}

	// list and read: the text as submitted (HTML stays literal text), the hash only in the read
	hostile := "<script>alert(1)</script>\n<img src=x onerror=alert(1)>\n"
	id := k.submit("T <b>one</b>", hostile)
	w := k.do("GET", "/kb/submissions", "")
	var list struct {
		Submissions []kbRead
		Next        int64
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &list) != nil || len(list.Submissions) != 1 || list.Submissions[0].ID != id || list.Submissions[0].Title != "T <b>one</b>" || list.Next != 0 {
		t.Fatal("list", w.Code, w.Body.String())
	}
	for _, v := range secrets {
		if strings.Contains(w.Body.String(), v) {
			t.Fatal("the list leaks a secret")
		}
	}
	v := k.read(id)
	if v.Body != hostile || v.Check != "ok" || v.Hidden != 0 || len(v.ReviewHash) != 64 || v.Author != "ana" || v.Scope != "fakture" || v.State != "pending" {
		t.Fatalf("%+v", v)
	}

	// accept the version read: the note is that text, the core audit names the owner
	if w = k.accept(id, v.ReviewHash); w.Code != 200 {
		t.Fatal("accept", w.Code, w.Body.String())
	}
	var done kbRead
	json.Unmarshal(w.Body.Bytes(), &done)
	if done.State != "accepted" || done.Note == 0 || notes() != 1 || k.count(`SELECT COUNT(*) FROM notes WHERE title=? AND id=?`, "T <b>one</b>", done.Note) != 1 {
		t.Fatalf("%+v notes=%d", done, notes())
	}
	if k.count(`SELECT COUNT(*) FROM audit_events WHERE actor='owner' AND action='kb.accepted' AND subject=?`, fmt.Sprint(id)) != 1 {
		t.Fatal("no core audit row kb.accepted by owner")
	}
	// a replay of the same accept: 200, the same note, no second publication
	if w = k.accept(id, v.ReviewHash); w.Code != 200 || notes() != 1 {
		t.Fatal("replay", w.Code, w.Body.String(), notes())
	}

	// reject with a reason, then accept of the rejected one: 400
	id2 := k.submit("two", "second")
	v2 := k.read(id2)
	if w = k.reject(id2, "not for the team: <b>x</b>"); w.Code != 200 || k.read(id2).State != "rejected" || k.read(id2).Reason != "not for the team: <b>x</b>" {
		t.Fatal("reject", w.Code, w.Body.String())
	}
	if w = k.accept(id2, v2.ReviewHash); w.Code != 400 || notes() != 1 {
		t.Fatal("accept after reject", w.Code, w.Body.String())
	}

	// changed after the read: refused with the core's message, nothing published, still pending
	id3 := k.submit("three", "before")
	v3 := k.read(id3)
	k.exec(`UPDATE kb_submissions SET body=? WHERE id=?`, "after", id3)
	if w = k.accept(id3, v3.ReviewHash); w.Code != 400 || !strings.Contains(w.Body.String(), "changed since you read it") || notes() != 1 || pending(id3) != 1 {
		t.Fatal("changed", w.Code, w.Body.String())
	}
	if v3b := k.read(id3); v3b.ReviewHash == v3.ReviewHash || v3b.Body != "after" {
		t.Fatal("the re-read shows the new text and a new hash")
	}

	// hidden characters: the read removes and counts them, accept is refused, reject works
	id4 := k.submit("four", "plain")
	k.exec(`UPDATE kb_submissions SET body=? WHERE id=?`, "a‮b​c", id4)
	v4 := k.read(id4)
	if v4.Body != "abc" || v4.Hidden != 2 || v4.Check != "hidden" {
		t.Fatalf("%+v", v4)
	}
	if w = k.accept(id4, v4.ReviewHash); w.Code != 403 || !strings.Contains(w.Body.String(), "hidden characters") || notes() != 1 || pending(id4) != 1 {
		t.Fatal("hidden accepted", w.Code, w.Body.String())
	}
	if w = k.reject(id4, "hidden characters"); w.Code != 200 {
		t.Fatal("reject of a hidden one", w.Code)
	}

	// every case of the shared review fixture, submitted as the stored text: the answer is the fixture's output
	var cases []struct{ Name, In, Out string }
	if err := json.Unmarshal(srcFile(t, "testdata/review-text.json"), &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		cid := k.submit("fx "+c.Name, "x")
		k.exec(`UPDATE kb_submissions SET body=? WHERE id=?`, c.In, cid)
		got := k.read(cid)
		if got.Body != c.Out || got.Hidden != len([]rune(c.In))-len([]rune(c.Out)) {
			t.Errorf("%s: %q hidden %d, want %q", c.Name, got.Body, got.Hidden, c.Out)
		}
		if w = k.accept(cid, got.ReviewHash); got.Hidden > 0 && (w.Code != 403 || pending(cid) != 1) {
			t.Errorf("%s: accepted with hidden characters: %d", c.Name, w.Code)
		}
	}

	// a 64 KiB body: read as review check "long" (it can only be rejected here), no failure
	big := k.submit("big", "x")
	k.exec(`UPDATE kb_submissions SET body=? WHERE id=?`, strings.Repeat("y", 64<<10), big)
	if got := k.read(big); got.Check != "long" {
		t.Fatalf("check %q", got.Check)
	} else if w = k.accept(big, got.ReviewHash); w.Code != 403 || !strings.Contains(w.Body.String(), "too long to review here") || pending(big) != 1 {
		t.Fatal("a long submission accepted remotely", w.Code, w.Body.String())
	}

	// a replacement is offered for accept only while the note it replaces is unchanged and both texts read ok
	noteRev := func() int64 { return int64(k.count(`SELECT revision FROM notes WHERE id=?`, done.Note)) }
	replace := func(title, body string) int64 {
		k.t.Helper()
		k.n++
		v, _, err := k.cf.s.core.Submit(t0, k.ana, k.pid, fmt.Sprintf("remote-kb-repl-%d", k.n), core.SubmissionInput{NoteInput: core.NoteInput{Class: core.Data, Title: title, Body: body}, Replaces: done.Note, Revision: noteRev()})
		if err != nil {
			k.t.Fatal(err)
		}
		return v.ID
	}
	edit := func(body string) {
		k.t.Helper()
		if _, err := k.cf.s.core.UpdateNote(t0, k.ana, k.pid, done.Note, noteRev(), core.NoteInput{Class: core.Data, Title: "T <b>one</b>", Body: body}); err != nil {
			k.t.Fatal(err)
		}
	}
	history := func() int { return k.count(`SELECT COUNT(*) FROM note_history WHERE note_id=?`, done.Note) }
	refuseAccept := func(what string, rid int64, want string) {
		k.t.Helper()
		got := k.read(rid)
		before := history()
		if w := k.acceptAt(rid, got.ReviewHash, got.CurrentRevision); w.Code != 403 || !strings.Contains(w.Body.String(), want) || pending(rid) != 1 || history() != before {
			k.t.Errorf("%s: %d %s", what, w.Code, w.Body.String())
		}
		if w := k.reject(rid, "not here"); w.Code != 200 {
			k.t.Errorf("%s: reject %d", what, w.Code)
		}
	}
	// eligible: both texts are in the read, accept with the revision shown publishes and keeps the old text
	repl := replace("T <b>one</b>", "new text")
	vr := k.read(repl)
	if vr.TargetState != "ok" || vr.Check != "ok" || vr.CurrentRevision != noteRev() || vr.CurrentTitle != "T <b>one</b>" || vr.CurrentBody != hostile || vr.Body != "new text" {
		t.Fatalf("%+v", vr)
	}
	deniedRows := func(id int64) (n int) {
		for _, row := range k.remoteAudit() {
			if row["Subject"] == fmt.Sprint(id) && row["Result"] == "denied" {
				n++
			}
			if row["Subject"] == fmt.Sprint(id) && row["Result"] == "started" {
				k.t.Fatal("a refused body wrote a started row")
			}
		}
		return
	}
	// a replacement without currentRevision (or 0) and a new note with one are refused before the limiter and the core
	for _, b := range []string{`{"reviewHash":"` + vr.ReviewHash + `"}`, `{"reviewHash":"` + vr.ReviewHash + `","currentRevision":0}`} {
		before := deniedRows(repl)
		if w = k.do("POST", fmt.Sprintf("/kb/submissions/%d/accept", repl), b); w.Code != 403 || !strings.Contains(w.Body.String(), "must carry currentRevision") || deniedRows(repl) != before+1 || pending(repl) != 1 {
			t.Fatal("accepted without currentRevision", w.Code, w.Body.String())
		}
	}
	plain := k.submit("plain with revision", "plain")
	pr := k.read(plain)
	if w = k.acceptAt(plain, pr.ReviewHash, 1); w.Code != 403 || !strings.Contains(w.Body.String(), "must not carry currentRevision") || deniedRows(plain) != 1 || pending(plain) != 1 {
		t.Fatal("a new note with currentRevision", w.Code, w.Body.String())
	}
	k.reject(plain, "done")
	// a wrong value is refused at the gateway's door: 403, one denied row, no started row, nothing reaches the core
	coreRows := k.count(`SELECT COUNT(*) FROM audit_events`)
	deniedBefore := deniedRows(repl)
	if w = k.acceptAt(repl, vr.ReviewHash, vr.CurrentRevision+1); w.Code != 403 || !strings.Contains(w.Body.String(), "changed since you read it") || deniedRows(repl) != deniedBefore+1 || k.count(`SELECT COUNT(*) FROM audit_events`) != coreRows || pending(repl) != 1 {
		t.Fatal("accepted with a wrong currentRevision", w.Code, w.Body.String())
	}
	if w = k.acceptAt(repl, vr.ReviewHash, vr.CurrentRevision); w.Code != 200 || k.count(`SELECT COUNT(*) FROM notes WHERE id=? AND body=?`, done.Note, "new text") != 1 || history() != 1 || k.count(`SELECT COUNT(*) FROM note_history WHERE note_id=? AND body=?`, done.Note, hostile) != 1 {
		t.Fatal("an eligible replacement", w.Code, w.Body.String())
	}
	// not eligible: stale, missing, hidden or long current text, an edit after the read
	stale := replace("T <b>one</b>", "for a stale target")
	edit("edited after the proposal")
	refuseAccept("stale", stale, "changed after this was written")
	// a reject without a typed reason of a stale replacement tells its author why (the same core rule as the CLI's)
	stale2 := replace("T <b>one</b>", "for a second stale target")
	edit("edited again after the proposal")
	if w = k.reject(stale2, ""); w.Code != 200 || k.count(`SELECT COUNT(*) FROM kb_submissions WHERE id=? AND reason LIKE ?`, stale2, "note % changed after this was written (it expected revision %, the note is at %): read the note and propose the change again") != 1 {
		t.Fatal("default reason", w.Code, w.Body.String())
	}
	// hidden characters in the CURRENT text do not block the accept: the new text is published clean, the history keeps the old one raw
	hiddenCur := replace("T <b>one</b>", "for a hidden current text")
	rawCur := "cur‮rent​"
	k.exec(`UPDATE notes SET body=? WHERE id=?`, rawCur, done.Note)
	got := k.read(hiddenCur)
	if got.CurrentHidden != 2 || got.Check != "ok" || got.CurrentBody != "current" {
		t.Fatalf("%+v", got)
	}
	if w = k.acceptAt(hiddenCur, got.ReviewHash, got.CurrentRevision); w.Code != 200 || k.count(`SELECT COUNT(*) FROM notes WHERE id=? AND body=?`, done.Note, "for a hidden current text") != 1 || k.count(`SELECT COUNT(*) FROM note_history WHERE note_id=? AND body=?`, done.Note, rawCur) != 1 {
		t.Fatal("hidden current text blocked or not kept", w.Code, w.Body.String())
	}
	k.exec(`UPDATE notes SET body=? WHERE id=?`, "plain again", done.Note)
	longCur := replace("T <b>one</b>", "for a long current text")
	k.exec(`UPDATE notes SET body=? WHERE id=?`, strings.Repeat("z", 64<<10), done.Note)
	if got := k.read(longCur); got.Check != "long" || got.TargetState != "ok" {
		t.Fatalf("%+v", got)
	}
	refuseAccept("long current", longCur, "too long to review here")
	k.exec(`UPDATE notes SET body=? WHERE id=?`, "plain again", done.Note)
	moved := replace("T <b>one</b>", "edited between the read and the accept")
	vm := k.read(moved)
	edit("edited by somebody else")
	if w = k.acceptAt(moved, vm.ReviewHash, vm.CurrentRevision); w.Code != 403 || pending(moved) != 1 {
		t.Fatal("an accept after an edit of the target", w.Code, w.Body.String())
	}
	k.reject(moved, "stale")
	// a hand-made accept with the hash of the 0.8.7 formula never passes the core's comparison
	old := replace("T <b>one</b>", "old formula")
	var cls, ttl, kd, bd, prov string
	var rp, rv int64
	db, _ := sql.Open("sqlite", filepath.Join(k.cf.s.cfgDir, "core", "core.db")+"?mode=ro")
	if err := db.QueryRow(`SELECT class,title,entry_kind,body,replaces,revision,provenance FROM kb_submissions WHERE id=?`, old).Scan(&cls, &ttl, &kd, &bd, &rp, &rv, &prov); err != nil {
		t.Fatal(err)
	}
	db.Close()
	ob, _ := json.Marshal([]any{old, cls, ttl, kd, bd, rp, rv, prov})
	sum := sha256.Sum256(ob)
	if w = k.acceptAt(old, hex.EncodeToString(sum[:]), noteRev()); w.Code != 400 || !strings.Contains(w.Body.String(), "changed since you read it") || pending(old) != 1 {
		t.Fatal("an old-formula hash", w.Code, w.Body.String())
	}
	k.reject(old, "done")
	// every case of the shared review fixture applied to the current text as the page receives it
	var curCases []struct{ Name, In, Out, Side string }
	if err := json.Unmarshal(srcFile(t, "testdata/review-text.json"), &curCases); err != nil {
		t.Fatal(err)
	}
	cur := replace("T <b>one</b>", "fixture")
	for _, c := range curCases {
		k.exec(`UPDATE notes SET body=?, title=? WHERE id=?`, c.In, "T "+c.In, done.Note)
		got := k.read(cur)
		if got.CurrentBody != c.Out || got.CurrentHidden < 0 {
			t.Errorf("%s: current body %q, want %q", c.Name, got.CurrentBody, c.Out)
		}
		if got.Check == "hidden" {
			t.Errorf("%s: the current side made the check hidden", c.Name)
		}
	}
	k.reject(cur, "done")
	k.exec(`UPDATE notes SET body=?, title=? WHERE id=?`, "plain again", "T <b>one</b>", done.Note)
	// a missing target
	gone := replace("T <b>one</b>", "for a deleted note")
	k.exec(`DELETE FROM notes WHERE id=?`, done.Note)
	if got := k.read(gone); got.TargetState != "missing" || got.Check != "target" {
		t.Fatalf("%+v", got)
	}
	refuseAccept("missing", gone, "no longer exists")

	// other bodies and routes never reach the core
	id5 := k.submit("five", "strict")
	h5 := k.read(id5).ReviewHash
	before, publishedBefore := k.count(`SELECT COUNT(*) FROM audit_events`), notes()
	accept := fmt.Sprintf("/kb/submissions/%d/accept", id5)
	reject := fmt.Sprintf("/kb/submissions/%d/reject", id5)
	for _, b := range []string{
		`{"reviewHash":"` + h5 + `","ReviewHash":"` + h5 + `"}`, `{"ReviewHash":"` + h5 + `"}`, `{"reviewhash":"` + h5 + `"}`, `{"reviewHash":"` + h5 + `","reviewHash":"` + h5 + `"}`,
		`{"reviewHash":"` + h5 + `","reason":"x"}`, `{"reviewHash":123}`, `{"reviewHash":null}`, `{"reviewHash":"` + h5[:63] + `"}`, `{"reviewHash":"` + h5 + `0"}`,
		`{"reviewHash":"` + strings.ToUpper(h5) + `"}`, `{"reviewHash":"` + strings.Repeat("g", 64) + `"}`, `{"reviewHash":""}`, `{}`, ``, `[]`, `"x"`, `{"reviewHash":"` + h5 + `"} {}`, `{"reviewHash":"` + h5 + `"}x`,
		`{"reviewHash":"` + h5 + `","currentRevision":"7"}`, `{"reviewHash":"` + h5 + `","currentRevision":-1}`, `{"reviewHash":"` + h5 + `","currentRevision":1.5}`, `{"reviewHash":"` + h5 + `","currentRevision":1e3}`,
		`{"reviewHash":"` + h5 + `","currentRevision":9007199254740993}`, `{"reviewHash":"` + h5 + `","currentRevision":null}`, `{"reviewHash":"` + h5 + `","currentRevision":01}`, `{"reviewHash":"` + h5 + `","currentRevision":1,"currentRevision":1}`,
		`{"reviewHash":"` + h5 + `","CurrentRevision":1}`, `{"reviewHash":"` + h5 + `","currentRevision":1,"reason":"x"}`, `{"reviewHash":"` + h5 + `","currentRevision":[1]}`,
	} {
		if w = k.do("POST", accept, b); w.Code != 403 {
			t.Errorf("accept body %q: %d", b, w.Code)
		}
	}
	long := strings.Repeat("r", 1025)
	for _, b := range []string{`{"reason":"x","reason":"y"}`, `{"Reason":"x"}`, `{"REASON":"x"}`, `{"reason":1}`, `{"reason":"` + long + `"}`, `{"reason":"x","reviewHash":"` + h5 + `"}`, `{}`, ``, `[]`, `{"reason":"x"} {}`} {
		if w = k.do("POST", reject, b); w.Code != 403 {
			t.Errorf("reject body %q: %d", b, w.Code)
		}
	}
	if w = k.do("POST", reject, `{"reason":"`+strings.Repeat("r", 9<<10)+`"}`); w.Code != 413 {
		t.Errorf("a body over the cap: %d", w.Code)
	}
	if w = k.reject(id5, strings.Repeat("é", 513)); w.Code != 403 { // 1026 bytes
		t.Errorf("a reason over 1024 bytes: %d", w.Code)
	}
	// session, Origin, request header and content type
	for name, change := range map[string]func(*http.Request){
		"no session":   func(r *http.Request) { r.Header.Del(remoteSessionHeader) },
		"no Origin":    func(r *http.Request) { r.Header.Del("Origin") },
		"wrong Origin": func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") },
		"no CSRF":      func(r *http.Request) { r.Header.Del(remoteCSRFHeader) },
		"text/plain":   func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") },
		"override":     func(r *http.Request) { r.Header.Set("X-HTTP-Method-Override", "DELETE") },
	} {
		r := k.f.request("POST", adminPrefix+accept, `{"reviewHash":"`+h5+`"}`)
		change(r)
		want := 403
		if name == "no session" {
			want = 401
		}
		if w = k.f.run(r); w.Code != want {
			t.Errorf("%s: %d", name, w.Code)
		}
	}
	if pending(id5) != 1 || notes() != publishedBefore || k.count(`SELECT COUNT(*) FROM audit_events`) != before {
		t.Fatal("a refused request changed the store")
	}

	// the remote audit: started, then the result, with the operation and the submission id
	var started, ok, denied int
	for _, row := range k.remoteAudit() {
		if row["Operation"] != "POST kb/submissions/{}/accept" || row["Subject"] != fmt.Sprint(id) {
			continue
		}
		switch row["Result"] {
		case "started":
			started++
		case "ok":
			ok++
		}
		if row["IP"] == "" || row["RequestID"] == "" || row["Principal"] != "remote-admin" || row["Action"] != "mutation" {
			t.Fatal(row)
		}
	}
	for _, row := range k.remoteAudit() {
		if row["Subject"] == fmt.Sprint(id3) && row["Result"] == "denied" && row["Operation"] == "POST kb/submissions/{}/accept" {
			denied++
		}
	}
	refused := 0
	for _, row := range k.remoteAudit() {
		if row["Subject"] == fmt.Sprint(id5) && row["Result"] == "denied" && row["Operation"] == "POST kb/submissions/{}/accept" {
			refused++
		}
		if row["Subject"] == fmt.Sprint(id5) && row["Result"] == "started" {
			t.Fatal("a refused body wrote a started row")
		}
	}
	if refused != 29 { // the 29 refused accept bodies above (18 before currentRevision, 11 after) each left one denied row
		t.Fatal("denied rows of refused bodies:", refused)
	}
	if started != 2 || ok != 2 || denied != 1 { // the accept and its replay; the changed one denied
		t.Fatal(started, ok, denied, k.remoteAudit())
	}
}

// The 31st decision in ten minutes is 429 and logged as limited; reads are not limited; the window moves on.
func TestRemoteKBReviewRateLimit(t *testing.T) {
	k := newKBRemote(t)
	hash := strings.Repeat("a", 64)
	for i := 1; i <= remoteDecisionLimit; i++ {
		if w := k.accept(int64(i), hash); w.Code != 404 { // no such submission: the core answers, the counter counts
			t.Fatal(i, w.Code, w.Body.String())
		}
	}
	id := k.submit("late", "text")
	w := k.reject(id, "no")
	if w.Code != 429 || k.read(id).State != "pending" {
		t.Fatal("the 31st decision", w.Code, w.Body.String())
	}
	if w = k.do("GET", "/kb/submissions", ""); w.Code != 200 {
		t.Fatal("reads are not limited", w.Code)
	}
	limited := 0
	for _, row := range k.remoteAudit() {
		if row["Result"] == "limited" && row["Operation"] == "POST kb/submissions/{}/reject" && row["Subject"] == fmt.Sprint(id) {
			limited++
		}
	}
	if limited != 1 {
		t.Fatal("no limited row", limited)
	}
	k.clock = k.clock.Add(remoteDecisionWindow + time.Second)
	k.f.token = remoteLogin(t, k.f.s.remoteAuth) // the session idle limit is 30 minutes, the window 10: log in again anyway
	if w = k.reject(id, "no"); w.Code != 200 {
		t.Fatal("after the window", w.Code, w.Body.String())
	}
}

// Every route of the KB admin API other than the four stays refused at the gateway, for every method; the four accept only theirs.
func TestRemoteKBPolicy(t *testing.T) {
	f := remoteTransport(t)
	hash := strings.Repeat("a", 64)
	names := map[string]bool{}
	for _, m := range regexp.MustCompile(`seg\[1\] == "([a-z]+)"`).FindAllStringSubmatch(string(srcFile(t, "coreapi/kb.go")), -1) {
		names[m[1]] = true
	}
	var refused []string
	for name := range names {
		if name != "submissions" {
			refused = append(refused, "/kb/"+name, "/kb/"+name+"/1", "/kb/"+name+"/rebuild", "/kb/"+name+"/1/accept")
		}
	}
	for _, name := range []string{"notes", "faq", "search"} {
		if !names[name] {
			t.Fatal("kb.go no longer routes", name, "(update this test)")
		}
	}
	refused = append(refused, "/kb", "/kb/", "/kb/submissions/", "/kb/submissions/1/", "/kb/submissions/1/versions/"+hash, "/kb/submissions/1/versions/ab", "/kb/submissions/1/accept/", "/kb/submissions/1/accept/2",
		"/kb/submissions/1a/accept", "/kb/submissions/-1/reject", "/kb/submissions/+1/accept", "/kb/submissions/1234567890123456789/accept", "/kb/submissions/1234567890123456789", "/kb/submissions/1/other",
		"/kb/submissions/%31/accept", "/kb%2Fsubmissions", "/kb/submissions%2F1", "/kb/submissions?project=1", "/kb/submissions?after=1", "/kb/submissions/1?x=1", "/kb/submissions/1/accept?x=1", "/KB/submissions", "/kb/Submissions/1/accept",
		"/kb/notes/1/history", "/kb/notes/1/history/2", "/kb/notes/1/restore")
	for _, path := range refused {
		for _, method := range []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"} {
			if remoteAdminAllowed(method, adminPrefix+path) && !strings.Contains(path, "?") {
				t.Fatal(method, "allowed", path)
			}
			if w := f.run(f.request(method, adminPrefix+path, `{"reviewHash":"`+hash+`"}`)); w.Code != 404 && w.Code != 403 && w.Code != 405 {
				t.Fatalf("%s %s: %d", method, path, w.Code)
			}
		}
	}
	for _, tc := range []struct {
		method, path string
		ok           bool
	}{
		{"GET", "/kb/submissions", true}, {"GET", "/kb/submissions/1", true}, {"GET", "/kb/submissions/0", true}, {"POST", "/kb/submissions/1/accept", true}, {"POST", "/kb/submissions/1/reject", true},
		{"POST", "/kb/submissions", false}, {"PUT", "/kb/submissions", false}, {"DELETE", "/kb/submissions/1", false}, {"POST", "/kb/submissions/1", false}, {"PATCH", "/kb/submissions/1", false},
		{"GET", "/kb/submissions/1/accept", false}, {"GET", "/kb/submissions/1/reject", false}, {"PUT", "/kb/submissions/1/accept", false}, {"DELETE", "/kb/submissions/1/reject", false}, {"HEAD", "/kb/submissions", false},
		{"HEAD", "/kb/submissions/1", false}, {"OPTIONS", "/kb/submissions", false},
	} {
		if remoteAdminAllowed(tc.method, adminPrefix+tc.path) != tc.ok {
			t.Fatal(tc)
		}
		if !tc.ok {
			if w := f.run(f.request(tc.method, adminPrefix+tc.path, `{"reviewHash":"`+hash+`"}`)); w.Code != 404 && w.Code != 403 && w.Code != 405 {
				t.Fatalf("%s %s: %d", tc.method, tc.path, w.Code)
			}
		}
	}
	for _, h := range []string{"X-HTTP-Method-Override", "X-HTTP-Method", "X-Method-Override"} {
		for _, path := range []string{"/kb/submissions/1/accept", "/kb/submissions/1/reject"} {
			r := f.request("POST", adminPrefix+path, `{"reviewHash":"`+hash+`"}`)
			r.Header.Set(h, "DELETE")
			if w := f.run(r); w.Code != 403 {
				t.Fatal("override", h, path, w.Code)
			}
		}
	}
}

// A body or an accept the gateway refuses before the core is asked never uses up one of the 30 decisions; the reason is one line of at most 1024 bytes.
func TestRemoteKBRefusalsDoNotCountAgainstTheLimiter(t *testing.T) {
	k := newKBRemote(t)
	count := func() int { return len(k.f.s.remoteAuth.decisions) }
	id := k.submit("limiter", "text")
	h := k.read(id).ReviewHash
	for _, why := range []string{"two\nlines", "tab\there", "carriage\rreturn", "esc\u001b[0m", strings.Repeat("é", 512) + "x", strings.Repeat("r", 1025)} {
		if w := k.reject(id, why); w.Code != 403 || count() != 0 || k.read(id).State != "pending" {
			t.Fatalf("reason %q: %d %s (count %d)", why, w.Code, w.Body.String(), count())
		}
	}
	for _, b := range []string{`{"reviewHash":"abc"}`, `{"ReviewHash":"` + h + `"}`, `{}`} {
		if w := k.do("POST", fmt.Sprintf("/kb/submissions/%d/accept", id), b); w.Code != 403 || count() != 0 {
			t.Fatal(b, w.Code, count())
		}
	}
	k.exec(`UPDATE kb_submissions SET body=? WHERE id=?`, "a\u200bb", id) // hidden: refused before the limiter too
	if w := k.accept(id, k.read(id).ReviewHash); w.Code != 403 || count() != 0 {
		t.Fatal("hidden", w.Code, count())
	}
	// 1024 bytes of two-byte characters is allowed, and it counts
	if w := k.reject(id, strings.Repeat("é", 512)); w.Code != 200 || count() != 1 {
		t.Fatal("a 1024 byte reason", w.Code, w.Body.String(), count())
	}
	// the 31st: 429 with a Retry-After the page turns into a time of day
	k.f.s.remoteAuth.decisions = nil
	for i := 1; i <= remoteDecisionLimit; i++ {
		k.accept(int64(1000+i), strings.Repeat("a", 64))
	}
	k.clock = k.clock.Add(90 * time.Second)
	w := k.reject(id, "again")
	if w.Code != 429 || w.Header().Get("Retry-After") != fmt.Sprint(int(remoteDecisionWindow/time.Second)-90+1) {
		t.Fatal(w.Code, w.Header())
	}
}

// Refused decisions have their own looser limit (so garbage cannot rotate the audit log), independent of the 30 real ones:
// over it the answer is 429 with Retry-After and one `limited` row per window; a real decision still goes through.
func TestRemoteKBRefusalLimiterIsIndependent(t *testing.T) {
	k := newKBRemote(t)
	id := k.submit("flood", "text")
	bad := func() *httptest.ResponseRecorder {
		return k.do("POST", fmt.Sprintf("/kb/submissions/%d/accept", id), `{"reviewHash":"abc"}`)
	}
	for i := 1; i <= remoteRefusalLimit; i++ {
		if w := bad(); w.Code != 403 {
			t.Fatal(i, w.Code)
		}
	}
	w := bad()
	if w.Code != 429 || w.Header().Get("Retry-After") != fmt.Sprint(int(remoteDecisionWindow/time.Second)+1) {
		t.Fatal("the 301st refused decision", w.Code, w.Header())
	}
	for i := 0; i < 50; i++ {
		if w = bad(); w.Code != 429 {
			t.Fatal(w.Code)
		}
	}
	denied, limited := 0, 0
	for _, row := range k.remoteAudit() {
		switch row["Result"] {
		case "denied":
			denied++
		case "limited":
			limited++
		}
	}
	if denied != remoteRefusalLimit || limited != 1 {
		t.Fatal("audit rows: denied", denied, "limited", limited)
	}
	// a real decision is not locked out by it, and it is governed by its own counter
	if w = k.reject(id, "a real decision"); w.Code != 200 || len(k.f.s.remoteAuth.decisions) != 1 {
		t.Fatal("a real decision during a flood of refused ones", w.Code, w.Body.String())
	}
	// after the window refused ones are answered (403) again, and the next window may write its own `limited` row
	k.clock = k.clock.Add(remoteDecisionWindow + time.Second)
	k.f.token = remoteLogin(t, k.f.s.remoteAuth)
	if w = bad(); w.Code != 403 {
		t.Fatal("after the window", w.Code)
	}
}
