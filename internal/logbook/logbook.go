// Package logbook is the activity log shown in the web UI. It has two parts:
// the main log (errors and events of runs, peers and the system) and the
// file log (one line per file a run touched or had a problem with). An entry
// with a Path goes to the file log. Both live in their own SQLite file, so a
// busy log never bloats the main database or its daily snapshots.
package logbook

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

// Levels. A book at level n keeps entries with level <= n.
const (
	LevelError = 1 // failures only
	LevelEvent = 2 // runs, peers, settings, maintenance; file issues
	LevelFile  = 3 // every file copied, restored, trashed, …
	LevelTrace = 4 // every decision: unchanged files, comparisons, each step
)

// Levels of file log entries.
const (
	FileError = LevelError // the file could not be handled
	FileIssue = LevelEvent // worth a look: unknown on the target, gone from the source, skipped
	FileOK    = LevelFile  // done as expected
	FileTrace = LevelTrace // a decision that changed nothing, e.g. unchanged, not copied
)

// Kinds that are not run kinds (runs use store.Kind*).
const (
	KindPeer     = "peer"
	KindSchedule = "schedule"
	KindSystem   = "system"
	KindAudit    = "audit"
	KindDrive    = "drive"
)

// Entry is one log line.
type Entry struct {
	ID       int64  `json:"id"`
	TS       int64  `json:"ts"` // unix milliseconds
	Level    int    `json:"level"`
	Kind     string `json:"kind"`               // run kind or one of the Kind constants
	Node     string `json:"node,omitempty"`     // where it happened: target, restore destination, peer
	SrcNode  string `json:"src_node,omitempty"` // the other side of a transfer
	JobID    int64  `json:"job_id,omitempty"`
	TargetID int64  `json:"target_id,omitempty"`
	RunID    int64  `json:"run_id,omitempty"`
	Path     string `json:"path,omitempty"`
	Message  string `json:"message"`
}

// Book writes entries in the background, in batches.
type Book struct {
	db    *sql.DB
	now   func() time.Time
	level atomic.Int32

	mu     sync.RWMutex // guards closed against sends on a closed ch
	closed bool
	ch     chan Entry
	flush  chan chan struct{}
	done   chan struct{}
}

const (
	queueSize = 10000
	batchSize = 1000
	batchWait = 300 * time.Millisecond
)

// Open opens (and creates) the log database at path.
func Open(path string) (*Book, error) {
	q := url.Values{}
	for _, p := range []string{"busy_timeout(10000)", "journal_mode(WAL)", "synchronous(NORMAL)"} {
		q.Add("_pragma", p)
	}
	q.Set("_txlock", "immediate")
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(4)
	if err := migrate(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("log database: %w", err)
	}
	os.Chmod(path, 0o600)
	b := &Book{db: db, now: time.Now, ch: make(chan Entry, queueSize), flush: make(chan chan struct{}), done: make(chan struct{})}
	b.level.Store(LevelEvent)
	go b.writer()
	return b, nil
}

const columns = `(
	id INTEGER PRIMARY KEY,
	ts INTEGER NOT NULL,
	level INTEGER NOT NULL,
	kind TEXT NOT NULL DEFAULT '',
	node TEXT NOT NULL DEFAULT '',
	src_node TEXT NOT NULL DEFAULT '',
	job_id INTEGER NOT NULL DEFAULT 0,
	target_id INTEGER NOT NULL DEFAULT 0,
	run_id INTEGER NOT NULL DEFAULT 0,
	path TEXT NOT NULL DEFAULT '',
	message TEXT NOT NULL
)`

var migrations = []string{
	// v1: the main log.
	`CREATE TABLE IF NOT EXISTS logs ` + columns + `;
CREATE INDEX IF NOT EXISTS logs_ts ON logs(ts);
CREATE INDEX IF NOT EXISTS logs_run ON logs(run_id);
CREATE INDEX IF NOT EXISTS logs_job ON logs(job_id, ts);`,
	// v2: per-file lines get their own table.
	`CREATE TABLE file_logs ` + columns + `;
CREATE INDEX file_logs_ts ON file_logs(ts);
CREATE INDEX file_logs_run ON file_logs(run_id, id);
CREATE INDEX file_logs_job ON file_logs(job_id, ts);
INSERT INTO file_logs(ts, level, kind, node, src_node, job_id, target_id, run_id, path, message)
	SELECT ts, level, kind, node, src_node, job_id, target_id, run_id, path, message FROM logs WHERE path != '' ORDER BY id;
DELETE FROM logs WHERE path != '';`,
}

