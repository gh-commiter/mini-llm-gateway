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
 SELECT id,title,body,'p'||project_id||'c'||class||' p'||project_id||'c'||class||'u'||author_id FROM notes`

func migrate12(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE VIRTUAL TABLE note_search USING fts5(title,body,scope, tokenize='unicode61'); ` + searchInsert)
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
func searchSQL(filter string) string {
	return `WITH visible AS MATERIALIZED (
 SELECT n.id,n.project_id,n.class,n.title,n.updated FROM notes n WHERE ` + filter + `),
 hits AS MATERIALIZED (SELECT v.* FROM visible v JOIN note_search ON note_search.rowid=v.id
 WHERE note_search MATCH ? ORDER BY v.updated DESC,v.id LIMIT 20)
 SELECT h.id,h.project_id,h.class,h.title,substr(n.body,1,160),h.updated
 FROM hits h JOIN notes n ON n.id=h.id ORDER BY h.updated DESC,h.id`
}
func searchNotes(ctx context.Context, tx *sql.Tx, user int64, rs []readable, text string) (SearchPage, error) {
	out := SearchPage{Hits: []SearchHit{}}
	query, err := searchQuery(text)
	if err != nil {
		return out, err
	}
	filter, args := where(user, rs)
	if filter == "0" {
		return out, nil
	}
	args = append(args, searchScope(user, rs)+" AND {title body} : ("+query+")")
	rows, err := tx.QueryContext(ctx, searchSQL(filter), args...)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var h SearchHit
		var updated int64
		if err = rows.Scan(&h.ID, &h.Project, &h.Class, &h.Title, &h.Snippet, &updated); err != nil {
			return SearchPage{}, err
		}
		h.Updated = fromMs(updated)
		candidate := append(out.Hits, h)
		b, _ := json.Marshal(SearchPage{Hits: candidate})
		if len(b) > 8191 { // Reserve the CLI's trailing newline.
			break
		}
		out.Hits = candidate
	}
	return out, rows.Err()
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
		out, e = searchNotes(ctx, tx, p.UserID, []readable{a.readable(project, false)}, text)
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
		rs, e := s.runScope(tx, c, approved)
		if e != nil {
			return e
		}
		if lim != nil {
			return lim
		}
		out, e = searchNotes(ctx, tx, c.UserID, rs, text)
		return e
	})
	if err != nil {
		return SearchPage{}, err
	}
	return
}
