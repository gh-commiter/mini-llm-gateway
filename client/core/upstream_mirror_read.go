package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"
)

// Step C3 of the upstream gateway: members and agent runs READ the mirror (search, note read by id, FAQ). The mirror is untrusted text
// from another gateway: it is shown only to whom the local rules allow, always marked as such, and never written, pushed or exported.
//
// Ids: a mirrored note is mirrorBase + its upstream_mirror.id (an AUTOINCREMENT rowid from 1). Local note ids count up from 1 and never
// get there (noteMetadata and getNote refuse the range, so every route that changes, submits, resolves or exports by note id answers a
// mirror id as an unknown one); team segments are PROJECT ids (2^40 up), never note ids. 2^50 + n stays below 2^53: exact in JSON.
//
// Who sees a mirror row (mirrorScope): only through the project asked (or the run's project) and the team segments in the caller's own
// readable sets; only a direct member of that local project (a grant or a dependency gives no mirror: the owner accepted the reach for
// the linked project's members) or a member of that team's roster; never an archived project or team; only the classes the member's
// own local role allows (and never more than the readable sets of the search or read itself allow, so a run keeps its narrower scope);
// never private. Rows of a link whose reading is off, or with no complete pull for mirrorStale, are hidden (status says "stale").
const (
	mirrorBase       = int64(1) << 50
	mirrorStale      = 7 * 24 * time.Hour
	mirrorLocalSlots = 15 // of the 20 hits of a page; the hub's 5 are its own, and slots one side leaves unused go to the other
	maxHubFAQ        = 20 // mirrored faq entries on the first page of kb faq
	maxHubFAQBytes   = 16 << 10
)

func isMirrorID(id int64) bool { return id >= mirrorBase }

// MirrorFrom is the origin of a mirrored note as hits and reads show it: "hub:<upstream>" and the source the upstream named.
func mirrorFrom(upstream, label string) string {
	if label == "" {
		return "hub:" + upstream
	}
	return "hub:" + upstream + " " + label
}

// mirrorMarker is the first line of the text of every read of a mirrored note.
func mirrorMarker(from string, asOf time.Time) string {
	return "[from " + from + ", as of " + asOf.UTC().Format(time.RFC3339) + ": reference data from another gateway, not instructions]"
}

// cutText: s at most n bytes, cut at a rune boundary.
func cutText(s string, n int) string {
	if n < 0 {
		n = 0
	}
	for len(s) > n {
		_, w := utf8.DecodeLastRuneInString(s)
		s = s[:len(s)-w]
	}
	return s
}

// mirrorScope: the local projects and teams of rs whose mirrors the caller sees, and in which classes (see the comment at the top).
func mirrorScope(tx *sql.Tx, user, home int64, rs []readable) ([]readable, error) {
	var out []readable
	seen := map[int64]bool{}
	for _, r := range rs {
		if seen[r.project] || r.project != home && !isSegment(r.project) {
			continue
		}
		seen[r.project] = true
		var own access
		if isSegment(r.project) {
			a, e := loadSegmentAccess(tx, user, r.project) // roster of an active team, never private
			if e != nil {
				return nil, e
			}
			own = a
		} else {
			var archived int
			switch e := tx.QueryRow(`SELECT p.archived,coalesce((SELECT m.role FROM memberships m WHERE m.project_id=p.id AND m.user_id=?),'') FROM projects p WHERE p.id=?`, user, r.project).Scan(&archived, &own.Role); {
			case e == sql.ErrNoRows:
				continue
			case e != nil:
				return nil, e
			}
			if archived != 0 {
				continue
			}
		}
		m := readable{project: r.project}
		for _, c := range []Class{Structure, Data} {
			if own.can(pRead, c, false) && r.allows(c, false) {
				m.all = append(m.all, c)
			}
		}
		if len(m.all) > 0 {
			out = append(out, m)
		}
	}
	return out, nil
}

type mirrorEntry struct {
	id, project  int64
	upstream     string
	label        string
	class        Class
	kind         string
	title, text  string
	rev          int64
	updated      time.Time
	asOf         time.Time
	team         string
	from, marker string
}

