package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// ExportVersion identifies the config export format.
const ExportVersion = 1

// ConfigExport is a full copy of the configuration (not the index or run
// history). It contains peer secrets, so treat it like a password.
type ConfigExport struct {
	Version    int               `json:"version"`
	ExportedAt int64             `json:"exported_at"`
	Settings   map[string]string `json:"settings"`
	Nodes      []ExportNode      `json:"nodes"`
	Jobs       []Job             `json:"jobs"`
	Drives     []Drive           `json:"drives,omitempty"`
}

// ExportNode is a node including its secret.
type ExportNode struct {
	Node
	Secret []byte `json:"secret"`
}

// settings that are never exported or imported.
var privateSettings = map[string]bool{"admin_password_hash": true}

// ExportConfig collects settings, nodes, jobs and targets.
func (s *Store) ExportConfig(ctx context.Context) (*ConfigExport, error) {
	all, err := s.Settings(ctx)
	if err != nil {
		return nil, err
	}
	ex := &ConfigExport{Version: ExportVersion, ExportedAt: s.now().Unix(), Settings: map[string]string{}}
	for k, v := range all {
		if !privateSettings[k] {
			ex.Settings[k] = v
		}
	}
	nodes, err := s.ListNodes(ctx)
	if err != nil {
		return nil, err
	}
	for _, n := range nodes {
		sec, err := s.NodeSecret(ctx, n.ID)
		if err != nil {
			return nil, err
		}
		ex.Nodes = append(ex.Nodes, ExportNode{Node: n, Secret: sec})
	}
	if ex.Jobs, err = s.ListJobs(ctx); err != nil {
		return nil, err
	}
	if ex.Drives, err = s.ListDrives(ctx); err != nil {
		return nil, err
	}
	return ex, nil
}

