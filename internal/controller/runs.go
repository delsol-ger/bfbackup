package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"bfbackup/internal/endpoint"
	"bfbackup/internal/engine"
	"bfbackup/internal/logbook"
	"bfbackup/internal/model"
	"bfbackup/internal/notify"
	"bfbackup/internal/store"
)

// ErrBusy is returned when a target already has a run of the same kind
// running or queued.
var ErrBusy = errors.New("the same run is already running or queued for this target")

type activeRun struct {
	run      store.Run
	cancel   context.CancelFunc
	progress *engine.Progress
	targets  []int64
	act      runActivity
	// Queue (see queue.go).
	nodes   []string          // peers the run uses
	names   map[string]string // peer names for the waiting text
	heavy   bool              // needs a parallel slot
	start   chan struct{}     // closed when the run may start
	granted bool
	waiting string        // why it still waits
	stall   time.Duration // the watchdog's limit; 0 = none
}

// RestoreRequest selects what to restore and where.
type RestoreRequest struct {
	Paths     []string `json:"paths"`
	DestNode  string   `json:"dest_node"` // "" = the job's source node
	DestPath  string   `json:"dest_path"` // "" = the job's source path
	Overwrite bool     `json:"overwrite"`
}

// DeleteRequest selects what to delete.
type DeleteRequest struct {
	Paths      []string `json:"paths"`
	FromSource bool     `json:"from_source"`
	TargetIDs  []int64  `json:"target_ids"` // empty = all targets of the job
	Permanent  bool     `json:"permanent"`  // skip the trash
}

// RunRequest starts a run.
type RunRequest struct {
	Kind     string
	JobID    int64 // required for delete
	TargetID int64 // required for everything else
	Trigger  string
	Approved bool
	Repair   bool // verify
	Hash     bool // rebuild_index
	// Sync runs a backup of a copy job once as a sync: files gone from the
	// source are removed from the target, to its trash (store.OldTrash) or
	// for good (store.OldDelete). The job itself stays a copy job.
	Sync    string
	Restore *RestoreRequest
	Delete  *DeleteRequest
}

func heavyKind(k string) bool {
	return k == store.KindBackup || k == store.KindVerifyQuick || k == store.KindVerifyDeep || k == store.KindRebuildIndex
}

