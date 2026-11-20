package core

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// ReconcileSpec carries the owner's decision. Listed is the last displayed set,
// never an implicit selection of whatever happens to exist when --all arrives.
type ReconcileSpec struct {
	Project  int64   `json:"project"`
	Team     int64   `json:"team"`
	Upstream string  `json:"upstream"`
	DryRun   bool    `json:"dryRun"`
	Note     int64   `json:"note"`
	Confirm  int64   `json:"confirm"`
	New      bool    `json:"new"`
	Skip     bool    `json:"skip"`
	All      bool    `json:"all"`
	Listed   []int64 `json:"listed"`
}

func (r ReconcileSpec) Valid() bool {
	n := 0
	if r.New {
		n++
	}
	if r.Skip {
		n++
	}
	if r.Confirm > 0 {
		n++
	}
	if r.Project < 0 || r.Team < 0 || (r.Project > 0) == (r.Team > 0) || r.Note < 0 || r.Confirm < 0 || r.Upstream != "" && !ValidUpstreamName(r.Upstream) {
		return false
	}
	if n == 0 {
		return r.Note == 0 && !r.All && len(r.Listed) == 0
	}
	return n == 1 && !r.DryRun && (r.Note > 0 && !r.All && len(r.Listed) == 0 || r.All && r.Note == 0 && r.Confirm == 0 && len(r.Listed) > 0)
}

// ReconcileRow is what the owner decides on. A candidate carries what the upstream said of the remote note (the decision is refused when
// the remote revision or content changed since the listing); an unknown with Matches lists trailer matches that are NOT attested.
type ReconcileRow struct {
	Note      int64  `json:"note"`
	Revision  int64  `json:"revision"`
	Class     string `json:"class"`
	Remote    int64  `json:"remote,omitempty"`
	RemoteRev int64  `json:"remoteRev,omitempty"`
	Fp        string `json:"fp,omitempty"`
	Title     string `json:"title,omitempty"`
	Writer    string `json:"writer,omitempty"`
	At        string `json:"at,omitempty"`
	Equal     bool   `json:"equal,omitempty"`
	Matches   string `json:"matches,omitempty"`
	Private   bool   `json:"private,omitempty"` // a private note: never visible on the upstream, so it cannot be compared
	// A trailer match is SHOWN with its proof, not gated by it: Proven = the upstream reports this gateway's device as the writer of the
	// current revision. Info is the text the owner saw; MatchFp binds every match (id:revision:fingerprint:proven) to the version listed.
	Proven  bool   `json:"proven,omitempty"`
	Info    string `json:"info,omitempty"`
	MatchFp string `json:"matchFp,omitempty"`
}
type UpstreamReconcile struct {
	Lines      []string       `json:"lines"`
	Notes      []ReconcileRow `json:"notes"`
	Unknowns   []int64        `json:"unknowns"`
	Candidates int            `json:"candidates"`
	Unknown    int            `json:"unknown"`
	Unchecked  int            `json:"unchecked"`
	Cleared    bool           `json:"cleared"`
}
type reconcileSnapshot struct {
	Address string
	Mark    string
	Link    UpstreamLink
	Notes   []ReconcileRow
	// Digest: what the manifest said of the destination when this listing was made (a decision reads the manifest again and needs it equal).
	Digest   string
	LocalKey string
	Orphans  []int64
}
type reconcileDecision struct {
	Mark, Hash, Action string
	Revision           int64
}

type ReconcileCounts struct {
	Candidates int `json:"candidates"`
	Unknown    int `json:"unknown"`
}

func reconcileCounts(tx *sql.Tx, l UpstreamLink) *ReconcileCounts {
	var value, mark string
	var snap reconcileSnapshot
	if tx.QueryRow(`SELECT value FROM meta WHERE key=?`, reconcileKey(l.Project, l.Upstream, "listing")).Scan(&value) != nil || json.Unmarshal([]byte(value), &snap) != nil ||
		tx.QueryRow(`SELECT value FROM meta WHERE key=?`, restoredKey(l.Project, l.Upstream)).Scan(&mark) != nil || mark != snap.Mark || !reconcileSameLink(l, snap.Link) {
		return nil
	}
	counts := &ReconcileCounts{}
	for _, row := range snap.Notes {
		if row.Class == "candidate" {
			counts.Candidates++
		}
		if row.Class == "unknown" || row.Class == "unchecked" { // an unchecked private note blocks the mark like an unknown
			counts.Unknown++
		}
	}
	return counts
}

func (s *Store) reconcileCountsText(ctx context.Context, l UpstreamLink) string {
	var counts *ReconcileCounts
	_ = s.read(ctx, func(tx *sql.Tx) error { counts = reconcileCounts(tx, l); return nil })
	if counts == nil {
		return ""
	}
	return fmt.Sprintf("; last listing: candidates %d, unknown %d", counts.Candidates, counts.Unknown)
}

