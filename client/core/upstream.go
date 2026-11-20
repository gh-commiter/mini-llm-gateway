package core

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
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
CREATE TABLE upstream_key(id INTEGER PRIMARY KEY CHECK(id=1), k BLOB NOT NULL);
CREATE TABLE upstream_sent(project_id INTEGER NOT NULL, upstream TEXT NOT NULL, note_id INTEGER NOT NULL, hash TEXT NOT NULL, revision INTEGER NOT NULL, remote_sub INTEGER NOT NULL, remote_note INTEGER NOT NULL, sent INTEGER NOT NULL, PRIMARY KEY(project_id,upstream,note_id)) WITHOUT ROWID;`)
	return err
}

// migrate20: step B of the upstream gateway: the automatic push (an outbox per link: a flag with the time of the first unsent change and
// the time of the last one, the outcome of the last runs and the back-off), the mode of a link (submit, or publish: notes written on the
// upstream directly) and what publish needs to remember of an upstream note (its revision after our last write, a conflict).
func migrate20(tx *sql.Tx) error {
	_, err := tx.Exec(`ALTER TABLE upstream_links ADD COLUMN auto INTEGER NOT NULL DEFAULT 0 CHECK(auto IN(0,1));
ALTER TABLE upstream_links ADD COLUMN mode TEXT NOT NULL DEFAULT 'submit' CHECK(mode IN('submit','publish'));
ALTER TABLE upstream_links ADD COLUMN dirty_since INTEGER NOT NULL DEFAULT 0;
ALTER TABLE upstream_links ADD COLUMN last_change INTEGER NOT NULL DEFAULT 0;
ALTER TABLE upstream_links ADD COLUMN last_ok INTEGER NOT NULL DEFAULT 0;
ALTER TABLE upstream_links ADD COLUMN last_fail INTEGER NOT NULL DEFAULT 0;
ALTER TABLE upstream_links ADD COLUMN fail_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE upstream_links ADD COLUMN next_attempt INTEGER NOT NULL DEFAULT 0;
ALTER TABLE upstream_links ADD COLUMN push_cursor INTEGER NOT NULL DEFAULT 0; -- an automatic run that ended with "more": the last local note it dealt with
ALTER TABLE upstream_links ADD COLUMN last_audit TEXT NOT NULL DEFAULT ''; -- the outcome of the last run that wrote an audit row
ALTER TABLE upstream_sent ADD COLUMN remote_rev INTEGER NOT NULL DEFAULT 0;
ALTER TABLE upstream_sent ADD COLUMN conflict INTEGER NOT NULL DEFAULT 0 CHECK(conflict IN(0,1,2)); -- 1: changed on the upstream since our last write, 2: the owner chose "ours": send it as a submission
ALTER TABLE upstream_sent ADD COLUMN conflict_rev INTEGER NOT NULL DEFAULT 0;`)
	return err
}

type Upstream struct {
	Name    string `json:"name"`
	Address string `json:"address"`
}

// migrate23 restarts old cursors: they have no generation or remembered refusals.
func migrate23(tx *sql.Tx) error {
	_, err := tx.Exec(`ALTER TABLE upstream_links ADD COLUMN push_generation INTEGER NOT NULL DEFAULT 0;
ALTER TABLE upstream_links ADD COLUMN push_failed INTEGER NOT NULL DEFAULT 0 CHECK(push_failed IN(0,1));
UPDATE upstream_links SET push_cursor=0;`)
	return err
}

// migrate24: step C of the upstream gateway (reading the upstream's KB): per link whether it is read, what (classes, extra sources), when
// the owner accepted that reach and how the last pulls went; and the mirror itself, a bounded cache of what the upstream served. The
// mirror is not in `notes`: nothing here ever mixes with the local knowledge base.
func migrate24(tx *sql.Tx) error {
	_, err := tx.Exec(`ALTER TABLE upstream_links ADD COLUMN read INTEGER NOT NULL DEFAULT 0 CHECK(read IN(0,1));
ALTER TABLE upstream_links ADD COLUMN read_classes TEXT NOT NULL DEFAULT 'structure';
ALTER TABLE upstream_links ADD COLUMN read_sources TEXT NOT NULL DEFAULT '';
ALTER TABLE upstream_links ADD COLUMN read_accepted INTEGER NOT NULL DEFAULT 0;
ALTER TABLE upstream_links ADD COLUMN read_reach TEXT NOT NULL DEFAULT ''; -- what the owner accepted: upstream, remote project or team, classes, sources
ALTER TABLE upstream_links ADD COLUMN last_pull INTEGER NOT NULL DEFAULT 0;
ALTER TABLE upstream_links ADD COLUMN last_pull_ok INTEGER NOT NULL DEFAULT 0;
ALTER TABLE upstream_links ADD COLUMN pull_cursor INTEGER NOT NULL DEFAULT 0;
ALTER TABLE upstream_links ADD COLUMN pull_error TEXT NOT NULL DEFAULT '';
CREATE TABLE upstream_mirror(id INTEGER PRIMARY KEY AUTOINCREMENT, project_id INTEGER NOT NULL, upstream TEXT NOT NULL, source_id INTEGER NOT NULL, source_label TEXT NOT NULL, remote_note INTEGER NOT NULL, remote_rev INTEGER NOT NULL, class TEXT NOT NULL CHECK(class IN('structure','data')), kind TEXT NOT NULL CHECK(kind IN('finding','faq')), title TEXT NOT NULL, body TEXT NOT NULL, bytes INTEGER NOT NULL, updated INTEGER NOT NULL, fetched INTEGER NOT NULL, UNIQUE(project_id,upstream,remote_note));
CREATE VIRTUAL TABLE upstream_mirror_search USING fts5(title,body, tokenize='unicode61');`)
	return err
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
	mu := s.upstreamLock(name)
	if !mu.TryLock() {
		return errPushRunning{}
	}
	defer mu.Unlock()
	return s.adminWrite(ctx, "upstream.remove", name, nil, func(tx *sql.Tx) (string, error) {
		if _, e := tx.Exec(`DELETE FROM meta WHERE key IN (SELECT m.key FROM meta m JOIN upstream_links l ON substr(m.key,1,length('upstream_reconcile|'||l.project_id||'|'||l.upstream||'|'))='upstream_reconcile|'||l.project_id||'|'||l.upstream||'|' WHERE l.upstream=?)`, name); e != nil {
			return "", e
		}
		if e := purgeUpstreamMirror(tx, name); e != nil {
			return "", e
		}
		for _, q := range []string{`DELETE FROM meta WHERE key IN (SELECT '` + restoredPrefix + `'||project_id||'|'||upstream FROM upstream_links WHERE upstream=?)`, `DELETE FROM upstream_sent WHERE upstream=?`, `DELETE FROM upstream_links WHERE upstream=?`} {
			if _, e := tx.Exec(q, name); e != nil {
				return "", e
			}
		}
		return "", countOne(tx.Exec(`DELETE FROM upstreams WHERE name=?`, name))
	})
}

// migrate25: step C4 (the automatic pull): per link the owner's interval (seconds; 0 = the default of 15 minutes), the earliest time of the
// next pull (a successful run sets it one interval ahead, a failure the back-off), the failures in a row, the outcome of the last run that
// wrote an audit row, and the timer's own switch (off after the upstream withdrew the right to read: only the owner turns it on again).
func migrate25(tx *sql.Tx) error {
	_, err := tx.Exec(`ALTER TABLE upstream_links ADD COLUMN pull_every INTEGER NOT NULL DEFAULT 0;
