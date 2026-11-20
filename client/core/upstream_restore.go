package core

import (
	"context"
	"database/sql"
	"errors"
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
// ClearRestoreMark; editing the link does not touch it.
const (
	restoredPrefix = "upstream_restored|"
	// RestoredState is how a status names the state of a marked link.
	RestoredState = "restored: reconcile before pushing"
	// restorePendingFile is written by `backup.sh --restore` into the restored core folder: the gateway marks its links at its next start and then removes it.
	restorePendingFile = "core.restored"
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
	var v string
	switch e := q.QueryRow(`SELECT value FROM meta WHERE key=?`, restoredKey(project, upstream)).Scan(&v); e {
	case nil:
	case sql.ErrNoRows:
		return t, false, nil
	default:
		return t, false, e
	}
	n, _ := strconv.ParseInt(strings.TrimPrefix(v, "restored|"), 10, 64)
	return fromMs(n), true, nil
}

// consumeRestorePending: the sentinel of a raw restore (backup.sh --restore) marks the links in one transaction, and only then is removed
// (a crash in between marks again at the next start: harmless).
func (s *Store) consumeRestorePending(ctx context.Context) error {
	p := filepath.Join(s.dir, restorePendingFile)
	if _, err := os.Lstat(p); err != nil {
		return nil // none (or unreadable: nothing to act on)
	}
	if err := s.write(ctx, func(tx *sql.Tx) error { return markRestored(tx, s.now()) }); err != nil {
		return err
	}
	if err := removeFn(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

// restoreBlockedLine is the one line a blocked push reports (and the first line of a dry run's lines).
func restoreBlockedLine(l UpstreamLink, label string, marked int) string {
	who := label
	if l.Team {
		who = "--team " + strings.TrimPrefix(label, "team ")
	}
	return "blocked: " + RestoredState + "; nothing is sent (" + markedText(marked) + "; reconcile is not available in this version: compare with the upstream, then minigw gw upstream push " + who + " --upstream " + l.Upstream + " --after-restore)"
}

func markedText(n int) string {
	if n == 1 {
		return "1 link of this gateway is marked"
	}
	return strconv.Itoa(n) + " links of this gateway are marked"
}

// restoreGate says whether the link is marked and, if so, the report of a push that sends nothing.
func (s *Store) restoreGate(ctx context.Context, l UpstreamLink, label string) (UpstreamPush, bool, error) {
	var ok bool
	var marked int
	err := s.read(ctx, func(tx *sql.Tx) (e error) {
		if _, ok, e = restoredAt(tx, l.Project, l.Upstream); e != nil || !ok {
			return
		}
		return tx.QueryRow(`SELECT count(*) FROM meta WHERE substr(key,1,?)=?`, len(restoredPrefix), restoredPrefix).Scan(&marked)
	})
	if err != nil || !ok {
		return UpstreamPush{}, false, err
	}
	return UpstreamPush{Label: label, Upstream: l.Upstream, Remote: l.Remote, Project: l.Project, Team: l.Team, Blocked: true, Lines: []string{restoreBlockedLine(l, label, marked)}}, true, nil
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
		return "", countOne(tx.Exec(`DELETE FROM meta WHERE key=?`, restoredKey(local, names[0])))
	})
}
