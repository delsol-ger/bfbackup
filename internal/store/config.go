package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/robfig/cron/v3"

	"bfbackup/internal/match"
	"bfbackup/internal/model"
)

// Node is a peer (the controller itself is model.LocalNodeID and has no row).
type Node struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	CreatedAt int64  `json:"created_at"`
	LastSeen  int64  `json:"last_seen"`
	Hostname  string `json:"hostname"`
	Version   string `json:"version"`
	// OfflineNotify sends a notification once the peer has been offline
	// for OfflineHours.
	OfflineNotify bool `json:"offline_notify"`
	OfflineHours  int  `json:"offline_hours"`
}

// MaxOfflineHours is the longest offline alert delay (30 days).
const MaxOfflineHours = 720

// Job is what to back up.
type Job struct {
	ID         int64    `json:"id"`
	Name       string   `json:"name"`
	SourceNode string   `json:"source_node"`
	SourcePath string   `json:"source_path"`
	Includes   []string `json:"includes"`
	Excludes   []string `json:"excludes"`
	// IgnoreGlobalExcludes turns off the global exclusion list for this job.
	IgnoreGlobalExcludes bool `json:"ignore_global_excludes"`
	// CleanExcluded removes files from the targets that match the
	// exclusions (they go to the target's trash).
	CleanExcluded bool `json:"clean_excluded"`
	// Mode: ModeCopy leaves extra files on the targets, ModeSync removes
	// files that are gone from the source.
	Mode string `json:"mode"`
	// OnConflict decides what happens with a changed file that exists on the
	// target: Conflict* constants.
	OnConflict string `json:"on_conflict"`
	// OldCopies: OldTrash moves removed files to the target's trash,
	// OldDelete deletes them. Replaced files are overwritten in place.
	OldCopies   string   `json:"old_copies"`
	CompareMode string   `json:"compare_mode"`
	PreHook     string   `json:"pre_hook"`
	PostHook    string   `json:"post_hook"`
	ProtectPct  float64  `json:"protect_pct"`
	ProtectMin  int64    `json:"protect_min"`
	Workers     int      `json:"workers"`
	Enabled     bool     `json:"enabled"`
	CreatedAt   int64    `json:"created_at"`
	UpdatedAt   int64    `json:"updated_at"`
	Targets     []Target `json:"targets"`
}

// Target is where a job's files go.
type Target struct {
	ID             int64  `json:"id"`
	JobID          int64  `json:"job_id"`
	NodeID         string `json:"node_id"`
	Path           string `json:"path"`
	Schedule       string `json:"schedule"`
	VerifySchedule string `json:"verify_schedule"`
	VerifyMode     string `json:"verify_mode"`
	Enabled        bool   `json:"enabled"`
	// RunOnConnect starts a backup when the target's or the source's peer
	// connects after being offline.
	RunOnConnect bool  `json:"run_on_connect"`
	StoredFiles  int64 `json:"stored_files"`
	StoredBytes  int64 `json:"stored_bytes"`
	LastScanRun  int64 `json:"last_scan_run"`
}

// CronParser parses schedules: standard 5 fields or descriptors (@daily).
var CronParser = cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor)

// Job modes.
const (
	ModeCopy = "copy" // copy new and changed files, keep extra files on the target
	ModeSync = "sync" // also remove files from the target that are gone from the source
)

// What a backup does with a changed file that exists on the target.
const (
	ConflictNewer  = "newer"  // replace it only if the source copy is newer
	ConflictSource = "source" // always replace it with the source copy
	ConflictKeep   = "keep"   // never replace it, only copy new files
	ConflictAsk    = "ask"    // stop and ask before replacing anything
)

// What happens to files a backup replaces or removes on a target.
const (
	OldTrash  = "trash"
	OldDelete = "delete"
)