// StartRun validates the request, records the run and queues it. It starts
// in the background as soon as its peers and targets are free and, for heavy
// runs (backup, verify, rebuild), a parallel slot is free.
func (c *Controller) StartRun(ctx context.Context, req RunRequest) (store.Run, error) {
	var job store.Job
	var targets []int64
	var err error
	switch req.Kind {
	case store.KindDelete:
		if req.Delete == nil || len(req.Delete.Paths) == 0 {
			return store.Run{}, errors.New("nothing selected")
		}
		if job, err = c.store.GetJob(ctx, req.JobID); err != nil {
			return store.Run{}, err
		}
		sel := map[int64]bool{}
		for _, id := range req.Delete.TargetIDs {
			sel[id] = true
		}
		for _, t := range job.Targets {
			if len(sel) == 0 || sel[t.ID] {
				targets = append(targets, t.ID)
			}
		}
		if len(targets) == 0 && !req.Delete.FromSource {
			return store.Run{}, errors.New("no target selected")
		}
	case store.KindBackup, store.KindDryRun, store.KindVerifyQuick, store.KindVerifyDeep, store.KindRestore, store.KindRebuildIndex:
		t, err := c.store.GetTarget(ctx, req.TargetID)
		if err != nil {
			return store.Run{}, err
		}
		if job, err = c.store.GetJob(ctx, t.JobID); err != nil {
			return store.Run{}, err
		}
		if req.Kind == store.KindRestore && (req.Restore == nil || len(req.Restore.Paths) == 0) {
			return store.Run{}, errors.New("nothing selected to restore")
		}
		targets = []int64{t.ID}
	default:
		return store.Run{}, fmt.Errorf("unknown run kind %q", req.Kind)
	}
	if req.Sync != "" {
		switch {
		case req.Kind != store.KindBackup:
			return store.Run{}, errors.New("only a backup can run as a sync")
		case job.Mode != store.ModeCopy:
			return store.Run{}, errors.New("the job already syncs; use Run now")
		case req.Sync != store.OldTrash && req.Sync != store.OldDelete:
			return store.Run{}, fmt.Errorf("sync removal must be %q or %q", store.OldTrash, store.OldDelete)
		}
	}
	if req.Trigger == "" {
		req.Trigger = "manual"
	}
	details := map[string]any{}
	if req.Restore != nil {
		details["restore"] = req.Restore
	}
	if req.Delete != nil {
		details["delete"] = req.Delete
	}
	if req.Repair {
		details["repair"] = true
	}
	if req.Hash {
		details["hash"] = true
	}
	if req.Sync != "" {
		details["sync"] = req.Sync
	}
	db, _ := json.Marshal(details)
	run := store.Run{JobID: job.ID, Kind: req.Kind, Status: store.StatusQueued, Trigger: req.Trigger, Details: db}
	if len(targets) > 0 && req.Kind != store.KindDelete {
		run.TargetID = targets[0]
	}

	nodes := runNodes(job, targets, req)
	names := map[string]string{}
	for _, n := range nodes {
		names[n] = c.nodeName(ctx, n)
	}

	c.mu.Lock()
	if id := c.sameKindLocked(req.Kind, targets); id != 0 {
		c.mu.Unlock()
		return store.Run{}, fmt.Errorf("%w (run #%d)", ErrBusy, id)
	}
	if err := c.store.CreateRun(ctx, &run); err != nil {
		c.mu.Unlock()
		return store.Run{}, err
	}
	runCtx, cancel := context.WithCancel(context.Background())
	ar := &activeRun{run: run, cancel: cancel, progress: engine.NewProgress(), targets: targets,
		act:   runActivity{book: c.book, base: runEntry(job, run, req), last: &lastLine{}},
		nodes: nodes, names: names, heavy: heavyKind(req.Kind), start: make(chan struct{})}
	ar.progress.SetPhase("queued")
	c.active[run.ID] = ar
	c.queue = append(c.queue, ar)
	c.pumpLocked()
	if ar.waiting != "" {
		ar.act.logRun(logbook.LevelEvent, "%s queued (%s): %s", kindLabel(run.Kind), run.Trigger, ar.waiting)
	}
	c.mu.Unlock()

	c.wg.Add(1)
	go func() {
		defer c.wg.Done()
		c.execute(runCtx, ar, job, req)
	}()
	return run, nil
}

// runEntry is the log entry template of a run: its job, target and the
// nodes involved (Node is where files are written).
func runEntry(job store.Job, run store.Run, req RunRequest) logbook.Entry {
	e := logbook.Entry{Kind: run.Kind, JobID: job.ID, TargetID: run.TargetID, RunID: run.ID}
	tgtNode := ""
	for _, t := range job.Targets {
		if t.ID == run.TargetID {
			tgtNode = t.NodeID
		}
	}
	switch req.Kind {
	case store.KindBackup, store.KindDryRun:
		e.Node, e.SrcNode = tgtNode, job.SourceNode
	case store.KindRestore:
		e.Node, e.SrcNode = job.SourceNode, tgtNode
		if req.Restore != nil && req.Restore.DestNode != "" {
			e.Node = req.Restore.DestNode
		}
	case store.KindDelete:
		if req.Delete != nil && req.Delete.FromSource {
			e.SrcNode = job.SourceNode
		}
		// With the targets on one node, that node is where the delete happens.
		nodes := map[string]bool{}
		sel := map[int64]bool{}
		if req.Delete != nil {
			for _, id := range req.Delete.TargetIDs {
				sel[id] = true
			}
		}
		for _, t := range job.Targets {
			if len(sel) == 0 || sel[t.ID] {
				nodes[t.NodeID] = true
			}
		}
		if len(nodes) == 1 {
			for n := range nodes {
				e.Node = n
			}
		}
	default:
		e.Node = tgtNode
	}
	return e
}

// logRun writes a run-level line (no path).
func (a runActivity) logRun(level int, format string, args ...any) {
	a.Log(level, "", "", fmt.Sprintf(format, args...))
}

