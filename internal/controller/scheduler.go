package controller

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"bfbackup/internal/logbook"
	"bfbackup/internal/model"
	"bfbackup/internal/notify"
	"bfbackup/internal/store"
)

// schedulerLoop starts scheduled backups and verifies. Runs missed while
// the controller was down are not caught up.
func (c *Controller) schedulerLoop(ctx context.Context) {
	t := time.NewTicker(c.cfg.SchedulerTick)
	defer t.Stop()
	last := c.cfg.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		now := c.cfg.Now()
		c.scheduleDue(ctx, last, now)
		last = now
	}
}

// scheduleDue starts every run whose schedule fires in (from, to].
func (c *Controller) scheduleDue(ctx context.Context, from, to time.Time) {
	jobs, err := c.store.ListJobs(ctx)
	if err != nil {
		c.log.Error("scheduler: list jobs", "err", err)
		return
	}
	for _, j := range jobs {
		if !j.Enabled {
			continue
		}
		for _, t := range j.Targets {
			if !t.Enabled {
				continue
			}
			if due(t.Schedule, from, to) {
				c.startScheduled(ctx, RunRequest{Kind: store.KindBackup, TargetID: t.ID, Trigger: "schedule"})
			}
			if due(t.VerifySchedule, from, to) {
				kind := store.KindVerifyQuick
				if t.VerifyMode == "deep" {
					kind = store.KindVerifyDeep
				}
				c.startScheduled(ctx, RunRequest{Kind: kind, TargetID: t.ID, Trigger: "schedule"})
			}
		}
	}
}

func due(spec string, from, to time.Time) bool {
	if spec == "" {
		return false
	}
	s, err := store.CronParser.Parse(spec)
	if err != nil {
		return false
	}
	next := s.Next(from)
	return !next.After(to)
}

func (c *Controller) startScheduled(ctx context.Context, req RunRequest) {
	if req.Kind == store.KindBackup {
		// A backup that waits for the user's decision blocks the next ones,
		// so the same question is not asked again and again.
		if id := c.pendingDecision(ctx, req.TargetID); id != 0 {
			e := logbook.Entry{Kind: logbook.KindSchedule, TargetID: req.TargetID, Level: logbook.LevelEvent,
				Message: fmt.Sprintf("Backup (%s) skipped: run #%d waits for your decision", req.Trigger, id)}
			if t, err := c.store.GetTarget(ctx, req.TargetID); err == nil {
				e.JobID, e.Node = t.JobID, t.NodeID
			}
			c.logEvent(e)
			return
		}
	}
	if _, err := c.StartRun(ctx, req); err != nil {
		e := logbook.Entry{Kind: logbook.KindSchedule, TargetID: req.TargetID}
		if t, terr := c.store.GetTarget(ctx, req.TargetID); terr == nil {
			e.JobID, e.Node = t.JobID, t.NodeID
		}
		if errors.Is(err, ErrBusy) {
			c.log.Info("scheduled run skipped, already running or queued", "target", req.TargetID, "kind", req.Kind)
			e.Level, e.Message = logbook.LevelEvent, fmt.Sprintf("%s (%s) skipped: %v", kindLabel(req.Kind), req.Trigger, err)
			c.logEvent(e)
			return
		}
		c.log.Error("scheduled run failed to start", "target", req.TargetID, "err", err)
		e.Level, e.Message = logbook.LevelError, fmt.Sprintf("%s (%s) could not start: %v", kindLabel(req.Kind), req.Trigger, err)
		c.logEvent(e)
	}
}

// NextRuns returns the next scheduled backup time per target.
func (c *Controller) NextRun(spec string) int64 {
	if spec == "" {
		return 0
	}
	s, err := store.CronParser.Parse(spec)
	if err != nil {
		return 0
	}
	return s.Next(c.cfg.Now()).Unix()
}

// watchLoop checks peers once a minute and runs daily maintenance.
func (c *Controller) watchLoop(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	lastLogPrune := c.cfg.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		c.checkPeers(ctx)
		c.checkDrivesOverdue(ctx)
		c.maybeMaintenance(ctx)
		// At level 3 one backup can add millions of lines; keep the cap hourly.
		if now := c.cfg.Now(); now.Sub(lastLogPrune) >= time.Hour {
			lastLogPrune = now
			c.pruneLogs(ctx)
		}
	}
}

// pruneLogs applies the log retention settings.
func (c *Controller) pruneLogs(ctx context.Context) (int64, error) {
	s, err := c.Settings(ctx)
	if err != nil {
		return 0, err
	}
	return c.book.Prune(ctx, c.cfg.Now().AddDate(0, 0, -s.LogRetentionDays), s.LogMaxEntries)
}