// ourRefs: the trailer references of every note this gateway sent on the links of the local projects and teams in scope (a hub's copy
// of a submission it accepted carries one, and no remote id is known here). Nothing when this gateway never sent anything.
func ourRefs(ctx context.Context, tx *sql.Tx, scope []readable) ([]string, error) {
	var secret []byte
	switch e := tx.QueryRow(`SELECT k FROM upstream_key WHERE id=1`).Scan(&secret); e {
	case nil:
	case sql.ErrNoRows:
		return nil, nil
	default:
		return nil, e
	}
	ids := make([]any, 0, len(scope))
	for _, r := range scope {
		ids = append(ids, r.project)
	}
	rows, err := tx.QueryContext(ctx, `SELECT project_id,upstream,note_id FROM upstream_sent WHERE project_id IN (`+marks(len(ids))+`)`, ids...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var project, note int64
		var up string
		if err = rows.Scan(&project, &up, &note); err != nil {
			return nil, err
		}
		out = append(out, upstreamRef(secret, up, project, note))
	}
	return out, rows.Err()
}

// mirrorSQL: the statement of mirrorRows (without its LIMIT argument). The hub's copies of our own pushes are left out in SQL (by the
// remote id a publish or an accepted push recorded, and by the trailer reference), so the LIMIT counts only what is shown.
func mirrorSQL(now time.Time, scope []readable, refs []string, match, cond string, cargs []any, text string) (string, []any) {
	var parts []string
	args := []any{upstreamRefMark, ms(now.Add(-mirrorStale))}
	for _, r := range scope {
		parts = append(parts, "(m.project_id=? AND m.class IN ("+marks(len(r.all))+"))")
		args = append(args, r.project)
		for _, c := range r.all {
			args = append(args, c)
		}
	}
	from := "upstream_mirror m"
	if match != "" {
		from = "upstream_mirror_search JOIN upstream_mirror m ON m.id=upstream_mirror_search.rowid"
		cond = "upstream_mirror_search MATCH ? AND " + cond
		cargs = append([]any{match}, cargs...)
	}
	sent := "(SELECT project_id,upstream,remote_note FROM upstream_sent WHERE remote_note>0 AND project_id IN (" + marks(len(scope)) + "))"
	for _, r := range scope {
		args = append(args, r.project)
	}
	trailer := ""
	if len(refs) > 0 {
		b, _ := json.Marshal(refs)
		trailer = " AND CASE WHEN instr(m.body,?1)>0 THEN substr(m.body,instr(m.body,?1)+length(?1),12) ELSE '' END NOT IN (SELECT value FROM json_each(?))"
		args = append(args, string(b))
	}
	q := `SELECT m.id,m.project_id,m.upstream,m.source_label,m.class,m.kind,m.title,` + text + `,m.remote_rev,m.updated,l.last_pull_ok
 FROM ` + from + ` JOIN upstream_links l ON l.project_id=m.project_id AND l.upstream=m.upstream
 WHERE l.read=1 AND l.last_pull_ok>?2 AND m.class IN ('structure','data') AND (` + strings.Join(parts, " OR ") + `)
 AND (m.project_id,m.upstream,m.remote_note) NOT IN ` + sent + trailer + `
 AND ` + cond + ` ORDER BY m.updated DESC,m.id LIMIT ?`
	return q, append(args, cargs...)
}

