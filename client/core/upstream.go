package core

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// An upstream is another gateway that this one sends a project's published notes to, as an ordinary client (its own device key; the
// secret lives in the host's credential store, never here). What is stored here is only the name, the address (no secret), the links
// and the receipts of what was sent. Step A: by hand, submit mode.
const (
	maxUpstreams     = 16
	maxUpstreamLinks = 8 // per project or team
)

// maxPushNotes: the notes SENT (new or changed, not the unchanged ones passed over) per link and run; the rest goes on the next run.
// A variable so that a test can lower it.
var maxPushNotes = 1000

var numericHost = regexp.MustCompile(`^(0[xX][0-9a-fA-F]+|[0-9]+)(\.(0[xX][0-9a-fA-F]+|[0-9]+))*$`)

var upstreamNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

func ValidUpstreamName(s string) bool { return upstreamNameRe.MatchString(s) }

// ValidDeviceName: how this gateway names its device on an upstream: printable ASCII (it goes into an HTTP header), 1 to 64 characters.
func ValidDeviceName(s string) bool {
	if len(s) < 1 || len(s) > 64 {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r > 0x7e {
			return false
		}
	}
	return strings.TrimSpace(s) == s
}

// NormalizeUpstreamAddress: host[:port] of a gateway (the encrypted client protocol of `minigw connect`, default port 8765). No scheme,
// no user, no path; not an unspecified or link-local address (cloud metadata and the like).
func NormalizeUpstreamAddress(s string) (string, error) {
	bad := ErrRefusal{"upstream address: give host or host:port of a gateway (no scheme, user or path), for example mini.local:8765"}
	if s == "" || len(s) > 253 || strings.ContainsAny(s, "/@?#\\ \t\r\n") || strings.Contains(s, "://") {
		return "", bad
	}
	host, port := s, "8765"
	if h, p, err := net.SplitHostPort(s); err == nil {
		host, port = h, p
	} else if strings.HasPrefix(s, "[") {
		if !strings.HasSuffix(s, "]") { // "[::1]" without a port; anything else after the bracket is not an address
			return "", bad
		}
	} else if strings.Contains(s, ":") {
		return "", bad
	}
	host = strings.Trim(host, "[]")
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 || host == "" {
		return "", bad
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
			return "", ErrRefusal{"upstream address: " + host + " is not an address of a gateway"}
		}
	} else if !regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?$`).MatchString(host) || numericHost.MatchString(host) {
		return "", bad // not a name: "2852039166", "0x7f000001", "127.1", "010.0.0.1" are addresses in disguise
	}
	return net.JoinHostPort(strings.ToLower(host), port), nil
}

func migrate19(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE upstreams(name TEXT PRIMARY KEY, address TEXT NOT NULL, created INTEGER NOT NULL) WITHOUT ROWID;
CREATE TABLE upstream_links(project_id INTEGER NOT NULL REFERENCES projects(id), upstream TEXT NOT NULL REFERENCES upstreams(name), remote TEXT NOT NULL, remote_team INTEGER NOT NULL CHECK(remote_team IN(0,1)), classes TEXT NOT NULL, with_private INTEGER NOT NULL CHECK(with_private IN(0,1)), last_push INTEGER NOT NULL DEFAULT 0, last_error TEXT NOT NULL DEFAULT '', PRIMARY KEY(project_id,upstream)) WITHOUT ROWID;
CREATE TABLE upstream_sent(project_id INTEGER NOT NULL, upstream TEXT NOT NULL, note_id INTEGER NOT NULL, hash TEXT NOT NULL, revision INTEGER NOT NULL, remote_sub INTEGER NOT NULL, remote_note INTEGER NOT NULL, sent INTEGER NOT NULL, PRIMARY KEY(project_id,upstream,note_id)) WITHOUT ROWID;`)
	return err
}

type Upstream struct {
	Name    string `json:"name"`
	Address string `json:"address"`
}

