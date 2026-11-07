package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"bfbackup/internal/model"
)

// IndexEntry is what the index knows about one stored file.
type IndexEntry struct {
	Dir      string          `json:"dir"`
	Name     string          `json:"name"`
	Type     model.EntryType `json:"type"`
	Size     int64           `json:"size"`
	MTime    int64           `json:"mtime"`
	Mode     uint32          `json:"mode"`
	Hash     string          `json:"hash,omitempty"`
	Link     string          `json:"link,omitempty"`
	SyncedAt int64           `json:"synced_at"`
	SeenRun  int64           `json:"seen_run"`
}

// Rel returns the path relative to the job root.
func (e IndexEntry) Rel() string { return model.JoinRel(e.Dir, e.Name) }

// Entry converts to a model.Entry.
func (e IndexEntry) Entry() model.Entry {
	return model.Entry{Name: e.Name, Type: e.Type, Size: e.Size, MTime: e.MTime, Mode: e.Mode, Hash: e.Hash, Link: e.Link}
}

const fileCols = "dir, name, type, size, mtime, mode, hash, link, synced_at, seen_run"

func scanFile(r interface{ Scan(...any) error }) (IndexEntry, error) {
	var e IndexEntry
	err := r.Scan(&e.Dir, &e.Name, &e.Type, &e.Size, &e.MTime, &e.Mode, &e.Hash, &e.Link, &e.SyncedAt, &e.SeenRun)
	return e, err
}

// DirIndex returns the entries stored for one directory, keyed by name.
func (s *Store) DirIndex(ctx context.Context, targetID int64, dir string) (map[string]IndexEntry, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+fileCols+" FROM files WHERE target_id = ? AND dir = ?", targetID, dir)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]IndexEntry{}
	for rows.Next() {
		e, err := scanFile(rows)
		if err != nil {
			return nil, err
		}
		out[e.Name] = e
	}
	return out, rows.Err()
}

// subtreeUpper returns the exclusive upper bound for paths below p: every
// "p/..." sorts between "p/" and "p0" because '0' follows '/'.
func subtreeUpper(p string) string { return p + "0" }

// DirStat is a subdirectory with the totals of everything below it.
type DirStat struct {
	Name  string `json:"name"`
	Files int64  `json:"files"`
	Bytes int64  `json:"bytes"`
	// NotInSource counts the files below it not seen by the last scan.
	NotInSource int64 `json:"not_in_source"`
}

// BrowseDir returns the direct subdirectories (with subtree totals) and the
// files of dir. The totals come from one grouped scan of the subtree.
// lastScan is the target's last scan run: files not seen by it count as
// not in source (0: none do).
func (s *Store) BrowseDir(ctx context.Context, targetID int64, dir string, lastScan int64) ([]DirStat, []IndexEntry, error) {
	// Every dir row below dir, reduced to its first path segment.
	var rows *sql.Rows
	var err error
	const group = `SELECT CASE WHEN instr(rest, '/') = 0 THEN rest ELSE substr(rest, 1, instr(rest, '/') - 1) END AS child,
		count(*), coalesce(sum(size), 0), coalesce(sum(seen_run < ?), 0) FROM (%s) WHERE rest != '' GROUP BY child`
	if dir == "" {
		rows, err = s.db.QueryContext(ctx, fmt.Sprintf(group,
			"SELECT dir AS rest, size, seen_run FROM files WHERE target_id = ? AND dir != ''"), lastScan, targetID)
	} else {
		rows, err = s.db.QueryContext(ctx, fmt.Sprintf(group,
			"SELECT substr(dir, length(?) + 1) AS rest, size, seen_run FROM files WHERE target_id = ? AND dir >= ? AND dir < ?"),
			lastScan, dir+"/", targetID, dir+"/", subtreeUpper(dir))
	}
	if err != nil {
		return nil, nil, err
	}
	stats := map[string]DirStat{}
	for rows.Next() {
		var d DirStat
		if err := rows.Scan(&d.Name, &d.Files, &d.Bytes, &d.NotInSource); err != nil {
			rows.Close()
			return nil, nil, err
		}
		stats[d.Name] = d
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	// Directories without any file below them only exist in the dirs table.
	rows, err = s.db.QueryContext(ctx, "SELECT path FROM dirs WHERE target_id = ? AND parent = ?", targetID, dir)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			rows.Close()
			return nil, nil, err
		}
		_, name := model.SplitRel(p)
		if _, ok := stats[name]; !ok {
			stats[name] = DirStat{Name: name}
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	dirs := make([]DirStat, 0, len(stats))
	for _, d := range stats {
		dirs = append(dirs, d)
	}
	sort.Slice(dirs, func(i, j int) bool { return dirs[i].Name < dirs[j].Name })

	files := []IndexEntry{}
	rows, err = s.db.QueryContext(ctx, "SELECT "+fileCols+" FROM files WHERE target_id = ? AND dir = ? ORDER BY name", targetID, dir)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		e, err := scanFile(rows)
		if err != nil {
			return nil, nil, err
		}
		files = append(files, e)
	}
	return dirs, files, rows.Err()
}

