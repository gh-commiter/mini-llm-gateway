package core

import (
	"context"
	"database/sql"
)

// AdminCounts are the numbers and names the owner's project and team lists carry, by aggregate queries (one transaction, four
// queries, whatever the number of projects, teams and people) instead of a query per row.
type AdminCounts struct {
	TeamOfProject    map[int64]string // project id -> the name of its team (a project is in at most one)
	MembersOfProject map[int64]int    // project id -> people with a role in it (as ListMembers counts them)
	MembersOfTeam    map[int64]int    // team id -> people in the team (as ListTeamMembers)
	ProjectsOfTeam   map[int64]int    // team id -> projects of the team (as ListTeamProjects)
}

func (s *Store) AdminCounts(ctx context.Context) (c AdminCounts, err error) {
	c = AdminCounts{map[int64]string{}, map[int64]int{}, map[int64]int{}, map[int64]int{}}
	err = s.read(ctx, func(tx *sql.Tx) error {
		count := func(q string, into map[int64]int) error {
			rows, e := tx.Query(q)
			if e != nil {
				return e
			}
			defer rows.Close()
			for rows.Next() {
				var id int64
				var n int
				if e = rows.Scan(&id, &n); e != nil {
					return e
				}
				into[id] = n
			}
			return rows.Err()
		}
		if e := count(`SELECT m.project_id,count(*) FROM memberships m JOIN users u ON u.id=m.user_id GROUP BY m.project_id`, c.MembersOfProject); e != nil {
			return e
		}
		if e := count(`SELECT m.team_id,count(*) FROM team_members m JOIN users u ON u.id=m.user_id GROUP BY m.team_id`, c.MembersOfTeam); e != nil {
			return e
		}
		if e := count(`SELECT t.team_id,count(*) FROM team_projects t JOIN projects p ON p.id=t.project_id GROUP BY t.team_id`, c.ProjectsOfTeam); e != nil {
			return e
		}
		rows, e := tx.Query(`SELECT t.project_id,tm.name FROM team_projects t JOIN teams tm ON tm.id=t.team_id`)
		if e != nil {
			return e
		}
		defer rows.Close()
		for rows.Next() {
			var id int64
			var name string
			if e = rows.Scan(&id, &name); e != nil {
				return e
			}
			c.TeamOfProject[id] = name
		}
		return rows.Err()
	})
	return
}
