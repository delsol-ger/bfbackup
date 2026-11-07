// Package store persists configuration, the file index and run history in
// SQLite (pure Go driver, no cgo).
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"time"

	_ "modernc.org/sqlite"
)

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

// Store wraps the database.
type Store struct {
	db  *sql.DB
	now func() time.Time
}

// Open opens (and creates or migrates) the database at path.
func Open(path string) (*Store, error) {
	q := url.Values{}
	for _, p := range []string{"busy_timeout(10000)", "journal_mode(WAL)", "foreign_keys(1)", "synchronous(NORMAL)"} {
		q.Add("_pragma", p)
	}
	q.Set("_txlock", "immediate")
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(8)
	s := &Store{db: db, now: time.Now}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	os.Chmod(path, 0o600)
	return s, nil
}

// SetClock replaces the clock (tests).
func (s *Store) SetClock(now func() time.Time) { s.now = now }

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// DB exposes the handle for tests.
func (s *Store) DB() *sql.DB { return s.db }

var migrations = []string{
	// v1
	`
CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE sessions (token_hash TEXT PRIMARY KEY, expires_at INTEGER NOT NULL);
CREATE TABLE nodes (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL UNIQUE,
	secret BLOB NOT NULL,
	created_at INTEGER NOT NULL,
	last_seen INTEGER NOT NULL DEFAULT 0,
	hostname TEXT NOT NULL DEFAULT '',
	version TEXT NOT NULL DEFAULT ''
);
CREATE TABLE jobs (
	id INTEGER PRIMARY KEY,
	name TEXT NOT NULL UNIQUE,
	source_node TEXT NOT NULL,
	source_path TEXT NOT NULL,
	includes TEXT NOT NULL DEFAULT '[]',
	excludes TEXT NOT NULL DEFAULT '[]',
	compare_mode TEXT NOT NULL DEFAULT 'mtime',
	pre_hook TEXT NOT NULL DEFAULT '',
	post_hook TEXT NOT NULL DEFAULT '',
	protect_pct REAL NOT NULL DEFAULT 10,
	protect_min INTEGER NOT NULL DEFAULT 100,
	workers INTEGER NOT NULL DEFAULT 4,
	enabled INTEGER NOT NULL DEFAULT 1,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE TABLE targets (
	id INTEGER PRIMARY KEY,
	job_id INTEGER NOT NULL REFERENCES jobs(id) ON DELETE CASCADE,
	node_id TEXT NOT NULL,
	path TEXT NOT NULL,
	schedule TEXT NOT NULL DEFAULT '',
	verify_schedule TEXT NOT NULL DEFAULT '',
	verify_mode TEXT NOT NULL DEFAULT 'quick',
	enabled INTEGER NOT NULL DEFAULT 1,
	stored_files INTEGER NOT NULL DEFAULT 0,
	stored_bytes INTEGER NOT NULL DEFAULT 0,
	last_scan_run INTEGER NOT NULL DEFAULT 0,
	UNIQUE(node_id, path)
);
CREATE TABLE runs (
	id INTEGER PRIMARY KEY,
	job_id INTEGER NOT NULL,
	target_id INTEGER NOT NULL,
	kind TEXT NOT NULL,
	status TEXT NOT NULL,
	trigger TEXT NOT NULL DEFAULT '',
	started_at INTEGER NOT NULL,
	finished_at INTEGER NOT NULL DEFAULT 0,
	files_transferred INTEGER NOT NULL DEFAULT 0,
	bytes_transferred INTEGER NOT NULL DEFAULT 0,
	stats TEXT NOT NULL DEFAULT '{}',
	error TEXT NOT NULL DEFAULT '',
	details TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX runs_target ON runs(target_id, id);
CREATE INDEX runs_started ON runs(started_at);
CREATE TABLE run_errors (
	run_id INTEGER NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
	path TEXT NOT NULL,
	message TEXT NOT NULL
);
CREATE INDEX run_errors_run ON run_errors(run_id);
CREATE TABLE files (
	target_id INTEGER NOT NULL REFERENCES targets(id) ON DELETE CASCADE,
	dir TEXT NOT NULL,
	name TEXT NOT NULL,
	type INTEGER NOT NULL,
	size INTEGER NOT NULL,
	mtime INTEGER NOT NULL,
	mode INTEGER NOT NULL,
	hash TEXT NOT NULL DEFAULT '',
	link TEXT NOT NULL DEFAULT '',
	synced_at INTEGER NOT NULL,
	seen_run INTEGER NOT NULL,
	PRIMARY KEY (target_id, dir, name)
) WITHOUT ROWID;
CREATE INDEX files_seen ON files(target_id, seen_run);
CREATE TABLE dirs (
	target_id INTEGER NOT NULL REFERENCES targets(id) ON DELETE CASCADE,
	path TEXT NOT NULL,
	parent TEXT NOT NULL,
	PRIMARY KEY (target_id, path)
) WITHOUT ROWID;
CREATE INDEX dirs_parent ON dirs(target_id, parent);
CREATE TABLE audit (
	id INTEGER PRIMARY KEY,
	ts INTEGER NOT NULL,
	action TEXT NOT NULL,
	detail TEXT NOT NULL
);
`,
	// v2: jobs can opt out of the global exclusion list.
	`ALTER TABLE jobs ADD COLUMN ignore_global_excludes INTEGER NOT NULL DEFAULT 0;`,
	// v3: last disks a peer reported, shown while it is offline.
	`
ALTER TABLE nodes ADD COLUMN disks TEXT NOT NULL DEFAULT '[]';
ALTER TABLE nodes ADD COLUMN disks_at INTEGER NOT NULL DEFAULT 0;
`,
	// v4: offline alerts per peer (replacing the global minutes setting) and
	// backups that start when a peer connects.
	`
ALTER TABLE nodes ADD COLUMN offline_notify INTEGER NOT NULL DEFAULT 1;
ALTER TABLE nodes ADD COLUMN offline_hours INTEGER NOT NULL DEFAULT 1;
UPDATE nodes SET offline_hours = MAX(1, (COALESCE((SELECT CAST(value AS INTEGER) FROM settings WHERE key = 'node_offline_minutes'), 10) + 59) / 60);
ALTER TABLE targets ADD COLUMN run_on_connect INTEGER NOT NULL DEFAULT 0;
`,
	// v5: jobs can clean excluded files off their targets; run errors say
	// on which node they happened.
	`
ALTER TABLE jobs ADD COLUMN clean_excluded INTEGER NOT NULL DEFAULT 0;
ALTER TABLE run_errors ADD COLUMN node TEXT NOT NULL DEFAULT '';
`,
	// v6: job modes (copy/sync), what to do with existing files, what happens
	// to replaced copies; runs can be dismissed from the attention list.
	`
ALTER TABLE jobs ADD COLUMN mode TEXT NOT NULL DEFAULT 'copy';
ALTER TABLE jobs ADD COLUMN on_conflict TEXT NOT NULL DEFAULT 'source';
ALTER TABLE jobs ADD COLUMN old_copies TEXT NOT NULL DEFAULT 'trash';
ALTER TABLE runs ADD COLUMN acknowledged INTEGER NOT NULL DEFAULT 0;
`,
	// v7: USB drives on the controller. Their targets have node_id
	// "usb:<uuid>". Existing notification choices get the drive events.
	`
CREATE TABLE drives (
	uuid TEXT PRIMARY KEY,
	name TEXT NOT NULL UNIQUE,
	created_at INTEGER NOT NULL,
	last_seen INTEGER NOT NULL DEFAULT 0,
	last_mount TEXT NOT NULL DEFAULT '',
	fstype TEXT NOT NULL DEFAULT '',
	total INTEGER NOT NULL DEFAULT 0,
	free INTEGER NOT NULL DEFAULT 0,
	on_connect INTEGER NOT NULL DEFAULT 1,
	verify_after INTEGER NOT NULL DEFAULT 0,
	overdue_days INTEGER NOT NULL DEFAULT 30,
	overdue_notified INTEGER NOT NULL DEFAULT 0
);
UPDATE settings SET value = value || ',drive_done,drive_overdue' WHERE key = 'notify_events' AND value <> '';
`,
}