// WalkPrefix calls fn for the file at rel and for every file below rel
// ("" = everything). Iteration is ordered by dir and name.
func (s *Store) WalkPrefix(ctx context.Context, targetID int64, rel string, fn func(IndexEntry) error) error {
	var rows *sql.Rows
	var err error
	if rel == "" {
		rows, err = s.db.QueryContext(ctx, "SELECT "+fileCols+" FROM files WHERE target_id = ? ORDER BY dir, name", targetID)
	} else {
		dir, name := model.SplitRel(rel)
		rows, err = s.db.QueryContext(ctx, "SELECT "+fileCols+` FROM files WHERE target_id = ? AND
			((dir = ? AND name = ?) OR dir = ? OR (dir >= ? AND dir < ?)) ORDER BY dir, name`,
			targetID, dir, name, rel, rel+"/", subtreeUpper(rel))
	}
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		e, err := scanFile(rows)
		if err != nil {
			return err
		}
		if err := fn(e); err != nil {
			return err
		}
	}
	return rows.Err()
}

// CountPrefix returns files and bytes at or below rel.
func (s *Store) CountPrefix(ctx context.Context, targetID int64, rel string) (files, bytes int64, err error) {
	if rel == "" {
		err = s.db.QueryRowContext(ctx, "SELECT count(*), coalesce(sum(size),0) FROM files WHERE target_id = ?", targetID).Scan(&files, &bytes)
		return
	}
	dir, name := model.SplitRel(rel)
	err = s.db.QueryRowContext(ctx, `SELECT count(*), coalesce(sum(size),0) FROM files WHERE target_id = ? AND
		((dir = ? AND name = ?) OR dir = ? OR (dir >= ? AND dir < ?))`,
		targetID, dir, name, rel, rel+"/", subtreeUpper(rel)).Scan(&files, &bytes)
	return
}

// DeletePrefix removes rel and everything below it from the index.
func (s *Store) DeletePrefix(ctx context.Context, targetID int64, rel string) (int64, error) {
	return s.DeletePrefixes(ctx, targetID, []string{rel})
}

// DeletePrefixes is DeletePrefix for several paths in one transaction and
// returns how many file rows were removed in all.
func (s *Store) DeletePrefixes(ctx context.Context, targetID int64, rels []string) (int64, error) {
	var n int64
	err := s.withTx(ctx, func(tx *sql.Tx) error {
		for _, rel := range rels {
			m, err := deletePrefixTx(ctx, tx, targetID, rel)
			if err != nil {
				return err
			}
			n += m
		}
		return nil
	})
	return n, err
}

