package core

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
)

func migrate10(tx *sql.Tx) error {
	var installed int
	if err := tx.QueryRow("SELECT count(*) FROM sqlite_master WHERE type='table' AND name='direct_memberships'").Scan(&installed); err != nil {
		return err
	}
	if installed != 0 {
		return checkMemberships(tx)
	}
	_, err := tx.Exec(`CREATE TABLE IF NOT EXISTS direct_memberships(project_id INTEGER NOT NULL REFERENCES projects(id), user_id INTEGER NOT NULL REFERENCES users(id), role TEXT NOT NULL CHECK(role IN ('reader','developer','admin')), PRIMARY KEY(project_id,user_id));
CREATE TABLE IF NOT EXISTS team_assignments(team_id INTEGER NOT NULL REFERENCES teams(id), project_id INTEGER NOT NULL, user_id INTEGER NOT NULL, role TEXT NOT NULL CHECK(role IN ('reader','developer','admin')), PRIMARY KEY(team_id,project_id,user_id), FOREIGN KEY(team_id,project_id) REFERENCES team_projects(team_id,project_id) ON DELETE CASCADE, FOREIGN KEY(team_id,user_id) REFERENCES team_members(team_id,user_id) ON DELETE CASCADE);
INSERT OR IGNORE INTO direct_memberships SELECT project_id,user_id,role FROM memberships;`)
	if err != nil {
		return err
	}
	return checkMemberships(tx)
}

const effectiveMemberships = `SELECT project_id,user_id,CASE MAX(CASE role WHEN 'admin' THEN 3 WHEN 'developer' THEN 2 ELSE 1 END) WHEN 3 THEN 'admin' WHEN 2 THEN 'developer' ELSE 'reader' END AS role FROM (SELECT project_id,user_id,role FROM direct_memberships UNION ALL SELECT project_id,user_id,role FROM team_assignments) GROUP BY project_id,user_id`

func checkMemberships(tx *sql.Tx) error {
	var n int
	err := tx.QueryRow(`WITH expected AS (` + effectiveMemberships + `) SELECT (SELECT count(*) FROM (SELECT * FROM expected EXCEPT SELECT project_id,user_id,role FROM memberships)) + (SELECT count(*) FROM (SELECT project_id,user_id,role FROM memberships EXCEPT SELECT * FROM expected)) + (SELECT count(*) FROM team_assignments a JOIN teams t ON t.id=a.team_id WHERE t.archived=1 OR NOT EXISTS(SELECT 1 FROM team_members m WHERE m.team_id=a.team_id AND m.user_id=a.user_id) OR NOT EXISTS(SELECT 1 FROM team_projects p WHERE p.team_id=a.team_id AND p.project_id=a.project_id))`).Scan(&n)
	if err != nil {
		return err
	}
	if n != 0 {
		return fmt.Errorf("membership origins inconsistent: %d", n)
	}
	return nil
}

// CheckMemberships verifies that runtime permissions match their surviving origins.
func (s *Store) CheckMemberships(ctx context.Context) error { return s.read(ctx, checkMemberships) }

// rebuildMemberships reports narrowing so adminWrite invalidates capabilities in this transaction.
func rebuildMemberships(tx *sql.Tx) (string, error) {
	var narrowed int
	err := tx.QueryRow(`WITH expected AS (` + effectiveMemberships + `) SELECT count(*) FROM memberships m LEFT JOIN expected e ON e.project_id=m.project_id AND e.user_id=m.user_id WHERE e.role IS NULL OR (CASE m.role WHEN 'admin' THEN 3 WHEN 'developer' THEN 2 ELSE 1 END) > (CASE e.role WHEN 'admin' THEN 3 WHEN 'developer' THEN 2 ELSE 1 END)`).Scan(&narrowed)
	if err != nil {
		return "", err
	}
	if _, err = tx.Exec(`DELETE FROM memberships; INSERT INTO memberships(project_id,user_id,role) ` + effectiveMemberships); err != nil {
		return "", err
	}
	if err = checkMemberships(tx); err != nil {
		return "", err
	}
	if narrowed > 0 {
		return "membership origins narrowed", nil
	}
	return "", nil
}

type MembershipOrigin struct {
	Kind string `json:"kind"`
	Team int64  `json:"team,omitempty"`
	Name string `json:"name,omitempty"`
	Role Role   `json:"role"`
}

