package controller

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"bfbackup/internal/endpoint"
	"bfbackup/internal/model"
	"bfbackup/internal/store"
)

func overlaps(a, b string) bool {
	return a == b || strings.HasPrefix(b, a+"/") || strings.HasPrefix(a, b+"/")
}

func (c *Controller) checkJob(ctx context.Context, j *store.Job) error {
	if err := j.Validate(); err != nil {
		return err
	}
	if isDriveNode(j.SourceNode) {
		return errors.New("a USB drive can only be a backup target, not a source")
	}
	return c.nodeExists(ctx, j.SourceNode)
}

func (c *Controller) checkTarget(ctx context.Context, j store.Job, t *store.Target) error {
	if err := t.Validate(); err != nil {
		return err
	}
	if err := c.nodeExists(ctx, t.NodeID); err != nil {
		return err
	}
	if isDriveNode(t.NodeID) {
		if t.Schedule != "" || t.VerifySchedule != "" {
			return errors.New("USB drive targets have no schedule: they run when the drive is plugged in, or by hand")
		}
		t.RunOnConnect = false
	}
	if t.NodeID == j.SourceNode && overlaps(t.Path, j.SourcePath) {
		return fmt.Errorf("target %s overlaps the source %s on the same node", t.Path, j.SourcePath)
	}
	return nil
}

// CreateJob validates and stores a new job together with its targets.
func (c *Controller) CreateJob(ctx context.Context, j *store.Job) error {
	if err := c.checkJob(ctx, j); err != nil {
		return err
	}
	targets := j.Targets
	for i := range targets {
		if err := c.checkTarget(ctx, *j, &targets[i]); err != nil {
			return err
		}
	}
	if err := c.store.CreateJob(ctx, j); err != nil {
		return err
	}
	j.Targets = nil
	for _, t := range targets {
		t.JobID = j.ID
		if err := c.store.CreateTarget(ctx, &t); err != nil {
			c.store.DeleteJob(ctx, j.ID)
			return err
		}
		j.Targets = append(j.Targets, t)
	}
	c.audit(ctx, "job_create", "created job %q (%d)", j.Name, j.ID)
	return nil
}

// UpdateJob changes job settings. Targets are managed separately.
func (c *Controller) UpdateJob(ctx context.Context, j *store.Job) error {
	if err := c.checkJob(ctx, j); err != nil {
		return err
	}
	old, err := c.store.GetJob(ctx, j.ID)
	if err != nil {
		return err
	}
	for _, t := range old.Targets {
		if t.NodeID == j.SourceNode && overlaps(t.Path, j.SourcePath) {
			return fmt.Errorf("source %s overlaps target %s on the same node", j.SourcePath, t.Path)
		}
	}
	if err := c.store.UpdateJob(ctx, j); err != nil {
		return err
	}
	c.audit(ctx, "job_update", "updated job %q (%d)", j.Name, j.ID)
	return nil
}

// DeleteJob removes a job and its index. Files are not touched.
func (c *Controller) DeleteJob(ctx context.Context, id int64) error {
	j, err := c.store.GetJob(ctx, id)
	if err != nil {
		return err
	}
	for _, t := range j.Targets {
		if c.TargetBusy(t.ID) != 0 {
			return ErrBusy
		}
	}
	if err := c.store.DeleteJob(ctx, id); err != nil {
		return err
	}
	c.audit(ctx, "job_delete", "deleted job %q (%d); files were kept", j.Name, id)
	return nil
}

// AddTarget adds a target to a job.
func (c *Controller) AddTarget(ctx context.Context, t *store.Target) error {
	j, err := c.store.GetJob(ctx, t.JobID)
	if err != nil {
		return err
	}
	if err := c.checkTarget(ctx, j, t); err != nil {
		return err
	}
	if err := c.store.CreateTarget(ctx, t); err != nil {
		return err
	}
	c.audit(ctx, "target_add", "job %q: added target %s:%s", j.Name, t.NodeID, t.Path)
	return nil
}

// UpdateTarget changes schedules and flags.
func (c *Controller) UpdateTarget(ctx context.Context, t *store.Target) error {
	old, err := c.store.GetTarget(ctx, t.ID)
	if err != nil {
		return err
	}
	t.JobID, t.NodeID, t.Path = old.JobID, old.NodeID, old.Path
	if isDriveNode(t.NodeID) {
		t.Schedule, t.VerifySchedule, t.RunOnConnect = "", "", false
	}
	if err := c.store.UpdateTarget(ctx, t); err != nil {
		return err
	}
	c.audit(ctx, "target_update", "updated target %d", t.ID)
	return nil
}

// RemoveTarget deletes a target and its index (files stay).
func (c *Controller) RemoveTarget(ctx context.Context, id int64) error {
	if c.TargetBusy(id) != 0 {
		return ErrBusy
	}
	t, err := c.store.GetTarget(ctx, id)
	if err != nil {
		return err
	}
	if err := c.store.DeleteTarget(ctx, id); err != nil {
		return err
	}
	c.audit(ctx, "target_remove", "removed target %s:%s; files were kept", t.NodeID, t.Path)
	return nil
}

