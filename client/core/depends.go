package core

import (
	"context"
	"database/sql"
	"strconv"
)

// maxDepends is the cap of dependencies per project.
const maxDepends = 16

// migrate16: the owner-managed, directed relation "project depends on project". It is a search hint, never a
// permission. A team segment can be neither side (guards as in migration 15).
func migrate16(tx *sql.Tx) error {
	b := strconv.FormatInt(segmentBase, 10)
	_, err := tx.Exec(`CREATE TABLE project_depends(project_id INTEGER NOT NULL REFERENCES projects(id), depends_on INTEGER NOT NULL REFERENCES projects(id), PRIMARY KEY(project_id,depends_on), CHECK(project_id<>depends_on)) WITHOUT ROWID;
CREATE INDEX project_depends_target ON project_depends(depends_on);
CREATE TRIGGER segment_no_depends BEFORE INSERT ON project_depends WHEN NEW.project_id>` + b + ` OR NEW.depends_on>` + b + ` BEGIN SELECT RAISE(ABORT,'segment in a dependency'); END;
CREATE TRIGGER segment_no_depends_upd BEFORE UPDATE OF project_id,depends_on ON project_depends WHEN NEW.project_id>` + b + ` OR NEW.depends_on>` + b + ` BEGIN SELECT RAISE(ABORT,'segment in a dependency'); END;`)
	return err
}

// migrate18: a project may also depend on another TEAM's notes (the owner's decision): a search of the project then covers
// that team's segment for the people on the team's roster (never a permission of its own). A second table, so that the
// guarantee of migration 16 stays whole: a segment id is on neither side of project_depends. A segment can never HAVE a
// team dependency either (trigger).
func migrate18(tx *sql.Tx) error {
	b := strconv.FormatInt(segmentBase, 10)
	_, err := tx.Exec(`CREATE TABLE project_team_depends(project_id INTEGER NOT NULL REFERENCES projects(id), team_id INTEGER NOT NULL REFERENCES teams(id), PRIMARY KEY(project_id,team_id)) WITHOUT ROWID;
CREATE INDEX project_team_depends_team ON project_team_depends(team_id);
CREATE TRIGGER segment_no_team_depends BEFORE INSERT ON project_team_depends WHEN NEW.project_id>` + b + ` BEGIN SELECT RAISE(ABORT,'segment with a dependency'); END;
CREATE TRIGGER segment_no_team_depends_upd BEFORE UPDATE OF project_id ON project_team_depends WHEN NEW.project_id>` + b + ` BEGIN SELECT RAISE(ABORT,'segment with a dependency'); END;`)
	return err
}

func (s *Store) AddDepends(ctx context.Context, project, other int64) error {
	if project == other {
		return ErrRefusal{"a project cannot depend on itself"}
	}
	return s.adminWrite(ctx, "project.depends.add", strconv.FormatInt(project, 10), map[string]any{"depends_on": other}, func(tx *sql.Tx) (string, error) {
		for _, id := range []int64{project, other} {
			if e := existsProject(tx, id); e != nil {
				return "", e
			}
			if e := archivedErr(tx, id); e != nil {
				return "", e
			}
		}
		var n int
		if e := tx.QueryRow(`SELECT (SELECT count(*) FROM project_depends WHERE project_id=?1 AND depends_on<>?2)+(SELECT count(*) FROM project_team_depends WHERE project_id=?1)`, project, other).Scan(&n); e != nil {
			return "", e
		}
		if n >= maxDepends {
			return "", ErrRefusal{"a project can have at most 16 dependencies (projects and teams together)"}
		}
		_, e := tx.Exec(`INSERT OR IGNORE INTO project_depends(project_id,depends_on) VALUES(?,?)`, project, other)
		return "", e
	})
}

// RemoveDepends is always allowed (also for archived projects).
func (s *Store) RemoveDepends(ctx context.Context, project, other int64) error {
	return s.adminWrite(ctx, "project.depends.remove", strconv.FormatInt(project, 10), map[string]any{"depends_on": other}, func(tx *sql.Tx) (string, error) {
		return "", countOne(tx.Exec(`DELETE FROM project_depends WHERE project_id=? AND depends_on=?`, project, other))
	})
}