func membershipOrigins(tx *sql.Tx, project, user int64) (out []MembershipOrigin, err error) {
	out = []MembershipOrigin{}
	rows, err := tx.Query(`SELECT 'direct',0,'',role FROM direct_memberships WHERE project_id=? AND user_id=? UNION ALL SELECT 'team',a.team_id,t.name,a.role FROM team_assignments a JOIN teams t ON t.id=a.team_id WHERE a.project_id=? AND a.user_id=? ORDER BY 1,2`, project, user, project, user)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var o MembershipOrigin
		if err = rows.Scan(&o.Kind, &o.Team, &o.Name, &o.Role); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

type TeamAssignment struct {
	Project int64 `json:"project"`
	User    int64 `json:"user"`
	Role    Role  `json:"role"`
}

func (s *Store) ListTeamAssignments(ctx context.Context, team int64) (out []TeamAssignment, err error) {
	out = []TeamAssignment{}
	err = s.read(ctx, func(tx *sql.Tx) error {
		if e := exists(tx, "teams", team); e != nil {
			return e
		}
		rows, e := tx.Query(`SELECT project_id,user_id,role FROM team_assignments WHERE team_id=? ORDER BY project_id,user_id`, team)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var a TeamAssignment
			if e = rows.Scan(&a.Project, &a.User, &a.Role); e != nil {
				return e
			}
			out = append(out, a)
		}
		return rows.Err()
	})
	return
}
func (s *Store) SetTeamAssignment(ctx context.Context, team, project, user int64, role Role) error {
	if roleRank[role] == 0 {
		return ErrInvalid{"role"}
	}
	return s.adminWrite(ctx, "team.assignment.set", strconv.FormatInt(team, 10), TeamAssignment{project, user, role}, func(tx *sql.Tx) (string, error) {
		var n int
		if e := tx.QueryRow(`SELECT count(*) FROM teams t JOIN team_projects p ON p.team_id=t.id JOIN team_members m ON m.team_id=t.id WHERE t.id=? AND t.archived=0 AND p.project_id=? AND m.user_id=?`, team, project, user).Scan(&n); e != nil {
			return "", e
		}
		if n != 1 {
			return "", ErrRefusal{"an assignment needs an active team, a project of that team and a member of that team"}
		}
		if _, e := tx.Exec(`INSERT INTO team_assignments(team_id,project_id,user_id,role) VALUES(?,?,?,?) ON CONFLICT(team_id,project_id,user_id) DO UPDATE SET role=excluded.role`, team, project, user, role); e != nil {
			return "", e
		}
		return rebuildMemberships(tx)
	})
}
func (s *Store) RemoveTeamAssignment(ctx context.Context, team, project, user int64) error {
	return s.adminWrite(ctx, "team.assignment.remove", strconv.FormatInt(team, 10), TeamAssignment{Project: project, User: user}, func(tx *sql.Tx) (string, error) {
		if e := countOne(tx.Exec(`DELETE FROM team_assignments WHERE team_id=? AND project_id=? AND user_id=?`, team, project, user)); e != nil {
			return "", e
		}
		return rebuildMemberships(tx)
	})
}

// deleteTeamOrigins audits each implicit revocation before the directory entry changes.
func deleteTeamOrigins(tx *sql.Tx, team int64, column string, target int64) error {
	q := `SELECT project_id,user_id,role FROM team_assignments WHERE team_id=?`
	args := []any{team}
	if column != "" {
		q += ` AND ` + column + `=?`
		args = append(args, target)
	}
	rows, err := tx.Query(q, args...)
	if err != nil {
		return err
	}
	var assignments []TeamAssignment
	for rows.Next() {
		var a TeamAssignment
		if err = rows.Scan(&a.Project, &a.User, &a.Role); err != nil {
			rows.Close()
			return err
		}
		assignments = append(assignments, a)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, a := range assignments {
		if err = Audit(tx, adminActor, "team.assignment.remove", strconv.FormatInt(team, 10), a); err != nil {
			return err
		}
		if _, err = tx.Exec(`DELETE FROM team_assignments WHERE team_id=? AND project_id=? AND user_id=?`, team, a.Project, a.User); err != nil {
			return err
		}
	}
	return nil
}