// Mkdir creates a folder on a node for the folder picker.
func (c *Controller) Mkdir(ctx context.Context, nodeID, path string) error {
	ep, err := c.Endpoint(nodeID)
	if err != nil {
		return err
	}
	m, ok := ep.(endpoint.Mkdirer)
	if !ok {
		return model.ErrUnsupported
	}
	if err := m.Mkdir(ctx, path); err != nil {
		if errors.Is(err, model.ErrUnsupported) {
			return fmt.Errorf("%s runs BFBackup %s, which cannot create folders. Update it to %s", c.nodeName(ctx, nodeID), c.nodeVersion(nodeID), c.cfg.Version)
		}
		return err
	}
	c.audit(ctx, "mkdir", "created folder %s on %s", path, c.nodeName(ctx, nodeID))
	return nil
}

// ListDir lists a directory on a node for the folder picker. An empty path
// lists the node's allowed roots.
func (c *Controller) ListDir(ctx context.Context, nodeID, path string) ([]model.Entry, error) {
	ep, err := c.Endpoint(nodeID)
	if err != nil {
		return nil, err
	}
	if path == "" {
		info, err := ep.Info(ctx)
		if err != nil {
			return nil, err
		}
		out := []model.Entry{}
		for _, r := range info.Roots {
			out = append(out, model.Entry{Name: r.Path, Type: model.TypeDir})
		}
		return out, nil
	}
	return ep.ListDir(ctx, path)
}

// NodeInfo returns live information about a node.
func (c *Controller) NodeInfo(ctx context.Context, nodeID string) (model.NodeInfo, error) {
	ep, err := c.Endpoint(nodeID)
	if err != nil {
		return model.NodeInfo{}, err
	}
	return ep.Info(ctx)
}

// TargetStatus is a target with its recent history for the dashboard.
type TargetStatus struct {
	store.Target
	NodeName    string     `json:"node_name"`
	NodeOnline  bool       `json:"node_online"`
	LastBackup  *store.Run `json:"last_backup"`
	LastSuccess *store.Run `json:"last_success"`
	LastVerify  *store.Run `json:"last_verify"`
	NextBackup  int64      `json:"next_backup"`
	NextVerify  int64      `json:"next_verify"`
	ActiveRun   int64      `json:"active_run"`
	NotInSource int64      `json:"not_in_source"`
}

// JobStatus is a job with target status.
type JobStatus struct {
	store.Job
	SourceName   string         `json:"source_name"`
	SourceOnline bool           `json:"source_online"`
	Targets      []TargetStatus `json:"targets"`
}

// Overview is everything the dashboard shows.
type Overview struct {
	Nodes       []NodeView  `json:"nodes"`
	Jobs        []JobStatus `json:"jobs"`
	Active      []ActiveRun `json:"active"`
	Approvals   []store.Run `json:"approvals"`
	Failures    []store.Run `json:"failures"`
	StoredFiles int64       `json:"stored_files"`
	StoredBytes int64       `json:"stored_bytes"`
	Version     string      `json:"version"`
	// USB drives: how many, connected, overdue.
	Drives          int `json:"drives"`
	DrivesConnected int `json:"drives_connected"`
	DrivesOverdue   int `json:"drives_overdue"`
}

// JobStatuses returns all jobs with target status.
func (c *Controller) JobStatuses(ctx context.Context) ([]JobStatus, error) {
	nodes, err := c.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	byID := map[string]NodeView{}
	for _, n := range nodes {
		byID[n.ID] = n
	}
	if drives, err := c.store.ListDrives(ctx); err == nil {
		c.drv.mu.Lock()
		for _, d := range drives {
			_, on := c.drv.online[d.UUID]
			byID[model.DriveNodeID(d.UUID)] = NodeView{Node: store.Node{ID: model.DriveNodeID(d.UUID), Name: "USB · " + d.Name}, Online: on}
		}
		c.drv.mu.Unlock()
	}
	jobs, err := c.store.ListJobs(ctx)
	if err != nil {
		return nil, err
	}
	out := []JobStatus{}
	for _, j := range jobs {
		js := JobStatus{Job: j, SourceName: byID[j.SourceNode].Name, SourceOnline: byID[j.SourceNode].Online, Targets: []TargetStatus{}}
		for _, t := range j.Targets {
			ts := TargetStatus{Target: t, NodeName: byID[t.NodeID].Name, NodeOnline: byID[t.NodeID].Online, ActiveRun: c.TargetBusy(t.ID)}
			ts.LastBackup, _ = c.store.LastRun(ctx, t.ID, store.KindBackup)
			ts.LastSuccess, _ = c.store.LastSuccess(ctx, t.ID, store.KindBackup)
			ts.LastVerify, _ = c.store.LastRun(ctx, t.ID, store.KindVerifyQuick, store.KindVerifyDeep)
			if j.Enabled && t.Enabled {
				ts.NextBackup = c.NextRun(t.Schedule)
				ts.NextVerify = c.NextRun(t.VerifySchedule)
			}
			if t.LastScanRun > 0 {
				ts.NotInSource = c.countNotSeen(ctx, t)
			}
			js.Targets = append(js.Targets, ts)
		}
		out = append(out, js)
	}
	return out, nil
}

