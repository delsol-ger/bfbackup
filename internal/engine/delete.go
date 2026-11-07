package engine

import (
	"context"
	"fmt"
	"strings"

	"bfbackup/internal/endpoint"
	"bfbackup/internal/logbook"
	"bfbackup/internal/model"
	"bfbackup/internal/store"
)

// TargetEP is a target with the endpoint of its node.
type TargetEP struct {
	Target store.Target
	EP     endpoint.Endpoint
}

// DeleteSpec moves paths to the trash on the source and/or targets, or
// removes them for good.
type DeleteSpec struct {
	RunID   int64
	Job     store.Job
	Src     endpoint.Endpoint // nil: keep the source untouched
	Targets []TargetEP
	Paths   []string // relative to the job root
	// Permanent skips the trash: the files are gone at once.
	Permanent bool
	Log       Activity
	// Skipped are targets the controller could not reach; they are only
	// reported. Targets that go offline during the run are added to them.
	Skipped []SkippedTarget
}

// SkippedTarget is a target a delete did not reach.
type SkippedTarget struct {
	TargetID int64  `json:"target_id"`
	NodeID   string `json:"node"`
	Path     string `json:"path"`
	Reason   string `json:"reason"`
}

// Delete moves each path into the trash of the source (if Src is set) and of
// every target (or removes it for good with Permanent), and removes it from
// the targets' index. A path missing on one side is not an error.
func (e *Engine) Delete(ctx context.Context, spec DeleteSpec) (*Result, error) {
	res := &Result{Details: map[string]any{"paths": spec.Paths, "source": spec.Src != nil, "permanent": spec.Permanent}}
	st := &res.Stats
	errs := &errorList{max: e.maxErrors(), act: spec.Log}
	var rels []string
	for _, p := range spec.Paths {
		c, err := model.CleanRel(p)
		if err != nil {
			return res, err
		}
		if c == "" {
			return res, fmt.Errorf("refusing to delete the whole job root; delete the job instead")
		}
		rels = append(rels, c)
	}
	batch := e.trashBatch(spec.RunID)
	// remove trashes or deletes one path on one side and logs it.
	remove := func(ep endpoint.Endpoint, node, root, rel, side string) error {
		var err error
		if spec.Permanent {
			err = ep.Remove(ctx, root, rel)
		} else {
			err = ep.Trash(ctx, root, rel, batch)
		}
		switch {
		case err == nil && spec.Permanent:
			st.Deleted++
			logf(spec.Log, logbook.FileOK, node, rel, "deleted permanently on %s", side)
		case err == nil:
			st.Trashed++
			logf(spec.Log, logbook.FileOK, node, rel, "moved to the trash on %s", side)
		case isNotExist(err):
			logf(spec.Log, logbook.FileIssue, node, rel, "not on %s, nothing to delete there", side)
			return nil
		}
		return err
	}
	skipped := append([]SkippedTarget(nil), spec.Skipped...)
	gone := map[int64]bool{} // targets that went offline during the run
	failedTargets := map[int64]bool{}
	sourceFailed := false
	for _, rel := range rels {
		if spec.Src != nil {
			if err := remove(spec.Src, spec.Job.SourceNode, spec.Job.SourcePath, rel, "the source"); err != nil {
				st.Failed++
				sourceFailed = true
				errs.addAt(spec.Job.SourceNode, rel, fmt.Errorf("on the source %s: %w", spec.Job.SourcePath, describe(err)))
				continue // keep the backup if the source could not be removed
			}
		}
		for _, t := range spec.Targets {
			if gone[t.Target.ID] {
				continue
			}
			if err := remove(t.EP, t.Target.NodeID, t.Target.Path, rel, "target "+t.Target.Path); err != nil {
				if model.IsConnError(err) && ctx.Err() == nil {
					// Offline now: skip this target for the remaining paths
					// instead of failing every one of them.
					gone[t.Target.ID] = true
					skipped = append(skipped, SkippedTarget{TargetID: t.Target.ID, NodeID: t.Target.NodeID, Path: t.Target.Path, Reason: "went offline during the run"})
					logf(spec.Log, logbook.LevelError, t.Target.NodeID, "", "target %s went offline during the delete; the remaining paths were not deleted there", t.Target.Path)
					continue
				}
				st.Failed++
				failedTargets[t.Target.ID] = true
				errs.addAt(t.Target.NodeID, rel, fmt.Errorf("on target %s: %w", t.Target.Path, describe(err)))
				continue
			}
			n, err := e.Store.DeletePrefix(ctx, t.Target.ID, rel)
			if err != nil {
				return res, err
			}
			st.Indexed += n
		}
	}
	for _, t := range spec.Targets {
		e.Store.PruneEmptyDirs(ctx, t.Target.ID)
		e.Store.RefreshTargetTotals(ctx, t.Target.ID)
	}
	res.Errors = errs.items()
	res.Status = finalStatus(st.Failed)
	// What a "try again" has to repeat.
	if sourceFailed {
		res.Details["source_failed"] = true
	}
	if len(failedTargets) > 0 {
		var ids []int64
		for id := range failedTargets {
			ids = append(ids, id)
		}
		res.Details["failed_targets"] = ids
	}
	if len(skipped) > 0 {
		res.Details["skipped_targets"] = skipped
		res.Status = store.StatusWarning
		var where []string
		for _, s := range skipped {
			where = append(where, s.Path+" ("+s.Reason+")")
		}
		res.Message = "not deleted on " + strings.Join(where, ", ") + "; Try again deletes there once the node is back"
	}
	return res, nil
}