func (c *Controller) execute(ctx context.Context, ar *activeRun, job store.Job, req RunRequest) {
	defer ar.cancel()
	defer func() {
		c.mu.Lock()
		c.releaseLocked(ar)
		c.mu.Unlock()
	}()
	run := ar.run
	if !c.waitTurn(ctx, ar) {
		c.finish(ctx, &run, job, ar.act, nil, ctx.Err())
		return
	}
	run.Status = store.StatusRunning
	run.StartedAt = c.cfg.Now().Unix()
	c.store.UpdateRun(context.Background(), &run)
	c.mu.Lock()
	ar.run = run
	c.mu.Unlock()
	c.log.Info("run started", "run", run.ID, "kind", run.Kind, "job", job.Name, "trigger", run.Trigger)
	ar.act.logRun(logbook.LevelEvent, "%s started (%s)", kindLabel(run.Kind), run.Trigger)
	if req.Sync != "" {
		where := "moved to the trash"
		if req.Sync == store.OldDelete {
			where = "deleted permanently"
		}
		ar.act.logRun(logbook.LevelEvent, "runs once as a sync: files gone from the source are %s on the target", where)
	}

	ctx = endpoint.WithWaitNote(ctx, func(msg string) { ar.noteWait(msg) })
	stalled := c.watchStall(ctx, ar)
	res, err := c.dispatch(ctx, ar, job, req)
	err = stalled(err)
	setDetail(&run, "phases", ar.progress.Phases())
	c.finish(ctx, &run, job, ar.act, res, err)
}

// noteWait shows what the run waits for (a peer to reconnect) and logs
// when the wait starts and ends.
func (ar *activeRun) noteWait(msg string) {
	was := ar.progress.SetWaiting(msg)
	switch {
	case was == "" && msg != "":
		ar.act.logRun(logbook.LevelEvent, "%s", msg)
	case was != "" && msg == "":
		ar.act.logRun(logbook.LevelEvent, "the connection is back, continuing")
	}
}

// setDetail stores one value in the run's details.
func setDetail(run *store.Run, key string, v any) {
	details := map[string]any{}
	json.Unmarshal(run.Details, &details)
	details[key] = v
	run.Details, _ = json.Marshal(details)
}

func kindLabel(k string) string {
	l := strings.ReplaceAll(k, "_", " ")
	if l == "dryrun" {
		l = "dry run"
	}
	return strings.ToUpper(l[:1]) + l[1:]
}