// deletePrefixTx removes rel and everything below it. Each statement is a
// lookup on the primary key: ORed together, SQLite would only use target_id
// and read every row of the target, for each removed file.
func deletePrefixTx(ctx context.Context, tx *sql.Tx, targetID int64, rel string) (int64, error) {
	if rel == "" {
		res, err := tx.ExecContext(ctx, "DELETE FROM files WHERE target_id = ?", targetID)
		if err != nil {
			return 0, err
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM dirs WHERE target_id = ?", targetID); err != nil {
			return 0, err
		}
		n, _ := res.RowsAffected()
		return n, nil
	}
	dir, name := model.SplitRel(rel)
	var n int64
	for _, q := range []struct {
		files bool // counts toward the result
		sql   string
		args  []any
	}{
		{true, "DELETE FROM files WHERE target_id = ? AND dir = ? AND name = ?", []any{targetID, dir, name}},
		{true, "DELETE FROM files WHERE target_id = ? AND dir = ?", []any{targetID, rel}},
		{true, "DELETE FROM files WHERE target_id = ? AND dir >= ? AND dir < ?", []any{targetID, rel + "/", subtreeUpper(rel)}},
		{false, "DELETE FROM dirs WHERE target_id = ? AND path = ?", []any{targetID, rel}},
		{false, "DELETE FROM dirs WHERE target_id = ? AND path >= ? AND path < ?", []any{targetID, rel + "/", subtreeUpper(rel)}},
	} {
		res, err := tx.ExecContext(ctx, q.sql, q.args...)
		if err != nil {
			return 0, err
		}
		if q.files {
			m, _ := res.RowsAffected()
			n += m
		}
	}
	return n, nil
}

// PruneEmptyDirs removes directory rows that no longer contain anything.
func (s *Store) PruneEmptyDirs(ctx context.Context, targetID int64) error {
	for {
		res, err := s.db.ExecContext(ctx, `DELETE FROM dirs WHERE target_id = ?1 AND
			NOT EXISTS (SELECT 1 FROM files f WHERE f.target_id = ?1 AND f.dir = dirs.path) AND
			NOT EXISTS (SELECT 1 FROM dirs c WHERE c.target_id = ?1 AND c.parent = dirs.path)`, targetID)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return nil
		}
	}
}

// NotInSource lists files that the last complete scan did not see.
func (s *Store) NotInSource(ctx context.Context, targetID, lastScan int64, limit, offset int) ([]IndexEntry, int64, error) {
	var total int64
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM files WHERE target_id = ? AND seen_run < ?", targetID, lastScan).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+fileCols+" FROM files WHERE target_id = ? AND seen_run < ? ORDER BY dir, name LIMIT ? OFFSET ?",
		targetID, lastScan, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []IndexEntry{}
	for rows.Next() {
		e, err := scanFile(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, e)
	}
	return out, total, rows.Err()
}

// WalkNotSeen calls fn for every file with seen_run < run, ordered by dir
// and name, in one pass. Use it rather than paging NotInSource, whose
// OFFSET re-reads every earlier row: millions of files would take hours.
// fn returning ErrStopWalk ends the walk without an error.
func (s *Store) WalkNotSeen(ctx context.Context, targetID, run int64, fn func(IndexEntry) error) error {
	rows, err := s.db.QueryContext(ctx, "SELECT "+fileCols+" FROM files WHERE target_id = ? AND seen_run < ? ORDER BY dir, name", targetID, run)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		e, err := scanFile(rows)
		if err != nil {
			return err
		}
		if err := fn(e); err != nil {
			if errors.Is(err, ErrStopWalk) {
				return nil
			}
			return err
		}
	}
	return rows.Err()
}

// ErrStopWalk ends a walk early.
var ErrStopWalk = errors.New("stop walk")

// CountNotSeen counts files with seen_run < run.
func (s *Store) CountNotSeen(ctx context.Context, targetID, run int64) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM files WHERE target_id = ? AND seen_run < ?", targetID, run).Scan(&n)
	return n, err
}

// CountGone counts files that were seen by the previous scan (prevScan) but
// not by this one (run): what disappeared from the source in between.
func (s *Store) CountGone(ctx context.Context, targetID, prevScan, run int64) (int64, error) {
	if prevScan <= 0 {
		return 0, nil
	}
	var n int64
	err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM files WHERE target_id = ? AND seen_run >= ? AND seen_run < ?", targetID, prevScan, run).Scan(&n)
	return n, err
}

