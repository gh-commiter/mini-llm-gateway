// Package core is the storage of the 0.6.0 gateway core: one SQLite file with a life cycle (migrations, snapshots,
// backup, restore), the epoch, the project-id high-water mark and the audit trail. No HTTP and no policy here.
package core

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

const (
	dbFile    = "core.db"
	stateFile = "core.state"
	auditFile = "core.audit.log"
	lockFile  = "core.lock"
	keepSnaps = 3
)

var (
	ErrNewerSchema = errors.New("core database has a newer schema than this program")
	ErrLocked      = errors.New("core database is in use by another process")
	ErrForeign     = errors.New("core database belongs to another gateway")
)

// Options for Open. ProtectDir makes the folder private (on Windows a protected ACL that the files inherit; the
// caller's protectDir). ProtectFile makes one file user-only before anything is written to it (used for backups,
// which may be written outside the private folder); nil = the package's own (chmod 0600, a protected ACL on Windows).
type Options struct {
	ProtectDir  func(dir string) error
	ProtectFile func(path string) error
}

// Store is an open core database. Reads use DB(); every write goes through write().
type Store struct {
	dir   string
	opts  Options
	db    *sql.DB
	lock  *os.File
	rdb   *sql.DB      // read-only handle: deferred transactions, so readers never wait for the writer
	mu    sync.Mutex   // one writer; guards st
	epoch atomic.Int64 // mirror of st.Epoch for lock-free reads
	now   func() time.Time
	rl    rateLimits
	bound atomic.Pointer[map[string]int64] // device -> user snapshot for event attribution
	st    state
	gw    string
}

// state is what must outlive a restore: it is kept in core.state as well as in the database (max of both wins).
type state struct {
	GatewayID   string `json:"gateway_id"`
	Epoch       int64  `json:"epoch"`
	ProjectHigh int64  `json:"project_high"`
}

// migrations is ordered and forward only; migration i brings the schema to version i+1. Tests replace it.
var migrations = []func(*sql.Tx) error{migrate1, migrate2, migrate3, migrate4, migrate5, migrate6, migrate7, migrate8, migrate9, migrate10, migrate11, migrate12, migrate13, migrate14, migrate15, migrate16, migrate17}

func migrate1(tx *sql.Tx) error {
	_, err := tx.Exec(`
CREATE TABLE meta(key TEXT PRIMARY KEY, value TEXT NOT NULL) WITHOUT ROWID;
INSERT INTO meta VALUES('epoch','1'),('project_high','0');
CREATE TABLE audit_events(id INTEGER PRIMARY KEY AUTOINCREMENT, time TEXT NOT NULL, actor TEXT NOT NULL,
  action TEXT NOT NULL, subject TEXT NOT NULL, detail TEXT NOT NULL DEFAULT '{}');`)
	return err
}

// Open opens (creating if needed) <dir>/core.db and migrates it. It holds an exclusive lock on the folder
// until Close; a second Open (also from another process) fails with ErrLocked.
func Open(dir string, opts Options) (*Store, error) {
	dir, err := filepath.Abs(dir) // SQLite gets a URI; a relative path must not turn into an unrelated absolute one
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if opts.ProtectDir != nil {
		if err := opts.ProtectDir(dir); err != nil {
			return nil, err
		}
	}
	lock, err := lockDir(dir)
	if err != nil {
		return nil, err
	}
	if err := recoverRestore(dir); err != nil {
		lock.Close()
		return nil, err
	}
	if err := checkLinks(dir); err != nil {
		lock.Close()
		return nil, err
	}
	s := &Store{dir: dir, opts: opts, lock: lock, now: time.Now, rl: rateLimits{w: map[int64]*window{}}}
	if err := s.open(); err != nil {
		if s.db != nil {
			s.db.Close()
		}
		if s.rdb != nil {
			s.rdb.Close()
		}
		lock.Close()
		return nil, err
	}
	return s, nil
}

func lockDir(dir string) (*os.File, error) {
	p := filepath.Join(dir, lockFile)
	if fi, err := os.Lstat(p); err == nil && !fi.Mode().IsRegular() {
		return nil, errors.New(p + " is not a regular file")
	}
	f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, err
	}
	if busy, err := tryLock(f); err != nil {
		f.Close()
		if busy {
			return nil, fmt.Errorf("%w (%s)", ErrLocked, dir)
		}
		return nil, err
	}
	return f, nil // the lock ends when the file is closed (or the process dies)
}