func (s *Store) AddUpstream(ctx context.Context, name, address string) error {
	if !ValidUpstreamName(name) {
		return ErrRefusal{"upstream name: lowercase letters, digits and -, up to 32"}
	}
	addr, err := NormalizeUpstreamAddress(address)
	if err != nil {
		return err
	}
	return s.adminWrite(ctx, "upstream.add", name, map[string]any{"address": addr}, func(tx *sql.Tx) (string, error) {
		var n, same int
		if e := tx.QueryRow(`SELECT count(*), count(CASE WHEN name=? THEN 1 END) FROM upstreams`, name).Scan(&n, &same); e != nil {
			return "", e
		}
		if same > 0 {
			return "", ErrRefusal{"an upstream named " + name + " exists: remove it first (minigw gw upstream remove " + name + ")"}
		}
		if n >= maxUpstreams {
			return "", ErrRefusal{"at most 16 upstreams"}
		}
		_, e := tx.Exec(`INSERT INTO upstreams(name,address,created) VALUES(?,?,?)`, name, addr, ms(s.now()))
		return "", e
	})
}

// RemoveUpstream removes the upstream with its links and receipts (the notes already sent stay where they are).
func (s *Store) RemoveUpstream(ctx context.Context, name string) error {
	return s.adminWrite(ctx, "upstream.remove", name, nil, func(tx *sql.Tx) (string, error) {
		for _, q := range []string{`DELETE FROM upstream_sent WHERE upstream=?`, `DELETE FROM upstream_links WHERE upstream=?`} {
			if _, e := tx.Exec(q, name); e != nil {
				return "", e
			}
		}
		return "", countOne(tx.Exec(`DELETE FROM upstreams WHERE name=?`, name))
	})
}

func (s *Store) ListUpstreams(ctx context.Context) (out []Upstream, err error) {
	err = s.read(ctx, func(tx *sql.Tx) error {
		rows, e := tx.Query(`SELECT name,address FROM upstreams ORDER BY name`)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var u Upstream
			if e = rows.Scan(&u.Name, &u.Address); e != nil {
				return e
			}
			out = append(out, u)
		}
		return rows.Err()
	})
	return
}

func (s *Store) GetUpstream(ctx context.Context, name string) (u Upstream, err error) {
	err = s.read(ctx, func(tx *sql.Tx) error {
		return notFound(tx.QueryRow(`SELECT name,address FROM upstreams WHERE name=?`, name).Scan(&u.Name, &u.Address))
	})
	return
}

// UpstreamLink: what one project (or one team's own notes: Team) sends to one upstream. Remote is the project slug or the team name there.
type UpstreamLink struct {
	Project     int64      `json:"project"` // a real project id, or a team's segment id when Team
	Team        bool       `json:"team"`    // a team's own notes (Project is the segment)
	Upstream    string     `json:"upstream"`
	Remote      string     `json:"remote"`
	Classes     []Class    `json:"classes"`
	WithPrivate bool       `json:"withPrivate"`
	LastPush    *time.Time `json:"lastPush,omitempty"`
	LastError   string     `json:"lastError,omitempty"`
}

// UpstreamLinkInfo is a link with what a status shows.
type UpstreamLinkInfo struct {
	UpstreamLink
	Label   string `json:"label"`   // the project's short name, or "team <name>"
	Pending int    `json:"pending"` // notes that a push would send now (new or changed)
	Total   int    `json:"total"`   // published notes the link covers
}

func parseClasses(csv string, withPrivate bool) ([]Class, error) {
	var out []Class
	seen := map[Class]bool{}
	for _, c := range strings.Split(csv, ",") {
		cl := Class(strings.TrimSpace(c))
		if cl != Structure && cl != Data && cl != Private || seen[cl] {
			return nil, ErrRefusal{"classes: structure, data or private, each once (comma-separated)"}
		}
		if cl == Private && !withPrivate {
			return nil, ErrRefusal{"private notes are sent only with --with-private on the link"}
		}
		seen[cl] = true
		out = append(out, cl)
	}
	if len(out) == 0 {
		return nil, ErrRefusal{"classes: structure, data or private, each once (comma-separated)"}
	}
	return out, nil
}

func classesCSV(cs []Class) string {
	var p []string
	for _, c := range cs {
		p = append(p, string(c))
	}
	return strings.Join(p, ",")
}