// Validate checks and normalizes a job.
func (j *Job) Validate() error {
	j.Name = strings.TrimSpace(j.Name)
	if j.Name == "" || len(j.Name) > 100 {
		return errors.New("name is required (max 100 characters)")
	}
	if j.SourceNode == "" {
		return errors.New("source node is required")
	}
	p, err := model.CleanAbs(j.SourcePath)
	if err != nil {
		return fmt.Errorf("source path: must be absolute: %w", err)
	}
	if p == "/" {
		return errors.New("source path must not be /")
	}
	j.SourcePath = p
	var inc []string
	for _, i := range j.Includes {
		c, err := model.CleanRel(strings.TrimSpace(i))
		if err != nil {
			return fmt.Errorf("include %q: %w", i, err)
		}
		if c != "" {
			inc = append(inc, c)
		}
	}
	j.Includes = inc
	if j.Includes == nil {
		j.Includes = []string{}
	}
	var exc []string
	for _, e := range j.Excludes {
		if e = strings.TrimSpace(e); e != "" {
			exc = append(exc, e)
		}
	}
	j.Excludes = exc
	if j.Excludes == nil {
		j.Excludes = []string{}
	}
	if _, err := match.New(j.Excludes); err != nil {
		return err
	}
	if j.CompareMode == "" {
		j.CompareMode = model.CompareMTime
	}
	if !model.ValidCompareMode(j.CompareMode) {
		return fmt.Errorf("unknown compare mode %q", j.CompareMode)
	}
	if j.ProtectPct < 0 || j.ProtectPct > 100 {
		return errors.New("protection percentage must be between 0 and 100 (0 disables)")
	}
	if j.ProtectMin < 0 {
		return errors.New("protection minimum must not be negative")
	}
	if j.Workers <= 0 {
		j.Workers = 4
	}
	if j.Mode == "" {
		j.Mode = ModeCopy
	}
	if j.Mode != ModeCopy && j.Mode != ModeSync {
		return fmt.Errorf("mode must be %q or %q", ModeCopy, ModeSync)
	}
	if j.OnConflict == "" {
		j.OnConflict = ConflictNewer
	}
	switch j.OnConflict {
	case ConflictNewer, ConflictSource, ConflictKeep, ConflictAsk:
	default:
		return fmt.Errorf("unknown setting for existing files %q", j.OnConflict)
	}
	if j.OldCopies == "" {
		j.OldCopies = OldTrash
	}
	if j.OldCopies != OldTrash && j.OldCopies != OldDelete {
		return fmt.Errorf("old copies must be %q or %q", OldTrash, OldDelete)
	}
	if j.Workers > 32 {
		return errors.New("workers must be between 1 and 32")
	}
	for _, h := range []string{j.PreHook, j.PostHook} {
		if strings.ContainsAny(h, "/\\ \x00") {
			return fmt.Errorf("hook %q: only a script name from the hooks folder is allowed", h)
		}
	}
	return nil
}

// Validate checks and normalizes a target.
func (t *Target) Validate() error {
	if t.NodeID == "" {
		return errors.New("target node is required")
	}
	p, err := model.CleanAbs(t.Path)
	if err != nil {
		return fmt.Errorf("target path: must be absolute: %w", err)
	}
	if p == "/" {
		return errors.New("target path must not be /")
	}
	t.Path = p
	t.Schedule = strings.TrimSpace(t.Schedule)
	t.VerifySchedule = strings.TrimSpace(t.VerifySchedule)
	for _, s := range []string{t.Schedule, t.VerifySchedule} {
		if s == "" {
			continue
		}
		if _, err := CronParser.Parse(s); err != nil {
			return fmt.Errorf("schedule %q: %w", s, err)
		}
	}
	if t.VerifyMode == "" {
		t.VerifyMode = "quick"
	}
	if t.VerifyMode != "quick" && t.VerifyMode != "deep" {
		return fmt.Errorf("verify mode must be quick or deep")
	}
	return nil
}

// ---- nodes ----

// CreateNode adds a peer.
func (s *Store) CreateNode(ctx context.Context, id, name string, secret []byte) (Node, error) {
	n := Node{ID: id, Name: strings.TrimSpace(name), CreatedAt: s.now().Unix(), OfflineNotify: true, OfflineHours: 1}
	if n.Name == "" {
		return n, errors.New("name is required")
	}
	_, err := s.db.ExecContext(ctx, "INSERT INTO nodes(id, name, secret, created_at) VALUES(?, ?, ?, ?)", n.ID, n.Name, secret, n.CreatedAt)
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return n, fmt.Errorf("a node named %q already exists", n.Name)
	}
	return n, err
}