func (s *Store) open() error {
	side, err := readState(s.dir)
	if err != nil {
		return err
	}
	path := filepath.Join(s.dir, dbFile)
	if fi, err := os.Lstat(path); err == nil && !fi.Mode().IsRegular() {
		return errors.New(path + " is not a regular file")
	}
	if err := createPrivate(path); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	if s.db, err = openDB(path); err != nil {
		return err
	}
	ctx := context.Background()
	if err := s.migrate(ctx); err != nil {
		return err
	}
	mm, err := readMetaFull(ctx, s.db)
	if err != nil {
		return err
	}
	m := mm.state
	// A lost database file (fresh create) adopts the id of the sidecar, so a later restore of a backup still matches.
	if m.GatewayID == "" {
		m.GatewayID = side.GatewayID
		if m.GatewayID == "" {
			b := make([]byte, 16)
			rand.Read(b)
			m.GatewayID = hex.EncodeToString(b)
		}
	} else if side.GatewayID != "" && side.GatewayID != m.GatewayID {
		return fmt.Errorf("%w: core.state has another gateway id", ErrForeign)
	}
	m.Epoch, m.ProjectHigh = max(m.Epoch, side.Epoch), max(m.ProjectHigh, side.ProjectHigh)
	s.st, s.gw = m, m.GatewayID
	s.epoch.Store(m.Epoch)
	defer s.reloadBound(ctx)
	if err := s.write(ctx, markInterrupted); err != nil { // events a crash cut off (a spool replay is the caller's business)
		return err
	}
	if _, err := s.FoldOldUsage(ctx); err != nil {
		return err
	}
	if s.rdb, err = sql.Open("sqlite", dsn(path, "_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)&_pragma=query_only(1)&_pragma=trusted_schema(0)")); err != nil {
		return err
	}
	return s.write(ctx, func(tx *sql.Tx) error { return s.persist(tx) })
}

func openDB(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dsn(path, "_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=synchronous(FULL)&_pragma=trusted_schema(0)&_txlock=immediate"))
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	os.Chmod(path, 0o600) // belt and braces; WAL and SHM copy the mode of the main file
	return db, nil
}

// Close releases the database and the folder lock.
func (s *Store) Close() error {
	s.rdb.Close()
	err := s.db.Close()
	s.lock.Close()
	return err
}

// DB is the read-only handle (query_only); writes go through write(), which serializes them.
func (s *Store) DB() *sql.DB { return s.rdb }

// Dir is the folder of the database.
func (s *Store) Dir() string { return s.dir }

// GatewayID is created once and kept by every backup.
func (s *Store) GatewayID() string { return s.gw }