func (s *Store) migrate(ctx context.Context) error {
	var v int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&v); err != nil {
		return err
	}
	for i := v; i < len(migrations); i++ {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// withTx runs fn in a transaction.
func (s *Store) withTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// ---- settings ----

// Setting returns a setting or def when unset.
func (s *Store) Setting(ctx context.Context, key, def string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key = ?", key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return def, nil
	}
	return v, err
}

// SetSetting stores a setting.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO settings(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", key, value)
	return err
}

// Settings returns all settings.
func (s *Store) Settings(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT key, value FROM settings")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// ---- sessions ----

// CreateSession stores a session token hash.
func (s *Store) CreateSession(ctx context.Context, tokenHash string, expires time.Time) error {
	_, err := s.db.ExecContext(ctx, "INSERT INTO sessions(token_hash, expires_at) VALUES(?, ?)", tokenHash, expires.Unix())
	return err
}

// SessionValid reports whether a session exists and has not expired.
func (s *Store) SessionValid(ctx context.Context, tokenHash string) (bool, error) {
	exp, err := s.SessionExpires(ctx, tokenHash)
	return err == nil && exp.After(s.now()), err
}

// SessionExpires returns when a session expires, or the zero time if it does not exist.
func (s *Store) SessionExpires(ctx context.Context, tokenHash string) (time.Time, error) {
	var exp int64
	err := s.db.QueryRowContext(ctx, "SELECT expires_at FROM sessions WHERE token_hash = ?", tokenHash).Scan(&exp)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	return time.Unix(exp, 0), nil
}

// ExtendSession moves the expiry of a session (sliding sessions).
func (s *Store) ExtendSession(ctx context.Context, tokenHash string, expires time.Time) error {
	_, err := s.db.ExecContext(ctx, "UPDATE sessions SET expires_at = ? WHERE token_hash = ?", expires.Unix(), tokenHash)
	return err
}

// DeleteSession removes one session.
func (s *Store) DeleteSession(ctx context.Context, tokenHash string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM sessions WHERE token_hash = ?", tokenHash)
	return err
}

// DeleteAllSessions logs everybody out (password change).
func (s *Store) DeleteAllSessions(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM sessions")
	return err
}

// PurgeSessions removes expired sessions.
func (s *Store) PurgeSessions(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM sessions WHERE expires_at <= ?", s.now().Unix())
	return err
}

// ---- audit ----

// AuditEntry is one line of the audit log.
type AuditEntry struct {
	ID     int64  `json:"id"`
	TS     int64  `json:"ts"`
	Action string `json:"action"`
	Detail string `json:"detail"`
}

// Audit appends to the audit log.
func (s *Store) Audit(ctx context.Context, action, detail string) error {
	_, err := s.db.ExecContext(ctx, "INSERT INTO audit(ts, action, detail) VALUES(?, ?, ?)", s.now().Unix(), action, detail)
	return err
}

// ListAudit returns the newest audit entries first.
func (s *Store) ListAudit(ctx context.Context, limit, offset int) ([]AuditEntry, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id, ts, action, detail FROM audit ORDER BY id DESC LIMIT ? OFFSET ?", limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var a AuditEntry
		if err := rows.Scan(&a.ID, &a.TS, &a.Action, &a.Detail); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Snapshot writes a consistent copy of the database to path.
func (s *Store) Snapshot(ctx context.Context, path string) error {
	os.Remove(path)
	_, err := s.db.ExecContext(ctx, "VACUUM INTO ?", path)
	if err == nil {
		os.Chmod(path, 0o600)
	}
	return err
}
