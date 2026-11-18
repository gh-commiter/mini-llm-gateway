package core

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Step C, the mirror: a bounded copy of what an upstream serves for a link whose owner turned reading on. Everything the upstream says is
// DATA: it is checked against the manifest it came with, sanitised, and written only into upstream_mirror (and its search index) and the
// status columns of its own link. It never touches `notes`, never names a host, and its ids are used only in "%d" positions of requests to
// THAT upstream and as keys inside THAT link's mirror.
const (
	MaxMirrorNotes = 5000     // per link
	MaxMirrorBytes = 64 << 20 // what the mirrors of a gateway cost on disk, approximately: mirrorCost times their titles and bodies
	// mirrorCost: a mirrored note is kept three times: its row, the search table's own copy of the text and the search index (measured
	// roughly 2.5 to 3 times the text; the factor leaves the bound honest without a page count). So 64 MiB of disk is about 21 MiB of text.
	mirrorCost   = 3
	maxPullFails = 3 // transient failures (no answer, 5xx) of note GETs that end a run
)

// Variables so that a test can use small ones.
var (
	maxPullFetch    = 200              // note GETs per run
	pullCallTimeout = 30 * time.Second // one request to the upstream
	pullRunTimeout  = 5 * time.Minute  // the whole run (a helper's call has a shorter budget of its own)
)

func readInfo(tx *sql.Tx, now int64, i *UpstreamLinkInfo) error {
	var on int
	var cls, src string
	var acc, last, ok int64
	if e := tx.QueryRow(`SELECT read,read_classes,read_sources,read_accepted,last_pull,last_pull_ok,pull_error FROM upstream_links WHERE project_id=? AND upstream=?`, i.Project, i.Upstream).Scan(&on, &cls, &src, &acc, &last, &ok, &i.PullError); e != nil {
		return e
	}
	i.Read = on == 1
	for _, c := range strings.Split(cls, ",") {
		i.ReadClasses = append(i.ReadClasses, Class(c))
	}
	if src != "" {
		i.ReadSources = strings.Split(src, ",")
	}
	ptr := func(v int64) *time.Time {
		if v <= 0 {
			return nil
		}
		x := fromMs(v)
		return &x
	}
	i.ReadAccepted, i.LastPull, i.LastPullOK = ptr(acc), ptr(last), ptr(ok)
	if ok > 0 { // the owner's status says how old the mirror is; the age is the gateway's own clock, not the reader's
		i.LastPullAge = max(0, int((now-ok)/1000))
	}
	if e := tx.QueryRow(`SELECT count(*),coalesce(sum(bytes),0) FROM upstream_mirror WHERE project_id=? AND upstream=?`, i.Project, i.Upstream).Scan(&i.Mirror, &i.MirrorBytes); e != nil {
		return e
	}
	i.Stale = i.Read && i.Mirror > 0 && ok <= now-mirrorStale.Milliseconds() // members do not see it (search and read hide it)
	return nil
}

// pullSettings: what the owner accepted for the link, as the run starts. A run that finds these changed when it writes stores nothing.
type pullSettings struct {
	on       bool
	classes  map[Class]bool
	sources  map[string]bool
	accepted int64
	reach    string
	cursor   int64
}

func (s *Store) pullSettings(ctx context.Context, l UpstreamLink) (st pullSettings, err error) {
	err = s.read(ctx, func(tx *sql.Tx) error {
		var on int
		var cls, src string
		if e := tx.QueryRow(`SELECT read,read_classes,read_sources,read_accepted,read_reach,pull_cursor FROM upstream_links WHERE project_id=? AND upstream=?`, l.Project, l.Upstream).Scan(&on, &cls, &src, &st.accepted, &st.reach, &st.cursor); e != nil {
			return notFound(e)
		}
		st.on, st.classes, st.sources = on == 1, map[Class]bool{}, map[string]bool{}
		for _, c := range strings.Split(cls, ",") {
			if Class(c) == Structure || Class(c) == Data { // private is never read, whatever the row says
				st.classes[Class(c)] = true
			}
		}
		for _, x := range strings.Split(src, ",") {
			if x != "" {
				st.sources[x] = true
			}
		}
		return nil
	})
	return
}

