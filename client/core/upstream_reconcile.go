package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strconv"
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

type ReconcileRow struct {
	Note     int64  `json:"note"`
	Revision int64  `json:"revision"`
	Class    string `json:"class"`
	Remote   int64  `json:"remote,omitempty"`
}
type UpstreamReconcile struct {
	Lines      []string       `json:"lines"`
	Notes      []ReconcileRow `json:"notes"`
	Unknowns   []int64        `json:"unknowns"`
	Candidates int            `json:"candidates"`
	Unknown    int            `json:"unknown"`
	Cleared    bool           `json:"cleared"`
}
type reconcileSnapshot struct {
	Address string
	Mark    string
	Link    UpstreamLink
	Notes   []ReconcileRow
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
		if row.Class == "unknown" {
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

// freshReconcile reads raw destination notes, not the mirror or dependency notes.
// A second manifest rejects a read spanning different upstream revisions.
func (p *pusher) freshReconcile(l UpstreamLink) (map[int64]upNote, []int64, error) {
	remote, why := p.resolve(l)
	if why != "" {
		return nil, nil, ErrRefusal{why}
	}
	path := fmt.Sprintf("/minigw/v1/projects/%d/kb/manifest", remote)
	manifest := func() (*Manifest, error) {
		a := p.call("GET", path, nil, nil)
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
			if src.ID == remote {
				return m, nil
			}
		}
		return nil, ErrRefusal{"reconcile: destination is not readable"}
	}
	m, e := manifest()
	if e != nil {
		return nil, nil, e
	}
	fresh := map[int64]upNote{}
	var ids []int64
	totalBytes := 0
	for _, want := range m.Notes {
		if want.Source != remote {
			continue
		}
		a := p.call("GET", fmt.Sprintf("/minigw/v1/projects/%d/notes/%d", remote, want.ID), nil, nil)
		if a.status != 200 {
			return nil, nil, ErrRefusal{refusal(a)}
		}
		checked, why := checkMirrorNote(a.data, want, "")
		if why != "" {
			return nil, nil, ErrRefusal{"reconcile: " + why}
		}
		var raw struct {
			Body  *string
			Title string
		}
		if json.Unmarshal(a.data, &raw) != nil || raw.Body == nil || *raw.Body != checked.body || raw.Title != checked.title {
			return nil, nil, ErrRefusal{"reconcile: upstream text requires sanitization or is incomplete"}
		}
		totalBytes += len(raw.Title) + len(*raw.Body)
		if totalBytes > MaxMirrorBytes/mirrorCost {
			return nil, nil, ErrRefusal{"reconcile: destination exceeds the complete-read size limit"}
		}
		fresh[want.ID] = upNote{Revision: want.Rev, Class: want.Class, Title: raw.Title, Body: *raw.Body, EntryKind: want.Kind}
		ids = append(ids, want.ID)
	}
	again, e := manifest()
	if e != nil {
		return nil, nil, e
	}
	if !reflect.DeepEqual(m, again) {
		return nil, nil, ErrRefusal{"reconcile: upstream changed during the read; list again"}
	}
	return fresh, ids, nil
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
		if (l.Restored != nil || r.New && r.Note > 0) && (r.Upstream == "" || l.Upstream == r.Upstream) {
			chosen = append(chosen, l)
		}
	}
	if len(chosen) != 1 {
		return out, ErrRefusal{"reconcile: name exactly one restored link with --upstream <name>"}
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
	readCtx, cancel := context.WithTimeout(ctx, pullDryTimeout)
	defer cancel()
	p := &pusher{s: s, tr: tr, up: l.Upstream, ctx: readCtx, sleep: func(time.Duration) bool { return false }}
	fresh, ids, e := p.freshReconcile(l)
	if e != nil {
		return out, e
	}
	used := map[int64]bool{}
	for _, n := range scan.Notes {
		if n.Foreign {
			continue
		}
		row := ReconcileRow{Note: n.ID, Revision: n.Revision, Class: "unknown"}
		var matches []int64
		if len(key) > 0 {
			for _, id := range ids {
				if _, ok := ownTrailerSplit(fresh[id].Body, upstreamRef(key, l.Upstream, l.Project, n.ID), trailerAuthor(n.Author), math.MaxInt64); ok {
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
		if row.Class != "known" && n.Sent == nil && len(matches) == 1 {
			row.Class, row.Remote = "candidate", matches[0]
		}
		out.Notes = append(out.Notes, row)
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
		if addr != address || stamp != mark || !reflect.DeepEqual(l, cur) || !reflect.DeepEqual(scan, now) {
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
		targets := map[int64]bool{}
		if r.Note > 0 || r.All {
			if previous.Address != address || mark != "" && previous.Mark != mark || !reconcileSameLink(previous.Link, l) {
				return ErrRefusal{"reconcile: list this link before deciding"}
			}
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
				var old, current *ReconcileRow
				for i := range previous.Notes {
					if previous.Notes[i].Note == id {
						old = &previous.Notes[i]
					}
				}
				for i := range out.Notes {
					if out.Notes[i].Note == id {
						current = &out.Notes[i]
					}
				}
				if mark == "" && (old == nil || old.Class != "skip") {
					return ErrRefusal{"reconcile: this note was not skipped in the last listing"}
				}
				if old != nil && current != nil && r.New && old.Class == "skip" && current.Class == "unknown" && old.Note == current.Note && old.Revision == current.Revision && old.Remote == current.Remote {
					copy := *old
					copy.Class = "unknown"
					old = &copy
				}
				if old == nil || current == nil || *old != *current || r.All && old.Class != "unknown" {
					return ErrRefusal{"reconcile: note was not listed or changed; list again"}
				}
				if r.Confirm > 0 && (old.Class != "candidate" || old.Remote != r.Confirm) || r.Confirm == 0 && old.Class != "unknown" {
					return ErrRefusal{"reconcile: decision does not match the listed class or remote id"}
				}
			}
		}
		for i := range out.Notes {
			row := &out.Notes[i]
			var n UpstreamNote
			for _, v := range scan.Notes {
				if v.ID == row.Note {
					n = v
					break
				}
			}
			if targets[row.Note] {
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
					hash, content, rev, conflict, conflictRev := "", "", int64(0), 1, u.Revision
					if equal {
						hash, content, rev, conflict, conflictRev = n.Hash, sentContentHash(u.Class, u.EntryKind, u.Title, body), u.Revision, 0, 0
					}
					_, e = tx.Exec(`INSERT INTO upstream_sent(project_id,upstream,note_id,hash,revision,remote_sub,remote_note,sent,remote_rev,sent_content,conflict,conflict_rev) VALUES(?,?,?,?,?,0,?,?,?,?,?,?)`, l.Project, l.Upstream, n.ID, hash, n.Revision, r.Confirm, ms(s.now()), rev, content, conflict, conflictRev)
					if e != nil {
						return e
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
			} else if row.Class == "unknown" {
				var v string
				var d reconcileDecision
				if tx.QueryRow(`SELECT value FROM meta WHERE key=?`, reconcileDecisionKey(l.Project, l.Upstream, n.ID)).Scan(&v) == nil && json.Unmarshal([]byte(v), &d) == nil && d.Mark == mark && d.Revision == n.Revision && d.Hash == n.Hash {
					row.Class = d.Action
				}
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
		}
		if r.DryRun {
			return nil
		}
		if e = reconcileMeta(tx, reconcileKey(l.Project, l.Upstream, "listing"), reconcileSnapshot{address, mark, l, out.Notes}); e != nil {
			return e
		}
		if out.Candidates == 0 && out.Unknown == 0 {
			if _, e = tx.Exec(`DELETE FROM meta WHERE key=?`, restoredKey(l.Project, l.Upstream)); e != nil {
				return e
			}
			out.Cleared = true
		}
		counts := map[string]int{}
		for _, row := range out.Notes {
			counts[row.Class]++
		}
		for _, id := range ids {
			if !used[id] && looksFromGateway.MatchString(fresh[id].Body) {
				counts["orphan"]++
			}
		}
		return Audit(tx, adminActor, "upstream.reconcile", l.Upstream, map[string]any{"project": l.Project, "counts": counts, "candidates": out.Candidates, "unknown": out.Unknown, "cleared": out.Cleared})
	}
	if r.DryRun {
		e = s.read(ctx, apply)
	} else {
		e = s.write(ctx, apply)
	}
	if e != nil {
		return UpstreamReconcile{}, e
	}
	for _, row := range out.Notes {
		out.Lines = append(out.Lines, fmt.Sprintf("%s note %d (revision %d), remote %d", row.Class, row.Note, row.Revision, row.Remote))
	}
	for _, id := range ids {
		if !used[id] && looksFromGateway.MatchString(fresh[id].Body) {
			out.Lines = append(out.Lines, fmt.Sprintf("orphan remote %d", id))
		}
	}
	out.Lines = append(out.Lines, fmt.Sprintf("candidates %d, unknown %d, restored mark cleared: %t", out.Candidates, out.Unknown, out.Cleared))
	return out, nil
}

// Push bookkeeping may change between a listing and its decision; destination,
// policy and mode must still be the same. The final transaction checks all fields.
func reconcileSameLink(a, b UpstreamLink) bool {
	return a.Project == b.Project && a.Upstream == b.Upstream && a.Remote == b.Remote && a.Team == b.Team && a.WithPrivate == b.WithPrivate && a.Mode == b.Mode && a.Auto == b.Auto && reflect.DeepEqual(a.Classes, b.Classes)
}