// RebuildSpec recreates a target's index from the files on the target.
type RebuildSpec struct {
	RunID    int64
	Target   store.Target
	Dst      endpoint.Endpoint
	Hash     bool
	Progress *Progress
	Log      Activity
}

// RebuildIndex replaces the index of a target with what is on disk, e.g.
// after losing the controller database. Without Hash, checksums are learned
// by the next deep verify.
func (e *Engine) RebuildIndex(ctx context.Context, spec RebuildSpec) (*Result, error) {
	res := &Result{Details: map[string]any{"hash": spec.Hash}}
	st := &res.Stats
	prog := spec.Progress
	if prog == nil {
		prog = NewProgress()
	}
	prog.ScanEstimate.Store(spec.Target.StoredFiles)
	prog.SetPhase("scanning target")
	if _, err := e.Store.DeletePrefix(ctx, spec.Target.ID, ""); err != nil {
		return res, err
	}
	iw := e.Store.NewIndexWriter(spec.Target.ID)
	now := e.now().Unix()
	req := model.ScanRequest{Root: spec.Target.Path, Hash: spec.Hash,
		Hashed: func(n int64) { prog.HashedBytes.Add(n) }, Reading: prog.reading}
	err := spec.Dst.Scan(ctx, req, func(l model.DirListing) error {
		prog.listed(l.Dir)
		for _, ent := range l.Entries {
			if ent.Type == model.TypeDir {
				continue
			}
			prog.Scanned.Add(1)
			st.Indexed++
			if err := iw.Put(ctx, store.IndexEntry{Dir: l.Dir, Name: ent.Name, Type: ent.Type, Size: ent.Size,
				MTime: ent.MTime, Mode: ent.Mode, Hash: ent.Hash, Link: ent.Link, SyncedAt: now, SeenRun: spec.RunID}); err != nil {
				return err
			}
			logf(spec.Log, logbook.FileOK, "", model.JoinRel(l.Dir, ent.Name), "indexed (%s)", model.HumanBytes(ent.Size))
		}
		return nil
	})
	if cerr := iw.Close(context.WithoutCancel(ctx)); err == nil {
		err = cerr
	}
	e.Store.RefreshTargetTotals(context.WithoutCancel(ctx), spec.Target.ID)
	if err != nil {
		return res, fmt.Errorf("scanning target: %w", err)
	}
	res.Status = store.StatusSuccess
	return res, nil
}