const nodeCols = "id, name, created_at, last_seen, hostname, version, offline_notify, offline_hours"

func scanNode(r interface{ Scan(...any) error }) (Node, error) {
	var n Node
	err := r.Scan(&n.ID, &n.Name, &n.CreatedAt, &n.LastSeen, &n.Hostname, &n.Version, &n.OfflineNotify, &n.OfflineHours)
	return n, err
}

// ListNodes returns all peers by name.
func (s *Store) ListNodes(ctx context.Context) ([]Node, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+nodeCols+" FROM nodes ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Node
	for rows.Next() {
		n, err := scanNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// GetNode returns one peer.
func (s *Store) GetNode(ctx context.Context, id string) (Node, error) {
	n, err := scanNode(s.db.QueryRowContext(ctx, "SELECT "+nodeCols+" FROM nodes WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return n, ErrNotFound
	}
	return n, err
}

// NodeSecret returns a peer's shared secret.
func (s *Store) NodeSecret(ctx context.Context, id string) ([]byte, error) {
	var b []byte
	err := s.db.QueryRowContext(ctx, "SELECT secret FROM nodes WHERE id = ?", id).Scan(&b)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return b, err
}

// RenameNode changes a peer's display name.
func (s *Store) RenameNode(ctx context.Context, id, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("name is required")
	}
	return s.execOne(ctx, "UPDATE nodes SET name = ? WHERE id = ?", name, id)
}

// SetNodeOffline sets a peer's offline alert.
func (s *Store) SetNodeOffline(ctx context.Context, id string, notify bool, hours int) error {
	if hours < 1 || hours > MaxOfflineHours {
		return fmt.Errorf("offline hours must be between 1 and %d", MaxOfflineHours)
	}
	return s.execOne(ctx, "UPDATE nodes SET offline_notify = ?, offline_hours = ? WHERE id = ?", notify, hours, id)
}

// SetNodeSecret replaces a peer's secret.
func (s *Store) SetNodeSecret(ctx context.Context, id string, secret []byte) error {
	return s.execOne(ctx, "UPDATE nodes SET secret = ? WHERE id = ?", secret, id)
}

// TouchNode records that a peer connected.
func (s *Store) TouchNode(ctx context.Context, id, hostname, version string) error {
	_, err := s.db.ExecContext(ctx, "UPDATE nodes SET last_seen = ?, hostname = ?, version = ? WHERE id = ?",
		s.now().Unix(), hostname, version, id)
	return err
}

// SeenNode updates last_seen only.
func (s *Store) SeenNode(ctx context.Context, id string) error {
	_, err := s.db.ExecContext(ctx, "UPDATE nodes SET last_seen = ? WHERE id = ?", s.now().Unix(), id)
	return err
}

// NodeDisks are the disks a peer reported last; At is 0 if it never did.
type NodeDisks struct {
	Disks []model.DiskInfo
	At    int64
}

// SetNodeDisks remembers the disks a peer reported.
func (s *Store) SetNodeDisks(ctx context.Context, id string, disks []model.DiskInfo) error {
	b, err := json.Marshal(disks)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, "UPDATE nodes SET disks = ?, disks_at = ? WHERE id = ?", string(b), s.now().Unix(), id)
	return err
}

// ListNodeDisks returns the last reported disks of every peer by ID.
func (s *Store) ListNodeDisks(ctx context.Context) (map[string]NodeDisks, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT id, disks, disks_at FROM nodes")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]NodeDisks{}
	for rows.Next() {
		var id, js string
		var d NodeDisks
		if err := rows.Scan(&id, &js, &d.At); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(js), &d.Disks); err != nil {
			return nil, fmt.Errorf("disks of node %s: %w", id, err)
		}
		out[id] = d
	}
	return out, rows.Err()
}

// DeleteNode removes a peer that no job uses.
func (s *Store) DeleteNode(ctx context.Context, id string) error {
	var n int
	err := s.db.QueryRowContext(ctx,
		"SELECT (SELECT count(*) FROM jobs WHERE source_node = ?) + (SELECT count(*) FROM targets WHERE node_id = ?)", id, id).Scan(&n)
	if err != nil {
		return err
	}
	if n > 0 {
		return errors.New("node is still used by jobs or targets")
	}
	return s.execOne(ctx, "DELETE FROM nodes WHERE id = ?", id)
}