ALTER TABLE upstream_links ADD COLUMN pull_fail_count INTEGER NOT NULL DEFAULT 0;
ALTER TABLE upstream_links ADD COLUMN pull_next INTEGER NOT NULL DEFAULT 0;
ALTER TABLE upstream_links ADD COLUMN last_pull_audit TEXT NOT NULL DEFAULT '';
ALTER TABLE upstream_links ADD COLUMN pull_timer INTEGER NOT NULL DEFAULT 1 CHECK(pull_timer IN(0,1));
ALTER TABLE upstream_links ADD COLUMN pull_failed INTEGER NOT NULL DEFAULT 0 CHECK(pull_failed IN(0,1)); -- a slice of the traversal in progress failed: the traversal never ends fresh
ALTER TABLE upstream_links ADD COLUMN pull_skipped INTEGER NOT NULL DEFAULT 0; -- notes the upstream answered about definitively (not served, invalid) in the last traversal
ALTER TABLE upstream_links ADD COLUMN pull_skipped_ids TEXT NOT NULL DEFAULT ''; -- the first 50 of them as id:reason (gone, refused, invalid, large), never upstream text
ALTER TABLE upstream_links ADD COLUMN pull_audit_n INTEGER NOT NULL DEFAULT 0; -- audit rows written in the current failure episode (at most 5)
ALTER TABLE upstream_sent ADD COLUMN sent_content TEXT NOT NULL DEFAULT ''; -- publish mode: hash of the content we sent (sentContentHash); '' = none recorded, revisions decide`)
	return err
}

// remote_rev on a receipt, no schema change: publish mode (remote_sub 0): the upstream note's revision after our last write. A receipt with a
// submission (remote_sub>0): the BASE, the upstream note's revision our last accepted text corresponds to (>= 1; learned from the accepted
// submission: the revision it declared + 1, or 1 for a new note; 0 = not known), or -1: the first submission is still pending (the wait for a
// review). A note whose upstream revision is above its base has changed there since our last version: a conflict. Only upstreamNotes counts waits.

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
	// Step B: the automatic push and its outbox. DirtySince is the first change not yet sent, LastChange the latest (the debounce counts
	// from it), NextAttempt the earliest time of the next automatic attempt (the back-off after a failure).
	Auto        bool       `json:"auto"`
	Mode        string     `json:"mode"` // submit or publish
	DirtySince  *time.Time `json:"dirtySince,omitempty"`
	LastChange  *time.Time `json:"lastChange,omitempty"`
	LastOK      *time.Time `json:"lastOk,omitempty"`
	LastFail    *time.Time `json:"lastFail,omitempty"`
	FailCount   int        `json:"failCount,omitempty"`
	NextAttempt *time.Time `json:"nextAttempt,omitempty"`
}

// NotPropagated is a note that was sent and has since been deleted or made private here: neither is propagated (the copy stays on the
// upstream until removed there), so the status lists them for the owner to act on.
type NotPropagated struct {
	Note int64  `json:"note"`
	Why  string `json:"why"` // "deleted here" or "now private here"
}

const maxNotPropagated = 50

// UpstreamConflict: Gone = the upstream note is deleted or no longer visible; RemoteRev is the revision seen there. State 2 (resolved "ours") is not
// listed: it is waiting to be sent.
type UpstreamConflict struct {
	Note      int64 `json:"note"`
	RemoteRev int64 `json:"remoteRev"`
	Gone      bool  `json:"gone,omitempty"`
	// Submission: the earlier submission can no longer be read there, so it is not known whether it became a note
	Submission bool `json:"submission,omitempty"`
}

// UpstreamLinkInfo is a link with what a status shows.
type UpstreamLinkInfo struct {
	UpstreamLink
	NotPropagated []NotPropagated `json:"notPropagated,omitempty"`
	// Conflicts: publish mode notes that changed (or were deleted) on the upstream since our last write; they are not overwritten.
	Conflicts []UpstreamConflict `json:"conflicts,omitempty"`
	// Waiting: notes whose earlier submission is still pending upstream: nothing more is sent for them until it is reviewed (not a failure).
	Waiting int `json:"waiting,omitempty"`
	// Restored: since when the link is marked "restored: reconcile before pushing" (see upstream_restore.go): it sends nothing until cleared.
	ReconcileCounts *ReconcileCounts `json:"reconcileCounts,omitempty"`
	Restored        *time.Time       `json:"restored,omitempty"`
	Label           string           `json:"label"`   // the project's short name, or "team <name>"
	Pending         int              `json:"pending"` // notes that a push would send now (new or changed)
	Total           int              `json:"total"`   // published notes the link covers
	// Foreign: published notes that arrived from another gateway (they end with its trailer): never pushed onward, not in Total.
	Foreign int `json:"foreign,omitempty"`
	// Step C: reading the upstream's notes into the mirror (nothing of it is shown to members yet).
	Read         bool       `json:"read"`
	ReadClasses  []Class    `json:"readClasses,omitempty"`
	ReadSources  []string   `json:"readSources,omitempty"` // labels (project:<slug>, team:<name>) beside the linked project itself
	ReadAccepted *time.Time `json:"readAccepted,omitempty"`
	LastPull     *time.Time `json:"lastPull,omitempty"`    // the last try
	LastPullOK   *time.Time `json:"lastPullOk,omitempty"`  // the last pull that went through to the end
	LastPullAge  int        `json:"lastPullAge,omitempty"` // seconds since then, by this gateway's clock
	PullError    string     `json:"pullError,omitempty"`
	Mirror       int        `json:"mirror"`          // notes in the mirror of this link
	MirrorBytes  int64      `json:"mirrorBytes"`     // their size (titles and bodies)
	Stale        bool       `json:"stale,omitempty"` // no complete pull for 7 days (or none yet): members do not see the mirror
	// Step C4, the automatic pull: the interval in seconds (the default when the owner set none), the next attempt, the failures in a row,
	// and whether the timer was switched off by a refusal of the upstream (only a new acceptance of the reach, or a pull that went through, switches it on).
	PullEvery     int        `json:"pullEvery"`
	PullNext      *time.Time `json:"pullNext,omitempty"`
	PullFailCount int        `json:"pullFailCount,omitempty"`
	PullTimerOff  bool       `json:"pullTimerOff,omitempty"`
	// RevisionBackwards: local notes this link published whose copy on the upstream has a LOWER revision than the one our push recorded
	// (the upstream's copy was replaced by an older one: a revision number is no proof of the text, so such a copy is judged by its content).
	RevisionBackwards []int64 `json:"revisionBackwards,omitempty"`
	// SkippedNotes: notes of the last traversal that the upstream answered about definitively (not served, an answer that failed the checks)
	// and that were therefore not taken: they do not make the mirror stale; SkippedList names the first 50 as "id:reason".
	SkippedNotes int      `json:"skippedNotes,omitempty"`
	SkippedList  []string `json:"skippedList,omitempty"`
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
	return s.LinkUpstreamWith(ctx, LinkSpec{Project: project, Team: team, Upstream: upstream, Remote: remote, Classes: classes, WithPrivate: withPrivate})
}

// LinkSpec is what an owner's `upstream link` says. Auto nil and Mode "" keep what the link has (a new link: manual, submit).
type LinkSpec struct {
	Project, Team   int64
	Upstream        string
	Remote, Classes string
	WithPrivate     bool
	Auto            *bool
	Mode            string
	// Step C: Read nil keeps what the link has; true (or any of the two lists) needs AcceptReach: the owner has seen what the upstream's
	// user may read (pull --dry-run) and accepts it for this link. false turns reading off and deletes the mirror. ReadOnly: an existing
	// link keeps its push settings (remote, classes, private, auto, mode) untouched.
	Read                  *bool
	ReadClasses           string
	ReadSources           *string // nil keeps the sources the link reads; a pointer to "" means NO extra sources
	AcceptReach, ReadOnly bool
	// PullEvery: the interval of the automatic pull in seconds (MinPullEvery to MaxPullEvery); nil keeps it. It is not part of the reach.
	PullEvery *int
}

func (s *Store) LinkUpstreamWith(ctx context.Context, sp LinkSpec) error {
	_, err := s.LinkUpstreamReport(ctx, sp)
	return err
}

// LinkUpstreamReport is LinkUpstreamWith and says whether it turned reading off because the link now reaches something else than what the
// owner accepted (the remote project or team changed without a new --read ... --accept-reach): the mirror is then deleted.
func (s *Store) LinkUpstreamReport(ctx context.Context, sp LinkSpec) (turnedOff bool, err error) {
	project, team, upstream, remote, classes, withPrivate := sp.Project, sp.Team, sp.Upstream, sp.Remote, sp.Classes, sp.WithPrivate
	if sp.Mode != "" && sp.Mode != "submit" && sp.Mode != "publish" {
		return false, ErrRefusal{"mode: submit or publish"}
	}
	if classes == "" {
		classes = "structure,data"
		if withPrivate {
			classes += ",private"
		}
	}
	cls, err := parseClasses(classes, withPrivate)
	if err != nil {
		return false, err
	}
	if withPrivate && !contains(cls, Private) {
		return false, ErrRefusal{"--with-private names the private class: add it to --class (or leave --class out)"}
	}
	if sp.PullEvery != nil && (*sp.PullEvery < MinPullEvery || *sp.PullEvery > MaxPullEvery) {
		return false, ErrRefusal{fmt.Sprintf("pull interval: %d minutes to %d hours", MinPullEvery/60, MaxPullEvery/3600)}
	}
	readOn, readOff := sp.Read != nil && *sp.Read, sp.Read != nil && !*sp.Read
	readLists := sp.ReadClasses != "" || sp.ReadSources != nil
	var readCls string
	var readSrc *string
	switch {
	case readOff && readLists:
		return false, ErrRefusal{"--no-read with --read-class or --read-sources: choose one"}
	case (readOn || readLists) && !sp.AcceptReach:
		return false, ErrRefusal{"reading gives the members of this project what the upstream's user may read there: look first (minigw gw upstream pull <project> --dry-run on a link without --read), then repeat with --accept-reach"}
	}
	if sp.ReadClasses != "" {
		if strings.Contains(","+strings.ReplaceAll(sp.ReadClasses, " ", "")+",", ",private,") {
			return false, ErrRefusal{"private notes are never read from an upstream"}
		}
		rc, err := parseClasses(sp.ReadClasses, false)
		if err != nil {
			return false, err
		}
		readCls = classesCSV(rc)
	}
	if sp.ReadSources != nil {
		csv := ""
		if *sp.ReadSources != "" {
			var err error
			if csv, err = parseReadSources(*sp.ReadSources); err != nil {
				return false, err
			}
		}
		readSrc = &csv
	}
	detail := map[string]any{"pullEvery": sp.PullEvery, "project": project, "team": team, "remote": remote, "classes": classesCSV(cls), "withPrivate": withPrivate, "auto": sp.Auto, "mode": sp.Mode, "read": sp.Read, "readClasses": readCls, "readSources": readSrc, "readOnly": sp.ReadOnly}
	// A link update never overlaps a running push or pull of the link (they read the link's reach and acceptance once, under this lock):
	// it is refused, as an unlink is, and the owner tries again.
	lockID := project
	if team > 0 {
		lockID = segmentID(team)
	}
	mu := s.linkLock(lockID, upstream)
	if !mu.TryLock() {
		return false, errPushRunning{}
	}
	defer mu.Unlock()
	err = s.adminWrite(ctx, "upstream.link", upstream, detail, func(tx *sql.Tx) (string, error) {
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
		if team == 0 && (!slugRe.MatchString(remote) || strings.Trim(remote, "0123456789") == "") { // it goes into a path: a short name as the gateways make them
			return "", ErrRefusal{"the project's short name on the upstream: lowercase letters, digits and -, 2 to 63 characters"}
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
		if sp.Mode != "" {
			var cur string
			var n int
			switch e := tx.QueryRow(`SELECT mode,(SELECT count(*) FROM upstream_sent WHERE project_id=?1 AND upstream=?2) FROM upstream_links WHERE project_id=?1 AND upstream=?2`, local, upstream).Scan(&cur, &n); e {
			case nil:
				if cur != sp.Mode && n > 0 { // the receipts of one mode mean nothing in the other (a submission is not a note)
					return "", ErrRefusal{"the link already sent notes in mode " + cur + ": unlink it and link it again to change the mode (what was sent stays on the upstream)"}
				}
			case sql.ErrNoRows:
			default:
				return "", e
			}
		}
		var have int
		if e := tx.QueryRow(`SELECT count(*) FROM upstream_links WHERE project_id=? AND upstream=?`, local, upstream).Scan(&have); e != nil {
			return "", e
		}
		if sp.ReadOnly && have > 0 { // only the reading changes: the push settings of the link stay as they are
			if e := setPullEvery(tx, s.now(), local, upstream, sp.PullEvery); e != nil {
				return "", e
			}
			return "", setLinkRead(tx, s.now(), local, upstream, sp, readOn, readOff, readCls, readSrc, detail)
		}
		var curRemote string
		var curTeam, sentN int
		switch e := tx.QueryRow(`SELECT remote,remote_team,(SELECT count(*) FROM upstream_sent WHERE project_id=?1 AND upstream=?2) FROM upstream_links WHERE project_id=?1 AND upstream=?2`, local, upstream).Scan(&curRemote, &curTeam, &sentN); e {
		case nil:
			if (curRemote != remote || curTeam != t) && sentN > 0 { // the remembered submission and note numbers are those of the old destination
				return "", ErrRefusal{"the link already sent notes to " + curRemote + ": unlink it and link it again to change the destination (what was sent stays on the upstream)"}
			}
		case sql.ErrNoRows:
		default:
			return "", e
		}
		var au any
		if sp.Auto != nil {
			au = 0
			if *sp.Auto {
				au = 1
			}
		}
		_, e := tx.Exec(`INSERT INTO upstream_links(project_id,upstream,remote,remote_team,classes,with_private,auto,mode) VALUES(?,?,?,?,?,?,COALESCE(?,0),CASE WHEN ?='' THEN 'submit' ELSE ? END)
