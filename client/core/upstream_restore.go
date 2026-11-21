package core

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// The restore mark (#167). A restored database may be older than what this gateway sent upstream: it no longer knows which notes the
// upstream already has, so an edited note would be sent as a NEW note (a second copy). Every upstream link of a restored database is
// therefore marked, and a marked link sends and creates nothing until the owner clears the mark.
//
// Two things mark a restored database, both exact: `gw core restore` (inside stamp()) and the "restore pending" file that backup.sh --restore writes
// (restorePendingFile). Nothing else is guessed: an epoch gap between the database and core.state says nothing reliable (it also appears on a healthy
// gateway after a crash, and is absent when an older database with the same epoch is copied back), so a raw copy of the files is not detected unless the
// operator creates that file by hand (docs/usage.md, upstream chapter).
//
// The mark is one record in the meta table (no schema change) per link: key restoredPrefix + project id (a team's segment id for a team
// link) + "|" + upstream, value "restored|<unix ms of the restore>". It is removed with the link (unlink, removal of the upstream) and by
// ClearRestoreMark or a completed reconcile; editing the link does not touch it.
const (
	restoredPrefix = "upstream_restored|"
	// RestoredState is how a status names the state of a marked link.
	RestoredState = "restored: reconcile before pushing"
	// restorePendingFile is written by `backup.sh --restore` into the restored core folder: the gateway marks its links at its next start and then removes it.
	restorePendingFile = "core.restored"
	// hubLastWayOut: what is left when an upstream keeps changing its generation (reconcile cannot finish against it).
	hubLastWayOut = "if the upstream keeps changing its generation, the last way out is unlink and link again: every published note is then sent again as new"
	// HubRestoredCause is how a status names the cause of a mark that the UPSTREAM's restore set (its generation changed, see hubGenerationSeen).
	HubRestoredCause = "the upstream was restored from a backup"
	// The value of such a mark starts with hubRestoredMark instead of "restored|".
	hubRestoredMark = "hub-restored|"
	// hubGenKey: this gateway's own generation as an upstream (a random id, replaced by every restore of this database).
	hubGenKey = "hub_generation"
	// hubSeenPrefix + upstream: the generation that upstream handed out last (the linked gateway's side).
	hubSeenPrefix = "upstream_generation|"
)

// restoredSQL is true for a link of upstream_links that carries the mark (the gate of the automatic selection).
const restoredSQL = `EXISTS(SELECT 1 FROM meta WHERE key='` + restoredPrefix + `'||upstream_links.project_id||'|'||upstream_links.upstream)`

func restoredKey(project int64, upstream string) string {
	return restoredPrefix + strconv.FormatInt(project, 10) + "|" + upstream
}

// markRestored marks every upstream link in tx (manual and automatic, submit and publish) and resets the push traversal, so that nothing
// resumes in the middle of a run that the backup remembers. The outbox (dirty_since) is kept: the pending work waits. Marks that are already
// there keep their time.
func markRestored(tx *sql.Tx, now time.Time) error {
	var n int
	if err := tx.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='upstream_links'`).Scan(&n); err != nil || n == 0 {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO meta(key,value) SELECT '`+restoredPrefix+`'||project_id||'|'||upstream, ? FROM upstream_links WHERE true ON CONFLICT(key) DO NOTHING`, "restored|"+strconv.FormatInt(ms(now), 10)); err != nil {
		return err
	}
	_, err := tx.Exec(`UPDATE upstream_links SET push_cursor=0, push_generation=0, push_failed=0`)
	return err
}

// restoredAt: when the link was marked, or ok=false.
func restoredAt(q interface {
	QueryRow(string, ...any) *sql.Row
}, project int64, upstream string) (t time.Time, ok bool, err error) {
	t, _, ok, err = restoredMark(q, project, upstream)
	return
}

// restoredMark: restoredAt, and whether the upstream's restore set the mark (hub) rather than a restore of this gateway.
func restoredMark(q interface {
	QueryRow(string, ...any) *sql.Row
}, project int64, upstream string) (t time.Time, hub, ok bool, err error) {
	var v string
	switch e := q.QueryRow(`SELECT value FROM meta WHERE key=?`, restoredKey(project, upstream)).Scan(&v); e {
	case nil:
	case sql.ErrNoRows:
		return t, false, false, nil
	default:
		return t, false, false, e
	}
	hub = strings.HasPrefix(v, hubRestoredMark)
	_, at, _ := strings.Cut(v, "|")
	n, _ := strconv.ParseInt(at, 10, 64)
	return fromMs(n), hub, true, nil
}

// newHubGeneration gives this database a new generation as an upstream. Every restore calls it (stamp, consumeRestorePending): a restored
// backup holds the generation of its time, and a linked gateway must see one it never saw before.
func newHubGeneration(tx *sql.Tx) error {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return err
	}
	return setMeta(tx, hubGenKey, hex.EncodeToString(b))
}

