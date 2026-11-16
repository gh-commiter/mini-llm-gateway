package core

import (
	"context"
	"database/sql"
	"strconv"
	"strings"
)

// Teams are a directory. These tables never confer project or note access.
func migrate9(tx *sql.Tx) error {
	_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS teams(id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL, archived INTEGER NOT NULL DEFAULT 0);
CREATE UNIQUE INDEX IF NOT EXISTS teams_name ON teams(name COLLATE NOCASE);
CREATE TABLE IF NOT EXISTS team_members(team_id INTEGER NOT NULL REFERENCES teams(id), user_id INTEGER NOT NULL REFERENCES users(id), PRIMARY KEY(team_id,user_id));
CREATE INDEX IF NOT EXISTS team_members_user ON team_members(user_id);
CREATE TABLE IF NOT EXISTS team_projects(team_id INTEGER NOT NULL REFERENCES teams(id), project_id INTEGER NOT NULL UNIQUE REFERENCES projects(id), PRIMARY KEY(team_id,project_id));`)
	return err
}

type Team struct {
	ID       int64
	Name     string
	Archived bool
}
type TeamMember struct {
	UserID int64
	Name   string
}
type TeamDetail struct {
	Team
	Members  []TeamMember
	Projects []Project
}

func (s *Store) CreateTeam(ctx context.Context, name string) (t Team, err error) {
	if !validName(name) {
		return t, ErrInvalid{"name"}
	}
	err = s.adminWrite(ctx, "team.create", name, nil, func(tx *sql.Tx) (string, error) {
		r, e := tx.Exec(`INSERT INTO teams(name) VALUES(?)`, name)
		if e != nil {
			return "", teamUnique(e)
		}
		t = Team{Name: name}
		t.ID, e = r.LastInsertId()
		return "", e
	})
	return
}
func teamUnique(err error) error {
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return ErrInvalid{"name or project already used"}
	}
	return err
}
func (s *Store) RenameTeam(ctx context.Context, id int64, name string) error {
	if !validName(name) {
		return ErrInvalid{"name"}
	}
	return s.adminWrite(ctx, "team.rename", strconv.FormatInt(id, 10), map[string]any{"name": name}, func(tx *sql.Tx) (string, error) {
		r, e := tx.Exec(`UPDATE teams SET name=? WHERE id=?`, name, id)
		return "", teamUnique(countOne(r, e))
	})
}
func (s *Store) SetTeamArchived(ctx context.Context, id int64, archived bool) error {
	return s.adminWrite(ctx, "team.archive", strconv.FormatInt(id, 10), map[string]any{"archived": archived}, func(tx *sql.Tx) (string, error) {
		return "", countOne(tx.Exec(`UPDATE teams SET archived=? WHERE id=?`, archived, id))
	})
}
func (s *Store) AdminListTeams(ctx context.Context) (out []Team, err error) {
	err = s.read(ctx, func(tx *sql.Tx) error {
		var e error
		out, e = teamList(tx, `SELECT id,name,archived FROM teams ORDER BY id`)
		return e
	})
	return
}
func teamList(tx *sql.Tx, q string, args ...any) (out []Team, err error) {
	rows, err := tx.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var t Team
		if err = rows.Scan(&t.ID, &t.Name, &t.Archived); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
func teamMembers(tx *sql.Tx, id int64) (out []TeamMember, err error) {
	rows, err := tx.Query(`SELECT u.id,u.name FROM users u JOIN team_members m ON m.user_id=u.id WHERE m.team_id=? ORDER BY u.id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var m TeamMember
		if err = rows.Scan(&m.UserID, &m.Name); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
func teamProjects(tx *sql.Tx, id, user int64) (out []Project, err error) {
	q := `SELECT p.id,p.slug,p.name,p.archived FROM projects p JOIN team_projects t ON t.project_id=p.id WHERE t.team_id=?`
	args := []any{id}
	if user != 0 {
		q += ` AND (p.id IN (SELECT project_id FROM memberships WHERE user_id=?) OR p.id IN (SELECT g.source_id FROM grants g JOIN memberships m ON m.project_id=g.reader_id WHERE m.user_id=?))`
		args = append(args, user, user)
	}
	rows, err := tx.Query(q+` ORDER BY p.slug`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var p Project
		if err = rows.Scan(&p.ID, &p.Slug, &p.Name, &p.Archived); err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, p)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if user != 0 {
		for i := range out {
			a, e := loadAccess(tx, user, out[i].ID)
			if e != nil {
				return nil, e
			}
			out[i].Role = a.Role
			for _, c := range []Class{Structure, Data} {
				if a.Grants[c] {
					out[i].Granted = append(out[i].Granted, c)
				}
			}
		}
	}
	return out, nil
}
func (s *Store) ListTeamMembers(ctx context.Context, id int64) (out []TeamMember, err error) {
	err = s.read(ctx, func(tx *sql.Tx) error {
		if e := exists(tx, "teams", id); e != nil {
			return e
		}
		var e error
		out, e = teamMembers(tx, id)
		return e
	})
	return
}
func (s *Store) ListTeamProjects(ctx context.Context, id int64) (out []Project, err error) {
	err = s.read(ctx, func(tx *sql.Tx) error {
		if e := exists(tx, "teams", id); e != nil {
			return e
		}
		var e error
		out, e = teamProjects(tx, id, 0)
		return e
	})
	return
}
func (s *Store) AdminGetTeam(ctx context.Context, id int64) (out TeamDetail, err error) {
	err = s.read(ctx, func(tx *sql.Tx) error {
		if e := tx.QueryRow(`SELECT id,name,archived FROM teams WHERE id=?`, id).Scan(&out.ID, &out.Name, &out.Archived); e != nil {
			return notFound(e)
		}
		var e error
		out.Members, e = teamMembers(tx, id)
		if e != nil {
			return e
		}
		out.Projects, e = teamProjects(tx, id, 0)
		return e
	})
	return
}
func (s *Store) changeTeamEntry(ctx context.Context, id, target int64, project, add bool) error {
	table, column, kind, entity := "team_members", "user_id", "member", "users"
	if project {
		table, column, kind, entity = "team_projects", "project_id", "project", "projects"
	}
	op := "remove"
	if add {
		op = "add"
	}
	return s.adminWrite(ctx, "team."+kind+"."+op, strconv.FormatInt(id, 10), map[string]any{kind: target}, func(tx *sql.Tx) (string, error) {
		var archived bool
		if e := tx.QueryRow(`SELECT archived FROM teams WHERE id=?`, id).Scan(&archived); e != nil {
			return "", notFound(e)
		}
		if add {
			if archived {
				return "", ErrInvalid{"team archived"}
			}
			if e := exists(tx, entity, target); e != nil {
				return "", e
			}
			_, e := tx.Exec(`INSERT INTO `+table+`(team_id,`+column+`) VALUES(?,?) ON CONFLICT(team_id,`+column+`) DO NOTHING`, id, target)
			return "", teamUnique(e)
		}
		return "", countOne(tx.Exec(`DELETE FROM `+table+` WHERE team_id=? AND `+column+`=?`, id, target))
	})
}
func (s *Store) AddTeamMember(ctx context.Context, id, user int64) error {
	return s.changeTeamEntry(ctx, id, user, false, true)
}
func (s *Store) RemoveTeamMember(ctx context.Context, id, user int64) error {
	return s.changeTeamEntry(ctx, id, user, false, false)
}
func (s *Store) AddTeamProject(ctx context.Context, id, project int64) error {
	return s.changeTeamEntry(ctx, id, project, true, true)
}
func (s *Store) RemoveTeamProject(ctx context.Context, id, project int64) error {
	return s.changeTeamEntry(ctx, id, project, true, false)
}

func (s *Store) ListTeams(ctx context.Context, p Principal, includeArchived bool) (out []Team, err error) {
	lim := s.rate(p.UserID, false)
	err = s.read(ctx, func(tx *sql.Tx) error {
		if e := checkPrincipal(tx, p); e != nil {
			return e
		}
		if lim != nil {
			return lim
		}
		var e error
		out, e = teamList(tx, `SELECT t.id,t.name,t.archived FROM teams t JOIN team_members m ON m.team_id=t.id WHERE m.user_id=? AND (t.archived=0 OR ?) ORDER BY t.name COLLATE NOCASE`, p.UserID, includeArchived)
		return e
	})
	return
}
func (s *Store) GetTeam(ctx context.Context, p Principal, ref string) (out TeamDetail, err error) {
	lim := s.rate(p.UserID, false)
	err = s.read(ctx, func(tx *sql.Tx) error {
		if e := checkPrincipal(tx, p); e != nil {
			return e
		}
		q, arg := `t.name=? COLLATE NOCASE`, any(ref)
		if id, e := strconv.ParseInt(ref, 10, 64); e == nil {
			q, arg = `t.id=?`, id
		}
		if e := tx.QueryRow(`SELECT t.id,t.name,t.archived FROM teams t JOIN team_members m ON m.team_id=t.id WHERE m.user_id=? AND `+q, p.UserID, arg).Scan(&out.ID, &out.Name, &out.Archived); e != nil {
			return notFound(e)
		}
		if lim != nil {
			return lim
		}
		var e error
		out.Members, e = teamMembers(tx, out.ID)
		if e != nil {
			return e
		}
		out.Projects, e = teamProjects(tx, out.ID, p.UserID)
		return e
	})
	if err != nil {
		return TeamDetail{}, err
	}
	return
}
