package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"bfbackup/internal/model"
)

// Run kinds.
const (
	KindBackup       = "backup"
	KindDryRun       = "dryrun"
	KindVerifyQuick  = "verify_quick"
	KindVerifyDeep   = "verify_deep"
	KindRestore      = "restore"
	KindDelete       = "delete"
	KindRebuildIndex = "rebuild_index"
)

// Run statuses.
const (
	StatusQueued      = "queued"
	StatusRunning     = "running"
	StatusSuccess     = "success"
	StatusWarning     = "warning" // finished, but some files failed
	StatusFailed      = "failed"
	StatusCancelled   = "cancelled"
	StatusApproval    = "awaiting_approval"
	StatusApproved    = "approved"
	StatusInterrupted = "interrupted"
	StatusDeclined    = "declined" // stopped by the user at a question
)

// Run is one operation on one target.
type Run struct {
	ID         int64           `json:"id"`
	JobID      int64           `json:"job_id"`
	TargetID   int64           `json:"target_id"`
	Kind       string          `json:"kind"`
	Status     string          `json:"status"`
	Trigger    string          `json:"trigger"`
	StartedAt  int64           `json:"started_at"`
	FinishedAt int64           `json:"finished_at"`
	Stats      model.RunStats  `json:"stats"`
	Error      string          `json:"error"`
	Details    json.RawMessage `json:"details"`
	// Acknowledged: the user dismissed it from the attention list.
	Acknowledged bool `json:"acknowledged"`
}

// RunError is a per-file problem recorded during a run.
type RunError struct {
	Path    string `json:"path"`
	Message string `json:"message"`
	// Node is where it happened, when the run knows it ("" otherwise).
	Node string `json:"node,omitempty"`
}

// RunFilter narrows ListRuns.
type RunFilter struct {
	JobID    int64
	TargetID int64
	// Node: runs on targets of this node (e.g. a USB drive).
	Node     string
	Kinds    []string
	Statuses []string
	// Unacknowledged leaves out runs dismissed from the attention list.
	Unacknowledged bool
	Since          int64 // started at or after (unix)
	Limit          int
	Offset         int
}

// CreateRun inserts a run and sets its ID.
func (s *Store) CreateRun(ctx context.Context, r *Run) error {
	if r.StartedAt == 0 {
		r.StartedAt = s.now().Unix()
	}
	if len(r.Details) == 0 {
		r.Details = json.RawMessage("{}")
	}
	stats, _ := json.Marshal(r.Stats)
	res, err := s.db.ExecContext(ctx, `INSERT INTO runs(job_id, target_id, kind, status, trigger, started_at, stats, details)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?)`, r.JobID, r.TargetID, r.Kind, r.Status, r.Trigger, r.StartedAt, string(stats), string(r.Details))
	if err != nil {
		return err
	}
	r.ID, _ = res.LastInsertId()
	return nil
}

// UpdateRun stores status, start time (a queued run starts later than it
// was created), stats, error and details.
func (s *Store) UpdateRun(ctx context.Context, r *Run) error {
	stats, _ := json.Marshal(r.Stats)
	if len(r.Details) == 0 {
		r.Details = json.RawMessage("{}")
	}
	return s.execOne(ctx, `UPDATE runs SET status=?, started_at=COALESCE(NULLIF(?, 0), started_at), finished_at=?,
		files_transferred=?, bytes_transferred=?, stats=?, error=?, details=? WHERE id=?`,
		r.Status, r.StartedAt, r.FinishedAt, r.Stats.Transferred, r.Stats.TransferredBytes, string(stats), r.Error, string(r.Details), r.ID)
}

const runCols = "id, job_id, target_id, kind, status, trigger, started_at, finished_at, stats, error, details, acknowledged"

func scanRun(r interface{ Scan(...any) error }) (Run, error) {
	var run Run
	var stats, details string
	err := r.Scan(&run.ID, &run.JobID, &run.TargetID, &run.Kind, &run.Status, &run.Trigger, &run.StartedAt,
		&run.FinishedAt, &stats, &run.Error, &details, &run.Acknowledged)
	if err != nil {
		return run, err
	}
	json.Unmarshal([]byte(stats), &run.Stats)
	run.Details = json.RawMessage(details)
	return run, nil
}

