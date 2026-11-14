package core

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strconv"
	"sync"
	"time"
)

type Role string
type Class string

const (
	Reader    Role = "reader"
	Developer Role = "developer"
	Admin     Role = "admin"

	Structure Class = "structure"
	Data      Class = "data"
	Private   Class = "private"
)

var allClasses = []Class{Structure, Data, Private}

func validClass(c Class) bool { return c == Structure || c == Data || c == Private }

var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}$`)

// Limits of section 6 (variables so that tests can use small ones).
var (
	maxTitle       = 256
	maxBody        = 64 << 10
	maxNotes       = 10000
	maxBytes       = int64(100 << 20)
	writesPerMin   = 60
	readsPerMin    = 600
	idempotencyTTL = 24 * time.Hour
)

// perm is one cell group of the permission matrix of section 2.
type perm uint8

const (
	pRead perm = 1 << iota
	pReadOwn
	pCreate
	pWriteOwn
	pWriteAny
)

// matrix is the source of truth for every decision (everything not listed is denied). Keep it identical to the
// table in docs/plans/contracts-0.6.0.md section 2.
var matrix = map[Role]map[Class]perm{
	Reader:    {Structure: pRead, Data: 0, Private: pReadOwn},
	Developer: {Structure: pRead | pCreate | pWriteOwn, Data: pRead | pCreate | pWriteOwn, Private: pReadOwn | pCreate | pWriteOwn},
	Admin:     {Structure: pRead | pCreate | pWriteOwn | pWriteAny, Data: pRead | pCreate | pWriteOwn | pWriteAny, Private: pRead | pCreate | pWriteOwn | pWriteAny},
}

// grantPerm: a grant for class c gives read of c only.
func grantPerm(c Class) perm {
	if c == Structure || c == Data {
		return pRead
	}
	return 0
}

// shrink lists the class changes an author may make on their own note without being admin
// (section 2, "Class changes"); every other class change is admin only.
var devTransitions = map[[2]Class]bool{
	{Structure, Data}: true, {Data, Structure}: true, {Private, Structure}: true, {Private, Data}: true,
}

// access is what one user has in one project: the direct role and the classes reached through grants.
type access struct {
	Role   Role
	Grants map[Class]bool
}

func (a access) visible() bool { return a.Role != "" || len(a.Grants) > 0 }

func (a access) perms(c Class) perm {
	p := matrix[a.Role][c]
	if a.Grants[c] {
		p |= grantPerm(c)
	}
	return p
}

// can is THE authorization function: p on a note of class c, own = the caller is its author.
func (a access) can(p perm, c Class, own bool) bool {
	m := a.perms(c)
	switch p {
	case pRead:
		return m&pRead != 0 || own && m&pReadOwn != 0
	case pWriteOwn: // change an existing note
		return m&pWriteAny != 0 || own && m&pWriteOwn != 0
	}
	return m&p != 0
}

// canMove: may the caller change a note of class from to class to (both writes already allowed as a plain change)?
func (a access) canMove(from, to Class, own bool) bool {
	if from == to || a.Role == Admin {
		return true
	}
	return own && devTransitions[[2]Class{from, to}]
}

// loadAccess reads the role and the grants of user in project from the current state.
func loadAccess(tx *sql.Tx, user, project int64) (a access, err error) {
	err = tx.QueryRow(`SELECT role FROM memberships WHERE project_id=? AND user_id=?`, project, user).Scan(&a.Role)
	if err == sql.ErrNoRows {
		err = nil
	}
	if err != nil {
		return
	}
	rows, err := tx.Query(`SELECT DISTINCT g.class FROM grants g JOIN memberships m ON m.project_id=g.reader_id AND m.user_id=? WHERE g.source_id=?`, user, project)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var c Class
		if err = rows.Scan(&c); err != nil {
			return
		}
		if a.Grants == nil {
			a.Grants = map[Class]bool{}
		}
		a.Grants[c] = true
	}
	return a, rows.Err()
}

// Principal is built by the gateway for one request from the proven device.
type Principal struct {
	UserID   int64
	DeviceID string
}

// Principal checks steps 2 and 3 of section 1: approved now, bound, user active. Any failure is the one error
// ErrNoProjectAccess; the wrapped text is only for the gateway log.
func (s *Store) Principal(ctx context.Context, deviceID string, approved bool) (Principal, error) {
	if !approved {
		return Principal{}, noAccess("device not approved")
	}
	var p Principal
	err := s.read(ctx, func(tx *sql.Tx) error {
		var active bool
		err := tx.QueryRow(`SELECT u.id,u.active FROM device_bindings b JOIN users u ON u.id=b.user_id WHERE b.device_id=?`, deviceID).Scan(&p.UserID, &active)
		if err == sql.ErrNoRows {
			return noAccess("device not bound")
		}
		if err == nil && !active {
			err = noAccess("user inactive")
		}
		return err
	})
	p.DeviceID = deviceID
	if err != nil {
		return Principal{}, err
	}
	return p, nil
}

// checkPrincipal repeats the binding check inside the operation's own transaction (nothing is cached).
func checkPrincipal(tx *sql.Tx, p Principal) error {
	var n int
	if err := tx.QueryRow(`SELECT count(*) FROM device_bindings b JOIN users u ON u.id=b.user_id WHERE b.device_id=? AND b.user_id=? AND u.active=1`, p.DeviceID, p.UserID).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		return noAccess("binding or user changed")
	}
	return nil
}

// read runs fn in a deferred (snapshot) transaction on the read handle.
func (s *Store) read(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.rdb.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	return fn(tx)
}

// Rate limits: fixed window per user over all devices; refused requests count. In memory: one process.
type window struct {
	start         time.Time
	reads, writes int
}
type rateLimits struct {
	mu sync.Mutex
	w  map[int64]*window
}

// rate counts the request now and returns the limit error; the caller returns it only after the checks that
// come first (principal, visible, allowed, archived, revision), so an exhausted user still gets 404/403/409.
func (s *Store) rate(user int64, write bool) error {
	r := &s.rl
	r.mu.Lock()
	defer r.mu.Unlock()
	now := s.now()
	w := r.w[user]
	if w == nil || !now.Before(w.start.Add(time.Minute)) {
		w = &window{start: now}
		r.w[user] = w
	}
	n, limit := &w.reads, readsPerMin
	if write {
		n, limit = &w.writes, writesPerMin
	}
	*n++
	if *n > limit {
		return ErrLimit{RetryAfter: w.start.Add(time.Minute).Sub(now)}
	}
	return nil
}

func actor(p Principal) string { return "user:" + strconv.FormatInt(p.UserID, 10) }

func ms(t time.Time) int64 { return t.UnixMilli() }

func fromMs(n int64) time.Time { return time.UnixMilli(n).UTC() }

func errf(what string, a ...any) error { return ErrInvalid{What: fmt.Sprintf(what, a...)} }