func (c *Controller) dispatch(ctx context.Context, ar *activeRun, job store.Job, req RunRequest) (*engine.Result, error) {
	// Always use fresh rows: totals and last scan change between runs.
	job, err := c.store.GetJob(ctx, job.ID)
	if err != nil {
		return nil, err
	}
	if req.Kind == store.KindDelete {
		spec := engine.DeleteSpec{RunID: ar.run.ID, Job: job, Paths: req.Delete.Paths, Permanent: req.Delete.Permanent, Log: ar.act}
		if req.Delete.FromSource {
			if spec.Src, err = c.Endpoint(job.SourceNode); err != nil {
				return nil, fmt.Errorf("source %s: %w", c.nodeName(ctx, job.SourceNode), err)
			}
		}
		for _, id := range ar.targets {
			t, err := c.store.GetTarget(ctx, id)
			if err != nil {
				return nil, err
			}
			ep, err := c.Endpoint(t.NodeID)
			if errors.Is(err, model.ErrOffline) {
				// Delete on the reachable targets; this one is reported
				// and done by "Try again" once its node is back.
				spec.Skipped = append(spec.Skipped, engine.SkippedTarget{TargetID: t.ID, NodeID: t.NodeID,
					Path: t.Path, Reason: "offline"})
				continue
			}
			if err != nil {
				return nil, fmt.Errorf("target %s: %w", c.nodeName(ctx, t.NodeID), err)
			}
			spec.Targets = append(spec.Targets, engine.TargetEP{Target: t, EP: ep})
		}
		if spec.Permanent {
			if err := c.checkCanRemove(ctx, spec); err != nil {
				return nil, err
			}
		}
		return c.engine.Delete(ctx, spec)
	}

	tgt, err := c.store.GetTarget(ctx, req.TargetID)
	if err != nil {
		return nil, err
	}
	dst, err := c.Endpoint(tgt.NodeID)
	if err != nil {
		return nil, fmt.Errorf("target node %s: %w", c.nodeName(ctx, tgt.NodeID), err)
	}
	prog, runID := ar.progress, ar.run.ID
	switch req.Kind {
	case store.KindBackup, store.KindDryRun:
		src, err := c.Endpoint(job.SourceNode)
		if err != nil {
			return nil, fmt.Errorf("source node %s: %w", c.nodeName(ctx, job.SourceNode), err)
		}
		if job.Excludes, err = c.effectiveExcludes(ctx, job); err != nil {
			return nil, err
		}
		s, err := c.Settings(ctx)
		if err != nil {
			return nil, err
		}
		if req.Sync != "" {
			job.Mode, job.OldCopies = store.ModeSync, req.Sync
		}
		job = c.withoutHooks(ctx, job)
		history, err := c.backupHistory(ctx, tgt.ID, runID)
		if err != nil {
			return nil, err
		}
		return c.engine.Backup(ctx, engine.BackupSpec{RunID: runID, Job: job, Target: tgt, Src: src, Dst: dst,
			DryRun: req.Kind == store.KindDryRun, Approved: req.Approved, Progress: prog, Log: ar.act,
			Guards: &s.Guards, History: history})
	case store.KindVerifyQuick, store.KindVerifyDeep:
		excludes, err := c.effectiveExcludes(ctx, job)
		if err != nil {
			return nil, err
		}
		return c.engine.Verify(ctx, engine.VerifySpec{RunID: runID, Target: tgt, Dst: dst, Excludes: excludes,
			Clean: job.CleanExcluded, CleanLimit: c.deleteLimit(ctx, tgt.StoredFiles),
			Deep: req.Kind == store.KindVerifyDeep, Repair: req.Repair, Progress: prog, Log: ar.act})
	case store.KindRebuildIndex:
		return c.engine.RebuildIndex(ctx, engine.RebuildSpec{RunID: runID, Target: tgt, Dst: dst, Hash: req.Hash, Progress: prog, Log: ar.act})
	case store.KindRestore:
		r := req.Restore
		destNode, destPath := r.DestNode, r.DestPath
		if destNode == "" {
			destNode = job.SourceNode
		}
		if destPath == "" {
			destPath = job.SourcePath
		}
		to, err := c.Endpoint(destNode)
		if err != nil {
			return nil, fmt.Errorf("destination node %s: %w", c.nodeName(ctx, destNode), err)
		}
		return c.engine.Restore(ctx, engine.RestoreSpec{RunID: runID, Target: tgt, From: dst, To: to, ToRoot: destPath,
			Paths: r.Paths, Overwrite: r.Overwrite, Workers: job.Workers, Progress: prog, Log: ar.act})
	}
	return nil, fmt.Errorf("unknown run kind %q", req.Kind)
}

func (c *Controller) finish(ctx context.Context, run *store.Run, job store.Job, act runActivity, res *engine.Result, err error) {
	bg := context.WithoutCancel(ctx)
	details := map[string]any{}
	json.Unmarshal(run.Details, &details)
	if res != nil {
		run.Stats = res.Stats
		for k, v := range res.Details {
			details[k] = v
		}
		if res.Message != "" {
			details["message"] = res.Message
		}
		c.store.AddRunErrors(bg, run.ID, res.Errors)
	}
	switch {
	case errors.Is(err, context.Canceled):
		run.Status, run.Error = store.StatusCancelled, "cancelled"
	case err != nil:
		run.Status, run.Error = store.StatusFailed, err.Error()
	case res != nil:
		run.Status = res.Status
		if res.Status == store.StatusApproval {
			run.Error = res.Message
		}
	default:
		run.Status = store.StatusFailed
	}
	run.Details, _ = json.Marshal(details)
	run.FinishedAt = c.cfg.Now().Unix()
	if uerr := c.store.UpdateRun(bg, run); uerr != nil {
		c.log.Error("saving run failed", "run", run.ID, "err", uerr)
	}
	c.log.Info("run finished", "run", run.ID, "kind", run.Kind, "status", run.Status, "err", run.Error,
		"transferred", run.Stats.Transferred, "bytes", run.Stats.TransferredBytes, "failed", run.Stats.Failed)
	c.logFinish(bg, act, run, res)
	c.notifyRun(bg, run, job, res)
}

