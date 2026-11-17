package core

import (
	"context"
	"database/sql"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Entry kinds of notes and submissions. An empty NoteInput.EntryKind means "unspecified".
const (
	KindFinding = "finding"
	KindFAQ     = "faq"
)

const (
	maxFAQTitle    = 256
	maxFAQBody     = 2048
	maxRemotes     = 8
	maxIdentity    = 256
	maxTeamPathLen = 128
)

func validKind(k string) bool { return k == KindFinding || k == KindFAQ }

// migrate14: registered git identities of projects, the team path label and the entry kind.
func migrate14(tx *sql.Tx) error {
	_, err := tx.Exec(`ALTER TABLE teams ADD COLUMN path TEXT NOT NULL DEFAULT '';
CREATE TABLE project_remotes(project_id INTEGER NOT NULL REFERENCES projects(id), identity TEXT NOT NULL, PRIMARY KEY(project_id,identity)) WITHOUT ROWID;
CREATE INDEX project_remotes_identity ON project_remotes(identity);
ALTER TABLE notes ADD COLUMN entry_kind TEXT NOT NULL DEFAULT 'finding' CHECK(entry_kind IN('finding','faq'));
ALTER TABLE kb_submissions ADD COLUMN entry_kind TEXT NOT NULL DEFAULT 'finding' CHECK(entry_kind IN('finding','faq'));`)
	return err
}

// NormalizeRemote turns a git remote URL into the identity "host/path": lower case, no userinfo, no port, no
// ".git", at least two path segments. Local paths, file:// and a missing host are refused.
func NormalizeRemote(raw string) (string, error) {
	bad := ErrInvalid{"git remote"}
	raw = strings.TrimSpace(raw)
	var host, p string
	if i := strings.Index(raw, "://"); i >= 0 {
		u, err := url.Parse(raw)
		if err != nil {
			return "", bad
		}
		switch strings.ToLower(u.Scheme) {
		case "https", "http", "ssh", "git":
		default:
			return "", bad
		}
		host, p = u.Hostname(), u.Path
	} else {
		i := strings.Index(raw, ":")
		if i <= 0 || strings.Contains(raw[:i], "/") || strings.Contains(raw, "\\") {
			return "", bad
		}
		host, p = raw[:i], raw[i+1:]
		if j := strings.LastIndex(host, "@"); j >= 0 {
			host = host[j+1:]
		}
		if len(host) == 1 { // a Windows drive letter, not a host
			return "", bad
		}
	}
	if host == "" || strings.ContainsFunc(host, func(r rune) bool {
		return !(r == '.' || r == '-' || r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r))
	}) {
		return "", bad
	}
	p = strings.TrimSuffix(strings.TrimRight(p, "/"), ".git")
	var segs []string
	for _, s := range strings.Split(p, "/") {
		if s == "" {
			continue
		}
		if s == "." || s == ".." || strings.ContainsFunc(s, unicode.IsControl) {
			return "", bad
		}
		segs = append(segs, s)
	}
	if len(segs) < 2 {
		return "", bad
	}
	id := foldName(host + "/" + strings.Join(segs, "/"))
	if len(id) > maxIdentity {
		return "", bad
	}
	return id, nil
}

// normalizeTeamPath checks a team path label (or a `?path=` prefix): folded, no control characters, no
// leading or trailing "/". An empty label is valid and clears.
func normalizeTeamPath(label string) (string, error) {
	label = foldName(strings.TrimSpace(label))
	if len(label) > maxTeamPathLen || strings.HasPrefix(label, "/") || strings.HasSuffix(label, "/") ||
		strings.ContainsFunc(label, unicode.IsControl) {
		return "", ErrInvalid{"path"}
	}
	return label, nil
}

// SetTeamPath sets the free-text path label of a team (owner only; "" clears).
func (s *Store) SetTeamPath(ctx context.Context, id int64, label string) error {
	label, err := normalizeTeamPath(label)
	if err != nil {
		return err
	}
	return s.adminWrite(ctx, "team.path", strconv.FormatInt(id, 10), map[string]any{"path": label}, func(tx *sql.Tx) (string, error) {
		return "", countOne(tx.Exec(`UPDATE teams SET path=? WHERE id=?`, label, id))
	})
}