// LinkUpstream creates or changes a link. The local side is named by the owner: a real project, or (team > 0) a team. A project
// without a link sends nothing.
func (s *Store) LinkUpstream(ctx context.Context, project, team int64, upstream, remote, classes string, withPrivate bool) error {
	if classes == "" {
		classes = "structure,data"
		if withPrivate {
			classes += ",private"
		}
	}
	cls, err := parseClasses(classes, withPrivate)
	if err != nil {
		return err
	}
	if withPrivate && !contains(cls, Private) {
		return ErrRefusal{"--with-private names the private class: add it to --class (or leave --class out)"}
	}
	return s.adminWrite(ctx, "upstream.link", upstream, map[string]any{"project": project, "team": team, "remote": remote, "classes": classesCSV(cls), "withPrivate": withPrivate}, func(tx *sql.Tx) (string, error) {
		if e := tx.QueryRow(`SELECT name FROM upstreams WHERE name=?`, upstream).Scan(new(string)); e != nil {
			return "", notFound(e)
		}
		local := project
		if team > 0 {
			var archived bool
			var tname string
			if e := tx.QueryRow(`SELECT archived,name FROM teams WHERE id=?`, team).Scan(&archived, &tname); e != nil {
				return "", notFound(e)
			}
			if archived {
				return "", ErrRefusal{"team " + tname + " is archived: unarchive it first, then link it"}
			}
			local = segmentID(team)
		} else {
			if e := existsProject(tx, project); e != nil {
				return "", e
			}
			var archived bool
			var slug string
			if e := tx.QueryRow(`SELECT archived,slug FROM projects WHERE id=?`, project).Scan(&archived, &slug); e != nil {
				return "", e
			}
			if archived {
				return "", ErrRefusal{"project " + slug + " is archived: unarchive it first, then link it"}
			}
		}
		if remote == "" || len(remote) > 128 || strings.ContainsAny(remote, "/\\\r\n") {
			return "", ErrRefusal{"the name of the project (or team) on the upstream is empty or not a name"}
		}
		var n int
		if e := tx.QueryRow(`SELECT count(*) FROM upstream_links WHERE project_id=? AND upstream<>?`, local, upstream).Scan(&n); e != nil {
			return "", e
		}
		if n >= maxUpstreamLinks {
			return "", ErrRefusal{"at most 8 upstream links per project"}
		}
		t := 0
		if team > 0 {
			t = 1
		}
		p := 0
		if withPrivate {
			p = 1
		}
		_, e := tx.Exec(`INSERT INTO upstream_links(project_id,upstream,remote,remote_team,classes,with_private) VALUES(?,?,?,?,?,?)
ON CONFLICT(project_id,upstream) DO UPDATE SET remote=excluded.remote, remote_team=excluded.remote_team, classes=excluded.classes, with_private=excluded.with_private, last_error=''`, local, upstream, remote, t, classesCSV(cls), p)
		return "", e
	})
}

func contains(cs []Class, c Class) bool {
	for _, x := range cs {
		if x == c {
			return true
		}
	}
	return false
}

// UnlinkUpstream removes the link and its receipts (a later link starts from nothing: notes already there are sent again as new).
func (s *Store) UnlinkUpstream(ctx context.Context, project, team int64, upstream string) error {
	local := project
	if team > 0 {
		local = segmentID(team)
	}
	return s.adminWrite(ctx, "upstream.unlink", upstream, map[string]any{"project": local}, func(tx *sql.Tx) (string, error) {
		if _, e := tx.Exec(`DELETE FROM upstream_sent WHERE project_id=? AND upstream=?`, local, upstream); e != nil {
			return "", e
		}
		return "", countOne(tx.Exec(`DELETE FROM upstream_links WHERE project_id=? AND upstream=?`, local, upstream))
	})
}

// UpstreamNote is one local published note as a push sees it, with what was sent of it before.
type UpstreamNote struct {
	ID        int64
	Class     Class
	Title     string
	Body      string
	EntryKind string
	Revision  int64
	Author    string
	Hash      string
	Sent      *UpstreamSent
	Unchanged bool // sent before with this very content: nothing to send
}