func dependsList(tx *sql.Tx, q string, arg int64) (out []Project, err error) {
	rows, err := tx.Query(q, arg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var p Project
		if err = rows.Scan(&p.ID, &p.Slug, &p.Name, &p.Archived); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// AdminListDepends: what the project depends on, and which projects depend on it (the owner sees everything).
func (s *Store) AdminListDepends(ctx context.Context, project int64) (depends, usedBy []Project, err error) {
	err = s.read(ctx, func(tx *sql.Tx) error {
		if e := existsProject(tx, project); e != nil {
			return e
		}
		var e error
		if depends, e = dependsList(tx, `SELECT p.id,p.slug,p.name,p.archived FROM project_depends d JOIN projects p ON p.id=d.depends_on WHERE d.project_id=? ORDER BY p.slug`, project); e != nil {
			return e
		}
		usedBy, e = dependsList(tx, `SELECT p.id,p.slug,p.name,p.archived FROM project_depends d JOIN projects p ON p.id=d.project_id WHERE d.depends_on=? ORDER BY p.slug`, project)
		return e
	})
	return
}

// ListDepends is the member view: only the dependencies the caller can see (role or grant); never a slug the
// caller cannot read.
func (s *Store) ListDepends(ctx context.Context, p Principal, project int64) (out []Project, err error) {
	lim := s.rate(p.UserID, false)
	err = s.read(ctx, func(tx *sql.Tx) error {
		if _, e := memberAccess(tx, p, project); e != nil || isSegment(project) {
			if e == nil {
				e = ErrNotFound
			}
			return e
		}
		if lim != nil {
			return lim
		}
		all, e := dependsList(tx, `SELECT p.id,p.slug,p.name,p.archived FROM project_depends d JOIN projects p ON p.id=d.depends_on WHERE d.project_id=? ORDER BY p.slug`, project)
		if e != nil {
			return e
		}
		for _, d := range all {
			a, e := loadAccess(tx, p.UserID, d.ID)
			if e != nil {
				return e
			}
			if a.visible() {
				out = append(out, d)
			}
		}
		return nil
	})
	return
}

// AddTeamDepends: the project depends on a team's notes. Refused for an archived project or team, for the project's own team
// (its notes are already searched) and above the cap (project and team dependencies together). Adding twice is a no-op.
func (s *Store) AddTeamDepends(ctx context.Context, project, team int64) error {
	return s.adminWrite(ctx, "project.depends.team.add", strconv.FormatInt(project, 10), map[string]any{"team": team}, func(tx *sql.Tx) (string, error) {
		if e := existsProject(tx, project); e != nil {
			return "", e
		}
		if e := archivedErr(tx, project); e != nil {
			return "", e
		}
		var name string
		var archived bool
		if e := tx.QueryRow(`SELECT name,archived FROM teams WHERE id=?`, team).Scan(&name, &archived); e != nil {
			return "", notFound(e)
		}
		if archived {
			return "", ErrRefusal{"team " + name + " is archived: unarchive it first"}
		}
		var own int
		if e := tx.QueryRow(`SELECT count(*) FROM team_projects WHERE project_id=? AND team_id=?`, project, team).Scan(&own); e != nil {
			return "", e
		}
		if own > 0 {
			return "", ErrRefusal{"team " + name + " already holds this project: its notes are searched anyway"}
		}
		var n int
		if e := tx.QueryRow(`SELECT (SELECT count(*) FROM project_depends WHERE project_id=?1)+(SELECT count(*) FROM project_team_depends WHERE project_id=?1 AND team_id<>?2)`, project, team).Scan(&n); e != nil {
			return "", e
		}
		if n >= maxDepends {
			return "", ErrRefusal{"a project can have at most 16 dependencies (projects and teams together)"}
		}
		_, e := tx.Exec(`INSERT OR IGNORE INTO project_team_depends(project_id,team_id) VALUES(?,?)`, project, team)
		return "", e
	})
}

// RemoveTeamDepends is always allowed.
func (s *Store) RemoveTeamDepends(ctx context.Context, project, team int64) error {
	return s.adminWrite(ctx, "project.depends.team.remove", strconv.FormatInt(project, 10), map[string]any{"team": team}, func(tx *sql.Tx) (string, error) {
		return "", countOne(tx.Exec(`DELETE FROM project_team_depends WHERE project_id=? AND team_id=?`, project, team))
	})
}

func teamDependsList(tx *sql.Tx, project int64, user int64) (out []Team, err error) {
	rows, err := tx.Query(`SELECT t.id,t.name,t.archived,t.path FROM project_team_depends d JOIN teams t ON t.id=d.team_id WHERE d.project_id=? ORDER BY t.name COLLATE NOCASE`, project)
	if err != nil {
		return nil, err
	}
	var all []Team
	for rows.Next() {
		var t Team
		if err = rows.Scan(&t.ID, &t.Name, &t.Archived, &t.Path); err != nil {
			rows.Close()
			return nil, err
		}
		all = append(all, t)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	for _, t := range all {
		if user != 0 { // the member view: only the teams the caller is on (a name the caller may not know is never shown)
			a, e := loadAccess(tx, user, segmentID(t.ID))
			if e != nil {
				return nil, e
			}
			if !a.visible() {
				continue
			}
		}
		out = append(out, t)
	}
	return out, nil
}

// AdminListTeamDepends: the teams the project depends on (the owner sees all).
func (s *Store) AdminListTeamDepends(ctx context.Context, project int64) (out []Team, err error) {
	err = s.read(ctx, func(tx *sql.Tx) error {
		if e := existsProject(tx, project); e != nil {
			return e
		}
		var e error
		out, e = teamDependsList(tx, project, 0)
		return e
	})
	return
}

// ListTeamDepends is the member view: only the teams the caller is on.
func (s *Store) ListTeamDepends(ctx context.Context, p Principal, project int64) (out []Team, err error) {
	lim := s.rate(p.UserID, false)
	err = s.read(ctx, func(tx *sql.Tx) error {
		if _, e := memberAccess(tx, p, project); e != nil || isSegment(project) {
			if e == nil {
				e = ErrNotFound
			}
			return e
		}
		if lim != nil {
			return lim
		}
		var e error
		out, e = teamDependsList(tx, project, p.UserID)
		return e
	})
	return
}