// GetRun returns one run.
func (s *Store) GetRun(ctx context.Context, id int64) (Run, error) {
	r, err := scanRun(s.db.QueryRowContext(ctx, "SELECT "+runCols+" FROM runs WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return r, ErrNotFound
	}
	return r, err
}

func inClause(col string, vals []string, args *[]any) string {
	ph := make([]string, len(vals))
	for i, v := range vals {
		ph[i] = "?"
		*args = append(*args, v)
	}
	return col + " IN (" + strings.Join(ph, ",") + ")"
}

// ListRuns returns runs newest first and the total count for the filter.
func (s *Store) ListRuns(ctx context.Context, f RunFilter) ([]Run, int, error) {
	var where []string
	var args []any
	if f.JobID != 0 {
		where = append(where, "job_id = ?")
		args = append(args, f.JobID)
	}
	if f.TargetID != 0 {
		where = append(where, "target_id = ?")
		args = append(args, f.TargetID)
	}
	if f.Node != "" {
		where = append(where, "target_id IN (SELECT id FROM targets WHERE node_id = ?)")
		args = append(args, f.Node)
	}
	if len(f.Kinds) > 0 {
		where = append(where, inClause("kind", f.Kinds, &args))
	}
	if len(f.Statuses) > 0 {
		where = append(where, inClause("status", f.Statuses, &args))
	}
	if f.Unacknowledged {
		where = append(where, "acknowledged = 0")
	}
	if f.Since > 0 {
		where = append(where, "started_at >= ?")
		args = append(args, f.Since)
	}
	w := ""
	if len(where) > 0 {
		w = " WHERE " + strings.Join(where, " AND ")
	}
	var total int
	if err := s.db.QueryRowContext(ctx, "SELECT count(*) FROM runs"+w, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	if f.Limit <= 0 || f.Limit > 1000 {
		f.Limit = 50
	}
	rows, err := s.db.QueryContext(ctx, "SELECT "+runCols+" FROM runs"+w+" ORDER BY id DESC LIMIT ? OFFSET ?",
		append(args, f.Limit, f.Offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []Run{}
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, r)
	}
	return out, total, rows.Err()
}

// LastRun returns the newest run of one of the kinds for a target.
func (s *Store) LastRun(ctx context.Context, targetID int64, kinds ...string) (*Run, error) {
	runs, _, err := s.ListRuns(ctx, RunFilter{TargetID: targetID, Kinds: kinds, Limit: 1})
	if err != nil || len(runs) == 0 {
		return nil, err
	}
	return &runs[0], nil
}

// LastSuccess returns the newest successful (or warning) run of a kind.
func (s *Store) LastSuccess(ctx context.Context, targetID int64, kinds ...string) (*Run, error) {
	runs, _, err := s.ListRuns(ctx, RunFilter{TargetID: targetID, Kinds: kinds, Statuses: []string{StatusSuccess, StatusWarning}, Limit: 1})
	if err != nil || len(runs) == 0 {
		return nil, err
	}
	return &runs[0], nil
}

// MarkInterrupted flags runs that were active when the process stopped.
func (s *Store) MarkInterrupted(ctx context.Context) (int64, error) {
	res, err := s.db.ExecContext(ctx, "UPDATE runs SET status = ?, finished_at = ?, error = 'controller restarted during the run' WHERE status IN (?, ?)",
		StatusInterrupted, s.now().Unix(), StatusQueued, StatusRunning)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// AddRunErrors records per-file errors.
func (s *Store) AddRunErrors(ctx context.Context, runID int64, errs []RunError) error {
	if len(errs) == 0 {
		return nil
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		st, err := tx.PrepareContext(ctx, "INSERT INTO run_errors(run_id, path, message, node) VALUES(?, ?, ?, ?)")
		if err != nil {
			return err
		}
		defer st.Close()
		for _, e := range errs {
			if _, err := st.ExecContext(ctx, runID, e.Path, e.Message, e.Node); err != nil {
				return err
			}
		}
		return nil
	})
}

// ListRunErrors returns the recorded errors of a run.
func (s *Store) ListRunErrors(ctx context.Context, runID int64, limit int) ([]RunError, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT path, message, node FROM run_errors WHERE run_id = ? LIMIT ?", runID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []RunError{}
	for rows.Next() {
		var e RunError
		if err := rows.Scan(&e.Path, &e.Message, &e.Node); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// AcknowledgeRun dismisses a run from the attention list.
func (s *Store) AcknowledgeRun(ctx context.Context, id int64) error {
	return s.execOne(ctx, "UPDATE runs SET acknowledged = 1 WHERE id = ?", id)
}

// PruneRuns deletes finished runs older than before.
func (s *Store) PruneRuns(ctx context.Context, before time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx, "DELETE FROM runs WHERE started_at < ? AND status NOT IN (?, ?, ?)",
		before.Unix(), StatusQueued, StatusRunning, StatusApproval)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// DayStat aggregates runs per day.
type DayStat struct {
	Day    string `json:"day"` // YYYY-MM-DD, local time
	Runs   int64  `json:"runs"`
	Failed int64  `json:"failed"`
	Files  int64  `json:"files"`
	Bytes  int64  `json:"bytes"`
}

// DailyStats returns per-day totals since the given time, optionally for one target.
func (s *Store) DailyStats(ctx context.Context, since time.Time, targetID int64) ([]DayStat, error) {
	q := `SELECT date(started_at, 'unixepoch', 'localtime') AS d, count(*),
		sum(CASE WHEN status = 'failed' THEN 1 ELSE 0 END), sum(files_transferred), sum(bytes_transferred)
		FROM runs WHERE started_at >= ? AND kind IN ('backup', 'restore')`
	args := []any{since.Unix()}
	if targetID != 0 {
		q += " AND target_id = ?"
		args = append(args, targetID)
	}
	q += " GROUP BY d ORDER BY d"
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DayStat{}
	for rows.Next() {
		var d DayStat
		if err := rows.Scan(&d.Day, &d.Runs, &d.Failed, &d.Files, &d.Bytes); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