// SearchIndex finds files whose path contains q.
func (s *Store) SearchIndex(ctx context.Context, targetID int64, q string, limit int) ([]IndexEntry, error) {
	like := "%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(q) + "%"
	rows, err := s.db.QueryContext(ctx, "SELECT "+fileCols+` FROM files WHERE target_id = ? AND
		(name LIKE ? ESCAPE '\' OR dir LIKE ? ESCAPE '\') ORDER BY dir, name LIMIT ?`, targetID, like, like, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []IndexEntry{}
	for rows.Next() {
		e, err := scanFile(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// IndexWriter batches index changes into transactions. Safe for
// concurrent use. Call Close to flush the rest.
type IndexWriter struct {
	s        *Store
	targetID int64
	mu       sync.Mutex
	ops      []indexOp
	dirs     map[string]bool
	err      error
}

type indexOp struct {
	kind int // 0 put, 1 seen, 2 delete
	e    IndexEntry
}

const indexBatch = 2000

// NewIndexWriter starts a batch writer for one target.
func (s *Store) NewIndexWriter(targetID int64) *IndexWriter {
	return &IndexWriter{s: s, targetID: targetID, dirs: map[string]bool{}}
}

// Put inserts or replaces a file.
func (w *IndexWriter) Put(ctx context.Context, e IndexEntry) error {
	return w.add(ctx, indexOp{kind: 0, e: e})
}

// Seen marks an unchanged file as seen by run.
func (w *IndexWriter) Seen(ctx context.Context, dir, name string, run int64) error {
	return w.add(ctx, indexOp{kind: 1, e: IndexEntry{Dir: dir, Name: name, SeenRun: run}})
}

// Delete removes one file.
func (w *IndexWriter) Delete(ctx context.Context, dir, name string) error {
	return w.add(ctx, indexOp{kind: 2, e: IndexEntry{Dir: dir, Name: name}})
}

func (w *IndexWriter) add(ctx context.Context, op indexOp) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return w.err
	}
	w.ops = append(w.ops, op)
	if len(w.ops) >= indexBatch {
		w.err = w.flushLocked(ctx)
	}
	return w.err
}

// Flush writes pending changes.
func (w *IndexWriter) Flush(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.err != nil {
		return w.err
	}
	w.err = w.flushLocked(ctx)
	return w.err
}

// Close flushes and returns the first error.
func (w *IndexWriter) Close(ctx context.Context) error { return w.Flush(ctx) }

func (w *IndexWriter) flushLocked(ctx context.Context) error {
	if len(w.ops) == 0 {
		return nil
	}
	ops := w.ops
	w.ops = nil
	return w.s.withTx(ctx, func(tx *sql.Tx) error {
		put, err := tx.PrepareContext(ctx, `INSERT INTO files(target_id, `+fileCols+`) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(target_id, dir, name) DO UPDATE SET type=excluded.type, size=excluded.size, mtime=excluded.mtime,
			mode=excluded.mode, hash=excluded.hash, link=excluded.link, synced_at=excluded.synced_at, seen_run=excluded.seen_run`)
		if err != nil {
			return err
		}
		defer put.Close()
		seen, err := tx.PrepareContext(ctx, "UPDATE files SET seen_run = ? WHERE target_id = ? AND dir = ? AND name = ?")
		if err != nil {
			return err
		}
		defer seen.Close()
		del, err := tx.PrepareContext(ctx, "DELETE FROM files WHERE target_id = ? AND dir = ? AND name = ?")
		if err != nil {
			return err
		}
		defer del.Close()
		mkdir, err := tx.PrepareContext(ctx, "INSERT OR IGNORE INTO dirs(target_id, path, parent) VALUES(?, ?, ?)")
		if err != nil {
			return err
		}
		defer mkdir.Close()

		for _, op := range ops {
			e := op.e
			switch op.kind {
			case 0:
				if _, err := put.ExecContext(ctx, w.targetID, e.Dir, e.Name, e.Type, e.Size, e.MTime, e.Mode, e.Hash, e.Link, e.SyncedAt, e.SeenRun); err != nil {
					return err
				}
				for d := e.Dir; d != "" && !w.dirs[d]; {
					parent, _ := model.SplitRel(d)
					if _, err := mkdir.ExecContext(ctx, w.targetID, d, parent); err != nil {
						return err
					}
					w.dirs[d] = true
					d = parent
				}
			case 1:
				if _, err := seen.ExecContext(ctx, e.SeenRun, w.targetID, e.Dir, e.Name); err != nil {
					return err
				}
			case 2:
				if _, err := del.ExecContext(ctx, w.targetID, e.Dir, e.Name); err != nil {
					return err
				}
			}
		}
		return nil
	})
}