type UpstreamSent struct {
	Hash       string
	Revision   int64
	RemoteSub  int64
	RemoteNote int64
	Sent       time.Time
}

func upstreamHash(n Note) string {
	h := sha256.Sum256([]byte(string(n.Class) + "\x00" + n.EntryKind + "\x00" + n.Title + "\x00" + n.Body))
	return hex.EncodeToString(h[:])
}

func linkLabel(tx *sql.Tx, project int64) (string, error) {
	if t, ok := SegmentTeam(project); ok {
		var name string
		err := tx.QueryRow(`SELECT name FROM teams WHERE id=?`, t).Scan(&name)
		return "team " + name, err
	}
	var slug string
	err := tx.QueryRow(`SELECT slug FROM projects WHERE id=?`, project).Scan(&slug)
	return slug, err
}

func scanLink(sc interface{ Scan(...any) error }) (l UpstreamLink, err error) {
	var cls string
	var t, p int
	var last int64
	if err = sc.Scan(&l.Project, &l.Upstream, &l.Remote, &t, &cls, &p, &last, &l.LastError); err != nil {
		return
	}
	l.Team, l.WithPrivate = t == 1, p == 1
	for _, c := range strings.Split(cls, ",") {
		l.Classes = append(l.Classes, Class(c))
	}
	if last > 0 {
		x := fromMs(last)
		l.LastPush = &x
	}
	return
}

const linkCols = `project_id,upstream,remote,remote_team,classes,with_private,last_push,last_error`

// upstreamScan is what upstreamNotes found.
type upstreamScan struct {
	Notes    []UpstreamNote // in id order, from the first note after the cursor: the ones to send (Body set) and the unchanged ones (Unchanged, no Body)
	Pending  int            // of Notes: how many are to send
	Total    int            // of Notes
	Overflow bool           // more notes to send follow the last of Notes (limit reached)
}

const scanPage = 200

// upstreamNotes: the published notes of the link's local side in its classes with an id above `after`, oldest first, with their receipts.
// Notes that were sent unchanged are passed over (they do not count against `limit`, the number of notes to SEND; 0 = no limit), so a run
// always reaches the notes that still need sending. It reads in pages, never the whole table at once.
func upstreamNotes(tx *sql.Tx, l UpstreamLink, after int64, limit int) (out upstreamScan, err error) {
	q := `SELECT ` + noteCols + ` FROM notes n JOIN users u ON u.id=n.author_id WHERE n.project_id=? AND n.id>? AND n.class IN (`
	base := []any{l.Project}
	var cls []any
	for i, c := range l.Classes {
		if i > 0 {
			q += ","
		}
		q += "?"
		cls = append(cls, string(c))
	}
	q += `) ORDER BY n.id LIMIT ` + strconv.Itoa(scanPage)
	for {
		args := append(append(append([]any{}, base...), after), cls...)
		rows, e := tx.Query(q, args...)
		if e != nil {
			return out, e
		}
		var page []Note
		for rows.Next() {
			n, e := scanNote(rows, true)
			if e != nil {
				rows.Close()
				return out, e
			}
			page = append(page, n)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return out, e
		}
		for _, n := range page {
			after = n.ID
			u := UpstreamNote{ID: n.ID, Class: n.Class, Title: n.Title, Body: n.Body, EntryKind: n.EntryKind, Revision: n.Revision, Author: n.Author, Hash: upstreamHash(n)}
			var sent UpstreamSent
			var at int64
			switch e := tx.QueryRow(`SELECT hash,revision,remote_sub,remote_note,sent FROM upstream_sent WHERE project_id=? AND upstream=? AND note_id=?`, l.Project, l.Upstream, n.ID).Scan(&sent.Hash, &sent.Revision, &sent.RemoteSub, &sent.RemoteNote, &at); e {
			case nil:
				sent.Sent = fromMs(at)
				u.Sent = &sent
			case sql.ErrNoRows:
			default:
				return out, e
			}
			if u.Sent != nil && u.Sent.Hash == u.Hash {
				u.Unchanged, u.Body = true, ""
			} else {
				if limit > 0 && out.Pending >= limit {
					out.Overflow = true
					return out, nil
				}
				out.Pending++
			}
			out.Total++
			out.Notes = append(out.Notes, u)
		}
		if len(page) < scanPage {
			return out, nil
		}
	}
}