func reconcileKey(project int64, up, suffix string) string {
	return "upstream_reconcile|" + strconv.FormatInt(project, 10) + "|" + up + "|" + suffix
}
func reconcileDecisionKey(project int64, up string, note int64) string {
	return reconcileKey(project, up, "decision|"+strconv.FormatInt(note, 10))
}
func reconcileMeta(tx *sql.Tx, key string, value any) error {
	b, e := json.Marshal(value)
	if e != nil {
		return e
	}
	_, e = tx.Exec(`INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, string(b))
	return e
}

// ReconcileListedUnknowns returns only the ids the last normal listing displayed.
// It is not a fresh classification and cannot itself authorize or clear anything.
func (s *Store) ReconcileListedUnknowns(ctx context.Context, r ReconcileSpec) ([]int64, error) {
	if !r.Valid() || r.Note != 0 || r.All || r.DryRun {
		return nil, ErrRefusal{"reconcile: invalid listing request"}
	}
	links, e := s.UpstreamLinks(ctx, r.Project, r.Team)
	if e != nil {
		return nil, e
	}
	var chosen []UpstreamLinkInfo
	for _, l := range links {
		if l.Restored != nil && (r.Upstream == "" || r.Upstream == l.Upstream) {
			chosen = append(chosen, l)
		}
	}
	if len(chosen) != 1 {
		return nil, ErrRefusal{"reconcile: name exactly one restored link with --upstream <name>"}
	}
	l := chosen[0].UpstreamLink
	ids := []int64{}
	e = s.read(ctx, func(tx *sql.Tx) error {
		var raw, mark string
		var snap reconcileSnapshot
		if e := tx.QueryRow(`SELECT value FROM meta WHERE key=?`, reconcileKey(l.Project, l.Upstream, "listing")).Scan(&raw); e != nil {
			return ErrRefusal{"reconcile: list this link before deciding"}
		}
		if json.Unmarshal([]byte(raw), &snap) != nil {
			return ErrRefusal{"reconcile: list this link before deciding"}
		}
		if e := tx.QueryRow(`SELECT value FROM meta WHERE key=?`, restoredKey(l.Project, l.Upstream)).Scan(&mark); e != nil {
			return e
		}
		if mark != snap.Mark || !reconcileSameLink(l, snap.Link) {
			return ErrRefusal{"reconcile: link changed; list again"}
		}
		for _, row := range snap.Notes {
			if row.Class == "unknown" {
				ids = append(ids, row.Note)
			}
		}
		if len(ids) == 0 {
			return ErrRefusal{"reconcile: the last listing has no undecided unknowns"}
		}
		return nil
	})
	return ids, e
}

// reconcileRemote is one destination note as the fresh read saw it, with what the upstream itself says about its writer:
// Device is the device that wrote the current revision (provenance). It is SHOWN to the owner ("written by this gateway's device" or "no
// proof"), and it decides how a binding is recorded (see the confirmation), but it is not a gate: a note whose last writer was a reviewer,
// or another device of this gateway (a restore onto a new computer makes a new device key), must still be bindable.
type reconcileRemote struct {
	upNote
	Device, Writer, At string
}

// ownDevice is optionally implemented by the host transport: this gateway's own device id on the upstream (a public key).
type ownDevice interface{ OwnDevice(upstream string) string }

func reconcileFingerprint(u reconcileRemote) string {
	h := sha256.Sum256([]byte(string(u.Class) + "\x00" + u.EntryKind + "\x00" + u.Title + "\x00" + u.Body))
	return hex.EncodeToString(h[:8])
}

// Time and rate of the read (variables so that tests can use small ones). Every request has a deadline of its own; the whole read has a
// budget that grows with the number of notes (capped). A deadline is never reported as the upstream being down.
var (
	reconcileCallTimeout = 30 * time.Second
	reconcileBaseBudget  = 2 * time.Minute
	reconcilePerNote     = 3 * time.Second
	reconcileMaxBudget   = 15 * time.Minute
	// reconcileWait waits out the upstream's read limit (false: the context ended first).
	reconcileWait = func(ctx context.Context, d time.Duration) bool {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-t.C:
			return true
		case <-ctx.Done():
			return false
		}
	}
)

type reconcileSession struct {
	p      *pusher
	l      UpstreamLink
	rctx   context.Context
	begin  time.Time
	end    time.Time
	waited time.Duration
	read   int
	total  int
	remote int64
}

func (c *reconcileSession) progress() string {
	if c.total == 0 {
		return "before the first note"
	}
	return fmt.Sprintf("read %d of %d notes", c.read, c.total)
}

func (c *reconcileSession) budgetErr() error {
	return ErrRefusal{fmt.Sprintf("reconcile: the time budget of the read is used (%s); nothing was changed and the upstream is not reported down: run it again", c.progress())}
}

// guard turns what happened to a request into the owner's words: the read limit (wait failed), the caller's own cancel, a request or budget deadline.
func (c *reconcileSession) guard(cctx context.Context, status int) error {
	switch {
	case c.p.more:
		return ErrRefusal{fmt.Sprintf("reconcile: the upstream's read limit is used (%s); nothing was changed: run it again in %d seconds", c.progress(), int(c.p.wait/time.Second)+1)}
	case c.rctx.Err() != nil && c.rctx.Err() != context.DeadlineExceeded:
		return ErrRefusal{"reconcile: canceled; nothing was changed"}
	case status == 0 && cctx.Err() != nil:
		if !time.Now().Before(c.end) || c.rctx.Err() != nil {
			return c.budgetErr()
		}
		return ErrRefusal{fmt.Sprintf("reconcile: a request took longer than %s (%s); nothing was changed and the upstream is not reported down: run it again", reconcileCallTimeout, c.progress())}
	}
	return nil
}

func (c *reconcileSession) get(path string) (upAnswer, error) {
	now := time.Now()
	if !now.Before(c.end) {
		return upAnswer{}, c.budgetErr()
	}
	dl := now.Add(reconcileCallTimeout)
	if dl.After(c.end) {
		dl = c.end
	}
	cctx, stop := context.WithDeadline(c.rctx, dl)
	defer stop()
	c.p.ctx, c.p.more = cctx, false
	defer func() { c.p.ctx = c.rctx }()
	a := c.p.call("GET", path, nil, nil)
	return a, c.guard(cctx, a.status)
}

func (c *reconcileSession) resolve() (int64, error) {
	cctx, stop := context.WithTimeout(c.rctx, 2*reconcileCallTimeout)
	defer stop()
	c.p.ctx, c.p.more = cctx, false
	defer func() { c.p.ctx = c.rctx }()
	remote, why := c.p.resolve(c.l)
	if e := c.guard(cctx, c.p.status); e != nil {
		return 0, e
	}
	if why != "" {
		return 0, ErrRefusal{why}
	}
	c.remote = remote
	return remote, nil
}

// manifest reads the destination's manifest and refuses a view that may be partial: the upstream itself must say (source "full") that this
// device reads every pushed non-private class of the destination in full. Private notes are never in a manifest: their local notes are
// "unchecked" rows, never matched.
func (c *reconcileSession) manifest() (*Manifest, error) {
	a, e := c.get(fmt.Sprintf("/minigw/v1/projects/%d/kb/manifest", c.remote))
	if e != nil {
		return nil, e
	}
	if a.status != 200 {
		return nil, ErrRefusal{refusal(a)}
	}
	m, why := parseManifest(a.data)
	if why != "" {
		return nil, ErrRefusal{why}
	}
	if m.Truncated {
		return nil, ErrRefusal{"reconcile: incomplete upstream manifest"}
	}
	for _, src := range m.Sources {
		if src.ID != c.remote {
			continue
		}
		if src.Full == nil {
			return nil, ErrRefusal{"reconcile: this upstream does not say which notes the device reads in full: update the upstream gateway to 0.9.3 or newer"}
		}
		var missing []string
		for _, cl := range c.l.Classes {
			if cl == Private {
				continue
			}
			seen := false
			for _, f := range src.Full {
				seen = seen || f == cl
			}
			if !seen {
				missing = append(missing, string(cl))
			}
		}
		if len(missing) > 0 {
			return nil, ErrRefusal{"reconcile: the upstream lets this device read only part of the destination (not in full: " + strings.Join(missing, ", ") + "): a role there that reads all " + strings.Join(missing, " and ") + " notes is needed"}
		}
		n := 0
		for _, x := range m.Notes {
			if x.Source == c.remote {
				n++
			}
		}
		c.total = n
		if b := reconcileBaseBudget + time.Duration(n)*reconcilePerNote; b < reconcileMaxBudget {
			c.end = c.begin.Add(b)
		} else {
			c.end = c.begin.Add(reconcileMaxBudget)
		}
		return m, nil
	}
	return nil, ErrRefusal{"reconcile: destination is not readable"}
}

// note reads one destination note as the manifest listed it.
func (c *reconcileSession) note(want ManifestNote) (reconcileRemote, error) {
	a, e := c.get(fmt.Sprintf("/minigw/v1/projects/%d/notes/%d", c.remote, want.ID))
	if e != nil {
		return reconcileRemote{}, e
	}
	if a.status != 200 {
		return reconcileRemote{}, ErrRefusal{refusal(a)}
	}
	checked, why := checkMirrorNote(a.data, want, "")
	if why != "" {
		return reconcileRemote{}, ErrRefusal{"reconcile: " + why + "; list again"}
	}
	var raw struct {
		Body       *string
		Title      string
		Author     string
		Editor     string
		Updated    string
		Provenance *struct{ Device string }
	}
	if json.Unmarshal(a.data, &raw) != nil || raw.Body == nil || *raw.Body != checked.body || raw.Title != checked.title {
		return reconcileRemote{}, ErrRefusal{"reconcile: upstream text requires sanitization or is incomplete"}
	}
	r := reconcileRemote{upNote: upNote{Revision: want.Rev, Class: want.Class, Title: raw.Title, Body: *raw.Body, EntryKind: want.Kind}, Writer: upText(raw.Author, 40), At: upText(raw.Updated, 40)}
	if raw.Editor != "" {
		r.Writer = upText(raw.Editor, 40)
	}
	if raw.Provenance != nil && raw.Provenance.Device != "UNKNOWN" {
		r.Device = raw.Provenance.Device
	}
	c.read++
	return r, nil
}

// reconcileDigest: what a stored listing depends on in the manifest: the destination's notes (id, revision, class, kind) and what is read in full.
func reconcileDigest(m *Manifest, remote int64) string {
	h := sha256.New()
	for _, src := range m.Sources {
		if src.ID == remote {
			fmt.Fprintf(h, "full:%v;", src.Full)
		}
	}
	for _, n := range m.Notes {
		if n.Source == remote {
			fmt.Fprintf(h, "%d|%d|%s|%s;", n.ID, n.Rev, n.Class, n.Kind)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// fullRead is a listing's read: every destination note, between two equal manifests.
func (c *reconcileSession) fullRead() (map[int64]reconcileRemote, []int64, string, error) {
	if _, e := c.resolve(); e != nil {
		return nil, nil, "", e
	}
	m, e := c.manifest()
	if e != nil {
		return nil, nil, "", e
	}
	fresh := map[int64]reconcileRemote{}
	var ids []int64
	totalBytes := 0
	for _, want := range m.Notes {
		if want.Source != c.remote {
			continue
		}
		u, e := c.note(want)
		if e != nil {
			return nil, nil, "", e
		}
		totalBytes += len(u.Title) + len(u.Body)
		if totalBytes > MaxMirrorBytes/mirrorCost {
			return nil, nil, "", ErrRefusal{"reconcile: destination exceeds the complete-read size limit"}
		}
		fresh[want.ID] = u
		ids = append(ids, want.ID)
	}
	again, e := c.manifest()
	if e != nil {
		return nil, nil, "", e
	}
	if !reflect.DeepEqual(m, again) {
		return nil, nil, "", ErrRefusal{"reconcile: upstream changed during the read; list again"}
	}
	return fresh, ids, reconcileDigest(m, c.remote), nil
}

// decisionRead serves a named decision: the manifest (it must equal the one the stored listing was made from: ids, revisions, classes,
// kinds, what is read in full) and, for a confirmation, the one note it binds. Nothing else is read: a project with many notes is listed once.
func (c *reconcileSession) decisionRead(digest string, confirm int64) (map[int64]reconcileRemote, error) {
	if _, e := c.resolve(); e != nil {
		return nil, e
	}
	m, e := c.manifest()
	if e != nil {
		return nil, e
	}
	if reconcileDigest(m, c.remote) != digest {
		return nil, ErrRefusal{"reconcile: the upstream's notes changed since the listing; list again"}
	}
	fresh := map[int64]reconcileRemote{}
	if confirm > 0 {
		for _, want := range m.Notes {
			if want.Source == c.remote && want.ID == confirm {
				u, e := c.note(want)
				if e != nil {
					return nil, e
				}
				fresh[confirm] = u
			}
		}
		if _, ok := fresh[confirm]; !ok {
			return nil, ErrRefusal{"reconcile: the remote note is not on the upstream any more; list again"}
		}
	}
	return fresh, nil
}

// reconcileMatchText is what the owner sees of one trailer match: what the upstream says of it and whether this gateway's device wrote it.
func reconcileMatchText(id int64, u reconcileRemote, equal, proven bool) string {
	eq := "differs from the local note"
	if equal {
		eq = "equal to the local note"
	}
	who := "no proof: last written by " + u.Writer
	if proven {
		who = "written by this gateway's device"
	}
	return fmt.Sprintf("remote %d %q, %s at %s, remote revision %d, %s", id, u.Title, who, u.At, u.Revision, eq)
}

func reconcileMatchEntry(id int64, u reconcileRemote, proven bool) string {
	return fmt.Sprintf("%d:%d:%s:%t", id, u.Revision, reconcileFingerprint(u), proven)
}

// reconcileRowMatches: the remote note read now is the very version (revision, content, proof) the row showed the owner.
func reconcileRowMatches(row ReconcileRow, id int64, u reconcileRemote, proven bool) bool {
	if row.Class == "candidate" {
		return row.Remote == id && row.RemoteRev == u.Revision && row.Fp == reconcileFingerprint(u) && row.Proven == proven
	}
	for _, e := range strings.Split(row.MatchFp, ",") {
		if e == reconcileMatchEntry(id, u, proven) {
			return true
		}
	}
	return false
}

func reconcileRowHasMatch(row ReconcileRow, id int64) bool {
	if row.Class == "candidate" {
		return row.Remote == id
	}
	for _, e := range strings.Split(row.MatchFp, ",") {
		if strings.HasPrefix(e, strconv.FormatInt(id, 10)+":") {
			return true
		}
	}
	return false
}

// reconcileHasSkip: the link has a persistent skip decision (it can be listed and released without a restore mark).
func (s *Store) reconcileHasSkip(ctx context.Context, l UpstreamLink) (has bool) {
	prefix := reconcileKey(l.Project, l.Upstream, "decision|")
	_ = s.read(ctx, func(tx *sql.Tx) error {
		return tx.QueryRow(`SELECT count(*)>0 FROM meta WHERE substr(key,1,?)=? AND value LIKE '%"Action":"skip"%'`, len(prefix), prefix).Scan(&has)
	})
	return
}

// reconcileNotesKey: what a decision depends on in the local notes (not the push bookkeeping that an automatic push may change).
func reconcileNotesKey(notes []UpstreamNote) string {
	var b strings.Builder
	for _, n := range notes {
		var rn, rs int64
		if n.Sent != nil {
			rn, rs = n.Sent.RemoteNote, n.Sent.RemoteSub
		}
		fmt.Fprintf(&b, "%d|%d|%s|%t|%t|%d|%d;", n.ID, n.Revision, n.Hash, n.Foreign, n.Sent != nil, rn, rs)
	}
	return b.String()
}

func (s *Store) ReconcileUpstream(ctx context.Context, tr UpstreamTransport, r ReconcileSpec) (out UpstreamReconcile, err error) {
	if !r.Valid() {
		return out, ErrRefusal{"reconcile: invalid request"}
	}
	ls, e := s.UpstreamLinks(ctx, r.Project, r.Team)
	if e != nil {
		return out, e
	}
	var chosen []UpstreamLinkInfo
	for _, l := range ls {
		if (r.Upstream == "" || l.Upstream == r.Upstream) && (l.Restored != nil || s.reconcileHasSkip(ctx, l.UpstreamLink)) {
			chosen = append(chosen, l)
		}
	}
	if len(chosen) != 1 {
		return out, ErrRefusal{"reconcile: name exactly one restored link (or one with skipped notes) with --upstream <name>"}
	}
	l := chosen[0].UpstreamLink
	mu := s.linkLock(l.Project, l.Upstream)
	if !mu.TryLock() {
		return out, errPushRunning{}
	}
	defer mu.Unlock()
	l, _, address, archived, scan, e := s.upstreamPlan(ctx, l.Project, l.Upstream, 0, 0)
	if e != nil {
		return out, e
	}
	if archived {
		return out, ErrRefusal{"reconcile: the local project or team is archived"}
	}
	var key []byte
	var mark string
	var previous reconcileSnapshot
	e = s.read(ctx, func(tx *sql.Tx) error {
		if e := tx.QueryRow(`SELECT value FROM meta WHERE key=?`, restoredKey(l.Project, l.Upstream)).Scan(&mark); e != nil && e != sql.ErrNoRows {
			return e
		}
		if e := tx.QueryRow(`SELECT k FROM upstream_key WHERE id=1`).Scan(&key); e != nil && e != sql.ErrNoRows {
			return e
		}
		var v string
		e := tx.QueryRow(`SELECT value FROM meta WHERE key=?`, reconcileKey(l.Project, l.Upstream, "listing")).Scan(&v)
		if e == nil {
			// A damaged listing is no authority for a decision; a new listing
			// must still be able to replace it.
			if json.Unmarshal([]byte(v), &previous) != nil {
				previous = reconcileSnapshot{}
			}
			return nil
		}
		if e == sql.ErrNoRows {
			return nil
		}
		return e
	})
	if e != nil {
		return out, e
	}
	decision := r.Note > 0 || r.All
	if decision && (previous.Address != address || mark != "" && previous.Mark != mark || !reconcileSameLink(previous.Link, l) || previous.Digest == "" || previous.LocalKey != reconcileNotesKey(scan.Notes)) {
		return out, ErrRefusal{"reconcile: list this link before deciding (the listing is missing, or the link or its notes changed since)"}
	}
	rctx, cancel := context.WithTimeout(ctx, reconcileMaxBudget)
	defer cancel()
	sess := &reconcileSession{l: l, rctx: rctx, begin: time.Now()}
	sess.end = sess.begin.Add(reconcileBaseBudget)
	sess.p = &pusher{s: s, tr: tr, up: l.Upstream, ctx: rctx, sleep: func(d time.Duration) bool {
		if time.Now().Add(d).After(sess.end) || !reconcileWait(rctx, d) {
			return false
		}
		sess.waited += d
		return true
	}}
	own := ""
	if d, ok := tr.(ownDevice); ok {
		own = d.OwnDevice(l.Upstream)
	}
	var fresh map[int64]reconcileRemote
	var orphans []int64
	digest := previous.Digest
	if decision {
		if fresh, e = sess.decisionRead(previous.Digest, r.Confirm); e != nil {
			return out, e
		}
		out.Notes = append([]ReconcileRow(nil), previous.Notes...)
		orphans = previous.Orphans
	} else {
		var ids []int64
		if fresh, ids, digest, e = sess.fullRead(); e != nil {
			return out, e
		}
		used := map[int64]bool{}
		for _, n := range scan.Notes {
			if n.Foreign {
				continue
			}
			row := ReconcileRow{Note: n.ID, Revision: n.Revision, Class: "unknown"}
			if n.Class == Private {
				// A private note is never in the manifest: the upstream cannot show whether it has a copy. A surviving receipt stays as far as it
				// goes (known, not compared); without one the note is unchecked: the owner decides by name.
				row.Private = true
				row.Class = "unchecked"
				if n.Sent != nil {
					row.Class = "known"
				}
				out.Notes = append(out.Notes, row)
				continue
			}
			ref := upstreamRef(key, l.Upstream, l.Project, n.ID)
			var matches []int64
			if len(key) > 0 {
				for _, id := range ids {
					if _, ok := ownTrailerSplit(fresh[id].Body, ref, trailerAuthor(n.Author), math.MaxInt64); ok {
						matches = append(matches, id)
						used[id] = true
					}
				}
			}
			if n.Sent != nil && n.Sent.RemoteNote == 0 && n.Sent.RemoteSub > 0 {
				// Pending submissions are not manifest notes. Keep their waiting receipts.
				row.Class = "known"
			} else if n.Sent != nil {
				for _, id := range matches {
					if id == n.Sent.RemoteNote {
						row.Class, row.Remote = "known", id
					}
				}
			}
			if row.Class != "known" && n.Sent == nil && len(matches) > 0 {
				var texts, entries []string
				for _, id := range matches {
					u := fresh[id]
					body, _ := ownTrailerSplit(u.Body, ref, trailerAuthor(n.Author), math.MaxInt64)
					equal := upstreamHash(Note{Class: u.Class, EntryKind: u.EntryKind, Title: u.Title, Body: body}) == n.Hash
					proven := own != "" && u.Device == own
					texts = append(texts, reconcileMatchText(id, u, equal, proven))
					entries = append(entries, reconcileMatchEntry(id, u, proven))
					if len(matches) == 1 {
						row.Class, row.Remote, row.RemoteRev, row.Fp, row.Proven, row.Equal = "candidate", id, u.Revision, reconcileFingerprint(u), proven, equal
						row.Title, row.Writer, row.At = u.Title, u.Writer, u.At
					}
				}
				row.Info, row.MatchFp = strings.Join(texts, "; "), strings.Join(entries, ",")
				if len(matches) > 1 {
					for i, id := range matches {
						if i > 0 {
							row.Matches += ","
						}
						row.Matches += strconv.FormatInt(id, 10)
					}
				}
			}
			out.Notes = append(out.Notes, row)
		}
		for _, id := range ids {
			if !used[id] && looksFromGateway.MatchString(fresh[id].Body) {
				orphans = append(orphans, id)
			}
		}
	}
	apply := func(tx *sql.Tx) error {
		cur, e := scanLink(tx.QueryRow(`SELECT `+linkCols+` FROM upstream_links WHERE project_id=? AND upstream=?`, l.Project, l.Upstream))
		if e != nil {
			return e
		}
		var addr, stamp string
		if e = tx.QueryRow(`SELECT address FROM upstreams WHERE name=?`, l.Upstream).Scan(&addr); e != nil {
			return e
		}
		if e = tx.QueryRow(`SELECT value FROM meta WHERE key=?`, restoredKey(l.Project, l.Upstream)).Scan(&stamp); e != nil && e != sql.ErrNoRows {
			return e
		}
		now, e := upstreamNotes(tx, cur, 0, 0)
		if e != nil {
			return e
		}
		if addr != address || stamp != mark || !reconcileSameLink(l, cur) || reconcileNotesKey(scan.Notes) != reconcileNotesKey(now.Notes) {
			return ErrRefusal{"reconcile: local notes or link changed; list again"}
		}
		var isArchived bool
		if team, ok := SegmentTeam(l.Project); ok {
			e = tx.QueryRow(`SELECT archived FROM teams WHERE id=?`, team).Scan(&isArchived)
		} else {
			e = tx.QueryRow(`SELECT archived FROM projects WHERE id=?`, l.Project).Scan(&isArchived)
		}
		if e != nil {
			return e
		}
		if isArchived {
			return ErrRefusal{"reconcile: local project or team was archived; list again"}
		}
		if !decision {
			// Stored decisions: a skip is persistent (it holds across local edits and outlives the restore mark, until the owner releases it);
			// a "new" only belongs to the mark and the note version it was made for.
			kept := out.Notes[:0]
			for i := range out.Notes {
				row := out.Notes[i]
				var v string
				var d reconcileDecision
				var n UpstreamNote
				for _, x := range scan.Notes {
					if x.ID == row.Note {
						n = x
					}
				}
				if tx.QueryRow(`SELECT value FROM meta WHERE key=?`, reconcileDecisionKey(l.Project, l.Upstream, row.Note)).Scan(&v) == nil && json.Unmarshal([]byte(v), &d) == nil &&
					(d.Action == "skip" || d.Action == "new" && mark != "" && d.Mark == mark && d.Revision == n.Revision && d.Hash == n.Hash && row.Class != "known") {
					row.Class = d.Action
				}
				if mark == "" && row.Class != "skip" {
					continue // no restore mark: only the skipped notes are of interest
				}
				kept = append(kept, row)
			}
			out.Notes = kept
		}
		targets := map[int64]bool{}
		if decision {
			if r.All {
				for _, id := range r.Listed {
					if id <= 0 || targets[id] {
						return ErrRefusal{"reconcile: invalid listed ids"}
					}
					targets[id] = true
				}
			} else {
				targets[r.Note] = true
			}
			for id := range targets {
				var row *ReconcileRow
				for i := range out.Notes {
					if out.Notes[i].Note == id {
						row = &out.Notes[i]
					}
				}
				if row == nil {
					return ErrRefusal{"reconcile: note was not listed; list again"}
				}
				ok := false
				switch {
				case r.All:
					if row.Class == "unchecked" {
						return ErrRefusal{"reconcile: a private note is unchecked (not visible on the upstream): decide it by name with --note <id> --new or --skip, not with --all"}
					}
					ok = row.Class == "unknown"
				case r.Confirm > 0:
					ok = (row.Class == "candidate" || row.Class == "unknown") && reconcileRowHasMatch(*row, r.Confirm)
				case r.New:
					ok = row.Class == "unknown" || row.Class == "candidate" || row.Class == "skip" || row.Class == "unchecked"
				case r.Skip:
					ok = row.Class == "unknown" || row.Class == "candidate" || row.Class == "unchecked"
				}
				if !ok {
					return ErrRefusal{"reconcile: decision does not match the listed class or remote id"}
				}
				if r.Confirm > 0 {
					u := fresh[r.Confirm]
					// the remote note read now must be the very version (revision, content, proof) the owner saw in the listing
					if !reconcileRowMatches(*row, r.Confirm, u, own != "" && u.Device == own) {
						return ErrRefusal{"reconcile: the remote note changed since the listing; list again"}
					}
				}
			}
		}
		for i := range out.Notes {
			row := &out.Notes[i]
			if !targets[row.Note] {
				continue
			}
			var n UpstreamNote
			for _, v := range scan.Notes {
				if v.ID == row.Note {
					n = v
					break
				}
			}
			action := "new"
			if r.Skip {
				action = "skip"
			}
			if r.Confirm > 0 {
				action = "confirm"
			}
			if r.Confirm > 0 {
				u := fresh[r.Confirm]
				body, _ := ownTrailerSplit(u.Body, upstreamRef(key, l.Upstream, l.Project, n.ID), trailerAuthor(n.Author), math.MaxInt64)
				equal := upstreamHash(Note{Class: u.Class, EntryKind: u.EntryKind, Title: u.Title, Body: body}) == n.Hash
				proven := own != "" && u.Device == own
				// An unknown base on every path but one: a binding the upstream does not attribute to this gateway's device is ALWAYS a conflict with
				// unknown base (equal or not), so the first push of the note goes through `resolve` (--ours: a submission for the upstream's
				// reviewers; --theirs) and never replaces or supersedes directly. Only a device-proven, equal binding keeps a receipt; on a publish link
				// its base stays unknown (0) as well.
				hash, content, rev, conflict, conflictRev := "", "", int64(0), 1, u.Revision
				if proven && equal {
					hash, content, rev, conflict, conflictRev = n.Hash, sentContentHash(u.Class, u.EntryKind, u.Title, body), u.Revision, 0, 0
					if l.Mode == "publish" {
						rev = 0
					}
				}
				_, e = tx.Exec(`INSERT INTO upstream_sent(project_id,upstream,note_id,hash,revision,remote_sub,remote_note,sent,remote_rev,sent_content,conflict,conflict_rev) VALUES(?,?,?,?,?,0,?,?,?,?,?,?)`, l.Project, l.Upstream, n.ID, hash, n.Revision, r.Confirm, ms(s.now()), rev, content, conflict, conflictRev)
				if e != nil {
					return e
				}
				if !proven {
					out.Lines = append(out.Lines, fmt.Sprintf("note %d is bound to remote %d WITHOUT proof that this gateway's device wrote it: it is recorded as a conflict with an unknown base; nothing is replaced directly. Settle it with: gw upstream resolve --ours (a submission for the upstream's reviewers) or --theirs", n.ID, r.Confirm))
				}
				row.Class = "known"
			} else {
				if e = reconcileMeta(tx, reconcileDecisionKey(l.Project, l.Upstream, n.ID), reconcileDecision{mark, n.Hash, action, n.Revision}); e != nil {
					return e
				}
				row.Class = action
				if r.New {
					if _, e = tx.Exec(`DELETE FROM upstream_sent WHERE project_id=? AND upstream=? AND note_id=?`, l.Project, l.Upstream, n.ID); e != nil {
						return e
					}
					if _, e = tx.Exec(`UPDATE upstream_links SET dirty_since=CASE WHEN dirty_since=0 THEN ?1 ELSE dirty_since END,last_change=max(?1,last_change+1) WHERE project_id=?2 AND upstream=?3 AND auto=1`, ms(s.now()), l.Project, l.Upstream); e != nil {
						return e
					}
				}
			}
			if e = Audit(tx, adminActor, "upstream.reconcile."+action, l.Upstream, map[string]any{"project": l.Project, "note": n.ID, "remote": r.Confirm}); e != nil {
				return e
			}
		}
		for _, row := range out.Notes {
			if row.Class == "candidate" {
				out.Candidates++
			}
			if row.Class == "unknown" {
				out.Unknown++
				out.Unknowns = append(out.Unknowns, row.Note)
			}
			if row.Class == "unchecked" {
				out.Unchecked++
			}
		}
		if r.DryRun {
			return nil
		}
		after, e := upstreamNotes(tx, cur, 0, 0)
		if e != nil {
			return e
		}
		if e = reconcileMeta(tx, reconcileKey(l.Project, l.Upstream, "listing"), reconcileSnapshot{Address: address, Mark: mark, Link: l, Notes: out.Notes, Digest: digest, LocalKey: reconcileNotesKey(after.Notes), Orphans: orphans}); e != nil {
			return e
		}
		if mark != "" && out.Candidates == 0 && out.Unknown == 0 && out.Unchecked == 0 {
			if _, e = tx.Exec(`DELETE FROM meta WHERE key=?`, restoredKey(l.Project, l.Upstream)); e != nil {
				return e
			}
			out.Cleared = true
		}
		counts := map[string]int{}
		for _, row := range out.Notes {
			counts[row.Class]++
		}
		counts["orphan"] = len(orphans)
		return Audit(tx, adminActor, "upstream.reconcile", l.Upstream, map[string]any{"project": l.Project, "counts": counts, "candidates": out.Candidates, "unknown": out.Unknown, "unchecked": out.Unchecked, "cleared": out.Cleared})
	}
	if r.DryRun {
		e = s.read(ctx, apply)
	} else {
		e = s.write(ctx, apply)
	}
	if e != nil {
		return UpstreamReconcile{}, e
	}
	lines := out.Lines // lines the decision itself wrote
	out.Lines = nil
	for _, row := range out.Notes {
		switch {
		case row.Class == "candidate":
			out.Lines = append(out.Lines, fmt.Sprintf("candidate note %d (revision %d), %s", row.Note, row.Revision, row.Info))
		case row.Class == "unknown" && row.Info != "":
			out.Lines = append(out.Lines, fmt.Sprintf("unknown note %d (revision %d), several upstream notes carry its trailer (bind one with --note %d --confirm <remote id>): %s", row.Note, row.Revision, row.Note, row.Info))
		case row.Private && row.Class == "known":
			out.Lines = append(out.Lines, fmt.Sprintf("known note %d (revision %d), remote %d; private note: the receipt could not be compared with the upstream", row.Note, row.Revision, row.Remote))
		default:
			out.Lines = append(out.Lines, fmt.Sprintf("%s note %d (revision %d), remote %d", row.Class, row.Note, row.Revision, row.Remote))
		}
	}
	for _, id := range orphans {
		out.Lines = append(out.Lines, fmt.Sprintf("orphan remote %d", id))
	}
	if len(out.Notes) == 0 && !out.Cleared {
		out.Lines = append(out.Lines, "no restore mark: only skipped notes are listed")
	}
	if out.Unchecked > 0 {
		out.Lines = append(out.Lines, fmt.Sprintf("unchecked %d: private notes are not visible on the upstream, so nothing can be compared; each needs its own decision (--note <id> --new may create a second copy on the upstream, or --note <id> --skip); --all does not cover them", out.Unchecked))
	}
	if sess.waited > 0 {
		out.Lines = append(out.Lines, fmt.Sprintf("waited %d seconds in total for the upstream's read limit", int(sess.waited/time.Second)))
	}
	out.Lines = append(out.Lines, lines...)
	out.Lines = append(out.Lines, fmt.Sprintf("candidates %d, unknown %d, restored mark cleared: %t", out.Candidates, out.Unknown, out.Cleared))
	return out, nil
}

// Push bookkeeping may change between a listing and its decision; destination,
// policy and mode must still be the same. The final transaction checks all fields.
func reconcileSameLink(a, b UpstreamLink) bool {
	return a.Project == b.Project && a.Upstream == b.Upstream && a.Remote == b.Remote && a.Team == b.Team && a.WithPrivate == b.WithPrivate && a.Mode == b.Mode && a.Auto == b.Auto && reflect.DeepEqual(a.Classes, b.Classes)
}