// HubGeneration is this gateway's generation as an upstream: what its project and team answers hand out to linked gateways (created at the
// first use, replaced by every restore). It is not a secret and identifies nothing else.
func (s *Store) HubGeneration(ctx context.Context) (string, error) {
	var g string
	get := func(tx *sql.Tx) error {
		if e := tx.QueryRow(`SELECT value FROM meta WHERE key=?`, hubGenKey).Scan(&g); e != sql.ErrNoRows {
			return e
		}
		return nil
	}
	if err := s.read(ctx, get); err != nil || g != "" {
		return g, err
	}
	err := s.write(ctx, func(tx *sql.Tx) error {
		if e := get(tx); e != nil || g != "" {
			return e
		}
		if e := newHubGeneration(tx); e != nil {
			return e
		}
		return get(tx)
	})
	return g, err
}

// validGeneration: what is taken of an upstream's generation (DATA): 16 to 64 lowercase hex digits; anything else counts as none sent.
func validGeneration(g string) bool {
	if len(g) < 16 || len(g) > 64 {
		return false
	}
	for _, r := range g {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

// hubGenerationSeen compares the generation an upstream just handed out (gen; "" or malformed: an older upstream, nothing is checked) with
// the one stored for it. The first one is stored and nothing else happens. A different one means the upstream went back in time (restored
// from a backup): in ONE transaction every link of that upstream is marked (the mark's value names that cause; an earlier mark of a link is
// replaced, so a listing or decision made before is void), the traversals start again, the upstream's mirror is dropped, the new generation
// is stored and an audit row is written. Nothing is cleared or
// sent here: a changed generation only ever adds marks. It says whether it marked.
func (s *Store) hubGenerationSeen(ctx context.Context, upstream, gen string) (marked bool, err error) {
	if !validGeneration(gen) {
		return false, nil
	}
	err = s.write(ctx, func(tx *sql.Tx) error {
		var old string
		switch e := tx.QueryRow(`SELECT value FROM meta WHERE key=?`, hubSeenPrefix+upstream).Scan(&old); {
		case e == sql.ErrNoRows:
			return setMeta(tx, hubSeenPrefix+upstream, gen)
		case e != nil:
			return e
		case old == gen:
			return nil
		}
		r, e := tx.Exec(`INSERT INTO meta(key,value) SELECT '`+restoredPrefix+`'||project_id||'|'||upstream, ? FROM upstream_links WHERE upstream=? ON CONFLICT(key) DO UPDATE SET value=excluded.value`, hubRestoredMark+strconv.FormatInt(ms(s.now()), 10), upstream)
		if e != nil {
			return e
		}
		n, _ := r.RowsAffected()
		if _, e = tx.Exec(`UPDATE upstream_links SET push_cursor=0, push_generation=0, push_failed=0, pull_cursor=0, pull_failed=0, pull_skipped=0, pull_skipped_ids='' WHERE upstream=?`, upstream); e != nil {
			return e
		}
		// The mirror is keyed by the upstream's note numbers and revisions, which the restored upstream hands out again: a row could stand for
		// another note now and would never be fetched again. It is dropped; the next pull (this one, when a pull noticed) fetches everything.
		if e = purgeUpstreamMirror(tx, upstream); e != nil {
			return e
		}
		if e = setMeta(tx, hubSeenPrefix+upstream, gen); e != nil {
			return e
		}
		marked = true
		return Audit(tx, "gateway", "upstream.hub-restored", upstream, map[string]any{"old": old[:min(8, len(old))], "new": gen[:8], "links": n})
	})
	return marked, err
}

// consumeRestorePending: the sentinel of a raw restore (backup.sh --restore) marks the links in one transaction, and only then is removed
// (a crash in between marks again at the next start: harmless).
func (s *Store) consumeRestorePending(ctx context.Context) error {
	p := filepath.Join(s.dir, restorePendingFile)
	if _, err := os.Lstat(p); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil { // cannot tell whether a restore is pending: the start fails closed rather than pushing from a possibly restored database
		return fmt.Errorf("the restore pending file cannot be checked: %w", err)
	}
	if err := s.write(ctx, func(tx *sql.Tx) error {
		if e := markRestored(tx, s.now()); e != nil {
			return e
		}
		return newHubGeneration(tx)
	}); err != nil {
		return err
	}
	s.sentinelMarked, _ = markedLinks(ctx, s.db)
	if err := removeFn(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// restoreBlockedLine is the one line a blocked push reports (and the first line of a dry run's lines).
func restoreBlockedLine(l UpstreamLink, label string, marked int) string {
	return restoreBlockedCause(l, label, marked, false)
}

// restoreBlockedCause is restoreBlockedLine with the cause of the mark (hub: the upstream was restored).
func restoreBlockedCause(l UpstreamLink, label string, marked int, hub bool) string {
	who := label
	if l.Team {
		who = "--team " + strings.TrimPrefix(label, "team ")
	}
	if hub { // no override: a receipt may name another member's note on the restored upstream now (ClearRestoreMark refuses it)
		return "blocked: " + RestoredState + " (" + HubRestoredCause + "); nothing is sent (" + markedText(marked) + "; run minigw gw upstream reconcile " + who + " --upstream " + l.Upstream + "; " + hubLastWayOut + ")"
	}
	return "blocked: " + RestoredState + "; nothing is sent (" + markedText(marked) + "; run minigw gw upstream reconcile " + who + " --upstream " + l.Upstream + "; override: minigw gw upstream push " + who + " --upstream " + l.Upstream + " --after-restore)"
}

func markedText(n int) string {
	if n == 1 {
		return "1 link of this gateway is marked"
	}
	return strconv.Itoa(n) + " links of this gateway are marked"
}

// restoreGate says whether the link is marked and, if so, the report of a push that sends nothing.
func (s *Store) restoreGate(ctx context.Context, l UpstreamLink, label string) (UpstreamPush, bool, error) {
	var ok, hub bool
	var marked int
	err := s.read(ctx, func(tx *sql.Tx) (e error) {
		if _, hub, ok, e = restoredMark(tx, l.Project, l.Upstream); e != nil || !ok {
			return
		}
		return tx.QueryRow(`SELECT count(*) FROM meta WHERE substr(key,1,?)=?`, len(restoredPrefix), restoredPrefix).Scan(&marked)
	})
	if err != nil || !ok {
		return UpstreamPush{}, false, err
	}
	return UpstreamPush{Label: label, Upstream: l.Upstream, Remote: l.Remote, Project: l.Project, Team: l.Team, Blocked: true, Lines: []string{restoreBlockedCause(l, label, marked, hub) + s.reconcileCountsText(ctx, l)}}, true, nil
}

// ClearRestoreMark is the owner's override (push --after-restore): it clears the mark of ONE link (the upstream is named, or the project
// or team has exactly one marked link) and writes an audit row (the link and the time, never a note's text). Pushing without reconciling can
// create second copies of notes the upstream already has.
func (s *Store) ClearRestoreMark(ctx context.Context, project, team int64, upstream string) error {
	local := project
	if team > 0 {
		local = segmentID(team)
	}
	if local == 0 {
		return ErrRefusal{"--after-restore names one project or team"}
	}
	var names []string
	err := s.read(ctx, func(tx *sql.Tx) error {
		rows, e := tx.Query(`SELECT upstream FROM upstream_links WHERE project_id=? AND `+restoredSQL+` ORDER BY upstream`, local)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var n string
			if e = rows.Scan(&n); e != nil {
				return e
			}
			if upstream == "" || n == upstream {
				names = append(names, n)
			}
		}
		return rows.Err()
	})
	if err != nil {
		return err
	}
	switch {
	case len(names) == 0:
		return ErrRefusal{"this link is not marked as restored: nothing to clear"}
	case len(names) > 1:
		return ErrRefusal{"several links are marked as restored: name one with --upstream <name>"}
	}
	mu := s.linkLock(local, names[0])
	if !mu.TryLock() {
		return errPushRunning{}
	}
	defer mu.Unlock()
	return s.adminWrite(ctx, "upstream.after-restore", names[0], map[string]any{"project": local}, func(tx *sql.Tx) (string, error) {
		// A mark the UPSTREAM's restore set is not the owner's to override: its note numbers are handed out again, so a receipt may name
		// another member's note there now, and a push would replace (publish: overwrite) it. Only reconcile clears it.
		if _, hub, ok, e := restoredMark(tx, local, names[0]); e != nil {
			return "", e
		} else if ok && hub {
			return "", ErrRefusal{AfterRestoreHubRefusal(names[0])}
		}
		return "", countOne(tx.Exec(`DELETE FROM meta WHERE key=?`, restoredKey(local, names[0])))
	})
}

// AfterRestoreHubRefusal is the text of the refusal of --after-restore for a mark the upstream's restore set (the client says it before its warning).
func AfterRestoreHubRefusal(upstream string) string {
	return "--after-restore cannot clear this mark: " + HubRestoredCause + ", and a receipt here may name another member's note there now; run minigw gw upstream reconcile for this link (--upstream " + upstream + "); " + hubLastWayOut
}

// MarkedLinks is how many upstream links carry the restore mark now (a start-up log line and the restore command's report).
func (s *Store) MarkedLinks(ctx context.Context) (int, error) { return markedLinks(ctx, s.rdb) }

// SentinelMarked is how many links the "restore pending" file marked at this start (0: there was none).
func (s *Store) SentinelMarked() int { return s.sentinelMarked }

// MarkedLinksIn counts the marked links of the database file in dir (no gateway running: the restore command reports it).
func MarkedLinksIn(dir string) int {
	db, err := openDB(filepath.Join(dir, dbFile))
	if err != nil {
		return 0
	}
	defer db.Close()
	n, _ := markedLinks(context.Background(), db)
	return n
}

func markedLinks(ctx context.Context, db *sql.DB) (n int, err error) {
	err = db.QueryRowContext(ctx, `SELECT count(*) FROM meta WHERE substr(key,1,?)=?`, len(restoredPrefix), restoredPrefix).Scan(&n)
	return
}
