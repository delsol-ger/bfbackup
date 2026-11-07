package engine

import (
	"context"
	"fmt"
	"path"
	"sync/atomic"

	"bfbackup/internal/endpoint"
	"bfbackup/internal/logbook"
	"bfbackup/internal/model"
	"bfbackup/internal/store"
)

// RestoreSpec copies files from a target back to the source or elsewhere.
type RestoreSpec struct {
	RunID  int64
	Target store.Target
	From   endpoint.Endpoint // the target's node
	To     endpoint.Endpoint // where files go
	ToRoot string            // absolute; files land at ToRoot/<path relative to the job root>
	Paths  []string          // relative to the job root; "" = everything
	// Overwrite copies even files that already look identical.
	Overwrite bool
	Workers   int
	Progress  *Progress
	Log       Activity // default node: the destination
}

// RestoreDetails are stored with the run.
type RestoreDetails struct {
	ToNode string   `json:"to_node"`
	ToRoot string   `json:"to_root"`
	Paths  []string `json:"paths"`
}

const statBatch = 500

// Restore copies the selected paths. Existing identical files are skipped
// (incremental) unless Overwrite is set; existing different files are moved
// to the trash first.
func (e *Engine) Restore(ctx context.Context, spec RestoreSpec) (*Result, error) {
	prog := spec.Progress
	if prog == nil {
		prog = NewProgress()
	}
	res := &Result{Details: map[string]any{}}
	st := &res.Stats
	errs := &errorList{max: e.maxErrors(), act: spec.Log}
	toRoot, err := model.CleanAbs(spec.ToRoot)
	if err != nil {
		return res, err
	}
	if len(spec.Paths) == 0 {
		return res, fmt.Errorf("nothing selected")
	}
	var rels []string
	for _, p := range spec.Paths {
		c, err := model.CleanRel(p)
		if err != nil {
			return res, err
		}
		rels = append(rels, c)
	}
	trashBatch := e.trashBatch(spec.RunID)
	prog.SetPhase("planning")
	for _, rel := range rels {
		n, b, err := e.Store.CountPrefix(ctx, spec.Target.ID, rel)
		if err != nil {
			return res, err
		}
		prog.FilesTotal.Add(n)
		prog.BytesTotal.Add(b)
	}

	// A peer that stays away ends the run with one error, not one per file.
	ctx, stopGuard := guardOffline(ctx)
	defer stopGuard()
	prog.SetPhase("transferring")
	stopBeat := heartbeat(ctx, func() { prog.logTransfer(spec.Log, "") })
	defer stopBeat()
	var transferred, transferredBytes, skipped, failed, trashed atomic.Int64
	work := make(chan store.IndexEntry, 64)
	workers := spec.Workers
	if workers <= 0 {
		workers = 4
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		bufs := make([][]byte, workers)
		runPool(ctx, workers, work, func(worker int, ie store.IndexEntry) {
			if bufs[worker] == nil {
				bufs[worker] = make([]byte, 1<<20)
			}
			rel := ie.Rel()
			ctx := withFile(ctx, prog.startFile(worker, rel, ie.Size))
			defer prog.endFile(worker)
			defer prog.FilesDone.Add(1)
			var err error
			dst := path.Join(toRoot, rel)
			if ie.Type == model.TypeSymlink {
				err = spec.To.Symlink(ctx, dst, ie.Link)
			} else {
				var sum string
				meta := ie.Entry()
				_, sum, err = copyFile(ctx, spec.From, spec.To, path.Join(spec.Target.Path, rel), dst, bufs[worker], prog, &meta)
				if err == nil && ie.Hash != "" && sum != ie.Hash {
					errs.addMsg(rel, "restored, but the backup copy differs from the checksum recorded at backup time")
				}
			}
			if err != nil {
				if !offline(ctx, err) && ctx.Err() == nil {
					failed.Add(1)
					errs.add(rel, describe(err))
				}
				return
			}
			transferred.Add(1)
			transferredBytes.Add(ie.Size)
			if ie.Type == model.TypeSymlink {
				logf(spec.Log, logbook.FileOK, "", rel, "symlink restored → %s", ie.Link)
			} else {
				logf(spec.Log, logbook.FileOK, "", rel, "restored (%s)", model.HumanBytes(ie.Size))
			}
		})
	}()

	// Walk the index, stat destinations in batches and queue what differs.
	var batch []store.IndexEntry
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		paths := make([]string, len(batch))
		for i, ie := range batch {
			paths[i] = path.Join(toRoot, ie.Rel())
		}
		stats, err := spec.To.Stat(ctx, paths)
		if err != nil {
			return err
		}
		for i, ie := range batch {
			s := stats[i]
			if s.Exists {
				same := s.Entry.Type == ie.Type &&
					(ie.Type == model.TypeSymlink && s.Entry.Link == ie.Link ||
						ie.Type == model.TypeFile && s.Entry.Size == ie.Size && abs64(s.Entry.MTime-ie.MTime) <= mtimeTolerance)
				if same && !spec.Overwrite {
					skipped.Add(1)
					prog.FilesDone.Add(1)
					prog.BytesDone.Add(ie.Size)
					logf(spec.Log, logbook.FileTrace, "", ie.Rel(), "skipped: an identical file is already there")
					continue
				}
				if err := spec.To.Trash(ctx, toRoot, ie.Rel(), trashBatch); err != nil && !isNotExist(err) {
					failed.Add(1)
					errs.add(ie.Rel(), fmt.Errorf("could not move the existing file to the trash: %w", err))
					prog.FilesDone.Add(1)
					continue
				}
				trashed.Add(1)
				logf(spec.Log, logbook.FileOK, "", ie.Rel(), "existing file moved to the trash before restoring")
			}
			select {
			case work <- ie:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		batch = batch[:0]
		return nil
	}
	var walkErr error
	for _, rel := range rels {
		walkErr = e.Store.WalkPrefix(ctx, spec.Target.ID, rel, func(ie store.IndexEntry) error {
			batch = append(batch, ie)
			if len(batch) >= statBatch {
				return flush()
			}
			return nil
		})
		if walkErr != nil {
			break
		}
	}
	if walkErr == nil {
		walkErr = flush()
	}
	close(work)
	<-done

	st.Transferred, st.TransferredBytes = transferred.Load(), transferredBytes.Load()
	st.Skipped, st.Failed, st.Trashed = skipped.Load(), failed.Load(), trashed.Load()
	res.Errors = errs.items()
	if err := offlineErr(ctx); err != nil {
		return res, err
	}
	if err := ctx.Err(); err != nil {
		return res, err
	}
	if walkErr != nil {
		return res, walkErr
	}
	res.Status = finalStatus(st.Failed)
	if len(res.Errors) > 0 && res.Status == store.StatusSuccess {
		res.Status = store.StatusWarning
	}
	return res, nil
}