// checkPeers keeps last_seen fresh for online peers and sends the offline
// alert of each peer that wants one, once per offline period.
func (c *Controller) checkPeers(ctx context.Context) {
	nodes, err := c.store.ListNodes(ctx)
	if err != nil {
		return
	}
	now := c.cfg.Now()
	for _, n := range nodes {
		limit := time.Duration(n.OfflineHours) * time.Hour
		c.mu.Lock()
		_, online := c.peers[n.ID]
		since, known := c.offSince[n.ID]
		notified := c.offNotify[n.ID]
		if !online && !known {
			// Offline since before this process started.
			since = c.started
			c.offSince[n.ID] = since
		}
		c.mu.Unlock()
		if online {
			c.store.SeenNode(ctx, n.ID)
			continue
		}
		if !n.OfflineNotify || n.LastSeen == 0 || notified || now.Sub(since) < limit {
			continue // no alert wanted, never connected yet, already reported, or within grace
		}
		c.mu.Lock()
		c.offNotify[n.ID] = true
		c.mu.Unlock()
		c.logEvent(logbook.Entry{Level: logbook.LevelError, Kind: logbook.KindPeer, Node: n.ID,
			Message: fmt.Sprintf("offline for more than %d hour(s)", n.OfflineHours)})
		c.Notify(ctx, notify.Event{Kind: notify.NodeOffline, Priority: 1, Title: "BFBackup: peer offline",
			Message: fmt.Sprintf("%s has been offline since %s", n.Name, time.Unix(max(n.LastSeen, since.Unix()), 0).Format("2006-01-02 15:04"))})
	}
}

func (c *Controller) maybeMaintenance(ctx context.Context) {
	s, err := c.Settings(ctx)
	if err != nil {
		return
	}
	now := c.cfg.Now()
	today := now.Format("2006-01-02")
	last, _ := c.store.Setting(ctx, "last_maintenance", "")
	if last == today || now.Hour() < s.MaintenanceHour {
		return
	}
	c.store.SetSetting(ctx, "last_maintenance", today)
	c.RunMaintenance(ctx)
}

// MaintenanceReport says what maintenance did.
type MaintenanceReport struct {
	TrashBatchesPurged int      `json:"trash_batches_purged"`
	RunsPruned         int64    `json:"runs_pruned"`
	LogsPruned         int64    `json:"logs_pruned"`
	DBSnapshot         string   `json:"db_snapshot"`
	Errors             []string `json:"errors"`
}

// RunMaintenance purges old trash and history and snapshots the database.
func (c *Controller) RunMaintenance(ctx context.Context) MaintenanceReport {
	rep := MaintenanceReport{Errors: []string{}}
	s, err := c.Settings(ctx)
	if err != nil {
		rep.Errors = append(rep.Errors, err.Error())
		return rep
	}
	now := c.cfg.Now()
	cutoff := now.AddDate(0, 0, -s.TrashRetentionDays)
	jobs, err := c.store.ListJobs(ctx)
	if err != nil {
		rep.Errors = append(rep.Errors, err.Error())
	}
	purge := func(node, root string) {
		ep, err := c.Endpoint(node)
		if err != nil {
			return // offline: next time
		}
		n, err := ep.PurgeTrash(ctx, root, cutoff)
		rep.TrashBatchesPurged += n
		if err != nil && !os.IsNotExist(err) {
			rep.Errors = append(rep.Errors, fmt.Sprintf("purge trash %s:%s: %v", node, root, err))
		}
	}
	for _, j := range jobs {
		purge(j.SourceNode, j.SourcePath)
		for _, t := range j.Targets {
			purge(t.NodeID, t.Path)
		}
	}
	if rep.RunsPruned, err = c.store.PruneRuns(ctx, now.AddDate(0, 0, -s.RunRetentionDays)); err != nil {
		rep.Errors = append(rep.Errors, err.Error())
	}
	if rep.LogsPruned, err = c.pruneLogs(ctx); err != nil {
		rep.Errors = append(rep.Errors, "log cleanup: "+err.Error())
	}
	c.store.PurgeSessions(ctx)
	if p, err := c.SnapshotDB(ctx, s.DBBackupKeep); err != nil {
		rep.Errors = append(rep.Errors, "database snapshot: "+err.Error())
	} else {
		rep.DBSnapshot = p
	}
	c.log.Info("maintenance done", "trash_purged", rep.TrashBatchesPurged, "runs_pruned", rep.RunsPruned, "errors", len(rep.Errors))
	c.logEvent(logbook.Entry{Level: logbook.LevelEvent, Kind: logbook.KindSystem, Node: model.LocalNodeID,
		Message: fmt.Sprintf("maintenance done: %d trash batches purged, %d old runs and %d log entries removed", rep.TrashBatchesPurged, rep.RunsPruned, rep.LogsPruned)})
	for _, e := range rep.Errors {
		c.logEvent(logbook.Entry{Level: logbook.LevelError, Kind: logbook.KindSystem, Message: "maintenance: " + e})
	}
	return rep
}

// SnapshotDB writes db-backups/bfbackup-<date>.db and keeps the newest keep files.
func (c *Controller) SnapshotDB(ctx context.Context, keep int) (string, error) {
	dir := filepath.Join(c.cfg.DataDir, "db-backups")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	p := filepath.Join(dir, "bfbackup-"+c.cfg.Now().Format("20060102-150405")+".db")
	if err := c.store.Snapshot(ctx, p); err != nil {
		return "", err
	}
	entries, _ := os.ReadDir(dir)
	var snaps []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "bfbackup-") && strings.HasSuffix(e.Name(), ".db") {
			snaps = append(snaps, e.Name())
		}
	}
	sort.Strings(snaps)
	for len(snaps) > keep {
		os.Remove(filepath.Join(dir, snaps[0]))
		snaps = snaps[1:]
	}
	return p, nil
}