// write runs fn in one immediate transaction. One writer at a time in this process; the folder lock keeps other
// processes out, so a writer never meets "database is locked".
func (s *Store) write(ctx context.Context, fn func(*sql.Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	epoch, high := dbInt(tx, "epoch"), dbInt(tx, "project_high")
	next := s.st
	next.Epoch, next.ProjectHigh = max(next.Epoch, epoch), max(next.ProjectHigh, high)
	// Keep SQL-allocated marks safe across a lost database and a restore too.
	if next != s.st {
		if err := writeState(s.dir, next); err != nil {
			tx.Rollback()
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// Imports allocate metadata in SQL; publish it only after the transaction commits.
	s.st.Epoch, s.st.ProjectHigh = max(s.st.Epoch, epoch), max(s.st.ProjectHigh, high)
	s.epoch.Store(s.st.Epoch)
	return nil
}

// persist writes the state to the database (in tx) and to the sidecar. Callers hold s.mu. The sidecar is written
// before the commit: after a crash or rollback it may be ahead of the database, which only skips numbers.
func (s *Store) persist(tx *sql.Tx) error {
	for k, v := range map[string]string{"gateway_id": s.st.GatewayID, "epoch": i64(s.st.Epoch), "project_high": i64(s.st.ProjectHigh)} {
		if err := setMeta(tx, k, v); err != nil {
			return err
		}
	}
	return writeState(s.dir, s.st)
}

func i64(n int64) string { return strconv.FormatInt(n, 10) }

func setMeta(tx *sql.Tx, k, v string) error {
	_, err := tx.Exec(`INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, k, v)
	return err
}

// meta is the content of the meta table (Schema 0 = no meta table yet).
type meta struct {
	state
	Schema int
}

func readMetaFull(ctx context.Context, q interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}) (m meta, err error) {
	rows, err := q.QueryContext(ctx, `SELECT key,value FROM meta`)
	if err != nil {
		return m, err
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return m, err
		}
		n, _ := strconv.ParseInt(v, 10, 64)
		switch k {
		case "schema":
			if n < 0 || n > math.MaxInt32 {
				return m, fmt.Errorf("meta: schema %q out of range", v)
			}
			m.Schema = int(n)
		case "gateway_id":
			m.GatewayID = v
		case "epoch":
			m.Epoch = n
		case "project_high":
			m.ProjectHigh = n
		}
	}
	return m, rows.Err()
}

func readState(dir string) (st state, err error) {
	b, err := os.ReadFile(filepath.Join(dir, stateFile))
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err == nil {
		err = json.Unmarshal(b, &st)
	}
	if err != nil {
		return st, fmt.Errorf("core.state: %w", err)
	}
	return st, nil
}

func writeState(dir string, st state) error {
	b, _ := json.Marshal(st)
	return writeAtomic(filepath.Join(dir, stateFile), append(b, '\n'))
}

// schemaVersion is 0 for a database without a meta table.
func schemaVersion(ctx context.Context, db *sql.DB) (int, error) {
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name='meta'`).Scan(&n); err != nil || n == 0 {
		return 0, err
	}
	m, err := readMetaFull(ctx, db)
	return m.Schema, err
}

// Epoch is the gateway-wide counter; it never goes down.
func (s *Store) Epoch(ctx context.Context) (int64, error) {
	return s.epoch.Load(), nil
}

// BumpEpoch raises the epoch by one and audits the reason.
func (s *Store) BumpEpoch(ctx context.Context, reason string) (n int64, err error) {
	err = s.write(ctx, func(tx *sql.Tx) error { n, err = s.BumpEpochTx(tx, reason); return err })
	return n, err
}

// BumpEpochTx is for use inside write() next to the change that takes access away. s.mu is held by the caller.
func (s *Store) BumpEpochTx(tx *sql.Tx, reason string) (int64, error) {
	s.st.Epoch = max(s.st.Epoch, dbInt(tx, "epoch")) + 1
	if err := s.persist(tx); err != nil {
		return 0, err
	}
	s.epoch.Store(s.st.Epoch)
	return s.st.Epoch, Audit(tx, "core", "epoch.bump", i64(s.st.Epoch), map[string]any{"reason": reason})
}

// NextProjectID returns an id above every id ever handed out, also after a restore. Inside write().
func (s *Store) NextProjectID(tx *sql.Tx) (int64, error) {
	s.st.ProjectHigh = max(s.st.ProjectHigh, dbInt(tx, "project_high")) + 1
	return s.st.ProjectHigh, s.persist(tx)
}

func dbInt(tx *sql.Tx, key string) (n int64) {
	var v string
	if tx.QueryRow(`SELECT value FROM meta WHERE key=?`, key).Scan(&v) == nil {
		n, _ = strconv.ParseInt(v, 10, 64)
	}
	return n
}

// AuditEvent is one metadata-only row of audit_events.
type AuditEvent struct {
	ID      int64
	Time    time.Time
	Actor   string
	Action  string
	Subject string
	Detail  json.RawMessage
}

// Audit adds a row inside the caller's transaction (so the event and the change commit together).
func Audit(tx *sql.Tx, actor, action, subject string, detail any) error {
	b, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	if detail == nil {
		b = []byte("{}")
	}
	_, err = tx.Exec(`INSERT INTO audit_events(time,actor,action,subject,detail) VALUES(?,?,?,?,?)`,
		time.Now().UTC().Format(time.RFC3339Nano), actor, action, subject, string(b))
	return err
}

// AuditEvents pages through the events with id > after, oldest first (limit 1..500, default 100).
func (s *Store) AuditEvents(ctx context.Context, after int64, limit int) ([]AuditEvent, error) {
	if limit < 1 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,time,actor,action,subject,detail FROM audit_events WHERE id>? ORDER BY id LIMIT ?`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEvent
	for rows.Next() {
		var e AuditEvent
		var ts, d string
		if err := rows.Scan(&e.ID, &ts, &e.Actor, &e.Action, &e.Subject, &d); err != nil {
			return nil, err
		}
		e.Time, _ = time.Parse(time.RFC3339Nano, ts)
		e.Detail = json.RawMessage(d)
		out = append(out, e)
	}
	return out, rows.Err()
}

// CheckLinks: a hard link to a core file from another folder (an allowed folder of an agent) would bypass every deny
// by path: the same inode under another name. The core refuses to open when a file of the core folder or its backups
// folder has more than one name (#106). Called by Open and by the gateway before each agent launch (a cheap stat).
func CheckLinks(dir string) error { return checkLinks(dir) }

func checkLinks(dir string) error {
	type file struct {
		path, id string
		links    uint64
	}
	var files []file
	inside := map[string]uint64{} // names of an identity within the core folder and its backups folder
	for _, d := range []string{dir, filepath.Join(dir, BackupFolder)} {
		entries, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		for _, e := range entries {
			p := filepath.Join(d, e.Name())
			if fi, err := os.Lstat(p); err == nil && fi.Mode().IsRegular() {
				n, id := linkInfo(p, fi)
				files = append(files, file{p, id, n})
				inside[id]++
			}
		}
	}
	for _, f := range files {
		// more names than the core folder itself holds (a failed restore keeps the old database as a snapshot: two names, both inside)
		if f.links > inside[f.id] {
			return fmt.Errorf("%w: %s has %d names, %d of them outside the core folder (a hard link to it exists elsewhere, and an agent could read the notes through that name): remove the other link", ErrHardLinked, f.path, f.links, f.links-inside[f.id])
		}
	}
	return nil
}
