package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Scope tokens let MATCH intersect permitted projects/classes/authors in the index.
const searchInsert = `INSERT INTO note_search(rowid,title,body,scope)
 SELECT id,title,body,'p'||project_id||'c'||class||' p'||project_id||'c'||class||'u'||CASE WHEN imported=0 THEN author_id ELSE 0 END FROM notes`

func migrate12(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE VIRTUAL TABLE note_search USING fts5(title,body,scope, tokenize='unicode61'); ` + strings.Replace(searchInsert, "CASE WHEN imported=0 THEN author_id ELSE 0 END", "author_id", 1))
	return err
}

// Explicit writes work with trusted_schema=0; virtual-table triggers do not.
func syncSearch(tx *sql.Tx, id int64) error {
	if _, err := tx.Exec(`DELETE FROM note_search WHERE rowid=?`, id); err != nil {
		return err
	}
	_, err := tx.Exec(searchInsert+` WHERE id=?`, id)
	return err
}

// rebuildSearch replaces every posting with one built from the notes.
func rebuildSearch(tx *sql.Tx) error {
	_, err := tx.Exec(`DELETE FROM note_search; ` + searchInsert)
	return err
}
func (s *Store) RebuildSearch(ctx context.Context) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		if err := rebuildSearch(tx); err != nil {
			return err
		}
		return Audit(tx, adminActor, "kb.search.rebuild", "", nil)
	})
}

type SearchHit struct {
	ID      int64     `json:"id"`
	Title   string    `json:"title"`
	Project int64     `json:"project"`
	Class   Class     `json:"class"`
	Snippet string    `json:"snippet"`
	Updated time.Time `json:"updated"`
	// EntryKind is finding or faq.
	EntryKind string `json:"entryKind"`
	// From names where a hit comes from when it is not the project asked: "team:<name>" for the team's notes,
	// "project:<slug>" for a project the asked one depends on.
	From string `json:"from,omitempty"`
}
type SearchPage struct {
	Hits []SearchHit `json:"hits"`
}

// Only literal words, combined with AND. Operators and prefixes never reach FTS.
func searchQuery(text string) (string, error) {
	if len(text) > 512 || !utf8.ValidString(text) {
		return "", ErrInvalid{"query: at most 512 UTF-8 bytes"}
	}
	words := strings.FieldsFunc(text, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) && !unicode.IsMark(r) })
	if len(words) == 0 || len(words) > 16 {
		return "", ErrInvalid{"query: 1 to 16 words"}
	}
	for i, w := range words {
		if len(w) > 64 {
			return "", ErrInvalid{"query: word too long"}
		}
		words[i] = `"` + w + `"`
	}
	return strings.Join(words, " AND "), nil
}

func searchScope(user int64, rs []readable) string {
	var terms []string
	for _, r := range rs {
		prefix := "p" + strconv.FormatInt(r.project, 10) + "c"
		for _, c := range r.all {
			terms = append(terms, `"`+prefix+string(c)+`"`)
		}
		for _, c := range r.own {
			terms = append(terms, `"`+prefix+string(c)+"u"+strconv.FormatInt(user, 10)+`"`)
		}
	}
	return "scope : (" + strings.Join(terms, " OR ") + ")"
}

// Both MATCH and SQL constrain visibility. No corpus-dependent rank or counts;
// bodies are loaded only for visible hits.
// The home project's hits come first, then the others, each by recency (there is no corpus rank).
func searchSQL(filter string) string {
	// Two bounded top-20 selections (the home project's, then the rest) instead of one ORDER BY on an expression:
	// the expression made SQLite sort every match of a large corpus and blew the 250 ms deadline.
	return `WITH visible AS MATERIALIZED (
 SELECT n.id,n.project_id,n.class,n.title,n.updated FROM notes n WHERE ` + filter + `),
 own AS (SELECT v.* FROM visible v JOIN note_search ON note_search.rowid=v.id WHERE note_search MATCH ? AND v.project_id=? ORDER BY v.updated DESC,v.id LIMIT 20),
 other AS (SELECT v.* FROM visible v JOIN note_search ON note_search.rowid=v.id WHERE note_search MATCH ? AND v.project_id<>? ORDER BY v.updated DESC,v.id LIMIT 20),
 hits AS MATERIALIZED (SELECT * FROM (SELECT *,0 g FROM own UNION ALL SELECT *,1 g FROM other) ORDER BY g,updated DESC,id LIMIT 20)
 SELECT h.id,h.project_id,h.class,h.title,substr(n.body,1,160),h.updated,n.entry_kind
 FROM hits h JOIN notes n ON n.id=h.id ORDER BY h.g,h.updated DESC,h.id`
}
func searchNotes(ctx context.Context, tx *sql.Tx, user, home int64, rs []readable, text string) (SearchPage, error) {
	out := SearchPage{Hits: []SearchHit{}}
	query, err := searchQuery(text)
	if err != nil {
		return out, err
	}
	filter, args := where(user, rs)
	if filter == "0" {
		return out, nil
	}
	match := searchScope(user, rs) + " AND {title body} : (" + query + ")"
	args = append(args, match, home, match, home)
	rows, err := tx.QueryContext(ctx, searchSQL(filter), args...)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	var found []SearchHit
	for rows.Next() {
		var h SearchHit
		var updated int64
		if err = rows.Scan(&h.ID, &h.Project, &h.Class, &h.Title, &h.Snippet, &updated, &h.EntryKind); err != nil {
			return SearchPage{}, err
		}
		h.Updated = fromMs(updated)
		found = append(found, h)
	}
	if err = rows.Err(); err != nil {
		return out, err
	}
	rows.Close()
	labels := map[int64]string{} // at most one lookup per source
	for i := range found {
		p := found[i].Project
		if p == home && !isSegment(p) {
			continue
		}
		if _, ok := labels[p]; !ok && !isSegment(p) { // a project that the home project depends on: its slug, else no label
			var slug string
			switch err = tx.QueryRow(`SELECT p.slug FROM project_depends d JOIN projects p ON p.id=d.depends_on WHERE d.project_id=? AND d.depends_on=?`, home, p).Scan(&slug); err {
			case nil:
				labels[p] = "project:" + slug
			case sql.ErrNoRows:
				labels[p] = ""
			default:
				return SearchPage{}, err
			}
		}
		if !isSegment(p) {
			found[i].From = labels[p]
			continue
		}
		if _, ok := labels[p]; !ok {
			var name string
			if err = tx.QueryRow(`SELECT name FROM teams WHERE id=?`, p-segmentBase).Scan(&name); err != nil {
				return SearchPage{}, err
			}
			labels[p] = "team:" + name
		}
		found[i].From = labels[p]
	}
	for _, h := range found {
		candidate := append(out.Hits, h)
		b, _ := json.Marshal(SearchPage{Hits: candidate})
		if len(b) > 8191 { // Reserve the CLI's trailing newline.
			break
		}
		out.Hits = candidate
	}
	return out, nil
}
func (s *Store) SearchNotes(ctx context.Context, p Principal, project int64, text string) (out SearchPage, err error) {
	ctx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()
	lim := s.rate(p.UserID, false)
	err = s.read(ctx, func(tx *sql.Tx) error {
		a, e := memberAccess(tx, p, project)
		if e != nil {
			return e
		}
		if lim != nil {
			return lim
		}
		rs, e := noteSources(tx, p.UserID, project, a)
		if e != nil {
			return e
		}
		out, e = searchNotes(ctx, tx, p.UserID, project, rs, text)
		return e
	})
	if err != nil {
		return SearchPage{}, err
	}
	return
}
func (s *Store) RunSearchNotes(ctx context.Context, c RunCap, approved bool, text string) (out SearchPage, err error) {
	ctx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	defer cancel()
	lim := s.rate(c.UserID, false)
	err = s.read(ctx, func(tx *sql.Tx) error {
		rs, e := s.runScope(tx, c, approved, true)
		if e != nil {
			return e
		}
		if lim != nil {
			return lim
		}
		out, e = searchNotes(ctx, tx, c.UserID, c.ProjectID, rs, text)
		return e
	})
	if err != nil {
		return SearchPage{}, err
	}
	return
}
