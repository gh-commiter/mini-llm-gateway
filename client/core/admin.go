package core

import (
	"context"
	"database/sql"
	"errors"
	"strconv"
	"strings"
	"unicode"
)

// Admin operations. The gateway has already authenticated the caller as owner; the core does not check that.
// Every operation writes an audit event; an operation that can take access away bumps the epoch in the same
// transaction. The rules (section 1, "Epoch"):
//   - binding set to another user, binding removed, user deactivated, member removed, role lowered, grant removed,
//     DeviceRevoked: bump. Adding or raising anything, renaming, archiving: no bump.
//   - every note class change (UpdateNote): structure->data takes the note from readers and structure-only grantees,
//     data->structure from data-only grantees, a move into private from everyone but author and admins, and a
//     move out of private from an author who is only a reader by now.
const adminActor = "owner"

type User struct {
	ID     int64
	Name   string
	Active bool
	Owner  bool
}

type Binding struct {
	DeviceID string
	UserID   int64
}

type Project struct {
	ID       int64
	Slug     string
	Name     string
	Archived bool
	Role     Role // "" = no effective membership
	Origins  []MembershipOrigin
	Granted  []Class // classes reached through grants
	Pending  int     // member project list: submissions an admin of the project may review (never their own); 0 for others
}

type Member struct {
	UserID  int64
	Name    string
	Role    Role
	Origins []MembershipOrigin
}

type Grant struct {
	SourceID int64
	ReaderID int64
	Class    Class
}

var roleRank = map[Role]int{Reader: 1, Developer: 2, Admin: 3}

func validName(n string) bool {
	if strings.TrimSpace(n) == "" || len(n) > 128 {
		return false
	}
	for _, r := range n {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '\u2028' || r == '\u2029' {
			return false
		}
	}
	return true
}

func (s *Store) adminWrite(ctx context.Context, action, subject string, detail any, fn func(tx *sql.Tx) (bump string, err error)) error {
	return s.write(ctx, func(tx *sql.Tx) error {
		bump, err := fn(tx)
		if err != nil {
			return err
		}
		if err := Audit(tx, adminActor, action, subject, detail); err != nil {
			return err
		}
		if bump != "" {
			_, err = s.BumpEpochTx(tx, bump)
		}
		return err
	})
}

func exists(tx *sql.Tx, table string, id int64) error { // table is a constant of this file
	var n int
	if err := tx.QueryRow(`SELECT count(*) FROM `+table+` WHERE id=?`, id).Scan(&n); err != nil || n == 0 {
		if err == nil {
			err = ErrNotFound
		}
		return err
	}
	return nil
}

func (s *Store) CreateUser(ctx context.Context, name string) (u User, err error) {
	if !validName(name) {
		return u, ErrInvalid{"name"}
	}
	err = s.adminWrite(ctx, "user.create", name, nil, func(tx *sql.Tx) (string, error) {
		r, err := tx.Exec(`INSERT INTO users(name) VALUES(?)`, name)
		if err != nil {
			if strings.Contains(err.Error(), "UNIQUE") {
				return "", ErrRefusal{"a user with this name already exists"}
			}
			return "", err
		}
		u = User{Name: name, Active: true}
		u.ID, err = r.LastInsertId()
		return "", err
	})
	return
}

func (s *Store) RenameUser(ctx context.Context, id int64, name string) error {
	if !validName(name) {
		return ErrInvalid{"name"}
	}
	return s.adminWrite(ctx, "user.rename", strconv.FormatInt(id, 10), nil, func(tx *sql.Tx) (string, error) {
		r, err := tx.Exec(`UPDATE users SET name=? WHERE id=?`, name, id)
		if err != nil && strings.Contains(err.Error(), "UNIQUE") {
			return "", ErrRefusal{"a user with this name already exists"}
		}
		return "", countOne(r, err)
	})
}

func countOne(r sql.Result, err error) error {
	if err != nil {
		return err
	}
	if n, _ := r.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetUserActive deactivates (bump) or activates a user.
func (s *Store) SetUserActive(ctx context.Context, id int64, active bool) error {
	return s.adminWrite(ctx, "user.active", strconv.FormatInt(id, 10), map[string]any{"active": active}, func(tx *sql.Tx) (string, error) {
		var was bool
		if err := tx.QueryRow(`SELECT active FROM users WHERE id=?`, id).Scan(&was); err != nil {
			return "", notFound(err)
		}
		if _, err := tx.Exec(`UPDATE users SET active=? WHERE id=?`, active, id); err != nil {
			return "", err
		}
		if was && !active {
			return "user deactivated", nil
		}
		return "", nil
	})
}

// SetUserOwner sets the owner mark (it only lets the owner be a project member; it gives no power).
func (s *Store) SetUserOwner(ctx context.Context, id int64, owner bool) error {
	return s.adminWrite(ctx, "user.owner", strconv.FormatInt(id, 10), map[string]any{"owner": owner}, func(tx *sql.Tx) (string, error) {
		r, err := tx.Exec(`UPDATE users SET owner=? WHERE id=?`, owner, id)
		return "", countOne(r, err)
	})
}

func notFound(err error) error {
	if err == sql.ErrNoRows {
		return ErrNotFound
	}
	return err
}

func (s *Store) ListUsers(ctx context.Context) (out []User, err error) {
	err = s.read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT id,name,active,owner FROM users ORDER BY id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var u User
			if err := rows.Scan(&u.ID, &u.Name, &u.Active, &u.Owner); err != nil {
				return err
			}
			out = append(out, u)
		}
		return rows.Err()
	})
	return
}