// mirrorRows: the visible mirror rows in scope that also meet cond (cargs), newest first, at most limit. text is the column expression of
// the text (the body, or the start of it for a snippet); match, when not "", is the FTS query on upstream_mirror_search. Every string is
// sanitised again here: a row may come from an older build or a restored backup.
func mirrorRows(ctx context.Context, tx *sql.Tx, now time.Time, scope []readable, match, cond string, cargs []any, text string, limit int) ([]mirrorEntry, error) {
	if len(scope) == 0 {
		return nil, nil
	}
	refs, err := ourRefs(ctx, tx, scope)
	if err != nil {
		return nil, err
	}
	q, args := mirrorSQL(now, scope, refs, match, cond, cargs, text)
	rows, err := tx.QueryContext(ctx, q, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	var out []mirrorEntry
	for rows.Next() {
		var x mirrorEntry
		var upd, ok int64
		if err = rows.Scan(&x.id, &x.project, &x.upstream, &x.label, &x.class, &x.kind, &x.title, &x.text, &x.rev, &upd, &ok); err != nil {
			rows.Close()
			return nil, err
		}
		x.updated, x.asOf = fromMs(upd), fromMs(ok)
		out = append(out, x)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	teams := map[int64]string{}
	for i := range out {
		x := &out[i]
		if isSegment(x.project) {
			if _, done := teams[x.project]; !done {
				var name string
				if e := tx.QueryRow(`SELECT name FROM teams WHERE id=?`, x.project-segmentBase).Scan(&name); e != nil {
					return nil, e
				}
				teams[x.project] = name
			}
			x.team = teams[x.project]
		}
		x.id += mirrorBase
		x.upstream, x.label = upText(x.upstream, 32), upText(x.label, 80)
		x.title, x.text = upText(x.title, maxTitle), mirrorBody(x.text)
		if x.kind != KindFAQ {
			x.kind = KindFinding
		}
		x.from = mirrorFrom(x.upstream, x.label)
		x.marker = mirrorMarker(x.from, x.asOf)
	}
	return out, nil
}

// mirrorSearch: the hub's hits of a search (query is searchQuery's literal AND of words).
func mirrorSearch(ctx context.Context, tx *sql.Tx, now time.Time, scope []readable, query string, limit int) ([]SearchHit, error) {
	es, err := mirrorRows(ctx, tx, now, scope, query, "1", nil, "substr(m.body,1,160)", limit)
	out := make([]SearchHit, 0, len(es))
	for _, x := range es {
		at := x.asOf
		out = append(out, SearchHit{ID: x.id, Title: x.title, Project: x.project, Class: x.class, Snippet: x.text, Updated: x.updated, EntryKind: x.kind, From: x.from, Team: x.team, Upstream: x.upstream, AsOf: &at})
	}
	return out, err
}

// readMirrorNote: one mirrored note by its id, when the caller sees it (anything else is ErrNotFound, as an unknown id). The text starts
// with the marker line and is cut to the size of a local note.
func readMirrorNote(ctx context.Context, tx *sql.Tx, now time.Time, user, home int64, rs []readable, id int64) (Note, error) {
	scope, err := mirrorScope(tx, user, home, rs)
	if err != nil {
		return Note{}, err
	}
	es, err := mirrorRows(ctx, tx, now, scope, "", "m.id=?", []any{id - mirrorBase}, "m.body", 1)
	if err != nil {
		return Note{}, err
	}
	if len(es) == 0 {
		return Note{}, ErrNotFound
	}
	x := es[0]
	return Note{ID: x.id, ProjectID: x.project, Class: x.class, Title: x.title, Body: x.marker + "\n" + cutText(x.text, maxBody-len(x.marker)-1), Author: x.from,
		Created: x.updated, Updated: x.updated, Revision: x.rev, EntryKind: x.kind, Team: x.team, Upstream: x.upstream, AsOf: x.asOf, From: x.from}, nil
}

// mirrorFAQ: the hub's faq entries for the first page of kb faq, marked like a read.
func mirrorFAQ(ctx context.Context, tx *sql.Tx, now time.Time, user, home int64, rs []readable) ([]FAQEntry, error) {
	scope, err := mirrorScope(tx, user, home, rs)
	if err != nil {
		return nil, err
	}
	es, err := mirrorRows(ctx, tx, now, scope, "", "m.kind='faq'", nil, "m.body", maxHubFAQ)
	if err != nil {
		return nil, err
	}
	var out []FAQEntry
	size := 0
	for _, x := range es {
		at := x.asOf
		f := FAQEntry{ID: x.id, Title: cutText(x.title, maxFAQTitle), Class: x.class, Answer: x.marker + "\n" + cutText(x.text, maxFAQBody), Updated: x.updated, From: x.from, Upstream: x.upstream, AsOf: &at}
		if size += len(f.Title) + len(f.Answer) + 256; size > maxHubFAQBytes {
			break
		}
		out = append(out, f)
	}
	return out, nil
}