func remoteList(tx *sql.Tx, project int64) ([]string, error) {
	rows, err := tx.Query(`SELECT identity FROM project_remotes WHERE project_id=? ORDER BY identity`, project)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// applyRemotes removes and adds identities in the caller's transaction; the project keeps at most maxRemotes.
func applyRemotes(tx *sql.Tx, project int64, add, remove []string) (current []string, detail map[string]any, err error) {
	if len(add)+len(remove) == 0 || len(add) > maxRemotes || len(remove) > maxRemotes {
		return nil, nil, ErrInvalid{"remotes"}
	}
	norm := func(raw []string) ([]string, error) {
		var out []string
		for _, r := range raw {
			id, e := NormalizeRemote(r)
			if e != nil {
				return nil, e
			}
			out = append(out, id)
		}
		return out, nil
	}
	adds, err := norm(add)
	if err != nil {
		return nil, nil, err
	}
	stored, err := remoteList(tx, project)
	if err != nil {
		return nil, nil, err
	}
	var removes []string
	for _, r := range remove {
		if slices.Contains(stored, r) { // an identity exactly as listed
			removes = append(removes, r)
			continue
		}
		id, e := NormalizeRemote(r)
		if e != nil {
			return nil, nil, e
		}
		removes = append(removes, id)
	}
	for _, id := range removes {
		if _, err = tx.Exec(`DELETE FROM project_remotes WHERE project_id=? AND identity=?`, project, id); err != nil {
			return nil, nil, err
		}
	}
	for _, id := range adds {
		if _, err = tx.Exec(`INSERT OR IGNORE INTO project_remotes(project_id,identity) VALUES(?,?)`, project, id); err != nil {
			return nil, nil, err
		}
	}
	if current, err = remoteList(tx, project); err != nil {
		return nil, nil, err
	}
	if len(current) > maxRemotes {
		return nil, nil, ErrInvalid{"remotes: at most 8 per project"}
	}
	return current, map[string]any{"project": project, "add": adds, "remove": removes}, nil
}

// ProjectRemotes lists the identities of a project; any member (a grant alone is not enough).
func (s *Store) ProjectRemotes(ctx context.Context, p Principal, project int64) (out []string, err error) {
	lim := s.rate(p.UserID, false)
	err = s.read(ctx, func(tx *sql.Tx) error {
		a, e := memberAccess(tx, p, project)
		if e != nil {
			return e
		}
		if a.Role == "" {
			return ErrForbidden
		}
		if lim != nil {
			return lim
		}
		out, e = remoteList(tx, project)
		return e
	})
	return
}

// ChangeProjectRemotes is the project admin's write (team-derived admin counts). Archived: ErrArchived.
func (s *Store) ChangeProjectRemotes(ctx context.Context, p Principal, project int64, add, remove []string) (out []string, err error) {
	lim := s.rate(p.UserID, true)
	err = s.write(ctx, func(tx *sql.Tx) error {
		a, e := memberAccess(tx, p, project)
		if e != nil {
			return e
		}
		if a.Role != Admin {
			return ErrForbidden
		}
		if e = archivedErr(tx, project); e != nil {
			return e
		}
		if lim != nil {
			return lim
		}
		var detail map[string]any
		if out, detail, e = applyRemotes(tx, project, add, remove); e != nil {
			return e
		}
		return Audit(tx, actor(p), "project.remotes", strconv.FormatInt(project, 10), detail)
	})
	if err != nil {
		return nil, err
	}
	return
}

// AdminProjectRemotes and AdminChangeProjectRemotes are the owner's views of the same list.
func (s *Store) AdminProjectRemotes(ctx context.Context, project int64) (out []string, err error) {
	err = s.read(ctx, func(tx *sql.Tx) error {
		if e := exists(tx, "projects", project); e != nil {
			return e
		}
		var e error
		out, e = remoteList(tx, project)
		return e
	})
	return
}
func (s *Store) AdminChangeProjectRemotes(ctx context.Context, project int64, add, remove []string) (out []string, err error) {
	err = s.write(ctx, func(tx *sql.Tx) error {
		if e := exists(tx, "projects", project); e != nil {
			return e
		}
		if e := archivedErr(tx, project); e != nil {
			return e
		}
		var detail map[string]any
		var e error
		if out, detail, e = applyRemotes(tx, project, add, remove); e != nil {
			return e
		}
		return Audit(tx, adminActor, "project.remotes", strconv.FormatInt(project, 10), detail)
	})
	if err != nil {
		return nil, err
	}
	return
}

// fillProject adds the caller's role, origins and granted classes to a project row.
func fillProject(tx *sql.Tx, user int64, pr *Project) error {
	a, err := loadAccess(tx, user, pr.ID)
	if err != nil {
		return err
	}
	pr.Role = a.Role
	if pr.Origins, err = membershipOrigins(tx, pr.ID, user); err != nil {
		return err
	}
	for _, c := range []Class{Structure, Data} {
		if a.Grants[c] {
			pr.Granted = append(pr.Granted, c)
		}
	}
	return nil
}

// ProjectsByRemote finds the caller's projects registered for a git remote: direct member, not archived.
// It returns the identity so the client can name it.
func (s *Store) ProjectsByRemote(ctx context.Context, p Principal, raw string) (identity string, out []Project, err error) {
	if identity, err = NormalizeRemote(raw); err != nil {
		return "", nil, err
	}
	lim := s.rate(p.UserID, false)
	err = s.read(ctx, func(tx *sql.Tx) error {
		if e := checkPrincipal(tx, p); e != nil {
			return e
		}
		if lim != nil {
			return lim
		}
		rows, e := tx.Query(`SELECT id,slug,name,archived FROM projects WHERE archived=0
 AND id IN (SELECT project_id FROM project_remotes WHERE identity=?1)
 AND id IN (SELECT project_id FROM memberships WHERE user_id=?2) ORDER BY slug`, identity, p.UserID)
		if e != nil {
			return e
		}
		for rows.Next() {
			var pr Project
			if e = rows.Scan(&pr.ID, &pr.Slug, &pr.Name, &pr.Archived); e != nil {
				break
			}
			out = append(out, pr)
		}
		if e == nil {
			e = rows.Err()
		}
		rows.Close()
		if e != nil {
			return e
		}
		for i := range out {
			if e = fillProject(tx, p.UserID, &out[i]); e != nil {
				return e
			}
		}
		return nil
	})
	if err != nil {
		return "", nil, err
	}
	return
}

// ListProjectsByPath is ListProjects restricted to projects of a non-archived team the caller belongs to whose
// path label equals prefix or starts with prefix + "/". The label never widens what the caller can see.
func (s *Store) ListProjectsByPath(ctx context.Context, p Principal, prefix string, includeArchived bool) ([]Project, error) {
	prefix, err := normalizeTeamPath(prefix)
	if err != nil || prefix == "" {
		return nil, ErrInvalid{"path"}
	}
	all, err := s.ListProjects(ctx, p, includeArchived)
	if err != nil {
		return nil, err
	}
	var keep map[int64]bool
	err = s.read(ctx, func(tx *sql.Tx) error {
		keep = map[int64]bool{}
		rows, e := tx.Query(`SELECT DISTINCT tp.project_id FROM team_projects tp JOIN teams t ON t.id=tp.team_id JOIN team_members m ON m.team_id=t.id
 WHERE m.user_id=?1 AND t.archived=0 AND t.path<>'' AND (t.path=?2 OR substr(t.path,1,length(?2)+1)=?2||'/')`, p.UserID, prefix)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			if e = rows.Scan(&id); e != nil {
				return e
			}
			keep[id] = true
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	out := []Project{}
	for _, pr := range all {
		if keep[pr.ID] {
			out = append(out, pr)
		}
	}
	return out, nil
}

// FAQEntry is one line of the FAQ view.
type FAQEntry struct {
	ID      int64
	Title   string
	Class   Class
	Answer  string
	Updated time.Time
}

// FAQPage is a page of FAQ entries; Next is "" for the last page.
type FAQPage struct {
	Entries []FAQEntry
	Next    string
}

// maxFAQPageBytes bounds one page of the FAQ view.
const maxFAQPageBytes = 64 << 10

// ListFAQ lists the faq notes of a project the caller may read, newest first. The visibility filter is the one
// of note lists; pending submissions are not notes and never appear.
func (s *Store) ListFAQ(ctx context.Context, p Principal, project int64, after string) (pg FAQPage, err error) {
	lim := s.rate(p.UserID, false)
	err = s.read(ctx, func(tx *sql.Tx) error {
		a, e := memberAccess(tx, p, project)
		if e != nil {
			return e
		}
		if lim != nil {
			return lim
		}
		q, args := where(p.UserID, []readable{a.readable(project, false)})
		q += " AND n.entry_kind='faq'"
		if after != "" {
			c, e := decodeCursor(after, p.UserID, project, "")
			if e != nil {
				return e
			}
			q += " AND (n.updated<? OR (n.updated=? AND n.id>?))"
			args = append(args, c.T, c.T, c.I)
		}
		rows, e := tx.Query(`SELECT n.id,n.class,n.title,n.body,n.updated FROM notes n WHERE `+q+` ORDER BY n.updated DESC, n.id LIMIT 51`, args...)
		if e != nil {
			return e
		}
		defer rows.Close()
		size := 0
		more := false
		for rows.Next() {
			var f FAQEntry
			var updated int64
			if e = rows.Scan(&f.ID, &f.Class, &f.Title, &f.Answer, &updated); e != nil {
				return e
			}
			f.Updated = fromMs(updated)
			if len(pg.Entries) == 50 || (len(pg.Entries) > 0 && size+len(f.Title)+len(f.Answer)+256 > maxFAQPageBytes) {
				more = true
				break
			}
			size += len(f.Title) + len(f.Answer) + 256
			pg.Entries = append(pg.Entries, f)
		}
		if e = rows.Err(); e != nil {
			return e
		}
		if more {
			last := pg.Entries[len(pg.Entries)-1]
			pg.Next = encodeCursor(cursor{U: p.UserID, P: project, T: ms(last.Updated), I: last.ID})
		}
		return nil
	})
	return
}