// SetBinding binds a device to a user. Changing an existing binding to another user bumps the epoch.
func (s *Store) SetBinding(ctx context.Context, deviceID string, userID int64) error {
	if deviceID == "" || len(deviceID) > 256 {
		return ErrInvalid{"device"}
	}
	defer s.reloadBound(ctx)
	return s.adminWrite(ctx, "binding.set", deviceID, map[string]any{"user": userID}, func(tx *sql.Tx) (string, error) {
		if err := exists(tx, "users", userID); err != nil {
			return "", err
		}
		var old int64
		err := tx.QueryRow(`SELECT user_id FROM device_bindings WHERE device_id=?`, deviceID).Scan(&old)
		if err == nil && old == userID {
			return "", nil
		}
		if err != nil && err != sql.ErrNoRows {
			return "", err
		}
		if _, err := tx.Exec(`INSERT INTO device_bindings(device_id,user_id) VALUES(?,?) ON CONFLICT(device_id) DO UPDATE SET user_id=excluded.user_id`, deviceID, userID); err != nil {
			return "", err
		}
		if old != 0 {
			return "binding changed", nil
		}
		return "", nil
	})
}

func (s *Store) RemoveBinding(ctx context.Context, deviceID string) error {
	defer s.reloadBound(ctx)
	return s.adminWrite(ctx, "binding.remove", deviceID, nil, func(tx *sql.Tx) (string, error) {
		if err := countOne(tx.Exec(`DELETE FROM device_bindings WHERE device_id=?`, deviceID)); err != nil {
			return "", err
		}
		return "binding removed", nil
	})
}

func (s *Store) ListBindings(ctx context.Context) (out []Binding, err error) {
	err = s.read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT device_id,user_id FROM device_bindings ORDER BY device_id`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var b Binding
			if err := rows.Scan(&b.DeviceID, &b.UserID); err != nil {
				return err
			}
			out = append(out, b)
		}
		return rows.Err()
	})
	return
}

// DeviceRevoked is called by the gateway when a device leaves its approved list.
func (s *Store) DeviceRevoked(ctx context.Context, deviceID string) error {
	return s.adminWrite(ctx, "device.revoked", deviceID, nil, func(*sql.Tx) (string, error) { return "device revoked", nil })
}

// CreateProject takes its id from the never-reused high-water mark. A slug of digits only is refused, because
// a lookup by "id-or-slug" treats digits as an id.
func (s *Store) CreateProject(ctx context.Context, slug, name string) (p Project, err error) {
	if !slugRe.MatchString(slug) || strings.Trim(slug, "0123456789") == "" {
		return p, ErrInvalid{"slug"}
	}
	if !validName(name) {
		return p, ErrInvalid{"name"}
	}
	err = s.adminWrite(ctx, "project.create", slug, nil, func(tx *sql.Tx) (string, error) {
		var n int
		if err := tx.QueryRow(`SELECT count(*) FROM projects WHERE slug=?`, slug).Scan(&n); err != nil {
			return "", err
		}
		if n > 0 {
			return "", ErrRefusal{"a project with this short name already exists"}
		}
		id, err := s.NextProjectID(tx)
		if err != nil {
			return "", err
		}
		_, err = tx.Exec(`INSERT INTO projects(id,slug,name) VALUES(?,?,?)`, id, slug, name)
		p = Project{ID: id, Slug: slug, Name: name}
		return "", err
	})
	return
}

// SetProjectArchived archives or unarchives (no epoch change: reads go on, only writes stop).
func (s *Store) SetProjectArchived(ctx context.Context, id int64, archived bool) error {
	return s.adminWrite(ctx, "project.archive", strconv.FormatInt(id, 10), map[string]any{"archived": archived}, func(tx *sql.Tx) (string, error) {
		if isSegment(id) {
			return "", ErrNotFound
		}
		r, err := tx.Exec(`UPDATE projects SET archived=? WHERE id=?`, archived, id)
		return "", countOne(r, err)
	})
}

func (s *Store) AdminListProjects(ctx context.Context) (out []Project, err error) {
	err = s.read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT id,slug,name,archived FROM projects WHERE id<=? ORDER BY id`, segmentBase)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var p Project
			if err := rows.Scan(&p.ID, &p.Slug, &p.Name, &p.Archived); err != nil {
				return err
			}
			out = append(out, p)
		}
		return rows.Err()
	})
	return
}