// checkCanRemove makes sure every node of a permanent delete supports it
// before anything is deleted: a source deleted for good while an old peer
// keeps its backup copy is the worst order.
func (c *Controller) checkCanRemove(ctx context.Context, spec engine.DeleteSpec) error {
	type side struct {
		node, root string
		ep         endpoint.Endpoint
	}
	var sides []side
	if spec.Src != nil {
		sides = append(sides, side{spec.Job.SourceNode, spec.Job.SourcePath, spec.Src})
	}
	for _, t := range spec.Targets {
		sides = append(sides, side{t.Target.NodeID, t.Target.Path, t.EP})
	}
	for _, s := range sides {
		// A name that never exists: supported nodes answer "not found".
		err := s.ep.Remove(ctx, s.root, ".bfbackup-capability-check")
		if errors.Is(err, model.ErrUnsupported) {
			return fmt.Errorf("%s runs BFBackup %s, which cannot delete permanently. Update it to %s. Nothing was deleted",
				c.nodeName(ctx, s.node), c.nodeVersion(s.node), c.cfg.Version)
		}
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return fmt.Errorf("%s: %w. Nothing was deleted", c.nodeName(ctx, s.node), err)
		}
	}
	return nil
}

// nodeVersion is the version a connected peer reported.
func (c *Controller) nodeVersion(id string) string {
	if id == model.LocalNodeID || isDriveNode(id) {
		return c.cfg.Version
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if p, ok := c.peers[id]; ok && p.info.Version != "" {
		return p.info.Version
	}
	return "(unknown version)"
}

// failedWhere says on which nodes files failed, e.g. "1 on CoLo", when the
// errors know their node.
func (c *Controller) failedWhere(ctx context.Context, errs []store.RunError) string {
	count := map[string]int{}
	var order []string
	for _, e := range errs {
		if e.Node == "" {
			return ""
		}
		if count[e.Node] == 0 {
			order = append(order, e.Node)
		}
		count[e.Node]++
	}
	var parts []string
	for _, n := range order {
		parts = append(parts, fmt.Sprintf("%d on %s", count[n], c.nodeName(ctx, n)))
	}
	return strings.Join(parts, ", ")
}

// logFinish writes the outcome of a run: failures and runs with problems at
// the error level, everything else as an event.
func (c *Controller) logFinish(ctx context.Context, act runActivity, run *store.Run, res *engine.Result) {
	kind, st := kindLabel(run.Kind), run.Stats
	switch run.Status {
	case store.StatusFailed:
		act.logRun(logbook.LevelError, "%s failed: %s", kind, run.Error)
	case store.StatusCancelled:
		act.logRun(logbook.LevelEvent, "%s cancelled", kind)
	case store.StatusApproval:
		act.logRun(logbook.LevelEvent, "%s waiting for approval: %s", kind, run.Error)
	default:
		var parts []string
		if st.Transferred > 0 {
			parts = append(parts, fmt.Sprintf("%d files copied (%s)", st.Transferred, humanBytes(st.TransferredBytes)))
		}
		for _, n := range []struct {
			v     int64
			label string
		}{{st.Checked, "checked"}, {st.Missing, "missing"}, {st.Mismatched, "damaged or changed"}, {st.Untracked, "unknown files on the target"},
			{st.NotInSource, "not in source"}, {st.Repaired, "removed from the index"}, {st.Trashed, "moved to the trash"},
			{st.Deleted, "deleted permanently"}, {st.Indexed, "indexed"}, {st.Skipped, "skipped"}, {st.Failed, "failed"}} {
			if n.v > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", n.v, n.label))
			}
		}
		if st.Failed > 0 && res != nil && int64(len(res.Errors)) == st.Failed {
			if where := c.failedWhere(ctx, res.Errors); where != "" {
				parts[len(parts)-1] += " (" + where + ")"
			}
		}
		if len(parts) == 0 {
			parts = append(parts, "nothing to do")
		}
		msg := fmt.Sprintf("%s finished (%s): %s", kind, run.Status, strings.Join(parts, ", "))
		if res != nil && res.Message != "" {
			msg += ". " + res.Message
		}
		if st.Missing+st.Mismatched+st.Untracked+st.NotInSource+st.Failed > 0 {
			msg += ". The file log lists the files"
		}
		level := logbook.LevelEvent
		if run.Status == store.StatusWarning {
			level = logbook.LevelError
		}
		act.logRun(level, "%s", msg)
	}
}