// UpstreamLinks lists the links of one project or team segment (local = 0: all) with how many notes would go.
func (s *Store) UpstreamLinks(ctx context.Context, project, team int64) (out []UpstreamLinkInfo, err error) {
	local := project
	if team > 0 {
		local = segmentID(team)
	}
	err = s.read(ctx, func(tx *sql.Tx) error {
		q, args := `SELECT `+linkCols+` FROM upstream_links ORDER BY project_id,upstream`, []any{}
		if local > 0 {
			q, args = `SELECT `+linkCols+` FROM upstream_links WHERE project_id=? ORDER BY upstream`, []any{local}
		}
		rows, e := tx.Query(q, args...)
		if e != nil {
			return e
		}
		var ls []UpstreamLink
		for rows.Next() {
			l, e := scanLink(rows)
			if e != nil {
				rows.Close()
				return e
			}
			ls = append(ls, l)
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return e
		}
		for _, l := range ls {
			label, e := linkLabel(tx, l.Project)
			if e != nil {
				return e
			}
			scan, e := upstreamNotes(tx, l, 0, 0)
			if e != nil {
				return e
			}
			out = append(out, UpstreamLinkInfo{UpstreamLink: l, Label: label, Total: scan.Total, Pending: scan.Pending})
		}
		return nil
	})
	sort.SliceStable(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return
}

// upstreamPlan reads one link and its notes in one transaction.
func (s *Store) upstreamPlan(ctx context.Context, project int64, upstream string, after int64, limit int) (l UpstreamLink, label, address string, archived bool, scan upstreamScan, err error) {
	err = s.read(ctx, func(tx *sql.Tx) error {
		var e error
		if l, e = scanLink(tx.QueryRow(`SELECT `+linkCols+` FROM upstream_links WHERE project_id=? AND upstream=?`, project, upstream)); e != nil {
			return notFound(e)
		}
		if e := tx.QueryRow(`SELECT address FROM upstreams WHERE name=?`, upstream).Scan(&address); e != nil {
			return notFound(e)
		}
		if label, e = linkLabel(tx, project); e != nil {
			return e
		}
		if t, ok := SegmentTeam(project); ok {
			e = tx.QueryRow(`SELECT archived FROM teams WHERE id=?`, t).Scan(&archived)
		} else {
			e = tx.QueryRow(`SELECT archived FROM projects WHERE id=?`, project).Scan(&archived)
		}
		if e != nil {
			return e
		}
		scan, e = upstreamNotes(tx, l, after, limit)
		return e
	})
	return
}

func (s *Store) upstreamRecordSent(ctx context.Context, project int64, upstream string, n UpstreamNote, sub, remoteNote int64) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		_, e := tx.Exec(`INSERT INTO upstream_sent(project_id,upstream,note_id,hash,revision,remote_sub,remote_note,sent) VALUES(?,?,?,?,?,?,?,?)
ON CONFLICT(project_id,upstream,note_id) DO UPDATE SET hash=excluded.hash, revision=excluded.revision, remote_sub=excluded.remote_sub, remote_note=excluded.remote_note, sent=excluded.sent`,
			project, upstream, n.ID, n.Hash, n.Revision, sub, remoteNote, ms(s.now()))
		return e
	})
}

func (s *Store) upstreamRecordPush(ctx context.Context, project int64, upstream, lastErr string, ok bool) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		if ok {
			_, e := tx.Exec(`UPDATE upstream_links SET last_push=?, last_error='' WHERE project_id=? AND upstream=?`, ms(s.now()), project, upstream)
			return e
		}
		_, e := tx.Exec(`UPDATE upstream_links SET last_error=? WHERE project_id=? AND upstream=?`, lastErr, project, upstream)
		return e
	})
}