ON CONFLICT(project_id,upstream) DO UPDATE SET remote=excluded.remote, remote_team=excluded.remote_team, classes=excluded.classes, with_private=excluded.with_private, last_error='', auto=COALESCE(?,auto), mode=CASE WHEN ?='' THEN mode ELSE ? END`,
			local, upstream, remote, t, classesCSV(cls), p, au, sp.Mode, sp.Mode, au, sp.Mode, sp.Mode)
		if e != nil {
			return "", e
		}
		if sp.Auto != nil && *sp.Auto { // switching it on: what is already unsent goes with the next push after the debounce
			_, e = tx.Exec(`UPDATE upstream_links SET dirty_since=CASE WHEN dirty_since=0 THEN ?1 ELSE dirty_since END, last_change=max(?1,last_change+1) WHERE project_id=?2 AND upstream=?3`, ms(s.now()), local, upstream)
		} else if sp.Auto != nil { // switching it off: no outbox, no back-off
			_, e = tx.Exec(`UPDATE upstream_links SET dirty_since=0, last_change=0, fail_count=0, next_attempt=0, push_cursor=0, push_generation=0, push_failed=0 WHERE project_id=? AND upstream=?`, local, upstream)
		}
		if e != nil {
			return "", e
		}
		if have > 0 { // a new link reads nothing yet
			if turnedOff, e = enforceReach(tx, local, upstream, detail); e != nil { // before the new acceptance, which may follow
				return "", e
			}
		}
		if e := setPullEvery(tx, s.now(), local, upstream, sp.PullEvery); e != nil {
			return "", e
		}
		return "", setLinkRead(tx, s.now(), local, upstream, sp, readOn, readOff, readCls, readSrc, detail)
	})
	if readOn || sp.ReadClasses != "" || sp.ReadSources != nil { // the same command accepted the new reach: reading is on again
		turnedOff = false
	}
	return turnedOff, err
}

// reachOf is what an acceptance covers: the upstream, the remote project or team (and which of the two), the classes and the sources.
func reachOf(upstream, remote string, remoteTeam int, classes, sources string) string {
	return fmt.Sprintf("%s|%s|%d|%s|%s", upstream, remote, remoteTeam, classes, sources)
}

// enforceReach: reading stays on only while the link reaches exactly what the owner accepted. A link that now points at another remote
// project or team (both CLIs default the remote to the local name, so a plain `link <project> <name> --auto` can do that) loses its
// reading and its mirror; the owner accepts the new reach with --read --accept-reach.
func enforceReach(tx *sql.Tx, project int64, upstream string, detail map[string]any) (bool, error) {
	var remote, cls, src, reach string
	var team, on int
	if e := tx.QueryRow(`SELECT remote,remote_team,read,read_classes,read_sources,read_reach FROM upstream_links WHERE project_id=? AND upstream=?`, project, upstream).Scan(&remote, &team, &on, &cls, &src, &reach); e != nil {
		return false, e
	}
	if on == 0 || reach == reachOf(upstream, remote, team, cls, src) {
		return false, nil
	}
	n, e := purgeMirror(tx, project, upstream)
	if e != nil {
		return false, e
	}
	detail["readTurnedOff"] = map[string]any{"accepted": reach, "now": reachOf(upstream, remote, team, cls, src), "mirrorNotes": n}
	_, e = tx.Exec(`UPDATE upstream_links SET read=0,read_classes='structure',read_sources='',read_accepted=0,read_reach='',pull_cursor=0,pull_error='',last_pull=0,last_pull_ok=0,pull_timer=1,pull_fail_count=0,pull_next=0,last_pull_audit='',pull_failed=0,pull_audit_n=0,pull_skipped=0,pull_skipped_ids='' WHERE project_id=? AND upstream=?`, project, upstream)
	return e == nil, e
}

// setLinkRead applies the reading part of a link spec. Any change of what is read (on, off, classes, sources) deletes the mirror of the
// link in the same transaction: what stays is never more than what the owner has accepted. The acceptance stores the exact reach and the
// audit row names it (the effective classes and sources, not only the flags that were given).
func setLinkRead(tx *sql.Tx, now time.Time, project int64, upstream string, sp LinkSpec, readOn, readOff bool, classes string, sourcesArg *string, detail map[string]any) error {
	if sp.Read == nil && sp.ReadClasses == "" && sp.ReadSources == nil {
		return nil
	}
	var cur, team int
	var curC, curS, remote string
	if e := tx.QueryRow(`SELECT read,read_classes,read_sources,remote,remote_team FROM upstream_links WHERE project_id=? AND upstream=?`, project, upstream).Scan(&cur, &curC, &curS, &remote, &team); e != nil {
		return e
	}
	on := !readOff
	if classes == "" {
		classes = curC
	}
	sources := curS
	if sourcesArg != nil { // explicit, also when empty: NO extra sources
		sources = *sourcesArg
	}
	if !on { // turning reading off forgets what was read: turning it on again starts from the default reach (structure, no extra sources)
		classes, sources = "structure", ""
	}
	reach := ""
	if on {
		reach = reachOf(upstream, remote, team, classes, sources)
		detail["readAccepted"] = map[string]any{"reach": reach, "classes": classes, "sources": sources, "remote": remote, "remoteTeam": team == 1}
	}
	if on == (cur == 1) && classes == curC && sources == curS {
		if on { // accepted again: the timer is on again too (a refusal of the upstream had switched it off)
			_, e := tx.Exec(`UPDATE upstream_links SET read_accepted=?,read_reach=?,pull_timer=1,pull_fail_count=0,pull_next=0 WHERE project_id=? AND upstream=?`, ms(now), reach, project, upstream)
			return e
		}
		return nil
	}
	if _, e := purgeMirror(tx, project, upstream); e != nil {
		return e
	}
	accepted, o := int64(0), 0
	if on {
		accepted, o = ms(now), 1
	}
	_, e := tx.Exec(`UPDATE upstream_links SET read=?,read_classes=?,read_sources=?,read_accepted=?,read_reach=?,pull_cursor=0,pull_error='',last_pull=0,last_pull_ok=0,pull_timer=1,pull_fail_count=0,pull_next=0,last_pull_audit='',pull_failed=0,pull_audit_n=0,pull_skipped=0,pull_skipped_ids='' WHERE project_id=? AND upstream=?`, o, classes, sources, accepted, reach, project, upstream)
	return e
}

// The interval of the automatic pull: 5 minutes to 24 hours, 15 minutes when the owner set none.
const (
	MinPullEvery     = 5 * 60
	MaxPullEvery     = 24 * 3600
	DefaultPullEvery = 15 * 60
)

// setPullEvery stores the owner's interval of one link (nil: unchanged). A shorter interval brings the next pull forward (never a failure's
// back-off), a longer one leaves it.
func setPullEvery(tx *sql.Tx, now time.Time, project int64, upstream string, every *int) error {
	if every == nil {
		return nil
	}
	_, e := tx.Exec(`UPDATE upstream_links SET pull_every=?1, pull_next=CASE WHEN pull_fail_count=0 AND pull_next>?2+?1*1000 THEN ?2+?1*1000 ELSE pull_next END WHERE project_id=?3 AND upstream=?4`, *every, ms(now), project, upstream)
	return e
}

var readSourceRe = regexp.MustCompile(`^(project|team):.+$`)

// parseReadSources: the labels of extra sources (project:<slug> or team:<name>, as the upstream's manifest names them), at most 16.
func parseReadSources(csv string) (string, error) {
	bad := ErrRefusal{"--read-sources: labels as pull --dry-run shows them (project:<slug> or team:<name>), comma-separated, at most 16"}
	var out []string
	seen := map[string]bool{}
	for _, x := range strings.Split(csv, ",") {
		x = strings.TrimSpace(x)
		if len(x) > 80 || seen[x] || !readSourceRe.MatchString(x) || strings.ContainsFunc(x, unicode.IsControl) || upText(x, 80) != x {
			return "", bad
		}
		seen[x] = true
		out = append(out, x)
	}
	if len(out) > 16 {
		return "", bad
	}
	return strings.Join(out, ","), nil
}

// purgeMirror deletes the mirror of one link (rows and search postings); purgeUpstreamMirror that of every link of an upstream.
func purgeMirror(tx *sql.Tx, project int64, upstream string) (int64, error) {
	if e := deleteMirrorSearch(tx, `project_id=? AND upstream=?`, project, upstream); e != nil {
		return 0, e
	}
	r, e := tx.Exec(`DELETE FROM upstream_mirror WHERE project_id=? AND upstream=?`, project, upstream)
	if e != nil {
		return 0, e
	}
	return r.RowsAffected()
}

func purgeUpstreamMirror(tx *sql.Tx, upstream string) error {
	if e := deleteMirrorSearch(tx, `upstream=?`, upstream); e != nil {
		return e
	}
	_, e := tx.Exec(`DELETE FROM upstream_mirror WHERE upstream=?`, upstream)
	return e
}

// deleteMirrorSearch removes the search postings of the mirror rows matching where. A few rows are deleted one by one with FTS5's
// secure-delete on (the usual guarantee); a bulk delete (a link of thousands of notes) runs with it off around the delete and merges
// afterwards (ftsBulk), because with it on such a delete is quadratic (3000 rows: 5 s instead of well under 1 s).
func deleteMirrorSearch(tx *sql.Tx, where string, args ...any) error {
	var n int
	if err := tx.QueryRow(`SELECT count(*) FROM upstream_mirror WHERE `+where, args...).Scan(&n); err != nil {
		return err
	}
	del := func() error {
		_, e := tx.Exec(`DELETE FROM upstream_mirror_search WHERE rowid IN (SELECT id FROM upstream_mirror WHERE `+where+`)`, args...)
		return e
	}
	if n < bulkMirrorRows {
		return del()
	}
	return ftsBulk(tx, "upstream_mirror_search", del)
}

// bulkMirrorRows: from this many rows a mirror delete takes the bulk path (the per-row path is as fast below it).
const bulkMirrorRows = 200

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
	mu := s.linkLock(local, upstream)
	if !mu.TryLock() {
		return errPushRunning{}
	}
	defer mu.Unlock()
	return s.adminWrite(ctx, "upstream.unlink", upstream, map[string]any{"project": local}, func(tx *sql.Tx) (string, error) {
		if _, e := tx.Exec(`DELETE FROM upstream_sent WHERE project_id=? AND upstream=?`, local, upstream); e != nil {
			return "", e
		}
		prefix := reconcileKey(local, upstream, "")
		if _, e := tx.Exec(`DELETE FROM meta WHERE substr(key,1,?)=?`, len(prefix), prefix); e != nil {
			return "", e
		}
		if _, e := tx.Exec(`DELETE FROM meta WHERE key=?`, restoredKey(local, upstream)); e != nil {
			return "", e
		}
		if _, e := purgeMirror(tx, local, upstream); e != nil {
			return "", e
		}
		return "", countOne(tx.Exec(`DELETE FROM upstream_links WHERE project_id=? AND upstream=?`, local, upstream))
	})
}

// UpstreamNote is one local published note as a push sees it, with what was sent of it before.
type UpstreamNote struct {
	ID               int64
	Class            Class
	Title            string
	Body             string
	EntryKind        string
	Revision         int64
	Author           string
	Hash             string
	Sent             *UpstreamSent
	Unchanged        bool // sent before with this very content: nothing to send
	Conflicted       bool // publish mode: the upstream note changed there since our last write; waits for `upstream resolve`
	ReconcileSkipped bool // owner skipped this revision independently of receipts
	Foreign          bool // arrived from another gateway (see fromGateway): never sent onward, nothing to send
}

type UpstreamSent struct {
	Hash       string
	Revision   int64
	RemoteSub  int64
	RemoteNote int64
	Sent       time.Time
	// publish mode: the revision of the upstream note after our last write, and a conflict (1: it changed there since; 2: "ours" was chosen)
	RemoteRev   int64
	Conflict    int
	ConflictRev int64
	Waiting     bool // waits for the review of the earlier submission upstream
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
	var t, p, auto int
	var last, dirty, change, ok, fail, next int64
	if err = sc.Scan(&l.Project, &l.Upstream, &l.Remote, &t, &cls, &p, &last, &l.LastError, &auto, &l.Mode, &dirty, &change, &ok, &fail, &l.FailCount, &next); err != nil {
		return
	}
	l.Team, l.WithPrivate, l.Auto = t == 1, p == 1, auto == 1
	ptr := func(ms int64) *time.Time {
		if ms <= 0 {
			return nil
		}
		x := fromMs(ms)
		return &x
	}
	l.DirtySince, l.LastChange, l.LastOK, l.LastFail, l.NextAttempt = ptr(dirty), ptr(change), ptr(ok), ptr(fail), ptr(next)
	for _, c := range strings.Split(cls, ",") {
		l.Classes = append(l.Classes, Class(c))
	}
	if last > 0 {
		x := fromMs(last)
		l.LastPush = &x
	}
	return
}

const linkCols = `project_id,upstream,remote,remote_team,classes,with_private,last_push,last_error,auto,mode,dirty_since,last_change,last_ok,last_fail,fail_count,next_attempt`

// upstreamScan is what upstreamNotes found.
type upstreamScan struct {
	Notes    []UpstreamNote // in id order, from the first note after the cursor: the ones to send (Body set) and the unchanged ones (Unchanged, no Body)
	Pending  int            // of Notes: how many are to send
	Waiting  int            // of those: how many wait for the review of their earlier submission upstream (only notes the link would push now)
	Total    int            // of Notes, without the Foreign ones
	Foreign  int            // of Notes: arrived from another gateway, never sent
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
			if fromGateway(n.Body) {
				u.Foreign, u.Body = true, ""
				out.Foreign++
				out.Notes = append(out.Notes, u)
				continue
			}
			var sent UpstreamSent
			var at int64
			switch e := tx.QueryRow(`SELECT hash,revision,remote_sub,remote_note,sent,remote_rev,conflict,conflict_rev FROM upstream_sent WHERE project_id=? AND upstream=? AND note_id=?`, l.Project, l.Upstream, n.ID).Scan(&sent.Hash, &sent.Revision, &sent.RemoteSub, &sent.RemoteNote, &at, &sent.RemoteRev, &sent.Conflict, &sent.ConflictRev); e {
			case nil:
				sent.Sent = fromMs(at)
				sent.Waiting = sent.RemoteSub > 0 && sent.RemoteRev == -1
				u.Sent = &sent
			case sql.ErrNoRows:
			default:
				return out, e
			}
			var decision string
			var d reconcileDecision
			if e := tx.QueryRow(`SELECT value FROM meta WHERE key=?`, reconcileDecisionKey(l.Project, l.Upstream, n.ID)).Scan(&decision); e != nil && e != sql.ErrNoRows {
				return out, e
			}
			if json.Unmarshal([]byte(decision), &d) == nil && d.Action == "skip" && d.Revision == n.Revision && d.Hash == u.Hash {
				u.ReconcileSkipped, u.Unchanged = true, true
			} else if u.Sent != nil && u.Sent.Conflict == 1 { // changed on the upstream: waits for the owner (resolve), is not "to send"
				u.Conflicted, u.Body = true, ""
			} else if u.Sent != nil && u.Sent.Hash == u.Hash && u.Sent.Conflict == 0 {
				u.Unchanged, u.Body = true, ""
			} else {
				if limit > 0 && out.Pending >= limit {
					out.Overflow = true
					return out, nil
				}
				out.Pending++
				if u.Sent != nil && u.Sent.Waiting {
					out.Waiting++
				}
			}
			out.Total++
			out.Notes = append(out.Notes, u)
		}
		if len(page) < scanPage {
			return out, nil
		}
	}
}

// fromGateway: the note arrived from another gateway: its text ends with the trailer every push writes (create, update and submission
// alike, so an accepted submission too). Such a note is never pushed onward, by an automatic or a manual push, in either mode: not back
// to the gateway it came from (that would copy it back and forth for ever) and not to any other upstream (a relay its origin never chose).
// Only a write without the trailer at the end (a member who takes the text over and removes that line) makes it local work again; its
// copy elsewhere then ends with OUR trailer, so no copy can travel more hops than people made edits. The text is the signal because it is
// all both sides have: the receiving gateway records no "this user is a gateway" (a device's "gateway, minigw" description is what the
// device says of itself). A member who ends a note with a trailer by hand only keeps a note they may change or delete anyway from going.
func fromGateway(body string) bool { return looksFromGateway.MatchString(body) }

// looksFromGateway is SUSPICION, as trailerExactAtEnd (upstream_push.go) is proof: relay protection must err toward "this came from a gateway", so
// that anything a gateway or a version writes or stores slightly differently (a normaliser, an older format, a future field, text added on that
// line) does not turn a received copy into local work that could travel on and loop between automatic links. A note whose LAST line has the
// opening mark of the trailer and ends with the closing is not pushed onward, however the line continues in between or after the mark.
var looksFromGateway = regexp.MustCompile(`<!-- minigw upstream ref ([0-9a-f]{12}), author [^\n]*-->\n?$`)

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
			info := UpstreamLinkInfo{UpstreamLink: l, Label: label, Total: scan.Total, Pending: scan.Pending, Foreign: scan.Foreign, Waiting: scan.Waiting}
			if at, ok, e := restoredAt(tx, l.Project, l.Upstream); e != nil {
				return e
			} else if ok {
				info.Restored = &at
				info.ReconcileCounts = reconcileCounts(tx, l)
			}
			if e := readInfo(tx, ms(s.now()), &info); e != nil {
				return e
			}
			// notes that went and are no longer here as they were: deleted, or now private (and not covered by the link): the copy on the
			// upstream stays until the owner removes it there
			nr, e := tx.Query(`SELECT s.note_id, n.id IS NULL, coalesce(n.class,'') FROM upstream_sent s LEFT JOIN notes n ON n.id=s.note_id AND n.project_id=s.project_id WHERE s.project_id=? AND s.upstream=? ORDER BY s.note_id`, l.Project, l.Upstream)
			if e != nil {
				return e
			}
			for nr.Next() && len(info.NotPropagated) < maxNotPropagated {
				var id int64
				var gone bool
				var cl string
				if e = nr.Scan(&id, &gone, &cl); e != nil {
					nr.Close()
					return e
				}
				switch {
				case gone:
					info.NotPropagated = append(info.NotPropagated, NotPropagated{id, "deleted here"})
				case Class(cl) == Private && !contains(l.Classes, Private):
					info.NotPropagated = append(info.NotPropagated, NotPropagated{id, "now private here"})
				}
			}
			nr.Close()
			cr, e := tx.Query(`SELECT note_id, conflict_rev FROM upstream_sent WHERE project_id=? AND upstream=? AND conflict=1 ORDER BY note_id LIMIT ?`, l.Project, l.Upstream, maxNotPropagated)
			if e != nil {
				return e
			}
			for cr.Next() {
				var c UpstreamConflict
				if e = cr.Scan(&c.Note, &c.RemoteRev); e != nil {
					cr.Close()
					return e
				}
				c.Gone, c.Submission = c.RemoteRev == 0, c.RemoteRev < 0
				info.Conflicts = append(info.Conflicts, c)
			}
			cr.Close()
			out = append(out, info)
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
		// a submission leaves no conflict; the base (remote_rev, see upstreamRecordBase) of a replacement of a known note stays: it is the upstream
		// revision our last ACCEPTED text corresponds to, and a pending or rejected replacement does not change it
		_, e := tx.Exec(`INSERT INTO upstream_sent(project_id,upstream,note_id,hash,revision,remote_sub,remote_note,sent) VALUES(?,?,?,?,?,?,?,?)