func (c *Controller) targetLabel(ctx context.Context, job store.Job, targetID int64) string {
	for _, t := range job.Targets {
		if t.ID == targetID {
			return fmt.Sprintf("%s → %s:%s", job.Name, c.nodeName(ctx, t.NodeID), t.Path)
		}
	}
	return job.Name
}

func (c *Controller) notifyRun(ctx context.Context, run *store.Run, job store.Job, res *engine.Result) {
	label := c.targetLabel(ctx, job, run.TargetID)
	kind := strings.ReplaceAll(run.Kind, "_", " ")
	verify := run.Kind == store.KindVerifyQuick || run.Kind == store.KindVerifyDeep
	switch run.Status {
	case store.StatusFailed:
		c.Notify(ctx, notify.Event{Kind: notify.RunFailed, Priority: 1, Title: "BFBackup: " + kind + " failed", Message: label + "\n" + run.Error})
	case store.StatusApproval:
		c.Notify(ctx, notify.Event{Kind: notify.ApprovalRequired, Priority: 1, Title: "BFBackup: approval needed", Message: label + "\n" + run.Error})
	case store.StatusWarning:
		if verify {
			msg := label
			if res != nil {
				msg += "\n" + res.Message
			}
			c.Notify(ctx, notify.Event{Kind: notify.VerifyProblem, Priority: 1, Title: "BFBackup: verify found problems", Message: msg})
		} else {
			c.Notify(ctx, notify.Event{Kind: notify.RunWarning, Title: "BFBackup: " + kind + " finished with errors",
				Message: fmt.Sprintf("%s\n%d files failed, %d copied", label, run.Stats.Failed, run.Stats.Transferred)})
		}
	case store.StatusSuccess:
		if run.Kind == store.KindBackup {
			c.Notify(ctx, notify.Event{Kind: notify.RunSuccess, Priority: -1, Title: "BFBackup: backup finished",
				Message: fmt.Sprintf("%s\n%d files, %s copied", label, run.Stats.Transferred, humanBytes(run.Stats.TransferredBytes))})
		}
	}
	if run.Kind == store.KindBackup && run.Stats.TargetTotal > 0 {
		c.checkLowSpace(ctx, run.TargetID, label, run.Stats.TargetFree, run.Stats.TargetTotal)
	}
}

func (c *Controller) checkLowSpace(ctx context.Context, targetID int64, label string, free, total uint64) {
	s, err := c.Settings(ctx)
	if err != nil || s.LowSpacePct <= 0 {
		return
	}
	pct := float64(free) / float64(total) * 100
	if pct >= float64(s.LowSpacePct) {
		return
	}
	c.mu.Lock()
	last := c.lowSpace[targetID]
	now := c.cfg.Now()
	if now.Sub(last).Hours() < 24 {
		c.mu.Unlock()
		return
	}
	c.lowSpace[targetID] = now
	c.mu.Unlock()
	c.Notify(ctx, notify.Event{Kind: notify.LowSpace, Priority: 1, Title: "BFBackup: target almost full",
		Message: fmt.Sprintf("%s\nonly %.1f%% free (%s)", label, pct, humanBytes(int64(free)))})
}

// ActiveRun is a running or queued run with live progress.
type ActiveRun struct {
	store.Run
	Progress engine.Snapshot `json:"progress"`
	Waiting  string          `json:"waiting,omitempty"` // why a queued run waits
}