// SetMember changes only the direct origin. Surviving origins determine the effective role.
func (s *Store) SetMember(ctx context.Context, projectID, userID int64, role Role) error {
	if roleRank[role] == 0 {
		return ErrInvalid{"role"}
	}
	return s.adminWrite(ctx, "member.set", strconv.FormatInt(projectID, 10), map[string]any{"user": userID, "role": role}, func(tx *sql.Tx) (string, error) {
		if err := existsProject(tx, projectID); err != nil {
			return "", err
		}
		if err := exists(tx, "users", userID); err != nil {
			return "", err
		}
		if _, err := tx.Exec(`INSERT INTO direct_memberships(project_id,user_id,role) VALUES(?,?,?) ON CONFLICT(project_id,user_id) DO UPDATE SET role=excluded.role`, projectID, userID, role); err != nil {
			return "", err
		}
		return rebuildMemberships(tx)
	})
}

func (s *Store) RemoveMember(ctx context.Context, projectID, userID int64) error {
	return s.adminWrite(ctx, "member.remove", strconv.FormatInt(projectID, 10), map[string]any{"user": userID}, func(tx *sql.Tx) (string, error) {
		if err := countOne(tx.Exec(`DELETE FROM direct_memberships WHERE project_id=? AND user_id=?`, projectID, userID)); err != nil {
			if errors.Is(err, ErrNotFound) {
				var team, project, user string
				if tx.QueryRow(`SELECT t.name,p.slug,u.name FROM team_assignments a JOIN teams t ON t.id=a.team_id JOIN projects p ON p.id=a.project_id JOIN users u ON u.id=a.user_id WHERE a.project_id=? AND a.user_id=? ORDER BY a.team_id LIMIT 1`, projectID, userID).Scan(&team, &project, &user) == nil {
					return "", ErrRefusal{"no direct role; team access: minigw gw team access remove " + team + " " + project + " " + user}
				}
			}
			return "", err
		}
		return rebuildMemberships(tx)
	})
}

func (s *Store) ListMembers(ctx context.Context, projectID int64) (out []Member, err error) {
	err = s.read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT m.user_id,u.name,m.role FROM memberships m JOIN users u ON u.id=m.user_id WHERE m.project_id=? ORDER BY m.user_id`, projectID)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var m Member
			if err := rows.Scan(&m.UserID, &m.Name, &m.Role); err != nil {
				return err
			}
			out = append(out, m)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		rows.Close()
		for i := range out {
			out[i].Origins, err = membershipOrigins(tx, projectID, out[i].UserID)
			if err != nil {
				return err
			}
		}
		return nil
	})
	return
}

// AddGrant: the members of reader may read class of source. Only structure and data can be granted.
func (s *Store) AddGrant(ctx context.Context, source, reader int64, class Class) error {
	if class != Structure && class != Data {
		return ErrInvalid{"class"}
	}
	if source == reader {
		return ErrRefusal{"a project cannot be granted to itself"}
	}
	return s.adminWrite(ctx, "grant.add", strconv.FormatInt(source, 10), map[string]any{"reader": reader, "class": class}, func(tx *sql.Tx) (string, error) {
		if err := existsProject(tx, source); err != nil {
			return "", err
		}
		if err := existsProject(tx, reader); err != nil {
			return "", err
		}
		_, err := tx.Exec(`INSERT OR IGNORE INTO grants(source_id,reader_id,class) VALUES(?,?,?)`, source, reader, class)
		return "", err
	})
}

func (s *Store) RemoveGrant(ctx context.Context, source, reader int64, class Class) error {
	return s.adminWrite(ctx, "grant.remove", strconv.FormatInt(source, 10), map[string]any{"reader": reader, "class": class}, func(tx *sql.Tx) (string, error) {
		if err := countOne(tx.Exec(`DELETE FROM grants WHERE source_id=? AND reader_id=? AND class=?`, source, reader, class)); err != nil {
			return "", err
		}
		return "grant removed", nil
	})
}

func (s *Store) ListGrants(ctx context.Context) (out []Grant, err error) {
	err = s.read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT source_id,reader_id,class FROM grants ORDER BY source_id,reader_id,class`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var g Grant
			if err := rows.Scan(&g.SourceID, &g.ReaderID, &g.Class); err != nil {
				return err
			}
			out = append(out, g)
		}
		return rows.Err()
	})
	return
}