ON CONFLICT(project_id,upstream,note_id) DO UPDATE SET hash=excluded.hash, revision=excluded.revision, remote_sub=excluded.remote_sub, remote_note=excluded.remote_note, sent=excluded.sent, remote_rev=CASE WHEN excluded.remote_note>0 AND upstream_sent.remote_rev>0 THEN upstream_sent.remote_rev ELSE 0 END, sent_content='', conflict=0, conflict_rev=0`,
			project, upstream, n.ID, n.Hash, n.Revision, sub, remoteNote, ms(s.now()))
		return e
	})
}

// upstreamRecordPublished: a note written on the upstream directly (publish mode): its id and the revision it has after our write, the
// baseline the next update compares with.
func (s *Store) upstreamRecordPublished(ctx context.Context, project int64, upstream string, n UpstreamNote, remoteNote, remoteRev int64) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		_, e := tx.Exec(`INSERT INTO upstream_sent(project_id,upstream,note_id,hash,revision,remote_sub,remote_note,sent,remote_rev,sent_content) VALUES(?,?,?,?,?,0,?,?,?,?)
ON CONFLICT(project_id,upstream,note_id) DO UPDATE SET hash=excluded.hash, revision=excluded.revision, remote_sub=0, remote_note=excluded.remote_note, sent=excluded.sent, remote_rev=excluded.remote_rev, sent_content=excluded.sent_content, conflict=0, conflict_rev=0`,
			project, upstream, n.ID, n.Hash, n.Revision, remoteNote, ms(s.now()), remoteRev, sentContentHash(n.Class, n.EntryKind, n.Title, n.Body))
		return e
	})
}

// upstreamRecordConflict: the upstream note changed (or is gone: rev 0) since our last write; it is not overwritten.
func (s *Store) upstreamRecordConflict(ctx context.Context, project int64, upstream string, note, rev int64) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		_, e := tx.Exec(`UPDATE upstream_sent SET conflict=1, conflict_rev=?, remote_rev=CASE WHEN remote_sub>0 AND remote_rev=-1 THEN 0 ELSE remote_rev END WHERE project_id=? AND upstream=? AND note_id=?`, rev, project, upstream, note)
		return e
	})
}

// ResolveUpstream settles one link's conflict. Without an upstream, exactly one conflict must exist. "theirs": our change is forgotten for the revision on
// the upstream (the receipt takes the note as it is here now and the upstream's revision as the baseline); "ours": the next push sends our
// version as a SUBMISSION for the upstream's reviewers (never an overwrite).
func (s *Store) ResolveUpstream(ctx context.Context, project, team, note int64, ours bool, upstream ...string) error {
	up := ""
	if len(upstream) > 0 {
		up = upstream[0]
	}
	if up != "" && !ValidUpstreamName(up) {
		return ErrRefusal{"invalid upstream name"}
	}
	local := project
	if team > 0 {
		local = segmentID(team)
	}
	return s.adminWrite(ctx, "upstream.resolve", up, map[string]any{"project": local, "note": note, "ours": ours}, func(tx *sql.Tx) (string, error) {
		rows, e := tx.Query(`SELECT upstream, conflict_rev FROM upstream_sent WHERE project_id=? AND note_id=? AND conflict=1 AND (?='' OR upstream=?) ORDER BY upstream`, local, note, up, up)
		if e != nil {
			return "", e
		}
		type c struct {
			up  string
			rev int64
		}
		var cs []c
		for rows.Next() {
			var x c
			if e = rows.Scan(&x.up, &x.rev); e != nil {
				rows.Close()
				return "", e
			}
			cs = append(cs, x)
		}
		rows.Close()
		if e := rows.Err(); e != nil {
			return "", e
		}
		if len(cs) == 0 {
			return "", ErrRefusal{"no conflict on this note"}
		}
		if len(cs) > 1 {
			names := make([]string, len(cs))
			for i, x := range cs {
				names[i] = x.up
			}
			return "", ErrRefusal{"conflicts on upstreams " + strings.Join(names, ", ") + ": choose --upstream <name>"}
		}
		var n Note
		gone := false
		if !ours {
			switch e := tx.QueryRow(`SELECT class,title,body,entry_kind FROM notes WHERE id=? AND project_id=?`, note, local).Scan(&n.Class, &n.Title, &n.Body, &n.EntryKind); e {
			case nil:
			case sql.ErrNoRows:
				gone = true
			default:
				return "", e
			}
		}
		for _, x := range cs {
			switch {
			case ours:
				_, e = tx.Exec(`UPDATE upstream_sent SET conflict=2 WHERE project_id=? AND upstream=? AND note_id=?`, local, x.up, note)
			case gone: // gone here too: nothing left to settle
				_, e = tx.Exec(`DELETE FROM upstream_sent WHERE project_id=? AND upstream=? AND note_id=?`, local, x.up, note)
			default:
				_, e = tx.Exec(`UPDATE upstream_sent SET hash=?, remote_rev=max(?,0), sent_content='', conflict=0, conflict_rev=0 WHERE project_id=? AND upstream=? AND note_id=?`, upstreamHash(n), x.rev, local, x.up, note)
			}
			if e != nil {
				return "", e
			}
		}
		if ours {
			_, e = tx.Exec(`UPDATE upstream_links SET dirty_since=CASE WHEN dirty_since=0 THEN ?1 ELSE dirty_since END, last_change=max(?1,last_change+1) WHERE project_id=?2 AND upstream=?3 AND auto=1`, ms(s.now()), local, cs[0].up)
			return "", e
		}
		return "", nil
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

// upstreamKey is this gateway's secret for what it sends upstream (idempotency keys, references): 32 random bytes, made on first use. It
// is a key, not a credential of an upstream, and it never leaves; it is in the core database (so in its backups, like the notes).
func (s *Store) upstreamKey(ctx context.Context) (k []byte, err error) {
	err = s.write(ctx, func(tx *sql.Tx) error {
		if _, e := tx.Exec(`INSERT OR IGNORE INTO upstream_key(id,k) VALUES(1,randomblob(32))`); e != nil {
			return e
		}
		return tx.QueryRow(`SELECT k FROM upstream_key WHERE id=1`).Scan(&k)
	})
	return
}

// upstreamRecordRemoteNote: the pusher learned that the earlier submission of this note was accepted as the upstream's note remoteNote.
func (s *Store) upstreamRecordRemoteNote(ctx context.Context, project int64, upstream string, note, remoteNote int64) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		_, e := tx.Exec(`UPDATE upstream_sent SET remote_note=? WHERE project_id=? AND upstream=? AND note_id=?`, remoteNote, project, upstream, note)
		return e
	})
}

// upstreamRecordWaiting: the earlier submission of this note is still pending upstream; waiting=0 (a decision, a reverted text) ends it.
func (s *Store) upstreamRecordWaiting(ctx context.Context, project int64, upstream string, note int64, waiting bool) error {
	set, from := int64(0), int64(-1) // waiting: remote_rev -1 (see waitingSQL); not waiting: 0
	if waiting {
		set, from = -1, 0
	}
	return s.write(ctx, func(tx *sql.Tx) error {
		_, e := tx.Exec(`UPDATE upstream_sent SET remote_rev=? WHERE project_id=? AND upstream=? AND note_id=? AND remote_sub>0 AND remote_rev=?`, set, project, upstream, note, from)
		return e
	})
}

// upstreamRecordBase: the pusher learned the upstream revision our last accepted text corresponds to (see the remote_rev comment above). It only
// moves up (resolve --theirs takes the upstream's revision directly, also a lower one after a restore).
func (s *Store) upstreamRecordBase(ctx context.Context, project int64, upstream string, note, base int64) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		_, e := tx.Exec(`UPDATE upstream_sent SET remote_rev=? WHERE project_id=? AND upstream=? AND note_id=? AND remote_sub>0 AND remote_rev<?`, base, project, upstream, note, base)
		return e
	})
}