// ActiveRuns lists running runs, then the queue in the order it will start.
func (c *Controller) ActiveRuns() []ActiveRun {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := []ActiveRun{}
	for _, ar := range c.sortedActiveLocked() {
		out = append(out, ActiveRun{Run: ar.run, Progress: ar.snapshot(), Waiting: ar.waiting})
	}
	return out
}

// RunProgress returns live progress of an active run.
func (c *Controller) RunProgress(id int64) (engine.Snapshot, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ar, ok := c.active[id]
	if !ok {
		return engine.Snapshot{}, false
	}
	return ar.snapshot(), true
}

// snapshot is the progress with what the controller knows: the watchdog's
// limit and the newest log line. Call with c.mu held.
func (ar *activeRun) snapshot() engine.Snapshot {
	s := ar.progress.Snapshot()
	s.StallSeconds = int64(ar.stall.Seconds())
	if msg, at := ar.act.last.get(); msg != "" {
		s.Latest, s.LatestAt = msg, at.Unix()
	}
	return s
}

// CancelRun stops an active run.
func (c *Controller) CancelRun(ctx context.Context, id int64) error {
	c.mu.Lock()
	ar, ok := c.active[id]
	c.mu.Unlock()
	if !ok {
		return errors.New("run is not active")
	}
	ar.cancel()
	c.audit(ctx, "run_cancel", "cancelled run %d", id)
	return nil
}

// CancelAll stops every active or queued run and returns how many.
func (c *Controller) CancelAll(ctx context.Context) int {
	c.mu.Lock()
	n := len(c.active)
	for _, ar := range c.active {
		ar.cancel()
	}
	c.mu.Unlock()
	if n > 0 {
		c.audit(ctx, "run_cancel", "stopped all %d active run(s)", n)
	}
	return n
}

// backupHistory returns the stats of the last finished backups of a
// target, newest first, for the history anomaly check.
func (c *Controller) backupHistory(ctx context.Context, targetID, exclude int64) ([]model.RunStats, error) {
	runs, _, err := c.store.ListRuns(ctx, store.RunFilter{TargetID: targetID, Kinds: []string{store.KindBackup},
		Statuses: []string{store.StatusSuccess, store.StatusWarning}, Limit: 10})
	if err != nil {
		return nil, err
	}
	var out []model.RunStats
	for _, r := range runs {
		if r.ID != exclude {
			out = append(out, r.Stats)
		}
	}
	return out, nil
}

// deleteLimit is how many files a run may remove without asking.
func (c *Controller) deleteLimit(ctx context.Context, stored int64) int64 {
	s, err := c.Settings(ctx)
	if err != nil {
		return 1 // be careful when the settings cannot be read
	}
	return s.Guards.DeleteLimit(stored)
}