// ImportConfig replaces nodes, jobs, targets and settings with the export.
// IDs are kept, so a rebuilt index matches the same targets. The index and
// run history of targets that no longer exist are dropped.
func (s *Store) ImportConfig(ctx context.Context, ex *ConfigExport) error {
	if ex == nil || ex.Version != ExportVersion {
		return errors.New("unsupported export file version")
	}
	for i := range ex.Drives {
		if err := ex.Drives[i].Validate(); err != nil {
			return fmt.Errorf("drive %q: %w", ex.Drives[i].UUID, err)
		}
	}
	for i := range ex.Jobs {
		j := ex.Jobs[i]
		if err := j.Validate(); err != nil {
			return fmt.Errorf("job %q: %w", j.Name, err)
		}
		for _, t := range j.Targets {
			if err := t.Validate(); err != nil {
				return fmt.Errorf("job %q target %s: %w", j.Name, t.Path, err)
			}
		}
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		exec := func(q string, args ...any) error {
			_, err := tx.ExecContext(ctx, q, args...)
			return err
		}
		for k, v := range ex.Settings {
			if privateSettings[k] {
				continue
			}
			if err := exec("INSERT INTO settings(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value", k, v); err != nil {
				return err
			}
		}
		if err := exec("DELETE FROM nodes"); err != nil {
			return err
		}
		for _, n := range ex.Nodes {
			if n.OfflineHours == 0 {
				// Export from before per-peer offline alerts: use the defaults.
				n.OfflineNotify, n.OfflineHours = true, 1
			}
			n.OfflineHours = min(n.OfflineHours, MaxOfflineHours)
			if err := exec("INSERT INTO nodes(id, name, secret, created_at, last_seen, hostname, version, offline_notify, offline_hours) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?)",
				n.ID, n.Name, n.Secret, n.CreatedAt, n.LastSeen, n.Hostname, n.Version, n.OfflineNotify, n.OfflineHours); err != nil {
				return err
			}
		}
		if err := exec("DELETE FROM drives"); err != nil {
			return err
		}
		for _, d := range ex.Drives {
			if err := exec("INSERT INTO drives("+driveCols+") VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
				d.UUID, d.Name, d.CreatedAt, d.LastSeen, d.LastMount, d.FSType, d.Total, d.Free,
				d.OnConnect, d.VerifyAfter, d.OverdueDays, d.OverdueNotified); err != nil {
				return uniqueErr(err, "duplicate drive in import file")
			}
		}
		// Jobs and targets are upserted by ID (never deleted and re-created),
		// so cascades do not wipe the index of targets that survive. Unique
		// columns get temporary values first to allow swaps.
		if err := exec("UPDATE jobs SET name = '__import_' || id"); err != nil {
			return err
		}
		if err := exec("UPDATE targets SET path = '/__import_' || id"); err != nil {
			return err
		}
		jobIDs, targetIDs := []any{}, []any{}
		for _, j := range ex.Jobs {
			inc, _ := json.Marshal(j.Includes)
			exc, _ := json.Marshal(j.Excludes)
			jobIDs = append(jobIDs, j.ID)
			if err := exec(`INSERT INTO jobs(id, name, source_node, source_path, includes, excludes, compare_mode,
				pre_hook, post_hook, protect_pct, protect_min, workers, enabled, created_at, updated_at, ignore_global_excludes, clean_excluded,
				mode, on_conflict, old_copies)
				VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
				ON CONFLICT(id) DO UPDATE SET name=excluded.name, source_node=excluded.source_node,
				source_path=excluded.source_path, includes=excluded.includes, excludes=excluded.excludes,
				compare_mode=excluded.compare_mode, pre_hook=excluded.pre_hook, post_hook=excluded.post_hook,
				protect_pct=excluded.protect_pct, protect_min=excluded.protect_min, workers=excluded.workers,
				enabled=excluded.enabled, updated_at=excluded.updated_at, ignore_global_excludes=excluded.ignore_global_excludes,
				clean_excluded=excluded.clean_excluded, mode=excluded.mode, on_conflict=excluded.on_conflict, old_copies=excluded.old_copies`,
				j.ID, j.Name, j.SourceNode, j.SourcePath, string(inc), string(exc), j.CompareMode, j.PreHook, j.PostHook,
				j.ProtectPct, j.ProtectMin, j.Workers, j.Enabled, j.CreatedAt, j.UpdatedAt, j.IgnoreGlobalExcludes, j.CleanExcluded,
				j.Mode, j.OnConflict, j.OldCopies); err != nil {
				return uniqueErr(err, "duplicate job name in import file")
			}
			for _, t := range j.Targets {
				targetIDs = append(targetIDs, t.ID)
				if err := exec(`INSERT INTO targets(id, job_id, node_id, path, schedule, verify_schedule, verify_mode, enabled,
					stored_files, stored_bytes, last_scan_run, run_on_connect) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
					ON CONFLICT(id) DO UPDATE SET job_id=excluded.job_id, node_id=excluded.node_id, path=excluded.path,
					schedule=excluded.schedule, verify_schedule=excluded.verify_schedule, verify_mode=excluded.verify_mode,
					enabled=excluded.enabled, run_on_connect=excluded.run_on_connect`,
					t.ID, j.ID, t.NodeID, t.Path, t.Schedule, t.VerifySchedule, t.VerifyMode, t.Enabled,
					t.StoredFiles, t.StoredBytes, t.LastScanRun, t.RunOnConnect); err != nil {
					return uniqueErr(err, "duplicate target node/path in import file")
				}
			}
		}
		if err := exec("DELETE FROM targets WHERE id NOT IN ("+placeholders(len(targetIDs))+")", targetIDs...); err != nil {
			return err
		}
		return exec("DELETE FROM jobs WHERE id NOT IN ("+placeholders(len(jobIDs))+")", jobIDs...)
	})
}

func placeholders(n int) string {
	if n == 0 {
		return "SELECT NULL WHERE 0" // empty set: NOT IN is true for every row
	}
	b := make([]byte, 0, 2*n)
	for i := 0; i < n; i++ {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, '?')
	}
	return string(b)
}