func (s *Store) execOne(ctx context.Context, q string, args ...any) error {
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---- jobs ----

// CreateJob inserts a validated job.
func (s *Store) CreateJob(ctx context.Context, j *Job) error {
	if err := j.Validate(); err != nil {
		return err
	}
	inc, _ := json.Marshal(j.Includes)
	exc, _ := json.Marshal(j.Excludes)
	now := s.now().Unix()
	res, err := s.db.ExecContext(ctx, `INSERT INTO jobs(name, source_node, source_path, includes, excludes, compare_mode,
		pre_hook, post_hook, protect_pct, protect_min, workers, enabled, created_at, updated_at, ignore_global_excludes, clean_excluded,
		mode, on_conflict, old_copies)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		j.Name, j.SourceNode, j.SourcePath, string(inc), string(exc), j.CompareMode, j.PreHook, j.PostHook,
		j.ProtectPct, j.ProtectMin, j.Workers, j.Enabled, now, now, j.IgnoreGlobalExcludes, j.CleanExcluded,
		j.Mode, j.OnConflict, j.OldCopies)
	if err != nil {
		return uniqueErr(err, "a job with this name already exists")
	}
	j.ID, _ = res.LastInsertId()
	j.CreatedAt, j.UpdatedAt = now, now
	return nil
}

// UpdateJob replaces a job's settings (targets are separate).
func (s *Store) UpdateJob(ctx context.Context, j *Job) error {
	if err := j.Validate(); err != nil {
		return err
	}
	inc, _ := json.Marshal(j.Includes)
	exc, _ := json.Marshal(j.Excludes)
	j.UpdatedAt = s.now().Unix()
	err := s.execOne(ctx, `UPDATE jobs SET name=?, source_node=?, source_path=?, includes=?, excludes=?, compare_mode=?,
		pre_hook=?, post_hook=?, protect_pct=?, protect_min=?, workers=?, enabled=?, updated_at=?, ignore_global_excludes=?, clean_excluded=?,
		mode=?, on_conflict=?, old_copies=? WHERE id=?`,
		j.Name, j.SourceNode, j.SourcePath, string(inc), string(exc), j.CompareMode, j.PreHook, j.PostHook,
		j.ProtectPct, j.ProtectMin, j.Workers, j.Enabled, j.UpdatedAt, j.IgnoreGlobalExcludes, j.CleanExcluded,
		j.Mode, j.OnConflict, j.OldCopies, j.ID)
	return uniqueErr(err, "a job with this name already exists")
}

func uniqueErr(err error, msg string) error {
	if err != nil && strings.Contains(err.Error(), "UNIQUE") {
		return errors.New(msg)
	}
	return err
}

// DeleteJob removes a job, its targets and their index.
func (s *Store) DeleteJob(ctx context.Context, id int64) error {
	return s.execOne(ctx, "DELETE FROM jobs WHERE id = ?", id)
}

const jobCols = `id, name, source_node, source_path, includes, excludes, compare_mode, pre_hook, post_hook,
	protect_pct, protect_min, workers, enabled, created_at, updated_at, ignore_global_excludes, clean_excluded,
	mode, on_conflict, old_copies`

func scanJob(r interface{ Scan(...any) error }) (Job, error) {
	var j Job
	var inc, exc string
	err := r.Scan(&j.ID, &j.Name, &j.SourceNode, &j.SourcePath, &inc, &exc, &j.CompareMode, &j.PreHook, &j.PostHook,
		&j.ProtectPct, &j.ProtectMin, &j.Workers, &j.Enabled, &j.CreatedAt, &j.UpdatedAt, &j.IgnoreGlobalExcludes, &j.CleanExcluded,
		&j.Mode, &j.OnConflict, &j.OldCopies)
	if err != nil {
		return j, err
	}
	json.Unmarshal([]byte(inc), &j.Includes)
	json.Unmarshal([]byte(exc), &j.Excludes)
	return j, nil
}

// GetJob returns a job with its targets.
func (s *Store) GetJob(ctx context.Context, id int64) (Job, error) {
	j, err := scanJob(s.db.QueryRowContext(ctx, "SELECT "+jobCols+" FROM jobs WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return j, ErrNotFound
	}
	if err != nil {
		return j, err
	}
	j.Targets, err = s.listTargets(ctx, "WHERE job_id = ?", id)
	return j, err
}

// ListJobs returns all jobs with targets.
func (s *Store) ListJobs(ctx context.Context) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+jobCols+" FROM jobs ORDER BY name")
	if err != nil {
		return nil, err
	}
	var out []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, j)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	targets, err := s.listTargets(ctx, "")
	if err != nil {
		return nil, err
	}
	for i := range out {
		for _, t := range targets {
			if t.JobID == out[i].ID {
				out[i].Targets = append(out[i].Targets, t)
			}
		}
	}
	return out, nil
}

// ---- targets ----

const targetCols = `id, job_id, node_id, path, schedule, verify_schedule, verify_mode, enabled,
	stored_files, stored_bytes, last_scan_run, run_on_connect`

func scanTarget(r interface{ Scan(...any) error }) (Target, error) {
	var t Target
	err := r.Scan(&t.ID, &t.JobID, &t.NodeID, &t.Path, &t.Schedule, &t.VerifySchedule, &t.VerifyMode, &t.Enabled,
		&t.StoredFiles, &t.StoredBytes, &t.LastScanRun, &t.RunOnConnect)
	return t, err
}

func (s *Store) listTargets(ctx context.Context, where string, args ...any) ([]Target, error) {
	rows, err := s.db.QueryContext(ctx, "SELECT "+targetCols+" FROM targets "+where+" ORDER BY id", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Target{}
	for rows.Next() {
		t, err := scanTarget(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ListTargets returns all targets.
func (s *Store) ListTargets(ctx context.Context) ([]Target, error) { return s.listTargets(ctx, "") }

// GetTarget returns one target.
func (s *Store) GetTarget(ctx context.Context, id int64) (Target, error) {
	t, err := scanTarget(s.db.QueryRowContext(ctx, "SELECT "+targetCols+" FROM targets WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return t, ErrNotFound
	}
	return t, err
}

// CreateTarget adds a target to a job.
func (s *Store) CreateTarget(ctx context.Context, t *Target) error {
	if err := t.Validate(); err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO targets(job_id, node_id, path, schedule, verify_schedule, verify_mode, enabled, run_on_connect)
		VALUES(?, ?, ?, ?, ?, ?, ?, ?)`, t.JobID, t.NodeID, t.Path, t.Schedule, t.VerifySchedule, t.VerifyMode, t.Enabled, t.RunOnConnect)
	if err != nil {
		return uniqueErr(err, "this node and path is already used by another target")
	}
	t.ID, _ = res.LastInsertId()
	return nil
}

// UpdateTarget changes schedule and settings. Node and path cannot change,
// because the index describes what is stored there.
func (s *Store) UpdateTarget(ctx context.Context, t *Target) error {
	if err := t.Validate(); err != nil {
		return err
	}
	return s.execOne(ctx, "UPDATE targets SET schedule=?, verify_schedule=?, verify_mode=?, enabled=?, run_on_connect=? WHERE id=?",
		t.Schedule, t.VerifySchedule, t.VerifyMode, t.Enabled, t.RunOnConnect, t.ID)
}

// DeleteTarget removes a target and its index (not the files).
func (s *Store) DeleteTarget(ctx context.Context, id int64) error {
	return s.execOne(ctx, "DELETE FROM targets WHERE id = ?", id)
}

// SetTargetScan records the last complete source scan.
func (s *Store) SetTargetScan(ctx context.Context, id, runID int64) error {
	_, err := s.db.ExecContext(ctx, "UPDATE targets SET last_scan_run = ? WHERE id = ?", runID, id)
	return err
}

// RefreshTargetTotals recomputes stored file count and bytes from the index.
func (s *Store) RefreshTargetTotals(ctx context.Context, id int64) (files, bytes int64, err error) {
	err = s.db.QueryRowContext(ctx, "SELECT count(*), coalesce(sum(size), 0) FROM files WHERE target_id = ?", id).Scan(&files, &bytes)
	if err != nil {
		return
	}
	_, err = s.db.ExecContext(ctx, "UPDATE targets SET stored_files = ?, stored_bytes = ? WHERE id = ?", files, bytes, id)
	return
}