// pullLink: one real pull of one link. As a push it holds the upstream's shared lock and the link's lock: a push, a pull, an unlink or a
// removal of the upstream never run over each other (the one that comes second is refused and says so).
func (s *Store) pullLink(ctx context.Context, tr UpstreamTransport, l UpstreamLink, label string, sleep func(time.Duration) bool) (UpstreamPull, error) {
	upMu := s.upstreamLock(l.Upstream)
	if !upMu.TryRLock() {
		return UpstreamPull{}, errPushRunning{}
	}
	defer upMu.RUnlock()
	mu := s.linkLock(l.Project, l.Upstream)
	if !mu.TryLock() {
		return UpstreamPull{}, errPushRunning{}
	}
	defer mu.Unlock()
	res, err := s.pullRun(ctx, tr, l, label, sleep)
	if len(res.Lines) > maxReportLines {
		res.Lines = append(res.Lines[:maxReportLines], fmt.Sprintf("... and %d more", len(res.Lines)-maxReportLines))
	}
	return res, err
}

type mirrorRow struct {
	source      int64
	label       string
	note, rev   int64
	class       Class
	kind        string
	title, body string
	updated     time.Time
	bytes       int
}

func (s *Store) pullRun(ctx context.Context, tr UpstreamTransport, l UpstreamLink, label string, sleep func(time.Duration) bool) (res UpstreamPull, err error) {
	_, isTeam := SegmentTeam(l.Project)
	res = UpstreamPull{Label: label, Upstream: l.Upstream, Remote: l.Remote, Project: l.Project, Team: isTeam}
	wctx := context.WithoutCancel(ctx) // what was fetched is written even when the time budget of the call is used up meanwhile
	st, err := s.pullSettings(ctx, l)
	if err != nil {
		return res, err
	}
	if !st.on {
		res.Stopped = "reading is off for this link: minigw gw upstream link <project> <upstream> --read --accept-reach"
		return res, nil
	}
	rctx, cancel := context.WithTimeout(ctx, pullRunTimeout)
	defer cancel()
	waitFor := func(d time.Duration) bool { // a wait for the upstream's rate limit never outlives the run (or the caller)
		if dl, ok := rctx.Deadline(); ok && time.Until(dl) < d+time.Second || rctx.Err() != nil {
			return false
		}
		return sleep(d)
	}
	p := &pusher{s: s, tr: tr, up: l.Upstream, sleep: waitFor, ctx: rctx}
	get := func(path string) upAnswer { // one request, with a time of its own: a slow upstream holds nothing for long
		c, stop := context.WithTimeout(rctx, pullCallTimeout)
		defer stop()
		p.ctx = c
		defer func() { p.ctx = rctx }()
		return p.call("GET", path, nil, nil)
	}
	waiting := func() UpstreamPull {
		res.More, res.Wait = true, int(p.wait/time.Second)
		res.Stopped = "the upstream's read limit: pull again in " + strconv.Itoa(res.Wait) + " seconds"
		res.Next = st.cursor
		return res
	}
	// finish writes the outcome: what was fetched (when mirror), the deletions, the cursor and the status of the link, in one transaction.
	// No fetched row, no deletion and no cursor survives a link that was changed, removed or turned off meanwhile.
	record := func(why string) (UpstreamPull, error) { // a failure that leaves the mirror as it is
		res.Stopped = why
		e := s.write(wctx, func(tx *sql.Tx) error {
			_, e := tx.Exec(`UPDATE upstream_links SET last_pull=?,pull_error=? WHERE project_id=? AND upstream=? AND read_accepted=?`, ms(s.now()), why, l.Project, l.Upstream, st.accepted)
			return e
		})
		return res, e
	}
	purge := func(why string) (UpstreamPull, error) { // the upstream withdrew the right: fail closed
		res.Stopped, res.Purged = why, true
		e := s.write(wctx, func(tx *sql.Tx) error {
			n, e := purgeMirror(tx, l.Project, l.Upstream)
			if e != nil {
				return e
			}
			r, e := tx.Exec(`UPDATE upstream_links SET pull_cursor=0,last_pull=?,last_pull_ok=0,pull_error=? WHERE project_id=? AND upstream=? AND read_accepted=?`, ms(s.now()), why, l.Project, l.Upstream, st.accepted)
			if e != nil {
				return e
			}
			if k, _ := r.RowsAffected(); k == 0 {
				return nil
			}
			return Audit(tx, "gateway", "upstream.mirror-purge", l.Upstream, map[string]any{"project": label, "notes": n, "why": upText(why, 200)})
		})
		return res, e
	}
	withdrawn := func(status int) bool { return status == 401 || status == 403 || status == 404 }

	remote, why := p.resolve(l)
	switch {
	case p.more:
		return waiting(), nil
	case why != "" && withdrawn(p.status):
		return purge(why)
	case why != "":
		return record(why)
	}
	a := get("/minigw/v1/projects/" + strconv.FormatInt(remote, 10) + "/kb/manifest")
	switch {
	case p.more:
		return waiting(), nil
	case a.status == 401 || a.status == 403:
		return purge(refusal(a))
	case a.status == 404 || a.status == 501:
		return record("the upstream does not support reading (or the project is gone there): nothing was stored")
	case a.status != 200:
		return record(refusal(a))
	}
	m, why := parseManifest(a.data)
	if why != "" {
		return record(why)
	}
	// what this link takes of it: the linked project itself and the sources the owner named, in the classes the owner accepted
	labels := map[int64]string{}
	take := map[int64]bool{}
	for _, sc := range m.Sources {
		labels[sc.ID] = sc.Label
		take[sc.ID] = sc.ID == remote || st.sources[sc.Label]
	}
	want := map[int64]ManifestNote{}
	for _, n := range m.Notes {
		if take[n.Source] && st.classes[n.Class] {
			want[n.ID] = n
		}
	}
	res.Total = len(want)
	res.Truncated = m.Truncated
	have := map[int64]ManifestNote{}
	err = s.read(ctx, func(tx *sql.Tx) error {
		rows, e := tx.Query(`SELECT remote_note,source_id,remote_rev,class,kind FROM upstream_mirror WHERE project_id=? AND upstream=?`, l.Project, l.Upstream)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var n ManifestNote
			if e = rows.Scan(&n.ID, &n.Source, &n.Rev, &n.Class, &n.Kind); e != nil {
				return e
			}
			have[n.ID] = n
		}
		return rows.Err()
	})
	if err != nil {
		return res, err
	}
	var skipNote []string
	// The room the mirror has for NEW notes, known before anything is fetched: a full mirror ends the traversal ("mirror full") instead of
	// fetching and discarding notes run after run.
	var total int64
	if err = s.read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT coalesce(sum(bytes),0) FROM upstream_mirror`).Scan(&total)
	}); err != nil {
		return res, err
	}
	room := MaxMirrorNotes - len(have)
	if !m.Truncated {
		for id := range have {
			if _, in := want[id]; !in {
				room++ // deleted in the same write
			}
		}
	}
	if total*mirrorCost >= MaxMirrorBytes {
		room = 0
	}
	var todo []int64
	notFetched := 0
	for id, n := range want {
		if id <= st.cursor || have[id] == n {
			continue
		}
		if _, known := have[id]; !known && room <= 0 {
			notFetched++
			continue
		}
		todo = append(todo, id)
	}
	sort.Slice(todo, func(i, j int) bool { return todo[i] < todo[j] })
	// new notes beyond the room are cut from the end of the list (the list is in id order: the ones that come last are not fetched)
	if room < len(todo) {
		kept := todo[:0]
		for _, id := range todo {
			if _, known := have[id]; known {
				kept = append(kept, id)
			} else if room > 0 {
				kept = append(kept, id)
				room--
			} else {
				notFetched++
			}
		}
		todo = kept
	}
	if notFetched > 0 {
		res.Skipped += notFetched
		skipNote = append(skipNote, fmt.Sprintf("%d new notes were not fetched: mirror full (%d notes per link, %d MiB of disk per gateway)", notFetched, MaxMirrorNotes, MaxMirrorBytes>>20))
	}

	var fetched []mirrorRow
	skip := func(id int64, why string) {
		res.Skipped++
		skipNote = append(skipNote, "skip "+strconv.FormatInt(id, 10)+": "+why)
	}
	last, gets, fails := st.cursor, 0, 0
	early := false    // stopped by failures, with notes left: the next run goes on above the cursor
	var failed string // a transient failure: the run is not "ok" (and the mirror is not claimed to be current)
	more := false
fetch:
	for _, id := range todo {
		if rctx.Err() != nil || gets >= maxPullFetch { // the budget of this call (or the note limit of a run) is used: the next run goes on
			more = true
			break
		}
		gets++
		a := get(fmt.Sprintf("/minigw/v1/projects/%d/notes/%d", remote, id))
		switch {
		case p.more:
			more = true
			break fetch
		case a.status == 401 || a.status == 403:
			return purge(refusal(a))
		case a.status == 0 && p.big:
			skip(id, "the answer is over the size limit")
		case a.status == 0 && rctx.Err() != nil:
			more = true
			break fetch
		case a.status == 0 || a.status >= 500:
			fails++
			failed = refusal(a)
			skip(id, failed)
			if fails >= maxPullFails {
				last, early = id, true
				break fetch
			}
		case a.status == 200:
			row, why := checkMirrorNote(a.data, want[id], labels[want[id].Source])
			if why != "" {
				skip(id, why)
			} else {
				fetched = append(fetched, row)
			}
		default: // 404 (gone since the manifest), a redirect, any other refusal of this one note: it is not taken
			skip(id, "not served ("+strconv.Itoa(a.status)+")")
		}
		last = id
	}
	if failed != "" {
		more = false
	}
	cursor := int64(0)
	if more || early {
		cursor = last
	}
	if p.more {
		res.More, res.Wait = true, int(p.wait/time.Second)
	} else {
		res.More = more
	}
	res.Next = cursor
	if res.More && res.Wait == 0 && cursor == st.cursor && len(fetched) == 0 && res.Skipped == 0 { // no progress: said, not hidden
		res.Stopped = "no progress in this run (the time budget of the call is used): pull again"
	}
	ok := !res.More && failed == ""
	pullErr := ""
	if failed != "" {
		pullErr = "some notes could not be fetched: " + failed
		res.Stopped = pullErr
	}

	err = s.write(wctx, func(tx *sql.Tx) error {
		var on int
		var acc int64
		var reach string
		switch e := tx.QueryRow(`SELECT read,read_accepted,read_reach FROM upstream_links WHERE project_id=? AND upstream=?`, l.Project, l.Upstream).Scan(&on, &acc, &reach); {
		case e == sql.ErrNoRows || e == nil && (on == 0 || acc != st.accepted || reach != st.reach): // the link was changed: what was fetched belongs to another reach
			res.Stopped, res.More, res.Next, fetched = "the link was removed or its reading changed during the pull: nothing was stored", false, 0, nil
			return nil
		case e != nil:
			return e
		}
		// A complete manifest: what it does not list is gone. A cut manifest proves nothing about the notes it leaves out, but what this
		// link no longer takes (a class or a source the owner dropped, a note the upstream moved to a class that is not read) is a local
		// decision and goes either way.
		listed := make(map[int64]bool, len(m.Notes))
		for _, n := range m.Notes {
			listed[n.ID] = true
		}
		var gone []int64
		rows, e := tx.Query(`SELECT remote_note,source_id,class FROM upstream_mirror WHERE project_id=? AND upstream=?`, l.Project, l.Upstream)
		if e != nil {
			return e
		}
		for rows.Next() {
			var id, src int64
			var cl string
			if e = rows.Scan(&id, &src, &cl); e != nil {
				rows.Close()
				return e
			}
			_, in := want[id]
			if !m.Truncated && !in || m.Truncated && (listed[id] && !in || !st.classes[Class(cl)] || !take[src]) {
				gone = append(gone, id)
			}
		}
		rows.Close()
		for _, id := range gone {
			if e := deleteMirrored(tx, l.Project, l.Upstream, id); e != nil {
				return e
			}
		}
		res.Removed = len(gone)
		var count int
		var total int64
		if e := tx.QueryRow(`SELECT count(*) FROM upstream_mirror WHERE project_id=? AND upstream=?`, l.Project, l.Upstream).Scan(&count); e != nil {
			return e
		}
		if e := tx.QueryRow(`SELECT coalesce(sum(bytes),0) FROM upstream_mirror`).Scan(&total); e != nil {
			return e
		}
		now := ms(s.now())
		for _, r := range fetched {
			var oldID, oldBytes int64
			switch e := tx.QueryRow(`SELECT id,bytes FROM upstream_mirror WHERE project_id=? AND upstream=? AND remote_note=?`, l.Project, l.Upstream, r.note).Scan(&oldID, &oldBytes); e {
			case nil:
			case sql.ErrNoRows:
				oldID = 0
			default:
				return e
			}
			add := 0
			if oldID == 0 {
				add = 1
			}
			if count+add > MaxMirrorNotes || (total-oldBytes+int64(r.bytes))*mirrorCost > MaxMirrorBytes {
				skip(r.note, "mirror full")
				continue
			}
			upd := ms(r.updated)
			if upd <= 0 || upd > now+24*3600*1000 {
				upd = now
			}
			if _, e := tx.Exec(`INSERT INTO upstream_mirror(project_id,upstream,source_id,source_label,remote_note,remote_rev,class,kind,title,body,bytes,updated,fetched) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(project_id,upstream,remote_note) DO UPDATE SET source_id=excluded.source_id,source_label=excluded.source_label,remote_rev=excluded.remote_rev,class=excluded.class,kind=excluded.kind,title=excluded.title,body=excluded.body,bytes=excluded.bytes,updated=excluded.updated,fetched=excluded.fetched`,
				l.Project, l.Upstream, r.source, r.label, r.note, r.rev, string(r.class), r.kind, r.title, r.body, r.bytes, upd, now); e != nil {
				return e
			}
			var id int64
			if e := tx.QueryRow(`SELECT id FROM upstream_mirror WHERE project_id=? AND upstream=? AND remote_note=?`, l.Project, l.Upstream, r.note).Scan(&id); e != nil {
				return e
			}
			if _, e := tx.Exec(`DELETE FROM upstream_mirror_search WHERE rowid=?`, id); e != nil {
				return e
			}
			if _, e := tx.Exec(`INSERT INTO upstream_mirror_search(rowid,title,body) VALUES(?,?,?)`, id, r.title, r.body); e != nil {
				return e
			}
			if oldID == 0 {
				res.Added++
			} else {
				res.Updated++
			}
			count, total = count+add, total-oldBytes+int64(r.bytes)
		}
		res.Mirror = count
		okAt := "last_pull_ok"
		if ok {
			okAt = "?"
		}
		args := []any{now, cursor, pullErr}
		if ok {
			args = append(args, now)
		}
		args = append(args, l.Project, l.Upstream)
		if _, e := tx.Exec(`UPDATE upstream_links SET last_pull=?,pull_cursor=?,pull_error=?,last_pull_ok=`+okAt+` WHERE project_id=? AND upstream=?`, args...); e != nil {
			return e
		}
		return Audit(tx, adminActor, "upstream.pull", l.Upstream, map[string]any{"project": label, "added": res.Added, "updated": res.Updated, "removed": res.Removed, "skipped": res.Skipped, "more": res.More, "failed": failed != ""})
	})
	if err != nil {
		return res, err
	}
	res.Lines = append(res.Lines, fmt.Sprintf("%d of %d notes offered are read here (%d new or changed to fetch)", len(want), len(m.Notes), len(todo)))
	if m.Truncated {
		res.Lines = append(res.Lines, fmt.Sprintf("the upstream's list stops at %d notes: more exist, and nothing is deleted from the mirror", MaxManifest))
	}
	res.Lines = append(res.Lines, skipNote...)
	return res, nil
}

func deleteMirrored(tx *sql.Tx, project int64, upstream string, note int64) error {
	var id int64
	switch e := tx.QueryRow(`SELECT id FROM upstream_mirror WHERE project_id=? AND upstream=? AND remote_note=?`, project, upstream, note).Scan(&id); e {
	case nil:
	case sql.ErrNoRows:
		return nil
	default:
		return e
	}
	if _, e := tx.Exec(`DELETE FROM upstream_mirror_search WHERE rowid=?`, id); e != nil {
		return e
	}
	_, e := tx.Exec(`DELETE FROM upstream_mirror WHERE id=?`, id)
	return e
}

// checkMirrorNote reads the upstream's answer for one note as DATA: one JSON object, the same id, revision, class and kind as the manifest
// it was listed in (a different one is not the note the manifest promised), valid UTF-8 (otherwise the note is skipped, never repaired), within the sizes a local note may have; terminal
// and format controls are removed from title and body (newline and tab stay). Anything else rejects this one note.
func checkMirrorNote(data []byte, want ManifestNote, label string) (mirrorRow, string) {
	var n struct {
		ID        int64   `json:"id"`
		Class     Class   `json:"class"`
		Title     string  `json:"title"`
		Body      *string `json:"body"`
		Updated   string  `json:"updated"`
		Revision  int64   `json:"revision"`
		EntryKind string  `json:"entryKind"`
	}
	if !utf8.Valid(data) {
		return mirrorRow{}, "text that is not UTF-8"
	}
	d := json.NewDecoder(bytes.NewReader(data))
	if d.Decode(&n) != nil {
		return mirrorRow{}, "not the expected JSON"
	}
	if _, err := d.Token(); err != io.EOF {
		return mirrorRow{}, "data after the JSON object"
	}
	body := ""
	if n.Body != nil {
		body = *n.Body
	}
	kind := n.EntryKind
	if kind == "" {
		kind = KindFinding
	}
	switch {
	case n.ID != want.ID || n.Revision != want.Rev || n.Class != want.Class || kind != want.Kind:
		return mirrorRow{}, "not the note, revision, class or kind the manifest listed"
	case (strings.ContainsRune(n.Title, utf8.RuneError) || strings.ContainsRune(body, utf8.RuneError)) && !bytes.Contains(bytes.ToLower(data), []byte(`\ufffd`)) && !bytes.Contains(data, []byte("\xef\xbf\xbd")):
		return mirrorRow{}, "text that is not UTF-8" // a lone surrogate escape: the decoder would have repaired it to U+FFFD
	case len(n.Title) > maxTitle || len(body) > maxBody:
		return mirrorRow{}, "too large"
	case kind == KindFAQ && (len(n.Title) > maxFAQTitle || len(body) > maxFAQBody):
		return mirrorRow{}, "too large for a FAQ"
	}
	title, text := upText(n.Title, maxTitle), mirrorBody(body)
	if title == "" {
		return mirrorRow{}, "no title"
	}
	t, _ := time.Parse(time.RFC3339, n.Updated) // a date that is not one is "now" (see the write)
	return mirrorRow{source: want.Source, label: label, note: want.ID, rev: want.Rev, class: want.Class, kind: kind, title: title, body: text, updated: t, bytes: len(title) + len(text)}, ""
}

// mirrorBody: what a person or an agent may read of a pulled body: newline and tab, and nothing invisible (invisibleRune).
func mirrorBody(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if invisibleRune(r) {
			return -1
		}
		return r
	}, s)
}