// RetryRun starts a failed or incomplete run again with the same request.
// A delete is only repeated where it did not finish: on the targets that
// were offline or failed, and on the source only if deleting there failed.
// The old run is dismissed from the attention list.
func (c *Controller) RetryRun(ctx context.Context, id int64) (store.Run, error) {
	old, err := c.store.GetRun(ctx, id)
	if err != nil {
		return store.Run{}, err
	}
	switch old.Status {
	case store.StatusFailed, store.StatusWarning, store.StatusInterrupted, store.StatusCancelled:
	default:
		return store.Run{}, fmt.Errorf("run #%d is %s; only failed or incomplete runs can be tried again", id, old.Status)
	}
	var d struct {
		Restore        *RestoreRequest        `json:"restore"`
		Delete         *DeleteRequest         `json:"delete"`
		Repair         bool                   `json:"repair"`
		Hash           bool                   `json:"hash"`
		Sync           string                 `json:"sync"`
		SourceFailed   bool                   `json:"source_failed"`
		FailedTargets  []int64                `json:"failed_targets"`
		SkippedTargets []engine.SkippedTarget `json:"skipped_targets"`
	}
	if len(old.Details) > 0 {
		if err := json.Unmarshal(old.Details, &d); err != nil {
			return store.Run{}, fmt.Errorf("run #%d: %w", id, err)
		}
	}
	req := RunRequest{Kind: old.Kind, JobID: old.JobID, TargetID: old.TargetID, Repair: d.Repair, Hash: d.Hash,
		Sync: d.Sync, Restore: d.Restore, Trigger: fmt.Sprintf("try again of run #%d", id)}
	if old.Kind == store.KindDelete {
		if d.Delete == nil {
			return store.Run{}, fmt.Errorf("run #%d has no delete request to repeat", id)
		}
		del := *d.Delete
		// Only what is left: targets skipped or failed, the source if it failed.
		// A run that stopped before deleting anything has neither and is
		// repeated as it was.
		var left []int64
		for _, s := range d.SkippedTargets {
			left = append(left, s.TargetID)
		}
		left = append(left, d.FailedTargets...)
		if len(left) > 0 || d.SourceFailed {
			del.TargetIDs = left
			del.FromSource = del.FromSource && d.SourceFailed
			if len(left) == 0 {
				// Only the source is left: no targets at all, not "all targets".
				del.TargetIDs = []int64{-1}
			}
		}
		req.Delete = &del
	}
	nr, err := c.StartRun(ctx, req)
	if err != nil {
		return nr, err
	}
	c.store.AcknowledgeRun(ctx, id)
	c.audit(ctx, "run_retry", "tried run %d again as run %d", id, nr.ID)
	return nr, nil
}

// DeclineRun answers "stop" to a run that waits for a decision.
func (c *Controller) DeclineRun(ctx context.Context, id int64) error {
	r, err := c.store.GetRun(ctx, id)
	if err != nil {
		return err
	}
	if r.Status != store.StatusApproval {
		return errors.New("run is not waiting for a decision")
	}
	r.Status = store.StatusDeclined
	if err := c.store.UpdateRun(ctx, &r); err != nil {
		return err
	}
	c.store.AcknowledgeRun(ctx, id)
	c.audit(ctx, "run_decline", "stopped run %d at its question; nothing was changed", id)
	return nil
}

// AcknowledgeRun dismisses a finished run from the attention list.
func (c *Controller) AcknowledgeRun(ctx context.Context, id int64) error {
	if err := c.store.AcknowledgeRun(ctx, id); err != nil {
		return err
	}
	c.audit(ctx, "run_dismiss", "dismissed run %d from the attention list", id)
	return nil
}

// pendingDecision returns a run of the target that waits for the user, or 0.
func (c *Controller) pendingDecision(ctx context.Context, targetID int64) int64 {
	runs, _, err := c.store.ListRuns(ctx, store.RunFilter{TargetID: targetID, Statuses: []string{store.StatusApproval}, Limit: 1})
	if err != nil || len(runs) == 0 {
		return 0
	}
	return runs[0].ID
}

// ApproveRun continues a backup that stopped for approval.
func (c *Controller) ApproveRun(ctx context.Context, id int64) (store.Run, error) {
	r, err := c.store.GetRun(ctx, id)
	if err != nil {
		return store.Run{}, err
	}
	if r.Status != store.StatusApproval {
		return store.Run{}, errors.New("run is not waiting for approval")
	}
	var d struct {
		Sync string `json:"sync"`
	}
	json.Unmarshal(r.Details, &d)
	nr, err := c.StartRun(ctx, RunRequest{Kind: store.KindBackup, TargetID: r.TargetID, Approved: true, Sync: d.Sync,
		Trigger: fmt.Sprintf("approval of run #%d", r.ID)})
	if err != nil {
		return nr, err
	}
	r.Status = store.StatusApproved
	c.store.UpdateRun(ctx, &r)
	c.store.AcknowledgeRun(ctx, id)
	c.audit(ctx, "run_approve", "continued run %d after its question, as run %d", id, nr.ID)
	return nr, nil
}

// TargetBusy reports the running run of a target, else its first queued
// run, or 0.
func (c *Controller) TargetBusy(id int64) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, ar := range c.sortedActiveLocked() {
		for _, t := range ar.targets {
			if t == id {
				return ar.run.ID
			}
		}
	}
	return 0
}

func humanBytes(n int64) string { return model.HumanBytes(n) }