// Overview collects the dashboard data.
func (c *Controller) Overview(ctx context.Context) (Overview, error) {
	o := Overview{Version: c.cfg.Version, Active: c.ActiveRuns()}
	var err error
	if o.Nodes, err = c.Nodes(ctx); err != nil {
		return o, err
	}
	if o.Jobs, err = c.JobStatuses(ctx); err != nil {
		return o, err
	}
	for _, j := range o.Jobs {
		for _, t := range j.Targets {
			o.StoredFiles += t.StoredFiles
			o.StoredBytes += t.StoredBytes
		}
	}
	if o.Approvals, _, err = c.store.ListRuns(ctx, store.RunFilter{Statuses: []string{store.StatusApproval}, Limit: 20}); err != nil {
		return o, err
	}
	// Problems stay on the attention list until dismissed (at most a week).
	since := c.cfg.Now().AddDate(0, 0, -7).Unix()
	if o.Failures, _, err = c.store.ListRuns(ctx, store.RunFilter{Statuses: []string{store.StatusFailed, store.StatusWarning, store.StatusInterrupted},
		Unacknowledged: true, Since: since, Limit: 50}); err != nil {
		return o, err
	}
	o.Drives, o.DrivesConnected, o.DrivesOverdue = c.driveCounts(ctx)
	sort.Slice(o.Active, func(i, j int) bool { return o.Active[i].ID < o.Active[j].ID })
	return o, nil
}

// Forecast estimates when a target runs full.
type Forecast struct {
	TargetID      int64           `json:"target_id"`
	StoredBytes   int64           `json:"stored_bytes"`
	StoredFiles   int64           `json:"stored_files"`
	Free          uint64          `json:"free"`
	Total         uint64          `json:"total"`
	Live          bool            `json:"live"` // free/total measured now
	CopiedPerDay  float64         `json:"copied_per_day"`
	DaysUntilFull float64         `json:"days_until_full"` // -1 = not predictable
	Daily         []store.DayStat `json:"daily"`
}

// TargetForecast computes the space forecast from the last 30 days.
func (c *Controller) TargetForecast(ctx context.Context, id int64) (Forecast, error) {
	t, err := c.store.GetTarget(ctx, id)
	if err != nil {
		return Forecast{}, err
	}
	f := Forecast{TargetID: id, StoredBytes: t.StoredBytes, StoredFiles: t.StoredFiles, DaysUntilFull: -1}
	if ep, err := c.Endpoint(t.NodeID); err == nil {
		if u, err := ep.StatFS(ctx, t.Path); err == nil {
			f.Free, f.Total, f.Live = u.Free, u.Total, true
		}
	}
	if !f.Live {
		if r, _ := c.store.LastSuccess(ctx, id, store.KindBackup); r != nil {
			f.Free, f.Total = r.Stats.TargetFree, r.Stats.TargetTotal
		}
	}
	if f.Daily, err = c.store.DailyStats(ctx, c.cfg.Now().AddDate(0, 0, -30), id); err != nil {
		return f, err
	}
	var sum int64
	for _, d := range f.Daily {
		sum += d.Bytes
	}
	f.CopiedPerDay = float64(sum) / 30
	if f.CopiedPerDay > 0 && f.Free > 0 {
		f.DaysUntilFull = float64(f.Free) / f.CopiedPerDay
	}
	return f, nil
}

// ExportConfig returns the configuration for download.
func (c *Controller) ExportConfig(ctx context.Context) (*store.ConfigExport, error) {
	ex, err := c.store.ExportConfig(ctx)
	if err == nil {
		c.audit(ctx, "config_export", "configuration exported")
	}
	return ex, err
}

// ImportConfig replaces the configuration. All peers reconnect.
func (c *Controller) ImportConfig(ctx context.Context, ex *store.ConfigExport) error {
	c.mu.Lock()
	active := len(c.active)
	c.mu.Unlock()
	if active > 0 {
		return fmt.Errorf("stop the %d active run(s) first", active)
	}
	if err := c.store.ImportConfig(ctx, ex); err != nil {
		return err
	}
	c.mu.Lock()
	peers := c.peers
	c.peers = map[string]*peerConn{}
	c.mu.Unlock()
	for _, p := range peers {
		p.sess.Close()
	}
	s, _ := c.Settings(ctx)
	c.setHeavyCap(s.MaxParallelRuns)
	c.audit(ctx, "config_import", "configuration imported (%d jobs, %d peers)", len(ex.Jobs), len(ex.Nodes))
	return nil
}