func migrate(db *sql.DB) error {
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		return err
	}
	if v == 0 {
		// Created before versioning: v1 is in place if the table exists.
		var n int
		db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = 'logs'").Scan(&n)
		if n > 0 {
			v = 1
		}
	}
	for i := v; i < len(migrations); i++ {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// SetClock replaces the clock (tests).
func (b *Book) SetClock(now func() time.Time) { b.now = now }

// SetLevel sets the highest level that is kept (1-4).
func (b *Book) SetLevel(l int) { b.level.Store(int32(min(max(l, LevelError), LevelTrace))) }

// Level returns the current level.
func (b *Book) Level() int { return int(b.level.Load()) }

// Enabled reports whether entries of this level are kept. Callers check it
// before building expensive messages.
func (b *Book) Enabled(level int) bool { return b != nil && level <= int(b.level.Load()) }

// Add queues an entry. It blocks while the queue is full, so a run at level
// 3 is slowed down rather than losing lines.
func (b *Book) Add(e Entry) {
	if !b.Enabled(e.Level) {
		return
	}
	if e.TS == 0 {
		e.TS = b.now().UnixMilli()
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	if !b.closed {
		b.ch <- e
	}
}

// Flush waits until everything queued so far is written.
func (b *Book) Flush() {
	b.mu.RLock()
	if b.closed {
		b.mu.RUnlock()
		return
	}
	done := make(chan struct{})
	b.flush <- done
	b.mu.RUnlock()
	<-done
}

// Close writes what is queued and closes the database.
func (b *Book) Close() error {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return nil
	}
	b.closed = true
	close(b.ch)
	b.mu.Unlock()
	<-b.done
	return b.db.Close()
}

func (b *Book) writer() {
	defer close(b.done)
	t := time.NewTicker(batchWait)
	defer t.Stop()
	var batch []Entry
	write := func() {
		if len(batch) > 0 {
			b.insert(batch)
			batch = batch[:0]
		}
	}
	// drain takes everything that is queued right now.
	drain := func() bool {
		for {
			select {
			case e, ok := <-b.ch:
				if !ok {
					return false
				}
				batch = append(batch, e)
			default:
				return true
			}
		}
	}
	for {
		select {
		case e, ok := <-b.ch:
			if !ok {
				write()
				return
			}
			batch = append(batch, e)
			if len(batch) >= batchSize {
				write()
			}
		case done := <-b.flush:
			open := drain()
			write()
			close(done)
			if !open {
				return
			}
		case <-t.C:
			write()
		}
	}
}

func (b *Book) insert(batch []Entry) {
	tx, err := b.db.Begin()
	if err != nil {
		fmt.Fprintln(os.Stderr, "logbook: begin:", err)
		return
	}
	var stmts [2]*sql.Stmt // main log, file log
	for i, table := range []string{tableMain, tableFiles} {
		if stmts[i], err = tx.Prepare("INSERT INTO " + table + "(ts, level, kind, node, src_node, job_id, target_id, run_id, path, message) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)"); err != nil {
			tx.Rollback()
			fmt.Fprintln(os.Stderr, "logbook: prepare:", err)
			return
		}
		defer stmts[i].Close()
	}
	for _, e := range batch {
		stmt := stmts[0]
		if e.Path != "" {
			stmt = stmts[1]
		}
		if _, err := stmt.Exec(e.TS, e.Level, e.Kind, e.Node, e.SrcNode, e.JobID, e.TargetID, e.RunID, e.Path, e.Message); err != nil {
			tx.Rollback()
			fmt.Fprintln(os.Stderr, "logbook: insert:", err)
			return
		}
	}
	if err := tx.Commit(); err != nil {
		fmt.Fprintln(os.Stderr, "logbook: commit:", err)
	}
}

// Filter selects and orders entries. Zero values match everything.
type Filter struct {
	Level    int    // exact level
	Kind     string // exact kind
	Node     string // node or source node
	JobID    int64
	TargetID int64
	RunID    int64
	Search   string // substring of message or path
	Since    int64  // unix ms, inclusive
	Until    int64  // unix ms, exclusive
	Sort     string // ts (default), level, kind, node, job, path, run
	Asc      bool
	Limit    int
	Offset   int
}

var sortCols = map[string]string{
	"ts": "ts", "level": "level", "kind": "kind", "node": "node", "job": "job_id", "path": "path", "run": "run_id",
}

const (
	tableMain  = "logs"
	tableFiles = "file_logs"
)

// Query returns one page of the main log and the number of all matching
// entries. Entries added before the call are included.
func (b *Book) Query(ctx context.Context, f Filter) ([]Entry, int, error) {
	return b.query(ctx, tableMain, f)
}

// QueryFiles is Query for the file log.
func (b *Book) QueryFiles(ctx context.Context, f Filter) ([]Entry, int, error) {
	return b.query(ctx, tableFiles, f)
}

func (b *Book) query(ctx context.Context, table string, f Filter) ([]Entry, int, error) {
	b.Flush()
	var where []string
	var args []any
	add := func(cond string, a ...any) {
		where = append(where, cond)
		args = append(args, a...)
	}
	if f.Level > 0 {
		add("level = ?", f.Level)
	}
	if f.Kind != "" {
		add("kind = ?", f.Kind)
	}
	if f.Node != "" {
		add("(node = ? OR src_node = ?)", f.Node, f.Node)
	}
	if f.JobID != 0 {
		add("job_id = ?", f.JobID)
	}
	if f.TargetID != 0 {
		add("target_id = ?", f.TargetID)
	}
	if f.RunID != 0 {
		add("run_id = ?", f.RunID)
	}
	if s := strings.TrimSpace(f.Search); s != "" {
		like := "%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(s) + "%"
		add(`(message LIKE ? ESCAPE '\' OR path LIKE ? ESCAPE '\')`, like, like)
	}
	if f.Since > 0 {
		add("ts >= ?", f.Since)
	}
	if f.Until > 0 {
		add("ts < ?", f.Until)
	}
	w := ""
	if len(where) > 0 {
		w = " WHERE " + strings.Join(where, " AND ")
	}
	var total int
	if err := b.db.QueryRowContext(ctx, "SELECT count(*) FROM "+table+w, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	col, ok := sortCols[f.Sort]
	if !ok {
		col = "ts"
	}
	dir := "DESC"
	if f.Asc {
		dir = "ASC"
	}
	if f.Limit <= 0 || f.Limit > 1000 {
		f.Limit = 100
	}
	rows, err := b.db.QueryContext(ctx, "SELECT id, ts, level, kind, node, src_node, job_id, target_id, run_id, path, message FROM "+table+w+
		" ORDER BY "+col+" "+dir+", id "+dir+" LIMIT ? OFFSET ?", append(args, f.Limit, max(f.Offset, 0))...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []Entry{}
	for rows.Next() {
		var e Entry
		if err := rows.Scan(&e.ID, &e.TS, &e.Level, &e.Kind, &e.Node, &e.SrcNode, &e.JobID, &e.TargetID, &e.RunID, &e.Path, &e.Message); err != nil {
			return nil, 0, err
		}
		out = append(out, e)
	}
	return out, total, rows.Err()
}

// Prune deletes entries older than before and the oldest entries beyond
// maxRows (0 = no cap), in the main log and the file log separately. It
// returns how many were deleted.
func (b *Book) Prune(ctx context.Context, before time.Time, maxRows int) (int64, error) {
	b.Flush()
	var n int64
	for _, table := range []string{tableMain, tableFiles} {
		m, err := b.deleteChunked(ctx, table, "ts < ?", before.UnixMilli())
		n += m
		if err != nil {
			return n, err
		}
		if maxRows > 0 {
			var cut int64
			err := b.db.QueryRowContext(ctx, "SELECT id FROM "+table+" ORDER BY id DESC LIMIT 1 OFFSET ?", maxRows).Scan(&cut)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return n, err
			}
			m, err := b.deleteChunked(ctx, table, "id <= ?", cut)
			n += m
			if err != nil {
				return n, err
			}
		}
	}
	return n, nil
}

// pruneChunk is how many rows one delete statement removes. Small
// statements keep the write lock short, so a run logging at a high level is
// not held up while a million old lines go.
const pruneChunk = 5000

func (b *Book) deleteChunked(ctx context.Context, table, where string, arg any) (int64, error) {
	var n int64
	for {
		res, err := b.db.ExecContext(ctx, "DELETE FROM "+table+" WHERE id IN (SELECT id FROM "+table+" WHERE "+where+" LIMIT ?)", arg, pruneChunk)
		if err != nil {
			return n, err
		}
		m, _ := res.RowsAffected()
		n += m
		if m < pruneChunk {
			return n, nil
		}
	}
}
