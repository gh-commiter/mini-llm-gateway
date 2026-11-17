package core

import (
	"database/sql"
	"fmt"
	"strconv"
)

// A team's own notes (its "general segment") live under a hidden projects row, so notes, submissions, receipts,
// limits and search need no other table. The row id is segmentBase+team id; ids of real projects count up from 1
// and never get there. isSegment is the one test; everything that lists or accepts real projects refuses it.
const segmentBase = int64(1) << 40

func isSegment(id int64) bool { return id > segmentBase }

// segmentID is 0 (no project) for a team id that is not a real one, so a dry run on a new team never reads an id near 2^40.
func segmentID(team int64) int64 {
	if team <= 0 {
		return 0
	}
	return segmentBase + team
}

// TeamSegment is the id under which the notes of a team live (a project-shaped id for the notes routes only).
func TeamSegment(team int64) int64 { return segmentID(team) }

// SegmentTeam is the team id behind a segment id (ok false for a real project id).
func SegmentTeam(id int64) (team int64, ok bool) { return id - segmentBase, isSegment(id) }

func segmentSlug(team int64) string { return "@team-" + strconv.FormatInt(team, 10) }

// ensureSegment creates the hidden row of a team (idempotent).
func ensureSegment(tx *sql.Tx, team int64) error {
	_, err := tx.Exec(`INSERT OR IGNORE INTO projects(id,slug,name) VALUES(?,?,'')`, segmentID(team), segmentSlug(team))
	return err
}

// existsProject is exists(tx,"projects",id) for real projects only: a segment is not found.
func existsProject(tx *sql.Tx, id int64) error {
	if isSegment(id) {
		return ErrNotFound
	}
	return exists(tx, "projects", id)
}

// migrate15: one segment row per team, and triggers that keep a segment row out of everything a real project
// takes part in (members, grants, remotes, team assignment, archive); the row of a team stays unarchived, the team
// carries that state.
func migrate15(tx *sql.Tx) error {
	rows, err := tx.Query(`SELECT id FROM teams`)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err = ensureSegment(tx, id); err != nil {
			return err
		}
	}
	b := strconv.FormatInt(segmentBase, 10)
	q := `CREATE TRIGGER segment_shape BEFORE INSERT ON projects WHEN (NEW.id>` + b + `)<>(NEW.slug LIKE '@team-%') BEGIN SELECT RAISE(ABORT,'segment id and slug'); END;
CREATE TRIGGER segment_no_archive BEFORE UPDATE OF archived ON projects WHEN NEW.id>` + b + ` AND NEW.archived<>0 BEGIN SELECT RAISE(ABORT,'segment cannot be archived'); END;
CREATE TRIGGER segment_no_grant_source BEFORE INSERT ON grants WHEN NEW.source_id>` + b + ` BEGIN SELECT RAISE(ABORT,'segment in a grant'); END;
CREATE TRIGGER segment_no_grant_reader BEFORE INSERT ON grants WHEN NEW.reader_id>` + b + ` BEGIN SELECT RAISE(ABORT,'segment in a grant'); END;`
	q += `
CREATE TRIGGER segment_keep_shape BEFORE UPDATE OF id,slug ON projects WHEN (NEW.id>` + b + `)<>(NEW.slug LIKE '@team-%') OR (OLD.id>` + b + ` AND (NEW.id<>OLD.id OR NEW.slug<>OLD.slug)) BEGIN SELECT RAISE(ABORT,'segment id and slug'); END;
CREATE TRIGGER segment_no_grant_source_upd BEFORE UPDATE OF source_id ON grants WHEN NEW.source_id>` + b + ` BEGIN SELECT RAISE(ABORT,'segment in a grant'); END;
CREATE TRIGGER segment_no_grant_reader_upd BEFORE UPDATE OF reader_id ON grants WHEN NEW.reader_id>` + b + ` BEGIN SELECT RAISE(ABORT,'segment in a grant'); END;`
	for _, t := range []string{"memberships", "direct_memberships", "team_assignments", "team_projects", "project_remotes"} {
		q += `
CREATE TRIGGER segment_no_` + t + ` BEFORE INSERT ON ` + t + ` WHEN NEW.project_id>` + b + ` BEGIN SELECT RAISE(ABORT,'segment in ` + t + `'); END;
CREATE TRIGGER segment_no_` + t + `_upd BEFORE UPDATE OF project_id ON ` + t + ` WHEN NEW.project_id>` + b + ` BEGIN SELECT RAISE(ABORT,'segment in ` + t + `'); END;`
	}
	_, err = tx.Exec(q)
	return err
}

// checkSegments: every team has its row and no segment row lacks a team. Fails loudly (a team created by a path
// that forgot ensureSegment would otherwise be silently unreadable).
func checkSegments(tx *sql.Tx) error {
	var n int
	err := tx.QueryRow(`SELECT (SELECT count(*) FROM teams t WHERE NOT EXISTS(SELECT 1 FROM projects p WHERE p.id=?1+t.id AND p.slug='@team-'||t.id AND p.archived=0))
 + (SELECT count(*) FROM projects p WHERE p.id>?1 AND NOT EXISTS(SELECT 1 FROM teams t WHERE ?1+t.id=p.id))
 + (SELECT count(*) FROM memberships WHERE project_id>?1) + (SELECT count(*) FROM direct_memberships WHERE project_id>?1)
 + (SELECT count(*) FROM team_assignments WHERE project_id>?1) + (SELECT count(*) FROM team_projects WHERE project_id>?1)
 + (SELECT count(*) FROM project_remotes WHERE project_id>?1) + (SELECT count(*) FROM grants WHERE source_id>?1 OR reader_id>?1)`, segmentBase).Scan(&n)
	if err != nil {
		return err
	}
	if n != 0 {
		return fmt.Errorf("team segments inconsistent: %d", n)
	}
	return nil
}

// extraSources: the readable sets, besides the home project, that search and read by id also cover: today the
// team segment of the project's (active) team, as far as loadAccess gives the user read of it. THE one function
// of search, note read and the run channel; a hit's source is a hint, never a permission of its own.
func extraSources(tx *sql.Tx, user, project int64) ([]readable, error) {
	if isSegment(project) {
		return nil, nil
	}
	var team int64
	err := tx.QueryRow(`SELECT tp.team_id FROM team_projects tp JOIN teams t ON t.id=tp.team_id WHERE tp.project_id=? AND t.archived=0`, project).Scan(&team)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	a, err := loadAccess(tx, user, segmentID(team))
	if err != nil {
		return nil, err
	}
	if r := a.readable(segmentID(team), true); len(r.all)+len(r.own) > 0 {
		return []readable{r}, nil
	}
	return nil, nil
}

// noteSources is the home project's readable set (the caller's access a) followed by extraSources.
func noteSources(tx *sql.Tx, user, project int64, a access) ([]readable, error) {
	more, err := extraSources(tx, user, project)
	return append([]readable{a.readable(project, false)}, more...), err
}
